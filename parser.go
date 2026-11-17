package rql

import (
	"context"
	"strings"
)

// parser is a recursive-descent parser over the token stream.
//
// The grammar is:
//
//	query      = group EOF
//	group      = expression (('&' | ',') expression)*     -- homogeneous, see below
//	           | expression ('|' expression)*
//	expression = '(' group ')'                            -- grouping
//	           | '(' prop (',' prop)+ ')' afterField      -- array-of-properties
//	           | op '(' args ')'                          -- normalized form
//	           | prop afterField                          -- sugar and FIQL
//	afterField = '=' value
//	           | '=' op '=' value
//	           | cmp value                                -- bare FIQL comparison
//	value      = literal | '(' literal (',' literal)* ')'
//
// A group may use '&' (or ',') or '|' but not both: mixing is rejected with
// ErrMixedConjunctions rather than being given an implementation-defined
// precedence.
type parser struct {
	cfg     *Config
	src     string
	ctx     context.Context
	r       renderer
	sc      scanner
	tok     Token
	pk      Token
	hasPk   bool
	scanErr *Error
	nodes   []astNode
	res     Result

	// nodeArr backs nodes for the first maxInlineNodes nodes, so the arena needs
	// no allocation for typical queries.
	nodeArr [maxInlineNodes]astNode
	// valArr backs the elements of small array literals, which would otherwise
	// have to escape from the parser.
	valArr [maxInlineValues]Value
	valTop int
}

// maxInlineValues is how many array-literal elements fit without allocating.
const maxInlineValues = 8

// allocValues reserves n values in the parser's value arena.
func (p *parser) allocValues(n int) []Value {
	if n <= 0 {
		return nil
	}
	if p.valTop+n <= len(p.valArr) {
		out := p.valArr[p.valTop : p.valTop+n]
		p.valTop += n
		return out
	}
	return make([]Value, n)
}

// parserState is a full parser snapshot, used for the one ambiguous grammar
// position: '(' starts either a grouped expression, an array-of-properties
// field, or (in value position) an array literal.
type parserState struct {
	tok      Token
	pk       Token
	hasPk    bool
	pos      int
	tokens   int
	scanErr  *Error
	nodesLen int
}

// maxInlineNodes is how many AST nodes fit in the parser without allocating.
const maxInlineNodes = 5

// init prepares the parser for src.
func (p *parser) init(ctx context.Context, cfg *Config, src string, d Dialect) {
	p.cfg = cfg
	p.src = src
	p.ctx = ctx
	p.r = renderer{d: d}
	p.sc = newScanner(src, cfg.maxTokens())
	p.nodes = p.nodeArr[:0]
	p.res = Result{d: d}
	p.advance()
}

// advance consumes the current token.
func (p *parser) advance() {
	if p.hasPk {
		p.tok = p.pk
		p.hasPk = false
		return
	}
	p.tok = p.scanToken()
}

// peek returns the token after the current one without consuming it.
func (p *parser) peek() Token {
	if !p.hasPk {
		p.pk = p.scanToken()
		p.hasPk = true
	}
	return p.pk
}

// scanToken scans the next token, recording the first diagnostic error.
func (p *parser) scanToken() Token {
	if p.scanErr != nil {
		return Token{Kind: tokEOF, Start: len(p.src), End: len(p.src)}
	}
	t, err := p.sc.next()
	if err != nil {
		p.scanErr = err
		return Token{Kind: tokEOF, Start: len(p.src), End: len(p.src)}
	}
	return t
}

// checkErr reports the pending scan error, if any.
func (p *parser) checkErr() *Error { return p.scanErr }

// save snapshots the parser state.
func (p *parser) save() parserState {
	return parserState{
		tok: p.tok, pk: p.pk, hasPk: p.hasPk,
		pos: p.sc.pos, tokens: p.sc.tokens,
		scanErr: p.scanErr, nodesLen: len(p.nodes),
	}
}

// restore resets the parser state.
func (p *parser) restore(s parserState) {
	p.tok, p.pk, p.hasPk = s.tok, s.pk, s.hasPk
	p.sc.pos, p.sc.tokens = s.pos, s.tokens
	p.scanErr = s.scanErr
	p.nodes = p.nodes[:s.nodesLen]
}

// describe renders a token for an error message.
func (p *parser) describe(t Token) string {
	if t.Kind == tokWord {
		return quote(decode(p.src, t, false))
	}
	return t.Kind.String()
}

// unexpected builds an "expected X, found Y" error at the current token.
func (p *parser) unexpected(expected string) *Error {
	return errAt(ErrInvalidSyntax, p.tok.Start, "expected "+expected+", found "+p.describe(p.tok))
}

// ctxErr converts a context error for propagation, preserving errors.Is.
func (p *parser) ctxErr(err error) *Error {
	return errCause(ErrInvalidSyntax, -1, "translation cancelled", err)
}

// wordText decodes the current token as text.
func (p *parser) wordText(t Token, plusAsSpace bool) string {
	return decode(p.src, t, plusAsSpace)
}

// parse translates the whole query.
func (p *parser) parse() (astIndex, *Error) {
	if err := p.checkErr(); err != nil {
		return noNode, err
	}
	if p.tok.Kind == tokEOF {
		return noNode, nil
	}
	root, err := p.parseGroup(0)
	if err != nil {
		return noNode, err
	}
	if err := p.checkErr(); err != nil {
		return noNode, err
	}
	if p.tok.Kind != tokEOF {
		return noNode, p.unexpected("end of query")
	}
	return root, nil
}

// parseGroup parses a homogeneous conjunction or disjunction.
func (p *parser) parseGroup(depth int) (astIndex, *Error) {
	if depth > p.cfg.maxDepth() {
		return noNode, errAt(ErrMaxDepth, p.tok.Start, "query nests deeper than the configured maximum ("+itoa(p.cfg.maxDepth())+")")
	}
	kind := astKind(0)
	haveKind := false
	var inline [maxInlineKids]astIndex
	kids := inline[:0]
	for {
		if err := p.ctx.Err(); err != nil {
			return noNode, p.ctxErr(err)
		}
		if err := p.checkErr(); err != nil {
			return noNode, err
		}
		expr, err := p.parseExpression(depth)
		if err != nil {
			return noNode, err
		}
		if expr != noNode {
			kids = append(kids, expr)
		}
		next, ok, err := p.groupSeparator(kind, haveKind)
		if err != nil {
			return noNode, err
		}
		if !ok {
			break
		}
		kind, haveKind = next, true
		p.advance()
	}
	switch len(kids) {
	case 0:
		return noNode, nil
	case 1:
		return kids[0], nil
	}
	node := p.addNode(astNode{kind: kind, pos: p.nodes[kids[0]].pos})
	for _, kid := range kids {
		p.addKid(node, kid)
	}
	return node, nil
}

// groupSeparator classifies the conjunction token between group members and
// enforces that a group does not mix '&' with '|'.
func (p *parser) groupSeparator(kind astKind, haveKind bool) (astKind, bool, *Error) {
	var next astKind
	switch p.tok.Kind {
	case tokAnd, tokComma:
		next = astAnd
	case tokOr:
		next = astOr
	default:
		return 0, false, nil
	}
	if haveKind && next != kind {
		return 0, false, errAt(ErrMixedConjunctions, p.tok.Start,
			"cannot mix '&' (or ',') with '|' in one group; parenthesise each group")
	}
	return next, true, nil
}

// parseExpression parses one filter expression or shaping operator.
func (p *parser) parseExpression(depth int) (astIndex, *Error) {
	if err := p.checkErr(); err != nil {
		return noNode, err
	}
	if depth > p.cfg.maxDepth() {
		return noNode, errAt(ErrMaxDepth, p.tok.Start,
			"query nests deeper than the configured maximum ("+itoa(p.cfg.maxDepth())+")")
	}
	switch p.tok.Kind {
	case tokLParen:
		return p.parseParen(depth)
	case tokWord:
		return p.parseWord(depth)
	default:
		return noNode, p.unexpected("an expression")
	}
}

// parseParen parses either a grouped expression or an array-of-properties field
// such as (foo,bar)=3.
func (p *parser) parseParen(depth int) (astIndex, *Error) {
	pos := p.tok.Start
	f, path, ok, err := p.tryPropertyPath()
	if err != nil {
		return noNode, err
	}
	if ok {
		return p.parseAfterField(f, path, pos)
	}
	p.advance() // '('
	inner, err := p.parseGroup(depth + 1)
	if err != nil {
		return noNode, err
	}
	if err := p.checkErr(); err != nil {
		return noNode, err
	}
	if p.tok.Kind != tokRParen {
		return noNode, p.unexpected("')'")
	}
	p.advance()
	return inner, nil
}

// tryPropertyPath attempts the array-of-properties form. It restores the parser
// state and reports ok=false when the shape does not match.
func (p *parser) tryPropertyPath() (f *field, path []string, ok bool, err *Error) {
	pos := p.tok.Start
	saved := p.save()
	p.advance() // '('
	var names [maxInlineKids + 1]string
	n := 0
	for {
		if p.tok.Kind != tokWord || n == len(names) {
			p.restore(saved)
			return nil, nil, false, nil
		}
		names[n] = p.wordText(p.tok, false)
		n++
		p.advance()
		if p.tok.Kind == tokComma {
			p.advance()
			continue
		}
		if p.tok.Kind == tokRParen {
			p.advance()
			break
		}
		p.restore(saved)
		return nil, nil, false, nil
	}
	if err := p.checkErr(); err != nil {
		return nil, nil, false, err
	}
	if p.tok.Kind != tokEq && !p.tok.isComparison() {
		p.restore(saved)
		return nil, nil, false, nil
	}
	segs := names[:n]
	root := segs[0]
	f = p.cfg.lookupField(root)
	if f == nil {
		// A leaf registered with dots may be addressed with slashes.
		f = p.cfg.lookupField(strings.Join(segs, "."))
		if f == nil {
			return nil, nil, false, errAt(ErrUnknownField, pos, "unknown field "+quote(root))
		}
		return f, nil, true, nil
	}
	if f.regErr != nil {
		return nil, nil, false, f.regErr
	}
	if len(segs) == 1 {
		return f, nil, true, nil
	}
	if !f.dynamic {
		return nil, nil, false, errAt(ErrInvalidPath, pos,
			"field "+quote(root)+" is not a JSONB path root; register the leaf or mark it dynamic")
	}
	path = make([]string, 0, len(segs)-1)
	for _, seg := range segs[1:] {
		if !isPathSegment(seg) {
			return nil, nil, false, errAt(ErrInvalidPath, pos, "invalid path segment "+quote(seg))
		}
		path = append(path, seg)
	}
	return f, path, true, nil
}

// parseWord parses a normalized call, a bare custom operator, or a field.
func (p *parser) parseWord(depth int) (astIndex, *Error) {
	wtok := p.tok
	name := p.wordText(wtok, false)
	if p.peek().Kind == tokLParen {
		return p.parseCall(name, wtok.Start, depth)
	}
	if p.cfg.lookupField(name) == nil {
		if cop := p.cfg.lookupCustom(strings.ToLower(name)); cop != nil && cop.kind == BinaryOp {
			return p.parseBareCustomOp(cop, wtok.Start)
		}
	}
	f, path, err := p.resolveField(name, wtok.Start)
	if err != nil {
		return noNode, err
	}
	p.advance()
	return p.parseAfterField(f, path, wtok.Start)
}

// resolveField resolves a field name, which may address a JSONB path.
//
// The RQL spellings are unified: an exact registered name wins, then a
// slashed or dotted name is split into a registered root plus path segments, and
// finally a slashed name may also address a leaf registered with dots (so
// "a/b" and "a.b" are interchangeable and (a,b)=v matches both).
func (p *parser) resolveField(name string, pos int) (*field, []string, *Error) {
	if p.isParamRef(name) {
		return nil, nil, errAt(ErrUnsupportedOperator, pos,
			"parameter reference "+quote(name)+" is not supported; values must be inlined")
	}
	if err := p.rejectConverterField(name, pos); err != nil {
		return nil, nil, err
	}
	if f := p.cfg.lookupField(name); f != nil {
		if f.regErr != nil {
			return nil, nil, f.regErr
		}
		return f, nil, nil
	}
	segs := splitPath(name)
	root, ok := pathRoot(segs)
	if !ok {
		return nil, nil, errAt(ErrInvalidPath, pos, "path "+quote(name)+" has an empty root segment")
	}
	if len(segs) < 2 {
		return nil, nil, errAt(ErrUnknownField, pos, "unknown field "+quote(name))
	}
	if f := p.cfg.lookupField(root); f != nil {
		if f.regErr != nil {
			return nil, nil, f.regErr
		}
		if !f.dynamic {
			// A plain field is a column, not a json container: reject the extra
			// segments instead of emitting an extraction from a non-json column.
			return nil, nil, errAt(ErrInvalidPath, pos,
				"field "+quote(f.name)+" is not a JSONB path root; register "+
					quote(name)+" or mark "+quote(f.name)+" as dynamic")
		}
		path := make([]string, 0, len(segs)-1)
		for _, seg := range segs[1:] {
			if !isPathSegment(seg) {
				return nil, nil, errAt(ErrInvalidPath, pos, "invalid path segment "+quote(seg)+" in field "+quote(name))
			}
			path = append(path, seg)
		}
		return f, path, nil
	}
	if joined := strings.Join(segs, "."); joined != name {
		if f := p.cfg.lookupField(joined); f != nil {
			if f.regErr != nil {
				return nil, nil, f.regErr
			}
			return f, nil, nil
		}
	}
	return nil, nil, errAt(ErrUnknownField, pos, "unknown field "+quote(name))
}

// isParamRef reports whether name is a $n parameter reference.
func (p *parser) isParamRef(name string) bool {
	if len(name) < 2 || name[0] != '$' {
		return false
	}
	for i := 1; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}

// rejectConverterField reports a converter prefix written as a field, e.g.
// re:pattern, which means the author expected a regexp value.
func (p *parser) rejectConverterField(name string, pos int) *Error {
	idx := converterSplit(name)
	if idx <= 0 {
		return nil
	}
	return errAt(ErrUnsupportedConverter, pos,
		"converter "+quote(name[:idx])+" is not supported in field position")
}

// parseAfterField parses the operator part following a field.
func (p *parser) parseAfterField(f *field, path []string, pos int) (astIndex, *Error) {
	switch {
	case p.tok.Kind == tokEq:
		p.advance()
		return p.parseSugar(f, path, pos)
	case p.tok.isComparison():
		op := comparisonFromToken(p.tok.Kind)
		p.advance()
		val, err := p.parseValueToken(false)
		if err != nil {
			return noNode, err
		}
		return p.buildWithOp(f, path, op.name(), val, pos)
	default:
		return noNode, p.unexpected("'=' after field " + quote(f.name))
	}
}

// parseSugar parses the right-hand side of `field=`, which may hold a value, a
// FIQL operator, or a percent-encoded comparison operator.
func (p *parser) parseSugar(f *field, path []string, pos int) (astIndex, *Error) {
	if p.tok.isComparison() {
		op := comparisonFromToken(p.tok.Kind)
		p.advance()
		val, err := p.parseValueToken(true)
		if err != nil {
			return noNode, err
		}
		return p.buildWithOp(f, path, op.name(), val, pos)
	}
	if p.tok.Kind != tokWord {
		val, err := p.parseValueToken(true)
		if err != nil {
			return noNode, err
		}
		return p.buildWithOp(f, path, "eq", val, pos)
	}
	text := p.wordText(p.tok, true)
	if op, rest, ok := splitComparisonPrefix(text); ok {
		return p.parseEncodedComparison(f, path, op, rest, pos)
	}
	if p.peek().Kind == tokEq && !isExplicitValue(text) {
		p.advance() // operator name
		p.advance() // '='
		return p.parseNamedOp(f, path, strings.ToLower(text), pos)
	}
	p.advance()
	val, err := parseLiteral(text, pos)
	if err != nil {
		return noNode, err
	}
	// Plain equality is routed through the remapping-aware builder so that a
	// field which maps eq onto another operator (a virtual field such as
	// createdAtFrom -> createdAt >= v) behaves the same in sugar and FIQL form.
	return p.buildWithOp(f, path, "eq", val, pos)
}

// isExplicitValue reports whether text is already an explicit value form (a
// converter prefix or a quoted string) and must not be read as an operator name.
func isExplicitValue(text string) bool {
	if text == "" {
		return false
	}
	if text[0] == '\'' {
		return true
	}
	return converterSplit(text) > 0
}

// splitComparisonPrefix splits a percent-encoded comparison operator from the
// value that follows it, e.g. "<10" into lt and "10".
func splitComparisonPrefix(text string) (builtinOp, string, bool) {
	switch {
	case strings.HasPrefix(text, "!="):
		return opNe, text[2:], true
	case strings.HasPrefix(text, ">="):
		return opGe, text[2:], true
	case strings.HasPrefix(text, "<="):
		return opLe, text[2:], true
	case strings.HasPrefix(text, ">"):
		return opGt, text[1:], true
	case strings.HasPrefix(text, "<"):
		return opLt, text[1:], true
	case strings.HasPrefix(text, "!"):
		return opNe, text[1:], true
	default:
		return opEq, text, false
	}
}

// parseEncodedComparison finishes a comparison whose operator arrived percent
// encoded, e.g. price=%3C10 or price=%3C=10.
func (p *parser) parseEncodedComparison(f *field, path []string, op builtinOp, rest string, pos int) (astIndex, *Error) {
	p.advance() // the encoded operator token
	var (
		val Value
		err *Error
	)
	if rest == "" {
		// The operator was written on its own, e.g. %3C=10: the '=' separates the
		// two-character operator from the value.
		if p.tok.Kind == tokEq {
			// The '=' completes the two-character operator: %3C= is <= and %3E=
			// is >=. Other prefixes keep their meaning.
			switch op {
			case opLt:
				op = opLe
			case opGt:
				op = opGe
			case opEq, opNe, opGe, opLe, opLike, opILike, opIn, opOut,
				opContains, opContainsAll, opOverlaps, opExcludes, numBuiltinOps:
			}
			p.advance()
		}
		val, err = p.parseValueToken(true)
	} else {
		val, err = parseLiteral(rest, pos)
	}
	if err != nil {
		return noNode, err
	}
	return p.buildWithOp(f, path, op.name(), val, pos)
}

// parseNamedOp parses `field=op=value` for built-in and registered operators.
func (p *parser) parseNamedOp(f *field, path []string, name string, pos int) (astIndex, *Error) {
	target := f.remap(name)
	if cop := p.cfg.lookupCustom(target); cop != nil {
		if !f.allows(target) {
			return noNode, p.operatorNotAllowed(target, f.name, pos)
		}
		vals, err := p.parseCustomArgs()
		if err != nil {
			return noNode, err
		}
		return p.addNode(astNode{
			kind: astCustom, custom: cop, field: f, fieldName: f.name,
			path: path, args: vals, pos: pos,
		}), nil
	}
	op, ok := builtinByName[target]
	if !ok {
		return noNode, errAt(ErrUnsupportedOperator, pos, "unsupported operator "+quote(target)+" for field "+quote(f.name))
	}
	if !f.allows(target) {
		return noNode, p.operatorNotAllowed(target, f.name, pos)
	}
	val, err := p.parseOperatorValue(op, f, pos, true)
	if err != nil {
		return noNode, err
	}
	return p.buildPredicate(f, path, op, val, pos)
}

// operatorNotAllowed builds the rejection for an operator disabled on a field.
func (p *parser) operatorNotAllowed(op, name string, pos int) *Error {
	return errAt(ErrOperatorNotAllowed, pos,
		"operator "+quote(op)+" is not allowed for field "+quote(name))
}

// parseOperatorValue parses the value side of an operator, which for container
// operators may be an array literal.
func (p *parser) parseOperatorValue(op builtinOp, f *field, pos int, allowEmpty bool) (Value, *Error) {
	switch op {
	case opIn, opOut, opOverlaps:
		if p.tok.Kind == tokLParen {
			return p.parseArrayLiteral()
		}
		return Value{}, errAt(ErrInvalidValue, pos,
			"operator "+quote(op.String())+" requires an array value, e.g. in(field,(a,b))")
	case opContainsAll:
		if p.tok.Kind == tokLParen {
			return p.parseArrayLiteral()
		}
		val, err := p.parseValueToken(allowEmpty)
		if err != nil {
			return Value{}, err
		}
		if f.typ == TypeJSON || val.Kind == KindArray {
			return val, nil
		}
		return Value{Kind: KindArray, Array: []Value{val}}, nil
	default:
		return p.parseValueToken(allowEmpty)
	}
}

// parseValueToken parses one value: a literal or an array literal.
func (p *parser) parseValueToken(allowEmpty bool) (Value, *Error) {
	if err := p.checkErr(); err != nil {
		return Value{}, err
	}
	switch {
	case p.tok.isTerminator():
		if allowEmpty {
			return Value{Kind: KindString}, nil
		}
		return Value{}, p.unexpected("a value")
	case p.tok.Kind == tokLParen:
		return p.parseArrayLiteral()
	case p.tok.Kind == tokWord:
		if p.peek().Kind == tokLParen {
			return Value{}, errAt(ErrUnsupportedOperator, p.tok.Start,
				"expression arguments are not supported; use a literal value")
		}
		pos := p.tok.Start
		text := p.wordText(p.tok, true)
		p.advance()
		return parseLiteral(text, pos)
	default:
		return Value{}, p.unexpected("a value")
	}
}

// parseArrayLiteral parses a parenthesised value list.
func (p *parser) parseArrayLiteral() (Value, *Error) {
	p.advance() // '('
	if p.tok.Kind == tokRParen {
		p.advance()
		return Value{Kind: KindArray}, nil
	}
	var inline [maxInlineKids]Value
	vals := inline[:0]
	for {
		if p.tok.Kind == tokLParen {
			nested, err := p.parseArrayLiteral()
			if err != nil {
				return Value{}, err
			}
			vals = append(vals, nested)
		} else {
			val, err := p.parseValueToken(false)
			if err != nil {
				return Value{}, err
			}
			vals = append(vals, val)
		}
		switch p.tok.Kind {
		case tokComma:
			p.advance()
		case tokRParen:
			p.advance()
			out := p.allocValues(len(vals))
			copy(out, vals)
			return Value{Kind: KindArray, Array: out}, nil
		default:
			return Value{}, p.unexpected("',' or ')' in array")
		}
	}
}

// parseCall parses a normalized operator call, e.g. eq(a,b) or and(...).
func (p *parser) parseCall(name string, pos, depth int) (astIndex, *Error) {
	p.advance() // operator name
	if p.tok.Kind != tokLParen {
		return noNode, p.unexpected("'(' after operator " + quote(name))
	}
	p.advance() // '('
	lower := strings.ToLower(name)
	switch lower {
	case "and":
		return p.parseLogicCall(astAnd, depth, pos)
	case "or":
		return p.parseLogicCall(astOr, depth, pos)
	case "not":
		return p.parseNotCall(depth, pos)
	case "sort":
		return p.parseSortCall(pos)
	case "limit":
		return p.parseLimitCall(pos)
	case "select":
		return p.parseSelectCall(pos)
	case "distinct":
		return p.parseNoArgFlagCall(pos, true)
	case "count":
		return p.parseNoArgFlagCall(pos, false)
	}
	if cop := p.cfg.lookupCustom(lower); cop != nil {
		vals, err := p.parseCustomCallArgs()
		if err != nil {
			return noNode, err
		}
		return p.addNode(astNode{kind: astCustom, custom: cop, args: vals, pos: pos}), nil
	}
	if op, ok := builtinByName[lower]; ok {
		return p.parseBuiltinCall(op, pos)
	}
	return noNode, errAt(ErrUnsupportedOperator, pos, "unsupported operator "+quote(name))
}

// parseLogicCall parses and(...) / or(...).
func (p *parser) parseLogicCall(kind astKind, depth, pos int) (astIndex, *Error) {
	if p.tok.Kind == tokRParen {
		return noNode, errAt(ErrInvalidSyntax, pos, "operator needs at least one argument")
	}
	node := p.addNode(astNode{kind: kind, pos: pos})
	for {
		child, err := p.parseExpression(depth + 1)
		if err != nil {
			return noNode, err
		}
		if child != noNode {
			p.addKid(node, child)
		}
		switch p.tok.Kind {
		case tokComma:
			p.advance()
		case tokRParen:
			p.advance()
			kids := p.nodes[node].kidCount()
			if kids == 0 {
				return noNode, nil
			}
			if kids == 1 {
				return p.nodes[node].kidAt(0), nil
			}
			return node, nil
		default:
			return noNode, p.unexpected("',' or ')'")
		}
	}
}

// parseNotCall parses not(expression). Its argument is a whole group, so
// not(a=1&b=2) is accepted and negated through De Morgan's laws.
func (p *parser) parseNotCall(depth, pos int) (astIndex, *Error) {
	child, err := p.parseGroup(depth + 1)
	if err != nil {
		return noNode, err
	}
	if p.tok.Kind != tokRParen {
		return noNode, p.unexpected("')'")
	}
	p.advance()
	if child == noNode {
		return noNode, nil
	}
	return p.negate(child, pos), nil
}

// parseBuiltinCall parses the normalized form of a built-in predicate, e.g.
// eq(foo,3), in(foo,(3,4)) or contains(tags,go).
func (p *parser) parseBuiltinCall(op builtinOp, pos int) (astIndex, *Error) {
	if p.tok.Kind != tokWord {
		return noNode, p.unexpected("a field name in " + op.String() + "()")
	}
	nameTok := p.tok
	f, path, err := p.resolveField(p.wordText(nameTok, false), nameTok.Start)
	if err != nil {
		return noNode, err
	}
	p.advance()
	if p.tok.Kind == tokRParen {
		return noNode, errAt(ErrInvalidSyntax, pos, "operator "+quote(op.String())+" requires 2 arguments")
	}
	if p.tok.Kind != tokComma {
		return noNode, p.unexpected("','")
	}
	p.advance()
	val, verr := p.parseOperatorValue(op, f, pos, false)
	if verr != nil {
		return noNode, verr
	}
	if p.tok.Kind != tokRParen {
		return noNode, p.unexpected("')'")
	}
	p.advance()
	return p.buildPredicate(f, path, op, val, pos)
}

// parseCustomCallArgs parses the argument list of a normalized custom call.
func (p *parser) parseCustomCallArgs() ([]Value, *Error) {
	if p.tok.Kind == tokRParen {
		p.advance()
		return nil, nil
	}
	var inline [maxInlineKids]Value
	vals := inline[:0]
	for {
		val, err := p.parseValueToken(false)
		if err != nil {
			return nil, err
		}
		vals = append(vals, val)
		switch p.tok.Kind {
		case tokComma:
			p.advance()
		case tokRParen:
			p.advance()
			return vals, nil
		default:
			return nil, p.unexpected("',' or ')'")
		}
	}
}

// parseCustomArgs parses the value arguments of a binary custom operator.
func (p *parser) parseCustomArgs() ([]Value, *Error) {
	val, err := p.parseValueToken(false)
	if err != nil {
		return nil, err
	}
	return []Value{val}, nil
}

// parseBareCustomOp parses `op=value` for a binary custom operator whose name is
// not a configured field (a flag-only operator such as showArchived=true).
func (p *parser) parseBareCustomOp(cop *customOp, pos int) (astIndex, *Error) {
	p.advance() // operator name
	if p.tok.Kind != tokEq {
		return noNode, p.unexpected("'=' after operator " + quote(cop.name))
	}
	p.advance()
	vals, err := p.parseCustomArgs()
	if err != nil {
		return noNode, err
	}
	return p.addNode(astNode{kind: astCustom, custom: cop, args: vals, pos: pos}), nil
}

// parseSortCall parses sort(+a,-b). The legacy two-argument form
// sort(field,asc|desc) is accepted as an extension.
func (p *parser) parseSortCall(pos int) (astIndex, *Error) {
	if p.res.Sort != nil {
		return noNode, errAt(ErrInvalidSyntax, pos, "sort() may only be used once")
	}
	if p.tok.Kind == tokRParen {
		p.advance()
		return noNode, nil
	}
	var inline [maxInlineKids]string
	args := inline[:0]
	for {
		if p.tok.Kind != tokWord {
			return noNode, p.unexpected("a field name in sort()")
		}
		args = append(args, p.wordText(p.tok, false))
		p.advance()
		switch p.tok.Kind {
		case tokComma:
			p.advance()
		case tokRParen:
			p.advance()
			fields, err := p.sortFields(args, pos)
			if err != nil {
				return noNode, err
			}
			p.res.Sort = fields
			return noNode, nil
		default:
			return noNode, p.unexpected("',' or ')'")
		}
	}
}

// sortFields resolves sort arguments, honouring the direction prefix and the
// legacy sort(field,asc|desc) form.
func (p *parser) sortFields(args []string, pos int) ([]SortField, *Error) {
	if len(args) == 2 {
		args = p.applySortDirection(args)
	}
	fields := make([]SortField, 0, len(args))
	for _, arg := range args {
		if arg == "" {
			continue
		}
		sortColumn, desc := trimSortDirection(arg)
		f, path, err := p.resolveField(sortColumn, pos)
		if err != nil {
			return nil, err
		}
		if !f.sortable {
			return nil, errAt(ErrNotSortable, pos, "field "+quote(f.name)+" is not sortable")
		}
		sf := SortField{Column: f.parts[0], Desc: desc}
		if len(f.parts) > 1 || len(path) > 0 {
			sf.Path = appendPath(f.parts[1:], path)
		}
		fields = append(fields, sf)
	}
	return fields, nil
}

// applySortDirection rewrites the legacy two-argument form into the prefixed one.
func (p *parser) applySortDirection(args []string) []string {
	direction := strings.ToLower(args[1])
	if direction != "asc" && direction != "desc" {
		return args
	}
	if p.cfg.lookupField(args[1]) != nil {
		return args
	}
	column, _ := trimSortDirection(args[0])
	prefix := "+"
	if direction == "desc" {
		prefix = "-"
	}
	out := make([]string, 2)
	out[0] = prefix + column
	out[1] = ""
	return out
}

// trimSortDirection strips the '+'/'-' direction prefix from a sort argument.
func trimSortDirection(arg string) (string, bool) {
	if arg == "" {
		return arg, false
	}
	switch arg[0] {
	case '-':
		return arg[1:], true
	case '+':
		return arg[1:], false
	default:
		return arg, false
	}
}

// appendPath concatenates the configured path tail with the query path.
func appendPath(prefix, path []string) []string {
	out := make([]string, 0, len(prefix)+len(path))
	out = append(out, prefix...)
	out = append(out, path...)
	return out
}

// parseLimitCall parses limit(count[,start[,maxCount]]).
func (p *parser) parseLimitCall(pos int) (astIndex, *Error) {
	if p.res.Limit != nil || p.res.Offset != nil {
		return noNode, errAt(ErrInvalidSyntax, pos, "limit() may only be used once")
	}
	var nums [3]int
	count := 0
	for {
		if p.tok.Kind == tokRParen {
			if count == 0 {
				return noNode, errAt(ErrInvalidSyntax, pos, "limit() requires an argument")
			}
			p.advance()
			break
		}
		if p.tok.Kind != tokWord {
			return noNode, p.unexpected("a non-negative integer in limit()")
		}
		text := p.wordText(p.tok, false)
		value, ok := parseNumberLiteral(text)
		if !ok || value.Kind != KindInt || value.Int < 0 || value.Int > maxLimitValue {
			return noNode, errAt(ErrInvalidValue, pos,
				"limit() arguments must be non-negative integers, got "+quote(text))
		}
		if count == len(nums) {
			return noNode, errAt(ErrInvalidSyntax, pos, "limit() takes at most 3 arguments")
		}
		nums[count] = int(value.Int)
		count++
		p.advance()
		if p.tok.Kind == tokComma {
			p.advance()
			continue
		}
	}
	limit := nums[0]
	if count > 2 && nums[2] > 0 && limit > nums[2] {
		limit = nums[2]
	}
	p.res.Limit = &limit
	if count > 1 {
		offset := nums[1]
		p.res.Offset = &offset
	}
	return noNode, nil
}

// maxLimitValue bounds limit()/offset() arguments so that they convert to int
// without truncation on any platform.
const maxLimitValue = 1<<31 - 1

// parseSelectCall parses select(field,...).
func (p *parser) parseSelectCall(pos int) (astIndex, *Error) {
	if p.res.Select != nil {
		return noNode, errAt(ErrInvalidSyntax, pos, "select() may only be used once")
	}
	if p.tok.Kind == tokRParen {
		return noNode, errAt(ErrInvalidSyntax, pos, "select() requires at least one field")
	}
	cols := make([]string, 0, maxInlineKids)
	for {
		if p.tok.Kind != tokWord {
			return noNode, p.unexpected("a field name in select()")
		}
		tok := p.tok
		f, path, err := p.resolveField(p.wordText(tok, false), tok.Start)
		if err != nil {
			return noNode, err
		}
		if !f.selectable {
			return noNode, errAt(ErrNotSelectable, tok.Start, "field "+quote(f.name)+" is not selectable")
		}
		col, cerr := p.r.column(f, "select", path, true, f.typ)
		if cerr != nil {
			return noNode, cerr
		}
		cols = append(cols, col)
		p.advance()
		switch p.tok.Kind {
		case tokComma:
			p.advance()
		case tokRParen:
			p.advance()
			p.res.Select = cols
			return noNode, nil
		default:
			return noNode, p.unexpected("',' or ')'")
		}
	}
}

// parseNoArgFlagCall parses distinct() and count().
func (p *parser) parseNoArgFlagCall(_ int, distinct bool) (astIndex, *Error) {
	if p.tok.Kind != tokRParen {
		return noNode, p.unexpected("')'")
	}
	p.advance()
	if distinct {
		p.res.Distinct = true
	} else {
		p.res.Count = true
	}
	return noNode, nil
}

// buildWithOp applies the field's per-operator remapping and builds the node.
func (p *parser) buildWithOp(f *field, path []string, name string, val Value, pos int) (astIndex, *Error) {
	target := f.remap(name)
	if cop := p.cfg.lookupCustom(target); cop != nil {
		if !f.allows(target) {
			return noNode, p.operatorNotAllowed(target, f.name, pos)
		}
		return p.addNode(astNode{
			kind: astCustom, custom: cop, field: f, fieldName: f.name,
			path: path, args: []Value{val}, pos: pos,
		}), nil
	}
	op, ok := builtinByName[target]
	if !ok {
		return noNode, errAt(ErrUnsupportedOperator, pos,
			"unsupported operator "+quote(target)+" for field "+quote(f.name))
	}
	if !f.allows(target) {
		return noNode, p.operatorNotAllowed(target, f.name, pos)
	}
	return p.buildPredicate(f, path, op, val, pos)
}

// buildPredicate coerces and validates a predicate's value, then adds the node.
func (p *parser) buildPredicate(f *field, path []string, op builtinOp, val Value, pos int) (astIndex, *Error) {
	if op.isContainerOperator() && !f.typ.isArrayType() && f.typ != TypeJSON {
		return noNode, errAt(ErrOperatorNotAllowed, pos,
			"operator "+quote(op.String())+" requires an array or json field, but "+
				quote(f.name)+" is "+f.typ.String())
	}
	coerced, err := p.coerceFor(f, op, val, pos)
	if err != nil {
		return noNode, err
	}
	if err := p.validate(f, coerced, pos); err != nil {
		return noNode, err
	}
	return p.addNode(astNode{kind: astBuiltin, op: op, field: f, path: path, val: coerced, pos: pos}), nil
}

// coerceFor coerces a predicate value to the type the operator compares against:
// the field type for comparisons, the element type for container operators.
func (p *parser) coerceFor(f *field, op builtinOp, val Value, pos int) (Value, *Error) {
	elem := f.typeFor(op.name())
	if elem.isArrayType() {
		elem = elem.elemType()
	}
	switch op {
	case opIn, opOut:
		if val.Kind != KindArray {
			return Value{}, errAt(ErrInvalidValue, pos, "operator "+quote(op.String())+" requires an array value")
		}
		if len(val.Array) == 0 {
			return Value{}, errAt(ErrInvalidValue, pos, "operator "+quote(op.String())+" requires at least one value")
		}
		return coerceElements(val, elem, pos)
	case opContainsAll, opOverlaps:
		if f.typ == TypeJSON {
			return val, nil
		}
		if val.Kind != KindArray {
			val = Value{Kind: KindArray, Array: []Value{val}}
		}
		if len(val.Array) == 0 {
			return Value{}, errAt(ErrInvalidValue, pos, "operator "+quote(op.String())+" requires at least one value")
		}
		return coerceElements(val, elem, pos)
	case opContains, opExcludes:
		if f.typ == TypeJSON {
			return val, nil
		}
		if val.Kind == KindArray {
			return coerceElements(val, elem, pos)
		}
		return coerceAt(val, elem, pos)
	default:
		return coerceAt(val, f.typeFor(op.name()), pos)
	}
}

// coerceElements coerces each element of an array value. When every element is
// already of the target kind the original value is returned unwrapped, which
// keeps the common case allocation free.
func coerceElements(val Value, elem FieldType, pos int) (Value, *Error) {
	if elem == TypeArray || len(val.Array) == 0 {
		return val, nil
	}
	changed := false
	var out []Value
	for i := range val.Array {
		coerced, err := coerceAt(val.Array[i], elem, pos)
		if err != nil {
			return Value{}, err
		}
		if !changed && !sameValue(coerced, val.Array[i]) {
			changed = true
			out = make([]Value, len(val.Array))
			copy(out, val.Array[:i])
		}
		if changed {
			out[i] = coerced
		}
	}
	if !changed {
		return val, nil
	}
	return Value{Kind: KindArray, Array: out}, nil
}

// validate runs the configured validator over the coerced value, passing the request context
// so a check can depend on request-scoped values or the caller's deadline.
func (p *parser) validate(f *field, val Value, pos int) *Error {
	if f.validator == nil || val.Kind == KindNull {
		return nil
	}
	ctx := p.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if val.Kind == KindArray {
		for i := range val.Array {
			if err := f.validator(ctx, val.Array[i].Any()); err != nil {
				return errCause(ErrInvalidValue, pos, "field "+quote(f.name)+": "+err.Error(), err)
			}
		}
		return nil
	}
	if err := f.validator(ctx, val.Any()); err != nil {
		return errCause(ErrInvalidValue, pos, "field "+quote(f.name)+": "+err.Error(), err)
	}
	return nil
}
