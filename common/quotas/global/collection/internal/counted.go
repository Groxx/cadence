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

package internal

import (
	"context"

	"go.uber.org/atomic"

	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/quotas"
)

type (
	CountedLimiter struct {
		wrapped quotas.Limiter
		usage   *AtomicUsage
	}

	allowedReservation struct {
		wrapped clock.Reservation
		usage   *AtomicUsage // reference to the reservation's limiter's usage
	}
	deniedReservation struct{}

	AtomicUsage struct {
		allowed, rejected, idle atomic.Int64
	}

	UsageMetrics struct {
		Allowed, Rejected, Idle int
	}
)

var _ quotas.Limiter = CountedLimiter{}
var _ clock.Reservation = allowedReservation{}
var _ clock.Reservation = deniedReservation{}

func NewCountedLimiter(limiter quotas.Limiter) CountedLimiter {
	return CountedLimiter{
		wrapped: limiter,
		usage:   &AtomicUsage{},
	}
}

func (c CountedLimiter) Allow() bool {
	allowed := c.wrapped.Allow()
	c.usage.Count(allowed)
	return allowed
}

func (c CountedLimiter) Wait(ctx context.Context) error {
	err := c.wrapped.Wait(ctx)
	c.usage.Count(err == nil)
	return err
}

func (c CountedLimiter) Reserve() clock.Reservation {
	res := c.wrapped.Reserve()
	if res.Allow() {
		return allowedReservation{
			wrapped: c.wrapped.Reserve(),
			usage:   c.usage,
		}
	}
	res.Used(false) // rejected so not used
	c.usage.Count(false)
	return deniedReservation{}
}

func (c CountedLimiter) Collect() UsageMetrics {
	return c.usage.Collect()
}

func (d deniedReservation) Allow() bool { return false }
func (d deniedReservation) Used(bool)   {}

func (a allowedReservation) Allow() bool { return true }
func (a allowedReservation) Used(wasUsed bool) {
	a.wrapped.Used(wasUsed) // forward the call
	a.usage.idle.Store(0)   // not idle regardless of usage
	if wasUsed {
		// only counts as allowed if used, else it is hopefully rolled back.
		// this may or may not restore the token, but it does imply "this limiter did not limit the event".
		a.usage.allowed.Add(1)
	}

	// else it was canceled, and not "used".
	//
	// currently these are not tracked because some other rejection will occur
	// and be emitted in all our current uses, but with bad enough luck or
	// latency before canceling it could lead to misleading metrics.
}

func (a *AtomicUsage) Count(allowed bool) {
	if allowed {
		a.allowed.Add(1)
	} else {
		a.rejected.Add(1)
	}
	a.idle.Store(0)
}

func (a *AtomicUsage) Collect() UsageMetrics {
	m := UsageMetrics{
		Allowed:  int(a.allowed.Swap(0)),
		Rejected: int(a.rejected.Swap(0)),
	}
	if m.Allowed+m.Rejected == 0 {
		m.Idle = int(a.idle.Add(1))
	} // else 0
	return m
}
