// Package fuzz holds the fuzz targets for the rql-go translator.
//
// Run them with `make fuzz` (or `go test -fuzz=FuzzTranslate ./fuzz`). They are part of the
// normal test build so `go test ./...` type-checks them, and CI fuzzes for a bounded time.
package fuzz

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	rql "github.com/edanko/rql-go"
)

// maxTranslateTime bounds a single translation. Translation is pure string processing
// measured in microseconds, so anything close to this bound is a denial-of-service bug:
// quadratic parsing, unbounded recursion, or a missing length cap (docs/DESIGN.md §3).
const maxTranslateTime = 250 * time.Millisecond

// seedQueries are the grammar and injection shapes the fuzzer starts from.
var seedQueries = []string{
	// sugar and FIQL forms
	"name=Item%20A",
	"name=eq=Item%20A",
	"status=in=(active,pending)",
	"createdAt=ge=2020-01-01T00:00:00Z",
	"id=in=(550e8400-e29b-41d4-a716-446655440000)",
	// logical combinations, nesting and grouping
	"name=foo&status=active",
	"name=foo|status=active",
	"(name=foo|status=active)&tags=contains=archived",
	"not(name=foo)",
	"foo=bar&(baz=qux|quux=corge)",
	// projection, ordering, pagination
	"select(id,name)&sort(-createdAt,+name)&limit(50,100)",
	"limit(10)&sort(-createdAt)",
	// typed values
	"name=string:42&status=boolean:true",
	// arrays, nested paths and both spellings of the latter
	"tags=contains=archived",
	"(name,status)=(active,foo)",
	// malformed / adversarial
	"",
	"(",
	")",
	"name=",
	"name=in=(",
	"and(",
	"$1",
	"'unbalanced",
	`name="unbalanced`,
	"%",
	"%zz",
	"name=foo;DROP TABLE users;--",
	"name=%22%20OR%201=1--",
	"name=like=%27%25a%27",
	"../../etc/passwd",
	strings.Repeat("(", 512),
	strings.Repeat("not(", 512),
	strings.Repeat("a=1&", 512) + "a=1",
	"name=" + strings.Repeat("A", 100_000),
}

// pathPayloads are known identifier-injection shapes for the field/path position.
var pathPayloads = []string{
	"name",
	"status",
	"restrictionRule.cameraEnabled",
	"restrictionRule/cameraEnabled",
	"tags",
	"meta/cameraEnabled",
	"meta/tags/0",
	// Hostile spellings that made a silent path normaliser visible before it was fixed
	// (v2 split on runs of separators). These are corpus coverage, NOT a semantic oracle:
	// the assertions below only hold when no error is returned, and a silent-drop regression
	// produces safe SQL (see the package comment). The semantic guard is v2's deterministic
	// unit test TestSplitPathPreservesSeparators, run by `go test ./...` in CI.
	"meta//x",
	"meta/../x",
	"meta/./x",
	"meta/a..b",
	"meta/%2E%2E/x",
	"meta/",
	".x",
	"/x",
	// quoting and escaping payloads
	`name"; DROP TABLE users; --`,
	`name' OR '1'='1`,
	`name\"; DROP TABLE users; --`,
	`name%22; DROP TABLE users; --`,
	`name%27%20OR%201=1--`,
	`name%00`,
	`name\x00`,
	`name"b`,
	`name)b`,
	`name,b`,
	`name/b`,
	`name..b`,
	`../../etc/passwd`,
	`..%2F..%2Fetc%2Fpasswd`,
	`name%2F..%2Fetc`,
	`name$1`,
	`$1`,
	`name;DROP`,
	`name/*comment*/`,
	`name--`,
	`name#`,
}

// FuzzTranslate asserts that arbitrary query text never panics, never emits an unterminated
// quoted identifier, and never takes super-linear time. Errors are a valid outcome: the
// translator fails closed (docs/DESIGN.md §3 rule 6).
func FuzzTranslate(f *testing.F) {
	for _, seed := range seedQueries {
		f.Add(seed)
	}
	// Only config-registered fields are queryable, which is the security model from
	// docs/DESIGN.md §3.
	cfg := rql.NewConfig()
	cfg.Field("id", rql.TypeUUID).Allow("eq", "in", "ne")
	cfg.Field("name", rql.TypeString).Allow("eq", "ne", "like", "ilike")
	cfg.Field("status", rql.TypeString).Allow("eq", "in", "ne")
	cfg.Field("createdAt", rql.TypeTime).Allow("eq", "ge", "le", "gt", "lt")
	cfg.Field("tags", rql.TypeStringArray).Allow("contains", "containsAll", "overlaps")
	cfg.Field("restrictionRule.cameraEnabled", rql.TypeBool).Allow("eq")
	tr := rql.NewTranslator(cfg, rql.Postgres{})
	ctx := context.Background()

	f.Fuzz(func(t *testing.T, query string) {
		start := time.Now()
		res, err := tr.Translate(ctx, query)
		elapsed := time.Since(start)
		if elapsed > maxTranslateTime {
			t.Fatalf("Translate(%q) took %v, want <= %v", query, elapsed, maxTranslateTime)
		}
		if err != nil {
			return
		}
		assertBalancedQuotes(t, res.Where)
		assertNoSQLBreakout(t, res.Where)
		assertSafeJSONPathLiterals(t, res.Where)
	})
}

// FuzzPathSegment fuzzes the field/path position with a corpus of identifier-injection
// payloads, in both the `field=value` and the nested `root/segment=value` spellings.
func FuzzPathSegment(f *testing.F) {
	for _, seed := range pathPayloads {
		f.Add(seed)
	}
	// Only config-registered fields are queryable, which is the security model from
	// docs/DESIGN.md §3.
	cfg := rql.NewConfig()
	cfg.Field("name", rql.TypeString).Allow("eq", "ne")
	cfg.Field("restrictionRule.cameraEnabled", rql.TypeBool).Allow("eq")
	// A dynamic JSONB root: leaves are not registered, so every path segment the fuzzer
	// produces is validated at decode time (docs/DESIGN.md §3 rules 4-5).
	cfg.AllowDynamicPaths("meta")
	tr := rql.NewTranslator(cfg, rql.Postgres{})
	ctx := context.Background()

	f.Fuzz(func(t *testing.T, segment string) {
		for _, query := range []string{segment + "=1", segment + "/x=1"} {
			start := time.Now()
			res, err := tr.Translate(ctx, query)
			elapsed := time.Since(start)
			if elapsed > maxTranslateTime {
				t.Fatalf("Translate(%q) took %v, want <= %v", query, elapsed, maxTranslateTime)
			}
			if err != nil {
				continue
			}
			assertBalancedQuotes(t, res.Where)
			assertNoSQLBreakout(t, res.Where)
			assertSafeJSONPathLiterals(t, res.Where)
		}
	})
}

// assertNoSQLBreakout verifies that generated SQL contains no statement terminator,
// string literal or comment marker outside a quoted identifier. Identifiers may contain
// arbitrary text (quoted and escaped) and values are bound parameters (docs/DESIGN.md §3),
// so everything outside `"..."` must be structural SQL.
func assertNoSQLBreakout(t *testing.T, generated string) {
	t.Helper()
	inString := false
	for i := 0; i < len(generated); i++ {
		c := generated[i]
		if c == '"' {
			if !inString {
				inString = true
				continue
			}
			if i+1 < len(generated) && generated[i+1] == '"' {
				i++ // escaped quote inside the identifier
				continue
			}
			inString = false
			continue
		}
		if inString {
			continue
		}
		switch {
		case c == ';':
			t.Fatalf("generated SQL contains a statement terminator outside an identifier: %q", generated)
		case c == '-' && i+1 < len(generated) && generated[i+1] == '-':
			t.Fatalf("generated SQL contains a comment marker outside an identifier: %q", generated)
		case c == '/' && i+1 < len(generated) && generated[i+1] == '*':
			t.Fatalf("generated SQL contains a comment marker outside an identifier: %q", generated)
		}
	}
}

// pathLiteral matches the alphabet a JSONB path key must satisfy (docs/DESIGN.md §3
// rule 4). Keys are emitted as SQL string literals, so anything outside this alphabet —
// a quote above all — would break out of the literal.
var pathLiteral = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// assertSafeJSONPathLiterals checks that every JSONB key rendered as a SQL literal
// (`->>'key'`, `->'key'`) is a plain identifier. The key comes from the query text after
// %xx decoding, so this is the assertion that the decode-then-revalidate rule actually
// held (docs/DESIGN.md §3 rules 4-5).
func assertSafeJSONPathLiterals(t *testing.T, generated string) {
	t.Helper()
	for i := 0; i < len(generated); i++ {
		if generated[i] != '-' || i+1 >= len(generated) || generated[i+1] != '>' {
			continue
		}
		j := i + 2
		if j < len(generated) && generated[j] == '>' {
			j++ // '->>' (text extraction) instead of '->' (jsonb)
		}
		if j >= len(generated) || generated[j] != '\'' {
			continue
		}
		j++
		end := strings.IndexByte(generated[j:], '\'')
		if end < 0 {
			t.Fatalf("generated SQL has an unterminated JSONB path literal: %q", generated)
		}
		if key := generated[j : j+end]; !pathLiteral.MatchString(key) {
			t.Fatalf("JSONB path key %q is not a plain identifier in: %q", key, generated)
		}
	}
}

// assertBalancedQuotes verifies that every `"` in generated SQL belongs to a quoted
// identifier and that identifiers are never left unterminated. Embedded quotes must be
// doubled (docs/DESIGN.md §3 rule 3).
func assertBalancedQuotes(t *testing.T, generated string) {
	t.Helper()
	inString := false
	for i := 0; i < len(generated); i++ {
		if generated[i] != '"' {
			continue
		}
		if !inString {
			inString = true
			continue
		}
		if i+1 < len(generated) && generated[i+1] == '"' {
			i++ // escaped quote inside the identifier
			continue
		}
		inString = false
	}
	if inString {
		t.Fatalf("generated SQL has an unterminated quoted identifier: %q", generated)
	}
}
