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

package typedmap

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"golang.org/x/sync/errgroup"
)

func TestNew(t *testing.T) {
	t.Run("sane", func(t *testing.T) {
		t.Run("pointer", func(t *testing.T) {
			m, err := New(func(key string) *string {
				return nil
			})
			assert.NoError(t, err)
			assert.NotNil(t, m)
		})
		t.Run("interface", func(t *testing.T) {
			m, err := New(func(key string) error {
				return nil
			})
			assert.NoError(t, err)
			assert.NotNil(t, m)
		})
	})
	t.Run("values not allowed", func(t *testing.T) {
		_, err := New(func(key string) string {
			return ""
		})
		assert.Error(t, err, "New should fail if given a value")
	})
}

func TestNotRacy(t *testing.T) {
	count := atomic.NewInt64(0)
	m, err := New(func(key string) *string {
		s := key
		s += "-"
		s += strconv.Itoa(int(count.Inc())) // just to be recognizable
		return &s
	})
	require.NoError(t, err)

	var g errgroup.Group
	const loops = 100
	for i := 0; i < loops; i++ {
		i := i
		g.Go(func() error {
			v := m.Load(strconv.Itoa(i))
			assert.NotEmpty(t, *v) // nils also asserted by crashing
			return nil
		})
		// try to load the same key multiple times
		g.Go(func() error {
			v := m.Load(strconv.Itoa(i))
			assert.NotEmpty(t, *v)
			return nil
		})
		// range over it while reading/writing
		g.Go(func() error {
			m.Range(func(k string, v *string) bool {
				assert.NotEmpty(t, k)
				assert.NotEmpty(t, *v)
				return true
			})
			return nil
		})
	}
	require.NoError(t, g.Wait())

	// sanity-check to show decent concurrency:
	// - out-of-order inits (count is higher and lower than key)
	// - duplicate inits (counts higher than 100)
	same, higher, lower, max := 0, 0, 0, int64(0)
	m.Range(func(k string, v *string) bool {
		parts := strings.SplitN(*v, "-", 2)

		// sanity check that keys and values stay associated
		assert.Equal(t, k, parts[0], "key %q and first part of value must match: %q", k, *v)

		if parts[0] == parts[1] {
			same++
		} else if parts[0] < parts[1] {
			higher++
		} else {
			lower++
		}

		vint, err := strconv.ParseInt(parts[1], 10, 64)
		assert.NoError(t, err, "count-%v should be parse-able as an int", parts[1])
		if vint > max { // hacky numeric sort
			max = vint
		}
		return true
	})

	assert.True(t, loops <= max, "loops %v <= max %v", loops, max)
	assert.True(t, max <= count.Load(), "max %v <= count %v", max, count.Load())
	t.Logf(
		"Metrics:\n"+
			"\tSame (1=>1-1):        %v\n"+
			"\tHigher (5=>5-100):    %v\n"+
			"\tLower (100=>100-5):   %v\n"+
			"\tNumber of iterations: %v\n"+
			"\tHighest saved create: %v\n"+ // same or higher than iterations
			"\tTotal num of creates: %v", // same or higher than creates
		same, higher, lower, loops, max, count.Load(),
	)
}
