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

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/uber/cadence/client/history"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/errors"
	"github.com/uber/cadence/common/types"
)

const AnyUpdateRequestTypeID = "cadence:loadbalanced:update_request"
const AnyAllowResponseTypeID = "cadence:loadbalanced:response"

// serialized into types.Any, JSON format must remain stable
type (
	AnyUpdateRequest struct {
		Load map[string]Metrics `json:"data,omitempty"`
	}
	Metrics struct {
		Allowed  int `json:"allowed,omitempty"`
		Rejected int `json:"rejected,omitempty"`
	}
	AnyAllowResponse struct {
		Allow map[string]float64 `json:"allow,omitempty"`
	}
)

type (
	Client interface {
		// Update performs concurrent calls to all aggregating peers to send load info
		// and retrieve new, aggregated load info from the rest of the cluster.
		// This is intended to be called periodically in the background per process,
		// not synchronously or concurrently.
		//
		// Each peer's response is delivered via the callback, on a random goroutine.
		// Update will return after all responses AND all callbacks have completed.
		//
		// Any keys requested but not returned, or previously returned but not now,
		// should be considered lost state by the aggregating peers, and the previous
		// RPS should be used until knowledge of that key spreads through the cluster again.
		// This allows aggregating peers to shut down and lose state with minimal impact.
		//
		// The passed context only applies to the underlying RPC calls, not the
		// callbacks - if you need to address cancellation in your callbacks, check in
		// them by hand (e.g. use a context derived from the one sent to Update).
		//
		// To retrieve initial data when this host is starting up / from a blank slate,
		// use Startup instead.
		Update(ctx context.Context, period time.Duration, load AnyUpdateRequest, results func(request AnyUpdateRequest, response *AnyAllowResponse, err error)) error

		// Startup performs concurrent calls to all aggregating peers to load initial
		// data at service startup, to reduce the need to rely on possibly-incorrect
		// fallback logic.  Unlike Update, performing this call does not directly imply
		// that the caller has received zero requests since the last call.
		//
		// Each peer's response is delivered via the callback, on a random goroutine.
		// Startup will return after all responses AND all callbacks have completed.
		//
		// The passed context only applies to the underlying RPC calls, not the
		// callbacks - if you need to address cancellation in your callbacks, check in
		// them by hand (e.g. use a context derived from the one sent to Startup).
		Startup(ctx context.Context, results func(batch *AnyAllowResponse, err error)) error
	}

	Host string // random UUID specified at startup, as membership.HostInfo does not seem guaranteed unique

	// PushLoad allows extremely simple load reporting, no accounting for spikiness / etc.
	PushLoad struct {
		Host   Host               // random UUID at startup
		Period time.Duration      // Non-zero.  How long since the last reporting, or since service startup / RPC-inbound allowed.
		Load   map[string]Metrics // Request data per ratelimit key to this host. (May be useful to include multiple periods if the value changed)
	}
	// Load info for one key
	Load struct {
		Allowed  int // How many requests were Allowed
		Rejected int // How many requests were Rejected
	}
	// RetLoad defines how many requests to allow per key for this Host
	RetLoad struct {
		Host   Host           // implied == self, remove soon
		Allow  map[string]int // Map of ratelimit keys to RPS
		Remove []string       // List of keys that are no longer owned by this host (from PushLoad)
	}

	client struct {
		history  history.Client
		resolver history.PeerResolver
		thisHost Host
	}
)

var _ Client = (*client)(nil)

func (c *client) Update(ctx context.Context, period time.Duration, load AnyUpdateRequest, results func(request AnyUpdateRequest, response *AnyAllowResponse, err error)) error {
	batches, err := c.shard(load)
	if err != nil {
		// should only happen if peers are unavailable, individual requests are handled other ways
		return fmt.Errorf("unable to shard ratelimit update data: %w", err)
	}

	if ctx.Err() != nil {
		// worth checking before spawning a bunch of costly goroutines that may achieve nothing.
		return fmt.Errorf("unable to start ratelimit update requests, canceled: %w", ctx.Err())
	}

	var g errgroup.Group // TODO: needs panic resistance
	g.SetLimit(100)      // TODO: limited concurrency?  configurable?
	for peerAddress, batch := range batches {
		g.Go(func() error {
			push, err := UpdateRequestToAny(batch)
			if err != nil {
				// serialization is treated as a fatal coding error, it should never happen outside dev-ing.
				return err
			}

			result, err := c.history.RatelimitUpdate(ctx, peerAddress, &types.RatelimitUpdateRequest{
				Caller:      string(c.thisHost),
				LastUpdated: period,
				Data:        push, // TODO: make this the Any type, not a map containing them, should save tons of data on network and moderate cpu
			})
			if err != nil {
				results(load, nil, errors.ErrFromRPC(fmt.Errorf("ratelimit update request: %w", err)))
				return nil
			}

			resp, err := AnyToAllowResponse(result.Data)
			if err != nil {
				results(load, nil, err)
				return nil
			}

			results(batch, &resp, nil)
			return nil
		})
	}

	// always nil, wrap with doesNotError for troubleshooting if something happens
	return doesNotError(
		"ratelimit update errgroup should never return an error",
		g.Wait(),
	)
}

func (c *client) Startup(ctx context.Context, results func(batch *AnyAllowResponse, err error)) error {
	peers, err := c.resolver.GetAllPeers()
	if err != nil {
		return errors.ErrFromRPC(fmt.Errorf("unable begin ratelimit-startup request, cannot get all peers: %w", err))
	}

	var g errgroup.Group // TODO: needs panic resistance
	g.SetLimit(100)      // TODO: configurable?  this is an upper limit, so no need to make it precise.
	for _, peerAddress := range peers {
		g.Go(func() error {
			result, err := c.history.RatelimitStartup(ctx, peerAddress, &types.RatelimitStartupRequest{
				Caller: string(c.thisHost),
			})
			if err != nil {
				results(nil, errors.ErrFromRPC(fmt.Errorf("ratelimit startup request: %w", err)))
				return nil
			}

			resp, err := AnyToAllowResponse(result.Data)
			if err != nil {
				results(nil, err)
				return nil
			}

			results(&resp, nil)
			return nil
		})
	}

	// always nil, doesNotError for troubleshooting if something happens
	return doesNotError(
		"ratelimit startup errgroup should never return an error",
		g.Wait(),
	)
}

// shard splits load-requests by ratelimit-keys for each peer in the ratelimit ring,
// and synchronously calls the callback for each one before returning.
//
// if an error is returned, no callbacks will be performed.
func (c *client) shard(r AnyUpdateRequest) (map[string]AnyUpdateRequest, error) {
	byPeers, err := c.resolver.SplitFromLoadBalancedRatelimit(keys(r.Load))
	if err != nil {
		return nil, errors.ErrFromRPC(fmt.Errorf("unable to shard ratelimits to hosts: %w", err))
	}
	results := make(map[string]AnyUpdateRequest, len(byPeers))
	for peerAddress, ratelimits := range byPeers {
		batch := AnyUpdateRequest{
			Load: make(map[string]Metrics, len(ratelimits)),
		}
		for _, key := range ratelimits {
			batch.Load[key] = r.Load[key]
		}
		results[peerAddress] = batch
	}
	return results, nil
}

func keys[K comparable, V any](data map[K]V) []K {
	result := make([]K, 0, len(data))
	for k := range data {
		result = append(result, k)
	}
	return result
}

// doesNotError recognizably wraps a "this should not occur" error so it can be
// consistently checked in tests / found in logs if it somehow passes tests.
//
// just describe the error, %w will be added for the caller.
func doesNotError(format string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("coding error: "+format+": %w", err)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func AnyToAllowResponse(a types.Any) (AnyAllowResponse, error) {
	return deserializeTo[AnyAllowResponse](a, AnyAllowResponseTypeID)
}
func AnyToUpdateRequest(a types.Any) (AnyUpdateRequest, error) {
	return deserializeTo[AnyUpdateRequest](a, AnyUpdateRequestTypeID)
}
func UpdateRequestToAny(r AnyUpdateRequest) (types.Any, error) {
	return serializeTo(r, AnyUpdateRequestTypeID)
}
func AllowResponseToAny(r AnyAllowResponse) (types.Any, error) {
	return serializeTo(r, AnyAllowResponseTypeID)
}
func serializeTo[T any](r T, typeID string) (types.Any, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return types.Any{}, fmt.Errorf("should not be possible: %T failed to JSON-serialize: %w", r, err)
	}
	return types.Any{
		TypeID: typeID,
		Value:  data,
	}, nil
}
func deserializeTo[T any](a types.Any, typeID string) (T, error) {
	var out T
	if a.TypeID != typeID {
		return out, errors.ErrFromDeserialization(
			fmt.Errorf("wrong Any.TypeID %q for type %T, should be %q", a.TypeID, out, typeID),
		)
	}
	err := json.Unmarshal(a.Value, &out)
	if err != nil {
		return out, errors.ErrFromDeserialization(
			fmt.Errorf("decoding error for Any.TypeID %q for type %T, data: %.100v", a.TypeID, out, a.Value),
		)
	}
	return out, nil
}
