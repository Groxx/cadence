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

// Code generated by MockGen. DO NOT EDIT.
// Source: interfaces.go

// Package quotas is a generated GoMock package.
package quotas

import (
	context "context"
	reflect "reflect"

	gomock "github.com/golang/mock/gomock"
	rate "golang.org/x/time/rate"
)

// MockLimiter is a mock of Limiter interface.
type MockLimiter struct {
	ctrl     *gomock.Controller
	recorder *MockLimiterMockRecorder
}

// MockLimiterMockRecorder is the mock recorder for MockLimiter.
type MockLimiterMockRecorder struct {
	mock *MockLimiter
}

// NewMockLimiter creates a new mock instance.
func NewMockLimiter(ctrl *gomock.Controller) *MockLimiter {
	mock := &MockLimiter{ctrl: ctrl}
	mock.recorder = &MockLimiterMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockLimiter) EXPECT() *MockLimiterMockRecorder {
	return m.recorder
}

// Allow mocks base method.
func (m *MockLimiter) Allow() bool {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Allow")
	ret0, _ := ret[0].(bool)
	return ret0
}

// Allow indicates an expected call of Allow.
func (mr *MockLimiterMockRecorder) Allow() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Allow", reflect.TypeOf((*MockLimiter)(nil).Allow))
}

// Reserve mocks base method.
func (m *MockLimiter) Reserve() *rate.Reservation {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Reserve")
	ret0, _ := ret[0].(*rate.Reservation)
	return ret0
}

// Reserve indicates an expected call of Reserve.
func (mr *MockLimiterMockRecorder) Reserve() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Reserve", reflect.TypeOf((*MockLimiter)(nil).Reserve))
}

// Wait mocks base method.
func (m *MockLimiter) Wait(ctx context.Context) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Wait", ctx)
	ret0, _ := ret[0].(error)
	return ret0
}

// Wait indicates an expected call of Wait.
func (mr *MockLimiterMockRecorder) Wait(ctx interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Wait", reflect.TypeOf((*MockLimiter)(nil).Wait), ctx)
}

// MockPolicy is a mock of Policy interface.
type MockPolicy struct {
	ctrl     *gomock.Controller
	recorder *MockPolicyMockRecorder
}

// MockPolicyMockRecorder is the mock recorder for MockPolicy.
type MockPolicyMockRecorder struct {
	mock *MockPolicy
}

// NewMockPolicy creates a new mock instance.
func NewMockPolicy(ctrl *gomock.Controller) *MockPolicy {
	mock := &MockPolicy{ctrl: ctrl}
	mock.recorder = &MockPolicyMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockPolicy) EXPECT() *MockPolicyMockRecorder {
	return m.recorder
}

// Allow mocks base method.
func (m *MockPolicy) Allow(info Info) bool {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Allow", info)
	ret0, _ := ret[0].(bool)
	return ret0
}

// Allow indicates an expected call of Allow.
func (mr *MockPolicyMockRecorder) Allow(info interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Allow", reflect.TypeOf((*MockPolicy)(nil).Allow), info)
}
