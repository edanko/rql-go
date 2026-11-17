package rql

import (
	"errors"
	"testing"
)

func TestParserAcceptsSpecForms(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	// Every form the specification defines must translate, and equivalent
	// spellings must produce identical SQL.
	equivalents := [][]string{
		{"eq(name,abc)", "name=abc", "name=eq=abc", "name='abc'", "name=%27abc%27"},
		{"lt(age,10)", "age=lt=10", "age<10", "age=%3C10"},
		{"le(age,10)", "age=le=10", "age<=10", "age=%3C=10"},
		{"gt(age,10)", "age=gt=10", "age>10", "age=%3E10"},
		{"ge(age,10)", "age=ge=10", "age>=10", "age=%3E=10"},
		{"ne(age,10)", "age=ne=10", "age!=10", "age=%21=10"},
		{"in(age,(1,2))", "age=in=(1,2)"},
		{"out(age,(1,2))", "age=out=(1,2)"},
		{"contains(tags,go)", "tags=contains=(go)"},
		{"containsall(tags,(go,rs))", "tags=containsall=(go,rs)"},
		{"overlaps(tags,(a,b))", "tags=overlaps=(a,b)"},
		{"excludes(tags,(a))", "tags=excludes=(a)"},
		{"and(name=a,age=1)", "name=a&age=1", "name=a,age=1"},
		{"or(name=a,age=1)", "name=a|age=1"},
		{"select(id,name)", "select(id,name)"},
		{"sort(-age)", "sort(-age)"},
	}
	for _, group := range equivalents {
		base := mustTranslate(t, tr, group[0])
		for _, query := range group[1:] {
			got := mustTranslate(t, tr, query)
			if got.Where != base.Where {
				t.Fatalf("%q: Where = %q, but %q gives %q", query, got.Where, group[0], base.Where)
			}
			if len(got.Args) != len(base.Args) {
				t.Fatalf("%q: %d args, but %q gives %d", query, len(got.Args), group[0], len(base.Args))
			}
		}
	}
}

func TestParserNestedPathEquivalence(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.AllowDynamicPaths("foo")
	tr := NewTranslator(cfg, Postgres{})

	slash := mustTranslate(t, tr, "foo/bar=3")
	property := mustTranslate(t, tr, "(foo,bar)=3")
	if slash.Where != property.Where {
		t.Fatalf("a/b=%q but (a,b)=%q", slash.Where, property.Where)
	}
	if want := `"foo"->>'bar' = $1`; slash.Where != want {
		t.Fatalf("Where = %q, want %q", slash.Where, want)
	}
	three := mustTranslate(t, tr, "foo/bar/baz=3")
	if want := `"foo"->'bar'->>'baz' = $1`; three.Where != want {
		t.Fatalf("three-level Where = %q, want %q", three.Where, want)
	}
}

func TestParserTypedPrefixes(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})

	res := mustTranslate(t, tr, "name=string:3")
	if len(res.Args) != 1 || res.Args[0] != "3" {
		t.Fatalf("string:3 args = %#v", res.Args)
	}
	res = mustTranslate(t, tr, "age=number:3")
	if len(res.Args) != 1 || res.Args[0] != int64(3) {
		t.Fatalf("number:3 args = %#v", res.Args)
	}
	res = mustTranslate(t, tr, "isActive=boolean:true")
	if res.Where != `"is_active" = TRUE` {
		t.Fatalf("boolean:true Where = %q", res.Where)
	}
	res = mustTranslate(t, tr, "createdAt=date:2000-01-01T00:00:00Z")
	if len(res.Args) != 1 {
		t.Fatalf("date: args = %#v", res.Args)
	}
	res = mustTranslate(t, tr, "createdAt=epoch:946684800000")
	if len(res.Args) != 1 {
		t.Fatalf("epoch: args = %#v", res.Args)
	}
	res = mustTranslate(t, tr, "createdAt=isodate:2000-01-01")
	if len(res.Args) != 1 {
		t.Fatalf("isodate: args = %#v", res.Args)
	}
}

func TestParserRejections(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.MaxDepth = 3
	tr := NewTranslator(cfg, Postgres{})

	cases := []struct {
		query string
		class error
	}{
		{"name=re:^ab", ErrUnsupportedConverter},
		{"name=RE:ab", ErrUnsupportedConverter},
		{"name=glob:ab*", ErrUnsupportedConverter},
		{"name=$1", ErrUnsupportedOperator},
		{"$1=name", ErrUnsupportedOperator},
		{"name=a&age=1|isActive=true", ErrMixedConjunctions},
		{"name=a|age=1&isActive=true", ErrMixedConjunctions},
		{"values(name)", ErrUnsupportedOperator},
		{"aggregate(name,sum(age))", ErrUnsupportedOperator},
		{"first()", ErrUnsupportedOperator},
		{"one()", ErrUnsupportedOperator},
		{"rel(x,name=a)", ErrUnsupportedOperator},
		{"recurse(name)", ErrUnsupportedOperator},
		{"offset(5)", ErrUnsupportedOperator},
		{"name", ErrInvalidSyntax},
		{"=1", ErrInvalidSyntax},
		{"name=a)", ErrInvalidSyntax},
		{"(name=a", ErrInvalidSyntax},
		{"name=a&", ErrInvalidSyntax},
		{"or()", ErrInvalidSyntax},
		{"not()", ErrInvalidSyntax},
		{"limit()", ErrInvalidSyntax},
		{"limit(1,2,3,4)", ErrInvalidSyntax},
		{"select()", ErrInvalidSyntax},
		{"select(id)&select(id)", ErrInvalidSyntax},
		{"sort(-age)&sort(-age)", ErrInvalidSyntax},
		{"limit(1)&limit(2)", ErrInvalidSyntax},
		{"name=contains=(a)", ErrOperatorNotAllowed},
		{"nope=1", ErrUnknownField},
		{"eq(nope,1)", ErrUnknownField},
		{"name=a AND age=1", ErrInvalidSyntax},
	}
	for _, tc := range cases {
		_ = translateErr(t, tr, tc.query, tc.class)
	}
}

func TestParserDepthLimit(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.MaxDepth = 3
	tr := NewTranslator(cfg, Postgres{})

	// Within the limit.
	mustTranslate(t, tr, "(((name=a)))")
	// Beyond it.
	_ = translateErr(t, tr, "((((name=a))))", ErrMaxDepth)
	// A deeply nested call chain also trips the limit.
	_ = translateErr(t, tr, "and(name=a,and(age=1,and(isActive=true,and(name=b))))", ErrMaxDepth)
}

func TestParserDeepNestingRaisesLimit(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.MaxDepth = 64
	tr := NewTranslator(cfg, Postgres{})
	query := "name=a"
	for range 20 {
		query = "and(" + query + ")"
	}
	if _, err := tr.Translate(newTestContext(t), query); err != nil {
		t.Fatalf("20 nested and(): %v", err)
	}
}

func TestParserNotSemantics(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})

	// De Morgan: not(a&b) == not(a) | not(b).
	got := mustTranslate(t, tr, "not(name=a&age=1)")
	if want := `(NOT ("name" = $1)) OR (NOT ("age" = $2))`; got.Where != want {
		t.Fatalf("Where = %q, want %q", got.Where, want)
	}
	// not(a|b) == not(a) & not(b).
	got = mustTranslate(t, tr, "not(name=a|age=1)")
	if want := `(NOT ("name" = $1)) AND (NOT ("age" = $2))`; got.Where != want {
		t.Fatalf("Where = %q, want %q", got.Where, want)
	}
	// Triple negation keeps one.
	got = mustTranslate(t, tr, "not(not(not(age=1)))")
	if want := `NOT ("age" = $1)`; got.Where != want {
		t.Fatalf("Where = %q, want %q", got.Where, want)
	}
}

func TestParserTopLevelCommaIsAnd(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	got := mustTranslate(t, tr, "name=a,age=1,isActive=true")
	if want := `("name" = $1) AND ("age" = $2) AND ("is_active" = TRUE)`; got.Where != want {
		t.Fatalf("Where = %q, want %q", got.Where, want)
	}
	// A comma group nested in a disjunction keeps its meaning.
	got = mustTranslate(t, tr, "(name=a,age=1)|isActive=true")
	if want := `(("name" = $1) AND ("age" = $2)) OR ("is_active" = TRUE)`; got.Where != want {
		t.Fatalf("Where = %q, want %q", got.Where, want)
	}
}

func TestParserRepeatedEqualityIsNotRewritten(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	// a=1&a=2 must stay two conjuncts: rewriting to IN would flip AND to OR and
	// rewriting to a containment test would break on a scalar column.
	got := mustTranslate(t, tr, "age=1&age=2")
	if want := `("age" = $1) AND ("age" = $2)`; got.Where != want {
		t.Fatalf("Where = %q, want %q", got.Where, want)
	}
	// or(eq,eq) is left alone too: the caller may rely on the exact plan.
	got = mustTranslate(t, tr, "or(age=1,age=2)")
	if want := `("age" = $1) OR ("age" = $2)`; got.Where != want {
		t.Fatalf("Where = %q, want %q", got.Where, want)
	}
}

func TestParserEmptyValue(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	for _, query := range []string{"name=", "name=eq="} {
		res := mustTranslate(t, tr, query)
		if len(res.Args) != 1 || res.Args[0] != "" {
			t.Fatalf("%q: Args = %#v, want one empty string", query, res.Args)
		}
	}
}

func TestParserArrayValues(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})

	// The specification's own example, including an unencoded ISO date.
	res := mustTranslate(t, tr, "name=in=(3,bar,true,2000-01-01T00:00:00Z)")
	if want := `"name" IN ($1, $2, $3, $4)`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
	if len(res.Args) != 4 || res.Args[0] != "3" || res.Args[1] != "bar" || res.Args[2] != "true" {
		t.Fatalf("Args = %#v", res.Args)
	}
	// Nested arrays are parsed as values.
	if _, err := tr.Translate(newTestContext(t), "age=in=((1,2),(3))"); err == nil {
		t.Fatal("a nested array of arrays should not coerce to ints")
	}
	// An empty array is rejected for in/out.
	_ = translateErr(t, tr, "age=in=()", ErrInvalidValue)
	// A non-array value is rejected for in/out.
	_ = translateErr(t, tr, "age=in=5", ErrInvalidValue)
}

func TestParserExpressionArgumentRejected(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	// contains(tags,eq(x,1)): the specification allows an expression, but it
	// cannot be bound as a parameter, so it is rejected rather than bound as a
	// Go AST node (which would fail at execution time).
	_ = translateErr(t, tr, "contains(tags,eq(age,1))", ErrUnsupportedOperator)
}

func TestParserResultReuseIsolation(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	first := mustTranslate(t, tr, "name=a&age=1&sort(-age)&limit(5)")
	second := mustTranslate(t, tr, "isActive=true")
	if second.Where != `"is_active" = TRUE` || len(second.Sort) != 0 || second.HasLimit() {
		t.Fatalf("second result leaked state: %#v", second)
	}
	if first.Where != `("name" = $1) AND ("age" = $2)` || len(first.Sort) != 1 {
		t.Fatalf("first result was mutated: %#v", first)
	}
}

func TestParserErrorPositions(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	cases := map[string]int{
		"nope=1":        0,
		"name=a&nope=1": 7,
		"name=a|nope=1": 7,
	}
	for query, want := range cases {
		err := translateErr(t, tr, query, ErrUnknownField)
		if err.Pos != want {
			t.Fatalf("%q: position %d, want %d (%v)", query, err.Pos, want, err)
		}
	}
	if !errors.Is(translateErr(t, tr, "name=a)", ErrInvalidSyntax), ErrInvalidSyntax) {
		t.Fatal("syntax error should classify as ErrInvalidSyntax")
	}
}
