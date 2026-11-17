package rql

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestNewConfigDefaults(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	if cfg.MaxDepth != defaultMaxDepth {
		t.Fatalf("MaxDepth = %d, want %d", cfg.MaxDepth, defaultMaxDepth)
	}
	if cfg.MaxInputLen != defaultMaxInputLen || cfg.MaxTokens != defaultMaxTokens {
		t.Fatalf("limits = %d/%d", cfg.MaxInputLen, cfg.MaxTokens)
	}
	// A zero value must fall back to the defaults rather than disable them.
	zero := &Config{}
	if zero.maxDepth() != defaultMaxDepth || zero.maxInputLen() != defaultMaxInputLen || zero.maxTokens() != defaultMaxTokens {
		t.Fatal("zero Config should fall back to the defaults")
	}
}

func TestFieldColumnDefaultsToSnakeCase(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("createdAt", TypeTime)
	cfg.Field("simple", TypeString)
	cfg.Field("organizationID", TypeString) // legacy quirk must be preserved
	cfg.Field("nested.leafKey", TypeBool)
	cfg.Field("full_name", TypeString)

	cases := map[string][]string{
		"createdAt":      {"created_at"},
		"simple":         {"simple"},
		"organizationID": {"organization_i_d"},
		"nested.leafKey": {"nested", "leafKey"},
		"full_name":      {"full_name"},
	}
	for name, want := range cases {
		f := cfg.lookupField(name)
		if f == nil {
			t.Fatalf("field %q not registered", name)
		}
		if len(f.parts) != len(want) {
			t.Fatalf("%s parts = %v, want %v", name, f.parts, want)
		}
		for i := range want {
			if f.parts[i] != want[i] {
				t.Fatalf("%s parts = %v, want %v", name, f.parts, want)
			}
		}
	}
}

func TestFieldBuilderOptions(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("id", TypeUUID).Allow("eq", "in").Forbid("in").Sortable().Selectable().Column("identifier")
	cfg.Field("age", TypeInt).Validator(func(v any) error {
		if n, ok := v.(int64); ok && n < 0 {
			return errors.New("must not be negative")
		}
		return nil
	})

	f := cfg.lookupField("id")
	if !f.sortable || !f.selectable {
		t.Fatal("sortable/selectable not set")
	}
	if !f.allows("eq") {
		t.Fatal("eq should be allowed")
	}
	if f.allows("in") {
		t.Fatal("in should have been forbidden")
	}
	if f.parts[0] != "identifier" {
		t.Fatalf("column = %v, want identifier", f.parts)
	}

	tr := NewTranslator(cfg, Postgres{})
	if _, err := tr.Translate(newTestContext(t), "age=1"); err != nil {
		t.Fatalf("validator should accept 1: %v", err)
	}
	if _, err := tr.Translate(newTestContext(t), "age=-1"); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("validator should reject -1: %v", err)
	}
}

func TestMapOpAppliesToSugarAndFIQL(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("createdAtFrom", TypeTime).Column("created_at").MapOp("eq", "ge")
	cfg.Field("createdAtTo", TypeTime).Column("created_at").MapOp("eq", "le")
	tr := NewTranslator(cfg, Postgres{})

	for _, query := range []string{"createdAtFrom=2021-06-09T11:00:00Z", "createdAtFrom=eq=2021-06-09T11:00:00Z"} {
		res := mustTranslate(t, tr, query)
		if want := `"created_at" >= $1`; res.Where != want {
			t.Fatalf("%s: Where = %q, want %q", query, res.Where, want)
		}
	}
	res := mustTranslate(t, tr, "createdAtTo=2021-06-09T11:00:00Z")
	if want := `"created_at" <= $1`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
	// MapOp enables both spellings on the field.
	f := cfg.lookupField("createdAtFrom")
	if !f.allows("eq") || !f.allows("ge") {
		t.Fatalf("MapOp should allow both operators, got %v", f.allowed)
	}
}

func TestOpColumnPerOperator(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("q", TypeString).Column("col_default").OpColumn("eq", "col_eq").OpColumn("like", "col_like")
	tr := NewTranslator(cfg, Postgres{})

	if res := mustTranslate(t, tr, "q=eq=x"); res.Where != `"col_eq" = $1` {
		t.Fatalf("eq column = %q", res.Where)
	}
	if res := mustTranslate(t, tr, "q=like=x"); res.Where != `"col_like" LIKE $1` {
		t.Fatalf("like column = %q", res.Where)
	}
	// An operator without an override uses the field column.
	plain := NewConfig()
	plain.Field("q", TypeString).Column("col_default").Allow("gt")
	if res := mustTranslate(t, NewTranslator(plain, Postgres{}), "q=gt=x"); res.Where != `"col_default" > $1` {
		t.Fatalf("gt column = %q", res.Where)
	}
}

func TestOpTypeOverride(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("mixed", TypeString).OpType("gt", TypeInt)
	tr := NewTranslator(cfg, Postgres{})

	// The override makes gt compare as an integer, so a non-numeric value fails.
	if _, err := tr.Translate(newTestContext(t), "mixed=gt=abc"); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("got %v, want ErrInvalidValue", err)
	}
	res := mustTranslate(t, tr, "mixed=gt=42")
	if want := `"mixed" > $1`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
	if len(res.Args) != 1 || res.Args[0] != int64(42) {
		t.Fatalf("Args = %#v, want int64(42)", res.Args)
	}
}

func TestDynamicPathRoots(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("name", TypeString)
	cfg.AllowDynamicPaths("config")
	cfg.Field("container.leaf", TypeBool)
	tr := NewTranslator(cfg, Postgres{})

	// A dynamic root accepts arbitrary (validated) segments.
	res := mustTranslate(t, tr, "config/theme=mode")
	if want := `"config"->>'theme' = $1`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
	// A plain field is a column, not a JSON container.
	if _, err := tr.Translate(newTestContext(t), "name/DROP=1"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("plain field with a path: got %v, want ErrInvalidPath", err)
	}
	// A leaf registered with dots is reachable both ways.
	for _, query := range []string{"container.leaf=true", "container/leaf=true"} {
		got := mustTranslate(t, tr, query).Where
		if want := `("container"->>'leaf')::bool = TRUE`; got != want {
			t.Fatalf("%s: Where = %q, want %q", query, got, want)
		}
	}
}

func TestCustomOperatorRegistry(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.RegisterUnaryOp("myOp", func(*OpContext, []any) (string, error) { return "TRUE", nil })
	cfg.RegisterBinaryOp("myBin", func(*OpContext, []any) (string, error) { return "FALSE", nil })
	cfg.RegisterOp("later", UnaryOp, func(*OpContext, []any) (string, error) { return "TRUE", nil })
	cfg.DeleteOp("later")

	if cfg.lookupCustom("myop") == nil {
		t.Fatal("RegisterUnaryOp registered nothing")
	}
	if cop := cfg.lookupCustom("mybin"); cop == nil || cop.kind != BinaryOp {
		t.Fatal("RegisterBinaryOp registered the wrong kind")
	}
	if cfg.lookupCustom("later") != nil {
		t.Fatal("DeleteOp did not remove the operator")
	}
	// A nil implementation must not panic or register.
	cfg.RegisterOp("nil", UnaryOp, nil)
	if cfg.lookupCustom("nil") != nil {
		t.Fatal("a nil operator must not be registered")
	}
}

func TestCustomOperatorOverridesBuiltin(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("name", TypeString)
	cfg.RegisterBinaryOp("eq", func(c *OpContext, args []any) (string, error) {
		col, err := c.Column(args[0].(string))
		if err != nil {
			return "", err
		}
		return col + " = " + c.Arg(args[1]) + " /*custom*/", nil
	})
	tr := NewTranslator(cfg, Postgres{})
	res := mustTranslate(t, tr, "name=abc")
	if want := `"name" = $1 /*custom*/`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
}

func TestPathSegmentValidation(t *testing.T) {
	t.Parallel()
	valid := []string{"a", "A", "_x", "a1", "cameraEnabled"}
	for _, s := range valid {
		if !isPathSegment(s) {
			t.Fatalf("%q should be a valid segment", s)
		}
	}
	invalid := []string{"", "1a", "a-b", "a b", "a'b", `a"b`, "a->b", "a;b", "a/b", "a.b", "об"}
	for _, s := range invalid {
		if isPathSegment(s) {
			t.Fatalf("%q should be rejected", s)
		}
	}
}

func TestFieldRegistrationRejectsBadSegments(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("root.bad-segment", TypeBool)
	tr := NewTranslator(cfg, Postgres{})
	if _, err := tr.Translate(newTestContext(t), "root.bad-segment=true"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("got %v, want ErrInvalidPath", err)
	}
}

// TestSplitPathPreservesSeparators proves path splitting never silently
// normalises segments away: an empty, "." or ".." segment is an error rather
// than being dropped (docs/DESIGN.md §3 rule 6).
func TestSplitPathPreservesSeparators(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.AllowDynamicPaths("dyn")
	tr := NewTranslator(cfg, Postgres{})

	hostile := []string{
		"dyn//x=1",
		"dyn/../x=1",
		"dyn/./x=1",
		"dyn/a..b=1",
		"dyn/%2E%2E/x=1",
		"dyn/%2E/x=1",
	}
	for _, query := range hostile {
		if _, err := tr.Translate(newTestContext(t), query); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("%q: got %v, want ErrInvalidPath", query, err)
		}
	}

	// The valid spellings still resolve, and each segment keeps its meaning.
	for query, want := range map[string]string{
		"dyn/x=1":   `"dyn"->>'x' = $1`,
		"dyn/a/b=1": `"dyn"->'a'->>'b' = $1`,
	} {
		if got := mustTranslate(t, tr, query).Where; got != want {
			t.Fatalf("%q: Where = %q, want %q", query, got, want)
		}
	}

	// Registration rejects the same shapes instead of silently collapsing them.
	for _, name := range []string{".x", "/x", "a..b", "a//b", "a."} {
		cfg2 := NewConfig()
		cfg2.Field(name, TypeString)
		if _, err := NewTranslator(cfg2, Postgres{}).Translate(newTestContext(t), name+"=1"); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("registering %q: got %v, want ErrInvalidPath", name, err)
		}
	}
}

// TestFieldBuilderChaining documents the fluent path from a Field declaration back to the
// configuration: without it, a multi-field Config cannot be written as one expression.
func TestFieldBuilderChaining(t *testing.T) {
	t.Parallel()

	cfg := NewConfig().
		Field("id", TypeInt).Allow("in").
		Field("name", TypeString).Column("full_name").Sortable().
		Field("createdAt", TypeTime).Column("created_at").Sortable().
		Config()

	if cfg == nil {
		t.Fatal("Config() returned nil")
	}
	tr := NewTranslator(cfg, Postgres{})
	res, err := tr.Translate(t.Context(), "id=in=(1,2)&name=X&sort(-createdAt)")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if want := `("id" IN ($1, $2)) AND ("full_name" = $3)`; res.Where != want {
		t.Fatalf("where = %q, want %q", res.Where, want)
	}
	if got := res.OrderBy(); got != `"created_at" DESC` {
		t.Fatalf("orderBy = %q", got)
	}

	// A chained field must still be independently configurable after the chain continues.
	cfg2 := NewConfig().Field("a", TypeString).Allow("like").Field("b", TypeBool).Config()
	if _, err := NewTranslator(cfg2, Postgres{}).Translate(t.Context(), "a=like=x*&b=true"); err != nil {
		t.Fatalf("Translate chained config: %v", err)
	}
}

// allowedStatusKey is the request-scoped value the validator tests read.
type allowedStatusKey struct{}

// TestValidatorContextReadsRequestValues proves a validator can depend on the request context,
// which is what makes rules like "this tenant may only filter by these statuses" expressible.
func TestValidatorContextReadsRequestValues(t *testing.T) {
	t.Parallel()

	cfg := NewConfig()
	cfg.Field("status", TypeString).ValidatorContext(func(ctx context.Context, v any) error {
		allowed, _ := ctx.Value(allowedStatusKey{}).([]string)
		if len(allowed) == 0 {
			return ErrInvalidValue // fail closed: no policy in the request
		}
		status, _ := v.(string)
		for _, a := range allowed {
			if a == status {
				return nil
			}
		}
		return fmt.Errorf("status %q is not allowed for this tenant", status)
	})
	tr := NewTranslator(cfg, Postgres{})

	ctx := context.WithValue(t.Context(), allowedStatusKey{}, []string{"active", "inactive"})

	tests := []struct {
		name    string
		ctx     context.Context
		query   string
		wantErr bool
	}{
		{name: "allowed status", ctx: ctx, query: "status=active"},
		{name: "allowed second status", ctx: ctx, query: "status=inactive"},
		{name: "rejected status", ctx: ctx, query: "status=archived", wantErr: true},
		{name: "no policy in context fails closed", ctx: t.Context(), query: "status=active", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tr.Translate(tt.ctx, tt.query)
			if tt.wantErr && err == nil {
				t.Fatalf("Translate(%q) succeeded, want an error", tt.query)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Translate(%q): %v", tt.query, err)
			}
			if tt.wantErr && !errors.Is(err, ErrInvalidValue) {
				t.Fatalf("error = %v, want ErrInvalidValue", err)
			}
		})
	}
}

// TestValidatorContextSeesArrayElements pins that an array value is validated per element and
// that each call receives the same request context.
func TestValidatorContextSeesArrayElements(t *testing.T) {
	t.Parallel()

	var seenContexts, seenValues int
	cfg := NewConfig()
	cfg.Field("status", TypeString).Allow("in").
		ValidatorContext(func(ctx context.Context, v any) error {
			if ctx == nil {
				return fmt.Errorf("nil context")
			}
			if _, ok := ctx.Value(allowedStatusKey{}).([]string); !ok {
				return fmt.Errorf("context value missing")
			}
			seenContexts++
			seenValues++
			if v == "archived" {
				return fmt.Errorf("archived is not allowed")
			}
			return nil
		})
	tr := NewTranslator(cfg, Postgres{})
	ctx := context.WithValue(t.Context(), allowedStatusKey{}, []string{"active"})

	if _, err := tr.Translate(ctx, "status=in=(active,inactive)"); err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if seenValues != 2 || seenContexts != 2 {
		t.Fatalf("validator calls = %d (contexts %d), want 2 with the request context", seenValues, seenContexts)
	}

	if _, err := tr.Translate(ctx, "status=in=(active,archived)"); err == nil {
		t.Fatal("Translate accepted an array containing a rejected element")
	}
}

// TestValidatorContextNilSafety ensures a nil context can never reach a validator, so a check
// can call ctx.Value unconditionally.
func TestValidatorContextNilSafety(t *testing.T) {
	t.Parallel()

	var got context.Context
	cfg := NewConfig()
	cfg.Field("status", TypeString).ValidatorContext(func(ctx context.Context, _ any) error {
		got = ctx
		return nil
	})
	tr := NewTranslator(cfg, Postgres{})

	//nolint:staticcheck // deliberately exercising the supported nil-context path
	if _, err := tr.Translate(nil, "status=active"); err != nil {
		t.Fatalf("Translate with nil context: %v", err)
	}
	if got == nil {
		t.Fatal("validator received a nil context")
	}
}

// TestValidatorAndValidatorContextReplaceEachOther documents that the two methods configure the
// same slot: the last call wins, so a config cannot accidentally run two checks.
func TestValidatorAndValidatorContextReplaceEachOther(t *testing.T) {
	t.Parallel()

	var called string
	cfg := NewConfig()
	cfg.Field("status", TypeString).
		Validator(func(any) error { called = "simple"; return nil }).
		ValidatorContext(func(context.Context, any) error { called = "context"; return nil })

	if _, err := NewTranslator(cfg, Postgres{}).Translate(t.Context(), "status=active"); err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if called != "context" {
		t.Fatalf("called = %q, want the last-installed validator", called)
	}
}
