package quotas

import (
	"context"

	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/metrics"
)

type (
	TrackedLimiter struct {
		wrapped Limiter
		scope   metrics.Scope
	}

	trackedReservation struct {
		wrapped clock.Reservation
		count   func(bool)
	}
)

var _ Limiter = TrackedLimiter{}
var _ clock.Reservation = trackedReservation{}

// and then create this with the necessary tags to identify this limiter uniquely
func NewTrackedLimiter(limiter Limiter, scope metrics.Scope) Limiter {
	return TrackedLimiter{
		wrapped: limiter,
		scope:   scope,
	}
}

func (c TrackedLimiter) count(allowed bool) {
	if allowed {
		c.scope.IncCounter(metrics.LimiterAllowed)
	} else {
		c.scope.IncCounter(metrics.LimiterRejected)
	}
}

func (c TrackedLimiter) Allow() bool {
	allowed := c.wrapped.Allow()
	c.count(allowed)
	return allowed
}

func (c TrackedLimiter) Wait(ctx context.Context) error {
	err := c.wrapped.Wait(ctx)
	c.count(err == nil)
	return err
}

func (c TrackedLimiter) Reserve() clock.Reservation {
	return trackedReservation{
		wrapped: c.wrapped.Reserve(),
		count:   c.count,
	}
}

func (c trackedReservation) Allow() bool {
	return c.wrapped.Allow()
}

func (c trackedReservation) Used(wasUsed bool) {
	c.wrapped.Used(wasUsed)
	if c.Allow() {
		if wasUsed {
			// only counts as allowed if used, else it is hopefully rolled back.
			// this may or may not restore the token, but it does imply "this limiter did not limit the event".
			c.count(true)
		}

		// else it was canceled, and not "used".
		//
		// currently these are not tracked because some other rejection will occur
		// and be emitted in all our current uses, but with bad enough luck or
		// latency before canceling it could lead to misleading metrics.
	} else {
		// these reservations cannot be waited on so they cannot become allowed,
		// and they cannot be returned, so they are always rejected.
		//
		// specifically: it is likely that `wasUsed == Allow()`, so false cannot be
		// trusted to mean "will not use for some other reason", and the underlying
		// rate.Limiter did not change state anyway because it returned the
		// pending-token before becoming a clock.Reservation.
		c.count(false)
	}
}
