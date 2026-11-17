// Integration tests that execute ValueSQL fragments against a real PostgreSQL instance.
//
// These cover the helper a ported custom operator relies on: the generated fragment must be
// valid SQL for the column type, not merely plausible-looking text.
package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	rql "github.com/edanko/rql-go"
)

// probeOps maps the operator name used in a query to the ValueSQL operation it exercises.
// One registration per shape keeps the table readable and proves each fragment independently.
var probeOps = map[string]rql.ValueOp{
	"probe_eq":        rql.ValueEq,
	"probe_ne":        rql.ValueNe,
	"probe_ilike":     rql.ValueILike,
	"probe_ilike_any": rql.ValueILikeAny,
	"probe_in":        rql.ValueIn,
	"probe_notin":     rql.ValueNotIn,
	"probe_like":      rql.ValueLike,
	"probe_null_eq":   rql.ValueEq,
	"probe_null_ne":   rql.ValueNe,
}

// opFixture installs the fixture table used by the ValueSQL tests.
func opFixture(t *testing.T, db *sql.DB) {
	t.Helper()

	ctx := context.Background()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS rql_ops`,
		`CREATE TABLE rql_ops (id BIGINT PRIMARY KEY, name TEXT, note TEXT, score INTEGER)`,
		`INSERT INTO rql_ops (id, name, note, score) VALUES
			(1, 'head office', 'x', 10),
			(2, 'headquarters', NULL, 20),
			(3, 'back office', 'y', 30),
			(4, 'warehouse', NULL, 40)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
}

// TestValueSQLExecutes proves the ValueSQL fragments are valid PostgreSQL for every shape a
// ported operator uses: scalar comparison, ILIKE with the '*' wildcard, ILIKE ANY over an
// array, = ANY / <> ALL over an array, and NULL handling.
func TestValueSQLExecutes(t *testing.T) { //nolint:paralleltest // shared fixture table, recreated per test function
	db, err := sql.Open("pgx", integrationDSN(t))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	opFixture(t, db)

	cfg := rql.NewConfig()
	for _, field := range []struct {
		name string
		typ  rql.FieldType
	}{{"name", rql.TypeString}, {"note", rql.TypeString}, {"score", rql.TypeInt}} {
		fb := cfg.Field(field.name, field.typ)
		for op := range probeOps {
			fb.Allow(op)
		}
	}
	registerProbeOps(cfg)
	tr := rql.NewTranslator(cfg, rql.Postgres{})
	ctx := context.Background()

	tests := []struct {
		name  string
		query string
		want  []int64
	}{
		{name: "eq", query: "name=probe_eq=head%20office", want: []int64{1}},
		{name: "ne", query: "name=probe_ne=head%20office", want: []int64{2, 3, 4}},
		{name: "ilike wildcard", query: "name=probe_ilike=head*", want: []int64{1, 2}},
		{name: "ilike any", query: "name=probe_ilike_any=(head*,ware*)", want: []int64{1, 2, 4}},
		{name: "in any ints", query: "score=probe_in=(10,30)", want: []int64{1, 3}},
		{name: "not in all ints", query: "score=probe_notin=(10,30)", want: []int64{2, 4}},
		{name: "like", query: "name=probe_like=*office", want: []int64{1, 3}},
		{name: "is null", query: "note=probe_null_eq=null", want: []int64{2, 4}},
		{name: "is not null", query: "note=probe_null_ne=null", want: []int64{1, 3}},
	}

	//nolint:paralleltest // shared fixture table, recreated per test function
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := tr.Translate(ctx, tt.query)
			if err != nil {
				t.Fatalf("Translate(%q): %v", tt.query, err)
			}
			//nolint:gosec // the fragment is produced by the translator under test; asserting it is safe is the point
			stmt := `SELECT id FROM rql_ops WHERE ` + res.Where + ` ORDER BY id`
			rows, err := db.QueryContext(ctx, stmt, res.Args...)
			if err != nil {
				t.Fatalf("query %q (args %v): %v", stmt, res.Args, err)
			}
			defer func() { _ = rows.Close() }()

			var got []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("query %q returned %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

// registerProbeOps registers one operator per ValueSQL shape used by the table above.
func registerProbeOps(cfg *rql.Config) {
	for name, valueOp := range probeOps {
		cfg.RegisterBinaryOp(name, func(c *rql.OpContext, _ []any) (string, error) {
			frag, err := c.ValueSQL(valueOp)
			if err != nil {
				return "", err
			}
			field, _ := c.Field()
			return `"` + field + `" ` + frag, nil
		})
	}
}

// TestLikePredicateMatchesLikeAny proves the OR-expanded form and the compact ANY form select the
// same rows on a real server: they differ only in whether a btree prefix index can serve them.
func TestLikePredicateMatchesLikeAny(t *testing.T) { //nolint:paralleltest // shared fixture table
	db, err := sql.Open("pgx", integrationDSN(t))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	opFixture(t, db)

	cfg := rql.NewConfig()
	cfg.Field("name", rql.TypeString).Allow("probe")
	cfg.RegisterBinaryOp("probe", func(c *rql.OpContext, _ []any) (string, error) {
		col, err := c.Column("name")
		if err != nil {
			return "", err
		}
		return c.LikePredicate(col)
	})
	cfg.RegisterBinaryOp("probe_any", func(c *rql.OpContext, _ []any) (string, error) {
		frag, err := c.ValueSQL(rql.ValueLikeAny)
		if err != nil {
			return "", err
		}
		return `"name" ` + frag, nil
	})
	// probe_any needs to be allowed on the field as well.
	cfg.Field("name", rql.TypeString).Allow("probe", "probe_any")

	tr := rql.NewTranslator(cfg, rql.Postgres{})

	queries := []struct{ or, any string }{
		{or: "name=probe=(head*,ware*)", any: "name=probe_any=(head*,ware*)"},
		{or: "name=probe=head*", any: "name=probe_any=head*"},
	}

	for _, pair := range queries {
		orIDs := selectIDs(t, db, tr, pair.or)
		anyIDs := selectIDs(t, db, tr, pair.any)
		if fmt.Sprint(orIDs) != fmt.Sprint(anyIDs) {
			t.Fatalf("%q returned %v but %q returned %v", pair.or, orIDs, pair.any, anyIDs)
		}
		if len(orIDs) == 0 {
			t.Fatalf("%q matched nothing, the comparison is meaningless", pair.or)
		}
	}
}

// selectIDs runs a translated query and returns the matching ids.
func selectIDs(t *testing.T, db *sql.DB, tr *rql.Translator, query string) []int64 {
	t.Helper()
	ctx := t.Context()

	res, err := tr.Translate(ctx, query)
	if err != nil {
		t.Fatalf("Translate(%q): %v", query, err)
	}
	//nolint:gosec // the fragment is produced by the translator under test
	stmt := `SELECT id FROM rql_ops WHERE ` + res.Where + ` ORDER BY id`
	rows, err := db.QueryContext(ctx, stmt, res.Args...)
	if err != nil {
		t.Fatalf("query %q (args %v): %v", stmt, res.Args, err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return ids
}
