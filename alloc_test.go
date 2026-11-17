package rql

import (
	"context"
	"testing"
)

// Allocation ceilings from docs/DESIGN.md §6. These run under testing.AllocsPerRun
// so a regression fails the build instead of being noticed in review.
const (
	simpleAllocCeiling  = 8
	complexAllocCeiling = 40
)

// allocBudgetTranslator is configured for the budget queries.
func allocBudgetTranslator() *Translator {
	cfg := NewConfig()
	cfg.Field("id", TypeUUID).Allow("eq", "in").Selectable()
	cfg.Field("name", TypeString).Column("full_name").Allow("eq", "like", "ilike").Sortable().Selectable()
	cfg.Field("status", TypeString).Allow("eq", "in")
	cfg.Field("createdAt", TypeTime).Column("created_at").Allow("ge", "le").Sortable()
	cfg.Field("tags", TypeStringArray).Allow("contains", "containsall", "overlaps")
	return NewTranslator(cfg, Postgres{})
}

// TestAllocationBudget enforces the release allocation budget.
//
//nolint:paralleltest // testing.AllocsPerRun cannot be called from a parallel test.
func TestAllocationBudget(t *testing.T) {
	tr := allocBudgetTranslator()
	ctx := context.Background()
	cases := []struct {
		name    string
		query   string
		ceiling float64
	}{
		{
			name:    "simple",
			query:   "name=Item%20A&status=in=(active,pending)",
			ceiling: simpleAllocCeiling,
		},
		{
			name: "complex",
			query: "select(id,name)&(status=in=(active,pending)" +
				"|(createdAt=ge=2020-01-01T00:00:00Z&createdAt=le=2021-01-01T00:00:00Z))" +
				"&contains(tags,archived)&sort(-createdAt,+name)&limit(50,100)",
			ceiling: complexAllocCeiling,
		},
	}
	for _, tc := range cases {
		// Warm up first so caches and pools are populated before measuring.
		for range 100 {
			if _, err := tr.Translate(ctx, tc.query); err != nil {
				t.Fatalf("%s: warm-up failed: %v", tc.name, err)
			}
		}
		got := testing.AllocsPerRun(1000, func() {
			if _, err := tr.Translate(ctx, tc.query); err != nil {
				t.Fatalf("%s: translate failed: %v", tc.name, err)
			}
		})
		if got > tc.ceiling {
			t.Errorf("%s query allocates %.0f times per translation, budget is %.0f (docs/DESIGN.md §6)",
				tc.name, got, tc.ceiling)
		}
	}
}

// TestScannerAllocations asserts the lexer never allocates.
//
//nolint:paralleltest // testing.AllocsPerRun cannot be called from a parallel test.
func TestScannerAllocations(t *testing.T) {
	const query = "name=Item&status=active&age=gt=5"
	got := testing.AllocsPerRun(200, func() {
		s := newScanner(query, 0)
		for {
			tok, err := s.next()
			if err != nil || tok.Kind == tokEOF {
				return
			}
		}
	})
	if got != 0 {
		t.Errorf("scanner allocated %.1f times, want 0", got)
	}
}
