package rql

import (
	"errors"
	"strconv"
	"strings"
)

// Sentinel errors. Every error returned by this package wraps exactly one of
// these (or is a *Error carrying an underlying cause), so callers can match with
// errors.Is instead of inspecting message text.
var (
	// ErrUnsupportedOperator reports an operator this translator does not
	// implement (aggregate, rel, first, one, values, offset, ...) or an unknown
	// operator name.
	ErrUnsupportedOperator = errors.New("rql: unsupported operator")
	// ErrUnsupportedConverter reports a value converter prefix that cannot be
	// bound as a SQL parameter (re:, RE:, glob:) or an unrecognised prefix.
	ErrUnsupportedConverter = errors.New("rql: unsupported converter")
	// ErrUnknownField reports a field that is not registered in the Config.
	ErrUnknownField = errors.New("rql: unknown field")
	// ErrOperatorNotAllowed reports an operator that is not enabled for the field.
	ErrOperatorNotAllowed = errors.New("rql: operator not allowed")
	// ErrNotSortable reports a sort() argument that is not a sortable field.
	ErrNotSortable = errors.New("rql: field not sortable")
	// ErrNotSelectable reports a select() argument that is not a selectable field.
	ErrNotSelectable = errors.New("rql: field not selectable")
	// ErrMaxDepth reports a query nested deeper than Config.MaxDepth.
	ErrMaxDepth = errors.New("rql: maximum depth exceeded")
	// ErrMixedConjunctions reports a group that mixes '&' (or ',') with '|'.
	ErrMixedConjunctions = errors.New("rql: mixed conjunctions in one group")
	// ErrInvalidPath reports a nested path segment that is not a plain
	// identifier, or a path whose root is not registered.
	ErrInvalidPath = errors.New("rql: invalid path segment")
	// ErrInvalidSyntax reports malformed input.
	ErrInvalidSyntax = errors.New("rql: invalid syntax")
	// ErrInvalidValue reports a value that does not match its field type.
	ErrInvalidValue = errors.New("rql: invalid value")
	// ErrTooComplex reports input above Config.MaxInputLen or Config.MaxTokens.
	ErrTooComplex = errors.New("rql: query too complex")
)

// Error is the single typed error returned by the scanner, parser and builder.
//
// Err identifies the failure class for errors.Is; Msg carries a human readable
// detail; Pos is the byte offset of the offending construct inside the query
// (-1 when the error is not tied to a position, e.g. a configuration problem).
type Error struct {
	// Err is the sentinel this error classifies as. It may be nil for errors
	// that only wrap an underlying cause.
	Err error
	// Msg is the human readable detail, without the sentinel text.
	Msg string
	// Pos is the byte offset of the offending construct, or -1 if unknown.
	Pos int
	// causedBy is an underlying error returned by a custom operator or by a
	// validator. It is reachable through errors.Unwrap but is not printed twice.
	causedBy error
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	b.Grow(32 + len(e.Msg))
	if e.Err != nil {
		b.WriteString(e.Err.Error())
		if e.Msg != "" {
			b.WriteString(": ")
		}
	}
	b.WriteString(e.Msg)
	if e.Pos >= 0 {
		b.WriteString(" at byte ")
		b.WriteString(strconv.Itoa(e.Pos))
	}
	return b.String()
}

// Is reports whether the error classifies as target, so that errors.Is works
// even when the error also wraps a cause.
func (e *Error) Is(target error) bool {
	return e.Err != nil && errors.Is(e.Err, target)
}

// Unwrap returns the underlying cause, if any.
func (e *Error) Unwrap() error { return e.causedBy }

// Position returns the byte offset of the offending construct, or -1.
func (e *Error) Position() int { return e.Pos }

// ParseError is an alias for Error, kept so that callers migrating from the
// previous API can name the type. Parsing and building share one error type.
type ParseError = Error

// BuildError is an alias for Error, kept so that callers migrating from the
// previous API can name the type.
type BuildError = Error

// errAt builds a positioned error of the given class.
func errAt(class error, pos int, msg string) *Error {
	return &Error{Err: class, Msg: msg, Pos: pos}
}

// errNoPos builds an unpositioned error of the given class.
func errNoPos(class error, msg string) *Error {
	return &Error{Err: class, Msg: msg, Pos: -1}
}

// errCause wraps an underlying error, classifying as the given class.
func errCause(class error, pos int, msg string, cause error) *Error {
	return &Error{Err: class, Msg: msg, Pos: pos, causedBy: cause}
}

// quote renders s for inclusion in an error message. It is used on error paths
// only, never on the parse/build hot path.
func quote(s string) string {
	const maxInline = 64
	if len(s) > maxInline {
		return `"` + s[:maxInline] + `"...`
	}
	return `"` + s + `"`
}

// itoa is strconv.Itoa under a shorter name for error-message code.
func itoa(n int) string { return strconv.Itoa(n) }
