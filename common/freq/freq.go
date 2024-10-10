package freq

import (
	"sync"
	"time"

	"github.com/uber/cadence/common/clock"
)

type LimitedFreq struct {
	mut sync.Mutex

	ts       clock.TimeSource
	interval time.Duration
	cb       func()
	stopped  bool
	last     time.Time
	pending  clock.Timer
}

// NewLimitedFrequencyCallback will call `cb` at most once per [interval], and any
// calls that are more frequent will be deduplicated and delayed until [interval]
// after the previous successfully-performed call.
//
// All callbacks are performed asynchronously, so you may receive a call even after
// calling LimitedFreq.Stop().
// If this is problematic, you must synchronize with the callback separately, or
// check LimitedFreq.Stopped().
func NewLimitedFrequencyCallback(interval time.Duration, cb func()) *LimitedFreq {
	return &LimitedFreq{
		interval: interval,
		cb:       cb,
		ts:       clock.NewRealTimeSource(),
	}
}

// Handle calls or enqueues a d
func (l *LimitedFreq) Handle() {
	l.handleInternal(false)
}

// this method exists to prevent making a full closure for each pending call
func (l *LimitedFreq) handleDeferred() {
	l.handleInternal(true)
}

func (l *LimitedFreq) handleInternal(wasPending bool) {
	l.mut.Lock()
	defer l.mut.Unlock()
	if l.stopped {
		return
	}

	now := l.ts.Now()
	elapsed := now.Sub(l.last)
	if elapsed >= l.interval {
		go l.cb() // call always occurs async, because any deferred call is async and "always async" is easier to handle than "maybe async"
		l.last = now
		if l.pending != nil {
			// likely racing with afterfunc, which may have already begun calling.
			// the `l.last` update will take care of deduplicating that call, this
			// is just a minor optimization in case it hasn't happened yet.
			l.pending.Stop()
			l.pending = nil
		}
		return
	}

	// if wasPending and did not return above, another call already "stole" the time slot,
	// so there's no need to perform this call.
	// and if there's a pending call, it's already set up for the correct time, and there's no need to start another.
	if !wasPending && l.pending == nil {
		// (l.interval - elapsed) must be positive, given above check
		l.pending = l.ts.AfterFunc(l.interval-elapsed, l.handleDeferred)
	}
}

func (l *LimitedFreq) Stop() {
	l.mut.Lock()
	defer l.mut.Unlock()
	l.stopped = true
	if l.pending != nil {
		l.pending.Stop()
		l.pending = nil
	}
}

func (l *LimitedFreq) Stopped() bool {
	l.mut.Lock()
	defer l.mut.Unlock()
	return l.stopped
}
