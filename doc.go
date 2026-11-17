// Package rql translates RQL (Resource Query Language) query strings into
// parameterised SQL for PostgreSQL.
//
// # Guarantees
//
// Translation is injection free by construction. User input never becomes SQL
// text: values are always bound parameters (the sole exceptions are the TRUE,
// FALSE and NULL keywords, which are literals of a fixed spelling), identifiers
// come only from the developer-provided Config, identifiers are quoted with '"'
// doubling, and JSONB path segments are validated against a plain identifier
// alphabet after percent decoding. Unknown fields, unknown operators,
// unsupported constructs and malformed input all fail closed with a typed error
// that carries the byte offset of the offending construct.
//
// Translation is allocation conscious. The lexer is byte oriented with no
// per-token strings, values are decoded only for the tokens that need it, the
// AST is one arena, and SQL is assembled with a byte buffer rather than
// formatting. Simple filters translate in single-digit allocations (see the
// benchmarks and the allocation budget tests).
//
// # Conformance decisions
//
// The reference JavaScript implementation contains defects, so this package
// follows the documented specification and records every deliberate deviation:
//
//   - Raw ISO-8601 dates such as foo=2000-01-01T00:00:00Z are accepted (the
//     specification requires them "without colon encoding"); the reference
//     implementation throws on them.
//   - Percent-encoded comparison operators are implemented as intended:
//     %3C is lt, %3C= is le, %3E is gt, %3E= is ge.
//   - Mixing '&' (or ',') with '|' in one group is rejected with
//     ErrMixedConjunctions; callers must parenthesise.
//   - (foo,bar)=3 and foo/bar=3 are both accepted and equivalent.
//   - contains(prop,value) uses the specification's "contains any" meaning,
//     which renders as value = ANY(col); the extension operator containsAll
//     keeps the PostgreSQL "@>" (contains all) meaning, and excludes renders
//     NOT (value = ANY(col)).
//   - re:, RE: and glob: values are rejected with ErrUnsupportedConverter
//     instead of being bound as literal text.
//   - $n parameter references are rejected; there is no parameter channel and
//     binding "$1" as text would hide the mistake.
//   - Repeated equality is never rewritten: a=1&a=2 stays two conjuncts. The
//     previous implementations rewrote it to IN (changing AND into OR) or to a
//     containment test (which breaks on non-array columns).
//   - undefined is mapped to SQL NULL rather than bound as the text
//     "undefined".
//   - aggregate(), rel(), first(), one(), values() and recurse() return
//     ErrUnsupportedOperator, since they change the shape of the result set
//     rather than the filter.
//   - Single quotes around a value are stripped, so %27bar%27 and 'bar' both
//     yield bar.
//
// # Dialects
//
// Postgres renders $N placeholders and expands array predicates, so it works
// with lib/pq, database/sql and pgx. PostgresOptimized binds array predicates as
// a single slice parameter (= ANY($1)) and requires pgx. QuestionMark renders
// "?" placeholders for gorm and database/sql, which renumber their own
// placeholders.
//
// # Extending
//
// Custom operators are registered with Config.RegisterOp and receive an
// OpContext that can resolve configured columns (Column), bind parameters (Arg),
// set result flags (Flag) and shape the result (SetSort, SetLimit, SetOffset) —
// so an operator defined outside this package has the same capabilities as a
// built-in one.
package rql
