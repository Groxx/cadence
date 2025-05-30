// The MIT License (MIT)
//
// Copyright (c) 2017-2020 Uber Technologies Inc.
//
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

package failover

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pborman/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/mock/gomock"
	"go.uber.org/yarpc"

	"github.com/uber/cadence/client/history"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/dynamicconfig/dynamicproperties"
	"github.com/uber/cadence/common/metrics"
	mmocks "github.com/uber/cadence/common/mocks"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/resource"
	"github.com/uber/cadence/common/types"
	"github.com/uber/cadence/service/history/config"
)

type (
	coordinatorSuite struct {
		suite.Suite
		*require.Assertions

		controller          *gomock.Controller
		mockResource        *resource.Test
		mockMetadataManager *mmocks.MetadataManager
		historyClient       *history.MockClient
		config              *config.Config
		coordinator         *coordinatorImpl
	}
)

func TestCoordinatorSuite(t *testing.T) {
	s := new(coordinatorSuite)
	suite.Run(t, s)
}

func (s *coordinatorSuite) SetupTest() {
	s.Assertions = require.New(s.T())
	s.controller = gomock.NewController(s.T())
	s.mockResource = resource.NewTest(s.T(), s.controller, metrics.History)
	s.mockMetadataManager = s.mockResource.MetadataMgr
	s.historyClient = s.mockResource.HistoryClient
	s.config = config.NewForTest()
	s.config.NumberOfShards = 2
	s.config.NotifyFailoverMarkerInterval = dynamicproperties.GetDurationPropertyFn(10 * time.Millisecond)
	s.config.NotifyFailoverMarkerTimerJitterCoefficient = dynamicproperties.GetFloatPropertyFn(0.01)
	s.mockResource.DomainCache.EXPECT().GetDomainName(gomock.Any()).Return("test", nil).AnyTimes()

	s.coordinator = NewCoordinator(
		s.mockMetadataManager,
		s.historyClient,
		s.mockResource.GetTimeSource(),
		s.mockResource.GetDomainCache(),
		s.config,
		s.mockResource.GetMetricsClient(),
		s.mockResource.GetLogger(),
	).(*coordinatorImpl)
}

func (s *coordinatorSuite) TearDownTest() {
	s.controller.Finish()
	s.mockResource.Finish(s.T())
	s.coordinator.Stop()
	s.mockMetadataManager.AssertExpectations(s.T())
}

func (s *coordinatorSuite) TestNotifyFailoverMarkers() {
	doneCh := make(chan struct{})
	attributes := &types.FailoverMarkerAttributes{
		DomainID:        uuid.New(),
		FailoverVersion: 1,
		CreationTime:    common.Int64Ptr(1),
	}
	s.historyClient.EXPECT().NotifyFailoverMarkers(
		context.Background(), &types.NotifyFailoverMarkersRequest{
			FailoverMarkerTokens: []*types.FailoverMarkerToken{
				{
					ShardIDs:       []int32{1, 2},
					FailoverMarker: attributes,
				},
			},
		},
	).DoAndReturn(func(ctx context.Context, request *types.NotifyFailoverMarkersRequest, opts ...yarpc.CallOption) error {
		close(doneCh)
		return nil
	}).Times(1)

	s.coordinator.NotifyFailoverMarkers(
		1,
		[]*types.FailoverMarkerAttributes{attributes},
	)
	s.coordinator.NotifyFailoverMarkers(
		2,
		[]*types.FailoverMarkerAttributes{attributes},
	)
	s.coordinator.Start()
	<-doneCh
}

func (s *coordinatorSuite) TestNotifyRemoteCoordinator_Empty() {
	requestByMarker := make(map[types.FailoverMarkerAttributes]*receiveRequest)
	s.historyClient.EXPECT().NotifyFailoverMarkers(context.Background(), gomock.Any()).Times(0)
	err := s.coordinator.notifyRemoteCoordinator(requestByMarker)
	s.NoError(err)
}

func (s *coordinatorSuite) TestNotifyRemoteCoordinator() {

	requestByMarker := make(map[types.FailoverMarkerAttributes]*receiveRequest)
	attributes := types.FailoverMarkerAttributes{
		DomainID:        uuid.New(),
		FailoverVersion: 1,
		CreationTime:    common.Int64Ptr(1),
	}
	requestByMarker[attributes] = &receiveRequest{
		shardIDs: []int32{1, 2, 3},
		marker:   &attributes,
	}

	s.historyClient.EXPECT().NotifyFailoverMarkers(
		context.Background(), &types.NotifyFailoverMarkersRequest{
			FailoverMarkerTokens: []*types.FailoverMarkerToken{
				{
					ShardIDs:       []int32{1, 2, 3},
					FailoverMarker: &attributes,
				},
			},
		},
	).Return(nil).Times(1)
	err := s.coordinator.notifyRemoteCoordinator(requestByMarker)
	s.NoError(err)
	s.Equal(1, len(requestByMarker))
}

func (s *coordinatorSuite) TestNotifyRemoteCoordinator_Error() {

	requestByMarker := make(map[types.FailoverMarkerAttributes]*receiveRequest)
	attributes := types.FailoverMarkerAttributes{
		DomainID:        uuid.New(),
		FailoverVersion: 1,
		CreationTime:    common.Int64Ptr(1),
	}
	requestByMarker[attributes] = &receiveRequest{
		shardIDs: []int32{1, 2, 3},
		marker:   &attributes,
	}

	s.historyClient.EXPECT().NotifyFailoverMarkers(
		context.Background(), &types.NotifyFailoverMarkersRequest{
			FailoverMarkerTokens: []*types.FailoverMarkerToken{
				{
					ShardIDs:       []int32{1, 2, 3},
					FailoverMarker: &attributes,
				},
			},
		},
	).Return(fmt.Errorf("test")).Times(1)
	err := s.coordinator.notifyRemoteCoordinator(requestByMarker)
	s.Error(err)
}

func (s *coordinatorSuite) TestAggregateNotificationRequests() {
	requestByMarker := make(map[types.FailoverMarkerAttributes]*receiveRequest)
	attributes1 := types.FailoverMarkerAttributes{
		DomainID:        uuid.New(),
		FailoverVersion: 1,
		CreationTime:    common.Int64Ptr(1),
	}
	attributes2 := types.FailoverMarkerAttributes{
		DomainID:        uuid.New(),
		FailoverVersion: 2,
		CreationTime:    common.Int64Ptr(2),
	}
	request1 := &notificationRequest{
		shardID: 1,
		markers: []*types.FailoverMarkerAttributes{&attributes1},
	}
	aggregateNotificationRequests(request1, requestByMarker)
	request2 := &notificationRequest{
		shardID: 2,
		markers: []*types.FailoverMarkerAttributes{&attributes1},
	}
	aggregateNotificationRequests(request2, requestByMarker)
	request3 := &notificationRequest{
		shardID: 3,
		markers: []*types.FailoverMarkerAttributes{&attributes1, &attributes2},
	}
	aggregateNotificationRequests(request3, requestByMarker)
	attributes1.CreationTime = nil
	attributes2.CreationTime = nil
	s.Equal([]int32{1, 2, 3}, requestByMarker[attributes1].shardIDs)
	s.Equal([]int32{3}, requestByMarker[attributes2].shardIDs)
}

func (s *coordinatorSuite) TestHandleFailoverMarkers_DeleteExpiredFailoverMarker() {
	domainID := uuid.New()
	attributes1 := &types.FailoverMarkerAttributes{
		DomainID:        domainID,
		FailoverVersion: 1,
		CreationTime:    common.Int64Ptr(1),
	}
	attributes2 := &types.FailoverMarkerAttributes{
		DomainID:        domainID,
		FailoverVersion: 2,
		CreationTime:    common.Int64Ptr(1),
	}
	request1 := &receiveRequest{
		shardIDs: []int32{1},
		marker:   attributes1,
	}
	request2 := &receiveRequest{
		shardIDs: []int32{2},
		marker:   attributes2,
	}

	s.coordinator.handleFailoverMarkers(request1)
	s.coordinator.handleFailoverMarkers(request2)
	s.Equal(1, len(s.coordinator.recorder))
}

func (s *coordinatorSuite) TestHandleFailoverMarkers_IgnoreExpiredFailoverMarker() {
	domainID := uuid.New()
	attributes1 := &types.FailoverMarkerAttributes{
		DomainID:        domainID,
		FailoverVersion: 1,
		CreationTime:    common.Int64Ptr(1),
	}
	attributes2 := &types.FailoverMarkerAttributes{
		DomainID:        domainID,
		FailoverVersion: 2,
		CreationTime:    common.Int64Ptr(1),
	}
	request1 := &receiveRequest{
		shardIDs: []int32{1},
		marker:   attributes1,
	}
	request2 := &receiveRequest{
		shardIDs: []int32{2},
		marker:   attributes2,
	}

	s.coordinator.handleFailoverMarkers(request2)
	s.coordinator.handleFailoverMarkers(request1)
	s.Equal(1, len(s.coordinator.recorder))
}

func (s *coordinatorSuite) TestHandleFailoverMarkers_CleanPendingActiveState_Success() {
	domainID := uuid.New()
	attributes1 := &types.FailoverMarkerAttributes{
		DomainID:        domainID,
		FailoverVersion: 2,
		CreationTime:    common.Int64Ptr(1),
	}
	attributes2 := &types.FailoverMarkerAttributes{
		DomainID:        domainID,
		FailoverVersion: 2,
		CreationTime:    common.Int64Ptr(1),
	}
	request1 := &receiveRequest{
		shardIDs: []int32{1},
		marker:   attributes1,
	}
	request2 := &receiveRequest{
		shardIDs: []int32{2},
		marker:   attributes2,
	}
	info := &persistence.DomainInfo{
		ID:          domainID,
		Name:        uuid.New(),
		Status:      persistence.DomainStatusRegistered,
		Description: "some random description",
		OwnerEmail:  "some random email",
		Data:        nil,
	}
	domainConfig := &persistence.DomainConfig{
		Retention:  1,
		EmitMetric: true,
	}
	replicationConfig := &persistence.DomainReplicationConfig{
		ActiveClusterName: "active",
		Clusters: []*persistence.ClusterReplicationConfig{
			{
				"active",
			},
		},
	}

	s.mockMetadataManager.On("GetMetadata", mock.Anything).Return(&persistence.GetMetadataResponse{
		NotificationVersion: 1,
	}, nil)
	s.mockMetadataManager.On("GetDomain", mock.Anything, &persistence.GetDomainRequest{
		ID: domainID,
	}).Return(&persistence.GetDomainResponse{
		Info:                        info,
		Config:                      domainConfig,
		ReplicationConfig:           replicationConfig,
		IsGlobalDomain:              true,
		ConfigVersion:               1,
		FailoverVersion:             2,
		FailoverNotificationVersion: 2,
		FailoverEndTime:             common.Int64Ptr(1),
		NotificationVersion:         1,
	}, nil).Times(1)
	s.mockMetadataManager.On("UpdateDomain", mock.Anything, &persistence.UpdateDomainRequest{
		Info:                        info,
		Config:                      domainConfig,
		ReplicationConfig:           replicationConfig,
		ConfigVersion:               1,
		FailoverVersion:             2,
		FailoverNotificationVersion: 2,
		FailoverEndTime:             nil,
		NotificationVersion:         1,
	}).Return(nil).Times(1)

	s.coordinator.handleFailoverMarkers(request1)
	s.coordinator.handleFailoverMarkers(request2)
	s.Equal(0, len(s.coordinator.recorder))
}

func (s *coordinatorSuite) TestHandleFailoverMarkers_CleanPendingActiveState_Error() {
	domainID := uuid.New()
	attributes1 := &types.FailoverMarkerAttributes{
		DomainID:        domainID,
		FailoverVersion: 2,
		CreationTime:    common.Int64Ptr(1),
	}
	attributes2 := &types.FailoverMarkerAttributes{
		DomainID:        domainID,
		FailoverVersion: 2,
		CreationTime:    common.Int64Ptr(1),
	}
	request1 := &receiveRequest{
		shardIDs: []int32{1},
		marker:   attributes1,
	}
	request2 := &receiveRequest{
		shardIDs: []int32{2},
		marker:   attributes2,
	}
	info := &persistence.DomainInfo{
		ID:          domainID,
		Name:        uuid.New(),
		Status:      persistence.DomainStatusRegistered,
		Description: "some random description",
		OwnerEmail:  "some random email",
		Data:        nil,
	}
	domainConfig := &persistence.DomainConfig{
		Retention:  1,
		EmitMetric: true,
	}
	replicationConfig := &persistence.DomainReplicationConfig{
		ActiveClusterName: "active",
		Clusters: []*persistence.ClusterReplicationConfig{
			{
				"active",
			},
		},
	}

	s.mockMetadataManager.On("GetMetadata", mock.Anything).Return(&persistence.GetMetadataResponse{
		NotificationVersion: 1,
	}, nil)
	s.mockMetadataManager.On("GetDomain", mock.Anything, &persistence.GetDomainRequest{
		ID: domainID,
	}).Return(&persistence.GetDomainResponse{
		Info:                        info,
		Config:                      domainConfig,
		ReplicationConfig:           replicationConfig,
		IsGlobalDomain:              true,
		ConfigVersion:               1,
		FailoverVersion:             2,
		FailoverNotificationVersion: 2,
		FailoverEndTime:             common.Int64Ptr(1),
		NotificationVersion:         1,
	}, nil).Times(1)
	s.mockMetadataManager.On("UpdateDomain", mock.Anything, &persistence.UpdateDomainRequest{
		Info:                        info,
		Config:                      domainConfig,
		ReplicationConfig:           replicationConfig,
		ConfigVersion:               1,
		FailoverVersion:             2,
		FailoverNotificationVersion: 2,
		FailoverEndTime:             nil,
		NotificationVersion:         1,
	}).Return(fmt.Errorf("test error")).Times(3)

	s.coordinator.handleFailoverMarkers(request1)
	s.coordinator.handleFailoverMarkers(request2)
	s.Equal(1, len(s.coordinator.recorder))
}

func (s *coordinatorSuite) TestGetFailoverInfo_Success() {
	domainID := uuid.New()

	// Add failover marker
	attributes := &types.FailoverMarkerAttributes{
		DomainID:        domainID,
		FailoverVersion: 2,
		CreationTime:    common.Int64Ptr(1),
	}
	request := &receiveRequest{
		shardIDs: []int32{1},
		marker:   attributes,
	}
	s.coordinator.handleFailoverMarkers(request)

	resp, err := s.coordinator.GetFailoverInfo(domainID)
	s.NoError(err)
	s.Equal(int32(1), resp.GetCompletedShardCount())
	s.Contains(resp.GetPendingShards(), int32(0))
}

func (s *coordinatorSuite) TestGetFailoverInfo_DomainIDNotFound_Error() {
	domainID := uuid.New()
	resp, err := s.coordinator.GetFailoverInfo(domainID)
	s.Nil(resp)
	s.Error(err)
}
