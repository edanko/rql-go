package rql

import (
	"context"
	"fmt"
	"testing"
)

// benchTranslator is shared by the benchmarks: building a Config is a startup
// cost, translation is the hot path.
var benchTranslator = func() *Translator {
	cfg := NewConfig()
	cfg.Field("id", TypeInt).Sortable().Selectable().Allow("in")
	cfg.Field("name", TypeString).Sortable().Selectable().Column("full_name").Allow("like", "ilike")
	cfg.Field("status", TypeString).Selectable().Validator(func(v any) error {
		s, _ := v.(string)
		if s != "active" && s != "pending" {
			return fmt.Errorf("status must be active or pending")
		}
		return nil
	})
	cfg.Field("age", TypeInt).Sortable().Selectable()
	cfg.Field("isActive", TypeBool).Column("is_active")
	cfg.Field("tags", TypeStringArray).Allow("contains", "containsall", "overlaps")
	return NewTranslator(cfg, Postgres{})
}()

const (
	benchSimpleQuery  = "name=Item%20A&status=active"
	benchComplexQuery = "select(id,name,age)&(status=in=(active,pending)|(age=gt=25&age=lt=50))&contains(tags,archived)&sort(-age,+name)&limit(50,100)"
)

// BenchmarkSimpleQuery measures a two-predicate filter: the baseline hot path.
func BenchmarkSimpleQuery(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	var res *Result
	var err error
	for b.Loop() {
		res, err = benchTranslator.Translate(ctx, benchSimpleQuery)
	}
	benchResult, benchErr = res, err
}

// BenchmarkComplexQuery measures a realistic query with logic, arrays, shaping
// and sorting.
func BenchmarkComplexQuery(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	var res *Result
	var err error
	for b.Loop() {
		res, err = benchTranslator.Translate(ctx, benchComplexQuery)
	}
	benchResult, benchErr = res, err
}

// BenchmarkTranslatorCreation measures configuration cost.
func BenchmarkTranslatorCreation(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		cfg := NewConfig()
		cfg.Field("id", TypeInt).Sortable()
		cfg.Field("name", TypeString).Sortable()
		cfg.Field("age", TypeInt).Sortable()
		cfg.Field("status", TypeString)
		_ = NewTranslator(cfg, nil)
	}
}

// benchResult and benchErr keep the benchmarked values alive.
var (
	benchResult *Result
	benchErr    error
)

// BenchmarkSinglePredicate is the comparability anchor for the pre-rewrite baseline:
// it runs exactly the v0 BenchmarkSimpleQuery input ("name=Item%20A", one predicate)
// against this engine, so ns/op, B/op and allocs/op can be compared with the numbers
// recorded in docs/PERFORMANCE.md.
//
// Do not change its query or its config shape without re-recording the baseline.
func BenchmarkSinglePredicate(b *testing.B) {
	cfg := NewConfig()
	cfg.Field("id", TypeInt).Sortable()
	cfg.Field("name", TypeString).Sortable().Column("full_name")
	cfg.Field("age", TypeInt).Sortable()
	cfg.Field("status", TypeString)
	tr := NewTranslator(cfg, Postgres{})
	ctx := context.Background()

	b.ReportAllocs()
	var res *Result
	var err error
	for b.Loop() {
		res, err = tr.Translate(ctx, "name=Item%20A")
	}
	benchResult, benchErr = res, err
}
