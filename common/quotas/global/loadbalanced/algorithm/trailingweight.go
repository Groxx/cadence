package algorithm

import (
	"fmt"
	"math"
	"time"

	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/clock"
)

/*
plan:
- implement a running weighted distribution per host
- detect update rate and use to handle degrading values
- prune when below a value
- offer at max [(remaining unused quota / num unused hosts) * 2] to unrecognized hosts

base assumption:
- more ratelimits than hosts (more domains than hosts * multiple limits per domain)
- limits are relatively stable per host
  - i.e. keys are fairly stable, and weights per domain are pretty stable, and they change relatively slowly compared to check-in cadence
- each limit should check all hosts
  - needed for weight calculation
- limits can be ignored until they're checked
  - can defer zero-accounting until it's loaded
  - can clean up statistically if needed
  - background cleanup may also be worth doing

*/

/*
Overall concept is pretty simple:
- assume there are many more limits than frontend hosts, so the top-level key should be limit to reduce cardinality as fast as possible.
- per limit per host, maintain a rolling average of the last few updates.
- when figuring out [weight of host X in limit Y], just compute the limit's current weight for that host and return it.  it'll eventually become more correct.
- don't try to be clever with locks, many mutexes or atomics can cost much more than one coarse one.  benchmark before optimizing.
- garbage-collect as you go, removing things with >10x no updates.
*/

// history holds the
type history struct {
	lastUpdate         time.Time // only so we know if elapsed times are exceeded, not used to compute per-second rates
	accepted, rejected persecond // requests received, per second (conceptually divided by update rate)
}

type identity string
type limit string

type aggregator struct {
	updateRateMin        time.Duration // do not drop update rate below this value (likely 3s is fine)
	updateRateDecayAfter float64       // multiplier to decide how many min-updates to miss before decay (i.e. count zeros from non-reporting hosts)
	updateWeight         float64       // weight of new information
	gcAfter              float64       // multiplier to decide how many min-updates to miss before deleting data

	weightcache cache.Cache

	usage map[limit]map[identity]history

	clock clock.TimeSource
}

const (
	guessNumKeys   = 1024            // guesstimate at num of ratelimit keys in a cluster
	guessHostCount = 32              // guesstimate at num of frontend hosts in a cluster that receive traffic for each key
	updateRateMin  = 3 * time.Second // TODO: configurable
)

func New(newDataWeight float64) (*aggregator, error) {
	if newDataWeight <= 0 || newDataWeight > 1 {
		return nil, fmt.Errorf("bad weight, must be 0..1, got: %v", newDataWeight)
	}
	return &aggregator{
		updateRateMin:        updateRateMin,                                      // treat updates faster than this as if they came in at a normal rate
		updateRateDecayAfter: 2.0,                                                // can miss 2 updates before considering a host dead (could eliminate with ringpop)
		updateWeight:         newDataWeight,                                      // each update moves values 50%, must be <=1 or math explodes
		gcAfter:              20,                                                 // gc after 20 missed updates (1m)
		usage:                make(map[limit]map[identity]history, guessNumKeys), // start out relatively large
		weightcache: cache.New(&cache.Options{
			TTL:             time.Second,
			InitialCapacity: 128,
			MaxCount:        100000,
			ActivelyEvict:   false,
		}),

		clock: clock.NewRealTimeSource(),
	}, nil
}

type persecond float64

// Update performs a weighted update to the running RPS for this host's limit key
func (a *aggregator) Update(key limit, id identity, accepted, rejected int, elapsed time.Duration) {
	ih := a.usage[key]
	if ih == nil {
		ih = make(map[identity]history, guessHostCount)
	}

	var next history
	prev := ih[id]
	now := a.clock.Now()
	aps := persecond(float64(accepted) / float64(elapsed/time.Second))
	rps := persecond(float64(rejected) / float64(elapsed/time.Second))
	if prev.lastUpdate.IsZero() {
		next = history{
			lastUpdate: now,
			accepted:   aps, // no history == 100% weight
			rejected:   rps, // no history == 100% weight
		}
	} else {
		// account for missed updates.
		// ignoring gc as we are updating, and a very-low reduce just means similar results to zero data.
		reduce, _ := a.missedUpdateScalar(now.Sub(prev.lastUpdate))

		next = history{
			lastUpdate: now,
			accepted:   weighted(aps, prev.accepted*reduce, a.updateWeight),
			rejected:   weighted(rps, prev.rejected*reduce, a.updateWeight),
		}
	}

	ih[id] = next
	a.usage[key] = ih
}

// Weights returns the weights of observed hosts (based on ALL requests), and the total number of requests accepted per second.
//
// Callers should use this to determine:
// - existing identities get their weight-of-the-ratelimit
// - new identities can only consume some portion of remaining-in-ratelimit (accepted rps), and they will auto-adjust to be more accurate on the next cycle
func (a *aggregator) Weights(key limit) (weights map[identity]float64, accepted float64) {
	if res, ok := a.weightcache.Get(key).(map[identity]float64); ok {
		return res, 0
	}

	ih := a.usage[key]
	if len(ih) == 0 {
		return
	}

	now := a.clock.Now()

	weights = make(map[identity]float64, len(ih))
	total := persecond(0.0)
	for id, history := range ih {
		// account for missed updates
		reduce, gc := a.missedUpdateScalar(now.Sub(history.lastUpdate))
		if gc {
			// old, clean up
			delete(ih, id)
			continue
		}

		actual := (history.accepted + history.rejected) * reduce
		weights[id] = float64(actual) // populate with the reduced values so it doesn't have to be calculated again (TODO: perf compared to re-calculate?)

		total += actual // keep a running total to adjust all values when done
		accepted += float64(history.accepted * reduce)
	}

	if len(ih) == 0 {
		// completely empty limit, gc it as well
		delete(a.usage, key)
		return nil, 0
	}

	for id := range ih {
		// scale by the total
		weights[id] = weights[id] / float64(total)
	}
	a.weightcache.Put(key, weights)
	return weights, accepted
}

// HostWeights returns the weighted limits for all known keys for this host.
//
// Callers should use this to determine:
// - existing identities get their weight-of-the-ratelimit
// - new identities can only consume some portion of remaining-in-ratelimit (accepted rps), and they will auto-adjust to be more accurate on the next cycle
func (a *aggregator) HostWeights(host identity) (weights map[limit]float64) {
	// TODO: rather slow, 100,000ns or so.  worth doing something about it?
	// e.g. what about a host-to-limit map to reduce the keys scanned?
	// - ... would that even be useful if we assume balanced load?  maybe internally it'd cut out like 3/4?
	weights = make(map[limit]float64, len(a.usage))
	for key, data := range a.usage {
		_, ok := data[host]
		if ok {
			computed, consumed := a.Weights(key)
			_ = consumed // TODO: probably useful
			weights[key] = computed[host]
		}
	}
	return weights
}

type metrics struct {
	hostLimits, limits       int
	oldHostLimits, oldLimits int
}

func (a *aggregator) GC() metrics {
	m := metrics{}
	now := a.clock.Now()
	for lim, dat := range a.usage {
		for host, hist := range dat {
			if _, gc := a.missedUpdateScalar(now.Sub(hist.lastUpdate)); gc {
				// clean up stale host data within limits
				delete(dat, host)
				m.oldHostLimits++
			} else {
				m.hostLimits++
			}
		}

		// clean up stale limits
		if len(dat) == 0 {
			delete(a.usage, lim)
			m.oldLimits++
		} else {
			m.limits++
		}
	}

	return m
}

// missedUpdateScalar returns an amount to multiply old RPS data by, to account for missed updates outside SLA.
func (a *aggregator) missedUpdateScalar(elapsed time.Duration) (scalar persecond, gc bool) {
	reduce := persecond(1.0)

	// fast path: check the bounds for "new enough to ignore"
	if elapsed <= time.Duration(float64(a.updateRateMin)*a.updateRateDecayAfter) {
		return reduce, false // within SLA, no changes
	}
	// fast path: check the bounds for "old enough to prune"
	if elapsed > time.Duration(float64(a.updateRateMin)*a.gcAfter) {
		return reduce, true
	}

	// slow path: account for missed updates by simulating 0-value updates.

	// calculate missed updates, and compute an exponential decay as if we had kept track along the way
	missed := float64(elapsed) / float64(a.updateRateMin) // how many missed updates (fractional, 1 == one full duration passed, 2.5 == 2.5 durations passed, etc)
	missed -= a.updateRateDecayAfter                      // ignore updates until crossing the decay-after threshold
	missed = max(0, missed)                               // guarantee ignoring negatives (they are below the decay-after threshold, should be cut off by fast path too)
	if missed > 1 {
		// missed at least one update period beyond decay-after, calculate an exponential decay to the old values.
		// floor to an int when doing so because:
		// - precision isn't important
		// - tests are a bit easier (more stable / less crazy-looking values)
		// - as a bonus freebie: integer exponents are typically faster to compute
		reduce = persecond(math.Pow(a.updateWeight, math.Floor(missed)))
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
