# Performance: methodology, baseline and release budgets

This document records how query-translation performance is measured for the rql-go
conformance release, the recorded v0 baseline, and the budgets the release must meet.
The binding design contract is [DESIGN.md](DESIGN.md) §6; this file is the how-to.

## Release budgets

| Measurement | Ceiling | Where enforced |
|---|---|---|
| Simple two-predicate filter | **≤ 8 allocs/op** | `alloc_test.go` (`TestAllocationBudget`, canonical) and `fuzz/budget_test.go` (`TestAllocBudget`, public-API re-assertion); `make budget`; CI `budget` job |
| Complex query (projection + boolean logic + array op + sort + limit) | **≤ 40 allocs/op** | same |
| Lexer allocations for queries with no `%xx`/typed values | 0 allocs/op goal | reviewed via `benchstat` (not gated) |

Allocation counts are gated because they are machine-independent: `B/op` and `allocs/op`
repeat exactly across runs on the same Go version, while `ns/op` on shared CI runners is
too noisy to gate. `ns/op` and `B/op` targets are tracked through `benchstat` review.

## Recorded v0 baseline

Legacy engine (the pre-rewrite `rql-go` package). Source file:
`/tmp/bench-before.txt`, captured on the machine described below with `-count=6`.

| Benchmark | ns/op (median) | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkTranslatorCreation` | ≈ 2 000 (1 953) | 3 008 | 26 |
| `BenchmarkSimpleQuery` | ≈ 1 500 (1 511) | 2 651 | 43 |
| `BenchmarkComplexQuery` | ≈ 10 800 (10 765) | 8 786 | 348 |

`BenchmarkSimpleQuery` in v0 is `name=Item%20A`; the release budget uses a two-predicate
query (`name=Item%20A&status=in=(active,pending)`), so the v0 reference for the budget row
is the 43-alloc number with room for one more bound argument.
`BenchmarkComplexQuery` is
`select(id,name,age)&(status=in=(active,pending)|(age=gt=25&age=lt=50))&contains(tags,archived)&sort(-age,+name)&limit(50,100)`
(see `rql_bemchmark_test.go` in the root package).

Raw samples show the usual spread on an unisolated desktop (translator creation ranged
1 001–3 388 ns/op); `B/op` and `allocs/op` were constant across all samples. Treat the
ns/op medians as indicative and compare against a *freshly measured* baseline on the same
machine instead of these absolute numbers.

### Measurement environment

```
goos:   linux
goarch: amd64
cpu:    AMD Ryzen 7 9700X 8-Core Processor
go:     go1.27.4 (GOMAXPROCS=16)
cmd:    go test -run '^$' -bench . -benchmem -count=6 -p 1 ./...
```

## Methodology

1. **`benchstat` for decisions, never eyeballed numbers.** `benchstat` reports the median,
   the delta, and a p-value. A difference needs both a meaningful effect (roughly |delta|
   > 2% for ns/op) and p < 0.05 to be treated as real; allocation deltas are exact.
2. **`-count=10`.** The scripts default to ten repetitions per benchmark; benchstat needs
   several samples to estimate variance. Fewer samples produce unstable p-values.
3. **Serial runs.** `go test -p 1` serializes package execution and `-run '^$'` skips test
   functions, so only benchmarks run and they never compete with each other.
4. **Same machine, same Go version, same `GOMAXPROCS`.** Benchmarks from different hosts,
   Go toolchains, or CPU counts are not comparable (the `-16` suffix encodes GOMAXPROCS).
5. **Quiet machine.** Close browsers/IDEs/builds and on laptops plug in the charger. Where
   available, use the performance CPU governor
   (`sudo cpupower frequency-set -g performance`). The reference numbers above were taken
   on an unisolated desktop, which is why their ns/op spread is wide.
6. **Warm the code once.** GC, string interning and pools are warmed before
   comparative runs; the budget test warms up before `AllocsPerRun`.
7. **Record the baseline before changing code.** Capture `/tmp/bench-before.txt` from a
   pristine checkout of the commit you compare against.

## Reproducing

```sh
# 0. Install pinned tooling (benchstat) once.
make tools

# 1. Record the baseline from the reference tree (v0 or the previous release).
git worktree add /tmp/rql-baseline <baseline-ref>
cd /tmp/rql-baseline && make bench-before     # writes /tmp/bench-before.txt

# 2. Measure the tree under test.
cd - && make bench-after                       # writes /tmp/bench-after.txt

# 3. Diff.
make bench-compare                             # benchstat /tmp/bench-before.txt /tmp/bench-after.txt
```

To re-record a baseline in one step (for example after an accepted performance change):

```sh
make bench-baseline    # runs benchmarks, then copies /tmp/bench-after.txt over /tmp/bench-before.txt
```

`scripts/bench.sh` accepts the modes `before`, `after`, `baseline` and `compare`; the
environment variables `BENCH_COUNT`, `BENCH_PKGS`, `BENCH_FILTER` and `BENCH_TAGS` tune a
run. No build tags are needed to benchmark the engine: it is the root package.

The allocation gate runs independently:

```sh
make budget            # scripts/check_alloc.sh: runs both allocation-budget tests
```

`scripts/check_alloc.sh` runs the canonical in-package test (`TestAllocationBudget`) and the
external public-API test (`TestAllocBudget`) and fails closed if either has been renamed or
removed, so the gate can never pass with nothing to run. Because the two files carry the
same ceilings independently, a number changed in only one of them surfaces immediately.

## Reading `benchstat` output

```
              │ bench-before.txt │         bench-after.txt         │
              │      sec/op      │   sec/op     vs base            │
SimpleQuery     1.511µ ±  4%       0.412µ ±  2%  -72.73% (p=0.000 n=10)
ComplexQuery    10.77µ ±  2%       3.902µ ±  1%  -63.77% (p=0.000 n=10)
              │       B/op       │     B/op      vs base            │
SimpleQuery    2651.0 ± 0%         512.0 ± 0%    -80.69% (p=0.000 n=10)
```

- `vs base` is the change relative to the baseline; `±` is the spread across samples.
- `p=0.000` means the samples do not overlap within noise; `p≈0.5` means no signal.
- An `allocs/op` change is always real (integer counts), even when ns/op did not move.
- `geomean` at the bottom summarizes all benchmarks in the section; use per-benchmark rows
  for decisions, because one regressed benchmark can hide behind a big win elsewhere.

## CI behaviour

- **`bench.yml`** runs benchmarks on every pull request: `-count=10` on the PR head and on
  the merge base, diffs them with `benchstat`, and updates a single sticky PR comment. It
  is **informational**: shared GitHub runners are noisy and must never block a merge.
- **`ci.yml` `budget` job** is the gating signal. It runs `scripts/check_alloc.sh` and fails
  when either allocation-budget test exceeds its ceiling, or when one of the tests is
  missing (the gate fails closed rather than passing vacuously).
- **`ci.yml` `benchmark` job** is a smoke run (`-count=3`, allowed to fail) that only proves
  the benchmarks still compile and execute on CI.

## Caveats

- Microbenchmarks do not measure SQL execution; they cover parse → validate → build only.
- A single `ns/op` number is a sum over GC phases, scheduling and cache effects. Compare
  distributions (`benchstat` medians and p-values), not individual runs.
- Compiler inlining decisions change between Go minors. Compare within one toolchain version,
  and re-record the baseline after a toolchain upgrade.
- The allocation ceiling is deliberately strict: raising one is a design decision that must
  update [DESIGN.md](DESIGN.md) §6 and this document in the same change.
