package rql

// tokKind enumerates lexical token kinds.
//
// The scanner is byte oriented and never materialises token text: a Token is a
// (kind, start, end) triple into the source string. Text is decoded lazily and
// only where a string is actually needed (field names, operator names, values).
type tokKind uint8

const (
	// tokEOF marks the end of the query.
	tokEOF tokKind = iota
	// tokWord is a run of non-delimiter bytes: a field, operator, or value.
	tokWord
	// tokLParen is '('.
	tokLParen
	// tokRParen is ')'.
	tokRParen
	// tokComma is ','.
	tokComma
	// tokAnd is '&'.
	tokAnd
	// tokOr is '|'.
	tokOr
	// tokEq is '='.
	tokEq
	// tokLt is '<'.
	tokLt
	// tokLe is '<='.
	tokLe
	// tokGt is '>'.
	tokGt
	// tokGe is '>='.
	tokGe
	// tokNe is '!='.
	tokNe
)

// String returns a short name for the token kind, for diagnostics.
func (k tokKind) String() string {
	switch k {
	case tokEOF:
		return "end of query"
	case tokWord:
		return "word"
	case tokLParen:
		return "("
	case tokRParen:
		return ")"
	case tokComma:
		return ","
	case tokAnd:
		return "&"
	case tokOr:
		return "|"
	case tokEq:
		return "="
	case tokLt:
		return "<"
	case tokLe:
		return "<="
	case tokGt:
		return ">"
	case tokGe:
		return ">="
	case tokNe:
		return "!="
	default:
		return "unknown token"
	}
}

// Token flags, recorded by the scanner so later decoding can take its fast path
// (a token with no '%' and no '+' is used as a zero-copy slice of the source).
const (
	tokHasEscape uint8 = 1 << iota
	tokHasPlus
)

// Token is a lexical token identified by its byte range in the source query.
type Token struct {
	// Kind is the token kind.
	Kind tokKind
	// Start is the inclusive byte offset of the token.
	Start int
	// End is the exclusive byte offset of the token.
	End int
	// flags records whether the raw bytes contain '%' or '+'.
	flags uint8
}

// isWord reports whether the token carries text (a field, operator or value).
func (t Token) isWord() bool { return t.Kind == tokWord }

// isComparison reports whether the token is one of <, <=, >, >=, !=.
func (t Token) isComparison() bool {
	switch t.Kind {
	case tokLt, tokLe, tokGt, tokGe, tokNe:
		return true
	default:
		return false
	}
}

// isTerminator reports whether the token ends a value: EOF, ',', ')', '&', '|'.
func (t Token) isTerminator() bool {
	switch t.Kind {
	case tokEOF, tokComma, tokRParen, tokAnd, tokOr:
		return true
	default:
		return false
	}
}

// builtinOp enumerates the operators implemented by the translator core.
type builtinOp uint8

// Built-in operator identities. The zero value is opEq so that an unset field
// still means something sensible if a bug ever leaves it unset.
const (
	opEq builtinOp = iota
	opNe
	opGt
	opGe
	opLt
	opLe
	opLike
	opILike
	opIn
	opOut
	opContains
	opContainsAll
	opOverlaps
	opExcludes
	numBuiltinOps
)

// builtinNames maps operator identities to their canonical RQL names.
var builtinNames = [numBuiltinOps]string{
	opEq:          "eq",
	opNe:          "ne",
	opGt:          "gt",
	opGe:          "ge",
	opLt:          "lt",
	opLe:          "le",
	opLike:        "like",
	opILike:       "ilike",
	opIn:          "in",
	opOut:         "out",
	opContains:    "contains",
	opContainsAll: "containsAll",
	opOverlaps:    "overlaps",
	opExcludes:    "excludes",
}

// builtinKeys maps operator identities to the lower-case name used as the
// configuration key (RQL operator names are case insensitive, so this is also
// the name matched in a query).
var builtinKeys = [numBuiltinOps]string{
	opEq:          "eq",
	opNe:          "ne",
	opGt:          "gt",
	opGe:          "ge",
	opLt:          "lt",
	opLe:          "le",
	opLike:        "like",
	opILike:       "ilike",
	opIn:          "in",
	opOut:         "out",
	opContains:    "contains",
	opContainsAll: "containsall",
	opOverlaps:    "overlaps",
	opExcludes:    "excludes",
}

// builtinByName resolves a lower-case operator name to its identity.
var builtinByName = func() map[string]builtinOp {
	m := make(map[string]builtinOp, numBuiltinOps)
	for i := builtinOp(0); i < numBuiltinOps; i++ {
		m[builtinKeys[i]] = i
	}
	return m
}()

// name returns the lower-case configuration key of the operator.
func (o builtinOp) name() string {
	if o < numBuiltinOps {
		return builtinKeys[o]
	}
	return "unknown"
}

// String returns the canonical RQL name of the operator.
func (o builtinOp) String() string {
	if o < numBuiltinOps {
		return builtinNames[o]
	}
	return "unknown"
}

// isComparison reports whether the operator compares a column with one value.
func (o builtinOp) isComparison() bool {
	switch o {
	case opEq, opNe, opGt, opGe, opLt, opLe, opLike, opILike:
		return true
	default:
		return false
	}
}

// isContainerOperator reports whether the operator treats the column and the
// value as containers (arrays or json), which restricts it to container-typed
// fields.
func (o builtinOp) isContainerOperator() bool {
	switch o {
	case opContains, opContainsAll, opOverlaps, opExcludes:
		return true
	default:
		return false
	}
}

// sqlOperator returns the SQL infix operator for comparison operators.
func (o builtinOp) sqlOperator() string {
	switch o {
	case opEq:
		return "="
	case opNe:
		return "<>"
	case opGt:
		return ">"
	case opGe:
		return ">="
	case opLt:
		return "<"
	case opLe:
		return "<="
	case opLike:
		return "LIKE"
	case opILike:
		return "ILIKE"
	default:
		return ""
	}
}

// comparisonFromToken maps a comparison token to its operator.
func comparisonFromToken(k tokKind) builtinOp {
	switch k {
	case tokLt:
		return opLt
	case tokLe:
		return opLe
	case tokGt:
		return opGt
	case tokGe:
		return opGe
	case tokNe:
		return opNe
	default:
		return opEq
	}
}
