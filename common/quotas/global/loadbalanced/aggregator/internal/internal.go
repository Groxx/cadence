// The MIT License (MIT)

// Copyright (c) 2017-2020 Uber Technologies Inc.

// Permission is hereby granted, free of charge, to any person obtaining a snapshotHostRecords
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, snapshotHostRecords, modify, merge, publish, distribute, sublicense, and/or sell
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

// Package internal protects these types' concurrency primitives from accidents.
package internal

import (
	"sync"
	"time"

	"github.com/uber/cadence/common/dynamicconfig"
)

type (
	Limit struct {
		mut sync.Mutex

		rps      func() float64        // target rps across all hosts
		previous map[string]HostRecord // previous (full) bucket of data
		current  map[string]HostRecord // current (incomplete) bucket of data, being filled
	}
	// HostRecord is NOT protected against concurrent actions.
	// Other methods must be used if you are operating on a pointer type.
	HostRecord struct {
		allowed  float64 // scaled per second, set if zero, prev values have 25% influence when updating
		rejected float64 // scaled per second, set if zero, prev values have 25% influence when updating
	}

	HostSeen struct {
		mut sync.Mutex
		// host -> last seen time
		last map[string]time.Time
	}
)

func NewLimit(rps dynamicconfig.IntPropertyFn) *Limit {
	return &Limit{
		rps:      rps.AsFloat64(),
		previous: make(map[string]HostRecord),
		current:  make(map[string]HostRecord),
	}
}
func NewHostSeen() *HostSeen {
	return &HostSeen{
		last: make(map[string]time.Time),
	}
}

func (l *Limit) Snapshot() (rps float64, previous, current map[string]HostRecord) {
	l.mut.Lock()
	defer l.mut.Unlock()
	return l.rps(), snapshotHostRecords(l.previous), snapshotHostRecords(l.current)
}

func (l *Limit) Rotate() {
	l.mut.Lock()
	defer l.mut.Unlock()
	l.previous = l.current
	l.current = make(map[string]HostRecord, len(l.previous))

	// TODO: forward all past-current to new-current, increment an idle-fuse?
	// could use as:
	// - idle==0   -> true rps (always count)
	// - idle<max  -> historical rps (allow max(this, current-remaining), assuming it is still valid)
	// - idle>=max -> too old, garbage collect
}
func (l *Limit) Update(host string, allowed, rejected float64, elapsed time.Duration) {
	l.mut.Lock()
	defer l.mut.Unlock()
	l.current[host] = l.current[host].Update(allowed, rejected, elapsed)
}

func (h *HostSeen) Observe(host string, now time.Time) {
	h.mut.Lock()
	defer h.mut.Unlock()
	h.last[host] = now
}
func (h *HostSeen) LastObserved(host string, now time.Time) time.Duration {
	h.mut.Lock()
	defer h.mut.Unlock()
	return now.Sub(h.last[host])
}
func (h *HostSeen) GC(now time.Time, maxAge time.Duration) {
	h.mut.Lock()
	defer h.mut.Unlock()
	for k, v := range h.last {
		if now.Sub(v) > maxAge {
			delete(h.last, k)
		}
	}
}
func (h *HostSeen) Len() int {
	h.mut.Lock()
	defer h.mut.Unlock()
	return len(h.last)
}

func (h HostRecord) Update(allowed, rejected float64, elapsed time.Duration) HostRecord {
	dup := h
	dup.allowed = weight(h.allowed, allowed/elapsed.Seconds())
	dup.rejected = weight(h.rejected, rejected/elapsed.Seconds())
	return dup
}
func (h HostRecord) Snapshot() (allowed, rejected float64) {
	return h.allowed, h.rejected
}

func weight(prev float64, current float64) float64 {
	if prev == 0 {
		return current
	}
	// update so newer data has 75% of the weight with each update
	return (prev * 0.25) + (current * 0.75)
}

func snapshotHostRecords(m map[string]HostRecord) map[string]HostRecord {
	out := make(map[string]HostRecord, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
