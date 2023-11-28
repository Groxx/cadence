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
Package global blah

Plan:
  - quotas.Collection contains `For(key) Limiter`
  - quotas.Limiter contains Allow(), Wait(ctx), Reserve()
  - quotas.Policy contains `Allow(info{domain}) bool` and is the only thing used in frontend
  - I want limiters that are used to aggregate and report their use
  - I want limiters to allow warming (call Allow and Reserve but do not honor decision)
  - I want limiters to fall back to other limiters
  - None of these support shutting down, but we'll want to stop collections
    to turn off the background reporter.
  - quotas.Limiter is by far the majority used by number, but all three are in use.
    maybe this makes sense, to minimize locking on the collection e.g. for task processing.

Level to implement at:
  - pretty clearly I want a collection (or larger) to bulk-report metrics
  - limiters do not have APIs for updating or shutting down, and quotas.Collection only returns limiters
  - collections in frontend define dynamic config for rates?

quotas.MultiStageRateLimiter implements quotas.Policy and that seems like the thing to reuse.
it takes a quotas.Collection for per-domain, and calls For(domain) on each one.

Should fallback and warming be in-limiter or in-collection?
  - Or split?
  - Or outside both?
  - How are these used in code anyway?  What do I need to expose?

Thoughts:
  - quotas.Policy is kinda dumb but it may be worth maintaining as the top-level API.
    just convert the domain / type to a "domain-type" string.
  - ^ quotas.Policy is not sufficient for shutdown, but we could layer shutdowning inside it.
    also it's an interface, and does not need to be the *whole* API exposed, startup can use more
    when it constructs it.
  - quotas.Policy is literally only used with the multi-stage limiter, which always calls Reserve
    immediately after For.
    No need to be lazy, just return the single relevant limiter immediately.
*/
package global

import (
	"fmt"
	// for docs
	"github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/quotas"
	_ "github.com/uber/cadence/common/quotas"
)

// TODO: naaaaming

// MultiCollection selects a limiter based on the current strategy.
// currently this assumes usage like frontend ratelimiters, but it should be stretchable.
//
// This should be used as the per-domain limiter *before* the global limiter,
// so global limits are not double-consumed by cache warming.
//
// quotas.Policy is literally only used with the multi-stage limiter, which always calls Reserve
// immediately after For.
// So there is no need to be lazy, just return the single relevant limiter immediately.
type MultiCollection struct {
	// called to determine the current strategy
	strategy dynamicconfig.StringPropertyFn

	// ultimate fallback strategy, must be in collection at init
	//
	// TODO: no, fallback is part of each collection, so I can set per-host as the internal fallback in balanced-load
	fallbackStrategy dynamicconfig.RPSStrategy

	// initialized with all strategies, but likely unused
	collections map[dynamicconfig.RPSStrategy]*quotas.Collection

	keyfunc func(info quotas.Info) string
}

type StrategyCollection struct {
	Strategy   dynamicconfig.RPSStrategy
	Collection *quotas.Collection // TODO: should support policy?  or... something?  should I allow escaping this whole system, or require it?
}

func New(strategy dynamicconfig.StringPropertyFn, fallback StrategyCollection, supported ...StrategyCollection) (*MultiCollection, error) {
	use, _, err := fallback.Strategy.Get() // TODO: hmmm I don't like this double-use.  split components out to a separate type?
	if err != nil {
		return nil, fmt.Errorf("cannot init with invalid strategy: %w", err)
	}
	if fallback.Collection == nil {
		return nil, fmt.Errorf("cannot init with nil fallback collection")
	}

	colls := map[dynamicconfig.RPSStrategy]*quotas.Collection{
		use: fallback.Collection,
	}
	for _, s := range supported {
		if _, ok := colls[s.Strategy]; ok {
			return nil, fmt.Errorf("duplicate strategy %q", s.Strategy)
		}
		colls[s.Strategy] = s.Collection
	}
	return &MultiCollection{
		strategy:         strategy,
		fallbackStrategy: dynamicconfig.PerHost,
		collections:      colls,
	}, nil
}

var _ quotas.Policy = &MultiCollection{}

func (b *MultiCollection) Allow(info quotas.Info) bool {
	strat := dynamicconfig.RPSStrategy(b.strategy())
	use, warm, err := strat.Get()
	if err != nil {
		// TODO: log?  should not be allowed.
		use, warm = dynamicconfig.PerHost, "" // TODO: use fallback instead
	}

	usec, ok := b.collections[use]
	if !ok {
		usec = b.collections[b.fallbackStrategy]
	}
	warmc := b.collections[warm]

	if warmc != nil {
		_ = warmc.For(info.Domain).Allow() // consume but ignore result
	}
	return usec.For(info.Domain).Allow()
}
