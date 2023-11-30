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
	"github.com/uber/cadence/common/quotas/global/loadbalanced/limiter/limit"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/limiter/typedmap"

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
		usage *typedmap.TypedMap[string, *limit.BalancedLimit]

		// lifecycle ctx, used for background requests
		ctx     context.Context
		cancel  func()
		stopped chan struct{}
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
	contents, err := typedmap.New(func(key string) *limit.BalancedLimit {
		instance := limit.New(fallback.For(key))
		return instance
	})
	if err != nil {
		panic(fmt.Sprintf("should be impossible: bad collection type: %v", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &BalancedCollection{
		usage:      contents,
		fallback:   nil,
		stopped:    make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
		client:     client,
		updateRate: updateRate,
	}
}

// Start the collection's background updater, and begin warming the cache.
//
// This will block until the ctx is canceled or the initial warm-up request
// completes, but it does not shut down if those times are exceeded, so set
// a reasonable blocking-timeout for startup.
//
// To stop, call Stop.
func (b *BalancedCollection) Start(startCtx context.Context) {
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
		err := b.client.Startup(ctx, func(load rpc.RetLoad, err error) {
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
			for k, v := range load.Allow {
				if v < 0 {
					// error loading this key?, do something
				} else {
					b.usage.Load(k).Update(float64(v))
				}
			}
		})
		cancel()

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

		tickRate := b.updateRate()
		tick := time.NewTicker(tickRate)
		defer tick.Stop()

		last := time.Now()
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
				all := make(map[string]rpc.Load, capacity)

				now := time.Now() // TODO: injectable for tests?
				b.usage.Range(func(k string, v *limit.BalancedLimit) bool {
					used, refused, usingFallback := v.Collect()
					_ = usingFallback // TODO: interesting for metrics
					all[k] = rpc.Load{
						Allowed:  used,
						Rejected: refused,
					}
					return true
				})

				// push to common hosts.
				// client parallelizes and calls callback as many times as there are hosts to contact,
				// and returns after all are complete.
				ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second) // TODO: configurable
				err := b.client.Update(ctx, now.Sub(last), all, func(request map[string]rpc.Load, load rpc.RetLoad, err error) {
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

					for k, v := range load.Allow {
						if v < 0 {
							// error loading this key, do something
							b.usage.Load(k).FailedUpdate()
						} else {
							b.usage.Load(k).Update(float64(v))
						}
					}

					// mark all non-returned limits as failures.
					// TODO: push this to to something pluggable?  better semantics should be possible
					for k := range request {
						if _, ok := load.Allow[k]; ok {
							// handled above
							continue
						}

						// requested but not returned, bump the fallback fuse
						b.usage.Load(k).FailedUpdate()
					}
				})
				cancel()

				if err != nil {
					for k := range all {
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
	}()

	// wait for warmup until ctx expires, but everything continues if that
	// time is passed.
	select {
	case <-startCtx.Done(): // startup timed out
	case <-warmup: // startup worked
	}
}

func (b *BalancedCollection) Stop(ctx context.Context) error {
	b.cancel()
	select {
	case err := <-ctx.Done():
		return fmt.Errorf("timed out while stopping: %w", err)
	case <-b.stopped:
		return nil
	}
}
