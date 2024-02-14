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
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/quotas"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/errors"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/limiter/internal"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/rpc"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/typedmap"
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
	// strategy (once made pluggable).
	BalancedCollection struct {
		client     rpc.Client
		updateRate dynamicconfig.DurationPropertyFn
		fallback   *quotas.Collection

		logger log.Logger
		scope  metrics.Scope

		// usage info, held in a sync.Map because it matches one of its documented "good fit" cases:
		// write once + read many times per key.
		usage *typedmap.TypedMap[string, *internal.BalancedLimit]

		// lifecycle ctx, used for background requests
		ctx     context.Context
		cancel  func()
		stopped chan struct{}

		// now exists largely for tests, elsewhere it is always time.Now
		timesource clock.TimeSource
		// warmupTimeout exists largely for tests, in prod it's hardcoded
		warmupTimeout time.Duration
	}
)

// TODO: need to change this to a more limiter-friendly thing
var _ quotas.CollectionIface = (*BalancedCollection)(nil)

// NewBalanced
//
// The fallback arg is used for keys that are believed to be unhealthy, but it
// will be collected for every limit to ensure an immediate response is possible.
//
// No attempt is made to migrate usage data between the fallback and balanced,
// so limits may briefly be exceeded when switching (~1s with current code which
// always has burst==rps).
func NewBalanced(
	client rpc.Client,
	fallback *quotas.Collection,
	updateRate dynamicconfig.DurationPropertyFn,
	logger log.Logger,
	scope metrics.Scope,
) *BalancedCollection {
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
		client:     client,
		updateRate: updateRate,

		logger: logger.WithTags(tag.ComponentLoadbalancedRatelimiter),
		scope:  scope, // TODO: tag it

		stopped: make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,

		// override externally in tests
		timesource:    clock.NewRealTimeSource(), // TODO: inf
		warmupTimeout: 10 * time.Second,          // TODO: dynamic config?
	}
}

func (b *BalancedCollection) TestOverrides(t *testing.T, timesource clock.TimeSource) {
	t.Helper()
	b.timesource = timesource
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
		defer func() { log.CapturePanic(recover(), b.logger, nil) }() // should definitely not happen
		<-updater
		<-warmup
		close(b.stopped)
	}()

	go func() {
		defer func() { log.CapturePanic(recover(), b.logger, nil) }() // todo: describe what failed? is stack enough?
		defer close(warmup)

		deadline, ok := startCtx.Deadline()
		var timeout time.Duration
		if ok {
			timeout = deadline.Sub(b.timesource.Now())
		}

		ctx, cancel := context.WithTimeout(b.ctx, b.warmupTimeout)
		defer cancel()
		err := b.warmup(ctx)
		if err != nil {
			// TODO: log or fail startup, this should only be a dev error / incorrect types in serialization.
			b.logger.Error("failed to warm up before timeout/cancel",
				tag.Dynamic("timeout", timeout.Round(time.Millisecond).String()),
				tag.Error(err))
		}
		b.logger.Debug("warmup complete")
	}()

	go func() {
		defer func() { log.CapturePanic(recover(), b.logger, nil) }() // todo: describe what failed? is stack enough?
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
			if errors.IsRPCError(err) {
				// basically expected but potentially concerning
				b.logger.Warn("rpc error during warmup", tag.Error(err))
			} else {
				// unrecognized or serialization, either way should not happen
				b.logger.Error("other error during warmup", tag.Error(err))
			}
		}

		for k, v := range batch.Allow {
			if v < 0 {
				// should never happen
				b.logger.Error("negative rps", tag.Error(err), tag.Key(k), tag.Value(v))
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

	logger := b.logger.WithTags(tag.Dynamic("limiter", "update"))
	logger.Debug("update loop starting")

	ticker := b.timesource.NewTicker(tickRate)
	last := b.timesource.Now()
	for {
		select {
		// tick.C occurs every period regardless of time spent waiting on a response,
		// which is probably preferred - it ensures more regular updates, rather than
		// trying to reduce calls
		case <-ticker.Chan():
			logger.Debug("update tick")
			// update tick-rate if it changed
			newTickRate := b.updateRate()
			if tickRate != newTickRate {
				tickRate = newTickRate
				ticker.Reset(newTickRate)
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

			now := b.timesource.Now()
			b.usage.Range(func(k string, v *internal.BalancedLimit) bool {
				used, refused, usingFallback := v.Collect()
				_ = usingFallback // TODO: interesting for metrics?
				if used+refused == 0 {
					return true // no requests -> no update sent
				}
				all.Load[k] = rpc.Metrics{
					Allowed:  used,
					Rejected: refused,
				}
				return true
			})

			logger.Debug("found keys to update",
				tag.Dynamic("num", len(all.Load)),
				tag.Dynamic("all", all.Load))

			// push to common hosts.
			// client parallelizes and calls callback as many times as there are hosts to contact,
			// and returns after all are complete.
			ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second) // TODO: configurable
			err := b.client.Update(ctx, now.Sub(last), all, func(request rpc.AnyUpdateRequest, batch *rpc.AnyAllowResponse, err error) {
				logger.Debug("update callback",
					tag.Error(err),
					tag.Dynamic("request_keys", len(request.Load)),
					tag.Dynamic("response_keys", len(batch.Allow)))
				if err != nil {
					if errors.IsRPCError(err) {
						// basically expected but potentially a problem
						logger.Warn("rpc error during update", tag.Error(err))
					} else {
						// unrecognized or serialization, either way should not happen
						logger.Error("other error during update", tag.Error(err))
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
				// should not happen, this would mean e.g. no ringpop
				logger.Error("unable to perform update request", tag.Error(err))

				for k := range all.Load {
					// data requested but no request performed, bump the fallback fuse.
					// if a response is loaded successfully, it'll resume using the fancy limit.
					b.usage.Load(k).FailedUpdate()
				}
			}

			// and loop
			last = now
		case <-b.ctx.Done():
			ticker.Stop()
			logger.Debug("shutting down update loop")
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

func (b *BalancedCollection) For(key string) quotas.Limiter {
	return &allowonlylimiter{
		wrapped: b.usage.Load(key),
	}
}

type allowonlylimiter struct{ wrapped quotas.AllowLimiter }

var _ quotas.Limiter = (*allowonlylimiter)(nil)

func (a *allowonlylimiter) Allow() bool                    { return a.wrapped.Allow() }
func (a *allowonlylimiter) Wait(ctx context.Context) error { panic("not implemented") }
func (a *allowonlylimiter) Reserve() *rate.Reservation     { panic("not implemented") }

var _ clock.TimeSource
