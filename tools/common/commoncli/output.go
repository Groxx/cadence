package commoncli

import (
	"fmt"
	"io"
	"sync"
)

// Output contains any convenient APIs for a "collect io.Writer errors to simplify error handling, and return them at the end".
// This is concurrency-safe and intended to be shared: a mutex is held around each attempt to write, so writes will not overlap.
//
// This exists because the bare io.Writer interface is rather test-and-coverage unfriendly, with many possible error branches.
// Our usage pretty much always means "you can ignore errors for a burst of writing, and then return it at the end if any occurred".
//
// For test purposes, see MockOutput and its assertion-helpers.
type Output interface {
	// Printf is largely the same as [fmt.Fprintf] on the wrapped [io.Writer],
	// but it collects errors internally (and noops if any occur) to simplify error handling.
	Printf(format string, a ...any)
	// Println is largely the same as [fmt.Fprintln] on the wrapped [io.Writer],
	// but it collects errors internally (and noops if any occur) to simplify error handling.
	Println(a ...any)
	// Print is largely the same as [fmt.Fprint] on the wrapped [io.Writer],
	// but it collects errors internally (and noops if any occur) to simplify error handling.
	Print(a ...any)

	// Error returns any writing error that occurred, or nil.
	//
	// This is intended for use as a return value of any code that wants to produce output, when there is no other relevant error, e.g.:
	// 	func render(data, output) error {
	// 		output.Println(data)
	// 		return output.Error()
	// 	}
	//
	// Or any place where it may be useful to stop outputting if there are write-errors, such as in large loops:
	// 	func render(lotsOfData, output) error {
	// 		for _, data := range lotsOfData {
	// 			data.Preprocess() // lengthy computation
	//			output.Println(data.String())
	// 			if err := output.Error(); err != nil {
	// 				return err // early return to save time
	// 			}
	// 		}
	// 		return output.Error()
	// 	}
	//
	// In either case, once Error() would return a non-nil error, no further output will be written
	// and calls will be treated as a noop.
	Error() error
	// MaybeError will return the passed error if non-nil, or any writing error that occurred, or nil if neither apply.
	//
	// This is intended for use as a return value of any code that wants to produce output, when there may be another more-interesting error.
	// E.g.:
	// 	func render(data, output) error {
	// 		err := stuff(data, output)
	// 		return output.MaybeError(err)
	// 	}
	MaybeError(err error) error
}

// WriteTo produces the simplest Output, for writing an arbitrary [io.Writer].
//
// For tests, using NewMockOutput is likely simpler than using a custom [io.Writer].
func WriteTo(w io.Writer) Output {
	return &writeErrCoalescer{
		writer: w,
		err:    nil,
	}
}

var _ Output = (*writeErrCoalescer)(nil)

type writeErrCoalescer struct {
	mut    sync.Mutex
	writer io.Writer
	err    error
}

func (w *writeErrCoalescer) Printf(format string, a ...any) {
	w.mut.Lock()
	defer w.mut.Unlock()
	if w.err != nil {
		return
	}
	_, w.err = fmt.Fprintf(w.writer, format, a...)
}

func (w *writeErrCoalescer) Println(a ...any) {
	w.mut.Lock()
	defer w.mut.Unlock()
	if w.err != nil {
		return
	}
	_, w.err = fmt.Fprintln(w.writer, a...)
}
func (w *writeErrCoalescer) Print(a ...any) {
	w.mut.Lock()
	defer w.mut.Unlock()
	if w.err != nil {
		return
	}
	_, w.err = fmt.Fprint(w.writer, a...)
}

func (w *writeErrCoalescer) Error() error {
	w.mut.Lock()
	defer w.mut.Unlock()
	return w.err
}

func (w *writeErrCoalescer) MaybeError(err error) error {
	if err != nil {
		return err // non-printing errors are assumed to be much more valuable
	}
	w.mut.Lock()
	defer w.mut.Unlock()
	return w.err
}
