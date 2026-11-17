package rql

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// inline SQL and argument capacity. Queries that fit (the overwhelming majority)
// need no extra allocations for the SQL buffer or the argument list.
const (
	bufInline = 512
	argInline = 8
)

// workspace holds every scratch object of one translation in a single
// allocation: the parse arena (inline in the parser) and the SQL buffer and
// argument list (inline in the builder).
type workspace struct {
	p parser
	b builder
}

// Translator converts RQL query strings into SQL fragments for one Config and
// one Dialect. It holds no mutable state, so a single Translator is safe for
// concurrent use.
type Translator struct {
	cfg *Config
	d   Dialect
}

// NewTranslator returns a translator for the given configuration and dialect.
//
// A nil configuration means "no fields are known", so every field reference is
// rejected: there is no pass-through mode. A nil dialect selects Postgres.
func NewTranslator(cfg *Config, d Dialect) *Translator {
	if cfg == nil {
		cfg = NewConfig()
	}
	if d == nil {
		d = Postgres{}
	}
	return &Translator{cfg: cfg, d: d}
}

// Config returns the configuration the translator was built with.
func (t *Translator) Config() *Config { return t.cfg }

// Dialect returns the dialect the translator renders with.
func (t *Translator) Dialect() Dialect { return t.d }

// Translate parses query and returns the structured SQL fragments for it.
//
// The returned Result is always non-nil on success, even for an empty query
// (where Where is empty). Errors are *Error values that classify as one of the
// package sentinels and carry the byte offset of the offending construct.
func (t *Translator) Translate(ctx context.Context, query string) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, contextError(err, 0)
	}
	cfg := t.cfg
	if len(query) > cfg.maxInputLen() {
		return nil, errAt(ErrTooComplex, 0,
			"query is longer than the configured maximum ("+itoa(cfg.maxInputLen())+" bytes)")
	}
	// One allocation carries the parser arena, the SQL buffer and the argument
	// list; only the Result and the strings handed to the caller escape it.
	w := &workspace{}
	w.p.init(ctx, cfg, query, t.d)
	root, perr := w.p.parse()
	if perr != nil {
		return nil, perr
	}
	w.b.init(ctx, cfg, t.d, &w.p.res, w.p.nodes)
	if root != noNode {
		if _, err := w.b.write(root); err != nil {
			return nil, err
		}
	}
	out := new(Result)
	*out = w.p.res
	out.Where = string(w.b.buf)
	if len(w.b.args) > 0 {
		// Copy the arguments out so the Result does not pin the workspace.
		out.Args = make([]any, len(w.b.args))
		copy(out.Args, w.b.args)
	}
	return out, nil
}

// postgresStyler is an optional Dialect capability: placeholders are the
// PostgreSQL "$n" form, which lets the builder append numbering without
// allocating a string per parameter.
type postgresStyler interface {
	PostgresStyle() bool
}

// PostgresStyle implements postgresStyler.
func (Postgres) PostgresStyle() bool { return true }

// PostgresStyle implements postgresStyler.
func (PostgresOptimized) PostgresStyle() bool { return true }

// postgresStyle reports whether the dialect uses "$n" placeholders.
func postgresStyle(d Dialect) bool {
	if s, ok := d.(postgresStyler); ok {
		return s.PostgresStyle()
	}
	return false
}

// standardQuoting reports whether the dialect quotes identifiers the standard
// way.
func usesStandardQuoting(d Dialect) bool {
	if q, ok := d.(standardQuoting); ok {
		return q.StandardQuoting()
	}
	return false
}

// bindsSlices reports whether the dialect takes array predicates as one slice
// parameter.
func bindsSlices(d Dialect) bool {
	if s, ok := d.(sliceBinder); ok {
		return s.BindsArrayParameter()
	}
	return false
}

// builder assembles the WHERE expression and the bound arguments.
type builder struct {
	cfg   *Config
	d     Dialect
	r     renderer
	res   *Result
	nodes []astNode
	ctx   context.Context
	buf   []byte
	args  []any
	n     int
	// pg reports whether placeholders are "$n".
	pg bool
	// slice reports whether array predicates bind one slice parameter.
	slice bool
	// standardQuote reports whether identifiers use standard double-quote
	// escaping, which allows writing a column straight into the buffer.
	standardQuote bool

	bufArr [bufInline]byte
	argArr [argInline]any
}

// init prepares the builder for one translation.
func (b *builder) init(ctx context.Context, cfg *Config, d Dialect, res *Result, nodes []astNode) {
	b.cfg = cfg
	b.d = d
	b.r = renderer{d: d}
	b.res = res
	b.nodes = nodes
	b.ctx = ctx
	b.buf = b.bufArr[:0]
	b.args = b.argArr[:0]
	b.pg = postgresStyle(d)
	b.slice = bindsSlices(d)
	b.standardQuote = usesStandardQuoting(d)
}

// write renders one node. It reports whether the node produced SQL: a custom
// operator may return an empty fragment, which removes it from its conjunction.
func (b *builder) write(idx astIndex) (bool, *Error) {
	if err := b.ctx.Err(); err != nil {
		return false, contextError(err, 0)
	}
	n := &b.nodes[idx]
	switch n.kind {
	case astAnd, astOr:
		return b.writeLogic(n)
	case astNot:
		return b.writeNot(n)
	case astBuiltin:
		return b.writeBuiltin(n)
	case astCustom:
		return b.writeCustom(n)
	default:
		return false, errAt(ErrInvalidSyntax, n.pos, "malformed query")
	}
}

// writeLogic renders a conjunction or disjunction, dropping members that produce
// no SQL.
func (b *builder) writeLogic(n *astNode) (bool, *Error) {
	sep := " AND "
	if n.kind == astOr {
		sep = " OR "
	}
	wrote := false
	for i := 0; i < n.kidCount(); i++ {
		mark := len(b.buf)
		if wrote {
			b.buf = append(b.buf, sep...)
		}
		b.buf = append(b.buf, '(')
		ok, err := b.write(n.kidAt(i))
		if err != nil {
			return false, err
		}
		if !ok || len(b.buf) == mark+1 {
			// The member produced nothing: drop the separator and parentheses.
			b.buf = b.buf[:mark]
			continue
		}
		b.buf = append(b.buf, ')')
		wrote = true
	}
	return wrote, nil
}

// writeNot renders a negation.
func (b *builder) writeNot(n *astNode) (bool, *Error) {
	mark := len(b.buf)
	b.buf = append(b.buf, "NOT ("...)
	ok, err := b.write(n.kidAt(0))
	if err != nil {
		return false, err
	}
	if !ok {
		b.buf = b.buf[:mark]
		return false, nil
	}
	b.buf = append(b.buf, ')')
	return true, nil
}

// appendColumn appends the SQL column expression for a node to the SQL buffer.
//
// The common case (a plain configured column) is written straight into the
// buffer, which avoids materialising a string for every predicate; nested paths
// and casts go through the renderer.
func (b *builder) appendColumn(n *astNode, opName string, wantText bool) *Error {
	parts := n.field.columnFor(opName)
	if b.standardQuote && len(parts) == 1 && len(n.path) == 0 && !strings.ContainsRune(parts[0], '"') {
		b.buf = append(b.buf, '"')
		b.buf = append(b.buf, parts[0]...)
		b.buf = append(b.buf, '"')
		return nil
	}
	col, err := b.r.column(n.field, opName, n.path, wantText, b.castType(n))
	if err != nil {
		return err
	}
	b.buf = append(b.buf, col...)
	return nil
}

// castType returns the field type whose SQL cast applies to text extracted from a
// JSONB path. Untyped (json) leaves take their type from the compared value, so
// config/age=42 casts the extracted text to int8 and compares with an int64.
func (b *builder) castType(n *astNode) FieldType {
	if n.field.typ != TypeJSON {
		return n.field.typ
	}
	switch n.val.Kind {
	case KindArray:
		return TypeString
	default:
		return valueType(n.val)
	}
}

// writeBuiltin renders a built-in predicate.
func (b *builder) writeBuiltin(n *astNode) (bool, *Error) {
	if n.op.isComparison() {
		return b.writeComparison(n)
	}
	opName := n.op.name()
	switch n.op {
	case opIn, opOut:
		op := ArrayIn
		if n.op == opOut {
			op = ArrayNotIn
		}
		if err := b.appendArrayPredicate(n, opName, true, op, n.val.Array); err != nil {
			return false, err
		}
	case opContainsAll, opOverlaps:
		op := ArrayContainsAll
		if n.op == opOverlaps {
			op = ArrayOverlaps
		}
		if err := b.appendArrayPredicate(n, opName, false, op, valuesOrSingle(n.val)); err != nil {
			return false, err
		}
	case opContains, opExcludes:
		if n.op == opExcludes {
			b.buf = append(b.buf, "NOT ("...)
		}
		if err := b.writeContains(n, opName); err != nil {
			return false, err
		}
		if n.op == opExcludes {
			b.buf = append(b.buf, ')')
		}
	default:
		return false, errAt(ErrUnsupportedOperator, n.pos, "unsupported operator "+quote(n.op.String()))
	}
	return true, nil
}

// valuesOrSingle returns the elements of an array value, or the value itself as
// a one-element list.
func valuesOrSingle(v Value) []Value {
	if v.Kind == KindArray {
		return v.Array
	}
	return []Value{v}
}

// writeComparison renders a scalar comparison.
func (b *builder) writeComparison(n *astNode) (bool, *Error) {
	opName := n.op.name()
	val := n.val
	if val.Kind == KindNull && n.op != opEq && n.op != opNe {
		return false, errAt(ErrUnsupportedOperator, n.pos,
			"operator "+quote(n.op.String())+" does not support null")
	}
	if err := b.appendColumn(n, opName, true); err != nil {
		return false, err
	}
	switch {
	case val.Kind == KindNull:
		if n.op == opEq {
			b.buf = append(b.buf, " IS NULL"...)
		} else {
			b.buf = append(b.buf, " IS NOT NULL"...)
		}
	case val.Kind == KindBool && (n.op == opEq || n.op == opNe):
		// TRUE and FALSE are the only inlined literals: they have a fixed
		// spelling and cannot carry user input.
		b.buf = append(b.buf, ' ')
		b.buf = append(b.buf, n.op.sqlOperator()...)
		b.buf = append(b.buf, ' ')
		if val.Bool {
			b.buf = append(b.buf, "TRUE"...)
		} else {
			b.buf = append(b.buf, "FALSE"...)
		}
	default:
		b.buf = append(b.buf, ' ')
		b.buf = append(b.buf, n.op.sqlOperator()...)
		b.buf = append(b.buf, ' ')
		b.bind(applyLikeWildcard(n.op, val))
	}
	return true, nil
}

// applyLikeWildcard translates the RQL wildcard '*' into SQL's '%' for LIKE and ILIKE.
//
// '*' is the conventional RQL wildcard, while SQL spells it '%'. Passing it through unchanged
// would match a literal asterisk and silently return no rows, so the translation is part of the
// operator contract; '*' stays literal for every other operator.
// A value without '*' is returned by value, so the common path does not allocate.
func applyLikeWildcard(op builtinOp, v Value) Value {
	if op != opLike && op != opILike {
		return v
	}
	if v.Kind != KindString || !strings.Contains(v.Str, "*") {
		return v
	}
	return Value{Kind: KindString, Str: strings.ReplaceAll(v.Str, "*", "%")}
}

// writeContains renders the "contains any" form: a single value compares against
// every element of the column with "= ANY(col)", which is the specification's
// meaning; several values become an overlap test.
func (b *builder) writeContains(n *astNode, opName string) *Error {
	vals := valuesOrSingle(n.val)
	if len(vals) == 1 {
		b.bind(vals[0])
		b.buf = append(b.buf, " = ANY("...)
		if err := b.appendColumn(n, opName, false); err != nil {
			return err
		}
		b.buf = append(b.buf, ')')
		return nil
	}
	return b.appendArrayPredicate(n, opName, false, ArrayOverlaps, vals)
}

// bind appends a value to the argument list and writes its placeholder.
func (b *builder) bind(v Value) {
	b.args = append(b.args, v.Any())
	b.writePlaceholder()
}

// writePlaceholder writes the next placeholder into the SQL buffer.
func (b *builder) writePlaceholder() {
	b.n++
	if b.pg {
		b.buf = append(b.buf, '$')
		b.buf = strconv.AppendInt(b.buf, int64(b.n), 10)
		return
	}
	b.buf = append(b.buf, b.d.Placeholder(b.n)...)
}

// placeholderText returns the placeholder for the next parameter as a string,
// for operators that assemble their own SQL.
func (b *builder) placeholderText() string {
	b.n++
	if b.pg {
		return "$" + strconv.Itoa(b.n)
	}
	return b.d.Placeholder(b.n)
}

// appendArrayPredicate appends an array predicate over vals, binding the
// parameters.
//
// Built-in dialects render the predicate straight into the buffer, so the column
// is appended directly and nothing is materialised as a string. Other dialects go
// through Dialect.BuildArrayOp, which needs the column and the placeholder list
// as strings.
func (b *builder) appendArrayPredicate(n *astNode, opName string, wantText bool, op ArrayOp, vals []Value) *Error {
	if app, ok := b.d.(arrayAppender); ok {
		if err := b.appendColumn(n, opName, wantText); err != nil {
			return err
		}
		if b.slice {
			var arg any
			if len(vals) == 1 {
				arg = vals[0].Any()
			} else {
				arg = Value{Kind: KindArray, Array: vals}.Any()
			}
			b.args = append(b.args, arg)
			b.n++
			b.buf = app.AppendArrayOp(b.buf, op, "", b.n, 1)
			return nil
		}
		first := b.n + 1
		for i := range vals {
			b.args = append(b.args, vals[i].Any())
		}
		b.n += len(vals)
		b.buf = app.AppendArrayOp(b.buf, op, "", first, len(vals))
		return nil
	}
	col, err := b.r.column(n.field, opName, n.path, wantText, b.castType(n))
	if err != nil {
		return err
	}
	b.buf = append(b.buf, b.arrayPredicate(op, col, vals)...)
	return nil
}

// arrayPredicate renders an array predicate for dialects without the
// allocation-free path.
func (b *builder) arrayPredicate(op ArrayOp, col string, vals []Value) string {
	ph := make([]byte, 0, 8*len(vals))
	if b.slice {
		var arg any
		if len(vals) == 1 {
			arg = vals[0].Any()
		} else {
			arg = Value{Kind: KindArray, Array: vals}.Any()
		}
		b.args = append(b.args, arg)
		ph = b.appendPlaceholderText(ph, b.n+1)
		b.n++
		return b.d.BuildArrayOp(op, col, string(ph))
	}
	first := b.n + 1
	for i := range vals {
		if i > 0 {
			ph = append(ph, ',', ' ')
		}
		b.args = append(b.args, vals[i].Any())
		ph = b.appendPlaceholderText(ph, first+i)
	}
	b.n += len(vals)
	return b.d.BuildArrayOp(op, col, string(ph))
}

// appendPlaceholderText appends the dialect's placeholder for parameter n.
func (b *builder) appendPlaceholderText(dst []byte, n int) []byte {
	if b.pg {
		dst = append(dst, '$')
		return strconv.AppendInt(dst, int64(n), 10)
	}
	return append(dst, b.d.Placeholder(n)...)
}

// writeCustom renders a registered operator by calling it.
func (b *builder) writeCustom(n *astNode) (bool, *Error) {
	opCtx := &OpContext{
		b:         b,
		op:        n.custom.name,
		kind:      n.custom.kind,
		field:     n.field,
		fieldName: n.fieldName,
		values:    n.args,
		args:      operatorArgs(n),
		pos:       n.pos,
	}
	before := len(b.args)
	sql, err := n.custom.fn(opCtx, opCtx.args)
	if err != nil {
		var rqlErr *Error
		if errors.As(err, &rqlErr) {
			return false, rqlErr
		}
		return false, errCause(nil, n.pos, "operator "+quote(n.custom.name)+": "+err.Error(), err)
	}
	if sql == "" {
		if len(b.args) != before {
			return false, errAt(ErrInvalidSyntax, n.pos,
				"operator "+quote(n.custom.name)+" bound arguments but produced no SQL")
		}
		return false, nil
	}
	b.buf = append(b.buf, sql...)
	return true, nil
}

// operatorArgs materialises the argument list handed to a custom operator: the
// field name first for the infix form, then the values.
func operatorArgs(n *astNode) []any {
	return operatorArgsFor(n.fieldName, n.args)
}

// contextError wraps a context error so that errors.Is still matches it.
func contextError(err error, pos int) *Error {
	return &Error{Msg: "translation cancelled: " + err.Error(), Pos: pos, causedBy: err}
}
