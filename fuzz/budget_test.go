// Package fuzz also asserts the rql-go allocation budget (docs/DESIGN.md §6).
//
// The ceiling is a hard gate: `go test ./...` and `make budget` both fail on a regression,
// so an allocation increase cannot land without updating the design document.
package fuzz

import (
	"context"
	"testing"

	rql "github.com/edanko/rql-go"
)

// Budget ceilings from docs/DESIGN.md §6, measured with -benchmem on amd64 with Go 1.27.
// They are intentionally strict: raising one requires updating the design document and
// docs/PERFORMANCE.md in the same change.
const (
	simpleAllocCeiling  = 8
	complexAllocCeiling = 40
)

// Allocation-budget queries. The simple query is a two-predicate filter; the complex query
// mirrors the v0 BenchmarkComplexQuery (projection, boolean logic, array containment,
// ordering and pagination) so the numbers stay comparable with /tmp/bench-before.txt.
const (
	budgetSimpleQuery  = "name=Item%20A&status=in=(active,pending)"
	budgetComplexQuery = "select(id,name)&(status=in=(active,pending)" +
		"|(createdAt=ge=2020-01-01T00:00:00Z&createdAt=le=2021-01-01T00:00:00Z))" +
		"&contains(tags,archived)&sort(-createdAt,+name)&limit(50,100)"
)

// TestAllocBudget fails when translation allocates more than the release budget. It uses
// testing.AllocsPerRun rather than benchmark output so a regression fails CI instead of
// waiting for a benchmark review (docs/DESIGN.md §6).
//
// This test is deliberately not parallel: testing.AllocsPerRun panics when called from a
// parallel test, because it needs exclusive execution to attribute allocations to f.
func TestAllocBudget(t *testing.T) { //nolint:paralleltest // AllocsPerRun cannot run in a parallel test
	// Configure fields used by the budget queries. Selectable() is required for
	// select(...), Sortable() for sort(...) (docs/DESIGN.md §4).
	cfg := rql.NewConfig()
	cfg.Field("id", rql.TypeUUID).Allow("eq", "in").Selectable()
	cfg.Field("name", rql.TypeString).Column("full_name").Allow("eq", "like", "ilike").Sortable().Selectable()
	cfg.Field("status", rql.TypeString).Allow("eq", "in")
	cfg.Field("createdAt", rql.TypeTime).Column("created_at").Allow("ge", "le").Sortable()
	cfg.Field("tags", rql.TypeStringArray).Allow("contains", "containsAll", "overlaps")
	tr := rql.NewTranslator(cfg, rql.Postgres{})
	ctx := context.Background()

	cases := []struct {
		name    string
		query   string
		ceiling float64
	}{
		{name: "simple", query: budgetSimpleQuery, ceiling: simpleAllocCeiling},
		{name: "complex", query: budgetComplexQuery, ceiling: complexAllocCeiling},
	}

	// Warm pools, caches and code paths before measuring.
	for range 100 {
		if _, err := tr.Translate(ctx, budgetSimpleQuery); err != nil {
			t.Fatalf("warmup Translate(%q): %v", budgetSimpleQuery, err)
		}
	}

	//nolint:paralleltest // AllocsPerRun cannot run in a parallel test (see TestAllocBudget)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { //nolint:paralleltest // see TestAllocBudget
			var err error
			allocs := testing.AllocsPerRun(1000, func() {
				_, err = tr.Translate(ctx, tc.query)
			})
			if err != nil {
				t.Fatalf("Translate(%q): %v", tc.query, err)
			}
			if allocs > tc.ceiling {
				t.Fatalf("Translate(%q) = %.0f allocs/op, budget is %.0f allocs/op (docs/DESIGN.md §6)",
					tc.query, allocs, tc.ceiling)
			}
			t.Logf("%s: %.0f allocs/op (ceiling %.0f)", tc.name, allocs, tc.ceiling)
		})
	}
}
