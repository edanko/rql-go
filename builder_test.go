package rql

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// newTestContext returns the context used by the tests.
func newTestContext(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}

// testConfig is the configuration shared by the translation tests. It mirrors the
// field shapes an application typically declares.
func testConfig() *Config {
	cfg := NewConfig()
	cfg.Field("id", TypeUUID).Allow("eq", "in").Sortable().Selectable()
	cfg.Field("name", TypeString).Allow("like", "ilike").Sortable().Selectable()
	cfg.Field("status", TypeString).Allow("in")
	cfg.Field("age", TypeInt).Sortable().Selectable()
	cfg.Field("score", TypeFloat)
	cfg.Field("isActive", TypeBool).Column("is_active")
	cfg.Field("createdAt", TypeTime).Column("created_at").Sortable()
	cfg.Field("tags", TypeStringArray).Allow("contains", "containsall", "overlaps", "excludes")
	cfg.Field("meta", TypeJSON).Allow("containsall")
	cfg.Field("config", TypeString).Dynamic()
	return cfg
}

// mustTranslate translates query and fails the test on error.
func mustTranslate(t *testing.T, tr *Translator, query string) *Result {
	t.Helper()
	res, err := tr.Translate(newTestContext(t), query)
	if err != nil {
		t.Fatalf("Translate(%q): unexpected error: %v", query, err)
	}
	if res == nil {
		t.Fatalf("Translate(%q): nil result", query)
	}
	return res
}

// translateErr translates query and requires an error matching class.
func translateErr(t *testing.T, tr *Translator, query string, class error) *Error {
	t.Helper()
	res, err := tr.Translate(newTestContext(t), query)
	if err == nil {
		t.Fatalf("Translate(%q): expected an error, got Where=%q Args=%v", query, res.Where, res.Args)
	}
	if class != nil && !errors.Is(err, class) {
		t.Fatalf("Translate(%q): got %v, want a %v error", query, err, class)
	}
	var rqlErr *Error
	if !errors.As(err, &rqlErr) {
		t.Fatalf("Translate(%q): error is not a *Error: %T", query, err)
	}
	return rqlErr
}

// TestGoldenWhere pins the rendered SQL for representative queries.
func TestGoldenWhere(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	cases := []struct {
		query string
		where string
		args  []any
	}{
		{"name=Item%20A", `"name" = $1`, []any{"Item A"}},
		{"name=abc", `"name" = $1`, []any{"abc"}},
		{"name=", `"name" = $1`, []any{""}},
		{"name='bar'", `"name" = $1`, []any{"bar"}},
		{"name=%27bar%27", `"name" = $1`, []any{"bar"}},
		{"name=a+b", `"name" = $1`, []any{"a b"}},
		{"age=42", `"age" = $1`, []any{int64(42)}},
		{"score=1.5", `"score" = $1`, []any{1.5}},
		{"isActive=true", `"is_active" = TRUE`, nil},
		{"isActive=false", `"is_active" = FALSE`, nil},
		{"isActive=ne=true", `"is_active" <> TRUE`, nil},
		{"age=null", `"age" IS NULL`, nil},
		{"age=ne=null", `"age" IS NOT NULL`, nil},
		{"age=gt=1", `"age" > $1`, []any{int64(1)}},
		{"age>=1", `"age" >= $1`, []any{int64(1)}},
		{"age<1", `"age" < $1`, []any{int64(1)}},
		{"age<=1", `"age" <= $1`, []any{int64(1)}},
		{"age!=1", `"age" <> $1`, []any{int64(1)}},
		{"name=like=ab*", `"name" LIKE $1`, []any{"ab%"}}, // '*' is the RQL wildcard for like
		{"name=ilike=ab", `"name" ILIKE $1`, []any{"ab"}},
		{"name=%3C10", `"name" < $1`, []any{"10"}},
		{"age=%3C=10", `"age" <= $1`, []any{int64(10)}},
		{"age=%3E=10", `"age" >= $1`, []any{int64(10)}},
		{"age=in=(1,2)", `"age" IN ($1, $2)`, []any{int64(1), int64(2)}},
		{"age=out=(1,2)", `"age" NOT IN ($1, $2)`, []any{int64(1), int64(2)}},
		{"tags=contains=(go)", `$1 = ANY("tags")`, []any{"go"}},
		{"contains(tags,go)", `$1 = ANY("tags")`, []any{"go"}},
		{"tags=contains=(go,rs)", `"tags" && ARRAY[$1, $2]`, []any{"go", "rs"}},
		{"tags=containsall=(go,rs)", `"tags" @> ARRAY[$1, $2]`, []any{"go", "rs"}},
		{"tags=overlaps=(a,b)", `"tags" && ARRAY[$1, $2]`, []any{"a", "b"}},
		{"tags=excludes=(a)", `NOT ($1 = ANY("tags"))`, []any{"a"}},
		{"name=a&age=1", `("name" = $1) AND ("age" = $2)`, []any{"a", int64(1)}},
		{"name=a|age=1", `("name" = $1) OR ("age" = $2)`, []any{"a", int64(1)}},
		{"(name=a|age=1)&isActive=true", `(("name" = $1) OR ("age" = $2)) AND ("is_active" = TRUE)`, []any{"a", int64(1)}},
		{"name=a,age=1", `("name" = $1) AND ("age" = $2)`, []any{"a", int64(1)}},
		{"not(age=1)", `NOT ("age" = $1)`, []any{int64(1)}},
		{"not(not(age=1))", `"age" = $1`, []any{int64(1)}},
		{"not(age=1&isActive=true)", `(NOT ("age" = $1)) OR (NOT ("is_active" = TRUE))`, []any{int64(1)}},
		{"id=8a05d590-e085-4245-a164-ad9a5d4654b3", `"id" = $1`, []any{"8a05d590-e085-4245-a164-ad9a5d4654b3"}},
		{"createdAt=2021-06-09T11:00:00Z", `"created_at" = $1`, nil}, // compared separately
		{"config/theme=mode", `"config"->>'theme' = $1`, []any{"mode"}},
		{"tags=eq=(a,b)", `"tags" = $1`, nil}, // slice argument, compared separately
		{"", "", nil},
		{"distinct()", "", nil},
	}
	for _, tc := range cases {
		res := mustTranslate(t, tr, tc.query)
		if res.Where != tc.where {
			t.Fatalf("%q: Where = %q, want %q", tc.query, res.Where, tc.where)
		}
		if tc.args != nil && fmt.Sprint(res.Args) != fmt.Sprint(tc.args) {
			t.Fatalf("%q: Args = %#v, want %#v", tc.query, res.Args, tc.args)
		}
	}
}

// TestGoldenTypedArguments checks the Go type bound for each field type.
func TestGoldenTypedArguments(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})

	res := mustTranslate(t, tr, "createdAt=2021-06-09T11:00:00Z")
	if len(res.Args) != 1 {
		t.Fatalf("Args = %#v", res.Args)
	}
	if _, ok := res.Args[0].(time.Time); !ok {
		t.Fatalf("createdAt arg is %T, want a time.Time", res.Args[0])
	}

	res = mustTranslate(t, tr, "tags=eq=(a,b)")
	if len(res.Args) != 1 {
		t.Fatalf("Args = %#v", res.Args)
	}
	if got, ok := res.Args[0].([]string); !ok || len(got) != 2 || got[0] != "a" {
		t.Fatalf("tags arg = %#v, want []string{a,b}", res.Args[0])
	}

	res = mustTranslate(t, tr, "age=9007199254740993")
	if len(res.Args) != 1 || res.Args[0] != int64(9007199254740993) {
		t.Fatalf("large int arg = %#v", res.Args)
	}
}

// TestShapingOperators covers select, sort, limit, offset, distinct and count.
func TestShapingOperators(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})

	res := mustTranslate(t, tr, "select(id,name)&sort(-age,+name)&limit(50,100)&distinct()")
	if got := res.SelectString(); got != `"id", "name"` {
		t.Fatalf("Select = %q", got)
	}
	if len(res.Sort) != 2 || res.Sort[0].Column != "age" || !res.Sort[0].Desc || res.Sort[1].Column != "name" || res.Sort[1].Desc {
		t.Fatalf("Sort = %#v", res.Sort)
	}
	if got := res.OrderBy(); got != `"age" DESC, "name" ASC` {
		t.Fatalf("OrderBy = %q", got)
	}
	if !res.HasLimit() || res.LimitOrDefault(0) != 50 {
		t.Fatalf("Limit = %v", res.Limit)
	}
	if !res.HasOffset() || res.OffsetOrDefault(0) != 100 {
		t.Fatalf("Offset = %v", res.Offset)
	}
	if !res.Distinct || res.Count {
		t.Fatalf("Distinct = %v, Count = %v", res.Distinct, res.Count)
	}
}

func TestLimitForms(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})

	res := mustTranslate(t, tr, "limit(10)")
	if res.LimitOrDefault(-1) != 10 || res.HasOffset() {
		t.Fatalf("limit(10): Limit=%v Offset=%v", res.Limit, res.Offset)
	}
	res = mustTranslate(t, tr, "limit(10,5)")
	if res.LimitOrDefault(-1) != 10 || res.OffsetOrDefault(-1) != 5 {
		t.Fatalf("limit(10,5): Limit=%v Offset=%v", res.Limit, res.Offset)
	}
	// maxCount clamps the count: limit(10,5,3) means at most 3 rows.
	res = mustTranslate(t, tr, "limit(10,5,3)")
	if res.LimitOrDefault(-1) != 3 || res.OffsetOrDefault(-1) != 5 {
		t.Fatalf("limit(10,5,3): Limit=%v Offset=%v", res.Limit, res.Offset)
	}
	// A larger maxCount leaves the count alone.
	res = mustTranslate(t, tr, "limit(10,5,20)")
	if res.LimitOrDefault(-1) != 10 {
		t.Fatalf("limit(10,5,20): Limit=%v", res.Limit)
	}
	res = mustTranslate(t, tr, "limit(0)")
	if !res.HasLimit() || res.LimitOrDefault(-1) != 0 {
		t.Fatalf("limit(0): Limit=%v", res.Limit)
	}
}

func TestSortLegacyTwoArgumentForm(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	res := mustTranslate(t, tr, "sort(name,desc)")
	if got := res.OrderBy(); got != `"name" DESC` {
		t.Fatalf("OrderBy = %q", got)
	}
	res = mustTranslate(t, tr, "sort(name,asc)")
	if got := res.OrderBy(); got != `"name" ASC` {
		t.Fatalf("OrderBy = %q", got)
	}
}

func TestSortOnNestedPath(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("config", TypeJSON).Dynamic().Sortable()
	tr := NewTranslator(cfg, Postgres{})
	res := mustTranslate(t, tr, "sort(-config/theme)")
	if len(res.Sort) != 1 || res.Sort[0].Column != "config" || len(res.Sort[0].Path) != 1 || !res.Sort[0].Desc {
		t.Fatalf("Sort = %#v", res.Sort)
	}
	if got := res.OrderBy(); got != `"config"->>'theme' DESC` {
		t.Fatalf("OrderBy = %q", got)
	}
}

func TestJSONBPathCast(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("config", TypeJSON).Dynamic()
	tr := NewTranslator(cfg, Postgres{})

	cases := map[string]string{
		"config/age=42":                  `("config"->>'age')::int8 = $1`,
		"config/flag=true":               `("config"->>'flag')::bool = TRUE`,
		"config/name=x":                  `"config"->>'name' = $1`,
		"config/at=2021-01-01T00:00:00Z": `("config"->>'at')::timestamptz = $1`,
	}
	for query, want := range cases {
		if got := mustTranslate(t, tr, query).Where; got != want {
			t.Fatalf("%q: Where = %q, want %q", query, got, want)
		}
	}

	// A three-level path uses '->' for each inner segment and '->>' for the leaf.
	if got := mustTranslate(t, tr, "config/a/b/c=x").Where; got != `"config"->'a'->'b'->>'c' = $1` {
		t.Fatalf("nested Where = %q", got)
	}
}

func TestFlagsFromCustomOperator(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.RegisterBinaryOp("showArchived", func(c *OpContext, _ []any) (string, error) {
		v, err := c.Bool()
		if err != nil {
			return "", err
		}
		c.Flag("showArchived", v)
		return "", nil
	})
	tr := NewTranslator(cfg, Postgres{})

	res := mustTranslate(t, tr, "name=x&showArchived=true")
	// The conjunction keeps the surviving member parenthesised: dropping a
	// member that produced no SQL leaves the group's parentheses in place.
	if res.Where != `("name" = $1)` {
		t.Fatalf("Where = %q", res.Where)
	}
	if v, ok := res.BoolFlag("showArchived"); !ok || !v {
		t.Fatalf("flag = %v, %v", v, ok)
	}
	// A flag-only operator contributes no predicate.
	res = mustTranslate(t, tr, "showArchived=false")
	if res.Where != "" {
		t.Fatalf("Where = %q, want empty", res.Where)
	}
	if v, ok := res.BoolFlag("showArchived"); !ok || v {
		t.Fatalf("flag = %v, %v", v, ok)
	}
}

func TestCustomOperatorBindingsAndShaping(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.Field("name", TypeString).Allow("like", "ilike", "inlike").Sortable().Selectable()
	cfg.RegisterBinaryOp("inlike", func(c *OpContext, args []any) (string, error) {
		col, err := c.Column(args[0].(string))
		if err != nil {
			return "", err
		}
		values := c.Values()
		if len(values) != 1 || values[0].Kind != KindArray {
			return "", errors.New("inlike needs an array")
		}
		items := values[0].Array
		out := col + " LIKE ANY(ARRAY["
		for i := range items {
			if i > 0 {
				out += ", "
			}
			out += c.Arg(items[i].Any())
		}
		return out + "])", nil
	})
	cfg.RegisterUnaryOp("offset", func(c *OpContext, _ []any) (string, error) {
		n, err := c.Int()
		if err != nil {
			return "", err
		}
		c.SetOffset(int(n))
		return "", nil
	})
	tr := NewTranslator(cfg, Postgres{})

	res := mustTranslate(t, tr, "name=inlike=(a,b)")
	if want := `"name" LIKE ANY(ARRAY[$1, $2])`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
	if fmt.Sprint(res.Args) != fmt.Sprint([]any{"a", "b"}) {
		t.Fatalf("Args = %#v", res.Args)
	}

	res = mustTranslate(t, tr, "name=x&offset(20)")
	if res.OffsetOrDefault(-1) != 20 {
		t.Fatalf("Offset = %v", res.Offset)
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tr.Translate(ctx, "name=x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestNilTranslatorInputs(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(nil, nil)
	if _, err := tr.Translate(context.Background(), "name=x"); !errors.Is(err, ErrUnknownField) {
		t.Fatalf("got %v, want ErrUnknownField (there is no pass-through mode)", err)
	}
	if tr.Config() == nil || tr.Dialect() == nil {
		t.Fatal("NewTranslator(nil, nil) should supply defaults")
	}
	// A nil context is treated as Background rather than panicking.
	//nolint:staticcheck // deliberately checking that a nil context is tolerated.
	if _, err := NewTranslator(testConfig(), Postgres{}).Translate(nil, "name=x"); err != nil {
		t.Fatalf("nil context: %v", err)
	}
}

func TestEmptyAndShapingOnlyQueries(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	for _, query := range []string{"", "sort(-age)", "limit(5)", "select(id)"} {
		res := mustTranslate(t, tr, query)
		if res.Where != "" {
			t.Fatalf("%q: Where = %q, want empty", query, res.Where)
		}
		if res.Args != nil {
			t.Fatalf("%q: Args = %#v, want none", query, res.Args)
		}
	}
}

// TestLikeWildcardTranslation pins the '*' -> '%' translation for LIKE/ILIKE. The legacy
// translator applied it and clients send '*'; passing it through would match a literal
// asterisk and silently return no rows.
func TestLikeWildcardTranslation(t *testing.T) {
	t.Parallel()

	cfg := NewConfig()
	cfg.Field("name", TypeString)
	tr := NewTranslator(cfg, Postgres{})
	ctx := t.Context()

	tests := []struct {
		name    string
		query   string
		wantArg string
	}{
		{name: "trailing wildcard", query: "name=like=Jo*", wantArg: "Jo%"},
		{name: "leading wildcard", query: "name=like=*o*", wantArg: "%o%"},
		{name: "multiple wildcards ilike", query: "name=ilike=A*ce*", wantArg: "A%ce%"},
		{name: "only wildcard", query: "name=like=*", wantArg: "%"},
		{name: "sql percent passes through", query: "name=like=Jo%25e", wantArg: "Jo%e"},
		{name: "star is literal for eq", query: "name=Jo*", wantArg: "Jo*"},
		{name: "star is literal for in", query: "name=in=(Jo*,Bo*)", wantArg: "Jo*"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := tr.Translate(ctx, tt.query)
			if err != nil {
				t.Fatalf("Translate(%q): %v", tt.query, err)
			}
			if len(res.Args) == 0 {
				t.Fatalf("Translate(%q) produced no args", tt.query)
			}
			if got, ok := res.Args[0].(string); !ok || got != tt.wantArg {
				t.Fatalf("Translate(%q) arg = %#v, want %q", tt.query, res.Args[0], tt.wantArg)
			}
		})
	}
}
