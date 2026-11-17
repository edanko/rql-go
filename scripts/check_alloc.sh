#!/bin/sh
# Allocation budget gate for rql-go (docs/DESIGN.md §6):
#
#   simple two-predicate filter  <= 8 allocs/op
#   complex query                <= 40 allocs/op
#
# The ceilings are asserted with testing.AllocsPerRun in two places, and this script requires
# both to pass:
#   * alloc_test.go TestAllocationBudget — the canonical gate (docs/DESIGN.md §6), in-package,
#     so it also runs as part of `go test ./...`;
#   * fuzz/budget_test.go TestAllocBudget — the same ceilings re-asserted through the public
#     API from an external package.
# Running both means the two copies of the numbers cannot drift apart silently.
#
# The script fails closed: if a test is renamed or removed the gate errors out instead of
# passing with nothing to run.
#
# Usage: scripts/check_alloc.sh

set -eu

unset CDPATH
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

GO=${GO:-go}

# require_test <package pattern> <test name>
require_test() {
	if ! "$GO" test -list "^$2\$" "$1" 2>/dev/null | grep -qx "$2"; then
		printf 'check_alloc: %s not found in %s; refusing to pass with nothing to run\n' "$2" "$1" >&2
		exit 1
	fi
}

require_test . TestAllocationBudget
require_test ./fuzz TestAllocBudget

printf 'check_alloc: asserting allocation ceilings (simple <= 8, complex <= 40 allocs/op)\n'
"$GO" test -count=1 -run '^TestAllocationBudget$' -v .
"$GO" test -count=1 -run '^TestAllocBudget$' -v ./fuzz
