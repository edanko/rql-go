// Package fuzz contains adversarial and budget tests for the rql-go translator.
//
// The fuzz targets assert safety invariants that must hold for every input: no panic, no
// SQL breakout outside delimited identifiers, balanced quoting, and only typed errors. The
// budget test asserts the allocation ceilings from docs/DESIGN.md.
//
// Run the fuzzers with `make fuzz` (bounded time) or directly:
//
//	go test -fuzz=FuzzTranslate -fuzztime=30s ./fuzz
//	go test -fuzz=FuzzPathSegment -fuzztime=30s ./fuzz
//
// Safety invariants cannot catch a semantics-only regression (for example a path segment
// being silently dropped while the SQL stays valid); those are covered by unit tests such as
// TestSplitPathPreservesSeparators in the root package.
package fuzz
