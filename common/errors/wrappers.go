// Package errors contains custom error implementations and tools.
// All packages should import it rather than the stdlib errors.  Add any missing functions as needed.
//
// Due to how messy Is/As-equality is with value-typed errors, *always* use pointer types for custom errors.
// `fmt.Errorf` does this too.
package errors

import (
	"errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// stdlib section

// Is forwards directly to errors.Is
func Is(err error, target error) bool {
	return errors.Is(err, target)
}

// As forwards directly to errors.As
func As(err error, target any) bool {
	return errors.As(err, target) // IDEs complain that this is not &target, but that must be done by callers
}

// New forwards directly to errors.New
func New(message string) error {
	return errors.New(message)
}

// Details returns the `"error_details":{...}` log field of an error, if present.
// It is safe to use with any error, including nil - errors without log fields will simply lack this field.
//
// Log keys in all nested errors are collected and flattened, in increasing-depth order.
// Zap treats this as last-key-wins: if there are any duplicates, the *most specific* / furthest down the call stack
// will be used, and the others will be silently ignored.
//
// ---
//
// There are multiple reasonable ways to collect this, and we can definitely change it...
// But if the structure is not at least somewhat flattened, it'll have the same details at arbitrary depths based on
// call stack, which is often very hard to query (though it is more descriptive of the source of details).
//
// A better tactic is to simply have unique keys.  Suffix it with a uuid or something if it's truly important that it is
// not lost or confused with others.
//
// To help prevent a namespace explosion of related fields, use the &zapobj to group them - that way you only need to
// unique-ify the outer-most key, not all sub-keys.
//
// TODO: should I maybe suffix with a counter?  auto-collect the nearest path+line?
func Details(err error) zap.Field {
	var logfields *logerr
	if errors.As(err, &logfields) {
		return logfields.Field()
	}
	return zap.Skip() // a no-op field, since zero values are not valid
}

// WithDetails adds log fields to an error.  These can be included in logs with `.Info(Details(err))`.
// If err is nil, nil will be returned.
func WithDetails(err error, fields ...zap.Field) error {
	if err == nil {
		return nil
	}
	return &logerr{
		cause:  err,
		fields: fields,
	}
}

// logerr is an error wrapper that (recursively) holds zap.Field values related to an error.
//   - it does not contain a message: wrap errors with a fmt.Errorf or if you want the `.Error()` to contain something.
//   - it does not include the zap fields in the message string: use `.Info(Details(err))`, don't rely on `.Error()`.
//     (this also helps keep relatively stable messages, which are easier to search for)
//   - it recursively collects all fields into a flat, top-level `zap.Object("error_details", {"child":"data"})` field.
//     to use this, always use `.Info(Details(err))` when logging about an error.
type logerr struct {
	cause  error
	fields []zap.Field
}

var _ error = &logerr{}

func (l *logerr) Error() string {
	return l.cause.Error()
}

func (l *logerr) Field() zap.Field {
	return zap.Object("error_details", &zapobj{l.recursiveFields()})
}

func (l *logerr) recursiveFields() []zap.Field {
	var child *logerr
	if errors.As(l.cause, &child) {
		return append(l.fields, child.recursiveFields()...)
	}
	return l.fields
}

// zapobj is a helper structure to produce a key:value object out of a list of fields.
// oddly this is not part of zap already.  it's FAR more useful than zap.Namespace.
type zapobj struct {
	fields []zap.Field
}

var _ zapcore.ObjectMarshaler = &zapobj{}

func (z *zapobj) MarshalLogObject(encoder zapcore.ObjectEncoder) error {
	for _, f := range z.fields {
		f.AddTo(encoder)
	}
	return nil
}
