package rql

import (
	"strings"
	"testing"
)

func TestQuoteIdentifier(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"name":       `"name"`,
		`a"b`:        `"a""b"`,
		`a" OR 1=1`:  `"a"" OR 1=1"`,
		"":           `""`,
		"with space": `"with space"`,
	}
	for in, want := range cases {
		if got := quotedIdent(in); got != want {
			t.Fatalf("quotedIdent(%q) = %q, want %q", in, got, want)
		}
		if got := (Postgres{}).QuoteIdentifier(in); got != want {
			t.Fatalf("Postgres.QuoteIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostileConfiguredColumnIsEscaped(t *testing.T) {
	t.Parallel()
	const hostile = `a" OR 1=1 --`
	cfg := NewConfig()
	cfg.Field("name", TypeString).Column(hostile)
	tr := NewTranslator(cfg, Postgres{})
	res := mustTranslate(t, tr, "name=x")
	if want := `"a"" OR 1=1 --" = $1`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
}

func TestPlaceholders(t *testing.T) {
	t.Parallel()
	for _, d := range []Dialect{Postgres{}, PostgresOptimized{}} {
		for i, want := range []string{"$1", "$2", "$10"} {
			n := i + 1
			if n == 3 {
				n = 10
			}
			if got := d.Placeholder(n); got != want {
				t.Fatalf("%T.Placeholder(%d) = %q, want %q", d, n, got, want)
			}
		}
	}
	if got := (QuestionMark{}).Placeholder(7); got != "?" {
		t.Fatalf("QuestionMark.Placeholder = %q", got)
	}
}

func TestCastFor(t *testing.T) {
	t.Parallel()
	d := Postgres{}
	cases := map[FieldType]string{
		TypeString: "",
		TypeInt:    "::int8",
		TypeFloat:  "::float8",
		TypeBool:   "::bool",
		TypeTime:   "::timestamptz",
		TypeDate:   "::date",
		TypeUUID:   "::uuid",
		TypeJSON:   "",
		TypeArray:  "",
	}
	for typ, want := range cases {
		if got := d.CastFor(typ); got != want {
			t.Fatalf("CastFor(%v) = %q, want %q", typ, got, want)
		}
	}
}

func TestBuildArrayOp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d    Dialect
		op   ArrayOp
		col  string
		phs  string
		want string
	}{
		{Postgres{}, ArrayIn, `"a"`, "$1, $2", `"a" IN ($1, $2)`},
		{Postgres{}, ArrayNotIn, `"a"`, "$1", `"a" NOT IN ($1)`},
		{Postgres{}, ArrayContainsAll, `"t"`, "$1, $2", `"t" @> ARRAY[$1, $2]`},
		{Postgres{}, ArrayOverlaps, `"t"`, "$1", `"t" && ARRAY[$1]`},
		{PostgresOptimized{}, ArrayIn, `"a"`, "$1", `"a" = ANY($1)`},
		{PostgresOptimized{}, ArrayNotIn, `"a"`, "$1", `"a" <> ALL($1)`},
		{PostgresOptimized{}, ArrayContainsAll, `"t"`, "$1", `"t" @> $1`},
		{PostgresOptimized{}, ArrayOverlaps, `"t"`, "$1", `"t" && $1`},
		{QuestionMark{}, ArrayIn, `"a"`, "?, ?", `"a" IN (?, ?)`},
		{QuestionMark{}, ArrayContainsAll, `"t"`, "?", `"t" @> ARRAY[?]`},
	}
	for _, tc := range cases {
		if got := tc.d.BuildArrayOp(tc.op, tc.col, tc.phs); got != tc.want {
			t.Fatalf("%T.BuildArrayOp(%v) = %q, want %q", tc.d, tc.op, got, tc.want)
		}
	}
}

// TestAppendArrayOpMatchesBuild checks that the allocation-free path and the
// string-building path agree for every built-in dialect.
func TestAppendArrayOpMatchesBuild(t *testing.T) {
	t.Parallel()
	for _, d := range []Dialect{Postgres{}, PostgresOptimized{}, QuestionMark{}} {
		app, ok := d.(arrayAppender)
		if !ok {
			t.Fatalf("%T does not implement arrayAppender", d)
		}
		for _, op := range []ArrayOp{ArrayIn, ArrayNotIn, ArrayContainsAll, ArrayOverlaps} {
			phs := d.Placeholder(1)
			count := 1
			if _, isOpt := d.(sliceBinder); !isOpt {
				phs = phs + ", " + d.Placeholder(2)
				count = 2
			}
			want := d.BuildArrayOp(op, `"c"`, phs)
			got := string(app.AppendArrayOp(nil, op, `"c"`, 1, count))
			if got != want {
				t.Fatalf("%T op %v: AppendArrayOp = %q, BuildArrayOp = %q", d, op, got, want)
			}
		}
	}
}

func TestDialectsRenderSameQuery(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	query := "name=like=ab*&age=in=(1,2)&tags=containsall=(a,b)&sort(-age)&limit(5)"
	ctx := newTestContext(t)

	pg := mustTranslate(t, NewTranslator(cfg, Postgres{}), query)
	opt := mustTranslate(t, NewTranslator(cfg, PostgresOptimized{}), query)
	qm := mustTranslate(t, NewTranslator(cfg, QuestionMark{}), query)

	if !strings.Contains(pg.Where, "$1") || !strings.Contains(pg.Where, " IN ($") {
		t.Fatalf("Postgres Where = %q", pg.Where)
	}
	if !strings.Contains(opt.Where, "= ANY($") || len(opt.Args) != 3 {
		t.Fatalf("PostgresOptimized Where = %q args = %#v", opt.Where, opt.Args)
	}
	if !strings.Contains(qm.Where, "?") || strings.Contains(qm.Where, "$") {
		t.Fatalf("QuestionMark Where = %q", qm.Where)
	}
	if n := strings.Count(qm.Where, "?"); n != len(qm.Args) {
		t.Fatalf("QuestionMark has %d placeholders for %d args", n, len(qm.Args))
	}
	if pg.LimitOrDefault(0) != 5 || opt.LimitOrDefault(0) != 5 || qm.LimitOrDefault(0) != 5 {
		t.Fatal("limit lost in a dialect")
	}
	for _, res := range []*Result{pg, opt, qm} {
		if len(res.Sort) != 1 || res.Sort[0].Column != "age" || !res.Sort[0].Desc {
			t.Fatalf("sort lost: %#v", res.Sort)
		}
	}
	_ = ctx
}

func TestOptimizedDialectBindsSlice(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	res := mustTranslate(t, NewTranslator(cfg, PostgresOptimized{}), "age=in=(1,2)")
	if want := `"age" = ANY($1)`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
	if len(res.Args) != 1 {
		t.Fatalf("Args = %#v, want one slice", res.Args)
	}
	if got, ok := res.Args[0].([]int64); !ok || len(got) != 2 {
		t.Fatalf("arg = %#v, want []int64", res.Args[0])
	}
}

// customDialect proves a third-party dialect can drive rendering: it renders
// identifiers bare and uses named parameters.
type customDialect struct{ calls int }

func (d *customDialect) QuoteIdentifier(name string) string { return name }
func (d *customDialect) Placeholder(n int) string {
	return ":" + itoa(n)
}
func (d *customDialect) BuildArrayOp(op ArrayOp, col, placeholders string) string {
	d.calls++
	return buildExpandedArrayOp(op, col, placeholders, "ARRAY[", "IN (", "NOT IN (")
}
func (d *customDialect) CastFor(t FieldType) string { return postgresCast(t) }

func TestCustomDialect(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	d := &customDialect{}
	res := mustTranslate(t, NewTranslator(cfg, d), "name=a&age=in=(1,2)")
	// A dialect without the allocation-free path renders through the interface.
	if want := `(name = :1) AND (age IN (:2, :3))`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
	if d.calls == 0 {
		t.Fatal("custom dialect BuildArrayOp was not used")
	}
}
