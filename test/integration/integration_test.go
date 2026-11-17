// Integration tests that execute translated SQL against a real PostgreSQL instance.
//
// They live in their own module so the library itself stays dependency-free: only this
// module requires a database driver. Tests are skipped unless a DSN is configured, so
// `go test ./...` is safe on a machine without a database:
//
//	RQL_TEST_POSTGRES_DSN='postgres://user:pass@127.0.0.1:5432/db?sslmode=disable' \
//	  go test -count=1 ./...
//
// CI provides the DSN from a postgres service container (see .github/workflows/ci.yml).
package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	rql "github.com/edanko/rql-go"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// integrationDSN returns the configured DSN, or skips the test when none is available.
func integrationDSN(t *testing.T) string {
	t.Helper()
	for _, key := range []string{"RQL_TEST_POSTGRES_DSN", "POSTGRES_DSN", "DATABASE_URL"} {
		if dsn := os.Getenv(key); dsn != "" {
			return dsn
		}
	}
	t.Skip("no PostgreSQL DSN configured (set RQL_TEST_POSTGRES_DSN); skipping integration test")
	return ""
}

// integrationDB opens a connection and installs the fixture schema, returning the database
// with a dozen rows that exercise text, numeric, boolean, timestamp, JSONB and array columns.
func integrationDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("pgx", integrationDSN(t))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping database: %v", err)
	}

	stmts := []string{
		`DROP TABLE IF EXISTS rql_it`,
		`CREATE TABLE rql_it (
			id         BIGINT PRIMARY KEY,
			name       TEXT,
			age        INTEGER,
			active     BOOLEAN,
			created_at TIMESTAMPTZ,
			data       JSONB,
			tags       TEXT[]
		)`,
		`INSERT INTO rql_it (id, name, age, active, created_at, data, tags) VALUES
			(1, 'Alice',   25, TRUE,  '2024-01-01T00:00:00Z', '{"role":"admin","city":"SF"}',  '{go,sql}'),
			(2, 'Bob',     35, TRUE,  '2024-02-01T00:00:00Z', '{"role":"editor","city":"NY"}', '{go,rql}'),
			(3, 'Carol',   45, FALSE, '2024-03-01T00:00:00Z', '{"role":"viewer","city":"SF"}', '{api,rql}'),
			(4, 'Dave',    55, TRUE,  '2024-04-01T00:00:00Z', '{"role":"admin","city":"LA"}',  '{sql}'),
			(5, 'Eve',     30, FALSE, '2024-05-01T00:00:00Z', '{"role":"editor","city":"NY"}', '{go,api,sql}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	return db
}

// integrationTranslator is the configuration the table maps onto.
func integrationTranslator() *rql.Translator {
	cfg := rql.NewConfig().
		Field("id", rql.TypeInt).Allow("in", "eq").Sortable().Selectable().
		Field("name", rql.TypeString).Allow("like", "ilike").Sortable().Selectable().
		Field("age", rql.TypeInt).Sortable().Selectable().
		Field("active", rql.TypeBool).Allow("eq").Column("active").
		Field("createdAt", rql.TypeTime).Column("created_at").Sortable().
		Field("data", rql.TypeJSON).Dynamic().Allow("eq").
		Field("data.role", rql.TypeString).Column("data.role").Allow("eq").
		Field("tags", rql.TypeStringArray).Allow("contains", "containsAll", "overlaps").
		Config()
	cfg.AllowDynamicPaths("data")
	return rql.NewTranslator(cfg, rql.Postgres{})
}

// TestIntegrationQueryExecution runs translated queries against PostgreSQL and compares the
// returned row sets, which is the only way to catch a semantic regression that still produces
// syntactically valid SQL.
func TestIntegrationQueryExecution(t *testing.T) { //nolint:paralleltest // shared fixture table; setup is not concurrency-safe
	db := integrationDB(t)
	tr := integrationTranslator()
	ctx := context.Background()

	tests := []struct {
		name  string
		query string
		want  []int64
	}{
		{name: "equality", query: "name=Alice", want: []int64{1}},
		{name: "comparison", query: "age=gt=35", want: []int64{3, 4}},
		{name: "boolean", query: "active=false", want: []int64{3, 5}},
		{name: "in list", query: "id=in=(1,3,5)", want: []int64{1, 3, 5}},
		{name: "not in", query: "id=out=(1,2,3)", want: []int64{4, 5}},
		{name: "and", query: "active=true&age=lt=40", want: []int64{1, 2}},
		{name: "or with parens", query: "(name=Alice|name=Bob)&active=true", want: []int64{1, 2}},
		{name: "not", query: "not(active=true)", want: []int64{3, 5}},
		{name: "like wildcard", query: "name=like=A%25", want: []int64{1}},
		{name: "ilike case insensitive", query: "name=ilike=alice", want: []int64{1}},
		{name: "timestamp bound", query: "createdAt=ge=2024-03-01T00:00:00Z", want: []int64{3, 4, 5}},
		{name: "jsonb path text", query: "data/role=admin", want: []int64{1, 4}},
		{name: "jsonb registered leaf", query: "data.role=editor", want: []int64{2, 5}},
		{name: "jsonb path numeric-ish via dynamic", query: "data/city=SF", want: []int64{1, 3}},
		{name: "array contains any (spec semantics)", query: "contains(tags,api)", want: []int64{3, 5}},
		{name: "array contains all", query: "tags=containsAll=(go,sql)", want: []int64{1, 5}},
		{name: "array overlaps", query: "tags=overlaps=(api,rql)", want: []int64{2, 3, 5}},
	}

	//nolint:paralleltest // shared fixture table; see TestIntegrationQueryExecution
	for _, tt := range tests {
		t.Run("postgres/"+tt.name, func(t *testing.T) {
			res, err := tr.Translate(ctx, tt.query)
			if err != nil {
				t.Fatalf("Translate(%q): %v", tt.query, err)
			}
			//nolint:gosec // the fragment is produced by the translator under test; asserting it is safe is the point
			stmt := `SELECT id FROM rql_it WHERE ` + res.Where + ` ORDER BY id`
			rows, err := db.QueryContext(ctx, stmt, res.Args...)
			if err != nil {
				t.Fatalf("query %q: %v", stmt, err)
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
				t.Fatalf("query %q returned ids %v, want %v (where=%s args=%v)",
					tt.query, got, tt.want, res.Where, res.Args)
			}
		})
	}

}

// TestIntegrationShaping covers projection, ordering and paging against live data, separately
// from the predicate table so each test stays readable.
func TestIntegrationShaping(t *testing.T) { //nolint:paralleltest // shared fixture table; setup is not concurrency-safe
	db := integrationDB(t)
	tr := integrationTranslator()
	ctx := context.Background()

	// Shaping: sort + paging must produce the expected page of ids.

	res, err := tr.Translate(ctx, "active=true&sort(-age)&limit(2,1)&select(id,age)")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	//nolint:gosec // every fragment is produced by the translator under test
	stmt := fmt.Sprintf(`SELECT %s FROM rql_it WHERE %s ORDER BY %s LIMIT %d OFFSET %d`,
		res.SelectString(), res.Where, res.OrderBy(), res.LimitOrDefault(100), res.OffsetOrDefault(0))
	if !strings.Contains(stmt, "LIMIT 2") || !strings.Contains(stmt, "OFFSET 1") {
		t.Fatalf("unexpected paging in %q", stmt)
	}
	rows, err := db.QueryContext(ctx, stmt, res.Args...)
	if err != nil {
		t.Fatalf("query %q: %v", stmt, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id, age int64
		if err := rows.Scan(&id, &age); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	// active rows ordered by age DESC are 4 (55), 2 (35), 1 (25); OFFSET 1 LIMIT 2 is 2, 1.
	if fmt.Sprint(ids) != fmt.Sprint([]int64{2, 1}) {
		t.Fatalf("page ids = %v, want [2 1]", ids)
	}
}

// TestIntegrationInjectionRejected proves the injection payloads fail closed even with a live
// database behind them: nothing reaches the server.
func TestIntegrationInjectionRejected(t *testing.T) { //nolint:paralleltest // shared fixture table; setup is not concurrency-safe
	db := integrationDB(t)
	tr := integrationTranslator()
	ctx := context.Background()

	payloads := []string{
		`data/x%27%20OR%201%3D1%20--=v`,
		`data/a%27%3B%20DROP%20TABLE%20rql_it%3B--=v`,
		`data/../id=1`,
		`name%27%20OR%20%271%27%3D%271=x`,
	}
	//nolint:paralleltest // shared fixture table; see TestIntegrationInjectionRejected
	for _, payload := range payloads {
		t.Run(payload, func(t *testing.T) {
			if _, err := tr.Translate(ctx, payload); err == nil {
				t.Fatalf("payload %q produced SQL, want an error", payload)
			}
		})
	}

	// The table must still exist and be readable after the attempts above.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM rql_it`).Scan(&n); err != nil {
		t.Fatalf("table damaged: %v", err)
	}
	if n != 5 {
		t.Fatalf("row count = %d, want 5", n)
	}
}

// TestIntegrationQuestionMarkDialectOnly checks the gorm-style dialect renders placeholders it
// can hand to database/sql.
func TestIntegrationQuestionMarkDialectOnly(t *testing.T) { //nolint:paralleltest // shared fixture table; setup is not concurrency-safe
	tr := rql.NewTranslator(integrationTranslator().Config(), rql.QuestionMark{})
	res, err := tr.Translate(context.Background(), "name=Alice&age=gt=20")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if want := `("name" = ?) AND ("age" > ?)`; res.Where != want {
		t.Fatalf("where = %q, want %q", res.Where, want)
	}
	if len(res.Args) != 2 {
		t.Fatalf("args = %v, want 2", res.Args)
	}
}
