// Copyright (c) 2017-2020 Uber Technologies Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package resource

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/metrics"

	"github.com/uber/cadence/common/quotas/global/loadbalanced/aggregator"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/resource"
	"github.com/uber/cadence/common/service"
	"github.com/uber/cadence/service/history/config"
	"github.com/uber/cadence/service/history/events"
)

// Resource is the interface which expose common history resources
type Resource interface {
	resource.Resource
	GetEventCache() events.Cache
	GetRatelimitAggregator() aggregator.Aggregator
}

type resourceImpl struct {
	status int32

	resource.Resource
	eventCache   events.Cache
	ratelimitAgg aggregator.Aggregator
}

// Start starts all resources
func (h *resourceImpl) Start() {

	if !atomic.CompareAndSwapInt32(
		&h.status,
		common.DaemonStatusInitialized,
		common.DaemonStatusStarted,
	) {
		return
	}

	h.Resource.Start()
	h.GetLogger().Info("history resource started", tag.LifeCycleStarted)
}

// Stop stops all resources
func (h *resourceImpl) Stop() {

	if !atomic.CompareAndSwapInt32(
		&h.status,
		common.DaemonStatusStarted,
		common.DaemonStatusStopped,
	) {
		return
	}

	h.Resource.Stop()
	h.GetLogger().Info("history resource stopped", tag.LifeCycleStopped)
}

// GetEventCache return event cache
func (h *resourceImpl) GetEventCache() events.Cache {
	return h.eventCache
}

func (h *resourceImpl) GetRatelimitAggregator() aggregator.Aggregator {
	return h.ratelimitAgg
}

// New create a new resource containing common history dependencies
func New(
	params *resource.Params,
	serviceName string,
	config *config.Config,
) (historyResource Resource, retError error) {
	serviceResource, err := resource.New(
		params,
		serviceName,
		&service.Config{
			PersistenceMaxQPS:       config.PersistenceMaxQPS,
			PersistenceGlobalMaxQPS: config.PersistenceGlobalMaxQPS,
			ThrottledLoggerMaxRPS:   config.ThrottledLogRPS,

			EnableReadVisibilityFromES:      nil, // history service never read,
			AdvancedVisibilityWritingMode:   config.AdvancedVisibilityWritingMode,
			EnableReadVisibilityFromPinot:   nil, // history service never read,
			EnableLogCustomerQueryParameter: nil, // log customer parameter will be done in front-end

			EnableDBVisibilitySampling:                  config.EnableVisibilitySampling,
			EnableReadDBVisibilityFromClosedExecutionV2: nil, // history service never read,
			DBVisibilityListMaxQPS:                      nil, // history service never read,
			WriteDBVisibilityOpenMaxQPS:                 config.VisibilityOpenMaxQPS,
			WriteDBVisibilityClosedMaxQPS:               config.VisibilityClosedMaxQPS,

			ESVisibilityListMaxQPS: nil,                          // history service never read,
			ESIndexMaxResultWindow: nil,                          // history service never read,
			ValidSearchAttributes:  config.ValidSearchAttributes, // history service never read, (Pinot need this to initialize pinotQueryValidator)
		},
	)
	if err != nil {
		return nil, err
	}

	eventCache := events.NewGlobalCache(
		config.EventsCacheGlobalInitialCount(),
		config.EventsCacheGlobalMaxCount(),
		config.EventsCacheTTL(),
		serviceResource.GetHistoryManager(),
		params.Logger,
		params.MetricsClient,
		uint64(config.EventsCacheMaxSize()),
		serviceResource.GetDomainCache(),
	)

	agg, err := aggregator.New(
		// TODO: make a real config
		func(opts ...dynamicconfig.FilterOption) int {
			return 10
		},
		// TODO: make a real config
		func(opts ...dynamicconfig.FilterOption) time.Duration {
			return 10 * time.Second
		},
		params.Logger,
		params.MetricsClient.Scope(metrics.HistoryRatelimitStartupScope), // TODO: need new scope const / double check purpose of scope
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create ratelimit aggregator: %w", err)
	}

	historyResource = &resourceImpl{
		Resource:     serviceResource,
		eventCache:   eventCache,
		ratelimitAgg: agg,
	}
	return
}
