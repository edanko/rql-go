package rql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestOpContextAccessors exercises the surface a custom operator receives.
func TestOpContextAccessors(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.Field("name", TypeString).Allow("inspect")
	var seen struct {
		operator  string
		field     string
		hasField  bool
		fieldType FieldType
		cast      string
		values    int
		args      []any
		position  int
		dialect   Dialect
	}
	cfg.RegisterBinaryOp("inspect", func(c *OpContext, args []any) (string, error) {
		seen.operator = c.Operator()
		seen.field, seen.hasField = c.Field()
		seen.fieldType = c.FieldType()
		seen.cast = c.Cast()
		seen.values = len(c.Values())
		seen.args = c.Args()
		seen.position = c.Position()
		seen.dialect = c.Dialect()

		col, err := c.Column(seen.field)
		if err != nil {
			return "", err
		}
		if v, ok := c.ValueAt(0); !ok || v.Kind != KindString {
			return "", errors.New("ValueAt(0) is not the string value")
		}
		return col + " = " + c.Arg(args[1]), nil
	})
	tr := NewTranslator(cfg, Postgres{})
	res := mustTranslate(t, tr, "name=inspect=x")

	if want := `"name" = $1`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
	if seen.operator != "inspect" || !seen.hasField || seen.field != "name" {
		t.Fatalf("operator/field = %q %q %v", seen.operator, seen.field, seen.hasField)
	}
	if seen.fieldType != TypeString {
		t.Fatalf("FieldType = %v", seen.fieldType)
	}
	if seen.cast != "" {
		t.Fatalf("Cast = %q, want empty for a text field", seen.cast)
	}
	if seen.values != 1 || len(seen.args) != 2 {
		t.Fatalf("values = %d, args = %#v", seen.values, seen.args)
	}
	if seen.position != 0 {
		t.Fatalf("Position = %d", seen.position)
	}
	if seen.dialect == nil {
		t.Fatal("Dialect is nil")
	}
}

func TestOpContextTypedAccessors(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.Field("name", TypeString).Allow("typed")
	cfg.Field("age", TypeInt).Allow("typed")
	cfg.Field("score", TypeFloat).Allow("typed")
	cfg.Field("isActive", TypeBool).Allow("typed")
	cfg.Field("createdAt", TypeTime).Allow("typed")
	results := map[string]string{}
	record := func(name string, _ any, err error) {
		if err != nil {
			results[name] = "error: " + err.Error()
			return
		}
		results[name] = "ok"
	}
	cfg.RegisterBinaryOp("typed", func(c *OpContext, args []any) (string, error) {
		switch args[0] {
		case "name":
			v, err := c.String()
			record("string", v, err)
		case "age":
			v, err := c.Int()
			record("int", v, err)
		case "score":
			v, err := c.Float()
			record("float", v, err)
		case "isActive":
			v, err := c.Bool()
			record("bool", v, err)
		case "createdAt":
			v, err := c.Time()
			record("time", v, err)
		}
		return "", nil
	})
	tr := NewTranslator(cfg, Postgres{})

	for _, query := range []string{
		"name=typed=x", "age=typed=1", "score=typed=1.5",
		"isActive=typed=true", "createdAt=typed=2021-01-01T00:00:00Z",
	} {
		if _, err := tr.Translate(newTestContext(t), query); err != nil {
			t.Fatalf("%q: %v", query, err)
		}
	}
	for _, name := range []string{"string", "int", "float", "bool", "time"} {
		if results[name] != "ok" {
			t.Fatalf("%s accessor: %q", name, results[name])
		}
	}

	// A mistyped accessor returns an ErrInvalidValue error, not a panic.
	cfg2 := testConfig()
	cfg2.Field("name", TypeString).Allow("wantint")
	var accessErr error
	cfg2.RegisterBinaryOp("wantint", func(c *OpContext, _ []any) (string, error) {
		_, accessErr = c.Int()
		return "", nil
	})
	_, _ = NewTranslator(cfg2, Postgres{}).Translate(newTestContext(t), "name=wantint=abc")
	if accessErr == nil || !errors.Is(accessErr, ErrInvalidValue) {
		t.Fatalf("Int() on a string: %v", accessErr)
	}
}

func TestOpContextColumnErrors(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.RegisterUnaryOp("badcol", func(*OpContext, []any) (string, error) { return "TRUE", nil })
	var colErr error
	cfg.RegisterUnaryOp("badcol", func(c *OpContext, _ []any) (string, error) {
		_, colErr = c.Column("nope")
		return "", nil
	})
	_, _ = NewTranslator(cfg, Postgres{}).Translate(newTestContext(t), "badcol()")
	if colErr == nil || !errors.Is(colErr, ErrUnknownField) {
		t.Fatalf("Column(nope) = %v, want ErrUnknownField", colErr)
	}
}

func TestOpContextSortLimitOffset(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.RegisterUnaryOp("shape", func(*OpContext, []any) (string, error) { return "", nil })
	cfg.RegisterUnaryOp("shape", func(c *OpContext, _ []any) (string, error) {
		if len(c.Sort()) != 0 {
			return "", errors.New("sort should start empty")
		}
		c.SetSort(SortField{Column: "name", Desc: true})
		if _, ok := c.Limit(); ok {
			return "", errors.New("limit should start absent")
		}
		c.SetLimit(7)
		c.SetOffset(3)
		if n, ok := c.Offset(); !ok || n != 3 {
			return "", errors.New("offset was not stored")
		}
		return "", nil
	})
	tr := NewTranslator(cfg, Postgres{})
	res := mustTranslate(t, tr, "shape()")
	if len(res.Sort) != 1 || res.Sort[0].Column != "name" || !res.Sort[0].Desc {
		t.Fatalf("Sort = %#v", res.Sort)
	}
	if res.LimitOrDefault(-1) != 7 || res.OffsetOrDefault(-1) != 3 {
		t.Fatalf("Limit=%v Offset=%v", res.Limit, res.Offset)
	}
}

// TestOpContextCastForJSONLeaf proves Cast() reports the cast a custom operator
// needs for a JSONB leaf.
func TestOpContextCastForJSONLeaf(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("config", TypeJSON).Dynamic().Allow("jsoncast")
	var casts []string
	cfg.RegisterBinaryOp("jsoncast", func(c *OpContext, _ []any) (string, error) {
		casts = append(casts, c.Cast())
		col, err := c.Column("config")
		if err != nil {
			return "", err
		}
		return col + " IS NOT NULL", nil
	})
	tr := NewTranslator(cfg, Postgres{})

	for _, query := range []string{"config=jsoncast=1", "config=jsoncast=true"} {
		if _, err := tr.Translate(newTestContext(t), query); err != nil {
			t.Fatalf("%q: %v", query, err)
		}
	}
	if len(casts) != 2 || casts[0] != "::int8" || casts[1] != "::bool" {
		t.Fatalf("casts = %#v, want ::int8 then ::bool", casts)
	}
}

func TestOpContextUnaryValues(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.RegisterUnaryOp("multi", func(*OpContext, []any) (string, error) { return "", nil })
	cfg.RegisterUnaryOp("multi", func(c *OpContext, args []any) (string, error) {
		if len(args) != 3 {
			return "", errors.New("expected 3 arguments")
		}
		if len(c.Values()) != 3 {
			return "", errors.New("Values() disagrees with args")
		}
		out := c.Arg(valueAny(c, 0)) + " = " + c.Arg(valueAny(c, 1)) + " = " + c.Arg(valueAny(c, 2))
		return out, nil
	})
	tr := NewTranslator(cfg, Postgres{})
	res := mustTranslate(t, tr, "multi(1,2,3)")
	if want := `$1 = $2 = $3`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
	if len(res.Args) != 3 {
		t.Fatalf("Args = %#v", res.Args)
	}
}

// valueAny returns the i-th operator value as a Go value.
func valueAny(c *OpContext, i int) any {
	v, _ := c.ValueAt(i)
	return v.Any()
}

// tenantKey is the kind of request-scoped value an operator typically needs: attached with
// context.WithValue by the caller and read inside the operator.
type tenantKey struct{}

// TestOpContextContextGivesRequestValues proves a custom operator can read request-scoped
// values (tenant, request id, ...) and bind them as parameters, which is the reason the
// context is threaded through Translate in the first place.
func TestOpContextContextGivesRequestValues(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.RegisterUnaryOp("tenant", func(c *OpContext, _ []any) (string, error) {
		tenant, ok := c.Context().Value(tenantKey{}).(string)
		if !ok || tenant == "" {
			// Fail closed: a missing tenant must not silently widen the query.
			return "", errAt(ErrInvalidValue, c.Position(), "tenant() requires a tenant in the context")
		}
		return `"tenant_id" = ` + c.Arg(tenant), nil
	})
	tr := NewTranslator(cfg, Postgres{})
	ctx := context.WithValue(t.Context(), tenantKey{}, "acme")

	tests := []struct {
		name      string
		query     string
		wantWhere string
		wantArgs  []any
	}{
		{
			name:      "operator only",
			query:     "tenant()",
			wantWhere: `"tenant_id" = $1`,
			wantArgs:  []any{"acme"},
		},
		{
			name:      "combined with a normal predicate",
			query:     "status=active&tenant()",
			wantWhere: `("status" = $1) AND ("tenant_id" = $2)`,
			wantArgs:  []any{"active", "acme"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := tr.Translate(ctx, tt.query)
			if err != nil {
				t.Fatalf("Translate(%q): %v", tt.query, err)
			}
			if res.Where != tt.wantWhere {
				t.Fatalf("where = %q, want %q", res.Where, tt.wantWhere)
			}
			if fmt.Sprint(res.Args) != fmt.Sprint(tt.wantArgs) {
				t.Fatalf("args = %v, want %v", res.Args, tt.wantArgs)
			}
		})
	}
}

// TestOpContextContextMissingValueFails is the other half of the contract: an operator that
// requires a request value must be able to fail closed rather than emit a wider query.
func TestOpContextContextMissingValueFails(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.RegisterUnaryOp("tenant", func(c *OpContext, _ []any) (string, error) {
		if _, ok := c.Context().Value(tenantKey{}).(string); !ok {
			return "", errAt(ErrInvalidValue, c.Position(), "tenant() requires a tenant in the context")
		}
		return "TRUE", nil
	})
	tr := NewTranslator(cfg, Postgres{})

	_, err := tr.Translate(t.Context(), "tenant()")
	if err == nil {
		t.Fatal("Translate succeeded without a tenant in the context, want an error")
	}
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("error = %v, want ErrInvalidValue", err)
	}
}

// TestOpContextContextCancelled verifies that a cancelled request never reaches the operator,
// so an operator doing I/O cannot run after the caller gave up.
func TestOpContextContextCancelled(t *testing.T) {
	t.Parallel()

	var invoked bool
	cfg := testConfig()
	cfg.RegisterUnaryOp("tenant", func(_ *OpContext, _ []any) (string, error) {
		invoked = true
		return "TRUE", nil
	})
	tr := NewTranslator(cfg, Postgres{})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := tr.Translate(ctx, "tenant()"); err == nil {
		t.Fatal("Translate succeeded with a cancelled context, want an error")
	}
	if invoked {
		t.Fatal("operator ran despite the cancelled context")
	}
}

// TestOpContextContextNeverNil pins the nil-safety promise: an operator may call Context()
// unconditionally, including on a zero-value handle.
func TestOpContextContextNeverNil(t *testing.T) {
	t.Parallel()

	var c *OpContext
	if got := c.Context(); got == nil {
		t.Fatal("Context() on a nil OpContext returned nil, want context.Background()")
	} else if got.Err() != nil {
		t.Fatalf("fallback context has error %v, want nil", got.Err())
	}

	// A builder with no context set (possible in-package) must not panic either.
	if got := (&OpContext{b: &builder{}}).Context(); got == nil {
		t.Fatal("Context() with an unset builder context returned nil")
	}
}

// valueSQLConfig registers a custom operator that renders its value with op, so each
// subtest builds its own translator and runs in isolation.
func valueSQLConfig(t *testing.T, op ValueOp) *Config {
	t.Helper()
	cfg := testConfig()
	cfg.Field("status", TypeString).Allow("probe") // custom operators are per-field whitelisted
	cfg.RegisterBinaryOp("probe", func(c *OpContext, _ []any) (string, error) {
		frag, err := c.ValueSQL(op)
		if err != nil {
			return "", err
		}
		return `"status"::TEXT ` + frag, nil
	})
	return cfg
}

// TestOpContextValueSQL covers every rendering ValueSQL promises, in both dialects, so a
// ported operator never has to hand-build placeholders or wildcard translation.
func TestOpContextValueSQL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		op      ValueOp
		query   string
		want    string
		args    []any
		dialect Dialect
	}{
		{name: "eq scalar", op: ValueEq, query: "status=probe=active", want: `"status"::TEXT = $1`, args: []any{"active"}, dialect: Postgres{}},
		{name: "ne scalar", op: ValueNe, query: "status=probe=active", want: `"status"::TEXT <> $1`, args: []any{"active"}, dialect: Postgres{}},
		{name: "lt int", op: ValueLt, query: "status=probe=5", want: `"status"::TEXT < $1`, args: []any{int64(5)}, dialect: Postgres{}},
		{name: "ge int", op: ValueGe, query: "status=probe=5", want: `"status"::TEXT >= $1`, args: []any{int64(5)}, dialect: Postgres{}},
		{name: "eq null", op: ValueEq, query: "status=probe=null", want: `"status"::TEXT IS NULL`, dialect: Postgres{}},
		{name: "ne null", op: ValueNe, query: "status=probe=null", want: `"status"::TEXT IS NOT NULL`, dialect: Postgres{}},
		{name: "in array", op: ValueIn, query: "status=probe=(a,b)", want: `"status"::TEXT = ANY(ARRAY[$1, $2])`, args: []any{"a", "b"}, dialect: Postgres{}},
		{name: "not in array", op: ValueNotIn, query: "status=probe=(a,b)", want: `"status"::TEXT <> ALL(ARRAY[$1, $2])`, args: []any{"a", "b"}, dialect: Postgres{}},
		{name: "in scalar becomes array", op: ValueIn, query: "status=probe=a", want: `"status"::TEXT = ANY(ARRAY[$1])`, args: []any{"a"}, dialect: Postgres{}},
		{name: "like wildcard", op: ValueLike, query: "status=probe=ab*", want: `"status"::TEXT LIKE $1`, args: []any{"ab%"}, dialect: Postgres{}},
		{name: "ilike wildcard", op: ValueILike, query: "status=probe=*b*", want: `"status"::TEXT ILIKE $1`, args: []any{"%b%"}, dialect: Postgres{}},
		{name: "ilike any", op: ValueILikeAny, query: "status=probe=(a*,b*)", want: `"status"::TEXT ILIKE ANY(ARRAY[$1, $2])`, args: []any{"a%", "b%"}, dialect: Postgres{}},
		{name: "like any", op: ValueLikeAny, query: "status=probe=(a*,b*)", want: `"status"::TEXT LIKE ANY(ARRAY[$1, $2])`, args: []any{"a%", "b%"}, dialect: Postgres{}},
		{name: "ilike any from scalar", op: ValueILikeAny, query: "status=probe=a*", want: `"status"::TEXT ILIKE ANY(ARRAY[$1])`, args: []any{"a%"}, dialect: Postgres{}},
		{name: "ilike scalar accepts array", op: ValueILike, query: "status=probe=(a*,b*)", want: `"status"::TEXT ILIKE ANY(ARRAY[$1, $2])`, args: []any{"a%", "b%"}, dialect: Postgres{}},
		{name: "question mark dialect", op: ValueEq, query: "status=probe=a&age=1", want: `("status"::TEXT = ?) AND ("age" = ?)`, args: []any{"a", int64(1)}, dialect: QuestionMark{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := NewTranslator(valueSQLConfig(t, tt.op), tt.dialect)
			res, err := tr.Translate(t.Context(), tt.query)
			if err != nil {
				t.Fatalf("Translate(%q): %v", tt.query, err)
			}
			if res.Where != tt.want {
				t.Fatalf("where = %q, want %q", res.Where, tt.want)
			}
			if fmt.Sprint(res.Args) != fmt.Sprint(tt.args) {
				t.Fatalf("args = %#v, want %#v", res.Args, tt.args)
			}
		})
	}
}

// TestOpContextValueSQLErrors pins the fail-closed cases.
func TestOpContextValueSQLErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		op    ValueOp
		query string
	}{
		{name: "null with lt", op: ValueLt, query: "status=probe=null"},
		{name: "array with eq", op: ValueEq, query: "status=probe=(a,b)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := NewTranslator(valueSQLConfig(t, tt.op), Postgres{})
			if _, err := tr.Translate(t.Context(), tt.query); err == nil {
				t.Fatalf("%s: Translate(%q) succeeded, want an error", tt.name, tt.query)
			}
		})
	}
}

// TestOpContextStrings covers the accessor used by operators that inspect the query to set
// an out-of-band flag, for example "status=archived implies showArchived".
func TestOpContextStrings(t *testing.T) {
	t.Parallel()

	var seen []string
	cfg := testConfig()
	cfg.Field("status", TypeString).Allow("statuses")
	cfg.RegisterBinaryOp("statuses", func(c *OpContext, _ []any) (string, error) {
		vals, err := c.Strings()
		if err != nil {
			return "", err
		}
		seen = vals
		c.Flag("showArchived", containsString(vals, "archived"))
		return "TRUE", nil
	})
	tr := NewTranslator(cfg, Postgres{})

	tests := []struct {
		name  string
		query string
		want  []string
		flag  bool
	}{
		{name: "scalar", query: "status=statuses=archived", want: []string{"archived"}, flag: true},
		{name: "scalar other", query: "status=statuses=active", want: []string{"active"}, flag: false},
		{name: "array", query: "status=statuses=(active,archived)", want: []string{"active", "archived"}, flag: true},
	}
	//nolint:paralleltest // the registered operator reports into a shared observer, so these subtests run sequentially
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen = nil
			res, err := tr.Translate(t.Context(), tt.query)
			if err != nil {
				t.Fatalf("Translate(%q): %v", tt.query, err)
			}
			if fmt.Sprint(seen) != fmt.Sprint(tt.want) {
				t.Fatalf("Strings() = %v, want %v", seen, tt.want)
			}
			if got, _ := res.BoolFlag("showArchived"); got != tt.flag {
				t.Fatalf("showArchived = %v, want %v", got, tt.flag)
			}
		})
	}
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// TestOpContextValueSQLArrayCasts pins the ARRAY cast: without it PostgreSQL infers text[] and
// `integer = ANY(ARRAY[$1,$2])` fails at run time with "operator does not exist: integer = text".
// The text case must stay cast-free.
func TestOpContextValueSQLArrayCasts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		field string
		typ   FieldType
		query string
		want  string
	}{
		{name: "int field casts to int8", field: "score", typ: TypeInt, query: "score=probe=(1,2)", want: `"score" = ANY(ARRAY[$1, $2]::int8[])`},
		{name: "uuid field casts to uuid", field: "id", typ: TypeUUID, query: "id=probe=(11111111-1111-1111-1111-111111111111,22222222-2222-2222-2222-222222222222)", want: `"id" = ANY(ARRAY[$1, $2]::uuid[])`},
		{name: "bool field casts to bool", field: "isActive", typ: TypeBool, query: "isActive=probe=(true,false)", want: `"is_active" = ANY(ARRAY[$1, $2]::bool[])`},
		{name: "text field needs no cast", field: "name", typ: TypeString, query: "name=probe=(a,b)", want: `"name" = ANY(ARRAY[$1, $2])`},
		{name: "not in casts too", field: "score", typ: TypeInt, query: "score=probe=(1,2)", want: `"score" <> ALL(ARRAY[$1, $2]::int8[])`},
		{name: "ilike any stays uncast for text", field: "name", typ: TypeString, query: "name=probe=(a*,b*)", want: `"name" ILIKE ANY(ARRAY[$1, $2])`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			cfg.Field(tt.field, tt.typ).Allow("probe")
			op := ValueIn
			if tt.name == "not in casts too" {
				op = ValueNotIn
			}
			if tt.name == "ilike any stays uncast for text" {
				op = ValueILikeAny
			}
			cfg.RegisterBinaryOp("probe", func(c *OpContext, _ []any) (string, error) {
				frag, err := c.ValueSQL(op)
				if err != nil {
					return "", err
				}
				col, err := c.Column(tt.field)
				if err != nil {
					return "", err
				}
				return col + " " + frag, nil
			})
			res, err := NewTranslator(cfg, Postgres{}).Translate(t.Context(), tt.query)
			if err != nil {
				t.Fatalf("Translate(%q): %v", tt.query, err)
			}
			if res.Where != tt.want {
				t.Fatalf("where = %q, want %q", res.Where, tt.want)
			}
		})
	}
}

// TestOpContextLikePredicate covers the OR-expanded, index-friendly LIKE form.
func TestOpContextLikePredicate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		op      ValueOp // selects LikePredicate vs ILikePredicate
		query   string
		want    string
		args    []any
		dialect Dialect
	}{
		{name: "scalar like", query: "name=probe=ab*", want: `"name" LIKE $1`, args: []any{"ab%"}, dialect: Postgres{}},
		{name: "scalar ilike", op: ValueILike, query: "name=probe=ab*", want: `"name" ILIKE $1`, args: []any{"ab%"}, dialect: Postgres{}},
		{
			name: "array like becomes or chain", query: "name=probe=(a*,b*)",
			want: `("name" LIKE $1 OR "name" LIKE $2)`, args: []any{"a%", "b%"}, dialect: Postgres{},
		},
		{
			name: "array ilike becomes or chain", op: ValueILike, query: "name=probe=(a*,b*)",
			want: `("name" ILIKE $1 OR "name" ILIKE $2)`, args: []any{"a%", "b%"}, dialect: Postgres{},
		},
		{
			name: "question mark dialect", query: "name=probe=(a*,b*)",
			want: `("name" LIKE ? OR "name" LIKE ?)`, args: []any{"a%", "b%"}, dialect: QuestionMark{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			cfg.Field("name", TypeString).Allow("probe")
			useILike := tt.op == ValueILike
			cfg.RegisterBinaryOp("probe", func(c *OpContext, _ []any) (string, error) {
				col, err := c.Column("name")
				if err != nil {
					return "", err
				}
				if useILike {
					return c.ILikePredicate(col)
				}
				return c.LikePredicate(col)
			})
			res, err := NewTranslator(cfg, tt.dialect).Translate(t.Context(), tt.query)
			if err != nil {
				t.Fatalf("Translate(%q): %v", tt.query, err)
			}
			if res.Where != tt.want {
				t.Fatalf("where = %q, want %q", res.Where, tt.want)
			}
			if fmt.Sprint(res.Args) != fmt.Sprint(tt.args) {
				t.Fatalf("args = %#v, want %#v", res.Args, tt.args)
			}
		})
	}
}

// TestOpContextLikePredicateErrors pins the fail-closed cases for the OR form.
func TestOpContextLikePredicateErrors(t *testing.T) {
	t.Parallel()

	for _, query := range []string{"name=probe=null"} {
		cfg := testConfig()
		cfg.Field("name", TypeString).Allow("probe")
		cfg.RegisterBinaryOp("probe", func(c *OpContext, _ []any) (string, error) {
			col, err := c.Column("name")
			if err != nil {
				return "", err
			}
			return c.LikePredicate(col)
		})
		if _, err := NewTranslator(cfg, Postgres{}).Translate(t.Context(), query); err == nil {
			t.Fatalf("Translate(%q) succeeded, want an error", query)
		}
	}
}

// TestOpContextLikePredicateThreshold pins the OR/ANY switch measured with likeOrMax: at the
// threshold the index-friendly OR chain is emitted, one pattern beyond it the compact ANY form.
func TestOpContextLikePredicateThreshold(t *testing.T) {
	t.Parallel()

	render := func(t *testing.T, patterns int) *Result {
		t.Helper()
		cfg := testConfig()
		cfg.Field("name", TypeString).Allow("probe")
		cfg.RegisterBinaryOp("probe", func(c *OpContext, _ []any) (string, error) {
			col, err := c.Column("name")
			if err != nil {
				return "", err
			}
			return c.LikePredicate(col)
		})

		parts := make([]string, patterns)
		for i := range parts {
			parts[i] = fmt.Sprintf("p%d*", i)
		}
		query := `name=probe=(` + strings.Join(parts, ",") + `)`
		res, err := NewTranslator(cfg, Postgres{}).Translate(t.Context(), query)
		if err != nil {
			t.Fatalf("Translate(%d patterns): %v", patterns, err)
		}
		return res
	}

	atThreshold := render(t, likeOrMax)
	if strings.Contains(atThreshold.Where, "ANY(") {
		t.Fatalf("%d patterns: got the ANY form, want the OR chain: %s", likeOrMax, atThreshold.Where)
	}
	if got, want := strings.Count(atThreshold.Where, " OR "), likeOrMax-1; got != want {
		t.Fatalf("%d patterns: %d OR separators, want %d", likeOrMax, got, want)
	}
	if got, want := len(atThreshold.Args), likeOrMax; got != want {
		t.Fatalf("%d patterns: %d args, want %d", likeOrMax, got, want)
	}

	beyond := render(t, likeOrMax+1)
	if !strings.Contains(beyond.Where, "LIKE ANY(ARRAY[") {
		t.Fatalf("%d patterns: got the OR chain, want the ANY form: %s", likeOrMax+1, beyond.Where)
	}
	if got, want := len(beyond.Args), likeOrMax+1; got != want {
		t.Fatalf("%d patterns: %d args, want %d", likeOrMax+1, got, want)
	}
	if strings.Contains(beyond.Where, " OR ") {
		t.Fatalf("%d patterns: ANY form still contains OR separators: %s", likeOrMax+1, beyond.Where)
	}

	// Wildcard translation must hold in both renderings.
	for _, res := range []*Result{atThreshold, beyond} {
		for _, arg := range res.Args {
			if s, ok := arg.(string); ok && strings.Contains(s, "*") {
				t.Fatalf("argument %q still contains the RQL wildcard", s)
			}
		}
	}
}
