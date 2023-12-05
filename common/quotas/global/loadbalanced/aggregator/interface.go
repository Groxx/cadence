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
	"context"
	"time"

	"github.com/uber/cadence/common/quotas/global/loadbalanced/rpc"
)

// mockgen is archived and does not understand generics in -source mode, so this needs to be in its own file.
// TODO: we should really move off mockgen

//go:generate mockgen -package $GOPACKAGE -source $GOFILE -destination aggregator_mock.go -self_package github.com/uber/cadence/common/quotas/global/loadbalanced/aggregator

type (
	Aggregator interface {
		Start()
		Stop(ctx context.Context) error

		Update(host string, elapsed time.Duration, load rpc.AnyUpdateRequest)
		Get(host string, keys []string) rpc.AnyAllowResponse
		GetAll(host string) rpc.AnyAllowResponse
	}
)
