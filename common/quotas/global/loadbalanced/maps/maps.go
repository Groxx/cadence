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

// Package maps contains some multi-layer map-initializers because I got tired of doing it by hand.
package maps

// Put puts a value in a possibly-nil map.
func Put[
	K comparable,
	V any,
	M ~map[K]V,
](m M, k K, v V) M {
	if m == nil {
		m = make(M)
	}
	m[k] = v
	return m
}

// Put2 puts a value in a 3-layer-deep possibly-nil map.
func Put2[
	K1 comparable, K2 comparable,
	V any,
	M ~map[K1]Minner, Minner ~map[K2]V,
](m1 M, k1 K1, k2 K2, v V) M {
	inner := Put(m1[k1], k2, v)
	outer := Put(m1, k1, inner)
	return outer
}

// Put3 puts a value in a 3-layer-deep possibly-nil map.
// Generics are great, this is super easy and safe.
func Put3[
	K1 comparable, K2 comparable, K3 comparable,
	V any,
	M ~map[K1]Minner, Minner ~map[K2]Minner2, Minner2 ~map[K3]V,
](m1 M, k1 K1, k2 K2, k3 K3, v V) M {
	inner2 := Put(m1[k1][k2], k3, v)
	inner := Put(m1[k1], k2, inner2)
	outer := Put(m1, k1, inner)
	return outer
}
