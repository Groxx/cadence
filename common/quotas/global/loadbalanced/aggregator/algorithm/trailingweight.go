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

// Package algorithm contains a weighted average calculator for ratelimits.
// It is unaware of RPC or desired limits, it just tracks the observed rates of requests.
package algorithm

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/dynamicconfig"
)

type (
	// history holds the running per-second running average for a single key from a single host, and the last time it was updated.
	history struct {
		lastUpdate         time.Time // only so we know if elapsed times are exceeded, not used to compute per-second rates
		accepted, rejected PerSecond // requests received, per second (conceptually divided by update rate)
	}

	// Identity is an arbitrary (stable) identifier for a host that is accepting requests.
	Identity string
	// Limit is the key being used to Limit requests.
	Limit string
	// PerSecond represents a float64 already scaled to per-second values.
	PerSecond float64

	impl struct {
		mut sync.Mutex

		updateRateMin        dynamicconfig.DurationPropertyFn // do not drop update rate below this value (likely 3s is fine, should match caller rate)
		updateRateDecayAfter float64                          // multiplier to decide how many min-updates to miss before decay (i.e. count zeros from non-reporting hosts)
		updateWeight         float64                          // weight of new information
		gcAfter              float64                          // multiplier to decide how many min-updates to miss before deleting data

		usage map[Limit]map[Identity]history

		clock clock.TimeSource
	}

	// Metrics reports overall counts discovered as part of garbage collection.
	//
	// This is not necessarily worth maintaining verbatim, but while it's easy to
	// collect it might give us some insight into overall behavior.
	Metrics struct {
		// HostLimits is the number of per-host limits that remain after cleanup
		HostLimits int
		// Limits is the number of cross-host limits that remain after cleanup
		Limits int

		// RemovedHostLimits is the number of per-host limits that were removed as part of this GC pass
		RemovedHostLimits int
		// RemovedLimits is the number of cross-host limits that were removed as part of this GC pass
		RemovedLimits int
	}

	// WeightedAverage returns aggregated RPS data for its Limit keys, weighted across the passed host Identity names.
	WeightedAverage interface {
		Update(key Limit, id Identity, accepted, rejected int, elapsed time.Duration)

		// HostWeights returns the per-[Limit] weights for all requested + known keys for this Identity,
		// as well as the Limit's overall used RPS (to decide RPS to allow for new hosts).
		HostWeights(host Identity, limits []Limit) (weights map[Limit]float64, usedRPS map[Limit]float64)

		// GC can be called periodically to prune old ratelimits.
		//
		// Limit keys that are accessed normally will automatically collect themselves, so this may be unnecessary.
		GC() Metrics
	}
)

const (
	guessNumKeys   = 1024 // guesstimate at num of ratelimit keys in a cluster
	guessHostCount = 32   // guesstimate at num of frontend hosts in a cluster that receive traffic for each key
)

// New returns a host-weight aggregator.
//
// Each aggregator is single-threaded and does no internal locking to keep its use flexible,
// but as each key is independent you can just create multiple aggregators (hash the keys maybe) to reduce contention.
func New(newDataWeight float64, updateRateMin dynamicconfig.DurationPropertyFn) (WeightedAverage, error) {
	if newDataWeight <= 0 || newDataWeight > 1 {
		return nil, fmt.Errorf("bad weight, must be 0..1, got: %v", newDataWeight)
	}
	return &impl{
		updateRateMin:        updateRateMin,                                      // treat updates faster than this as if they came in at a normal rate
		updateRateDecayAfter: 2.0,                                                // can miss 2 updates before considering a host dead (could eliminate with ringpop)
		updateWeight:         newDataWeight,                                      // each update moves values 50%, must be <=1 or math explodes
		gcAfter:              20,                                                 // gc after 20 missed updates (1m)
		usage:                make(map[Limit]map[Identity]history, guessNumKeys), // start out relatively large

		clock: clock.NewRealTimeSource(),
	}, nil
}

// Update performs a weighted update to the running RPS for this host's Limit key
func (a *impl) Update(key Limit, id Identity, accepted, rejected int, elapsed time.Duration) {
	a.mut.Lock()
	defer a.mut.Unlock()

	ih := a.usage[key]
	if ih == nil {
		ih = make(map[Identity]history, guessHostCount)
	}

	var next history
	prev := ih[id]
	now := a.clock.Now()
	aps := PerSecond(float64(accepted) / float64(elapsed/time.Second))
	rps := PerSecond(float64(rejected) / float64(elapsed/time.Second))
	if prev.lastUpdate.IsZero() {
		next = history{
			lastUpdate: now,
			accepted:   aps, // no history == 100% weight
			rejected:   rps, // no history == 100% weight
		}
	} else {
		// account for missed updates.
		// ignoring gc as we are updating, and a very-low reduce just means similar results to zero data.
		minRate := a.updateRateMin()
		reduce, _ := a.missedUpdateScalar(now.Sub(prev.lastUpdate), minRate)

		next = history{
			lastUpdate: now,
			accepted:   weighted(aps, prev.accepted*reduce, a.updateWeight),
			rejected:   weighted(rps, prev.rejected*reduce, a.updateWeight),
		}
	}

	ih[id] = next
	a.usage[key] = ih
}

// getWeights returns the weights of observed hosts (based on ALL requests), and the total number of requests accepted per second.
func (a *impl) getWeights(key Limit, minRate time.Duration) (weights map[Identity]float64, usedRPS float64) {
	ih := a.usage[key]
	if len(ih) == 0 {
		return
	}

	now := a.clock.Now()

	weights = make(map[Identity]float64, len(ih))
	total := PerSecond(0.0)
	for id, history := range ih {
		// account for missed updates
		reduce, gc := a.missedUpdateScalar(now.Sub(history.lastUpdate), minRate)
		if gc {
			// old, clean up
			delete(ih, id)
			continue
		}

		actual := (history.accepted + history.rejected) * reduce
		weights[id] = float64(actual) // populate with the reduced values so it doesn't have to be calculated again (TODO: perf compared to re-calculate?)

		total += actual // keep a running total to adjust all values when done
		usedRPS += float64(history.accepted * reduce)
	}

	if len(ih) == 0 {
		// completely empty Limit, gc it as well
		delete(a.usage, key)
		return nil, 0
	}

	for id := range ih {
		// scale by the total
		weights[id] = weights[id] / float64(total)
	}
	return weights, usedRPS
}

func (a *impl) HostWeights(host Identity, limits []Limit) (weights map[Limit]float64, usedRPS map[Limit]float64) {
	a.mut.Lock()
	defer a.mut.Unlock()

	weights = make(map[Limit]float64, len(limits))
	usedRPS = make(map[Limit]float64, len(limits))
	minRate := a.updateRateMin()
	for _, lim := range limits {
		hosts, used := a.getWeights(lim, minRate)
		if len(hosts) > 0 {
			usedRPS[lim] = used // limit is known, has some usage
			if weight, ok := hosts[host]; ok {
				weights[lim] = weight // host has a known weight
			}
		}
	}
	return weights, usedRPS
}

func (a *impl) GC() Metrics {
	a.mut.Lock()
	defer a.mut.Unlock()

	m := Metrics{}
	now := a.clock.Now()
	minRate := a.updateRateMin()
	// TODO: too costly? can check the first-N% each time and it'll eventually visit all keys, demonstrated in tests.
	for lim, dat := range a.usage {
		for host, hist := range dat {
			if _, gc := a.missedUpdateScalar(now.Sub(hist.lastUpdate), minRate); gc {
				// clean up stale host data within limits
				delete(dat, host)
				m.RemovedHostLimits++
			} else {
				m.HostLimits++
			}
		}

		// clean up stale limits
		if len(dat) == 0 {
			delete(a.usage, lim)
			m.RemovedLimits++
		} else {
			m.Limits++
		}
	}

	return m
}

// missedUpdateScalar returns an amount to multiply old RPS data by, to account for missed updates outside SLA.
func (a *impl) missedUpdateScalar(elapsed time.Duration, minRate time.Duration) (scalar PerSecond, gc bool) {
	reduce := PerSecond(1.0)

	// fast path: check the bounds for "new enough to ignore"
	if elapsed <= time.Duration(float64(minRate)*a.updateRateDecayAfter) {
		return reduce, false // within SLA, no changes
	}
	// fast path: check the bounds for "old enough to prune"
	if elapsed > time.Duration(float64(minRate)*a.gcAfter) {
		return reduce, true
	}

	// slow path: account for missed updates by simulating 0-value updates.

	// calculate missed updates, and compute an exponential decay as if we had kept track along the way
	missed := float64(elapsed) / float64(minRate) // how many missed updates (fractional, 1 == one full duration passed, 2.5 == 2.5 durations passed, etc)
	missed -= a.updateRateDecayAfter              // ignore updates until crossing the decay-after threshold
	missed = max(0, missed)                       // guarantee ignoring negatives (they are below the decay-after threshold, should be cut off by fast path too)
	if missed > 1 {
		// missed at least one update period beyond decay-after, calculate an exponential decay to the old values.
		// floor to an int when doing so because:
		// - precision isn't important
		// - tests are a bit easier (more stable / less crazy-looking values)
		// - as a bonus freebie: integer exponents are typically faster to compute
		reduce = PerSecond(math.Pow(a.updateWeight, math.Floor(missed)))
	}

	return reduce, false
}

func max[T numeric](a, b T) T {
	if a > b {
		return a
	}
	return b
}

func weighted[T numeric](newer, older T, weight float64) T {
	return T((float64(newer) * weight) +
		(float64(older) * (1 - weight)))
}

type numeric interface {
	~int | ~int64 | ~float64
}
