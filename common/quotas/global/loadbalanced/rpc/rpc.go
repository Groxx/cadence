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

// Package rpc is used by both limiter and aggregator, so this is broken out to prevent cycles.
package rpc

/*

back-of-envelope calc to sanity check performance needs:
- 100 limiting hosts
- 100 aggregating hosts
- 1000 ratelimit keys per limit-type (domains)
- 3 limit-types (user/worker/search)
- 1/s check-in
- 0.2ms for 8 cpu (call it 2ms 1 cpu) to get a single ringpop host for each key

that means, if aggregating hosts do ringpop stuff:
- 1 limiting host spends 2ms per second to send (0.2% budget)
- 100 aggregating hosts spend 2ms to respond = 200ms per second with 100 limiters (20% budget)

so this will consume 20% of a core in the aggregating hosts, just due to ringpop.
ouch.

---

so:
- limiting hosts do whatever, it's cheap.
- aggregating hosts need be concerned about how they return data.
  - cache key/host pairs, evict on ring changes?  it'll pay off quickly.
  - filter to caller's keys?  efficient but does not spread load before needed.
  - don't filter responses, return everything?  100x more data tho.
*/

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/yarpc"
	"golang.org/x/exp/maps"
	"golang.org/x/sync/errgroup"

	"github.com/uber/cadence/client/history"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/aggregator"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/aggregator/algorithm"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/errors"
	"github.com/uber/cadence/common/types"
)

const AnyUpdateRequestTypeID = "cadence:loadbalanced:update_request"
const AnyAllowResponseTypeID = "cadence:loadbalanced:response"

type (
	// AnyUpdateRequest is serialized into types.Any, JSON format must remain stable (or versioned to handle changes during deploy).
	AnyUpdateRequest struct {
		// Elapsed time since last update, for calculating rps
		Elapsed time.Duration `json:"elapsed"`
		// Load contains total request data per key, containing two integers to be compact on the wire:
		//  - first is the number of allowed requests
		//  - second is rejected requests
		//
		// These values are counted since the last update (Elapsed time), both to give more
		// data to the aggregating servers and to use integers which compact a bit better than floats.
		//
		// For the purposes of API flexibility, this is a slice and not a [2]int.
		// This allows newer code to send additional values without failing deserialization,
		// if for some reason this proves useful.
		// Using an explicit version is generally safer though and should be preferred in most cases.
		Load map[string][]int `json:"data,omitempty"`
	}
	// AnyAllowResponse is serialized into types.Any, JSON format must remain stable (or versioned to handle changes during deploy)
	AnyAllowResponse struct {
		/*
			Allow contains the per-key limit info:
				{
					"the-limit-key": rpsToAllow,
				}

			Zero RPS implies "do not allow any requests", which likely means the domain is using all
			available RPS already, and you are a "new" host (for this limit).
			The next update cycle will likely allocate a non-zero portion to this host.

			Keys that were requested may be missing, which implies the key is currently allocated
			zero RPS, likely due to being unknown (either new or garbage collected).
		*/
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

	client struct {
		history  history.Client
		resolver history.PeerResolver
		thisHost Host

		logger log.Logger
		scope  metrics.Scope

		maxConcurrency int
	}
)

var _ Client = (*client)(nil)

func New(
	historyClient history.Client,
	resolver history.PeerResolver,
	logger log.Logger,
	scope metrics.Scope,
) Client {
	return &client{
		history:        historyClient,
		resolver:       resolver,
		thisHost:       Host(uuid.New().String()), // TODO: descriptive would be better?  but it works, unique ensures correctness.
		logger:         logger,
		scope:          scope,
		maxConcurrency: 100, // TODO: dynamic config?  GOMAXPROCS?  it's just an upper limit...
	}
}

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

	var g errgroup.Group
	g.SetLimit(c.maxConcurrency)
	for peerAddress, batch := range batches {
		g.Go(func() error {
			defer func() { log.CapturePanic(recover(), c.logger, nil) }() // todo: describe what failed? is stack enough?

			push, err := updateRequestToAny(batch)
			if err != nil {
				// serialization is treated as a fatal coding error, it should never happen outside dev-ing.
				return err
			}

			result, err := c.history.RatelimitUpdate(ctx, &types.RatelimitUpdateRequest{
				Caller:      string(c.thisHost),
				LastUpdated: period,
				Data:        push, // TODO: make this the Any type, not a map containing them, should save tons of data on network and moderate cpu
			}, yarpc.WithShardKey(peerAddress))
			if err != nil {
				results(load, nil, errors.ErrFromRPC(fmt.Errorf("ratelimit update request: %w", err)))
				return nil
			}

			resp, err := anyToAllowResponse(result.Data)
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

	var g errgroup.Group
	g.SetLimit(c.maxConcurrency)
	for _, peerAddress := range peers {
		g.Go(func() error {
			defer func() { log.CapturePanic(recover(), c.logger, nil) }() // todo: describe what failed? is stack enough?

			result, err := c.history.RatelimitStartup(ctx, &types.RatelimitStartupRequest{
				Caller: string(c.thisHost),
			}, yarpc.WithShardKey(peerAddress))
			if err != nil {
				results(nil, errors.ErrFromRPC(fmt.Errorf("ratelimit startup request: %w", err)))
				return nil
			}

			resp, err := anyToAllowResponse(result.Data)
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
//
// Caution for callers: this is quite CPU intensive.
// Periodic calls from hosts-enforcing-limits is fine, but avoid calling it in order to calculate
// responses on data-aggregating hosts - cumulative calls can easily consume multiple CPU cores per second.
func (c *client) shard(r AnyUpdateRequest) (map[string]AnyUpdateRequest, error) {
	byPeers, err := c.resolver.SplitFromLoadBalancedRatelimit(maps.Keys(r.Load))
	if err != nil {
		return nil, errors.ErrFromRPC(fmt.Errorf("unable to shard ratelimits to hosts: %w", err))
	}
	results := make(map[string]AnyUpdateRequest, len(byPeers))
	for peerAddress, ratelimits := range byPeers {
		batch := AnyUpdateRequest{
			Elapsed: r.Elapsed,
			Load:    make(map[string][]int, len(ratelimits)),
		}
		for _, key := range ratelimits {
			batch.Load[key] = r.Load[key]
		}
		results[peerAddress] = batch
	}
	return results, nil
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

// converting types to/from the aggregator's API (convenience for handlers)

func AggGetResponseToAny(res aggregator.GetResponse) (types.Any, error) {
	return AllowResponseToAny(AnyAllowResponse{
		Allow: res.RPS,
	})
}
func AnyToAggUpdate(caller string, a types.Any) (aggregator.UpdateRequest, error) {
	req, err := anyToUpdateRequest(a)
	if err != nil {
		return aggregator.UpdateRequest{}, fmt.Errorf("unable to convert to update request: %w", err)
	}
	load := make(map[algorithm.Limit]aggregator.Load, len(req.Load))
	for k, v := range req.Load {
		if len(v) < 2 {
			return aggregator.UpdateRequest{}, fmt.Errorf("insufficient values in load info for key %q, expected [allowed, rejected] but got %v", k, v)
		}
		load[algorithm.Limit(k)] = aggregator.Load{
			Allowed:  v[0],
			Rejected: v[1],
		}
	}
	return aggregator.UpdateRequest{
		Host:    algorithm.Identity(caller),
		Elapsed: req.Elapsed,
		Load:    load,
	}, nil
}

// typed to/from json

func anyToAllowResponse(a types.Any) (AnyAllowResponse, error) {
	return deserializeTo[AnyAllowResponse](a, AnyAllowResponseTypeID)
}
func anyToUpdateRequest(a types.Any) (AnyUpdateRequest, error) {
	return deserializeTo[AnyUpdateRequest](a, AnyUpdateRequestTypeID)
}
func updateRequestToAny(r AnyUpdateRequest) (types.Any, error) {
	return serializeTo(r, AnyUpdateRequestTypeID)
}
func AllowResponseToAny(r AnyAllowResponse) (types.Any, error) {
	return serializeTo(r, AnyAllowResponseTypeID)
}

// general to/from json via Any

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
