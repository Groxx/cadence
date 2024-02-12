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

package history

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pborman/uuid"
	"go.uber.org/yarpc/yarpcerrors"
	"golang.org/x/sync/errgroup"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/definition"
	"github.com/uber/cadence/common/future"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/membership"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/quotas"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/aggregator"
	"github.com/uber/cadence/common/quotas/global/loadbalanced/rpc"
	"github.com/uber/cadence/common/service"
	"github.com/uber/cadence/common/types"
	"github.com/uber/cadence/common/types/mapper/proto"
	"github.com/uber/cadence/service/history/config"
	"github.com/uber/cadence/service/history/engine"
	"github.com/uber/cadence/service/history/events"
	"github.com/uber/cadence/service/history/failover"
	"github.com/uber/cadence/service/history/replication"
	"github.com/uber/cadence/service/history/resource"
	"github.com/uber/cadence/service/history/shard"
	"github.com/uber/cadence/service/history/task"
	"github.com/uber/cadence/service/history/workflowcache"
)

const (
	shardOwnershipTransferDelay = 5 * time.Second
	workflowIDCacheTTL          = 1 * time.Second
	workflowIDCacheMaxCount     = 10_000
)

type (
	// handlerImpl is an implementation for history service independent of wire protocol
	handlerImpl struct {
		resource.Resource

		shuttingDown             int32
		controller               shard.Controller
		tokenSerializer          common.TaskTokenSerializer
		startWG                  sync.WaitGroup
		config                   *config.Config
		historyEventNotifier     events.Notifier
		rateLimiter              quotas.Limiter
		crossClusterTaskFetchers task.Fetchers
		replicationTaskFetchers  replication.TaskFetchers
		queueTaskProcessor       task.Processor
		failoverCoordinator      failover.Coordinator
		ratelimitAgg             aggregator.Aggregator
		workflowIDCache          workflowcache.WFCache
	}
)

var _ Handler = (*handlerImpl)(nil)
var _ shard.EngineFactory = (*handlerImpl)(nil)

var (
	errDomainNotSet            = &types.BadRequestError{Message: "Domain not set on request."}
	errWorkflowExecutionNotSet = &types.BadRequestError{Message: "WorkflowExecution not set on request."}
	errTaskListNotSet          = &types.BadRequestError{Message: "Tasklist not set."}
	errWorkflowIDNotSet        = &types.BadRequestError{Message: "WorkflowId is not set on request."}
	errRunIDNotValid           = &types.BadRequestError{Message: "RunID is not valid UUID."}
	errSourceClusterNotSet     = &types.BadRequestError{Message: "Source Cluster not set on request."}
	errTimestampNotSet         = &types.BadRequestError{Message: "Timestamp not set on request."}
	errInvalidTaskType         = &types.BadRequestError{Message: "Invalid task type"}
	errHistoryHostThrottle     = &types.ServiceBusyError{Message: "History host rps exceeded"}
	errShuttingDown            = &types.InternalServiceError{Message: "Shutting down"}
)

// NewHandler creates a thrift handler for the history service
func NewHandler(
	resource resource.Resource,
	config *config.Config,
) Handler {
	handler := &handlerImpl{
		Resource:        resource,
		config:          config,
		tokenSerializer: common.NewJSONTaskTokenSerializer(),
		rateLimiter:     quotas.NewDynamicRateLimiter(config.RPS.AsFloat64()),
		ratelimitAgg:    resource.GetRatelimitAggregator(),
		workflowIDCache: workflowcache.New(workflowcache.Params{
			TTL:                            workflowIDCacheTTL,
			ExternalLimiterFactory:         quotas.NewSimpleDynamicRateLimiterFactory(config.WorkflowIDExternalRPS),
			InternalLimiterFactory:         quotas.NewSimpleDynamicRateLimiterFactory(config.WorkflowIDInternalRPS),
			WorkflowIDCacheExternalEnabled: config.WorkflowIDCacheExternalEnabled,
			WorkflowIDCacheInternalEnabled: config.WorkflowIDCacheInternalEnabled,
			MaxCount:                       workflowIDCacheMaxCount,
			DomainCache:                    resource.GetDomainCache(),
			Logger:                         resource.GetLogger(),
			MetricsClient:                  resource.GetMetricsClient(),
		}),
	}

	// prevent us from trying to serve requests before shard controller is started and ready
	handler.startWG.Add(1)
	return handler
}

// Start starts the handler
func (h *handlerImpl) Start() {
	h.crossClusterTaskFetchers = task.NewCrossClusterTaskFetchers(
		h.GetClusterMetadata(),
		h.GetClientBean(),
		&task.FetcherOptions{
			Parallelism:                h.config.CrossClusterFetcherParallelism,
			AggregationInterval:        h.config.CrossClusterFetcherAggregationInterval,
			ServiceBusyBackoffInterval: h.config.CrossClusterFetcherServiceBusyBackoffInterval,
			ErrorRetryInterval:         h.config.CrossClusterFetcherErrorBackoffInterval,
			TimerJitterCoefficient:     h.config.CrossClusterFetcherJitterCoefficient,
		},
		h.GetMetricsClient(),
		h.GetLogger(),
	)
	h.crossClusterTaskFetchers.Start()

	h.replicationTaskFetchers = replication.NewTaskFetchers(
		h.GetLogger(),
		h.config,
		h.GetClusterMetadata(),
		h.GetClientBean(),
	)

	h.replicationTaskFetchers.Start()

	var err error
	taskPriorityAssigner := task.NewPriorityAssigner(
		h.GetClusterMetadata().GetCurrentClusterName(),
		h.GetDomainCache(),
		h.GetLogger(),
		h.GetMetricsClient(),
		h.config,
	)

	h.queueTaskProcessor, err = task.NewProcessor(
		taskPriorityAssigner,
		h.config,
		h.GetLogger(),
		h.GetMetricsClient(),
	)
	if err != nil {
		h.GetLogger().Fatal("Creating priority task processor failed", tag.Error(err))
	}
	h.queueTaskProcessor.Start()

	h.controller = shard.NewShardController(
		h.Resource,
		h,
		h.config,
	)
	h.historyEventNotifier = events.NewNotifier(h.GetTimeSource(), h.GetMetricsClient(), h.config.GetShardID)
	// events notifier must starts before controller
	h.historyEventNotifier.Start()

	h.failoverCoordinator = failover.NewCoordinator(
		h.GetDomainManager(),
		h.GetHistoryClient(),
		h.GetTimeSource(),
		h.GetDomainCache(),
		h.config,
		h.GetMetricsClient(),
		h.GetLogger(),
	)
	if h.config.EnableGracefulFailover() {
		h.failoverCoordinator.Start()
	}
	h.ratelimitAgg.Start()

	h.controller.Start()

	h.startWG.Done()
}

// Stop stops the handler
func (h *handlerImpl) Stop() {
	h.prepareToShutDown()
	h.crossClusterTaskFetchers.Stop()
	h.replicationTaskFetchers.Stop()
	h.queueTaskProcessor.Stop()
	h.controller.Stop()
	h.historyEventNotifier.Stop()
	h.failoverCoordinator.Stop()

	// TODO: raise this limit if we implement draining
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := h.ratelimitAgg.Stop(ctx)
	if err != nil {
		h.GetLogger().Error("failed to shut down ratelimit aggregator quickly", tag.Error(err))
		// non-fatal because it'll still stop eventually / the process will go away
	}
}

// PrepareToStop starts graceful traffic drain in preparation for shutdown
func (h *handlerImpl) PrepareToStop(remainingTime time.Duration) time.Duration {
	h.GetLogger().Info("ShutdownHandler: Initiating shardController shutdown")
	h.controller.PrepareToStop()
	h.GetLogger().Info("ShutdownHandler: Waiting for traffic to drain")
	remainingTime = common.SleepWithMinDuration(shardOwnershipTransferDelay, remainingTime)
	h.GetLogger().Info("ShutdownHandler: No longer taking rpc requests")
	h.prepareToShutDown()
	return remainingTime
}

func (h *handlerImpl) prepareToShutDown() {
	atomic.StoreInt32(&h.shuttingDown, 1)
}

func (h *handlerImpl) isShuttingDown() bool {
	return atomic.LoadInt32(&h.shuttingDown) != 0
}

// CreateEngine is implementation for HistoryEngineFactory used for creating the engine instance for shard
func (h *handlerImpl) CreateEngine(
	shardContext shard.Context,
) engine.Engine {
	return NewEngineWithShardContext(
		shardContext,
		h.GetVisibilityManager(),
		h.GetMatchingClient(),
		h.GetSDKClient(),
		h.historyEventNotifier,
		h.config,
		h.crossClusterTaskFetchers,
		h.replicationTaskFetchers,
		h.GetMatchingRawClient(),
		h.queueTaskProcessor,
		h.failoverCoordinator,
		h.workflowIDCache,
	)
}

// Health is for health check
func (h *handlerImpl) Health(ctx context.Context) (*types.HealthStatus, error) {
	h.startWG.Wait()
	h.GetLogger().Debug("History health check endpoint reached.")
	hs := &types.HealthStatus{Ok: true, Msg: "OK"}
	return hs, nil
}

// RecordActivityTaskHeartbeat - Record Activity Task Heart beat.
func (h *handlerImpl) RecordActivityTaskHeartbeat(
	ctx context.Context,
	wrappedRequest *types.HistoryRecordActivityTaskHeartbeatRequest,
) (resp *types.RecordActivityTaskHeartbeatResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRecordActivityTaskHeartbeatScope)
	defer sw.Stop()

	domainID := wrappedRequest.GetDomainUUID()
	if domainID == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return nil, h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	heartbeatRequest := wrappedRequest.HeartbeatRequest
	token, err0 := h.tokenSerializer.Deserialize(heartbeatRequest.TaskToken)
	if err0 != nil {
		err0 = &types.BadRequestError{Message: fmt.Sprintf("Error deserializing task token. Error: %v", err0)}
		return nil, h.error(err0, scope, domainID, "", "")
	}

	err0 = validateTaskToken(token)
	if err0 != nil {
		return nil, h.error(err0, scope, domainID, "", "")
	}
	workflowID := token.WorkflowID

	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return nil, h.error(err1, scope, domainID, workflowID, "")
	}

	response, err2 := engine.RecordActivityTaskHeartbeat(ctx, wrappedRequest)
	if err2 != nil {
		return nil, h.error(err2, scope, domainID, workflowID, "")
	}

	return response, nil
}

// RecordActivityTaskStarted - Record Activity Task started.
func (h *handlerImpl) RecordActivityTaskStarted(
	ctx context.Context,
	recordRequest *types.RecordActivityTaskStartedRequest,
) (resp *types.RecordActivityTaskStartedResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRecordActivityTaskStartedScope)
	defer sw.Stop()

	domainID := recordRequest.GetDomainUUID()
	workflowExecution := recordRequest.WorkflowExecution
	workflowID := workflowExecution.GetWorkflowID()

	h.emitInfoOrDebugLog(
		domainID,
		"RecordActivityTaskStarted",
		tag.WorkflowDomainID(domainID),
		tag.WorkflowID(workflowExecution.GetWorkflowID()),
		tag.WorkflowRunID(recordRequest.WorkflowExecution.RunID),
		tag.WorkflowScheduleID(recordRequest.GetScheduleID()),
	)

	if recordRequest.GetDomainUUID() == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, workflowID, "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return nil, h.error(errHistoryHostThrottle, scope, domainID, workflowID, "")
	}

	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return nil, h.error(err1, scope, domainID, workflowID, "")
	}

	response, err2 := engine.RecordActivityTaskStarted(ctx, recordRequest)
	if err2 != nil {
		return nil, h.error(err2, scope, domainID, workflowID, "")
	}

	return response, nil
}

// RecordDecisionTaskStarted - Record Decision Task started.
func (h *handlerImpl) RecordDecisionTaskStarted(
	ctx context.Context,
	recordRequest *types.RecordDecisionTaskStartedRequest,
) (resp *types.RecordDecisionTaskStartedResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRecordDecisionTaskStartedScope)
	defer sw.Stop()

	domainID := recordRequest.GetDomainUUID()
	workflowExecution := recordRequest.WorkflowExecution
	workflowID := workflowExecution.GetWorkflowID()
	runID := workflowExecution.GetRunID()

	h.emitInfoOrDebugLog(
		domainID,
		"RecordDecisionTaskStarted",
		tag.WorkflowDomainID(domainID),
		tag.WorkflowID(workflowExecution.GetWorkflowID()),
		tag.WorkflowRunID(recordRequest.WorkflowExecution.RunID),
		tag.WorkflowScheduleID(recordRequest.GetScheduleID()),
	)

	if domainID == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, workflowID, runID)
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return nil, h.error(errHistoryHostThrottle, scope, domainID, workflowID, runID)
	}

	if recordRequest.PollRequest == nil || recordRequest.PollRequest.TaskList.GetName() == "" {
		return nil, h.error(errTaskListNotSet, scope, domainID, workflowID, runID)
	}

	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		h.GetLogger().Error("RecordDecisionTaskStarted failed.",
			tag.Error(err1),
			tag.WorkflowID(recordRequest.WorkflowExecution.GetWorkflowID()),
			tag.WorkflowRunID(runID),
			tag.WorkflowRunID(recordRequest.WorkflowExecution.GetRunID()),
			tag.WorkflowScheduleID(recordRequest.GetScheduleID()),
		)
		return nil, h.error(err1, scope, domainID, workflowID, runID)
	}

	response, err2 := engine.RecordDecisionTaskStarted(ctx, recordRequest)
	if err2 != nil {
		return nil, h.error(err2, scope, domainID, workflowID, runID)
	}

	return response, nil
}

// RespondActivityTaskCompleted - records completion of an activity task
func (h *handlerImpl) RespondActivityTaskCompleted(
	ctx context.Context,
	wrappedRequest *types.HistoryRespondActivityTaskCompletedRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRespondActivityTaskCompletedScope)
	defer sw.Stop()

	domainID := wrappedRequest.GetDomainUUID()
	if domainID == "" {
		return h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	completeRequest := wrappedRequest.CompleteRequest
	token, err0 := h.tokenSerializer.Deserialize(completeRequest.TaskToken)
	if err0 != nil {
		err0 = &types.BadRequestError{Message: fmt.Sprintf("Error deserializing task token. Error: %v", err0)}
		return h.error(err0, scope, domainID, "", "")
	}

	err0 = validateTaskToken(token)
	if err0 != nil {
		return h.error(err0, scope, domainID, "", "")
	}
	workflowID := token.WorkflowID
	runID := token.RunID

	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return h.error(err1, scope, domainID, workflowID, runID)
	}

	err2 := engine.RespondActivityTaskCompleted(ctx, wrappedRequest)
	if err2 != nil {
		return h.error(err2, scope, domainID, workflowID, runID)
	}

	return nil
}

// RespondActivityTaskFailed - records failure of an activity task
func (h *handlerImpl) RespondActivityTaskFailed(
	ctx context.Context,
	wrappedRequest *types.HistoryRespondActivityTaskFailedRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRespondActivityTaskFailedScope)
	defer sw.Stop()

	domainID := wrappedRequest.GetDomainUUID()
	if domainID == "" {
		return h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	failRequest := wrappedRequest.FailedRequest
	token, err0 := h.tokenSerializer.Deserialize(failRequest.TaskToken)
	if err0 != nil {
		err0 = &types.BadRequestError{Message: fmt.Sprintf("Error deserializing task token. Error: %v", err0)}
		return h.error(err0, scope, domainID, "", "")
	}

	err0 = validateTaskToken(token)
	if err0 != nil {
		return h.error(err0, scope, domainID, "", "")
	}
	workflowID := token.WorkflowID
	runID := token.RunID

	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return h.error(err1, scope, domainID, workflowID, runID)
	}

	err2 := engine.RespondActivityTaskFailed(ctx, wrappedRequest)
	if err2 != nil {
		return h.error(err2, scope, domainID, workflowID, runID)
	}

	return nil
}

// RespondActivityTaskCanceled - records failure of an activity task
func (h *handlerImpl) RespondActivityTaskCanceled(
	ctx context.Context,
	wrappedRequest *types.HistoryRespondActivityTaskCanceledRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRespondActivityTaskCanceledScope)
	defer sw.Stop()

	domainID := wrappedRequest.GetDomainUUID()
	if domainID == "" {
		return h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	cancelRequest := wrappedRequest.CancelRequest
	token, err0 := h.tokenSerializer.Deserialize(cancelRequest.TaskToken)
	if err0 != nil {
		err0 = &types.BadRequestError{Message: fmt.Sprintf("Error deserializing task token. Error: %v", err0)}
		return h.error(err0, scope, domainID, "", "")
	}

	err0 = validateTaskToken(token)
	if err0 != nil {
		return h.error(err0, scope, domainID, "", "")
	}
	workflowID := token.WorkflowID
	runID := token.RunID

	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return h.error(err1, scope, domainID, workflowID, runID)
	}

	err2 := engine.RespondActivityTaskCanceled(ctx, wrappedRequest)
	if err2 != nil {
		return h.error(err2, scope, domainID, workflowID, runID)
	}

	return nil
}

// RespondDecisionTaskCompleted - records completion of a decision task
func (h *handlerImpl) RespondDecisionTaskCompleted(
	ctx context.Context,
	wrappedRequest *types.HistoryRespondDecisionTaskCompletedRequest,
) (resp *types.HistoryRespondDecisionTaskCompletedResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRespondDecisionTaskCompletedScope)
	defer sw.Stop()

	domainID := wrappedRequest.GetDomainUUID()
	if domainID == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return nil, h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	completeRequest := wrappedRequest.CompleteRequest
	if len(completeRequest.Decisions) == 0 {
		scope.IncCounter(metrics.EmptyCompletionDecisionsCounter)
	}
	token, err0 := h.tokenSerializer.Deserialize(completeRequest.TaskToken)
	if err0 != nil {
		err0 = &types.BadRequestError{Message: fmt.Sprintf("Error deserializing task token. Error: %v", err0)}
		return nil, h.error(err0, scope, domainID, "", "")
	}

	h.GetLogger().Debug(fmt.Sprintf("RespondDecisionTaskCompleted. DomainID: %v, WorkflowID: %v, RunID: %v, ScheduleID: %v",
		token.DomainID,
		token.WorkflowID,
		token.RunID,
		token.ScheduleID))

	err0 = validateTaskToken(token)
	if err0 != nil {
		return nil, h.error(err0, scope, domainID, "", "")
	}
	workflowID := token.WorkflowID
	runID := token.RunID

	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return nil, h.error(err1, scope, domainID, workflowID, runID)
	}

	response, err2 := engine.RespondDecisionTaskCompleted(ctx, wrappedRequest)
	if err2 != nil {
		return nil, h.error(err2, scope, domainID, workflowID, runID)
	}

	return response, nil
}

// RespondDecisionTaskFailed - failed response to decision task
func (h *handlerImpl) RespondDecisionTaskFailed(
	ctx context.Context,
	wrappedRequest *types.HistoryRespondDecisionTaskFailedRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRespondDecisionTaskFailedScope)
	defer sw.Stop()

	domainID := wrappedRequest.GetDomainUUID()
	if domainID == "" {
		return h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	failedRequest := wrappedRequest.FailedRequest
	token, err0 := h.tokenSerializer.Deserialize(failedRequest.TaskToken)
	if err0 != nil {
		err0 = &types.BadRequestError{Message: fmt.Sprintf("Error deserializing task token. Error: %v", err0)}
		return h.error(err0, scope, domainID, "", "")
	}

	h.GetLogger().Debug(fmt.Sprintf("RespondDecisionTaskFailed. DomainID: %v, WorkflowID: %v, RunID: %v, ScheduleID: %v",
		token.DomainID,
		token.WorkflowID,
		token.RunID,
		token.ScheduleID))

	if failedRequest != nil && failedRequest.GetCause() == types.DecisionTaskFailedCauseUnhandledDecision {
		h.GetLogger().Info("Non-Deterministic Error", tag.WorkflowDomainID(token.DomainID), tag.WorkflowID(token.WorkflowID), tag.WorkflowRunID(token.RunID))
		domainName, err := h.GetDomainCache().GetDomainName(token.DomainID)
		var domainTag metrics.Tag

		if err == nil {
			domainTag = metrics.DomainTag(domainName)
		} else {
			domainTag = metrics.DomainUnknownTag()
		}

		scope.Tagged(domainTag).IncCounter(metrics.CadenceErrNonDeterministicCounter)
	}
	err0 = validateTaskToken(token)
	if err0 != nil {
		return h.error(err0, scope, domainID, "", "")
	}
	workflowID := token.WorkflowID
	runID := token.RunID

	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return h.error(err1, scope, domainID, workflowID, runID)
	}

	err2 := engine.RespondDecisionTaskFailed(ctx, wrappedRequest)
	if err2 != nil {
		return h.error(err2, scope, domainID, workflowID, runID)
	}

	return nil
}

// StartWorkflowExecution - creates a new workflow execution
func (h *handlerImpl) StartWorkflowExecution(
	ctx context.Context,
	wrappedRequest *types.HistoryStartWorkflowExecutionRequest,
) (resp *types.StartWorkflowExecutionResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryStartWorkflowExecutionScope)
	defer sw.Stop()

	domainID := wrappedRequest.GetDomainUUID()
	if domainID == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return nil, h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	startRequest := wrappedRequest.StartRequest
	workflowID := startRequest.GetWorkflowID()

	h.workflowIDCache.AllowExternal(domainID, workflowID)

	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return nil, h.error(err1, scope, domainID, workflowID, "")
	}

	response, err2 := engine.StartWorkflowExecution(ctx, wrappedRequest)
	runID := response.GetRunID()
	if err2 != nil {
		return nil, h.error(err2, scope, domainID, workflowID, runID)
	}

	return response, nil
}

// DescribeHistoryHost returns information about the internal states of a history host
func (h *handlerImpl) DescribeHistoryHost(
	ctx context.Context,
	request *types.DescribeHistoryHostRequest,
) (resp *types.DescribeHistoryHostResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	numOfItemsInCacheByID, numOfItemsInCacheByName := h.GetDomainCache().GetCacheSize()
	status := ""
	switch h.controller.Status() {
	case common.DaemonStatusInitialized:
		status = "initialized"
	case common.DaemonStatusStarted:
		status = "started"
	case common.DaemonStatusStopped:
		status = "stopped"
	}

	resp = &types.DescribeHistoryHostResponse{
		NumberOfShards: int32(h.controller.NumShards()),
		ShardIDs:       h.controller.ShardIDs(),
		DomainCache: &types.DomainCacheInfo{
			NumOfItemsInCacheByID:   numOfItemsInCacheByID,
			NumOfItemsInCacheByName: numOfItemsInCacheByName,
		},
		ShardControllerStatus: status,
		Address:               h.GetHostInfo().GetAddress(),
	}
	return resp, nil
}

// RemoveTask returns information about the internal states of a history host
func (h *handlerImpl) RemoveTask(
	ctx context.Context,
	request *types.RemoveTaskRequest,
) (retError error) {
	executionMgr, err := h.GetExecutionManager(int(request.GetShardID()))
	if err != nil {
		return err
	}

	switch taskType := common.TaskType(request.GetType()); taskType {
	case common.TaskTypeTransfer:
		return executionMgr.CompleteTransferTask(ctx, &persistence.CompleteTransferTaskRequest{
			TaskID: request.GetTaskID(),
		})
	case common.TaskTypeTimer:
		return executionMgr.CompleteTimerTask(ctx, &persistence.CompleteTimerTaskRequest{
			VisibilityTimestamp: time.Unix(0, request.GetVisibilityTimestamp()),
			TaskID:              request.GetTaskID(),
		})
	case common.TaskTypeReplication:
		return executionMgr.CompleteReplicationTask(ctx, &persistence.CompleteReplicationTaskRequest{
			TaskID: request.GetTaskID(),
		})
	case common.TaskTypeCrossCluster:
		return executionMgr.CompleteCrossClusterTask(ctx, &persistence.CompleteCrossClusterTaskRequest{
			TargetCluster: request.GetClusterName(),
			TaskID:        request.GetTaskID(),
		})
	default:
		return errInvalidTaskType
	}
}

// CloseShard closes a shard hosted by this instance
func (h *handlerImpl) CloseShard(
	ctx context.Context,
	request *types.CloseShardRequest,
) (retError error) {
	h.controller.RemoveEngineForShard(int(request.GetShardID()))
	return nil
}

// ResetQueue resets processing queue states
func (h *handlerImpl) ResetQueue(
	ctx context.Context,
	request *types.ResetQueueRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryResetQueueScope)
	defer sw.Stop()

	engine, err := h.controller.GetEngineForShard(int(request.GetShardID()))
	if err != nil {
		return h.error(err, scope, "", "", "")
	}

	switch taskType := common.TaskType(request.GetType()); taskType {
	case common.TaskTypeTransfer:
		err = engine.ResetTransferQueue(ctx, request.GetClusterName())
	case common.TaskTypeTimer:
		err = engine.ResetTimerQueue(ctx, request.GetClusterName())
	case common.TaskTypeCrossCluster:
		err = engine.ResetCrossClusterQueue(ctx, request.GetClusterName())
	default:
		err = errInvalidTaskType
	}

	if err != nil {
		return h.error(err, scope, "", "", "")
	}
	return nil
}

// DescribeQueue describes processing queue states
func (h *handlerImpl) DescribeQueue(
	ctx context.Context,
	request *types.DescribeQueueRequest,
) (resp *types.DescribeQueueResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryDescribeQueueScope)
	defer sw.Stop()

	engine, err := h.controller.GetEngineForShard(int(request.GetShardID()))
	if err != nil {
		return nil, h.error(err, scope, "", "", "")
	}

	switch taskType := common.TaskType(request.GetType()); taskType {
	case common.TaskTypeTransfer:
		resp, err = engine.DescribeTransferQueue(ctx, request.GetClusterName())
	case common.TaskTypeTimer:
		resp, err = engine.DescribeTimerQueue(ctx, request.GetClusterName())
	case common.TaskTypeCrossCluster:
		resp, err = engine.DescribeCrossClusterQueue(ctx, request.GetClusterName())
	default:
		err = errInvalidTaskType
	}

	if err != nil {
		return nil, h.error(err, scope, "", "", "")
	}
	return resp, nil
}

// DescribeMutableState - returns the internal analysis of workflow execution state
func (h *handlerImpl) DescribeMutableState(
	ctx context.Context,
	request *types.DescribeMutableStateRequest,
) (resp *types.DescribeMutableStateResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryDescribeMutabelStateScope)
	defer sw.Stop()

	domainID := request.GetDomainUUID()
	if domainID == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, "", "")
	}

	workflowExecution := request.Execution
	workflowID := workflowExecution.GetWorkflowID()
	runID := workflowExecution.GetRunID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return nil, h.error(err1, scope, domainID, workflowID, runID)
	}

	resp, err2 := engine.DescribeMutableState(ctx, request)
	if err2 != nil {
		return nil, h.error(err2, scope, domainID, workflowID, runID)
	}
	return resp, nil
}

// GetMutableState - returns the id of the next event in the execution's history
func (h *handlerImpl) GetMutableState(
	ctx context.Context,
	getRequest *types.GetMutableStateRequest,
) (resp *types.GetMutableStateResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryGetMutableStateScope)
	defer sw.Stop()

	domainID := getRequest.GetDomainUUID()
	if domainID == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return nil, h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	workflowExecution := getRequest.Execution
	workflowID := workflowExecution.GetWorkflowID()
	runID := workflowExecution.GetWorkflowID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return nil, h.error(err1, scope, domainID, workflowID, runID)
	}

	resp, err2 := engine.GetMutableState(ctx, getRequest)
	if err2 != nil {
		return nil, h.error(err2, scope, domainID, workflowID, runID)
	}
	return resp, nil
}

// PollMutableState - returns the id of the next event in the execution's history
func (h *handlerImpl) PollMutableState(
	ctx context.Context,
	getRequest *types.PollMutableStateRequest,
) (resp *types.PollMutableStateResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryPollMutableStateScope)
	defer sw.Stop()

	domainID := getRequest.GetDomainUUID()
	if domainID == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return nil, h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	workflowExecution := getRequest.Execution
	workflowID := workflowExecution.GetWorkflowID()
	runID := workflowExecution.GetRunID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return nil, h.error(err1, scope, domainID, workflowID, runID)
	}

	resp, err2 := engine.PollMutableState(ctx, getRequest)
	if err2 != nil {
		return nil, h.error(err2, scope, domainID, workflowID, runID)
	}
	return resp, nil
}

// DescribeWorkflowExecution returns information about the specified workflow execution.
func (h *handlerImpl) DescribeWorkflowExecution(
	ctx context.Context,
	request *types.HistoryDescribeWorkflowExecutionRequest,
) (resp *types.DescribeWorkflowExecutionResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryDescribeWorkflowExecutionScope)
	defer sw.Stop()

	domainID := request.GetDomainUUID()
	if domainID == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return nil, h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	workflowExecution := request.Request.Execution
	workflowID := workflowExecution.GetWorkflowID()
	runID := workflowExecution.GetRunID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return nil, h.error(err1, scope, domainID, workflowID, runID)
	}

	resp, err2 := engine.DescribeWorkflowExecution(ctx, request)
	if err2 != nil {
		return nil, h.error(err2, scope, domainID, workflowID, runID)
	}
	return resp, nil
}

// RequestCancelWorkflowExecution - requests cancellation of a workflow
func (h *handlerImpl) RequestCancelWorkflowExecution(
	ctx context.Context,
	request *types.HistoryRequestCancelWorkflowExecutionRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRequestCancelWorkflowExecutionScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return errShuttingDown
	}

	domainID := request.GetDomainUUID()
	if domainID == "" || request.CancelRequest.GetDomain() == "" {
		return h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	cancelRequest := request.CancelRequest
	h.GetLogger().Debug(fmt.Sprintf("RequestCancelWorkflowExecution. DomainID: %v/%v, WorkflowID: %v, RunID: %v.",
		cancelRequest.GetDomain(),
		request.GetDomainUUID(),
		cancelRequest.WorkflowExecution.GetWorkflowID(),
		cancelRequest.WorkflowExecution.GetRunID()))

	workflowID := cancelRequest.WorkflowExecution.GetWorkflowID()
	runID := cancelRequest.WorkflowExecution.GetRunID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return h.error(err1, scope, domainID, workflowID, runID)
	}

	err2 := engine.RequestCancelWorkflowExecution(ctx, request)
	if err2 != nil {
		return h.error(err2, scope, domainID, workflowID, runID)
	}

	return nil
}

// SignalWorkflowExecution is used to send a signal event to running workflow execution.  This results in
// WorkflowExecutionSignaled event recorded in the history and a decision task being created for the execution.
func (h *handlerImpl) SignalWorkflowExecution(
	ctx context.Context,
	wrappedRequest *types.HistorySignalWorkflowExecutionRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistorySignalWorkflowExecutionScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return errShuttingDown
	}

	domainID := wrappedRequest.GetDomainUUID()
	if domainID == "" {
		return h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	workflowExecution := wrappedRequest.SignalRequest.WorkflowExecution
	workflowID := workflowExecution.GetWorkflowID()
	runID := workflowExecution.GetRunID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return h.error(err1, scope, domainID, workflowID, runID)
	}

	err2 := engine.SignalWorkflowExecution(ctx, wrappedRequest)
	if err2 != nil {
		return h.error(err2, scope, domainID, workflowID, runID)
	}

	return nil
}

// SignalWithStartWorkflowExecution is used to ensure sending a signal event to a workflow execution.
// If workflow is running, this results in WorkflowExecutionSignaled event recorded in the history
// and a decision task being created for the execution.
// If workflow is not running or not found, this results in WorkflowExecutionStarted and WorkflowExecutionSignaled
// event recorded in history, and a decision task being created for the execution
func (h *handlerImpl) SignalWithStartWorkflowExecution(
	ctx context.Context,
	wrappedRequest *types.HistorySignalWithStartWorkflowExecutionRequest,
) (resp *types.StartWorkflowExecutionResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistorySignalWithStartWorkflowExecutionScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	domainID := wrappedRequest.GetDomainUUID()
	if domainID == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return nil, h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	signalWithStartRequest := wrappedRequest.SignalWithStartRequest
	workflowID := signalWithStartRequest.GetWorkflowID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return nil, h.error(err1, scope, domainID, workflowID, "")
	}

	resp, err2 := engine.SignalWithStartWorkflowExecution(ctx, wrappedRequest)
	if err2 == nil {
		return resp, nil
	}
	// Two simultaneous SignalWithStart requests might try to start a workflow at the same time.
	// This can result in one of the requests failing with one of two possible errors:
	//    1) If it is a brand new WF ID, one of the requests can fail with WorkflowExecutionAlreadyStartedError
	//       (createMode is persistence.CreateWorkflowModeBrandNew)
	//    2) If it an already existing WF ID, one of the requests can fail with a CurrentWorkflowConditionFailedError
	//       (createMode is persisetence.CreateWorkflowModeWorkflowIDReuse)
	// If either error occurs, just go ahead and retry. It should succeed on the subsequent attempt.
	var e1 *persistence.WorkflowExecutionAlreadyStartedError
	var e2 *persistence.CurrentWorkflowConditionFailedError
	if !errors.As(err2, &e1) && !errors.As(err2, &e2) {
		return nil, h.error(err2, scope, domainID, workflowID, resp.GetRunID())
	}

	resp, err2 = engine.SignalWithStartWorkflowExecution(ctx, wrappedRequest)
	if err2 != nil {
		return nil, h.error(err2, scope, domainID, workflowID, resp.GetRunID())
	}
	return resp, nil
}

// RemoveSignalMutableState is used to remove a signal request ID that was previously recorded.  This is currently
// used to clean execution info when signal decision finished.
func (h *handlerImpl) RemoveSignalMutableState(
	ctx context.Context,
	wrappedRequest *types.RemoveSignalMutableStateRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRemoveSignalMutableStateScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return errShuttingDown
	}

	domainID := wrappedRequest.GetDomainUUID()
	if domainID == "" {
		return h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	workflowExecution := wrappedRequest.WorkflowExecution
	workflowID := workflowExecution.GetWorkflowID()
	runID := workflowExecution.GetRunID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return h.error(err1, scope, domainID, workflowID, runID)
	}

	err2 := engine.RemoveSignalMutableState(ctx, wrappedRequest)
	if err2 != nil {
		return h.error(err2, scope, domainID, workflowID, runID)
	}

	return nil
}

// TerminateWorkflowExecution terminates an existing workflow execution by recording WorkflowExecutionTerminated event
// in the history and immediately terminating the execution instance.
func (h *handlerImpl) TerminateWorkflowExecution(
	ctx context.Context,
	wrappedRequest *types.HistoryTerminateWorkflowExecutionRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryTerminateWorkflowExecutionScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return errShuttingDown
	}

	domainID := wrappedRequest.GetDomainUUID()
	if domainID == "" {
		return h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	workflowExecution := wrappedRequest.TerminateRequest.WorkflowExecution
	workflowID := workflowExecution.GetWorkflowID()
	runID := workflowExecution.GetRunID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return h.error(err1, scope, domainID, workflowID, runID)
	}

	err2 := engine.TerminateWorkflowExecution(ctx, wrappedRequest)
	if err2 != nil {
		return h.error(err2, scope, domainID, workflowID, runID)
	}

	return nil
}

// ResetWorkflowExecution reset an existing workflow execution
// in the history and immediately terminating the execution instance.
func (h *handlerImpl) ResetWorkflowExecution(
	ctx context.Context,
	wrappedRequest *types.HistoryResetWorkflowExecutionRequest,
) (resp *types.ResetWorkflowExecutionResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryResetWorkflowExecutionScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	domainID := wrappedRequest.GetDomainUUID()
	if domainID == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return nil, h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	workflowExecution := wrappedRequest.ResetRequest.WorkflowExecution
	workflowID := workflowExecution.GetWorkflowID()
	runID := workflowExecution.GetRunID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return nil, h.error(err1, scope, domainID, workflowID, runID)
	}

	resp, err2 := engine.ResetWorkflowExecution(ctx, wrappedRequest)
	if err2 != nil {
		return nil, h.error(err2, scope, domainID, workflowID, runID)
	}

	return resp, nil
}

// QueryWorkflow queries a types.
func (h *handlerImpl) QueryWorkflow(
	ctx context.Context,
	request *types.HistoryQueryWorkflowRequest,
) (resp *types.HistoryQueryWorkflowResponse, retError error) {
	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryQueryWorkflowScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	domainID := request.GetDomainUUID()
	if domainID == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return nil, h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	workflowID := request.GetRequest().GetExecution().GetWorkflowID()
	runID := request.GetRequest().GetExecution().GetRunID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return nil, h.error(err1, scope, domainID, workflowID, runID)
	}

	resp, err2 := engine.QueryWorkflow(ctx, request)
	if err2 != nil {
		return nil, h.error(err2, scope, domainID, workflowID, runID)
	}

	return resp, nil
}

// ScheduleDecisionTask is used for creating a decision task for already started workflow execution.  This is mainly
// used by transfer queue processor during the processing of StartChildWorkflowExecution task, where it first starts
// child execution without creating the decision task and then calls this API after updating the mutable state of
// parent execution.
func (h *handlerImpl) ScheduleDecisionTask(
	ctx context.Context,
	request *types.ScheduleDecisionTaskRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryScheduleDecisionTaskScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return errShuttingDown
	}

	domainID := request.GetDomainUUID()
	if domainID == "" {
		return h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	if request.WorkflowExecution == nil {
		return h.error(errWorkflowExecutionNotSet, scope, domainID, "", "")
	}

	workflowExecution := request.WorkflowExecution
	workflowID := workflowExecution.GetWorkflowID()
	runID := workflowExecution.GetRunID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return h.error(err1, scope, domainID, workflowID, runID)
	}

	err2 := engine.ScheduleDecisionTask(ctx, request)
	if err2 != nil {
		return h.error(err2, scope, domainID, workflowID, runID)
	}

	return nil
}

// RecordChildExecutionCompleted is used for reporting the completion of child workflow execution to parent.
// This is mainly called by transfer queue processor during the processing of DeleteExecution task.
func (h *handlerImpl) RecordChildExecutionCompleted(
	ctx context.Context,
	request *types.RecordChildExecutionCompletedRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRecordChildExecutionCompletedScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return errShuttingDown
	}

	domainID := request.GetDomainUUID()
	if domainID == "" {
		return h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	if request.WorkflowExecution == nil {
		return h.error(errWorkflowExecutionNotSet, scope, domainID, "", "")
	}

	workflowExecution := request.WorkflowExecution
	workflowID := workflowExecution.GetWorkflowID()
	runID := workflowExecution.GetRunID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return h.error(err1, scope, domainID, workflowID, runID)
	}

	err2 := engine.RecordChildExecutionCompleted(ctx, request)
	if err2 != nil {
		return h.error(err2, scope, domainID, workflowID, runID)
	}

	return nil
}

// ResetStickyTaskList reset the volatile information in mutable state of a given types.
// Volatile information are the information related to client, such as:
// 1. StickyTaskList
// 2. StickyScheduleToStartTimeout
// 3. ClientLibraryVersion
// 4. ClientFeatureVersion
// 5. ClientImpl
func (h *handlerImpl) ResetStickyTaskList(
	ctx context.Context,
	resetRequest *types.HistoryResetStickyTaskListRequest,
) (resp *types.HistoryResetStickyTaskListResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryResetStickyTaskListScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	domainID := resetRequest.GetDomainUUID()
	if domainID == "" {
		return nil, h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return nil, h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	workflowID := resetRequest.Execution.GetWorkflowID()
	runID := resetRequest.Execution.GetRunID()
	engine, err := h.controller.GetEngine(workflowID)
	if err != nil {
		return nil, h.error(err, scope, domainID, workflowID, runID)
	}

	resp, err = engine.ResetStickyTaskList(ctx, resetRequest)
	if err != nil {
		return nil, h.error(err, scope, domainID, workflowID, runID)
	}

	return resp, nil
}

// ReplicateEventsV2 is called by processor to replicate history events for passive domains
func (h *handlerImpl) ReplicateEventsV2(
	ctx context.Context,
	replicateRequest *types.ReplicateEventsV2Request,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	if h.isShuttingDown() {
		return errShuttingDown
	}

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryReplicateEventsV2Scope)
	defer sw.Stop()

	domainID := replicateRequest.GetDomainUUID()
	if domainID == "" {
		return h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	workflowExecution := replicateRequest.WorkflowExecution
	workflowID := workflowExecution.GetWorkflowID()
	runID := workflowExecution.GetRunID()
	engine, err1 := h.controller.GetEngine(workflowID)
	if err1 != nil {
		return h.error(err1, scope, domainID, workflowID, runID)
	}

	err2 := engine.ReplicateEventsV2(ctx, replicateRequest)
	if err2 != nil {
		return h.error(err2, scope, domainID, workflowID, runID)
	}

	return nil
}

// SyncShardStatus is called by processor to sync history shard information from another cluster
func (h *handlerImpl) SyncShardStatus(
	ctx context.Context,
	syncShardStatusRequest *types.SyncShardStatusRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistorySyncShardStatusScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return errShuttingDown
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, "", "", "")
	}

	if syncShardStatusRequest.SourceCluster == "" {
		return h.error(errSourceClusterNotSet, scope, "", "", "")
	}

	if syncShardStatusRequest.Timestamp == nil {
		return h.error(errTimestampNotSet, scope, "", "", "")
	}

	// shard ID is already provided in the request
	engine, err := h.controller.GetEngineForShard(int(syncShardStatusRequest.GetShardID()))
	if err != nil {
		return h.error(err, scope, "", "", "")
	}

	err = engine.SyncShardStatus(ctx, syncShardStatusRequest)
	if err != nil {
		return h.error(err, scope, "", "", "")
	}

	return nil
}

// SyncActivity is called by processor to sync activity
func (h *handlerImpl) SyncActivity(
	ctx context.Context,
	syncActivityRequest *types.SyncActivityRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistorySyncActivityScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return errShuttingDown
	}

	domainID := syncActivityRequest.GetDomainID()
	if syncActivityRequest.DomainID == "" || uuid.Parse(syncActivityRequest.GetDomainID()) == nil {
		return h.error(errDomainNotSet, scope, domainID, "", "")
	}

	if ok := h.rateLimiter.Allow(); !ok {
		return h.error(errHistoryHostThrottle, scope, domainID, "", "")
	}

	if syncActivityRequest.WorkflowID == "" {
		return h.error(errWorkflowIDNotSet, scope, domainID, "", "")
	}

	if syncActivityRequest.RunID == "" || uuid.Parse(syncActivityRequest.GetRunID()) == nil {
		return h.error(errRunIDNotValid, scope, domainID, "", "")
	}

	workflowID := syncActivityRequest.GetWorkflowID()
	runID := syncActivityRequest.GetRunID()
	engine, err := h.controller.GetEngine(workflowID)
	if err != nil {
		return h.error(err, scope, domainID, workflowID, runID)
	}

	err = engine.SyncActivity(ctx, syncActivityRequest)
	if err != nil {
		return h.error(err, scope, domainID, workflowID, runID)
	}

	return nil
}

// GetReplicationMessages is called by remote peers to get replicated messages for cross DC replication
func (h *handlerImpl) GetReplicationMessages(
	ctx context.Context,
	request *types.GetReplicationMessagesRequest,
) (resp *types.GetReplicationMessagesResponse, retError error) {
	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	h.GetLogger().Debug("Received GetReplicationMessages call.")

	_, sw := h.startRequestProfile(ctx, metrics.HistoryGetReplicationMessagesScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	var wg sync.WaitGroup
	wg.Add(len(request.Tokens))
	result := new(sync.Map)

	for _, token := range request.Tokens {
		go func(token *types.ReplicationToken) {
			defer wg.Done()

			engine, err := h.controller.GetEngineForShard(int(token.GetShardID()))
			if err != nil {
				h.GetLogger().Warn("History engine not found for shard", tag.Error(err))
				return
			}
			tasks, err := engine.GetReplicationMessages(
				ctx,
				request.GetClusterName(),
				token.GetLastRetrievedMessageID(),
			)
			if err != nil {
				h.GetLogger().Warn("Failed to get replication tasks for shard", tag.Error(err))
				return
			}

			result.Store(token.GetShardID(), tasks)
		}(token)
	}

	wg.Wait()

	responseSize := 0
	maxResponseSize := h.config.MaxResponseSize

	messagesByShard := make(map[int32]*types.ReplicationMessages)
	result.Range(func(key, value interface{}) bool {
		shardID := key.(int32)
		tasks := value.(*types.ReplicationMessages)

		size := proto.FromReplicationMessages(tasks).Size()
		if (responseSize + size) >= maxResponseSize {
			// Log shards that did not fit for debugging purposes
			h.GetLogger().Warn("Replication messages did not fit in the response (history host)",
				tag.ShardID(int(shardID)),
				tag.ResponseSize(size),
				tag.ResponseTotalSize(responseSize),
				tag.ResponseMaxSize(maxResponseSize),
			)
		} else {
			responseSize += size
			messagesByShard[shardID] = tasks
		}

		return true
	})

	h.GetLogger().Debug("GetReplicationMessages succeeded.")

	return &types.GetReplicationMessagesResponse{MessagesByShard: messagesByShard}, nil
}

// GetDLQReplicationMessages is called by remote peers to get replicated messages for DLQ merging
func (h *handlerImpl) GetDLQReplicationMessages(
	ctx context.Context,
	request *types.GetDLQReplicationMessagesRequest,
) (resp *types.GetDLQReplicationMessagesResponse, retError error) {
	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	_, sw := h.startRequestProfile(ctx, metrics.HistoryGetDLQReplicationMessagesScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	taskInfoPerExecution := map[definition.WorkflowIdentifier][]*types.ReplicationTaskInfo{}
	// do batch based on workflow ID and run ID
	for _, taskInfo := range request.GetTaskInfos() {
		identity := definition.NewWorkflowIdentifier(
			taskInfo.GetDomainID(),
			taskInfo.GetWorkflowID(),
			taskInfo.GetRunID(),
		)
		if _, ok := taskInfoPerExecution[identity]; !ok {
			taskInfoPerExecution[identity] = []*types.ReplicationTaskInfo{}
		}
		taskInfoPerExecution[identity] = append(taskInfoPerExecution[identity], taskInfo)
	}

	var wg sync.WaitGroup
	wg.Add(len(taskInfoPerExecution))
	tasksChan := make(chan *types.ReplicationTask, len(request.GetTaskInfos()))
	handleTaskInfoPerExecution := func(taskInfos []*types.ReplicationTaskInfo) {
		defer wg.Done()
		if len(taskInfos) == 0 {
			return
		}

		engine, err := h.controller.GetEngine(
			taskInfos[0].GetWorkflowID(),
		)
		if err != nil {
			h.GetLogger().Warn("History engine not found for workflow ID.", tag.Error(err))
			return
		}

		tasks, err := engine.GetDLQReplicationMessages(
			ctx,
			taskInfos,
		)
		if err != nil {
			h.GetLogger().Error("Failed to get dlq replication tasks.", tag.Error(err))
			return
		}

		for _, task := range tasks {
			tasksChan <- task
		}
	}

	for _, replicationTaskInfos := range taskInfoPerExecution {
		go handleTaskInfoPerExecution(replicationTaskInfos)
	}
	wg.Wait()
	close(tasksChan)

	replicationTasks := make([]*types.ReplicationTask, 0, len(tasksChan))
	for task := range tasksChan {
		replicationTasks = append(replicationTasks, task)
	}
	return &types.GetDLQReplicationMessagesResponse{
		ReplicationTasks: replicationTasks,
	}, nil
}

// ReapplyEvents applies stale events to the current workflow and the current run
func (h *handlerImpl) ReapplyEvents(
	ctx context.Context,
	request *types.HistoryReapplyEventsRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryReapplyEventsScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return errShuttingDown
	}

	domainID := request.GetDomainUUID()
	workflowID := request.GetRequest().GetWorkflowExecution().GetWorkflowID()
	runID := request.GetRequest().GetWorkflowExecution().GetRunID()
	engine, err := h.controller.GetEngine(workflowID)
	if err != nil {
		return h.error(err, scope, domainID, workflowID, runID)
	}
	// deserialize history event object
	historyEvents, err := h.GetPayloadSerializer().DeserializeBatchEvents(&persistence.DataBlob{
		Encoding: common.EncodingTypeThriftRW,
		Data:     request.GetRequest().GetEvents().GetData(),
	})
	if err != nil {
		return h.error(err, scope, domainID, workflowID, runID)
	}

	execution := request.GetRequest().GetWorkflowExecution()
	if err := engine.ReapplyEvents(
		ctx,
		request.GetDomainUUID(),
		execution.GetWorkflowID(),
		execution.GetRunID(),
		historyEvents,
	); err != nil {
		return h.error(err, scope, domainID, workflowID, runID)
	}
	return nil
}

func (h *handlerImpl) CountDLQMessages(
	ctx context.Context,
	request *types.CountDLQMessagesRequest,
) (resp *types.HistoryCountDLQMessagesResponse, retError error) {
	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryCountDLQMessagesScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	g := &errgroup.Group{}
	var mu sync.Mutex
	entries := map[types.HistoryDLQCountKey]int64{}
	for _, shardID := range h.controller.ShardIDs() {
		shardID := shardID
		g.Go(func() (e error) {
			defer func() { log.CapturePanic(recover(), h.GetLogger(), &e) }()

			engine, err := h.controller.GetEngineForShard(int(shardID))
			if err != nil {
				return fmt.Errorf("dlq count for shard %d: %w", shardID, err)
			}

			counts, err := engine.CountDLQMessages(ctx, request.ForceFetch)
			if err != nil {
				return fmt.Errorf("dlq count for shard %d: %w", shardID, err)
			}

			mu.Lock()
			defer mu.Unlock()
			for sourceCluster, count := range counts {
				key := types.HistoryDLQCountKey{ShardID: shardID, SourceCluster: sourceCluster}
				entries[key] = count
			}
			return nil
		})
	}

	err := g.Wait()
	return &types.HistoryCountDLQMessagesResponse{Entries: entries}, h.error(err, scope, "", "", "")
}

// ReadDLQMessages reads replication DLQ messages
func (h *handlerImpl) ReadDLQMessages(
	ctx context.Context,
	request *types.ReadDLQMessagesRequest,
) (resp *types.ReadDLQMessagesResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryReadDLQMessagesScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	engine, err := h.controller.GetEngineForShard(int(request.GetShardID()))
	if err != nil {
		return nil, h.error(err, scope, "", "", "")
	}

	return engine.ReadDLQMessages(ctx, request)
}

// PurgeDLQMessages deletes replication DLQ messages
func (h *handlerImpl) PurgeDLQMessages(
	ctx context.Context,
	request *types.PurgeDLQMessagesRequest,
) (retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryPurgeDLQMessagesScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return errShuttingDown
	}

	engine, err := h.controller.GetEngineForShard(int(request.GetShardID()))
	if err != nil {
		return h.error(err, scope, "", "", "")
	}

	return engine.PurgeDLQMessages(ctx, request)
}

// MergeDLQMessages reads and applies replication DLQ messages
func (h *handlerImpl) MergeDLQMessages(
	ctx context.Context,
	request *types.MergeDLQMessagesRequest,
) (resp *types.MergeDLQMessagesResponse, retError error) {

	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryMergeDLQMessagesScope)
	defer sw.Stop()

	engine, err := h.controller.GetEngineForShard(int(request.GetShardID()))
	if err != nil {
		return nil, h.error(err, scope, "", "", "")
	}

	return engine.MergeDLQMessages(ctx, request)
}

// RefreshWorkflowTasks refreshes all the tasks of a workflow
func (h *handlerImpl) RefreshWorkflowTasks(
	ctx context.Context,
	request *types.HistoryRefreshWorkflowTasksRequest) (retError error) {

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRefreshWorkflowTasksScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return errShuttingDown
	}

	domainID := request.DomainUIID
	execution := request.GetRequest().GetExecution()
	workflowID := execution.GetWorkflowID()
	runID := execution.GetWorkflowID()
	engine, err := h.controller.GetEngine(workflowID)
	if err != nil {
		return h.error(err, scope, domainID, workflowID, runID)
	}

	err = engine.RefreshWorkflowTasks(
		ctx,
		domainID,
		types.WorkflowExecution{
			WorkflowID: execution.WorkflowID,
			RunID:      execution.RunID,
		},
	)

	if err != nil {
		return h.error(err, scope, domainID, workflowID, runID)
	}

	return nil
}

// NotifyFailoverMarkers sends the failover markers to failover coordinator.
// The coordinator decides when the failover finishes based on received failover marker.
func (h *handlerImpl) NotifyFailoverMarkers(
	ctx context.Context,
	request *types.NotifyFailoverMarkersRequest,
) (retError error) {

	_, sw := h.startRequestProfile(ctx, metrics.HistoryNotifyFailoverMarkersScope)
	defer sw.Stop()

	for _, token := range request.GetFailoverMarkerTokens() {
		marker := token.GetFailoverMarker()
		h.GetLogger().Debug("Handling failover maker", tag.WorkflowDomainID(marker.GetDomainID()))
		h.failoverCoordinator.ReceiveFailoverMarkers(token.GetShardIDs(), token.GetFailoverMarker())
	}
	return nil
}

func (h *handlerImpl) GetCrossClusterTasks(
	ctx context.Context,
	request *types.GetCrossClusterTasksRequest,
) (resp *types.GetCrossClusterTasksResponse, retError error) {
	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	_, sw := h.startRequestProfile(ctx, metrics.HistoryGetCrossClusterTasksScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	ctx, cancel := common.CreateChildContext(ctx, 0.05)
	defer cancel()

	futureByShardID := make(map[int32]future.Future, len(request.ShardIDs))
	for _, shardID := range request.ShardIDs {
		future, settable := future.NewFuture()
		futureByShardID[shardID] = future
		go func(shardID int32) {
			logger := h.GetLogger().WithTags(tag.ShardID(int(shardID)))
			engine, err := h.controller.GetEngineForShard(int(shardID))
			if err != nil {
				logger.Error("History engine not found for shard", tag.Error(err))
				var owner membership.HostInfo
				if info, err := h.GetMembershipResolver().Lookup(service.History, strconv.Itoa(int(shardID))); err == nil {
					owner = info
				}
				settable.Set(nil, shard.CreateShardOwnershipLostError(h.GetHostInfo(), owner))
				return
			}

			if tasks, err := engine.GetCrossClusterTasks(ctx, request.TargetCluster); err != nil {
				logger.Error("Failed to get cross cluster tasks", tag.Error(err))
				settable.Set(nil, h.convertError(err))
			} else {
				settable.Set(tasks, nil)
			}
		}(shardID)
	}

	response := &types.GetCrossClusterTasksResponse{
		TasksByShard:       make(map[int32][]*types.CrossClusterTaskRequest),
		FailedCauseByShard: make(map[int32]types.GetTaskFailedCause),
	}
	for shardID, future := range futureByShardID {
		var taskRequests []*types.CrossClusterTaskRequest
		if futureErr := future.Get(ctx, &taskRequests); futureErr != nil {
			response.FailedCauseByShard[shardID] = common.ConvertErrToGetTaskFailedCause(futureErr)
		} else {
			response.TasksByShard[shardID] = taskRequests
		}
	}
	// not using a waitGroup for created goroutines here
	// as once all futures are unblocked,
	// those goroutines will eventually be completed

	return response, nil
}

func (h *handlerImpl) RespondCrossClusterTasksCompleted(
	ctx context.Context,
	request *types.RespondCrossClusterTasksCompletedRequest,
) (resp *types.RespondCrossClusterTasksCompletedResponse, retError error) {
	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRespondCrossClusterTasksCompletedScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	engine, err := h.controller.GetEngineForShard(int(request.GetShardID()))
	if err != nil {
		return nil, h.error(err, scope, "", "", "")
	}

	err = engine.RespondCrossClusterTasksCompleted(ctx, request.TargetCluster, request.TaskResponses)
	if err != nil {
		return nil, h.error(err, scope, "", "", "")
	}

	response := &types.RespondCrossClusterTasksCompletedResponse{}
	if request.FetchNewTasks {
		fetchTaskCtx, cancel := common.CreateChildContext(ctx, 0.05)
		defer cancel()

		response.Tasks, err = engine.GetCrossClusterTasks(fetchTaskCtx, request.TargetCluster)
		if err != nil {
			return nil, h.error(err, scope, "", "", "")
		}
	}
	return response, nil
}

func (h *handlerImpl) GetFailoverInfo(
	ctx context.Context,
	request *types.GetFailoverInfoRequest,
) (resp *types.GetFailoverInfoResponse, retError error) {
	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryGetFailoverInfoScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	resp, err := h.failoverCoordinator.GetFailoverInfo(request.GetDomainID())
	if err != nil {
		return nil, h.error(err, scope, request.GetDomainID(), "", "")
	}
	return resp, nil
}

func (h *handlerImpl) RatelimitStartup(ctx context.Context, request *types.RatelimitStartupRequest) (resp *types.RatelimitStartupResponse, retError error) {
	// bleh.  this stuff should really be in a generated wrapper, it's highly regular
	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRatelimitStartupScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	all := h.ratelimitAgg.GetAll(request.Caller)
	data, err := rpc.AllowResponseToAny(all)
	if err != nil {
		return nil, h.error(err, scope, "", "", "")
	}
	resp = &types.RatelimitStartupResponse{
		Data: data,
	}
	return resp, nil
}

func (h *handlerImpl) RatelimitUpdate(ctx context.Context, request *types.RatelimitUpdateRequest) (resp *types.RatelimitUpdateResponse, retError error) {
	defer func() { log.CapturePanic(recover(), h.GetLogger(), &retError) }()
	h.startWG.Wait()

	scope, sw := h.startRequestProfile(ctx, metrics.HistoryRatelimitUpdateScope)
	defer sw.Stop()

	if h.isShuttingDown() {
		return nil, errShuttingDown
	}

	// perform the update
	// TODO: push (de)serialization into the agg-API, it should be implementation-decided
	updateData, err := rpc.AnyToUpdateRequest(request.Data)
	if err != nil {
		return nil, h.error(err, scope, "", "", "")
	}

	h.ratelimitAgg.Update(request.Caller, request.LastUpdated, updateData)

	// retrieve all (now updated) rps data
	all := h.ratelimitAgg.GetAll(request.Caller)
	respondData, err := rpc.AllowResponseToAny(all)
	if err != nil {
		return nil, h.error(err, scope, "", "", "")
	}
	resp = &types.RatelimitUpdateResponse{
		Data: respondData,
	}
	return resp, nil
}

// convertError is a helper method to convert ShardOwnershipLostError from persistence layer returned by various
// HistoryEngine API calls to ShardOwnershipLost error return by HistoryService for client to be redirected to the
// correct shard.
func (h *handlerImpl) convertError(err error) error {
	switch err := err.(type) {
	case *persistence.ShardOwnershipLostError:
		info, err2 := h.GetMembershipResolver().Lookup(service.History, strconv.Itoa(err.ShardID))
		if err2 != nil {
			return shard.CreateShardOwnershipLostError(h.GetHostInfo(), membership.HostInfo{})
		}

		return shard.CreateShardOwnershipLostError(h.GetHostInfo(), info)
	case *persistence.WorkflowExecutionAlreadyStartedError:
		return &types.InternalServiceError{Message: err.Msg}
	case *persistence.CurrentWorkflowConditionFailedError:
		return &types.InternalServiceError{Message: err.Msg}
	case *persistence.TransactionSizeLimitError:
		return &types.BadRequestError{Message: err.Msg}
	}

	return err
}

func (h *handlerImpl) updateErrorMetric(
	scope metrics.Scope,
	domainID string,
	workflowID string,
	runID string,
	err error,
) {

	var yarpcE *yarpcerrors.Status

	var shardOwnershipLostError *types.ShardOwnershipLostError
	var eventAlreadyStartedError *types.EventAlreadyStartedError
	var badRequestError *types.BadRequestError
	var domainNotActiveError *types.DomainNotActiveError
	var workflowExecutionAlreadyStartedError *types.WorkflowExecutionAlreadyStartedError
	var entityNotExistsError *types.EntityNotExistsError
	var workflowExecutionAlreadyCompletedError *types.WorkflowExecutionAlreadyCompletedError
	var cancellationAlreadyRequestedError *types.CancellationAlreadyRequestedError
	var limitExceededError *types.LimitExceededError
	var retryTaskV2Error *types.RetryTaskV2Error
	var serviceBusyError *types.ServiceBusyError
	var internalServiceError *types.InternalServiceError

	if err == context.DeadlineExceeded || err == context.Canceled {
		scope.IncCounter(metrics.CadenceErrContextTimeoutCounter)
		return
	}

	if errors.As(err, &shardOwnershipLostError) {
		scope.IncCounter(metrics.CadenceErrShardOwnershipLostCounter)

	} else if errors.As(err, &eventAlreadyStartedError) {
		scope.IncCounter(metrics.CadenceErrEventAlreadyStartedCounter)

	} else if errors.As(err, &badRequestError) {
		scope.IncCounter(metrics.CadenceErrBadRequestCounter)

	} else if errors.As(err, &domainNotActiveError) {
		scope.IncCounter(metrics.CadenceErrDomainNotActiveCounter)

	} else if errors.As(err, &workflowExecutionAlreadyStartedError) {
		scope.IncCounter(metrics.CadenceErrExecutionAlreadyStartedCounter)

	} else if errors.As(err, &entityNotExistsError) {
		scope.IncCounter(metrics.CadenceErrEntityNotExistsCounter)

	} else if errors.As(err, &workflowExecutionAlreadyCompletedError) {
		scope.IncCounter(metrics.CadenceErrWorkflowExecutionAlreadyCompletedCounter)

	} else if errors.As(err, &cancellationAlreadyRequestedError) {
		scope.IncCounter(metrics.CadenceErrCancellationAlreadyRequestedCounter)

	} else if errors.As(err, &limitExceededError) {
		scope.IncCounter(metrics.CadenceErrLimitExceededCounter)

	} else if errors.As(err, &retryTaskV2Error) {
		scope.IncCounter(metrics.CadenceErrRetryTaskCounter)

	} else if errors.As(err, &serviceBusyError) {
		scope.IncCounter(metrics.CadenceErrServiceBusyCounter)

	} else if errors.As(err, &yarpcE) {

		if yarpcE.Code() == yarpcerrors.CodeDeadlineExceeded {
			scope.IncCounter(metrics.CadenceErrContextTimeoutCounter)
		}
		scope.IncCounter(metrics.CadenceFailures)

	} else if errors.As(err, &internalServiceError) {
		scope.IncCounter(metrics.CadenceFailures)
		h.GetLogger().Error("Internal service error",
			tag.Error(err),
			tag.WorkflowID(workflowID),
			tag.WorkflowRunID(runID),
			tag.WorkflowDomainID(domainID))

	} else {
		// Default / unknown error fallback
		scope.IncCounter(metrics.CadenceFailures)
		h.GetLogger().Error("Uncategorized error",
			tag.Error(err),
			tag.WorkflowID(workflowID),
			tag.WorkflowRunID(runID),
			tag.WorkflowDomainID(domainID))
	}
}

func (h *handlerImpl) error(
	err error,
	scope metrics.Scope,
	domainID string,
	workflowID string,
	runID string,
) error {
	err = h.convertError(err)

	h.updateErrorMetric(scope, domainID, workflowID, runID, err)
	return err
}

func (h *handlerImpl) emitInfoOrDebugLog(
	domainID string,
	msg string,
	tags ...tag.Tag,
) {
	if h.config.EnableDebugMode && h.config.EnableTaskInfoLogByDomainID(domainID) {
		h.GetLogger().Info(msg, tags...)
	} else {
		h.GetLogger().Debug(msg, tags...)
	}
}

func (h *handlerImpl) startRequestProfile(ctx context.Context, scope int) (metrics.Scope, metrics.Stopwatch) {
	metricsScope := h.GetMetricsClient().Scope(scope, metrics.GetContextTags(ctx)...)
	metricsScope.IncCounter(metrics.CadenceRequests)
	sw := metricsScope.StartTimer(metrics.CadenceLatency)
	return metricsScope, sw
}

func validateTaskToken(token *common.TaskToken) error {
	if token.WorkflowID == "" {
		return errWorkflowIDNotSet
	}
	if token.RunID != "" && uuid.Parse(token.RunID) == nil {
		return errRunIDNotValid
	}
	return nil
}
