package rql

import "strings"

// SortField is one ORDER BY term.
type SortField struct {
	// Column is the SQL column name, unquoted. It is the configured column name,
	// which for a nested field is the root column.
	Column string
	// Path holds JSONB path segments for a nested sort, if any.
	Path []string
	// Desc reports descending order.
	Desc bool
}

// Result is the structured outcome of translating an RQL query.
//
// It is deliberately structured rather than pre-rendered so callers can feed any
// query builder (gorm, sqlx, pgx) without re-parsing; the helper methods cover
// the common string-shaped needs.
type Result struct {
	// Where is the boolean SQL expression for the WHERE clause, without the
	// WHERE keyword. Empty when the query has no filter (e.g. only a sort).
	Where string
	// Args holds the bound parameters, in placeholder order.
	Args []any
	// Sort is the requested ordering, in order.
	Sort []SortField
	// Select lists the projected columns as quoted SQL expressions. Empty when
	// the query did not use select().
	Select []string
	// Limit is the requested row limit, or nil when absent.
	Limit *int
	// Offset is the requested row offset, or nil when absent.
	Offset *int
	// Distinct reports that distinct() was requested.
	Distinct bool
	// Count reports that count() was requested.
	Count bool
	// Flags holds out-of-band values set by custom operators through
	// OpContext.Flag (for example "showArchived"). Nil when unused.
	Flags map[string]any

	// d is the dialect in force, used by the rendering helpers.
	d Dialect
}

// WhereString returns the WHERE expression, or "" when there is none.
func (r *Result) WhereString() string {
	if r == nil {
		return ""
	}
	return r.Where
}

// GetValues returns the bound parameters, in placeholder order.
func (r *Result) GetValues() []any {
	if r == nil {
		return nil
	}
	return r.Args
}

// SelectString joins the projected columns with ", ", or returns "" when the
// query did not project anything.
func (r *Result) SelectString() string {
	if r == nil || len(r.Select) == 0 {
		return ""
	}
	var b strings.Builder
	size := 0
	for _, s := range r.Select {
		size += len(s) + 2
	}
	b.Grow(size)
	for i, s := range r.Select {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(s)
	}
	return b.String()
}

// OrderBy renders the ORDER BY expression with the translator's dialect,
// without the ORDER BY keyword. It returns "" when no sort was requested.
func (r *Result) OrderBy() string {
	if r == nil {
		return ""
	}
	return r.OrderByWith(r.d)
}

// OrderByWith renders the ORDER BY expression with an explicit dialect.
func (r *Result) OrderByWith(d Dialect) string {
	if r == nil || len(r.Sort) == 0 {
		return ""
	}
	if d == nil {
		d = Postgres{}
	}
	rend := renderer{d: d}
	var b strings.Builder
	b.Grow(32 * len(r.Sort))
	for i, s := range r.Sort {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(rend.sortColumn(s))
		if s.Desc {
			b.WriteString(" DESC")
		} else {
			b.WriteString(" ASC")
		}
	}
	return b.String()
}

// HasLimit reports whether the query specified a limit.
func (r *Result) HasLimit() bool { return r != nil && r.Limit != nil }

// HasOffset reports whether the query specified an offset.
func (r *Result) HasOffset() bool { return r != nil && r.Offset != nil }

// LimitOrDefault returns the limit, or def when the query did not set one.
func (r *Result) LimitOrDefault(def int) int {
	if r == nil || r.Limit == nil {
		return def
	}
	return *r.Limit
}

// OffsetOrDefault returns the offset, or def when the query did not set one.
func (r *Result) OffsetOrDefault(def int) int {
	if r == nil || r.Offset == nil {
		return def
	}
	return *r.Offset
}

// BoolFlag returns the named out-of-band flag if a custom operator set one and
// it is a boolean.
func (r *Result) BoolFlag(name string) (value, present bool) {
	if r == nil || r.Flags == nil {
		return false, false
	}
	v, ok := r.Flags[name]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}
