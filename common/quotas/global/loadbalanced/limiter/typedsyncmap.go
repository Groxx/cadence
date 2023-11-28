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

package limiter

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
)

// TypedMap adds type safety around a sync.Map, and:
//   - auto-inits values to the type's zero value, as a pointer
//   - simplifies the API a bit because not all methods are in use
//   - tracks length
type TypedMap[Key comparable, Value any] struct {
	contents sync.Map
	create   func(key Key) Value
	len      int64 // atomic
}

// NewTypedMap makes a new typed sync.Map that creates values as needed.
//
// Value must be a pointer or interface type, as otherwise there is no real purpose to using sync.Map.
//
// create will be called when creating a new value, possibly multiple times.  It should be "immediate" to reduce storage races.
func NewTypedMap[Key comparable, Value any](create func(key Key) Value) (*TypedMap[Key, Value], error) {
	var zero Value
	kind := reflect.TypeOf(zero).Kind()
	if kind != reflect.Pointer && kind != reflect.Interface {
		return nil, fmt.Errorf("must use an interface or pointer type with TypedMap: %T", zero)
	}
	return &TypedMap[Key, Value]{
		contents: sync.Map{},
		create:   create,
		len:      0,
	}, nil
}

// Load will get the current Value for a Key, initializing it if necessary.
func (t *TypedMap[Key, Value]) Load(key Key) Value {
	val, loaded := t.contents.Load(key)
	if loaded {
		return val.(Value)
	}
	val, loaded = t.contents.LoadOrStore(key, t.create(key))
	if !loaded {
		// stored a new value
		atomic.AddInt64(&t.len, 1)
	}
	return val.(Value)
}

// Range calls Range on the underlying sync.Map, and has the same semantics.
func (t *TypedMap[Key, Value]) Range(f func(k Key, v Value) bool) {
	t.contents.Range(func(k, v any) bool {
		return f(k.(Key), v.(Value))
	})
}

// Len is an atomic count of the size of the collection.
// Range may iterate over more or fewer entries.
func (t *TypedMap[Key, Value]) Len() int {
	return int(atomic.LoadInt64(&t.len))
}
