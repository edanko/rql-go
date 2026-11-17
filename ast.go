package rql

// astIndex identifies a node in the parser's node arena. Nodes are stored in a
// flat slice and refer to their children by index, which keeps parsing to a
// single arena allocation.
type astIndex int

// noNode is the absent node index.
const noNode astIndex = -1

// maxInlineKids is how many children a node holds without a side allocation.
const maxInlineKids = 4

// astKind enumerates node kinds.
type astKind uint8

const (
	// astAnd is a conjunction of two or more children.
	astAnd astKind = iota
	// astOr is a disjunction of two or more children.
	astOr
	// astNot is the negation of exactly one child.
	astNot
	// astBuiltin is a built-in predicate: field, operator, value.
	astBuiltin
	// astCustom is a registered operator invocation.
	astCustom
)

// astNode is one node of a parsed query.
type astNode struct {
	kind   astKind
	op     builtinOp
	custom *customOp
	field  *field
	// fieldName is the field name as written, for custom operators that resolve
	// their own columns (including names that are not registered fields).
	fieldName string
	// path holds JSONB path segments supplied by the query.
	path []string
	// val is the value of a built-in predicate.
	val Value
	// args holds the value arguments of a custom operator invocation.
	args []Value
	// kids holds child node indices, inline for the common small case.
	kids    [maxInlineKids]astIndex
	nkids   int
	kidOver []astIndex
	// pos is the byte offset of the construct, for diagnostics.
	pos int
}

// kidCount returns the number of children.
func (n *astNode) kidCount() int { return n.nkids }

// kidAt returns the i-th child index.
func (n *astNode) kidAt(i int) astIndex {
	if i < maxInlineKids {
		return n.kids[i]
	}
	return n.kidOver[i-maxInlineKids]
}

// addKid appends a child index.
func (n *astNode) addKid(k astIndex) {
	if n.nkids < maxInlineKids {
		n.kids[n.nkids] = k
		n.nkids++
		return
	}
	n.kidOver = append(n.kidOver, k)
	n.nkids++
}

// renderer turns configured fields and query paths into SQL column expressions.
// It is shared by the parser (select/sort rendering) and the builder.
type renderer struct {
	d Dialect
}

// column renders the SQL column expression for a field and path.
//
// wantText selects the jsonb extraction operator for the last segment: '->>'
// (text) for scalar comparisons and '->' (jsonb) for container operators. When a
// path is present and the field type needs a cast, the extracted text is cast so
// that a jsonb string compares correctly with a typed parameter.
//
// Every identifier comes from the configuration; every path segment is checked
// against the identifier alphabet here as well as at parse time, so this
// function is the single place where a name could reach SQL and it cannot.
func (r renderer) column(f *field, op string, path []string, wantText bool, castType FieldType) (string, *Error) {
	parts := f.columnFor(op)
	if len(parts) == 0 {
		return "", errNoPos(ErrInvalidPath, "field "+quote(f.name)+" has no column")
	}
	if len(parts) == 1 && len(path) == 0 {
		return r.d.QuoteIdentifier(parts[0]), nil
	}
	quoted := r.d.QuoteIdentifier(parts[0])
	buf := make([]byte, 0, len(quoted)+16*(len(parts)+len(path)))
	buf = append(buf, quoted...)
	total := len(parts) - 1 + len(path)
	writeSeg := func(seg string, last bool) *Error {
		if !isPathSegment(seg) {
			return errNoPos(ErrInvalidPath, "invalid path segment "+quote(seg))
		}
		if last && wantText {
			buf = append(buf, "->>'"...)
		} else {
			buf = append(buf, "->'"...)
		}
		buf = append(buf, seg...)
		buf = append(buf, '\'')
		return nil
	}
	written := 0
	for _, seg := range parts[1:] {
		written++
		if err := writeSeg(seg, written == total); err != nil {
			return "", err
		}
	}
	for _, seg := range path {
		written++
		if err := writeSeg(seg, written == total); err != nil {
			return "", err
		}
	}
	expr := string(buf)
	if wantText && total > 0 {
		if cast := r.d.CastFor(castType); cast != "" {
			return "(" + expr + ")" + cast, nil
		}
	}
	return expr, nil
}

// sortColumn renders the ORDER BY expression for a sort field.
func (r renderer) sortColumn(s SortField) string {
	if len(s.Path) == 0 {
		return r.d.QuoteIdentifier(s.Column)
	}
	buf := make([]byte, 0, len(s.Column)+16*len(s.Path))
	buf = append(buf, r.d.QuoteIdentifier(s.Column)...)
	for i, seg := range s.Path {
		if i == len(s.Path)-1 {
			buf = append(buf, "->>'"...)
		} else {
			buf = append(buf, "->'"...)
		}
		buf = append(buf, seg...)
		buf = append(buf, '\'')
	}
	return string(buf)
}
