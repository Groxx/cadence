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
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
)

// TypedMap adds type safety around a sync.Map, and:
//   - implicitly constructs values as needed, not relying on zero values
//   - simplifies the API a bit because not all methods are in use
//   - tracks length
//
// Value must be an interface, pointer, or map type, as otherwise there is no
// real purpose to using this sync.Map-based tool.
type TypedMap[Key comparable, Value any] struct {
	contents sync.Map
	create   func(key Key) Value
	len      int64
}

func NewZero[Key comparable, Value any]() (*TypedMap[Key, Value], error) {
	return New(func(key Key) Value {
		var zero Value
		return zero
	})
}

// New makes a new typed sync.Map that creates values as needed.
//
// Value must be an interface, pointer, or map type, as otherwise there is no
// real purpose to using this sync.Map-based tool.
//
// create will be called when creating a new value, possibly multiple times.
// It should return ASAP to reduce the window for storage races.
func New[Key comparable, Value any](create func(key Key) Value) (*TypedMap[Key, Value], error) {
	var zero Value
	vtype := reflect.TypeOf(zero)
	isInterface := vtype == nil                                 // nil interfaces do this, and zero is nil
	isPointer := isInterface || vtype.Kind() == reflect.Pointer // vtype.Kind panics if nil
	isMap := isInterface || vtype.Kind() == reflect.Map
	if !(isInterface || isPointer || isMap) {
		// using a non-pointer-type would mean copying on every load, making this object pointless.
		// unfortunately this cannot currently be expressed with type constraints, as there is no
		// way to describe e.g. "a struct with any keys" or "a map of any type".
		return nil, fmt.Errorf("must use an interface, pointer, or map type with TypedMap: %T", zero)
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
	created := t.create(key)
	val, loaded = t.contents.LoadOrStore(key, created)
	if !loaded {
		// stored a new value
		atomic.AddInt64(&t.len, 1)
	}
	return val.(Value)
}

// Try will load the current Value for a key, or return false if it did not exist.
// This will NOT initialize the key if it does not exist, it just directly calls
// the underlying Load on the sync.Map
func (t *TypedMap[Key, Value]) Try(key Key) (Value, bool) {
	v, ok := t.contents.Load(key)
	return v.(Value), ok
}

// Delete calls LoadAndDelete on the underlying sync.Map, and has the same semantics.
// This can be called concurrently with Range, and it updates length.
func (t *TypedMap[Key, Value]) Delete(k Key) {
	_, loaded := t.contents.LoadAndDelete(k)
	if loaded {
		atomic.AddInt64(&t.len, -1)
	}
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
