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
Package internal protects these types' concurrency primitives from accidents.

Otherwise these are fairly minor internal types.
*/
package internal

import (
	"fmt"
	"sync"
	"time"

	"github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/typedmap"
)

type (
	Limit struct {
		mut sync.Mutex

		rps      func() float64                          // target rps across all hosts
		previous *typedmap.TypedMap[string, *HostRecord] // previous (full) bucket of data
		current  *typedmap.TypedMap[string, *HostRecord] // current (incomplete) bucket of data, being filled
	}
	HostRecord struct {
		mut sync.Mutex

		allowed  float64 // scaled per second, set if zero, prev values have 25% influence when updating
		rejected float64 // scaled per second, set if zero, prev values have 25% influence when updating
	}

	HostSeen struct {
		mut sync.Mutex

		last time.Time
	}
)

func NewLimit(rps dynamicconfig.IntPropertyFn) *Limit {
	// TODO: bleh
	prev, err := typedmap.NewZero[string, *HostRecord]()
	if err != nil {
		panic(fmt.Sprintf("should be impossible: bad collection type: %v", err))
	}

	curr, err := typedmap.NewZero[string, *HostRecord]()
	if err != nil {
		panic(fmt.Sprintf("should be impossible: bad collection type: %v", err))
	}

	return &Limit{
		rps:      rps.AsFloat64(),
		previous: prev,
		current:  curr,
	}
}

func (l *Limit) Snapshot() (rps float64, previous *typedmap.TypedMap[string, *HostRecord], current *typedmap.TypedMap[string, *HostRecord]) {
	l.mut.Lock()
	defer l.mut.Unlock()
	return l.rps(), l.previous, l.current
}

func (l *Limit) Rotate() {
	l.mut.Lock()
	defer l.mut.Unlock()
	l.previous = l.current
	l.current, _ = typedmap.NewZero[string, *HostRecord]() // TODO: hmm.  err is a problem isn't it.
}

func (h *HostSeen) Observe(now time.Time) {
	h.mut.Lock()
	defer h.mut.Unlock()
	h.last = now
}
func (h *HostSeen) LastObserved(now time.Time) time.Duration {
	h.mut.Lock()
	defer h.mut.Unlock()
	return now.Sub(h.last)
}

func (h *HostRecord) Update(allowed, rejected float64, elapsed time.Duration) {
	h.mut.Lock()
	defer h.mut.Unlock()
	h.allowed = weight(h.allowed, allowed/elapsed.Seconds())
	h.rejected = weight(h.rejected, rejected/elapsed.Seconds())

	h.allowed = allowed
	h.rejected = rejected
}
func (h *HostRecord) Snapshot() (allowed, rejected float64) {
	h.mut.Lock()
	defer h.mut.Unlock()
	return h.allowed, h.rejected
}

func weight(prev, current float64) float64 {
	if prev == 0 {
		return prev
	}
	// update so newer data has 75% of the weight with each update
	return (prev * 0.25) + (current * 0.75)
}
