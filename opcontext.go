package rql

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// OpContext is the handle a registered operator uses to resolve columns, bind
// parameters and shape the result. It is valid only for the duration of the
// operator call: do not retain it or its arguments.
type OpContext struct {
	b         *builder
	op        string
	kind      OperatorKind
	field     *field
	fieldName string
	values    []Value
	args      []any
	pos       int
}

// Operator returns the operator name as registered.
func (c *OpContext) Operator() string { return c.op }

// Position returns the byte offset of the operator invocation in the query.
func (c *OpContext) Position() int { return c.pos }

// FieldType returns the type the operator's value was coerced to. For the infix
// form it is the field's type for this operator (a per-(field,operator) override
// applies); for the bare form it is the type the value parsed as.
func (c *OpContext) FieldType() FieldType {
	if c.field != nil {
		t := c.field.typeFor(c.op)
		if t == TypeJSON {
			// An untyped json leaf takes its type from the compared value, which
			// is what its extraction cast is based on.
			return valueType(c.Value())
		}
		return t
	}
	switch c.Value().Kind {
	case KindString:
		return TypeString
	case KindInt:
		return TypeInt
	case KindFloat:
		return TypeFloat
	case KindBool:
		return TypeBool
	case KindTime:
		return TypeTime
	case KindUUID:
		return TypeUUID
	case KindArray:
		return TypeArray
	default:
		return TypeString
	}
}

// Cast returns the SQL cast that turns extracted JSONB text into the field's
// type (for example "::int8"), or "" when none is needed. An out-of-tree
// operator can use it to render `(column)::TYPE`.
func (c *OpContext) Cast() string { return c.b.d.CastFor(c.FieldType()) }

// Dialect returns the dialect the translator renders with.
func (c *OpContext) Dialect() Dialect { return c.b.d }

// Context returns the request context passed to Translate.
//
// Custom operators use it for request-scoped values — a tenant code, a request id, the
// authenticated caller — and to observe the caller's deadline. Values are normally attached
// with context.WithValue:
//
//	tenant, _ := c.Context().Value(tenantKey{}).(string)
//	return `"tenant_id" = ` + c.Arg(tenant), nil
//
// The returned context is never nil, so an operator can call it unconditionally. It is the
// same context the resolver was given, so a cancelled context aborts the translation before
// the operator runs; an operator that does its own I/O should check ctx.Err() as usual.
func (c *OpContext) Context() context.Context {
	if c == nil || c.b == nil || c.b.ctx == nil {
		return context.Background()
	}
	return c.b.ctx
}

// Field returns the field name as written, which for the infix form is the
// registered field name (possibly a dotted or slashed path). It reports false
// for the bare form, e.g. `showArchived=true`.
func (c *OpContext) Field() (string, bool) { return c.fieldName, c.fieldName != "" }

// Args returns the operator arguments as Go values: the field name first for the
// infix form, then the decoded values.
func (c *OpContext) Args() []any {
	if c.args == nil {
		c.args = operatorArgsFor(c.fieldName, c.values)
	}
	return c.args
}

// Values returns the operator's value arguments. For the infix form this is the
// single value written after the operator; for a normalized call it is every
// argument.
func (c *OpContext) Values() []Value { return c.values }

// Value returns the last value argument, or a NULL value when there is none.
func (c *OpContext) Value() Value {
	if len(c.values) == 0 {
		return Value{Kind: KindNull}
	}
	return c.values[len(c.values)-1]
}

// ValueAt returns the i-th value argument.
func (c *OpContext) ValueAt(i int) (Value, bool) {
	if i < 0 || i >= len(c.values) {
		return Value{}, false
	}
	return c.values[i], true
}

// Column resolves a configured field name (or a nested path such as "a/b") to a
// quoted SQL column expression. It reuses the same resolution, path validation,
// JSONB extraction and casting rules as the built-in operators, so a custom
// operator cannot bypass them.
func (c *OpContext) Column(name string) (string, error) {
	f := c.b.cfg.lookupField(name)
	var path []string
	if f == nil {
		segs := splitPath(name)
		root, ok := pathRoot(segs)
		if !ok {
			return "", errNoPos(ErrInvalidPath, "path "+quote(name)+" has an empty root segment")
		}
		if len(segs) > 1 {
			f = c.b.cfg.lookupField(root)
			for _, seg := range segs[1:] {
				if !isPathSegment(seg) {
					return "", errNoPos(ErrInvalidPath, "invalid path segment "+quote(seg))
				}
				path = append(path, seg)
			}
		}
	}
	if f == nil {
		return "", errNoPos(ErrUnknownField, "unknown field "+quote(name))
	}
	if f.regErr != nil {
		return "", f.regErr
	}
	castType := f.typ
	if castType == TypeJSON {
		castType = valueType(c.Value())
	}
	col, err := c.b.r.column(f, c.op, path, true, castType)
	if err != nil {
		return "", err
	}
	return col, nil
}

// Arg binds one parameter and returns its placeholder. It may be called more
// than once to bind several parameters, in output order.
func (c *OpContext) Arg(v any) string {
	c.b.args = append(c.b.args, v)
	return c.b.placeholderText()
}

// ArgsAfter is a helper for operators that bind a whole slice element by
// element: it binds every value and returns their placeholders joined with ", ".
func (c *OpContext) ArgsAfter(values []Value) string {
	if len(values) == 0 {
		return ""
	}
	out := make([]byte, 0, 8*len(values))
	for i := range values {
		if i > 0 {
			out = append(out, ',', ' ')
		}
		c.b.args = append(c.b.args, values[i].Any())
		c.b.n++
		if c.b.pg {
			out = append(out, '$')
			out = strconv.AppendInt(out, int64(c.b.n), 10)
			continue
		}
		out = append(out, c.b.d.Placeholder(c.b.n)...)
	}
	return string(out)
}

// Flag records an out-of-band value on the result (for example "showArchived").
// It does not affect the SQL.
func (c *OpContext) Flag(key string, value any) {
	if c.b.res.Flags == nil {
		c.b.res.Flags = make(map[string]any, 2)
	}
	c.b.res.Flags[key] = value
}

// Sort returns the ordering requested so far.
func (c *OpContext) Sort() []SortField { return c.b.res.Sort }

// SetSort replaces the ordering.
func (c *OpContext) SetSort(fields ...SortField) { c.b.res.Sort = fields }

// Limit returns the row limit requested so far.
func (c *OpContext) Limit() (int, bool) {
	if c.b.res.Limit == nil {
		return 0, false
	}
	return *c.b.res.Limit, true
}

// SetLimit sets the row limit.
func (c *OpContext) SetLimit(n int) { c.b.res.Limit = &n }

// Offset returns the row offset requested so far.
func (c *OpContext) Offset() (int, bool) {
	if c.b.res.Offset == nil {
		return 0, false
	}
	return *c.b.res.Offset, true
}

// SetOffset sets the row offset.
func (c *OpContext) SetOffset(n int) { c.b.res.Offset = &n }

// Bool returns the value argument as a boolean.
func (c *OpContext) Bool() (bool, error) {
	v := c.Value()
	if v.Kind != KindBool {
		return false, c.argError("boolean", v)
	}
	return v.Bool, nil
}

// String returns the value argument as a string.
func (c *OpContext) String() (string, error) {
	v := c.Value()
	switch v.Kind {
	case KindString, KindUUID:
		return v.Str, nil
	case KindInt, KindFloat, KindBool, KindTime:
		return v.scalarString(), nil
	default:
		return "", c.argError("string", v)
	}
}

// Int returns the value argument as an integer.
func (c *OpContext) Int() (int64, error) {
	v := c.Value()
	switch v.Kind {
	case KindInt:
		return v.Int, nil
	case KindFloat:
		if v.Float == float64(int64(v.Float)) {
			return int64(v.Float), nil
		}
	case KindString:
		if n, err := strconv.ParseInt(v.Str, 10, 64); err == nil {
			return n, nil
		}
	case KindNull, KindBool, KindTime, KindUUID, KindArray:
	}
	return 0, c.argError("integer", v)
}

// Float returns the value argument as a float.
func (c *OpContext) Float() (float64, error) {
	v := c.Value()
	switch v.Kind {
	case KindFloat:
		return v.Float, nil
	case KindInt:
		return float64(v.Int), nil
	case KindString:
		if f, err := strconv.ParseFloat(v.Str, 64); err == nil {
			return f, nil
		}
	case KindNull, KindBool, KindTime, KindUUID, KindArray:
	}
	return 0, c.argError("number", v)
}

// Time returns the value argument as a timestamp.
func (c *OpContext) Time() (time.Time, error) {
	v := c.Value()
	switch v.Kind {
	case KindTime:
		return v.Time, nil
	case KindString:
		if t, ok := parseTimeText(v.Str); ok {
			return t, nil
		}
	case KindNull, KindInt, KindFloat, KindBool, KindUUID, KindArray:
	}
	return time.Time{}, c.argError("timestamp", v)
}

// IsNull reports whether the value argument is NULL.
func (c *OpContext) IsNull() bool { return c.Value().isNull() }

// argError builds the typed error for a mistyped operator argument.
func (c *OpContext) argError(want string, v Value) error {
	return errAt(ErrInvalidValue, c.pos,
		"operator "+quote(c.op)+" expects a "+want+" argument, got "+v.Kind.String())
}

// operatorArgsFor materialises operator arguments from a field name and values.
func operatorArgsFor(fieldName string, values []Value) []any {
	size := len(values)
	if fieldName != "" {
		size++
	}
	out := make([]any, 0, size)
	if fieldName != "" {
		out = append(out, fieldName)
	}
	for i := range values {
		out = append(out, values[i].Any())
	}
	return out
}

// ValueOp selects how an operator's value argument is rendered into SQL.
//
// ValueSQL with these constants is the direct equivalent of the value-SQL helper the
// translator this package replaced exposed: the returned fragment contains only the
// comparison operator and its bound placeholders (never a column), so an operator supplies
// the column, cast or subquery expression itself.
type ValueOp uint8

// Value operations accepted by ValueSQL.
const (
	ValueEq       ValueOp = iota // = $n
	ValueNe                      // <> $n
	ValueLt                      // < $n
	ValueLe                      // <= $n
	ValueGt                      // > $n
	ValueGe                      // >= $n
	ValueIn                      // = ANY(ARRAY[$1, $2, ...])
	ValueNotIn                   // <> ALL(ARRAY[$1, $2, ...])
	ValueLike                    // LIKE $n, with '*' translated to '%'
	ValueILike                   // ILIKE $n, with '*' translated to '%'
	ValueLikeAny                 // LIKE ANY(ARRAY[$1, $2, ...]), with '*' translated to '%'
	ValueILikeAny                // ILIKE ANY(ARRAY[$1, $2, ...]), with '*' translated to '%'
)

// String returns the operator spelling, for diagnostics.
func (op ValueOp) String() string {
	switch op {
	case ValueEq:
		return "eq"
	case ValueNe:
		return "ne"
	case ValueLt:
		return "lt"
	case ValueLe:
		return "le"
	case ValueGt:
		return "gt"
	case ValueGe:
		return "ge"
	case ValueIn:
		return "in"
	case ValueNotIn:
		return "notIn"
	case ValueLike:
		return "like"
	case ValueILike:
		return "ilike"
	case ValueLikeAny:
		return "likeAny"
	case ValueILikeAny:
		return "ilikeAny"
	default:
		return "unknown"
	}
}

// sqlText returns the scalar comparison operator for op.
func (op ValueOp) sqlText() string {
	switch op {
	case ValueEq:
		return "="
	case ValueNe:
		return "<>"
	case ValueLt:
		return "<"
	case ValueLe:
		return "<="
	case ValueGt:
		return ">"
	case ValueGe:
		return ">="
	default:
		return ""
	}
}

// ValueSQL renders operator's value argument as a SQL fragment and binds its parameters,
// which keeps custom operators from hand-building placeholders and wildcard translation:
//
//	status::TEXT " + fragment            // c.ValueSQL(ValueEq)  -> "= $1"
//	("groups"."name" " + fragment + ")"  // c.ValueSQL(ValueILikeAny) -> "ILIKE ANY(ARRAY[$1, $2])"
//
// A NULL value renders IS NULL / IS NOT NULL for ValueEq / ValueNe and is an error for every
// other operation. ValueIn, ValueNotIn, ValueLikeAny and ValueILikeAny always use the ARRAY
// form, so a scalar argument becomes a one-element array; ValueLike and ValueILike render the
// scalar form and also accept an array, in which case they emit the ANY form. The '*' wildcard
// is translated to '%' for the four LIKE/ILIKE operations only, and the ARRAY forms carry an
// explicit cast for non-text field types (ARRAY[$1, $2]::int8[]), because PostgreSQL otherwise
// infers text[] and rejects `integer = text`.
func (c *OpContext) ValueSQL(op ValueOp) (string, error) {
	v := c.Value()

	if v.Kind == KindNull {
		if op == ValueEq {
			return "IS NULL", nil
		}
		if op == ValueNe {
			return "IS NOT NULL", nil
		}
		return "", errAt(ErrInvalidValue, c.pos,
			"operator "+quote(c.op)+": "+op.String()+" does not accept null")
	}

	switch op {
	case ValueEq, ValueNe, ValueLt, ValueLe, ValueGt, ValueGe:
		if v.Kind == KindArray {
			return "", c.argError("scalar", v)
		}
		return op.sqlText() + " " + c.Arg(v.Any()), nil

	case ValueIn, ValueNotIn:
		vals := valueList(v)
		if len(vals) == 0 {
			return "", errAt(ErrInvalidValue, c.pos,
				"operator "+quote(c.op)+": "+op.String()+" needs at least one value")
		}
		sqlOp, quantifier := "=", "ANY"
		if op == ValueNotIn {
			sqlOp, quantifier = "<>", "ALL"
		}
		return sqlOp + " " + quantifier + "(ARRAY[" + c.ArgsAfter(vals) + "]" + c.arrayCast() + ")", nil

	case ValueLike, ValueILike, ValueLikeAny, ValueILikeAny:
		sqlOp := "LIKE"
		if op == ValueILike || op == ValueILikeAny {
			sqlOp = "ILIKE"
		}
		vals := wildcardList(valueList(v))
		if len(vals) == 0 {
			return "", errAt(ErrInvalidValue, c.pos,
				"operator "+quote(c.op)+": "+op.String()+" needs at least one value")
		}
		forceAny := op == ValueLikeAny || op == ValueILikeAny
		if len(vals) == 1 && !forceAny {
			return sqlOp + " " + c.Arg(vals[0].Any()), nil
		}
		return sqlOp + " ANY(ARRAY[" + c.ArgsAfter(vals) + "]" + c.arrayCast() + ")", nil
	}

	return "", errAt(ErrInvalidValue, c.pos,
		"operator "+quote(c.op)+": unknown value operation "+op.String())
}

// LikePredicate renders a complete LIKE predicate for column. It is the index-friendly
// counterpart of ValueSQL(ValueLikeAny), and it picks the rendering from the number of patterns:
// an OR chain (`col LIKE $1 OR col LIKE $2`) up to 32 patterns, the compact
// `col LIKE ANY(ARRAY[...])` beyond that. The `*` wildcard is translated to '%' in every element.
//
// PostgreSQL can use a btree prefix index (text_pattern_ops) for `col LIKE 'abc%'` and turns a
// chain of such predicates into a BitmapOr, but it cannot use that index for
// `col LIKE ANY(ARRAY[...])`. On 200k rows with a prefix index, two selective patterns measured
// 0.05 ms as an OR chain against 7.0 ms as LIKE ANY, and the OR chain stayed ahead at 128
// patterns (1.0 ms against 115 ms). A GIN trigram index serves both forms, and without any index
// the two measure the same.
func (c *OpContext) LikePredicate(column string) (string, error) {
	return c.likePredicate(column, "LIKE")
}

// ILikePredicate is LikePredicate for case-insensitive matching. A btree index cannot serve
// ILIKE at all (only a trigram index can), so the OR form mainly helps when combined with one.
func (c *OpContext) ILikePredicate(column string) (string, error) {
	return c.likePredicate(column, "ILIKE")
}

// likeOrMax is the largest number of patterns still rendered as an OR chain. It is a bound on
// SQL size and planning cost, not a point where the compact form gets faster: with a btree
// prefix index the OR chain measured faster at every count tried up to 128, and without an index
// the two forms measure the same. Measured on 200k rows, five runs, median execution time:
//
//	N      OR chain    LIKE ANY (no btree index)
//	1      0.061 ms    8.019 ms
//	16     0.228 ms    19.586 ms
//	64     0.599 ms    60.737 ms
//	128    0.999 ms    114.792 ms
//
// Above this count the OR chain still grows linearly in text length and planning time, so the
// compact ANY form is emitted instead; a GIN trigram index can serve it.
const likeOrMax = 32

// likePredicate renders a LIKE-family predicate for column: an OR chain for a small number of
// patterns, or the compact ANY form for a large number.
func (c *OpContext) likePredicate(column, sqlOp string) (string, error) {
	v := c.Value()
	if v.Kind == KindNull {
		return "", errAt(ErrInvalidValue, c.pos,
			"operator "+quote(c.op)+": "+sqlOp+" does not accept null")
	}
	vals := wildcardList(valueList(v))
	if len(vals) == 0 {
		return "", errAt(ErrInvalidValue, c.pos,
			"operator "+quote(c.op)+": "+sqlOp+" needs at least one value")
	}
	if len(vals) == 1 {
		return column + " " + sqlOp + " " + c.Arg(vals[0].Any()), nil
	}
	if len(vals) > likeOrMax {
		return column + " " + sqlOp + " ANY(ARRAY[" + c.ArgsAfter(vals) + "])", nil
	}

	var b strings.Builder
	b.Grow(len(vals)*len(column) + 8*len(vals) + 2)
	b.WriteByte('(')
	for i := range vals {
		if i > 0 {
			b.WriteString(" OR ")
		}
		b.WriteString(column)
		b.WriteByte(' ')
		b.WriteString(sqlOp)
		b.WriteByte(' ')
		b.WriteString(c.Arg(vals[i].Any()))
	}
	b.WriteByte(')')
	return b.String(), nil
}

// Strings returns the operator's value as strings: a scalar yields one element and an array
// value (as written `field=in=(a,b)`) yields one element per item. It avoids unpacking Value
// kinds by hand when an operator inspects the query for side effects, for example deciding a
// result flag from the requested statuses.
func (c *OpContext) Strings() ([]string, error) {
	vals := valueList(c.Value())
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v.Kind == KindNull {
			return nil, c.argError("non-null", v)
		}
		if v.Kind == KindArray {
			return nil, c.argError("scalar or flat array", v)
		}
		out = append(out, v.scalarString())
	}
	return out, nil
}

// arrayCast returns the SQL cast for an ARRAY of the field's type, or "" when the type needs
// no cast (text). Without it PostgreSQL resolves an untyped ARRAY[...] to text[] and the
// comparison against an int, uuid, timestamp or boolean column fails at run time.
func (c *OpContext) arrayCast() string {
	if cast := c.Cast(); cast != "" {
		return cast + "[]"
	}
	return ""
}

// valueList returns the elements of an array value, or the value itself as a one-element list.
func valueList(v Value) []Value {
	if v.Kind == KindArray {
		return v.Array
	}
	return []Value{v}
}

// wildcardList translates the RQL '*' wildcard to SQL's '%' in every element.
func wildcardList(vals []Value) []Value {
	out := make([]Value, 0, len(vals))
	for _, v := range vals {
		if v.Kind == KindString || v.Kind == KindUUID {
			out = append(out, Value{Kind: KindString, Str: strings.ReplaceAll(v.Str, "*", "%")})
			continue
		}
		out = append(out, v)
	}
	return out
}
