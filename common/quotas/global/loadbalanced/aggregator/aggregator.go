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

package aggregator

import (
	"fmt"
	"time"

	"github.com/uber/cadence/common/membership"
	"github.com/uber/cadence/common/quotas"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/aggregator/algorithm"
	"github.com/uber/cadence/common/service"
)

type ConfiguredLimits interface {
	Get(key string) float64 // or zero if unknown
}

type (
	// Impl is public for test purposes
	Impl struct {
		weights algorithm.WeightedAverage // target-less usage calculation
		limits  ConfiguredLimits          // target values
		peers   membership.Resolver
	}
)

func New(
	alg algorithm.WeightedAverage,
	limits *quotas.Collection, // TODO: wire up to dynamicconfig
	peers membership.Resolver,
) (Aggregator, error) {
	return &Impl{
		weights: alg,
		limits:  nil, // TODO: wire up to dynamicconfig
		peers:   peers,
	}, nil
}

type Load struct {
	Allowed  int
	Rejected int
}

// Update adds load information for the passed keys to this aggregator.
func (i *Impl) Update(host string, elapsed time.Duration, load map[string]Load) error {
	for key, data := range load {
		// update is cheap enough to do separately from get, no need to combine
		i.weights.Update(
			algorithm.Limit(key),
			algorithm.Identity(host),
			data.Allowed,
			data.Rejected,
			elapsed,
		)
	}
	return nil
}

func (i *Impl) Get(host string, limits []string) (map[string]float64, error) {
	result := make(map[string]float64, len(limits))

	frontendHosts, err := i.peers.MemberCount(service.Frontend)
	if err != nil {
		return nil, fmt.Errorf("unable to get number of frontend hosts: %w", err)
	}

	typed := make([]algorithm.Limit, len(limits))
	for i := range limits {
		typed[i] = algorithm.Limit(limits[i])
	}
	weights, usedRPS := i.weights.HostWeights(algorithm.Identity(host), typed)
	for _, limit := range typed {
		target := i.limits.Get(string(limit))

		if weight, ok := weights[limit]; ok {
			// known limit with known weight for this host.
			// host is allowed a weighted amount of the target limit.
			weighted := target * weight
			result[string(limit)] = weighted
		}

		if used, ok := usedRPS[limit]; ok {
			// whether known-weight or not, every host also gets a fraction of the
			// unused RPS to go above its exact weight, to allow some free growth
			result[string(limit)] += max(0, (target-used)/float64(frontendHosts))
		}

		// if neither, the limit is completely unknown, caller can just use fallback.
		//
		// this could be a previously-cached value from the previous limit-owner (i.e. lost knowledge due to ring change),
		// or it may be a dumber safe default that the host computes on its own (likely target / frontendHosts).
		//
		// returning nothing allows it to choose based on whether it thinks this is lost data or unknown limit.
	}

	return result, nil
}

type numeric interface {
	~int | ~float64
}

func max[T numeric](a, b T) T {
	if a > b {
		return a
	}
	return b
}
