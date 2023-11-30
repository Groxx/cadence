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

/*
Package aggregator blah

TODO: deserialize types.Any data, maintain a small sliding window of data per key, report back to callers.

strategy might be:
  - keep 10 (configurable) reporting periods (10s, configurable) in a cyclic buffer
  - per non-zero + per host in buffer, compute num of total requests per host (weight allow vs block?)
  - next update request updates data, recomputes host's share of requests, returns

start simple but high mem?
  - keep per-host data for N periods
  - new reports go into newest bucket, are ignored
  - take last 3 periods, compute weighted average of total traffic for this host (50%, 25%, 25%) ignoring its own zeros (sum others across zeros)
  - return max(1/N hosts, weight) to always allow a minimum through.  pool will re-balance soon.
  - ... make sure already-allowed is tracked so others do not exceed?

how do I handle "burst -> new host -> burst -> new host -> burst"?
  - across N periods, this would say "3 hosts each used 1/3rd" and the new 4th host gets a minimum of 1/N
  - next period, 4th host gets 50% as older 3 hosts had no activity (beyond time cutoff?)
  - next period, 4th host gets 75%
  - next period, 4th host gets 100%?

maybe:
  - compute total from previous N periods
  - compute proportion of latest update that this update would have consumed (can be >100%)
  - subtract already-used-this-period proportion from other updates
  - return whatever is left

this period: zeros are possible, as is 100%
next period: it'll get whatever proportion it contributed to last time

bah!  where do limits get considered, not just proportion?

eeehhhh... see what muttley does.  that can probably be copied directly.

muttley!
- aggregates exactly like I'm doing here
- returns only "drop %" information to limiting-frontends
- limiting-frontends do not run rate.Limiter instances at all!  they just allow spikes through, fix it on the next 3s cycle.
- every limiting-frontend gets the same % because that's fine for this kind of calc

so yea, we don't want to mimic that.  we'd rather reject traffic than allow infinite spikes.
*/

package aggregator

import (
	"context"
	"fmt"
	"time"

	"github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/aggregator/internal"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/rpc"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/typedmap"
	"github.com/uber/cadence/common/types"
)

/*
Plan:
- limiter always sends all non-zero data
- agg always responds with all data
- agg prunes after 60s of no updates

agg stores:
- numbers per host
- toggles between "updating" and "using to decide" buckets every 10s

agg decides:
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
	Agg struct {
		limits *typedmap.TypedMap[string, *internal.Limit]

		// used to estimate number of frontend hosts
		// TODO: replace with frontend ringpop resolver for a much more accurate count
		hostsLastSeen *typedmap.TypedMap[string, *internal.HostSeen]

		rotateRate dynamicconfig.DurationPropertyFn

		// for tests
		now func() time.Time

		// lifecycle management
		ctx     context.Context
		cancel  func()
		stopped chan struct{}
	}
)

func New(rps dynamicconfig.IntPropertyFn, updateRate dynamicconfig.DurationPropertyFn) *Agg {
	limits, err := typedmap.New(func(key string) *internal.Limit {
		return internal.NewLimit(rps)
	})
	if err != nil {
		panic(fmt.Sprintf("should be impossible: bad collection type: %v", err))
	}

	lastseen, err := typedmap.NewZero[string, *internal.HostSeen]()
	if err != nil {
		panic(fmt.Sprintf("should be impossible: bad collection type: %v", err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Agg{
		limits:        limits,
		hostsLastSeen: lastseen,
		rotateRate:    updateRate,
		ctx:           ctx,
		cancel:        cancel,
		stopped:       make(chan struct{}),
	}
}

func (a *Agg) Start() {
	go func() {
		defer close(a.stopped)
		// TODO: panic safety

		tickRate := a.rotateRate()
		tick := time.NewTicker(tickRate)
		defer tick.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return // shutting down
			case <-tick.C:
				// update tick-rate if it changed
				newTickRate := a.rotateRate()
				if tickRate != newTickRate {
					tickRate = newTickRate
					tick.Reset(newTickRate)
				}

				// rotate buckets
				a.rotate()
			}
		}
	}()
}

func (a *Agg) Stop(ctx context.Context) error {
	a.cancel()
	select {
	case <-a.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Agg) rotate() {
	// switch ratelimit buckets
	a.limits.Range(func(k string, v *internal.Limit) bool {
		v.Rotate()
		return true
	})

	// prune known limiting hosts
	now := a.now()
	a.hostsLastSeen.Range(func(k string, v *internal.HostSeen) bool {
		if v.LastObserved(now) > time.Minute { // TODO: configurable
			a.hostsLastSeen.Delete(k)
		}
		return true
	})
}

// Update adds load information for the passed keys to this aggregator.
func (a *Agg) Update(host string, elapsed time.Duration, load map[string]types.RatelimitLoad) {
	// refresh the host-seen record
	a.observeHost(host)

	for limit, data := range load {
		_, _, current := a.limits.Load(limit).Snapshot()
		thishost := current.Load(host)
		_ = data // TODO: pull from data
		allowed := 1.0
		rejected := 1.0
		thishost.Update(allowed, rejected, elapsed)
	}
}

// Get retrieves the known load / desired RPS for this host for this key, as a read-only operation.
func (a *Agg) Get(host string, keys []string) map[string]types.RatelimitAdjustment {
	// refresh the host-seen record
	a.observeHost(host)

	result := make(map[string]types.RatelimitAdjustment, len(keys))
	for _, limit := range keys {
		rps, previous, current := a.limits.Load(limit).Snapshot()

		if previous.Len() == 0 {
			// warming up, return nothing
			continue
		}

		allowed, reason := a.getLimit(host, rps, current, previous)
		_ = reason // TODO: metrics for sure

		if allowed < 0 {
			// warming up, return nothing
			continue
		}

		forRPC := rpc.AdjustmentToAny(allowed)
		result[limit] = types.RatelimitAdjustment{
			Any: forRPC,
		}
	}

	return result
}

func (a *Agg) observeHost(host string) {
	seen := a.hostsLastSeen.Load(host)
	seen.Observe(a.now())
}

func (a *Agg) getLimit(host string, rps float64, current, previous *typedmap.TypedMap[string, *internal.HostRecord]) (allowed float64, reason string) {
	previousTotalRPS := 0.0
	previousThisHostRPS := 0.0
	previousZeroHosts := 0
	previousNonzeroHosts := 0
	// keep track of how many hosts we've seen, for estimation later
	previousHostnames := make(map[string]struct{}, previous.Len())
	previous.Range(func(k string, v *internal.HostRecord) bool {
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
		return true
	})

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
	updatingHostnames := make(map[string]struct{}, current.Len())
	current.Range(func(k string, v *internal.HostRecord) bool {
		updatingHostnames[k] = struct{}{}

		allowed, rejected := v.Snapshot()
		updatingTotalRPS += allowed + rejected

		// TODO: scale to estimate by next rotation?

		return true
	})

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
		// and use that to estimate a safe proportion of the total rps.
		totalFrontendHosts := a.hostsLastSeen.Len()
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

	// Since the previous bucket was completely used, a few things are likely:
	// - they have been receiving ratelimit errors
	// - their load will continue similar to previous
	// - therefore they will likely more ratelimit errors this cycle too
	// - going from "some" ratelimit errors to "a bit more" is less noticeable than "none" to "some"
	//
	// This key is already excessively loaded, so reducing it a bit further for one cycle (for this host)
	// by cutting it off completely is acceptable and won't be particularly noticeable, and it
	// comes with zero risk of wildly exceeding a limit.
	//
	// So just do that and let it adjust soon.
	//
	// There are some scenarios where this will be noticeable, but we can always try
	// more sophisticated stuff later if it seems important.

	return 0, "new host, previous over budget"
}

func (a *Agg) GetAll(host string) map[string]types.RatelimitAdjustment {
	a.observeHost(host)

	// allocate space plus a bit of buffer for additions while ranging
	result := make(map[string]types.RatelimitAdjustment, int(float64(a.limits.Len())*1.1))
	a.limits.Range(func(k string, v *internal.Limit) bool {
		rps, previous, current := v.Snapshot()

		allowed, reason := a.getLimit(host, rps, current, previous)
		_ = reason // TODO: metrics for sure

		forRPC := rpc.AdjustmentToAny(allowed) // TODO: hmm.  feels bad in here maybe?
		result[k] = types.RatelimitAdjustment{
			Any: forRPC,
		}
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
