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
Package rpc is used by both limiter and aggregator, so this is broken out to prevent cycles.

The types here are used in the data blob field of the ratelimiter IDL.
*/
package rpc

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/yarpc"
	"golang.org/x/sync/errgroup"

	"github.com/uber/cadence/client/history"
)

type (
	Client interface {
		// Update pushes bulk info and updates limits based on the response.
		// RetLoad data is returned via the callback as common-hosts respond,
		// but the call blocks until all resulting calls have completed.
		//
		// Results will contain RetLoad data from _all_ common hosts, and a complete batch
		// should represent all currently-known keys for this host in the cluster.
		//
		// Any missing keys should be considered lost state by common hosts, and the previous
		// RPS should be used until an update spreads the knowledge to the cluster again.
		// This allows common-hosts to shut down and lose state with minimal impact.  Limiting
		// hosts can pre-warm their data via Startup.
		Update(ctx context.Context, period time.Duration, load map[string]Load, results func(batch RetLoad, err error)) error

		// Startup requests that all common-hosts return data for this limiting-host,
		// intended to be called during service startup or when warming from zero at runtime.
		//
		// Unlike Update, this call does not imply that the caller has not received
		// any requests - it's just pre-warming its empty state.
		//
		// Results are distributed via callback as they come in, to speed up partial responses.
		// The call blocks until all responses are received.
		Startup(ctx context.Context, results func(batch RetLoad, err error)) error
	}

	Host string // random UUID specified at startup, as membership.HostInfo does not seem guaranteed unique

	// PushLoad allows extremely simple load reporting, no accounting for spikiness / etc.
	PushLoad struct {
		Host   Host            // random UUID at startup
		Period time.Duration   // Non-zero.  How long since the last reporting, or since service startup / RPC-inbound allowed.
		Load   map[string]Load // Request data per ratelimit key to this host. (May be useful to include multiple periods if the value changed)
	}
	// Load info for one key
	Load struct {
		Allowed  int64 // How many requests were Allowed
		Rejected int64 // How many requests were Rejected
	}
	// RetLoad defines how many requests to allow per key for this Host
	RetLoad struct {
		Host   Host             // implied == self, remove soon
		Allow  map[string]int64 // Map of ratelimit keys to RPS
		Remove []string         // List of keys that are no longer owned by this host (from PushLoad)

		// TODO: some kind of self-healing ring ideally.... yea?
		// TODO: preload data that might come to this host if the ring changes?  or will that require the whole ring?
		// consistent hash ring should mean just +1 host worth of data.  "inheritable" basically.  filter before pushing.
		//
		// ringpop-go has this semantic, but not our wrapper.
		// could modify, but for now just yolo.
		//
		// separately: ringpop-go is surprisingly slow.
		//  - 100 hosts + 1000 keys + 1 host per key = 0.2ms for 8 cpu (our wrapper does this)
		//  - scales linearly with N hosts per key
		//  - farmhash on its own is low-double-digits of nanoseconds per key, ringpop is 200,000.
		//    - 20,000 for hashing 1k keys, so 10x more for 100 hosts?
		//    - not absurd I guess.  though seems high for a red/black tree walk for next-highest node.
		//  - so we can expect a full filter to take about 1ms of cpu.
		//
		// costly!  possibly even worth caching, evict when the ring changes.
		//
		//
		/*
			back-of-envelope calc to sanity check:
			- 100 limiting hosts
			- 100 common hosts
			- 1000 ratelimit keys per limit-type (domains)
			- 3 limit-types (user/worker/search)
			- 1/s check-ins
			- 0.2ms for 8 cpu (call it 2s)
			---------
			that means:
			- 1 limiting host spends 2ms per second to send (0.2% budget)
			- 100 common hosts spend 2ms to respond = 200ms per second (20% budget)
			so this will consume 20% of a core in the common hosts, just due to ringpop.
			-----
			so:
			- limiting hosts do whatever, it's cheap
			- common hosts need to cache key/host pairs, evict on ring changes.
			  (pays for itself quickly.)
			-----
			alternately, return all to limiting host, have it filter:
			- 4ms per second (0.4% cpu budget)
			- 100x more data
			- not cache friendly
			- very limiter-friendly, all available all the time
		*/
		/*
			everyone stores everything, limiters collect from everyone?
			- everyone pushes/pulls all data every period
			- 100x more data for limiters to pull, commons to return
			- commons are dumb, trivially replaced by redis
			- losing a common host means that host just has nothing.  all others still exist, still replicating data.
			- ... pretty sticky?  also high bandwidth, lots of small shifting, can't checksum to compare.  just average to get real values.
		*/
		/*
			ringpop to get 3 hosts, 3x data, average between?
			- 3x cpu per limiter, 6ms (0.6% budget)
			- 3x cpu per common, 600ms (60% budget)
			  - really needs cache here
			- missing data is just ignored, present is averaged as it's just a replica
		*/
		/*
			No, thinking I should:
			- send all we have observed
			- reply with all matching (no common-host cost, minimal data)
			- allow tuning "when no data, allow `(RPS/N-hosts) * configurable`".
			  - we'll probably use 2x internally, to be tolerant of moves for a period.
			  - this may cut spikes, but that's probably a good thing

			...
			What can I do with pre-warmed data?  Anything beyond "allow [that fallback]"?
			- Can reject when over / act like muttley, reject N% / beyond Nps on the assumption that
			  we are merely changing frontends.
			- I kinda like the "can use N% of remaining quota" which allows higher and lower than [that fallback]
			  - this could be based on N hosts in *zone*...
		*/
	}

	client struct {
		history  history.Client
		resolver history.PeerResolver
		thisHost Host
	}
)

var _ Client = (*client)(nil)

func (c *client) Update(ctx context.Context, period time.Duration, load map[string]Load, results func(batch RetLoad, err error)) error {
	var g errgroup.Group // TODO: needs panic resistance
	g.SetLimit(100)      // TODO: limited concurrency?  configurable?
	err := c.shard(period, load, func(peerAddress string, batch PushLoad) {
		g.Go(func() error {
			_ = batch
			result, err := c.history.RatelimitUpdate(ctx, nil, yarpc.WithShardKey(peerAddress))
			_ = result // TODO: turn into retload
			results(RetLoad{}, err)
			return nil
		})
	})
	if err != nil {
		// should only happen if peers are unavailable, individual requests are handled other ways
		return fmt.Errorf("unable to begin ratelimit-startup request: %w", err)
	}

	// always nil, wrap for troubleshooting if something happens
	return wrap(
		"ratelimit update errgroup should never return an error",
		g.Wait(),
	)
}

func (c *client) Startup(ctx context.Context, results func(batch RetLoad, err error)) error {
	var g errgroup.Group // TODO: needs panic resistance
	g.SetLimit(100)      // TODO: limited concurrency?  configurable?
	err := c.initAllPeers(func(peerAddress string, batch PushLoad) {
		g.Go(func() error {
			_ = batch
			result, err := c.history.RatelimitStartup(ctx, nil, yarpc.WithShardKey(peerAddress))
			_ = result // TODO: turn into retload
			results(RetLoad{}, err)
			return nil
		})
	})
	if err != nil {
		// should only happen if peers are unavailable and no requests were sent,
		// individual requests are handled via callback
		return fmt.Errorf("unable to begin ratelimit-startup request: %w", err)
	}

	// always nil, wrap for troubleshooting if something happens
	return wrap(
		"ratelimit startup errgroup should never return an error",
		g.Wait(),
	)
}

// initAllPeers sets up an empty request for each peer in the ratelimit ring,
// and synchronously calls the callback for each one before returning.
//
// if an error is returned, no callbacks will be performed.
func (c *client) initAllPeers(cb func(peerAddress string, batch PushLoad)) error {
	// send empty requests to all hosts, to pre-load with all data
	peers, err := c.resolver.GetAllPeers()
	if err != nil {
		return fmt.Errorf("unable to get all peers: %w", err)
	}
	for _, peerAddress := range peers {
		cb(peerAddress, PushLoad{
			Host:   c.thisHost,
			Period: 0,
			Load:   nil,
		})
	}
	return nil
}

// shard splits load-requests by ratelimit-keys for each peer in the ratelimit ring,
// and synchronously calls the callback for each one before returning.
//
// if an error is returned, no callbacks will be performed.
func (c *client) shard(period time.Duration, load map[string]Load, cb func(peerAddress string, batch PushLoad)) error {
	byPeers, err := c.resolver.SplitFromLoadBalancedRatelimit(keys(load))
	if err != nil {
		return fmt.Errorf("unable to shard ratelimitss to hosts: %w", err)
	}
	for peerAddress, ratelimits := range byPeers {
		batch := make(map[string]Load, len(ratelimits))
		for _, key := range ratelimits {
			batch[key] = load[key]
		}
		cb(peerAddress, PushLoad{
			Host:   c.thisHost,
			Period: period,
			Load:   batch,
		})
	}
	return nil
}

func keys[K comparable, V any](data map[K]V) []K {
	result := make([]K, 0, len(data))
	for k := range data {
		result = append(result, k)
	}
	return result
}

// wraps an error or nil if no error
func wrap(format string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf(format+": %w", err)
}
