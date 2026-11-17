#!/bin/sh
# Benchmark runner for rql-go.
#
# Captures `go test -bench` output and diffs two runs with benchstat. See
# docs/PERFORMANCE.md for the measurement methodology and the release budgets.
#
# Usage:
#   scripts/bench.sh [after|before|baseline|compare]
#
#     after     Run benchmarks, write /tmp/bench-after.txt (default).
#     before    Run benchmarks, write /tmp/bench-before.txt.
#     baseline  Run benchmarks, write /tmp/bench-after.txt, then copy it over
#               /tmp/bench-before.txt to (re)record a baseline.
#     compare   Print `benchstat /tmp/bench-before.txt /tmp/bench-after.txt`.
#
# Environment:
#   BENCH_COUNT   Repetitions per benchmark (default 10).
#   BENCH_PKGS    Package patterns (default ./...).
#   BENCH_FILTER  Benchmark name regexp (default .).
#   BENCH_TAGS    Extra build tags (default none).
#   GO            Go command (default go).
#
# Measurement rules: -count=10, packages serialized with -p 1, no test functions run,
# on a quiet machine. Run `compare` and read the benchstat medians; treat |delta| < 2%
# or p > 0.05 as noise.

set -eu

unset CDPATH
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

GO=${GO:-go}
BENCH_COUNT=${BENCH_COUNT:-10}
BENCH_PKGS=${BENCH_PKGS:-./...}
BENCH_FILTER=${BENCH_FILTER:-.}
BENCH_TAGS=${BENCH_TAGS:-}

before=/tmp/bench-before.txt
after=/tmp/bench-after.txt

usage() {
	# Print the header block (lines 2..25) of this script.
	sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
}

die() {
	printf 'bench: %s\n' "$*" >&2
	exit 1
}

run_bench() {
	_out=$1
	printf 'bench: running benchmarks for %s (-count=%s, -p 1) -> %s\n' \
		"$BENCH_PKGS" "$BENCH_COUNT" "$_out"
	if [ -n "$BENCH_TAGS" ]; then
		set -- -tags "$BENCH_TAGS"
	else
		set --
	fi
	# shellcheck disable=SC2086 # BENCH_PKGS is intentionally a package-pattern list.
	if ! "$GO" test -run '^$' -bench "$BENCH_FILTER" -benchmem \
		-count="$BENCH_COUNT" -p 1 "$@" $BENCH_PKGS >"$_out" 2>&1; then
		cat "$_out" >&2
		die "go test failed (output above)"
	fi
	grep -E '^(goos|goarch|pkg|cpu|Benchmark)' "$_out" | sed 's/^/    /' || true
}

need_benchstat() {
	command -v benchstat >/dev/null 2>&1 ||
		die "benchstat not found; install it with: make tools"
}

case "${1:-after}" in
after | run)
	run_bench "$after"
	if [ -s "$before" ]; then
		printf 'bench: baseline %s exists; run "make bench-compare" for the diff\n' "$before"
	fi
	;;
before)
	run_bench "$before"
	printf 'bench: recorded baseline %s\n' "$before"
	;;
baseline)
	run_bench "$after"
	cp "$after" "$before"
	printf 'bench: recorded new baseline %s (copied from %s)\n' "$before" "$after"
	;;
compare)
	need_benchstat
	[ -s "$before" ] || die "missing $before; run: make bench-before"
	[ -s "$after" ] || die "missing $after; run: make bench-after"
	printf 'bench: benchstat %s %s\n\n' "$before" "$after"
	benchstat "$before" "$after"
	;;
-h | --help | help)
	usage
	;;
*)
	usage
	die "unknown mode: $1"
	;;
esac
