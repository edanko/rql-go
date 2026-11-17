package rql

import (
	"strconv"
	"strings"
)

// ArrayOp selects the SQL shape of an array predicate.
type ArrayOp uint8

// Array predicate shapes.
const (
	// ArrayIn renders "col IN (…)" or, for dialects that bind slices, "col = ANY($n)".
	ArrayIn ArrayOp = iota
	// ArrayNotIn renders "col NOT IN (…)" or "col <> ALL($n)".
	ArrayNotIn
	// ArrayContainsAll renders "col @> ARRAY[…]" (contains every value).
	ArrayContainsAll
	// ArrayOverlaps renders "col && ARRAY[…]" (contains any value).
	ArrayOverlaps
)

// Dialect renders database-specific SQL syntax: identifier quoting, parameter
// placeholders, array predicates and JSONB extraction casts.
//
// Implementations must be safe for concurrent use, and QuoteIdentifier must
// escape its input (double '"').
type Dialect interface {
	// QuoteIdentifier renders name as a delimited identifier.
	QuoteIdentifier(name string) string
	// Placeholder renders the n-th (1-based) parameter placeholder.
	Placeholder(n int) string
	// BuildArrayOp renders an array predicate over col using the supplied
	// placeholder list.
	BuildArrayOp(op ArrayOp, col string, placeholders string) string
	// CastFor returns the SQL cast applied to text extracted from a JSONB path
	// so that it compares with a typed parameter, e.g. "::int8". It returns ""
	// when no cast is needed.
	CastFor(t FieldType) string
}

// standardQuoting is an optional Dialect capability: identifiers are quoted by
// doubling '"' inside double quotes, which lets the builder write a plain column
// straight into the SQL buffer. A dialect with different quoting must not
// implement it.
type standardQuoting interface {
	StandardQuoting() bool
}

// arrayAppender is an optional Dialect capability: the dialect renders an array
// predicate directly into the caller's buffer, which avoids materialising the
// placeholder list and the predicate as separate strings. firstArg is the
// 1-based number of the first parameter the predicate refers to.
type arrayAppender interface {
	AppendArrayOp(dst []byte, op ArrayOp, col string, firstArg, count int) []byte
}

// appendPlaceholders appends count placeholders starting at first.
func appendPlaceholders(dst []byte, first, count int, numbered bool) []byte {
	for i := 0; i < count; i++ {
		if i > 0 {
			dst = append(dst, ',', ' ')
		}
		if !numbered {
			dst = append(dst, '?')
			continue
		}
		dst = append(dst, '$')
		dst = strconv.AppendInt(dst, int64(first+i), 10)
	}
	return dst
}

// appendExpandedArrayOp appends an expanded array predicate.
func appendExpandedArrayOp(dst []byte, op ArrayOp, col string, first, count int, numbered bool) []byte {
	dst = append(dst, col...)
	switch op {
	case ArrayIn:
		dst = append(dst, " IN ("...)
		dst = appendPlaceholders(dst, first, count, numbered)
		dst = append(dst, ')')
	case ArrayNotIn:
		dst = append(dst, " NOT IN ("...)
		dst = appendPlaceholders(dst, first, count, numbered)
		dst = append(dst, ')')
	case ArrayContainsAll:
		dst = append(dst, " @> ARRAY["...)
		dst = appendPlaceholders(dst, first, count, numbered)
		dst = append(dst, ']')
	case ArrayOverlaps:
		dst = append(dst, " && ARRAY["...)
		dst = appendPlaceholders(dst, first, count, numbered)
		dst = append(dst, ']')
	}
	return dst
}

// sliceBinder is an optional Dialect capability: the dialect binds an array
// predicate as one slice parameter instead of expanding it into element
// placeholders. PostgresOptimized implements it; drivers that cannot bind Go
// slices must not.
type sliceBinder interface {
	BindsArrayParameter() bool
}

// quotedIdent renders name as a delimited identifier, doubling any embedded
// delimiter so that a name can never terminate its own quoting.
func quotedIdent(name string) string {
	needs := false
	for i := 0; i < len(name); i++ {
		if name[i] == '"' {
			needs = true
			break
		}
	}
	if !needs {
		return `"` + name + `"`
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// postgresCast returns the cast suffix for a field type.
func postgresCast(t FieldType) string {
	switch t {
	case TypeInt:
		return "::int8"
	case TypeFloat:
		return "::float8"
	case TypeBool:
		return "::bool"
	case TypeTime:
		return "::timestamptz"
	case TypeDate:
		return "::date"
	case TypeUUID:
		return "::uuid"
	default:
		return ""
	}
}

// Postgres renders standard PostgreSQL SQL with $N placeholders and expanded
// array predicates (IN (…), @> ARRAY[…]). It is driver neutral: it never binds a
// Go slice, so it works with lib/pq, pgx and database/sql alike.
type Postgres struct{}

// QuoteIdentifier implements Dialect.
func (Postgres) QuoteIdentifier(name string) string { return quotedIdent(name) }

// StandardQuoting implements standardQuoting.
func (Postgres) StandardQuoting() bool { return true }

// Placeholder implements Dialect.
func (Postgres) Placeholder(n int) string {
	buf := make([]byte, 0, 6)
	buf = append(buf, '$')
	buf = strconv.AppendInt(buf, int64(n), 10)
	return string(buf)
}

// BuildArrayOp implements Dialect.
func (Postgres) BuildArrayOp(op ArrayOp, col, placeholders string) string {
	return buildExpandedArrayOp(op, col, placeholders, "ARRAY[", "IN (", "NOT IN (")
}

// CastFor implements Dialect.
func (Postgres) CastFor(t FieldType) string { return postgresCast(t) }

// AppendArrayOp implements arrayAppender.
func (Postgres) AppendArrayOp(dst []byte, op ArrayOp, col string, firstArg, count int) []byte {
	return appendExpandedArrayOp(dst, op, col, firstArg, count, true)
}

// PostgresOptimized renders PostgreSQL SQL that binds array predicates as a
// single slice parameter (= ANY($1), <> ALL($1), @> $1, && $1). It requires a
// driver that can encode Go slices (pgx); it allocates one argument instead of
// one per element.
type PostgresOptimized struct{}

// QuoteIdentifier implements Dialect.
func (PostgresOptimized) QuoteIdentifier(name string) string { return quotedIdent(name) }

// StandardQuoting implements standardQuoting.
func (PostgresOptimized) StandardQuoting() bool { return true }

// Placeholder implements Dialect.
func (PostgresOptimized) Placeholder(n int) string {
	buf := make([]byte, 0, 6)
	buf = append(buf, '$')
	buf = strconv.AppendInt(buf, int64(n), 10)
	return string(buf)
}

// BuildArrayOp implements Dialect.
func (PostgresOptimized) BuildArrayOp(op ArrayOp, col, placeholders string) string {
	var b strings.Builder
	b.Grow(len(col) + len(placeholders) + 8)
	b.WriteString(col)
	switch op {
	case ArrayIn:
		b.WriteString(" = ANY(")
	case ArrayNotIn:
		b.WriteString(" <> ALL(")
	case ArrayContainsAll:
		b.WriteString(" @> ")
	case ArrayOverlaps:
		b.WriteString(" && ")
	}
	b.WriteString(placeholders)
	if op == ArrayIn || op == ArrayNotIn {
		b.WriteByte(')')
	}
	return b.String()
}

// CastFor implements Dialect.
func (PostgresOptimized) CastFor(t FieldType) string { return postgresCast(t) }

// BindsArrayParameter implements sliceBinder: array predicates are passed as a
// single slice parameter.
func (PostgresOptimized) BindsArrayParameter() bool { return true }

// AppendArrayOp implements arrayAppender.
func (PostgresOptimized) AppendArrayOp(dst []byte, op ArrayOp, col string, firstArg, _ int) []byte {
	dst = append(dst, col...)
	switch op {
	case ArrayIn:
		dst = append(dst, " = ANY("...)
	case ArrayNotIn:
		dst = append(dst, " <> ALL("...)
	case ArrayContainsAll:
		dst = append(dst, " @> "...)
	case ArrayOverlaps:
		dst = append(dst, " && "...)
	}
	dst = append(dst, '$')
	dst = strconv.AppendInt(dst, int64(firstArg), 10)
	if op == ArrayIn || op == ArrayNotIn {
		dst = append(dst, ')')
	}
	return dst
}

// QuestionMark renders PostgreSQL SQL with '?' placeholders for drivers and ORMs
// that renumber their own placeholders (database/sql, gorm). Array predicates are
// always expanded so that a Go slice is never bound directly.
type QuestionMark struct{}

// QuoteIdentifier implements Dialect.
func (QuestionMark) QuoteIdentifier(name string) string { return quotedIdent(name) }

// StandardQuoting implements standardQuoting.
func (QuestionMark) StandardQuoting() bool { return true }

// Placeholder implements Dialect.
func (QuestionMark) Placeholder(int) string { return "?" }

// BuildArrayOp implements Dialect.
func (QuestionMark) BuildArrayOp(op ArrayOp, col, placeholders string) string {
	return buildExpandedArrayOp(op, col, placeholders, "ARRAY[", "IN (", "NOT IN (")
}

// CastFor implements Dialect.
func (QuestionMark) CastFor(t FieldType) string { return postgresCast(t) }

// AppendArrayOp implements arrayAppender.
func (QuestionMark) AppendArrayOp(dst []byte, op ArrayOp, col string, firstArg, count int) []byte {
	return appendExpandedArrayOp(dst, op, col, firstArg, count, false)
}

// buildExpandedArrayOp renders an expanded array predicate.
func buildExpandedArrayOp(op ArrayOp, col, placeholders, arrayOpen, inOpen, notInOpen string) string {
	var b strings.Builder
	b.Grow(len(col) + len(placeholders) + len(arrayOpen) + 8)
	b.WriteString(col)
	switch op {
	case ArrayIn:
		b.WriteString(" ")
		b.WriteString(inOpen)
		b.WriteString(placeholders)
		b.WriteString(")")
	case ArrayNotIn:
		b.WriteString(" ")
		b.WriteString(notInOpen)
		b.WriteString(placeholders)
		b.WriteString(")")
	case ArrayContainsAll:
		b.WriteString(" @> ")
		b.WriteString(arrayOpen)
		b.WriteString(placeholders)
		b.WriteString("]")
	case ArrayOverlaps:
		b.WriteString(" && ")
		b.WriteString(arrayOpen)
		b.WriteString(placeholders)
		b.WriteString("]")
	}
	return b.String()
}
