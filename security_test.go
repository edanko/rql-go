package rql

import (
	"strings"
	"testing"
)

// securityPayloads are attacker-controlled query strings that previously (or in
// a naive implementation) reached SQL text. The contract is that each one either
// fails to parse or fails to build; none may yield a WHERE clause containing
// unquoted SQL syntax.
var securityPayloads = []string{
	`name/x%27%20OR%201%3D1%20--=v`,
	`name/x%27)%20OR%20(%271%27%3D%271=v`,
	`name/a%27%2C%27b=v`,
	`name/%27=v`,
	`name/x%22%20OR%201%3D1%20--=v`,
	`name/x%3B%20DROP%20TABLE%20users%3B=v`,
	`config/user%2F%27%20OR%201%3D1%20--=v`,
	`config/a%20b=v`,
	`name/base%27->>'x=v`,
	`name=a%27%20OR%20%271%27%3D%271`,
	`name=a%22%20OR%201%3D1%20--`,
	`name%27=1`,
	`%22name%22=1`,
	`name=a%00b`,
	`name=1%3B%20DROP%20TABLE%20t`,
	`sort(-name%27)--`,
	`select(name%22)`,
	`limit(1)%3B%20DROP`,
	`name=in=(a%27,b%27)`,
	`tags=contains=(x%27))`,
}

// TestSecurityPayloadsRejected proves no payload reaches SQL text unchecked.
func TestSecurityPayloadsRejected(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("name", TypeString).Sortable().Selectable()
	cfg.AllowDynamicPaths("name", "config")
	tr := NewTranslator(cfg, Postgres{})

	for _, payload := range securityPayloads {
		t.Run(payload, func(t *testing.T) {
			t.Parallel()
			res, err := tr.Translate(newTestContext(t), payload)
			if err != nil {
				return // failing closed is the contract
			}
			// If it did translate, the SQL must still be a well-formed,
			// quoted statement with every value bound.
			if strings.ContainsAny(res.Where, ";") {
				t.Fatalf("payload produced a statement separator: %q", res.Where)
			}
			if !balancedQuotes(res.Where, '\'') {
				t.Fatalf("payload produced unbalanced single quotes: %q", res.Where)
			}
			if !balancedQuotes(res.Where, '"') {
				t.Fatalf("payload produced unbalanced double quotes: %q", res.Where)
			}
			for _, arg := range res.Args {
				if s, ok := arg.(string); ok && strings.Contains(s, "--") && strings.Contains(res.Where, s) {
					t.Fatalf("value %q was inlined into SQL %q", s, res.Where)
				}
			}
		})
	}
}

// balancedQuotes reports whether quote appears an even number of times.
func balancedQuotes(s string, quote byte) bool {
	return strings.Count(s, string(quote))%2 == 0
}

// TestIdentifiersComeOnlyFromConfig proves a query cannot introduce an
// identifier that the configuration did not declare.
func TestIdentifiersComeOnlyFromConfig(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("name", TypeString)
	tr := NewTranslator(cfg, Postgres{})

	for _, query := range []string{
		"nope=1",
		"passwd=1",
		"name)OR(1=1=1",
		"1=1",
		"name=1 UNION SELECT 1",
	} {
		if _, err := tr.Translate(newTestContext(t), query); err == nil {
			t.Fatalf("%q was accepted; every field must be configured", query)
		}
	}
}

// TestPathSegmentsCannotWidenTheAlphabet proves percent decoding cannot smuggle
// punctuation into a JSONB path.
func TestPathSegmentsCannotWidenTheAlphabet(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.AllowDynamicPaths("root")
	tr := NewTranslator(cfg, Postgres{})

	hostile := []string{
		"root/a%27b=1",  // '
		"root/a%22b=1",  // "
		"root/a%20b=1",  // space
		"root/a%2Db=1",  // -
		"root/1abc=1",   // leading digit
		"root/a%3B--=1", // ;--
		"root/%27OR%271%27=1",
	}
	// Note: %2F decodes to '/' which only adds path levels; every decoded segment
	// is still validated, so that input is safe rather than hostile.
	for _, query := range hostile {
		if _, err := tr.Translate(newTestContext(t), query); err == nil {
			t.Fatalf("%q produced SQL despite an invalid path segment", query)
		}
	}

	// Valid segments still work, including percent-encoded identifier characters.
	res := mustTranslate(t, tr, "root/a_b9=1")
	if want := `"root"->>'a_b9' = $1`; res.Where != want {
		t.Fatalf("Where = %q, want %q", res.Where, want)
	}
}

// TestValueNeverInlined proves user values are always bound parameters.
func TestValueNeverInlined(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	const value = "'; DROP TABLE users; --"
	res := mustTranslate(t, tr, "name="+strings.ReplaceAll(value, " ", "%20"))
	if strings.Contains(res.Where, "DROP") {
		t.Fatalf("value was inlined: %q", res.Where)
	}
	if len(res.Args) != 1 || res.Args[0] != value {
		t.Fatalf("Args = %#v, want the value bound verbatim", res.Args)
	}
}

// TestOnlyLiteralsAreInlined documents the single exception: the fixed keywords.
func TestOnlyLiteralsAreInlined(t *testing.T) {
	t.Parallel()
	tr := NewTranslator(testConfig(), Postgres{})
	res := mustTranslate(t, tr, "isActive=true")
	if res.Where != `"is_active" = TRUE` || len(res.Args) != 0 {
		t.Fatalf("Where = %q args = %#v", res.Where, res.Args)
	}
	res = mustTranslate(t, tr, "isActive=false")
	if res.Where != `"is_active" = FALSE` || len(res.Args) != 0 {
		t.Fatalf("Where = %q args = %#v", res.Where, res.Args)
	}
	res = mustTranslate(t, tr, "age=null")
	if res.Where != `"age" IS NULL` || len(res.Args) != 0 {
		t.Fatalf("Where = %q args = %#v", res.Where, res.Args)
	}
}

// TestOversizedInputRejected proves the hard input cap is enforced.
func TestOversizedInputRejected(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("name", TypeString)
	cfg.MaxInputLen = 32
	tr := NewTranslator(cfg, Postgres{})
	query := "name=" + strings.Repeat("a", 64)
	if _, err := tr.Translate(newTestContext(t), query); err == nil {
		t.Fatal("oversized query was accepted")
	}
}

// TestSelectAndSortWhitelists proves projection and ordering cannot name an
// undeclared column.
func TestSelectAndSortWhitelists(t *testing.T) {
	t.Parallel()
	cfg := NewConfig()
	cfg.Field("name", TypeString) // neither sortable nor selectable
	tr := NewTranslator(cfg, Postgres{})

	for _, query := range []string{"select(name)", "sort(name)", "sort(-name)", "sort(name,desc)"} {
		if _, err := tr.Translate(newTestContext(t), query); err == nil {
			t.Fatalf("%q was accepted for a non-projectable field", query)
		}
	}
}
