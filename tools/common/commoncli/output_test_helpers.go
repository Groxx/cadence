package commoncli

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// MockOutput is intended to help simplify output-checking (and output-error-triggering) tests.
// Hopefully this is more useful than a generated mock object or manual [io.Writer] values.
type MockOutput struct {
	mut sync.Mutex
	t   *testing.T

	writes        []string                       // each write that occurred.  note that this is *writes*, not *lines* - one line may be multiple writes, or vise versa.
	errConditions []func(lastWrite string) error // called after every write to determine if an error "occurred", until it returns non-nil.  then that error becomes the returned Error()
	err           error                          // the error to return

	closed bool // when true, no further writes are expected, and ALL are test failures
}

func NewMockOutput(t *testing.T) *MockOutput {
	m := &MockOutput{
		t: t,
	}
	t.Cleanup(m.Close)
	return m
}

var _ Output = (*MockOutput)(nil)

// v--- Output interface impls ---v

func (m *MockOutput) Printf(format string, a ...any) {
	m.t.Helper()
	m.lockAndLogOrNoop(fmt.Sprintf(format, a...))
}
func (m *MockOutput) Println(a ...any) {
	m.t.Helper()
	m.lockAndLogOrNoop(fmt.Sprintln(a...))
}
func (m *MockOutput) Print(a ...any) {
	m.t.Helper()
	m.lockAndLogOrNoop(fmt.Sprint(a...))
}

func (m *MockOutput) Error() error {
	m.mut.Lock()
	defer m.mut.Unlock()
	return m.err
}

func (m *MockOutput) MaybeError(err error) error {
	if err != nil {
		return err
	}
	m.mut.Lock()
	defer m.mut.Unlock()
	return m.err
}

func (m *MockOutput) lockAndLogOrNoop(msg string) {
	m.t.Helper()
	m.mut.Lock()
	defer m.mut.Unlock()
	if m.closed {
		m.t.Errorf("output occurred after the test marked it as closed: %s", msg)
		return
	}
	if m.err == nil {
		m.t.Log(strings.TrimSuffix(msg, "\n")) // t.Log adds a newline, no trailing newline needed
		m.writes = append(m.writes, msg)
		for _, condition := range m.errConditions {
			if err := condition(msg); err != nil {
				m.err = err
				break
			}
		}
	}
}

// ^--- Output interface impls ---^
// v--- test helpers ---v

// Close marks this writer as "done", and will:
// - fail the test if any further prints occur, as this implies leaky goroutines
// - fail the test if all error conditions are unused, as this implies incorrect mocks/expectations (possibly just changed output strings)
//
// This can be called multiple times, though it is a noop after the first.
func (m *MockOutput) Close() {
	m.mut.Lock()
	defer m.mut.Unlock()

	if !m.closed {
		m.closed = true

		if len(m.errConditions) > 0 && m.err == nil {
			m.t.Errorf("%v error conditions are registered but none were triggered", len(m.errConditions))
		}
	}
}

// ErrorAfter adds a callback which will be checked after every write, to decide when to fake a write-error occurring.
//
// Only the first non-nil return from any condition will be used.
// If any error-conditions are set but none are used, the test will be failed.
func (m *MockOutput) ErrorAfter(condition func(lastWrite string) error) {
	m.mut.Lock()
	defer m.mut.Unlock()
	m.errConditions = append(m.errConditions, condition)
}

// String returns all writes joined together as a single string, unmodified.
func (m *MockOutput) String() string {
	m.mut.Lock()
	defer m.mut.Unlock()
	var buf strings.Builder
	for _, msg := range m.writes {
		buf.WriteString(msg)
	}
	return buf.String()
}

// Writes returns a copy of all writes that have occurred up to this point.
func (m *MockOutput) Writes() []string {
	m.mut.Lock()
	defer m.mut.Unlock()
	dup := make([]string, len(m.writes))
	copy(dup, m.writes)
	return dup
}

// Lines returns one string per line of output that has occurred up to this point, without trailing newlines.
// Any trailing not-yet-newline-terminated text will be the final entry, if present.
// Any blank lines are included, and whitespace (aside from \n) is untouched.
//
// This differs from Writes any time a line is built up from multiple print calls,
// or any time multiple lines are printed in one call.
// Because this is basically "what users see" and is a more implementation-agnostic
// model, it is probably what most tests should check against.
func (m *MockOutput) Lines() []string {
	m.mut.Lock()
	defer m.mut.Unlock()
	lines := make([]string, 0, len(m.writes)) // will often match num-writes, but it's better than zero regardless
	var pending string
	for _, msg := range m.writes {
		l := strings.Split(msg, "\n")
		if len(l) == 1 {
			// no newlines, so this is just appending to / starting the current in-progress line
			pending += l[0]
		} else {
			// Split never returns an empty list, so this implies 2+,
			// which means there was at least one newline in the text.

			// combine the first line with any pending line
			lines = append(lines, pending+l[0])
			pending = ""
			// add everything else, except the last one
			for idx, rest := range l[1:] {
				if idx == len(l)-2 {
					// last line.
					// if it's blank, there was a trailing newline, so ignore it.
					// otherwise this is now the new in-progress line of text
					// because it has not yet been ended.
					if rest != "" {
						pending = rest
					}
					break // and either way, do not append it yet
				}
				lines = append(lines, rest)
			}
		}
	}

	// include the straggler, if any
	if pending != "" {
		lines = append(lines, pending)
	}
	return lines
}

// ^--- test helpers ---^

// LineMatcher is an assertion-helper around lines of output, see its methods for details.
type LineMatcher struct {
	mut    sync.Mutex
	t      *testing.T
	lines  []string
	marked []bool
	closed bool
}

// NewLineMatcher makes a LineMatcher for the passed lines.
// All lines must be "marked" or the test will fail, to prevent accidental gaps.
func NewLineMatcher(t *testing.T, lines []string) *LineMatcher {
	dup := make([]string, len(lines))
	copy(dup, lines)
	l := &LineMatcher{
		t:      t,
		lines:  dup,
		marked: make([]bool, len(lines)),
	}
	t.Cleanup(func() {
		l.mut.Lock()
		defer l.mut.Unlock()
		l.closed = true
		for idx := range l.lines {
			if !l.marked[idx] {
				t.Error("un-marked line, failing test:", l.lines[idx])
			}
		}
	})
	return l
}

// Match calls the callback for each line being monitored, and includes whether it was previously marked or not.
// It returns two booleans, one for "any line matched" and one for "all lines matched", as convenience values.
// All line-inspecting methods eventually call this method.
//
// The callback's return values are interpreted as:
//  1. "match" means "this line matches", which is collected for the "any/all" return values
//  2. "mark" means "mark this line as expected / handled / etc".
//
// When the test completes, any un-marked lines will cause the test to fail.
//
// This is the lowest level utility method, for the most control.
// Most tests should probably use a small helper to make assertions more readable, e.g.:
//
//	func TestThing(t *testing.T) {
//		mockoutput := NewMockOutput(...)
//		// run your test
//		m := NewLineMatcher(t, mockoutput.Lines())
//		mustFindLine := func(t *testing.T, line string) {
//			matched := m.Match(func(l string, _ bool) (match, mark bool) {
//				match = line == l
//				return match, match
//			})
//			assert.NotZero(t, matched, "failed to find line: %v", line)
//		}
//		mustFindLine("found workflow")
//		mustFindLine("the-wid")
//		// this test will fail if any other lines of text are printed, as they were not matched
//	}
func (l *LineMatcher) Match(cb func(line string, previouslyMarked bool) (match, mark bool)) (matched int) {
	l.mut.Lock()
	defer l.mut.Unlock()
	if l.closed {
		// racy assertions are always wrong
		l.t.Error("LineMatcher cannot be used after the test has completed")
		// t.Fatal does not panic and is somewhat-frequently ignored, so panic manually instead
		panic("LineMatcher cannot be used after the test has completed")
	}

	if len(l.lines) == 0 {
		return 0
	}
	for i := range l.lines {
		match, mark := cb(l.lines[i], l.marked[i])
		if match {
			matched++
		}
		if mark {
			l.marked[i] = true
		}
	}
	return matched
}

// MarkAll marks all matching lines, and returns the count of lines that have been newly marked.
//
// All lines are marked if the callback is nil, as an easy way for a test to say "other lines do not matter".
func (l *LineMatcher) MarkAll(cb func(line string) bool) (newlyMarked int) {
	return l.Match(func(line string, previouslyMarked bool) (match, mark bool) {
		if !previouslyMarked {
			if cb == nil || cb(line) {
				return true, true
			}
		}
		return false, false
	})
}

// MatchesOnce is a simplified Match for common use:
// it returns true if any previously-un-matched line matches the callback, and
// it marks that line so future matches skip over it.
//
// Only one line will be marked per call (or none).
// If you expect multiple matching lines, you can just call this multiple times to mark them all.
//
// To help understand test failures, some troubleshooting logs are produced,
// e.g. the matching line (or none) and any other matching-but-ignored lines.
//
// To require this to succeed, assert that the return is true, or use a Must* method instead.
func (l *LineMatcher) MatchesOnce(t *testing.T, cb func(line string) bool) bool {
	t.Helper()

	var found bool
	var foundLine string
	var alsoMarked []string
	var unmarkedAfterFound []string
	_ = l.Match(func(line string, previouslyMarked bool) (bool, bool) {
		if cb(line) {
			if previouslyMarked {
				alsoMarked = append(alsoMarked, line)
			} else if found {
				unmarkedAfterFound = append(unmarkedAfterFound, line)
			} else {
				found = true
				foundLine = line
				return true, true
			}
		}
		return false, false
	})
	if found {
		t.Log("found matching line:", foundLine)
	} else {
		t.Log("did not match any new lines")
	}
	for _, line := range alsoMarked {
		t.Log("\talso matched previously-marked line:", line)
	}
	for _, line := range unmarkedAfterFound {
		t.Log("\talso matched another line:", line)
	}
	return found
}

// MustMatchExactly is the simplest matcher, a small wrapper around MatchesOnce:
//   - a line must == the passed line or the test is failed (t.Error)
//   - the line must not have matched previously
//   - only one matched-line is marked (other matches are ignored, if any)
func (l *LineMatcher) MustMatchExactly(t *testing.T, s string) bool {
	t.Helper()
	found := l.MatchesOnce(t, func(line string) bool {
		return line == s
	})
	if !found {
		t.Errorf("did not match any new line equal to: %q", s)
	}
	return found
}

// MustMatchContaining follows the same rules as MustMatchExactly, but will match
// any line containing the passed text, rather than == the passed text.
func (l *LineMatcher) MustMatchContaining(t *testing.T, s string) bool {
	t.Helper()
	found := l.MatchesOnce(t, func(line string) bool {
		return strings.Contains(line, s)
	})
	if !found {
		t.Errorf("did not match any new line containing: %q", s)
	}
	return found
}
