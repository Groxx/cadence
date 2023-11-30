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

package errors

import "errors"

type (
	rpcError             struct{ error }
	deserializationError struct{ error }
)

func (r *rpcError) Unwrap() error             { return r.error }
func (d *deserializationError) Unwrap() error { return d.error }

// IsRPCError returns true if an error came from an RPC request, not
// something in this package.  These are expected to some degree.
func IsRPCError(err error) bool {
	var target *rpcError
	return errors.As(err, &target)
}

// IsDeserializationError returns true if an error was due to deserialization
// issues, which should imply some kind of significant logical flaw.
func IsDeserializationError(err error) bool {
	var target *deserializationError
	return errors.As(err, &target)
}

func ErrFromRPC(err error) error {
	return &rpcError{
		error: err,
	}
}
func ErrFromDeserialization(err error) error {
	return &deserializationError{
		error: err,
	}
}
