package rql

import (
	"errors"
	"testing"
	"time"
)

// specConfig declares the fields used by the specification examples.
func specConfig() *Config {
	cfg := NewConfig()
	cfg.Field("foo", TypeString).Sortable()
	cfg.Field("bar", TypeString).Sortable()
	cfg.Field("name", TypeString).Sortable().Selectable()
	cfg.Field("price", TypeFloat).Sortable().Selectable()
	cfg.Field("rating", TypeFloat).Sortable()
	cfg.Field("tags", TypeStringArray).Allow("contains", "containsall", "excludes", "overlaps")
	cfg.AllowDynamicPaths("foo")
	return cfg
}

// TestSpecExamplesVerbatim runs the specification's own examples.
func TestSpecExamplesVerbatim(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(specConfig(), Postgres{})
	cases := []struct {
		query string
		where string
	}{
		// "A simple RQL query ... eq(foo,3)"
		{"eq(foo,3)", `"foo" = $1`},
		// "The following query is identical ... foo=3"
		{"foo=3", `"foo" = $1`},
		// "FIQL syntax: price=lt=10"
		{"price=lt=10", `"price" < $1`},
		// "identical ... normalized form: lt(price,10)"
		{"lt(price,10)", `"price" < $1`},
		// "combine conditions ... foo=3&price=lt=10"
		{"foo=3&price=lt=10", `("foo" = $1) AND ("price" < $2)`},
		// "the same as: and(eq(foo,3),lt(price,10))"
		{"and(eq(foo,3),lt(price,10))", `("foo" = $1) AND ("price" < $2)`},
		// "The | operator ... (foo=3|foo=bar)&price=lt=10"
		{"(foo=3|foo=bar)&price=lt=10", `(("foo" = $1) OR ("foo" = $2)) AND ("price" < $3)`},
		// "paranthesis to group expressions ... a=1&(b=2|c=3)" (spec spelling)
		//nolint:misspell // the specification itself spells it "paranthesis".
		{"foo=3&(bar=2|name=c)", `("foo" = $1) AND (("bar" = $2) OR ("name" = $3))`},
		// "values can be ... booleans ... null"
		{"foo=true", `"foo" = $1`},
		{"foo=null", `"foo" IS NULL`},
		// "nested by using an array of properties ... (foo,bar)=3"
		{"(foo,bar)=3", `"foo"->>'bar' = $1`},
		// "slashes as shorthand ... foo/bar=3"
		{"foo/bar=3", `"foo"->>'bar' = $1`},
		// "sort(<+|-><property>) ... sort(+price,-rating)"
		{"sort(+price,-rating)", ""},
		// "limit(count,start,maxCount) ... limit(10,5)"
		{"limit(10,5)", ""},
		// "select(<property>,...) ... select(name,price)"
		{"select(name,price)", ""},
		// "contains ... the array contains any value that equals the provided value"
		{"contains(tags,go)", `$1 = ANY("tags")`},
		// "excludes ... does not contain any"
		{"excludes(tags,go)", `NOT ($1 = ANY("tags"))`},
		// "in(<property>,<array-of-values>) ... in(foo,(3,4))"
		{"in(foo,(3,4))", `"foo" IN ($1, $2)`},
		{"out(foo,(3,4))", `"foo" NOT IN ($1, $2)`},
		{"foo=in=(3,4)", `"foo" IN ($1, $2)`},
	}
	for _, tc := range cases {
		res, err := tr.Translate(newTestContext(t), tc.query)
		if err != nil {
			t.Fatalf("%q: %v", tc.query, err)
		}
		if res.Where != tc.where {
			t.Fatalf("%q: Where = %q, want %q", tc.query, res.Where, tc.where)
		}
	}
}

// TestSpecFlagshipInExample is the example from the specification's own prose:
// foo=in=(3,bar,true,2000-01-01T00:00:00Z) — including a raw ISO date with no
// colon encoding. The reference implementation throws on this input.
func TestSpecFlagshipInExample(t *testing.T) {
	t.Parallel()
	cfg := specConfig()
	cfg.Field("mixed", TypeString)
	tr := NewTranslator(cfg, Postgres{})

	res := mustTranslate(t, tr, "mixed=in=(3,bar,true,2000-01-01T00:00:00Z)")
	if want := `"mixed" IN ($1, $2, $3, $4)`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
	if len(res.Args) != 4 {
		t.Fatalf("Args = %#v", res.Args)
	}
	// Coerced to the string field type.
	if res.Args[0] != "3" || res.Args[1] != "bar" || res.Args[2] != "true" {
		t.Fatalf("Args = %#v", res.Args)
	}
	// The date survives as an ISO timestamp string.
	if s, ok := res.Args[3].(string); !ok || s != "2000-01-01T00:00:00Z" {
		t.Fatalf("date arg = %#v", res.Args[3])
	}

	// On a timestamp field the same literal binds a time.Time.
	cfg2 := specConfig()
	cfg2.Field("at", TypeTime)
	tr2 := NewTranslator(cfg2, Postgres{})
	res = mustTranslate(t, tr2, "at=in=(2000-01-01T00:00:00Z)")
	when, ok := res.Args[0].(time.Time)
	if !ok || !when.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("timestamp arg = %#v", res.Args[0])
	}
}

// TestSpecConformanceDecisions walks the decision table in docs/DESIGN.md §2 and
// asserts each decision is implemented. One subtest per decision.
//
//nolint:gocognit,gocyclo // a flat list of one decision per subtest reads clearer than a table.
func TestSpecConformanceDecisions(t *testing.T) {
	t.Parallel()
	cfg := specConfig()
	tr := NewTranslator(cfg, Postgres{})

	t.Run("raw ISO dates accepted", func(t *testing.T) {
		t.Parallel()
		res := mustTranslate(t, tr, "name=2000-01-01T00:00:00Z")
		if _, ok := res.Args[0].(string); !ok {
			t.Fatalf("name is a string field, arg = %#v", res.Args[0])
		}
	})

	t.Run("percent-encoded operators", func(t *testing.T) {
		t.Parallel()
		cases := map[string]string{
			"price=%3C10":  `"price" < $1`,
			"price=%3C=10": `"price" <= $1`,
			"price=%3E10":  `"price" > $1`,
			"price=%3E=10": `"price" >= $1`,
		}
		for query, want := range cases {
			if got := mustTranslate(t, tr, query).Where; got != want {
				t.Fatalf("%q: Where = %q, want %q", query, got, want)
			}
		}
	})

	t.Run("mixed conjunctions rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := tr.Translate(newTestContext(t), "foo=1&bar=2|name=3"); !errors.Is(err, ErrMixedConjunctions) {
			t.Fatalf("got %v, want ErrMixedConjunctions", err)
		}
	})

	t.Run("both nested spellings accepted", func(t *testing.T) {
		t.Parallel()
		a := mustTranslate(t, tr, "(foo,bar)=3").Where
		b := mustTranslate(t, tr, "foo/bar=3").Where
		if a != b {
			t.Fatalf("(foo,bar)=3 gives %q but foo/bar=3 gives %q", a, b)
		}
	})

	t.Run("typed prefixes", func(t *testing.T) {
		t.Parallel()
		for _, query := range []string{
			"name=string:3", "name=number:3", "name=boolean:true",
			"name=date:2000-01-01T00:00:00Z", "name=isodate:2000-01-01", "name=epoch:946684800000",
		} {
			if _, err := tr.Translate(newTestContext(t), query); err != nil {
				t.Fatalf("%q: %v", query, err)
			}
		}
	})

	t.Run("regexp converters rejected", func(t *testing.T) {
		t.Parallel()
		for _, query := range []string{"name=re:^ab", "name=RE:ab", "name=glob:ab*"} {
			if _, err := tr.Translate(newTestContext(t), query); !errors.Is(err, ErrUnsupportedConverter) {
				t.Fatalf("%q: got %v, want ErrUnsupportedConverter", query, err)
			}
		}
	})

	t.Run("parameter references rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := tr.Translate(newTestContext(t), "name=$1"); !errors.Is(err, ErrUnsupportedOperator) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("contains is ANY and containsAll is ALL", func(t *testing.T) {
		t.Parallel()
		if got := mustTranslate(t, tr, "contains(tags,go)").Where; got != `$1 = ANY("tags")` {
			t.Fatalf("contains Where = %q", got)
		}
		if got := mustTranslate(t, tr, "containsall(tags,(go,rs))").Where; got != `"tags" @> ARRAY[$1, $2]` {
			t.Fatalf("containsAll Where = %q", got)
		}
	})

	t.Run("excludes is NOT ANY", func(t *testing.T) {
		t.Parallel()
		if got := mustTranslate(t, tr, "excludes(tags,go)").Where; got != `NOT ($1 = ANY("tags"))` {
			t.Fatalf("excludes Where = %q", got)
		}
	})

	t.Run("limit keeps maxCount", func(t *testing.T) {
		t.Parallel()
		res := mustTranslate(t, tr, "limit(10,5,20)")
		if res.LimitOrDefault(-1) != 10 || res.OffsetOrDefault(-1) != 5 {
			t.Fatalf("Limit=%v Offset=%v", res.Limit, res.Offset)
		}
	})

	t.Run("shaping operators supported", func(t *testing.T) {
		t.Parallel()
		res := mustTranslate(t, tr, "select(name)&sort(-price)&limit(2)&distinct()&count()")
		if len(res.Select) != 1 || len(res.Sort) != 1 || !res.Distinct || !res.Count {
			t.Fatalf("result = %#v", res)
		}
	})

	t.Run("aggregation operators rejected", func(t *testing.T) {
		t.Parallel()
		for _, query := range []string{
			"aggregate(foo,sum(price))", "first()", "one()", "rel(x,foo=1)",
			"recurse(foo)", "values(foo)",
		} {
			if _, err := tr.Translate(newTestContext(t), query); !errors.Is(err, ErrUnsupportedOperator) {
				t.Fatalf("%q: got %v, want ErrUnsupportedOperator", query, err)
			}
		}
	})

	t.Run("repeated equality not rewritten", func(t *testing.T) {
		t.Parallel()
		if got := mustTranslate(t, tr, "foo=1&foo=2").Where; got != `("foo" = $1) AND ("foo" = $2)` {
			t.Fatalf("Where = %q", got)
		}
	})

	t.Run("single quotes stripped", func(t *testing.T) {
		t.Parallel()
		res := mustTranslate(t, tr, "name=%27bar%27")
		if len(res.Args) != 1 || res.Args[0] != "bar" {
			t.Fatalf("Args = %#v", res.Args)
		}
	})
}

// TestSpecGrammarRequirements covers the additional grammar the design requires.
func TestSpecGrammarRequirements(t *testing.T) {
	t.Parallel()
	cfg := specConfig()
	tr := NewTranslator(cfg, Postgres{})

	// Bare FIQL comparison operators.
	for query, want := range map[string]string{
		"price<10":  `"price" < $1`,
		"price>10":  `"price" > $1`,
		"price<=10": `"price" <= $1`,
		"price>=10": `"price" >= $1`,
		"price!=10": `"price" <> $1`,
	} {
		if got := mustTranslate(t, tr, query).Where; got != want {
			t.Fatalf("%q: Where = %q, want %q", query, got, want)
		}
	}

	// '%xx' decoding and '+' as space.
	res := mustTranslate(t, tr, "name=a%20b")
	if res.Args[0] != "a b" {
		t.Fatalf("percent decoding: %#v", res.Args)
	}
	res = mustTranslate(t, tr, "name=a+b")
	if res.Args[0] != "a b" {
		t.Fatalf("plus decoding: %#v", res.Args)
	}

	// Top-level comma is a conjunction.
	if got := mustTranslate(t, tr, "foo=1,bar=2").Where; got != `("foo" = $1) AND ("bar" = $2)` {
		t.Fatalf("comma conjunction Where = %q", got)
	}

	// Depth limit.
	cfg.MaxDepth = 2
	limited := NewTranslator(cfg, Postgres{})
	if _, err := limited.Translate(newTestContext(t), "(((foo=1)))"); !errors.Is(err, ErrMaxDepth) {
		t.Fatalf("got %v, want ErrMaxDepth", err)
	}

	// No panics on malformed input.
	for _, query := range []string{"", "(", ")", "=", "&", "|", "()", "a", "a=", "=a", ",", "not(", "and(", "%"} {
		_, _ = NewTranslator(specConfig(), Postgres{}).Translate(newTestContext(t), query)
	}
}
