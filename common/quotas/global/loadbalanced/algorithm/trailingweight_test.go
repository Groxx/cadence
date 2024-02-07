package algorithm

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/uber/cadence/common/clock"
)

func TestRapidlyCoalesces(t *testing.T) {
	agg, err := New(0.5)
	require.NoError(t, err)

	tick := clock.NewMockedTimeSource()
	agg.clock = tick

	key := limit("start workflow")
	h1, h2, h3 := identity("one"), identity("two"), identity("three")

	weights, used := agg.Weights(key)
	assert.Zero(t, weights, "should have no weights")
	assert.Zero(t, used, "should have no used RPS")

	push := func(host identity, accept, reject int) {
		agg.Update(key, host, accept, reject, time.Second)
	}

	// init with anything <~1000, too large and even a small fraction of the original value can be too big.
	push(h1, rand.Intn(1000), rand.Intn(1000))
	push(h2, rand.Intn(1000), rand.Intn(1000))
	push(h3, rand.Intn(1000), rand.Intn(1000))

	// now update multiple times and make sure it gets to 90% within 4 steps == 12s (normally).
	//
	// 4 steps with 0.5 weight should mean only 0.5^4 => 6.25% of the original influence remains,
	// which feels pretty reasonable: after ~10 seconds (3s updates), the oldest data only has ~10% weight.
	const target = 10 + 200 + 999
	for i := 0; i < 4; i++ {
		weights, used = agg.Weights(key)
		t.Log("used:", used, "of actual:", target)
		t.Log("weights so far:", weights)
		push(h1, 10, 10)
		push(h2, 200, 200)
		push(h3, 999, 999)
	}
	weights, used = agg.Weights(key)
	t.Log("used:", used, "of actual:", target)
	t.Log("weights so far:", weights)

	// aggregated allowed-request values should be less than 10% off
	assert.InDeltaf(t, target, used, target*0.1, "should have allowed >90%% of target rps by the 5th round") // actually ~94%
	// also check weights, they should be within 10%
	assert.InDeltaMapValues(t, map[identity]float64{
		h1: 10 / 1209.0,  // 0.07698229407
		h2: 200 / 1209.0, // 0.1539645881
		h3: 999 / 1209.0, // 0.7690531178
	}, weights, 0.1, "should be close to true load balance")
}

func TestSimulate(t *testing.T) {
	agg, err := New(0.5)
	require.NoError(t, err)
	tick := clock.NewMockedTimeSource()
	agg.clock = tick

	key := limit("start workflow")
	h1, h2, h3 := identity("one"), identity("two"), identity("three")

	weights, used := agg.Weights(key)
	assert.Zero(t, weights, "should have no weights")
	assert.Zero(t, used, "should have no used RPS")

	var adjust time.Duration
	push := func(host identity, accept, reject int) {
		adjust = advance(tick, time.Second+adjust, 100*time.Millisecond)
		agg.Update(key, host, accept, reject, 3*time.Second)
	}

	// push in a random order
	data := []func(){
		func() { push(h1, 5*3, 5*3) },    // 10 rps, 5 accepted
		func() { push(h2, 1*3, 1*3) },    // 2 rps, 1 accepted
		func() { push(h3, 10*3, 100*3) }, // 110 rps, 10 accepted
	}
	rand.Shuffle(len(data), func(i, j int) {
		data[i], data[j] = data[j], data[i]
	})
	for _, d := range data {
		d()
	}
	/*
		rps after updates comes to:
		h1:
			accept: 5
			reject: 5
		h2:
			accept: 1
			reject: 1
		h3:
			accept: 10
			reject: 100
		total: 122 (sum of all)
		accepted: 16 (sum of accept)
	*/

	weights, used = agg.Weights(key)
	assert.EqualValues(t, 16, used, "accepted rps should match sum of per-second accepted")
	assert.EqualValues(t, map[identity]float64{
		h1: 10 / 122.0,
		h2: 2 / 122.0,
		h3: 110 / 122.0,
	}, weights, "weights should be similar")

	// push some more data, in a probably-different order.
	// total time is less than 2*3s so none should shrink.
	data = []func(){
		func() { push(h1, 10*3, 10*3) }, // 20 rps, 10 accepted
		func() { push(h2, 6*3, 6*3) },   // 12 rps, 6 accepted
		func() { push(h3, 10*3, 0) },    // 10 rps, 10 accepted
	}
	rand.Shuffle(len(data), func(i, j int) {
		data[i], data[j] = data[j], data[i]
	})
	for _, d := range data {
		d()
	}
	/*
		rps after updates comes to:
		h1:
			accept: 5 + 10 => 7.5
			reject: 5 + 10 => 7.5
		h2:
			accept: 1 + 6 => 3.5
			reject: 1 + 6 => 3.5
		h3:
			accept: 10 + 10 => 10
			reject: 100 + 0 => 50
		total: 122 => 82 (sum of all)
		accept: 16 => 21 (sum of accept)
	*/
	weights, used = agg.Weights(key)
	assert.EqualValues(t, 21, used, "accepted rps should match sum of new per-second accepted")
	assert.EqualValues(t, map[identity]float64{
		h1: 15 / 82.0,
		h2: 7 / 82.0,
		h3: 60 / 82.0,
	}, weights, "weights should match new load")

	// now advance time 3s -> everything is now +/-100ms within 3s old, nothing should drop
	tick.Advance(3 * time.Second)
	weights, used = agg.Weights(key)
	assert.EqualValues(t, 21, used, "accepted rps should not change yet")
	assert.EqualValues(t, map[identity]float64{
		h1: 15 / 82.0,
		h2: 7 / 82.0,
		h3: 60 / 82.0,
	}, weights, "weights should not have changed")

	// another 3s -> everything is now +/-100ms within 6s old, nothing should drop (6.1s does not give a full old cycle yet)
	tick.Advance(3 * time.Second)
	weights, used = agg.Weights(key)
	assert.EqualValues(t, 21, used, "accepted rps should not change yet")
	assert.EqualValues(t, map[identity]float64{
		h1: 15 / 82.0,
		h2: 7 / 82.0,
		h3: 60 / 82.0,
	}, weights, "weights should not have changed")

	// a final 3s -> everything is now +/-100ms within 9s old...
	tick.Advance(3 * time.Second)
	// ... and advance a bit more so all data is guaranteed past 9s (but <10s where the oldest could drop again)
	tick.Advance(200 * time.Millisecond)
	/*
		h1: 7.5/7.5 => 3.75/3.75
		h2: 3.5/3.5 => 1.75/1.75
		h3:  10/50  =>    5/25
		total: 122 => 82 => 41 ("sum of all" or "half of prev" are both semantically correct)
		accept: 16 => 21 => 10.5 (same, reduce by half)
	*/
	weights, used = agg.Weights(key)
	assert.EqualValues(t, 21/2.0, used, "accepted rps should have dropped to 1/2 due to data expiring")
	assert.EqualValues(t, map[identity]float64{
		// same values because dividing everything by 2 gives the exact same ratios
		h1: 15 / 82.0,
		h2: 7 / 82.0,
		h3: 60 / 82.0,
	}, weights, "weights should shrink by the same ratios everywhere, matching previous value")

	// and one last advance, to push everything another 3s (another drop)
	tick.Advance(3 * time.Second)
	weights, used = agg.Weights(key)
	// drops another /2.0
	assert.EqualValues(t, (21/2.0)/2.0, used, "accepted rps should have dropped to 1/4 due to data expiring")
	assert.EqualValues(t, map[identity]float64{
		// same weights because they all reduced again
		h1: 15 / 82.0,
		h2: 7 / 82.0,
		h3: 60 / 82.0,
	}, weights, "weights should shrink by the same ratios everywhere, matching previous value")

	// now update again, it should resume from these old-reduced values.
	// since this needs to happen in the same ~700ms window or the oldest might drop further, just update sync.
	// numbers will be ugly but eh.  that's fine.
	data = []func(){
		func() { agg.Update(key, h1, 3, 3, 3*time.Second) }, // 2 rps, 1 accepted
		func() { agg.Update(key, h2, 3, 3, 3*time.Second) },
		func() { agg.Update(key, h3, 3, 3, 3*time.Second) },
	}
	rand.Shuffle(len(data), func(i, j int) {
		data[i], data[j] = data[j], data[i]
	})
	for _, d := range data {
		d()
	}
	/*
		a numerically-ugly round, sorry.
		each value is 1/4 of the last example, due to two rounds of implied zeros,
		and then add a 1/1 round:

		h1: 1.875/1.875 + 1/1 => 1.4375/1.4375
		h2: 0.875/0.875 + 1/1 => 0.9375/0.9375
		h3:   2.5/12.5  + 1/1 =>   1.75/6.75
		total: 122 => 82 => 41   => 13.25
		accept: 16 => 21 => 10.5 => 4.125
	*/
	weights, used = agg.Weights(key)
	// drops another /2.0
	assert.EqualValues(t, 4.125, used, "accepted rps should have dropped further from previous 5.25 due to new round of 3")
	assert.EqualValues(t, map[identity]float64{
		h1: (1.4375 * 2) / 13.25,
		h2: (0.9375 * 2) / 13.25,
		h3: (1.75 + 6.75) / 13.25,
	}, weights, "weights should have flattened to new values")
	assert.NotEqualValues(t, map[identity]float64{
		// new values
		h1: (1.4375 * 2) / 13.25,
		h2: (0.9375 * 2) / 13.25,
		h3: (1.75 + 6.75) / 13.25,
	}, map[identity]float64{
		// should NOT match old values
		h1: 15 / 82.0,
		h2: 7 / 82.0,
		h3: 60 / 82.0,
	}, "ratios should have changed due to a round of evenly-balanced load") // sanity check as ratios are hard to eyeball
}

// advances a mocked time source by a target time, with a fuzzy variation, and returns
// the skew so future calculations can continue to center around predictable times (just add to the next target time).
func advance(tick clock.MockedTimeSource, target, spread time.Duration) time.Duration {
	actual := fuzzy(target, spread)
	tick.Advance(actual)
	return target - actual
}

// fuzzy offsets a number by positive or negative the passed spread value
func fuzzy[T numeric](d T, spread T) T {
	return T(float64(d) + ((rand.Float64() - 0.5) * 2 * float64(spread)))
}

func BenchmarkAggregator_Update(b *testing.B) {
	agg, err := New(0.5)
	require.NoError(b, err)

	// fairly fuzzy but somewhat representative:
	// benchmark "update and get ~50 random limits for one of 20 random hosts" and accumulate data across all iterations.
	// this is roughly what a server update operation will look like.
	//
	// while this obviously has major caveats and fake costs, the benchmark regularly runs several thousand iterations
	// before it settles, filling a moderate amount of data along the way.
	//
	// general results imply:
	// - updates are trivial, nice.  only 40k nanoseconds for 1m keys.
	// - host-weight reading is MUCH more expensive.  2k host-limits -> 0.2ms, 200k host-limits -> 2ms.
	// so it could be worth caching the weight calculation for... idk, like 1s?  could build a simple TTL-LRU and collect metrics on that.
	//
	// results: this cuts cost about in half with perfect cache retention, which is depressingly low.
	// 	weightcache: cache.New(&cache.Options{
	// 		TTL:             time.Second,
	// 		InitialCapacity: 128,
	// 		MaxCount:        100000,
	// 		ActivelyEvict:   false,
	// 	}),
	// very likely that's over-complicated though, and I could get much better results from a drastically-simplified cache.
	// e.g. statistical cleanup is likely good enough.  make sure map iteration is fair each time though.

	sawnonzero := 0
	for i := 0; i < b.N; i++ {
		id := identity(fmt.Sprintf("host %d", rand.Intn(200)))
		for i, l := 0, rand.Intn(100); i < l; i++ {
			agg.Update(
				limit(fmt.Sprintf("key %d", rand.Intn(10000))),
				id,
				rand.Intn(1000), rand.Intn(1000),
				time.Duration(rand.Int63n(3*time.Second.Nanoseconds())))
		}
		weights := agg.HostWeights(id)
		if len(weights) > 0 {
			// wrote data and later read it out, benchmark is likely functional
			sawnonzero++
		}
	}
	b.StopTimer()
	b.Log("N was:", b.N)
	b.Log("gc metrics:", agg.GC()) // to show how filled the data is, basically always hits max of {2k,100}
	b.Log("cache size:", agg.weightcache.Size())

	// sanity check, as "all zero" should mean something like "always looking at nonexistent keys" / bad benchmark code.
	b.Log("nonzero results:", sawnonzero)
	assert.True(b, b.N < 500 || sawnonzero > 0, "no non-zero result found on a large enough benchmark, likely not benchmarking anything useful")
}
