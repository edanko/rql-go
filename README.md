# rql-go

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![CI](https://github.com/edanko/rql-go/actions/workflows/ci.yml/badge.svg)](https://github.com/edanko/rql-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/edanko/rql-go.svg)](https://pkg.go.dev/github.com/edanko/rql-go)
[![Go Report Card](https://goreportcard.com/badge/github.com/edanko/rql-go)](https://goreportcard.com/report/github.com/edanko/rql-go)

An RQL → PostgreSQL translator for Go: it parses a [Resource Query Language](https://github.com/persvr/rql)
query string into a parameterized SQL `WHERE` expression plus structured shaping information
(projection, ordering, paging, distinct/count).

Three properties drive the design:

- **Spec-conformant.** The grammar follows the RQL specification, including the normalized form
  (`eq(foo,3)`), the form-encoded sugar (`foo=3`), FIQL (`price=lt=10`), bare comparison operators
  (`price<10`), arrays, `(a,b)=v` and `a/b=v` nested properties, typed values (`number:`, `date:`, …),
  and `sort`/`select`/`limit`/`distinct`/`count`. Deliberate deviations from the *reference
  implementation* (which has bugs) are documented in [docs/DESIGN.md](docs/DESIGN.md).
- **Safe by construction.** User input never becomes SQL text: values are always bound, identifiers
  come only from your `Config` and are quote-escaped, JSONB path segments are validated after
  percent-decoding, and every malformed input fails closed with a typed error.
- **Fast and allocation-light.** A single-predicate query translates in ~400 ns with **6 allocations**
  (was 1511 ns / 43 allocations before the rewrite); see [docs/PERFORMANCE.md](docs/PERFORMANCE.md).

Module path: `github.com/edanko/rql-go`.

## Install

```
go get github.com/edanko/rql-go
```

## Quick start

```go
package main

import (
	"context"
	"fmt"

	rql "github.com/edanko/rql-go"
)

func main() {
	cfg := rql.NewConfig().
		Field("id", rql.TypeInt).Allow("in").
		Field("name", rql.TypeString).Column("full_name").Allow("eq", "like", "ilike").Sortable().
		Field("status", rql.TypeString).Allow("eq", "in").
		Field("age", rql.TypeInt).Sortable().
		Field("createdAt", rql.TypeTime).Column("created_at").Sortable().
		Field("tags", rql.TypeStringArray).Allow("contains", "containsAll", "overlaps").
		Config() // back to *Config: Field() returns a field builder

	tr := rql.NewTranslator(cfg, rql.Postgres{}) // $1..$N placeholders

	res, err := tr.Translate(context.Background(),
		"status=active&age=gt=25&sort(-createdAt)&limit(20,40)")
	if err != nil {
		panic(err) // *rql.Error: check with errors.Is(err, rql.ErrUnknownField) etc.
	}

	fmt.Println(res.Where)                            // ("status" = $1) AND ("age" > $2)
	fmt.Println(res.Args)                             // [active 25]
	fmt.Println(res.OrderBy())                        // "created_at" DESC
	fmt.Println(res.LimitOrDefault(50), res.OffsetOrDefault(0)) // 20 40
}
```

`Result` is structured, not pre-rendered, so it feeds gorm, sqlx or pgx without re-parsing:

| Field | Meaning |
|---|---|
| `Where`, `Args` | boolean expression (no `WHERE` keyword) and its bound parameters |
| `Sort []SortField` | requested ordering, each `{Column, Path, Desc}` |
| `Select []string` | projected, already-quoted column expressions |
| `Limit`, `Offset *int` | `nil` when absent — absent and zero are distinguishable |
| `Distinct`, `Count bool` | `distinct()` / `count()` requested |
| `Flags map[string]any` | out-of-band values set by custom operators |

Helpers: `WhereString`, `GetValues`, `OrderBy`, `OrderByWith`, `SelectString`, `LimitOrDefault`,
`OffsetOrDefault`, `HasLimit`, `HasOffset`, `BoolFlag`.

## Dialects

| Dialect | Placeholders | `in`/`containsAll` shape |
|---|---|---|
| `rql.Postgres{}` | `$1, $2` | `IN ($1, $2)` |
| `rql.PostgresOptimized{}` | `$1, $2` | `= ANY($1)` / `@> $1` (one slice argument) |
| `rql.QuestionMark{}` | `?` | `IN (?, ?)` — for `database/sql`/gorm |

Custom dialects implement `rql.Dialect` (`QuoteIdentifier`, `Placeholder`, `BuildArrayOp`, `CastFor`)
and may opt into capabilities such as array-appending or slice binding.

## Custom operators

Custom operators are first-class: they can resolve columns, bind parameters, set flags and shaping.
This is how application-specific filters such as "show archived" are expressed without leaking the
decision into SQL text.

```go
cfg := rql.NewConfig()
cfg.Field("status", rql.TypeString)

cfg.RegisterOp("showArchived", rql.BinaryOp, func(c *rql.OpContext, args []any) (string, error) {
	archived, err := c.Bool() // typed accessor over the operator argument
	if err != nil {
		return "", err
	}
	c.Flag("showArchived", archived) // returned in Result.Flags, not in SQL
	return "TRUE", nil               // contributes no filter; the caller applies the flag
})

// status=active&showArchived=true
//   Where = (`"status" = $1) AND (TRUE)`, Args = [active], Flags = {"showArchived": true}
// The literal TRUE keeps the predicate well-formed; the decision itself travels in Flags.
// Returning "" instead elides the conjunct entirely.
```

`Field(...)` returns a field builder; call `Config()` to continue with configuration that is not
field-specific, or chain another `Field(...)` directly.

`OpContext` also exposes `Context()`, the request context given to `Translate`. Operators use it for
request-scoped values (tenant code, request id, authenticated caller) and for deadlines, which keeps
tenancy and audit rules where they belong — in the request, not in the query:

```go
cfg.RegisterUnaryOp("tenant", func(c *rql.OpContext, args []any) (string, error) {
	tenant, ok := c.Context().Value(tenantKey{}).(string)
	if !ok || tenant == "" {
		return "", rql.ErrInvalidValue // fail closed rather than widening the query
	}
	return `"tenant_id" = ` + c.Arg(tenant), nil
})

res, _ := tr.Translate(context.WithValue(ctx, tenantKey{}, "acme"), "status=active&tenant()")
// res.Where = `("status" = $1) AND ("tenant_id" = $2)`, res.Args = [active acme]
```

### Rendering operator values

`ValueSQL(op)` renders the operator's own value — placeholders, casts and the `*`→`%` wildcard
translated — so an operator never hand-builds SQL for its argument. `Strings()` returns the value as
strings (scalar → one element, `field=in=(a,b)` → the elements), for operators that inspect the query
to set a flag.

```go
cfg.Field("status", rql.TypeString).Allow("find_by_status")
cfg.RegisterBinaryOp("find_by_status", func(c *rql.OpContext, _ []any) (string, error) {
	frag, err := c.ValueSQL(rql.ValueEq) // "= $1", or "IS NULL" for a null value
	if err != nil {
		return "", err
	}
	return `"status"::TEXT ` + frag, nil
})

// A LIKE-family value translates the RQL wildcard and binds per element:
//   identifier=find_by_identifier_in=(ab*,cd*)
//   -> "id" IN (SELECT device_id FROM device_identifiers
//               WHERE identifier ILIKE ANY(ARRAY[$1, $2]) AND tenant_code = $3 UNION ...)
//      args: [ab% cd% acme ...]
```

| `ValueOp` | rendered |
|---|---|
| `ValueEq` `ValueNe` `ValueLt` `ValueLe` `ValueGt` `ValueGe` | `= $n`, `<> $n`, … (`IS NULL` / `IS NOT NULL` for a null value) |
| `ValueIn` / `ValueNotIn` | `= ANY(ARRAY[$1, $2]::int8[])` / `<> ALL(…)` — cast to the field type so PostgreSQL does not infer `text[]` |
| `ValueLike` / `ValueILike` | `LIKE $n` / `ILIKE $n`; an array value emits the `ANY(ARRAY[…])` form |
| `ValueLikeAny` / `ValueILikeAny` | `LIKE ANY(ARRAY[…])` / `ILIKE ANY(ARRAY[…])`, always the array form |

### Validating values

`Validator(fn)` rejects a value after it has been coerced to the field type. `ValidatorContext(fn)`
is the same check with access to the request context, for rules that depend on the caller — a
tenant's allowed statuses, a request id for audit, or a deadline:

```go
cfg.Field("status", rql.TypeString).ValidatorContext(func(ctx context.Context, v any) error {
	allowed, _ := ctx.Value(allowedStatusesKey{}).([]string)
	if len(allowed) == 0 {
		return rql.ErrInvalidValue // fail closed: no policy in the request
	}
	status, _ := v.(string)
	if !slices.Contains(allowed, status) {
		return fmt.Errorf("status %q is not allowed for this tenant", status)
	}
	return nil
})
```

The check runs per element for `field=in=(…)`, the context is never nil, and a returned error rejects
the query with `ErrInvalidValue` (so `errors.Is` works). Installing either method replaces the other —
they configure the same slot.

`LikePredicate(col)` / `ILikePredicate(col)` choose the rendering themselves: an OR chain
(`col LIKE $1 OR col LIKE $2`) for up to 32 patterns, the compact `col LIKE ANY(ARRAY[…])` beyond that.
The OR chain exists because PostgreSQL cannot serve a `LIKE ANY` expression from a btree prefix index,
while it does turn a chain of `LIKE` predicates into a BitmapOr over that index. Measured on 200k rows
with a `text_pattern_ops` index (median of five runs):

| patterns | OR chain | `LIKE ANY` |
|---|---|---|
| 2 | 0.06 ms | 8.6 ms |
| 16 | 0.23 ms | 19.6 ms |
| 64 | 0.60 ms | 60.7 ms |
| 128 | 1.00 ms | 114.8 ms |

The OR chain stayed ahead at every count, so 32 is a bound on SQL size and planning time rather than a
point where the compact form gets faster; with no index at all the two measure the same. A GIN trigram
index serves both forms. `ValueSQL(ValueLikeAny)` always emits the ANY form when you want it explicitly.

`OpContext` exposes `Column(name)`, `Arg(v)`/`ArgsAfter(vals)` (parameter binding), `Value`/`ValueAt`/
`Values` with typed accessors (`Bool`, `Int`, `Float`, `String`, `Time`, `IsNull`), `Field`, `Operator`,
`FieldType`, `Cast`, `Dialect`, `Flag`, `Sort`/`SetSort`, `Limit`/`SetLimit`, `Offset`/`SetOffset`.

Virtual fields (a query field that maps to a different column *and* operator) are configuration, not code:

```go
cfg := rql.NewConfig()
cfg.Field("createdAtFrom", rql.TypeTime).Column("created_at").MapOp("eq", "ge")
// createdAtFrom=2024-01-01T00:00:00Z  ->  "created_at" >= $1
```

## Supported RQL constructs

| Construct | Examples |
|---|---|
| Normalized form | `eq(foo,3)`, `and(a,b)`, `or(...)`, `not(...)` |
| Form sugar | `foo=3`, `foo=bar` |
| FIQL | `price=lt=10`, `name=like=Jo*` |
| Bare comparisons | `price<10`, `price>=10`, `price!=10` |
| Logical | `a=1&b=2`, `a=1\|b=2`, `(a=1\|b=2)&c=3`, `not(a=1)` |
| Arrays | `id=in=(1,2,3)`, `id=out=(8,9)` |
| Nested properties | `(a,b)=v`, `a/b=v`, `a.b=v` (JSONB, validated per segment) |
| Typed values | `number:3`, `string:3`, `boolean:true`, `date:2024-01-01`, `isodate:2024`, `epoch:1704067200000` (ms) |
| Shaping | `select(a,b)`, `sort(-createdAt,+name)`, `limit(20)`, `limit(20,40)`, `distinct()`, `count()` |
| Array predicates | `contains(tags,x)` (any element equals x), `containsAll`, `overlaps`, `excludes` |

Comparison operators: `eq`, `ne`, `lt`, `le`, `gt`, `ge`, `like`, `ilike`, `in`, `out`, `contains`,
`containsAll`, `overlaps`, `excludes`.

`aggregate`, `first`, `one`, `rel`, `recurse`, `sum`, `mean`, `min`, `max` are **rejected** with
`ErrUnsupportedOperator`: they require execution semantics a SQL `WHERE` translator cannot provide, and
an explicit error is better than silently wrong SQL.

## Security model

1. Values are bound parameters; the only inlined literals are `TRUE`, `FALSE`, `NULL`.
2. Identifiers come from `Config` only; unknown fields are `ErrUnknownField`, and quoting escapes `"`.
3. JSONB path segments are validated against `^[A-Za-z_][A-Za-z0-9_]*$` *after* percent-decoding, and
   traversal requires explicit opt-in (`Field(...).Dynamic()` or `Config.AllowDynamicPaths(...)`):
   `name.DROP=1` is `ErrInvalidPath`, not a column reference.
4. Limits: `MaxDepth`, `MaxInputLen`, `MaxTokens` bound adversarial input; oversized or deeply nested
   queries return `ErrTooComplex`/`ErrMaxDepth`.
5. Every error is a `*rql.Error` wrapping a sentinel, so `errors.Is` is reliable.

Regression coverage: a 20-payload injection corpus, plus fuzz targets asserting no panic, balanced
quoting, contiguous `$N`, and one parameter per argument.

## Performance

amd64, Go 1.27, `-benchmem`, quiet machine, `-count=6`:

| benchmark | before rewrite | now | delta |
|---|---|---|---|
| one predicate (`name=Item%20A`) | 1511 ns / 2651 B / 43 allocs | **395 ns / 3400 B / 6 allocs** | 3.8× faster, −86% allocs |
| complex (projection + logic + array + sort + paging) | 10765 ns / 8580 B / 348 allocs | **6919 ns / 6430 B / 18 allocs** | 36% faster, −95% allocs |

Allocation ceilings are enforced in CI (`fuzz/budget_test.go`), so a regression fails the build.
Methodology and reproduction: [docs/PERFORMANCE.md](docs/PERFORMANCE.md).

## Development

```bash
make help          # list targets
make check         # fmt + vet + lint + race tests + security scan
make bench         # benchmarks vs the recorded baseline
make budget        # allocation budget gate
make fuzz          # bounded fuzz run
make integration       # Postgres suite against the released tag (needs RQL_TEST_POSTGRES_DSN)
make integration-local # same suite against the working tree
```

The integration module requires the released tag rather than replacing it with `../..`, so the suite
is a real consumer: it fetches what `go get` would give everyone and proves the published artifact
works. `make integration-local` (and a second CI step) re-runs it with a temporary `replace` onto the
checkout, so a pull request still validates its own SQL instead of the last release.

The library has **no dependencies** (empty `go.mod`). Only the nested module `test/integration`
requires a PostgreSQL driver, so consuming this package downloads nothing.

Dependency updates are automated with [Renovate](renovate.json), which covers all four places versions
are pinned: the `go.mod` files (including the integration-test module), GitHub Actions, the pinned
Postgres service image, and the `go install`-style tool pins in [`tools-versions.txt`](tools-versions.txt).
That file is the single source of truth for tool versions — the workflows read it at run time, so there is
nothing to keep in sync by hand. Postgres majors and Go toolchain bumps are gated behind the dependency
dashboard because both can change results and need a human to read the integration diff.

Design decisions and the conformance table: [docs/DESIGN.md](docs/DESIGN.md).

## License

[MIT](LICENSE).
