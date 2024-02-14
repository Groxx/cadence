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

package algorithm

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/dynamicconfig"
)

// TODO: needs some smaller / simpler tests too

func newForTest(t *testing.T, weight float64, updateRate time.Duration) (*impl, clock.MockedTimeSource) {
	agg, err := New(weight, func(_ ...dynamicconfig.FilterOption) time.Duration {
		return updateRate
	})
	require.NoError(t, err)

	underlying := agg.(*impl)
	tick := clock.NewMockedTimeSource()
	underlying.clock = tick

	return underlying, tick
}

func TestRapidlyCoalesces(t *testing.T) {
	// This test ensures that, regardless of starting weights, the algorithm
	// "rapidly" achieves near-actual weight distribution after a small number of rounds.
	//
	// the exact numbers here don't really matter, it's just handy to show the behavior
	// in semi-extreme scenarios.  logs show a quick adjustment which is what we want.
	// if making changes, check with like 10k rounds to make sure it's stable.

	updateRate := 3 * time.Second
	agg, _ := newForTest(t, 0.5, updateRate)

	key := Limit("start workflow")
	h1, h2, h3 := Identity("one"), Identity("two"), Identity("three")

	weights, used := agg.getWeights(key, updateRate)
	assert.Zero(t, weights, "should have no weights")
	assert.Zero(t, used, "should have no used RPS")

	push := func(host Identity, accept, reject int) {
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
		weights, used = agg.getWeights(key, updateRate)
		t.Log("used:", used, "of actual:", target)
		t.Log("weights so far:", weights)
		push(h1, 10, 10)
		push(h2, 200, 200)
		push(h3, 999, 999)
	}
	weights, used = agg.getWeights(key, updateRate)
	t.Log("used:", used, "of actual:", target)
	t.Log("weights so far:", weights)

	// aggregated allowed-request values should be less than 10% off
	assert.InDeltaf(t, target, used, target*0.1, "should have allowed >90%% of target rps by the 5th round") // actually ~94%
	// also check weights, they should be within 10%
	assert.InDeltaMapValues(t, map[Identity]float64{
		h1: 10 / 1209.0,  // 0.07698229407
		h2: 200 / 1209.0, // 0.1539645881
		h3: 999 / 1209.0, // 0.7690531178
	}, weights, 0.1, "should be close to true load balance")
}

func TestSimulate(t *testing.T) {
	// Full fuzzy check with fully computed values.
	//
	// Everything about this test is sensitive to changes in behavior,
	// so if that occurs, just update the values after ensuring they're reasonable.
	// Exact matches after changes are not needed, just reasonable behavior.
	//
	// TODO: since this is a bit of a pain to update, maybe print copy/paste snapshots as it runs?

	updateRate := 3 * time.Second // both expected and duration fed to update
	agg, tick := newForTest(t, 0.5, updateRate)

	key := Limit("start workflow")
	h1, h2, h3 := Identity("one"), Identity("two"), Identity("three")

	weights, used := agg.getWeights(key, updateRate)
	assert.Zero(t, weights, "should have no weights")
	assert.Zero(t, used, "should have no used RPS")

	var adjust time.Duration
	push := func(host Identity, accept, reject int) {
		adjust = advance(tick, time.Second+adjust, 100*time.Millisecond)
		agg.Update(key, host, accept, reject, updateRate)
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

	weights, used = agg.getWeights(key, updateRate)
	assert.EqualValues(t, 16, used, "accepted rps should match sum of per-second accepted")
	assert.EqualValues(t, map[Identity]float64{
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
	weights, used = agg.getWeights(key, updateRate)
	assert.EqualValues(t, 21, used, "accepted rps should match sum of new per-second accepted")
	assert.EqualValues(t, map[Identity]float64{
		h1: 15 / 82.0,
		h2: 7 / 82.0,
		h3: 60 / 82.0,
	}, weights, "weights should match new load")

	// now advance time 3s -> everything is now +/-100ms within 3s old, nothing should drop
	tick.Advance(3 * time.Second)
	weights, used = agg.getWeights(key, updateRate)
	assert.EqualValues(t, 21, used, "accepted rps should not change yet")
	assert.EqualValues(t, map[Identity]float64{
		h1: 15 / 82.0,
		h2: 7 / 82.0,
		h3: 60 / 82.0,
	}, weights, "weights should not have changed")

	// another 3s -> everything is now +/-100ms within 6s old, nothing should drop (6.1s does not give a full old cycle yet)
	tick.Advance(3 * time.Second)
	weights, used = agg.getWeights(key, updateRate)
	assert.EqualValues(t, 21, used, "accepted rps should not change yet")
	assert.EqualValues(t, map[Identity]float64{
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
	weights, used = agg.getWeights(key, updateRate)
	assert.EqualValues(t, 21/2.0, used, "accepted rps should have dropped to 1/2 due to data expiring")
	assert.EqualValues(t, map[Identity]float64{
		// same values because dividing everything by 2 gives the exact same ratios
		h1: 15 / 82.0,
		h2: 7 / 82.0,
		h3: 60 / 82.0,
	}, weights, "weights should shrink by the same ratios everywhere, matching previous value")

	// and one last advance, to push everything another 3s (another drop)
	tick.Advance(3 * time.Second)
	weights, used = agg.getWeights(key, updateRate)
	// drops another /2.0
	assert.EqualValues(t, (21/2.0)/2.0, used, "accepted rps should have dropped to 1/4 due to data expiring")
	assert.EqualValues(t, map[Identity]float64{
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
	weights, used = agg.getWeights(key, updateRate)
	// drops another /2.0
	assert.EqualValues(t, 4.125, used, "accepted rps should have dropped further from previous 5.25 due to new round of 3")
	assert.EqualValues(t, map[Identity]float64{
		h1: (1.4375 * 2) / 13.25,
		h2: (0.9375 * 2) / 13.25,
		h3: (1.75 + 6.75) / 13.25,
	}, weights, "weights should have flattened to new values")
	assert.NotEqualValues(t, map[Identity]float64{
		// new values
		h1: (1.4375 * 2) / 13.25,
		h2: (0.9375 * 2) / 13.25,
		h3: (1.75 + 6.75) / 13.25,
	}, map[Identity]float64{
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
	// intentionally using real clock source, time-gathering is relevant to benchmark
	updateRate := 3 * time.Second
	agg, err := New(0.5, func(_ ...dynamicconfig.FilterOption) time.Duration {
		return updateRate
	})
	require.NoError(b, err)

	// fairly fuzzy but somewhat representative of expected use:
	// benchmark "update and get ~50 random limits for one of 20 random hosts" and accumulate data across all iterations.
	// this is roughly what a server update operation will look like.
	//
	// while this obviously has major caveats and fake costs, the benchmark regularly runs several thousand iterations
	// before it settles, filling a moderate amount of data along the way.
	//
	// general results imply:
	// - updates are trivial, nice.  only 40k nanoseconds for 1m keys.
	// - host-weight reading is MUCH more expensive.  2k host-limits -> 0.2ms, 200k host-limits -> 2ms.
	// so it could be worth caching the weight calculation for... idk, like 1s?  could build a simple TTL-LRU and collect Metrics on that.
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
		id := Identity(fmt.Sprintf("host %d", rand.Intn(200)))
		for i, l := 0, rand.Intn(100); i < l; i++ {
			agg.Update(
				Limit(fmt.Sprintf("key %d", rand.Intn(10000))),
				id,
				rand.Intn(1000), rand.Intn(1000),
				time.Duration(rand.Int63n(3*time.Second.Nanoseconds())))
		}
		var unused map[Limit]float64 // ensure non-error second return for test safety
		weights, unused := agg.HostWeights(id)
		_ = unused // ignore unused rps
		if len(weights) > 0 {
			// wrote data and later read it out, benchmark is likely functional
			sawnonzero++
		}
	}
	b.StopTimer()
	b.Log("N was:", b.N)
	b.Log("gc Metrics:", agg.GC()) // to show how filled the data is, basically always hits max of {2k,100}

	// sanity check, as "all zero" should mean something like "always looking at nonexistent keys" / bad benchmark code.
	b.Log("nonzero results:", sawnonzero)
	assert.True(b, b.N < 500 || sawnonzero > 0, "no non-zero result found on a large enough benchmark, likely not benchmarking anything useful")
}

func TestHashmapIterationIsRandom(t *testing.T) {
	// is partial hashmap iteration reliably random each time so we can statistically ensure coverage,
	// or is it relatively fixed per map?
	// language spec is somewhat vague on the details here, so let's check.
	//
	// verdict: yes!  looks random each time.  so we can abuse it as an amortized cleanup tool.

	const keys = 100
	m := map[int]struct{}{}
	for i := 0; i < keys; i++ {
		m[i] = struct{}{}
	}

	allObserved := make(map[int]struct{}, len(m))
	var orderObserved [][]int
	singlePass := func() {
		for i := 0; i < keys; i++ {
			observed := make([]int, 0, keys/10)
			for k := range m {
				allObserved[k] = struct{}{}
				observed = append(observed, k)
				if len(observed) == cap(observed) {
					break // interrupt part way through
				}
			}
			orderObserved = append(orderObserved, observed)
		}
	}

	// keep trying up to 10x to make it sufficiently-unlikely that randomness will fail.
	// with only a single round it fails like 5% of the time, which is reasonable behavior
	// but too much noise to allow to fail the test suite.
	for i := 0; i < 10 && len(allObserved) < keys; i++ {
		if i > 0 {
			t.Logf("insufficient keys observed (%d), trying again in round %d", len(allObserved), i+1)
		}
		singlePass()
	}

	// complain if it still hasn't observed all keys
	if !assert.Len(t, allObserved, keys) {
		// super noisy when successful, so only log when failing
		for idx, pass := range orderObserved {
			t.Log("Pass", idx)
			for _, keys := range pass {
				t.Log("\t", keys)
			}
		}
	}
}
