package rql

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// fuzzConfig is the configuration the fuzz target translates against.
func fuzzConfig() *Config {
	cfg := NewConfig()
	cfg.Field("id", TypeUUID).Allow("eq", "in", "ne").Sortable().Selectable()
	cfg.Field("name", TypeString).Allow("like", "ilike").Sortable().Selectable()
	cfg.Field("age", TypeInt).Sortable().Selectable()
	cfg.Field("score", TypeFloat)
	cfg.Field("active", TypeBool)
	cfg.Field("createdAt", TypeTime).Column("created_at").Sortable()
	cfg.Field("tags", TypeStringArray)
	cfg.Field("meta", TypeJSON)
	cfg.AllowDynamicPaths("config")
	cfg.MaxInputLen = 1 << 14
	cfg.MaxTokens = 1 << 12
	cfg.RegisterUnaryOp("noop", func(*OpContext, []any) (string, error) { return "", nil })
	cfg.RegisterBinaryOp("flag", func(c *OpContext, _ []any) (string, error) {
		c.Flag("seen", true)
		return "", nil
	})
	return cfg
}

// FuzzTranslate asserts the translator is total: no input panics it, every error
// is a typed *Error, and any SQL it produces is well formed (balanced quoting,
// placeholders numbered contiguously, one parameter per argument).
func FuzzTranslate(f *testing.F) {
	seeds := []string{
		"",
		"name=abc",
		"name=Item%20A&age=1",
		"name=a|age=1",
		"(name=a|age=1)&active=true",
		"eq(name,abc)",
		"and(eq(name,a),lt(age,3))",
		"not(name=a&age=1)",
		"age=in=(1,2,3)",
		"tags=contains=(go,rs)",
		"sort(-age,+name)&limit(10,5)",
		"select(id,name)&distinct()&count()",
		"createdAt=2020-01-01T00:00:00Z",
		"config/theme=mode",
		"(config,theme)=mode",
		"name=%27quoted%27",
		"name=string:3&age=number:4",
		"name=a%zz",
		"name=$1",
		"a=1&b=2|c=3",
		"name=a)",
		"((((name=a))))",
		"noop()",
		"name=flag=x",
		"age=9007199254740993",
		"name=\x00",
		"name=a%00b",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	tr := NewTranslator(fuzzConfig(), Postgres{})
	optimized := NewTranslator(fuzzConfig(), PostgresOptimized{})
	question := NewTranslator(fuzzConfig(), QuestionMark{})

	f.Fuzz(func(t *testing.T, query string) {
		ctx := context.Background()

		res, err := tr.Translate(ctx, query)
		if err != nil {
			var rqlErr *Error
			if !errors.As(err, &rqlErr) {
				t.Fatalf("error is not a *Error: %T %v", err, err)
			}
			return
		}
		if res == nil {
			t.Fatal("nil Result without an error")
		}
		checkInvariants(t, query, res, true)

		// The other dialects must accept and render the same queries.
		if _, oerr := optimized.Translate(ctx, query); oerr != nil {
			t.Fatalf("Postgres accepted %q but PostgresOptimized rejected it: %v", query, oerr)
		}
		qres, qerr := question.Translate(ctx, query)
		if qerr != nil {
			t.Fatalf("Postgres accepted %q but QuestionMark rejected it: %v", query, qerr)
		}
		checkInvariants(t, query, qres, false)
	})
}

// checkInvariants validates the structural guarantees of produced SQL.
func checkInvariants(t *testing.T, query string, res *Result, numbered bool) {
	t.Helper()

	if strings.ContainsRune(res.Where, 0) {
		t.Fatalf("query %q: NUL byte in SQL %q", query, res.Where)
	}
	if !balancedQuotes(res.Where, '\'') {
		t.Fatalf("query %q: unbalanced single quotes in %q", query, res.Where)
	}
	if !balancedQuotes(res.Where, '"') {
		t.Fatalf("query %q: unbalanced double quotes in %q", query, res.Where)
	}

	if !numbered {
		if n := strings.Count(res.Where, "?"); n != len(res.Args) {
			t.Fatalf("query %q: %d placeholders for %d args in %q", query, n, len(res.Args), res.Where)
		}
		return
	}

	// Placeholders must appear as $1, $2, ... with no gaps and no repeats beyond
	// what the argument list allows.
	maxPlaceholder := 0
	for i := 0; i < len(res.Where); i++ {
		if res.Where[i] != '$' {
			continue
		}
		j := i + 1
		for j < len(res.Where) && res.Where[j] >= '0' && res.Where[j] <= '9' {
			j++
		}
		n, err := strconv.Atoi(res.Where[i+1 : j])
		if err != nil || n < 1 {
			t.Fatalf("query %q: malformed placeholder in %q", query, res.Where)
		}
		if n > len(res.Args) {
			t.Fatalf("query %q: placeholder $%d exceeds %d args in %q", query, n, len(res.Args), res.Where)
		}
		if n > maxPlaceholder {
			maxPlaceholder = n
		}
		i = j - 1
	}
	if maxPlaceholder != len(res.Args) {
		t.Fatalf("query %q: %d args but highest placeholder is $%d in %q",
			query, len(res.Args), maxPlaceholder, res.Where)
	}
}
