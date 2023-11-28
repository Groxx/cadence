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
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

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
		// TODO: per key, track and report usage, fall back on lack of info, etc

		// usage info, held in a sync.Map because it matches one of its documented "good fit" cases:
		// write once + read many times per key.
		usage *TypedMap[string, *BalancedLimit]

		updates chan update

		// fallback is used for keys that are believed to be unhealthy.
		// no attempt is made to migrate usage data between fallback and balanced,
		// so limits may briefly be exceeded due to an empty ratelimit.
		fallback *quotas.Collection

		client rpc.Client

		stopped chan struct{}
	}

	update struct {
		key string
		rps float64
	}

	BalancedLimit struct {
		sync.Mutex // all access requires holding the lock

		status string // initializing, updating, failing, etc.  see if we have anything.

		// TODO: usage data cannot be gathered from rate.Limiter, sadly.  so we need to gather it separately.
		// or find a fork maybe.
		usage *usage

		limit *rate.Limiter // local-only limiter based on remote data

		lastCollected time.Time // zero if no report sent yet, used to normalize in the common hosts
		lastUpdated   time.Time // last update time, never zero
	}

	// usage is a simple usage-tracking mechanism for limiting hosts.
	//
	// all it cares about is total since last report.  no attempt is made to address
	// abnormal spikes within report times, widely-varying behavior across reports,
	// etc - that kind of logic is left to the common hosts, not the limiting ones.
	usage struct {
		rps     float64 // intended ratelimit
		used    int64   // atomic, reset to zero when a report is gathered
		refused int64   // atomic, reset to zero when a report is gathered
	}
)

func NewBalanced() *BalancedCollection {
	contents, err := NewTypedMap(func(key string) *BalancedLimit {
		instance := &BalancedLimit{
			status: "new",
			usage:  &usage{},
			limit:  rate.NewLimiter(1, 1), // will be adjusted
		}
		return instance
	})
	if err != nil {
		panic(fmt.Sprintf("should be impossible: bad collection type: %v", err))
	}
	return &BalancedCollection{
		usage:    contents,
		updates:  make(chan update, 100),
		fallback: nil,
		stopped:  make(chan struct{}),
	}
}

func (b *BalancedCollection) Start(ctx context.Context) {
	updater, warmup := make(chan struct{}), make(chan struct{})
	// wait for sub-goroutines when stopping
	go func() {
		<-updater
		<-warmup
		close(b.stopped)
	}()

	host := rpc.Host(uuid.New().String())

	// TODO: kick off a client.Startup request
	go func() {
		defer close(warmup)

		err := b.client.Startup(ctx, func(load rpc.RetLoad, err error) {
			if err != nil {
				// TODO: probably wrong, but it's something
				for k, v := range load.Allow {
					if v < 0 {
						// error loading this key, do something
					} else {
						b.adjust(ctx, k, float64(v))
					}
				}
			}
		})
		if err != nil {
			// TODO: log
			panic(err)
		}
	}()

	// TODO: periodically collect and push updates
	go func() {
		defer close(updater)

		// wait for warmup for simplicity, so interleaved updates are not possible
		select {
		case <-warmup:
		case <-ctx.Done():
			return // shutting down, do nothing
		}

		const updatePeriod time.Duration = 10 * time.Second // TODO: configurable
		tick := time.NewTicker(updatePeriod)
		defer tick.Stop()

		last := time.Now()
		for {
			select {
			// tick.C occurs every period regardless of time spent waiting on a response,
			// which is probably preferred - it ensures more regular updates, rather than
			// trying to reduce calls
			case <-tick.C:
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

				now := time.Now() // TODO: injectable
				b.usage.Range(func(k string, v *BalancedLimit) bool {
					all[k] = rpc.Load{
						Allowed:  v.usage.used,
						Rejected: v.usage.refused,
					}
					v.lastCollected = now
					return true
				})

				// push to common hosts
				// leave parallelizing / etc to the client?
				err := b.client.Update(ctx, rpc.PushLoad{
					Host:   host,
					Period: now.Sub(last),
					Load:   all,
				}, func(load rpc.RetLoad, err error) {
					if err != nil {
						// TODO: stuff?
						return
					}
					for k, v := range load.Allow {
						if v < 0 {
							// error loading this key, do something
						} else {
							b.adjust(ctx, k, float64(v))
						}
					}
				})

				if err != nil {
					// TODO: log
					log.Fatal(err)
				}

				// and loop
				last = now
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (b *BalancedCollection) Stop(ctx context.Context) error {
	select {
	case err := <-ctx.Done():
		return fmt.Errorf("timed out while stopping: %w", err)
	case <-b.stopped:
		return nil
	}
}

func (b *BalancedCollection) allow(key string) (allowed bool) {
	val := b.usage.Load(key)
	val.Lock()
	defer val.Unlock()

	allowed = val.limit.Allow()
	if allowed {
		val.usage.used++
	} else {
		val.usage.refused++
	}
	return allowed
}

func (b *BalancedCollection) adjust(ctx context.Context, key string, rps float64) {
	val := b.usage.Load(key)
	val.Lock()
	defer val.Unlock()
	val.init_locked(ctx, func(rps float64) {
		select {
		case b.updates <- update{key: key, rps: rps}: // pushed to update queue
		default: // buffer exceeded, drop the update and alert
			// TODO: metrics, this is risky.  >100 pending updates blocked on init.
		}
	})

	val.lastUpdated = time.Now() // always update, even if load is unchanged
	if val.limit.Limit() == rate.Limit(rps) {
		return
	}
	val.limit.SetLimit(rate.Limit(rps))
	val.limit.SetBurst(int(rps))
	val.usage.rps = rps
}

// each calls the underlying [sync.Map.Range] (i.e. identical semantics), and
// holds the *BalancedLimit lock during the callback.
//
// The *BalancedLimit MUST NOT be retained beyond the callback's scope.
func (b *BalancedCollection) each(f func(key string, limit *BalancedLimit) bool) {
	b.usage.Range(func(k string, v *BalancedLimit) bool {
		v.Lock()
		defer v.Unlock()
		return f(k, v)
	})
}

// init_locked will perform one-time init, e.g. to load the initial value of a limiter.
// the lock must be held while this is called.
func (b *BalancedLimit) init_locked(ctx context.Context, async func(rps float64)) {
	if b.status != "" {
		return
	}
	b.status = "initializing" // one-time init occurring

	initialized := make(chan float64)
	go func() {
		defer close(initialized) // failures and timeouts lead to zero, treated as uninitialized

		// TODO: rpc init
		select {
		case <-time.After(time.Duration(rand.Intn(200)) * time.Millisecond):
			select {
			case initialized <- 5:
			// sync update to still-waiting init, nothing left to do
			default:
				// caller went away, update async to avoid deadlock
				async(5)
			}
		case <-ctx.Done():
		}
	}()

	t := time.NewTimer(100 * time.Millisecond) // TODO: configurable
	defer t.Stop()

	rps, read := float64(0), false
	select {
	case <-t.C:
	case rps, read = <-initialized:
	}

	if read && rps > 0 {
		// fully initialized, nothing to do
		return
	}

	// pending, use fallback limit
	// TODO: update limits
}
