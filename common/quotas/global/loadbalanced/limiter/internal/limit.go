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

// Package internal protects these types' concurrency primitives and other internals from accidents.
// this has been rolled back in favor of simplicity and avoiding premature optimization before measuring,
// but the internal-separation is still broadly a good thing.  these are not dumb objects.
package internal

import (
	"golang.org/x/time/rate"

	"github.com/uber/cadence/common/quotas"
)

type (
	BalancedLimit struct {
		// usage data cannot be gathered from rate.Limiter, sadly.
		// so we need to gather it separately. or maybe find a fork.
		usage    usage
		fallback quotas.Limiter // fallback when limit is nil, accepted until updated
		limit    *rate.Limiter  // local-only limiter based on remote data.
	}

	// usage is a simple usage-tracking mechanism for limiting hosts.
	//
	// all it cares about is total since last report.  no attempt is made to address
	// abnormal spikes within report times, widely-varying behavior across reports,
	// etc - that kind of logic is left to the common hosts, not the limiting ones.
	usage struct {
		accepted     int
		rejected     int
		failedUpdate int // reset when an update occurs
	}
)

func New(fallback quotas.Limiter) *BalancedLimit {
	return &BalancedLimit{
		fallback: fallback,
	}
}

// Collect returns the current accepted/rejected values, and resets them to zero.
func (b *BalancedLimit) Collect() (used int, refused int, usingFallback bool) {
	used, refused = b.usage.accepted, b.usage.rejected
	b.usage.accepted = 0
	b.usage.rejected = 0
	return used, refused, b.limit == nil
}

// Update adjusts the underlying ratelimit.
func (b *BalancedLimit) Update(rps float64) {
	b.usage.failedUpdate = 0 // reset the use-fallback fuse

	if b.limit == nil {
		// fallback no longer needed, use limiter only
		b.limit = rate.NewLimiter(
			rate.Limit(rps),
			// 0 disallows all requests, so allow at least 1 and rely on rps to fill sanely.
			max(1, int(rps)),
		)
		return
	}
	if b.limit.Limit() == rate.Limit(rps) {
		return
	}

	b.limit.SetLimit(rate.Limit(rps))
	b.limit.SetBurst(max(1, int(rps))) // 0 disallows all requests, so allow at least 1 and rely on rps to fill sanely
}

// FailedUpdate should be called when a key fails to update from a common host,
// possibly implying some kind of problem, possibly with this key.
//
// After crossing a threshold of failures (currently 10), the fallback will be used.
func (b *BalancedLimit) FailedUpdate() (failures int) {
	b.usage.failedUpdate++ // always increment the count for monitoring purposes
	if b.usage.failedUpdate == 10 {
		b.limit = nil // defer to fallback when crossing the threshold
	}
	return b.usage.failedUpdate
}

// Clear erases the internal ratelimit, and defers to the fallback until an update is received.
// This is intended to be used when the current limit is no longer trustworthy for some reason.
func (b *BalancedLimit) Clear() {
	b.limit = nil
}

// Allow returns true if a request is allowed right now.
func (b *BalancedLimit) Allow() bool {
	var allowed bool
	if b.limit == nil {
		allowed = b.fallback.Allow()
	} else {
		allowed = b.limit.Allow()
	}

	if allowed {
		b.usage.accepted++
	} else {
		b.usage.rejected++
	}
	return allowed
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
