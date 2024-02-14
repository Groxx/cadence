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

package aggregator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/aggregator/algorithm"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/aggregator/internal"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/rpc"
)

type (
	// Impl is public for test purposes
	Impl struct {
		limits algorithm.WeightedAverage

		// used to estimate number of frontend hosts
		// TODO: replace with frontend ringpop resolver for a much more accurate count
		hostsLastSeen *internal.HostSeen
		rotateRate    dynamicconfig.DurationPropertyFn

		logger log.Logger
		scope  metrics.Scope

		// lifecycle management
		ctx     context.Context
		cancel  func()
		stopped chan struct{}

		// for tests
		timesource      clock.TimeSource
		hostObservedTTL time.Duration
	}
)

func New(
	rps dynamicconfig.IntPropertyFn,
	updateRate dynamicconfig.DurationPropertyFn,
	logger log.Logger,
	scope metrics.Scope,
) (Aggregator, error) {
	agg, err := algorithm.New(0.5, updateRate)
	if err != nil {
		// should not be possible
		return nil, fmt.Errorf("failed to create aggregator: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Impl{
		limits:        agg,
		hostsLastSeen: internal.NewHostSeen(),
		rotateRate:    updateRate,

		logger: logger.WithTags(tag.ComponentLoadbalancedRatelimiter),
		scope:  scope, // TODO: tag

		ctx:     ctx,
		cancel:  cancel,
		stopped: make(chan struct{}),

		timesource:      clock.NewRealTimeSource(),
		hostObservedTTL: time.Minute, // TODO: dynamic config?
	}, nil
}

func (i *Impl) TestOverrides(t *testing.T, timesource clock.TimeSource) {
	t.Helper()
	i.timesource = timesource
}

func (i *Impl) Start() {
	go func() {
		defer func() { log.CapturePanic(recover(), i.logger, nil) }() // todo: describe what failed? is stack enough?
		defer close(i.stopped)

		tickRate := i.rotateRate()
		ticker := i.timesource.NewTicker(tickRate)
		defer ticker.Stop()
		for {
			select {
			case <-i.ctx.Done():
				ticker.Stop()
				return // shutting down
			case <-ticker.Chan():
				// update tick-rate if it changed
				newTickRate := i.rotateRate()
				if tickRate != newTickRate {
					tickRate = newTickRate
					ticker.Reset(newTickRate)
				}

				// rotate buckets
				i.rotate()
			}
		}
	}()
}

func (i *Impl) Stop(ctx context.Context) error {
	i.cancel()
	select {
	case <-i.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Update adds load information for the passed keys to this aggregator.
func (i *Impl) Update(host string, elapsed time.Duration, load rpc.AnyUpdateRequest) error {
	for key, data := range load.Load {
		if len(data) < 2 {
			return fmt.Errorf("insufficient data for key: %q, need 2 values but got %d", key, len(data))
		}
		i.limits.Update(
			algorithm.Limit(key),
			algorithm.Identity(host),
			data[0], // allowed
			data[1], // rejected
			elapsed,
		)
	}
	return nil
}

// Get retrieves the known load / desired RPS for this host for this key, as a read-only operation.
func (i *Impl) Get(host string, keys []string) rpc.AnyAllowResponse {
	result := rpc.AnyAllowResponse{
		Allow: make(map[string]float64, len(keys)),
	}
	// TODO: could use per-key, but this would likely imply a need to GC
	all, unused := i.limits.HostWeights(algorithm.Identity(host))
	for _, limit := range keys {
		if weight, ok := all[algorithm.Limit(limit)]; ok {

		} else {
			// compute based on unused rps
			result.Allow[limit] = unused[]
		}
	}

	return result
}

func (i *Impl) GetAll(host string) rpc.AnyAllowResponse {
	i.hostsLastSeen.Observe(host, i.timesource.Now())

	result := rpc.AnyAllowResponse{
		// allocate space plus i bit of buffer for additions while ranging
		Allow: make(map[string]float64, int(float64(i.limits.Len())*1.1)),
	}
	i.limits.Range(func(k string, v *internal.Limit) bool {
		rps, previous, current := v.Snapshot()

		allowed, reason := i.getLimit(host, rps, current, previous)
		i.logger.Debug("getall limit calculated",
			tag.Key(k),
			tag.Dynamic("host", host),
			tag.Dynamic("reason", reason),
			tag.Dynamic("allowed", allowed),

			tag.Dynamic("current", current),
			tag.Dynamic("previous", previous))

		result.Allow[k] = allowed
		return true
	})

	return result
}

type numeric interface {
	~int | ~float64
}

func max[T numeric](a, b T) T {
	if a > b {
		return a
	}
	return b
}
func setUnion[T comparable](a, b map[T]struct{}) map[T]struct{} {
	total := make(map[T]struct{}, len(a)+len(b))
	for k := range a {
		total[k] = struct{}{}
	}
	for k := range b {
		total[k] = struct{}{}
	}
	return total
}
