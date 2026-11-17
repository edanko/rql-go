package rql

import (
	"errors"
	"strings"
	"testing"
)

func TestResultHelpers(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	res := mustTranslate(t, tr, "name=a&sort(-age)&select(id)&limit(3,4)&distinct()")

	if got := res.WhereString(); got != `"name" = $1` {
		t.Fatalf("WhereString = %q", got)
	}
	if got := res.GetValues(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("GetValues = %#v", got)
	}
	if got := res.SelectString(); got != `"id"` {
		t.Fatalf("SelectString = %q", got)
	}
	if got := res.OrderBy(); got != `"age" DESC` {
		t.Fatalf("OrderBy = %q", got)
	}
	if got := res.LimitOrDefault(99); got != 3 {
		t.Fatalf("LimitOrDefault = %d", got)
	}
	if got := res.OffsetOrDefault(99); got != 4 {
		t.Fatalf("OffsetOrDefault = %d", got)
	}
	if !res.HasLimit() || !res.HasOffset() {
		t.Fatal("limit/offset should be reported present")
	}
}

func TestResultAbsentValues(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	res := mustTranslate(t, tr, "name=a")
	if res.HasLimit() || res.HasOffset() {
		t.Fatal("limit/offset should be absent")
	}
	if got := res.LimitOrDefault(25); got != 25 {
		t.Fatalf("LimitOrDefault = %d, want the default", got)
	}
	if got := res.OffsetOrDefault(7); got != 7 {
		t.Fatalf("OffsetOrDefault = %d, want the default", got)
	}
	if res.OrderBy() != "" || res.SelectString() != "" {
		t.Fatalf("OrderBy = %q, Select = %q", res.OrderBy(), res.SelectString())
	}
	if _, ok := res.BoolFlag("nope"); ok {
		t.Fatal("absent flag reported present")
	}
}

func TestResultNilReceiver(t *testing.T) {
	t.Parallel()
	var res *Result
	if res.WhereString() != "" || res.SelectString() != "" || res.OrderBy() != "" {
		t.Fatal("nil Result should render empty strings")
	}
	if res.LimitOrDefault(5) != 5 || res.OffsetOrDefault(6) != 6 {
		t.Fatal("nil Result should return the defaults")
	}
	if res.HasLimit() || res.HasOffset() {
		t.Fatal("nil Result has no limit/offset")
	}
	if _, ok := res.BoolFlag("x"); ok {
		t.Fatal("nil Result has no flags")
	}
	if len(res.GetValues()) != 0 {
		t.Fatal("nil Result has no args")
	}
}

func TestResultOrderByWithExplicitDialect(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), QuestionMark{})
	res := mustTranslate(t, tr, "sort(-age,+name)")
	if got := res.OrderBy(); got != `"age" DESC, "name" ASC` {
		t.Fatalf("OrderBy = %q", got)
	}
	// A custom dialect can render its own ordering.
	if got := res.OrderByWith(&customDialect{}); got != `age DESC, name ASC` {
		t.Fatalf("OrderByWith = %q", got)
	}
}

func TestOptimizerRewrites(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.MaxDepth = 64
	tr := NewTranslator(cfg, Postgres{})

	// Flattening: and(a, and(b, c)) renders flat.
	flat := mustTranslate(t, tr, "and(name=a,and(age=1,isActive=true))")
	want := `("name" = $1) AND ("age" = $2) AND ("is_active" = TRUE)`
	if flat.Where != want {
		t.Fatalf("Where = %q, want %q", flat.Where, want)
	}
	// The flattened form agrees with the sugar form.
	if sugar := mustTranslate(t, tr, "name=a&age=1&isActive=true"); sugar.Where != want {
		t.Fatalf("sugar Where = %q, want %q", sugar.Where, want)
	}

	// Double negation is eliminated.
	if got := mustTranslate(t, tr, "not(not(not(not(age=1))))").Where; got != `"age" = $1` {
		t.Fatalf("double negation Where = %q", got)
	}

	// De Morgan is applied to a negated conjunction.
	if got := mustTranslate(t, tr, "not(age=1&isActive=true)").Where; got != `(NOT ("age" = $1)) OR (NOT ("is_active" = TRUE))` {
		t.Fatalf("De Morgan Where = %q", got)
	}
}

// TestOptimizerDoesNotChangeSemantics pins the rewrites that must NOT happen.
func TestOptimizerDoesNotChangeSemantics(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	cases := map[string]string{
		// Repeated equality must not become IN (that flips AND to OR) nor a
		// containment test (invalid for a scalar column).
		"age=1&age=2": `("age" = $1) AND ("age" = $2)`,
		// or(eq,eq) must not be rewritten either: callers may depend on the shape.
		"or(age=1,age=2)": `("age" = $1) OR ("age" = $2)`,
		// A negated comparison stays a NOT, preserving three-valued logic.
		"not(age=1)": `NOT ("age" = $1)`,
	}
	for query, want := range cases {
		if got := mustTranslate(t, tr, query).Where; got != want {
			t.Fatalf("%q: Where = %q, want %q", query, got, want)
		}
	}
}

func TestErrorClassification(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})

	err := translateErr(t, tr, "nope=1", ErrUnknownField)
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("message = %q", err.Error())
	}
	if err.Position() != 0 {
		t.Fatalf("position = %d", err.Position())
	}
	var target *Error
	if !errors.As(error(err), &target) {
		t.Fatal("error is not a *Error")
	}
	// The aliases name the same type, so callers migrating from the previous API
	// keep working.
	var parseErr *ParseError
	if !errors.As(error(err), &parseErr) {
		t.Fatal("ParseError alias does not match")
	}
	var buildErr *BuildError
	if !errors.As(error(err), &buildErr) {
		t.Fatal("BuildError alias does not match")
	}
	// A non-matching sentinel must not match.
	if errors.Is(err, ErrMaxDepth) {
		t.Fatal("unrelated sentinel matched")
	}
}

func TestErrorMessageIncludesPosition(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	err := translateErr(t, tr, "name=a&nope=1", ErrUnknownField)
	msg := err.Error()
	if !strings.Contains(msg, "byte 7") {
		t.Fatalf("message %q should name the byte offset", msg)
	}
	if !strings.Contains(msg, `"nope"`) {
		t.Fatalf("message %q should quote the field", msg)
	}
}

func TestErrorUnwrapPreservesCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("validator says no")
	cfg := NewConfig()
	cfg.Field("age", TypeInt).Validator(func(any) error { return cause })
	tr := NewTranslator(cfg, Postgres{})

	_, err := tr.Translate(newTestContext(t), "age=1")
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is did not reach the validator error: %v", err)
	}
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("error should classify as ErrInvalidValue: %v", err)
	}
}

func TestCustomOperatorErrorPropagation(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.Field("name", TypeString).Allow("like", "ilike", "boom", "boom2", "silent", "inspect", "typed", "wantint", "jsoncast")
	cfg.RegisterBinaryOp("boom", func(*OpContext, []any) (string, error) {
		return "", ErrUnsupportedOperator
	})
	tr := NewTranslator(cfg, Postgres{})
	_ = translateErr(t, tr, "name=boom=x", ErrUnsupportedOperator)

	cfg2 := testConfig()
	cfg2.Field("name", TypeString).Allow("boom2")
	cfg2.RegisterBinaryOp("boom2", func(*OpContext, []any) (string, error) {
		return "", errors.New("plain failure")
	})
	err := translateErr(t, NewTranslator(cfg2, Postgres{}), "name=boom2=x", nil)
	if !strings.Contains(err.Error(), "plain failure") {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestCustomOperatorBindingWithoutSQLFailsClosed(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.Field("name", TypeString).Allow("silent")
	cfg.RegisterBinaryOp("silent", func(c *OpContext, args []any) (string, error) {
		c.Arg(args[1]) // binds a parameter...
		return "", nil // ...but emits no SQL
	})
	tr := NewTranslator(cfg, Postgres{})
	_ = translateErr(t, tr, "name=silent=x", ErrInvalidSyntax)
}
