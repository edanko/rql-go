# rql-go design & conformance decisions

Status: agreed baseline for the conformance/security/extensibility rewrite.
Owner: library maintainers. Scope: the single package `rql`, plus the `fuzz` test package.

## 1. Goals

1. **Spec conformance** to the original RQL specification (persvr/rql), with an executable
   conformance suite covering every documented construct.
2. **No injection surface.** User input never reaches SQL text; only validated identifiers and
   bound parameters do.
3. **Extensible beyond the builtin operators**: custom operators that can resolve columns and bind
   values, per-(field,operator) remapping (virtual fields), out-of-band result flags (e.g. "show
   archived"), tri-state limit/offset, structured sort.
4. **Documented behaviour**: every intentional deviation from the reference implementation is recorded
   here and covered by a test, so callers can rely on the semantics rather than on an implementation.
5. **Performance**: fewer allocations than the pre-rewrite baseline (simple query 43 allocs/2651 B,
   complex query 348 allocs/8786 B) with no correctness trade-offs; enforced by a test budget.

Non-goals: query execution, aggregation/reduction execution, multi-table joins, non-Postgres SQL
dialects beyond the `?` placeholder form.

## 2. Conformance: spec prose wins over the reference implementation

The reference JS parser contains defects, so "conform" means **conform to the documented spec**,
and every intentional deviation from the *reference implementation* is recorded here and covered by
a test.

| Construct | Reference JS | Decision | Why |
|---|---|---|---|
| Raw ISO dates `foo=2000-01-01T00:00:00Z` | throws (`Unknown converter 2000-01-01T00`) | **accept** | Spec: "dates (in ISO UTC format **without colon encoding**)". The spec's own example must work. |
| `%3C`/`%3E` operator mapping | regex exists but yields a malformed tree | **implement correctly**: `%3C`→`lt`, `%3C=`→`le`, `%3E`→`gt`, `%3E=`→`ge` | Documented intent; a broken reference is not a definition. |
| `&` and `|` mixed in one group | throws "Can not mix conjunctions…" | **reject** with a clear error | Accepting it silently yields different result sets per implementation. Callers must parenthesise. |
| `(foo,bar)=3` and `foo/bar=3` | equivalent (slash is de-sugared) | **both accepted, equivalent** | Spec defines both spellings. |
| Typed values `string:`/`number:`/`boolean:`/`date:`/`isodate:`/`epoch:` | supported | **supported** | Spec lists them. |
| Typed values `re:`/`RE:`/`glob:` | supported (regexp values) | **rejected** (`ErrUnsupportedConverter`) | Cannot be bound safely as a SQL parameter; silently binding the literal (today's behaviour) is worse than failing closed. |
| `$1` parameter reference | substitutes an external parameter | **rejected** | No parameter channel in this API; silently binding the literal `"$1"` hides mistakes. |
| `contains(prop,value)` | "contains **any** value" | **`= ANY(col)`**; extension operator `containsAll` keeps `@>` | Spec semantics; `@>` means "contains all". |
| `excludes(prop,value)` | "does not contain any" | **`NOT (value = ANY(col))`** | Spec semantics. |
| `limit(count,start,maxCount)` | 3 arguments | **all three** (`maxCount` clamps `count`) | Spec defines the third argument; today it is silently dropped. |
| `select()`, `sort()`, `distinct()`, `count()`, `values()` | supported | **supported** → projection / ordering / DISTINCT / COUNT mode / value list | They map to a single-table SQL statement. |
| `aggregate()`, `first()`, `one()`, `rel()`, `recurse()`, `sum/mean/min/max` | supported | **`ErrUnsupportedOperator`** (explicit) | Require execution/aggregation semantics; an explicit error beats silent wrong SQL. |
| `a=1&a=2` (repeated eq) | two `eq` nodes | **two `eq` nodes** (no rewrite) | Rewriting to `IN` flips AND-semantics to OR; rewriting to `@>` breaks non-array columns. Both are wrong. |
| `like`/`ilike` wildcard `*` | n/a (`like` is not a spec operator) | **`*` translated to `%` for those two operators only** | `*` is the conventional RQL wildcard; passing it through would match a literal asterisk and silently return no rows |

Additional grammar requirements: normalized form `op(a,b)`, sugar `prop=val`, FIQL `prop=op=val`,
bare FIQL comparison operators (`<`, `>`, `<=`, `>=`, `!=`), arrays, nested paths, `not(...)`,
`%xx` decoding, `+` decoded as space, single-quote stripping, depth limit, and no panics on any input.

## 3. Security model

Trust boundary: the RQL string is attacker-controlled; `Config` and dialect are developer-controlled.

Rules, enforced by construction:

1. **Values are always bound parameters.** The only inlined tokens are `TRUE`, `FALSE`, `NULL`.
2. **Identifiers come only from `Config`** (RQL field → SQL column). Unknown fields are rejected when
   a config is present; the legacy "no config ⇒ pass through" mode is removed.
3. **Identifiers are quoted with escaping** (`"` → `""`), so a column name can never terminate its quote.
4. **JSONB path segments are validated** against `^[A-Za-z_][A-Za-z0-9_]*$` *after* `%xx` decoding,
   and must belong to a registered field (or to an explicitly enabled dynamic-path root).
   Rejection is an error, never a sanitised substitution.
5. **`%xx` decoding never widens the accepted alphabet.** Decoding happens per value token, and every
   decoded character is re-validated before it can be used as a path segment.
6. **Fail closed**: unknown operator, unknown converter, unsupported construct, oversized depth,
   over-long input, and malformed input all return typed errors. No silent truncation or dropping.
7. **No identifier list, no `select` wildcard, no ORDER BY injection**: sort fields come from the
   config and are checked with `Sortable`.

Regression payloads (must error, never build SQL) live in `security_test.go`; `FuzzTranslate`
asserts no panic and no unbalanced quotes.

## 4. Core API

```go
cfg := rql.NewConfig().
    Field("id", rql.TypeUUID).Allow("eq", "in").Sortable().
    Field("name", rql.TypeString).Column("full_name").Allow("eq", "like", "ilike").
    Field("createdAt", rql.TypeTime).Column("created_at").
    Field("createdAtFrom", rql.TypeTime).Column("created_at").MapOp("eq", "ge").  // virtual field
    Field("restrictionRule.cameraEnabled", rql.TypeBool).                          // nested path, per-leaf ops
    Field("tags", rql.TypeStringArray).Allow("contains", "containsAll", "overlaps")
cfg.RegisterOp("showArchived", rql.BinaryOp, func(c *rql.OpContext, args []any) (string, error) {
    v, err := c.Bool()            // typed accessor for the operator argument
    ...
    c.Flag("showArchived", v)     // out-of-band result flag
    return "TRUE", nil            // SQL fragment for the WHERE clause
})

tr := rql.NewTranslator(cfg, rql.Postgres{})       // or rql.PostgresOptimized{}, rql.QuestionMark{}
res, err := tr.Translate(ctx, "status=active&showArchived=true")
res.Where      // `"status" = $1`
res.Args       // ["active"]
res.Flags      // {"showArchived": true}
res.Sort       // []SortField{{Column: "created_at", Desc: true}}
res.Limit      // *int (nil = absent)
res.Select     // []string
```

Result fields are structured (no pre-rendered ORDER BY/LIMIT strings) so callers can feed gorm/sqlx/pgx
without reparsing; helper methods (`WhereString`, `GetValues`, `OrderBy`, `LimitOrDefault`, …) exist for
the common paths.

Custom operators receive `*OpContext` with exported helpers (`Column`, `Arg`, `Args`, `Dialect`, `Flag`,
`Sort`, `Limit`), so an out-of-tree operator can do everything an in-tree one can — this is the
extensibility gap that makes a naive `RegisterBinaryOp` unable to bind parameters.

Field-level validators get the same context through `FieldBuilder.ValidatorContext`, so a check can
depend on request-scoped values (a tenant's allowed values, an audit id) and on the caller's deadline;
`FieldBuilder.Validator` remains the context-free form.

`OpContext.Context()` returns the request context passed to `Translate`, so an operator can read
request-scoped values attached with `context.WithValue` — a tenant code, a request id, the caller
identity — and bind them, and can observe the caller's deadline. This matters because tenancy and
audit rules usually live in the request, not in the query:

```go
cfg.RegisterUnaryOp("tenant", func(c *rql.OpContext, args []any) (string, error) {
	tenant, ok := c.Context().Value(tenantKey{}).(string)
	if !ok || tenant == "" {
		return "", rql.ErrInvalidValue // fail closed: never widen the query
	}
	return `"tenant_id" = ` + c.Arg(tenant), nil
})
// status=active&tenant()  →  ("status" = $1) AND ("tenant_id" = $2)   Args: [active acme]
```

A cancelled context aborts the translation before the operator is invoked, and the operator must
still check `ctx.Err()` itself if it performs I/O.

`OpContext.LikePredicate` / `ILikePredicate` pick the rendering from the pattern count: the OR-expanded
form (`col LIKE $1 OR col LIKE $2`) up to 32 patterns, the compact `col LIKE ANY(ARRAY[...])` beyond.
PostgreSQL cannot use a btree prefix index for `col LIKE ANY(ARRAY[...])` but does turn a chain of LIKE
predicates into a BitmapOr over it; on 200k rows with such an index the OR chain stayed faster at every
count tried up to 128 (1.0 ms against 114.8 ms), and with no index the two measure the same. The
threshold therefore bounds SQL length and planning cost, not a crossover in speed. `ValueSQL(ValueLikeAny)`
always emits the ANY form, which a GIN trigram index can serve.

`OpContext.ValueSQL(op)` renders the operator's value argument with the wildcard translation and the
type cast applied, and `OpContext.Strings()` exposes the value as strings; between them a ported
operator needs no hand-built SQL for its argument. The ARRAY forms carry an explicit cast
(`ANY(ARRAY[$1, $2]::int8[])`) because PostgreSQL infers `text[]` for an untyped array, which fails at
run time against an integer, uuid, timestamp or boolean column.


## 5. Scope: not an API-compatible port of any existing translator

This package is spec-first. It does not reproduce the API, the SQL text or the quirks of any earlier
in-house translator, and no compatibility shim is provided: a shim would have been a second engine, so
every fix would land twice and the two could drift silently.

What is preserved instead is the *capability* set that services actually need — typed value coercion
(`int64`, `uuid`, `time.Time`, errors on bad values), per-`(field,operator)` remapping (virtual fields
such as `createdAtFrom` → `created_at >= ?`), out-of-band result flags (`showArchived`,
`showSoftDeleted`), tri-state limit/offset, the `?` placeholder dialect for gorm/`database/sql`, and a
custom-operator API that can bind parameters. Those are covered by tests, not by a compatibility layer.

## 6. Performance

Budget (amd64, Go 1.27, `-benchmem`): simple two-predicate filter **≤ 8 allocs/op**, complex query
**≤ 40 allocs/op**, zero allocations in the lexer for queries without values needing decoding.
Techniques: byte-level lexer producing `(kind, start, end)` positions (no per-token strings), lazy
value decoding only for leaves, pooled AST nodes with ownership rules, `strings.Builder` + `Grow` +
`strconv.AppendInt` instead of `fmt`, `int64` numbers (no `float64` rounding), preallocated args and
path slices. Measured with `benchstat` against `/tmp/bench-before.txt`; allocation ceilings asserted in
`alloc_test.go` with `testing.AllocsPerRun` so regressions fail CI rather than reviews.

## 7. Testing

`parser_test.go`, `scanner_test.go`, `builder_test.go`, `dialect_test.go`, `value_test.go`,
`config_test.go`, `spec_conformance_test.go` (every spec example verbatim + one case per audit row),
`security_test.go` (injection payloads), `opcontext_test.go` (custom-operator API),
`*_bench_test.go`, `alloc_test.go` and `fuzz/` (fuzz targets + allocation gate).

Integration tests live in the nested module `test/integration`, so the library itself has **no
dependencies at all** (empty `go.mod`) — only that module requires a PostgreSQL driver, and only tests
that need a database pull it in. Run them with `make integration` (needs `RQL_TEST_POSTGRES_DSN`); they
skip cleanly when no DSN is set, so `go test ./...` stays usable everywhere.
