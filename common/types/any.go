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

package types

// Any is a generic-serialized-data container, analogous to google.protobuf.Any
// but intentionally not implementing its API.
type Any struct {
	// TypeID string, structure unspecified but URLs are a good starting point.
	//
	// Deserializers must know what types they support and how to interpret them.
	// Generally speaking: type strings MUST be stable identifiers of types, and
	// not be impacted by renaming / rearranging.
	//
	// This means that:
	//  - hardcoded strings mapped to types in code are the safest, and are
	//    STRONGLY preferred.
	//  - RPC type names (e.g. protobuf urls) SHOULD be safe, but also they
	//    SHOULD explicitly state if they are used in this way, as
	//    otherwise-safe renames cannot be done.
	//  - reflect.Type.Name and reflect.Type.PkgPath or similar MUST NOT be used,
	//    as they are extremely easy to modify accidentally and unsafely.
	TypeID string `json:"type_id,omitempty"`

	// Value (bytes) for the specified TypeID-specified type
	Value []byte `json:"value,omitempty"`
}
