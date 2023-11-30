// The MIT License (MIT)

// Copyright (c) 2017-2020 Uber Technologies Inc.

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package limiter

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/errors"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/limiter/internal"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/typedmap"

	"github.com/uber/cadence/common/quotas"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/rpc"
)

type (
	// BalancedCollection defines a cluster-wide load-balanced rate limiter,
	// designed to handle requests arriving at frontend hosts in an uneven way
	// (per domain and request type).
	//
	// Load per key is collected in frontend hosts that receive traffic (and attempt
	// to limit).  This is periodically reported to sharded hosts which collect the
	// cluster-wide data and aggregate it.  These sharded hosts return weighted
	// limit information to the limiting hosts, which update their limits to match
	// the request balance.
	//
	// This can be plugged into [github.com/uber/cadence/common/quotas/global] as a
	// strategy.
	BalancedCollection struct {
		client     rpc.Client
		updateRate dynamicconfig.DurationPropertyFn
		fallback   *quotas.Collection

		// usage info, held in a sync.Map because it matches one of its documented "good fit" cases:
		// write once + read many times per key.
		usage *typedmap.TypedMap[string, *internal.BalancedLimit]

		// lifecycle ctx, used for background requests
		ctx     context.Context
		cancel  func()
		stopped chan struct{}

		// now exists largely for tests, elsewhere it is always time.Now
		now func() time.Time
	}
)

// NewBalanced
//
// The fallback arg is used for keys that are believed to be unhealthy, but it
// will be collected for every limit to ensure an immediate response is possible.
//
// No attempt is made to migrate usage data between the fallback and balanced,
// so limits may briefly be exceeded when switching (~1s with current code which
// always has burst==rps).
func NewBalanced(client rpc.Client, fallback *quotas.Collection, updateRate dynamicconfig.DurationPropertyFn) *BalancedCollection {
	contents, err := typedmap.New(func(key string) *internal.BalancedLimit {
		instance := internal.New(fallback.For(key))
		return instance
	})
	if err != nil {
		panic(fmt.Sprintf("should be impossible: bad collection type: %v", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &BalancedCollection{
		usage:      contents,
		fallback:   fallback,
		stopped:    make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
		client:     client,
		updateRate: updateRate,

		// override externally in tests
		now: time.Now,
	}
}

// Start the collection's background updater, and begin warming the cache.
//
// If it returns an error, the initial warm-up request has not yet returned,
// but the collection is still ready for use and will continue trying to warm
// or update its data.  Until an update of some kind occurs, any requested keys
// will use the fallback quotas.Collection.
//
// Start will block until the ctx is canceled (abandoning startup) or the
// initial warm-up request completes, but it does not *shut down* if those times
// are exceeded, so set a reasonable blocking-timeout for startup.
//
// To stop, whether started or not, call Stop.
func (b *BalancedCollection) Start(startCtx context.Context) error {
	updater, warmup := make(chan struct{}), make(chan struct{})
	// wait for sub-goroutines when stopping
	go func() {
		<-updater
		<-warmup
		close(b.stopped)
	}()

	// TODO: kick off a client.Startup request
	go func() {
		defer close(warmup)

		ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second) // TODO: configurable
		defer cancel()
		err := b.warmup(ctx)
		if err != nil {
			// TODO: log or fail startup, this should only be a dev error / incorrect types in serialization.
			log.Fatal(err)
		}
	}()

	// TODO: periodically collect and push updates
	go func() {
		defer close(updater)

		// wait for warmup for simplicity, so interleaved updates are not possible
		select {
		case <-warmup:
		case <-b.ctx.Done():
			return // shutting down, do nothing
		}

		// loops forever / until Stop is called
		b.backgroundUpdateLoop()
	}()

	// wait for warmup until ctx expires, but everything continues if that
	// time is passed.
	select {
	case <-startCtx.Done():
		return startCtx.Err() // startup timed out
	case <-warmup: // startup worked
		return nil
	}
}

func (b *BalancedCollection) warmup(ctx context.Context) error {
	return b.client.Startup(ctx, func(batch *rpc.AnyAllowResponse, err error) {
		if err != nil {
			// log, particularly if is-deserialization
			if errors.IsDeserializationError(err) {
				// major bug
				// TODO: be smarter
				log.Fatal(err)
			} else if errors.IsRPCError(err) {
				// expected, yolo?
			} else {
				// unexpected!
				// TODO: be smarter
				log.Fatal(err)
			}
		}

		// TODO: probably wrong, but it's something
		for k, v := range batch.Allow {
			if v < 0 {
				// error loading this key?, do something
			} else {
				b.usage.Load(k).Update(v)
			}
		}
	})
}

// backgroundUpdateLoop runs an infinite loop where each iteration will:
//   - wait on an update-able ticker (via updateRate)
//   - collect usage data since the last iteration
//   - push that usage data to aggregating hosts via [rpc.Client.Update]
//   - update ratelimits that were returned
//
// To stop this loop, call Stop().
func (b *BalancedCollection) backgroundUpdateLoop() {
	tickRate := b.updateRate()
	tick := time.NewTicker(tickRate)
	defer tick.Stop()

	last := b.now()
	for {
		select {
		// tick.C occurs every period regardless of time spent waiting on a response,
		// which is probably preferred - it ensures more regular updates, rather than
		// trying to reduce calls
		case <-tick.C:
			// update tick-rate if it changed
			newTickRate := b.updateRate()
			if tickRate != newTickRate {
				tickRate = newTickRate
				tick.Reset(newTickRate)
			}

			// len is an estimate and more may be added while iterating.
			// avoid a costly re-alloc as nums are expected to be large-ish,
			// add 10% more space.
			//
			// when very small the addition doesn't matter, and when very
			// large the re-allocs are more costly and iterating takes more
			// time + the change may be larger.
			capacity := b.usage.Len()
			capacity += capacity / 10
			all := rpc.AnyUpdateRequest{
				Load: make(map[string]rpc.Metrics, capacity),
			}

			now := b.now()
			b.usage.Range(func(k string, v *internal.BalancedLimit) bool {
				used, refused, usingFallback := v.Collect()
				_ = usingFallback // TODO: interesting for metrics?
				all.Load[k] = rpc.Metrics{
					Allowed:  used,
					Rejected: refused,
				}
				return true
			})

			// push to common hosts.
			// client parallelizes and calls callback as many times as there are hosts to contact,
			// and returns after all are complete.
			ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second) // TODO: configurable
			err := b.client.Update(ctx, now.Sub(last), all, func(request rpc.AnyUpdateRequest, batch *rpc.AnyAllowResponse, err error) {
				if err != nil {
					// log, particularly if is-deserialization
					if errors.IsDeserializationError(err) {
						// major bug
						// TODO: be smarter
						log.Fatal(err)
					} else if errors.IsRPCError(err) {
						// largely expected, yolo?
					} else {
						// unexpected!
						// TODO: be smarter
						log.Fatal(err)
					}
				}

				for k, v := range batch.Allow {
					if v < 0 {
						// error loading this key, do something
						b.usage.Load(k).FailedUpdate()
					} else {
						b.usage.Load(k).Update(v)
					}
				}

				// mark all non-returned limits as failures.
				// TODO: push this to to something pluggable?  better semantics should be possible
				for k := range request.Load {
					if _, ok := batch.Allow[k]; ok {
						// handled above
						continue
					}

					// requested but not returned, bump the fallback fuse
					b.usage.Load(k).FailedUpdate()
				}
			})
			cancel()

			if err != nil {
				for k := range all.Load {
					// data requested but no request performed, bump the fallback fuse.
					// if a response is loaded successfully, it'll resume using the fancy limit.
					b.usage.Load(k).FailedUpdate()
				}

				// log, particularly if is-deserialization
				if errors.IsDeserializationError(err) {
					// major bug
					// TODO: be smarter
					log.Fatal(err)
				} else if errors.IsRPCError(err) {
					// unexpected when healthy and pretty bad since the whole attempt failed
					// last-updated time is now incorrect, but... eh, seems harmless.
				} else {
					// unexpected!
					// TODO: be smarter
					log.Fatal(err)
				}
			}

			if err != nil {
				// TODO: log, this should only be a dev error / incorrect types in serialization.
				log.Fatal(err)
			}

			// and loop
			last = now
		case <-b.ctx.Done():
			return
		}
	}
}

// Stop attempts to stop all background goroutines, and blocks until they return
// or the passed context expires.  It can be called, concurrently, any number of
// times.
//
// If an error is returned, the *request* to stop has succeeded, but there may
// still be dangling goroutines.  Either call Stop again with additional time to
// continue waiting, or give up and hope for the best - the goroutines should
// still stop ASAP.
func (b *BalancedCollection) Stop(ctx context.Context) error {
	b.cancel()
	select {
	case <-ctx.Done():
		return fmt.Errorf("timed out while stopping: %w", ctx.Err())
	case <-b.stopped:
		return nil
	}
}
