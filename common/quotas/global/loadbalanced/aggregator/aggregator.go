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
	"github.com/uber/cadence/common/quotas/global/loadbalanced/aggregator/internal"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/rpc"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/typedmap"
)

/*
Plan:
- limiter always sends all non-zero data
- Agg always responds with all data
- Agg prunes after 60s of no updates

Agg stores:
- numbers per host
- toggles between "updating" and "using to decide" buckets every 10s

Agg decides:
- host in bucket?
  - get prev weight of all requests
  - can use weight-of-rps
- host unknown?
  - get unused rate
  - get num of non-zero-request hosts in bucket
  - get num of peers
  - get unused requests in UPDATING bucket
  - allow:
    - target of rps/nonzero-hosts, assuming it is becoming a new non-zero host
    - maximum of 2x rps/all-hosts, to ensure bursts to new hosts cannot exceed by more than 2x total
    - maximum of 1/10th of total rps, in case there are few known hosts
      - at worst, this 0%-allowed host will receive weighted allow-% next cycle

*/

type (
	// Impl is public for test purposes
	Impl struct {
		limits *typedmap.TypedMap[string, *internal.Limit]

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
	limits, err := typedmap.New(func(key string) *internal.Limit {
		return internal.NewLimit(rps)
	})
	if err != nil {
		return nil, fmt.Errorf("should be impossible: bad collection type: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Impl{
		limits:        limits,
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

func (i *Impl) rotate() {
	// begin i new round of limits
	i.limits.Range(func(k string, v *internal.Limit) bool {
		i.logger.Debug("rotating limit",
			tag.Key(k))
		v.Rotate()
		return true
	})

	// prune known limiting hosts
	i.hostsLastSeen.GC(i.timesource.Now(), i.hostObservedTTL)
}

// Update adds load information for the passed keys to this aggregator.
func (i *Impl) Update(host string, elapsed time.Duration, load rpc.AnyUpdateRequest) {
	// refresh the host-seen record
	i.hostsLastSeen.Observe(host, i.timesource.Now())

	for key, data := range load.Load {
		limit := i.limits.Load(key)
		i.logger.Debug("updating limit",
			tag.Key(key),
			tag.Dynamic("allowed", data.Allowed),
			tag.Dynamic("rejected", data.Rejected))
		limit.Update(host, float64(data.Allowed), float64(data.Rejected), elapsed)
	}
}

// Get retrieves the known load / desired RPS for this host for this key, as a read-only operation.
func (i *Impl) Get(host string, keys []string) rpc.AnyAllowResponse {
	// refresh the host-seen record
	i.hostsLastSeen.Observe(host, i.timesource.Now())

	result := rpc.AnyAllowResponse{
		Allow: make(map[string]float64, len(keys)),
	}
	for _, limit := range keys {
		rps, previous, current := i.limits.Load(limit).Snapshot()
		i.logger.Debug("get limit snapshot",
			tag.Key(limit),
			tag.Dynamic("host", host),
			tag.Dynamic("current", current),
			tag.Dynamic("previous", previous))

		if len(previous) == 0 {
			// warming up, return nothing
			continue
		}

		allowed, reason := i.getLimit(host, rps, current, previous)
		i.logger.Debug("get limit calculated",
			tag.Key(limit),
			tag.Dynamic("host", host),
			tag.Dynamic("reason", reason),
			tag.Dynamic("allowed", allowed),

			tag.Dynamic("current", current),
			tag.Dynamic("previous", previous))

		_ = reason // TODO: metrics for sure

		if allowed < 0 {
			// warming up, return nothing
			continue
		}

		result.Allow[limit] = allowed
	}

	return result
}

func (i *Impl) getLimit(host string, rps float64, current, previous map[string]internal.HostRecord) (allowed float64, reason string) {
	previousTotalRPS := 0.0
	previousThisHostRPS := 0.0
	previousZeroHosts := 0
	previousNonzeroHosts := 0
	// keep track of how many hosts we've seen, for estimation later
	previousHostnames := make(map[string]struct{}, len(previous))
	for k, v := range previous {
		previousHostnames[k] = struct{}{}

		allowed, rejected := v.Snapshot()
		sum := allowed + rejected
		if sum > 0 {
			previousNonzeroHosts++
		} else {
			previousZeroHosts++
		}

		if k == host {
			previousThisHostRPS = sum
		}
		previousTotalRPS += sum
	}

	if len(previousHostnames) == 0 || previousTotalRPS == 0 {
		return -1, "no data yet"
	}

	if _, ok := previousHostnames[host]; ok {
		// host in bucket?
		// - get prev weight of all requests
		// - can use weight-of-rps
		hostWeight := previousThisHostRPS / previousTotalRPS
		usableRPS := rps * hostWeight
		return usableRPS, "match from last period"
	}

	// Known:
	// - new host receiving traffic

	previousUnusedRPS := max(0, rps-previousTotalRPS)
	updatingTotalRPS := 0.0
	// keep track of how many hosts we've in this bucket
	updatingHostnames := make(map[string]struct{}, len(current))
	for k, v := range current {
		updatingHostnames[k] = struct{}{}

		allowed, rejected := v.Snapshot()
		updatingTotalRPS += allowed + rejected

		// TODO: scale to estimate by time until next rotation?
	}

	// safety cutoff if we appear already over-budget.
	if updatingTotalRPS >= rps {
		// easy case: new host and our in-progress data has already used all RPS.
		// reject it all, they're already getting ratelimit errors.
		// next cycle will allow this host some portion based on weight.
		return 0, "new host, current over budget failsafe"
	}

	// Known:
	// - new host receiving traffic
	// - current bucket has fewer requests than RPS allows.

	if previousUnusedRPS > 0 {
		// prepwork for below: figure out how many hosts there (probably) are,
		// and use that to estimate i safe proportion of the total rps.
		totalFrontendHosts := i.hostsLastSeen.Len()
		// this should never be zero as we just observed this host, but I'm being paranoid.
		// there is definitely at least one: the current get-ing host.
		totalFrontendHosts = max(1, totalFrontendHosts)
		// similarly, there are at least as many frontends as we have observed on this key (possibly 0).
		totalFrontendHosts = max(
			len(setUnion(previousHostnames, updatingHostnames)),
			totalFrontendHosts,
		)
		// so: totalFrontends >=1, and is as good of an upper-bound as we can safely guess at.

		// had some room last cycle, and some room exists this cycle.
		// allow this host:
		//  1. avg rps of non-zero hosts last cycle, else all rps
		//  2. cap at 2x weight of all known hosts
		//     - this allows max 2x rps going from 0 traffic to 100%-to-each-host,
		//       assuming an accurate view of all frontend hosts
		//  3. cap at 10% of the total rps for safety, it'll adjust next cycle
		//     - this covers when <10 hosts known
		allowedRPS := rps
		if previousNonzeroHosts > 0 {
			allowedRPS = previousTotalRPS / float64(previousNonzeroHosts) // 1
		}
		allowedRPS = max(allowedRPS, 2*(rps/float64(totalFrontendHosts))) // 2
		// TODO: configurable max?
		allowedRPS = max(allowedRPS, rps/10) // 3
		return allowedRPS, "new host, room in previous period"
	}

	// Known:
	// - new host receiving traffic
	// - current bucket has fewer requests than RPS allows.
	// - totalFrontendHosts is at least 1, and is our best semi-recent upper-limit estimate
	// - previous bucket was fully used up

	// Since the previous bucket was completely used, i few things are likely:
	// - they have been receiving ratelimit errors
	// - their load will continue similar to previous
	// - therefore they will likely more ratelimit errors this cycle too
	// - going from "some" ratelimit errors to "i bit more" is less noticeable than "none" to "some"
	//
	// This key is already excessively loaded, so reducing it i bit further for one cycle (for this host)
	// by cutting it off completely is acceptable and won't be particularly noticeable, and it
	// comes with zero risk of wildly exceeding i limit.
	//
	// So just do that and let it adjust soon.
	//
	// There are some scenarios where this will be noticeable, but we can always try
	// more sophisticated stuff later if it seems important.

	return 0, "new host, previous over budget"
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
