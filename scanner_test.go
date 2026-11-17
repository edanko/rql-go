package rql

import (
	"errors"
	"testing"
)

// scanAll scans every token of src.
func scanAll(t *testing.T, src string) []Token {
	t.Helper()
	s := newScanner(src, 0)
	var out []Token
	for {
		tok, err := s.next()
		if err != nil {
			t.Fatalf("scan %q: unexpected error: %v", src, err)
		}
		out = append(out, tok)
		if tok.Kind == tokEOF {
			return out
		}
	}
}

func TestScannerTokenKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		src   string
		kinds []tokKind
	}{
		{"", []tokKind{tokEOF}},
		{"a", []tokKind{tokWord, tokEOF}},
		{"(=)&|,", []tokKind{tokLParen, tokEq, tokRParen, tokAnd, tokOr, tokComma, tokEOF}},
		{"<=<", []tokKind{tokLe, tokLt, tokEOF}},
		{">=>", []tokKind{tokGe, tokGt, tokEOF}},
		{"!=", []tokKind{tokNe, tokEOF}},
		{"  \t\r\n a \t", []tokKind{tokWord, tokEOF}},
		{"name=Item%20A", []tokKind{tokWord, tokEq, tokWord, tokEOF}},
		{"a=1&b=2|c=3", []tokKind{tokWord, tokEq, tokWord, tokAnd, tokWord, tokEq, tokWord, tokOr, tokWord, tokEq, tokWord, tokEOF}},
		{"2000-01-01T00:00:00Z", []tokKind{tokWord, tokEOF}},
		{"foo/bar.baz=x", []tokKind{tokWord, tokEq, tokWord, tokEOF}},
		{"sort(-a,+b)", []tokKind{tokWord, tokLParen, tokWord, tokComma, tokWord, tokRParen, tokEOF}},
	}
	for _, tc := range cases {
		got := scanAll(t, tc.src)
		if len(got) != len(tc.kinds) {
			t.Fatalf("scan %q: got %d tokens, want %d", tc.src, len(got), len(tc.kinds))
		}
		for i, want := range tc.kinds {
			if got[i].Kind != want {
				t.Fatalf("scan %q: token %d is %v, want %v", tc.src, i, got[i].Kind, want)
			}
		}
	}
}

func TestScannerPositions(t *testing.T) {
	t.Parallel()
	toks := scanAll(t, "ab=cd")
	want := [][2]int{{0, 2}, {2, 3}, {3, 5}}
	if len(toks) < 3 {
		t.Fatalf("got %d tokens", len(toks))
	}
	for i, span := range want {
		if toks[i].Start != span[0] || toks[i].End != span[1] {
			t.Fatalf("token %d spans [%d,%d), want [%d,%d)", i, toks[i].Start, toks[i].End, span[0], span[1])
		}
	}
	if toks[3].Start != len("ab=cd") {
		t.Fatalf("EOF position is %d, want %d", toks[3].Start, len("ab=cd"))
	}
}

func TestScannerDecodePercent(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Item%20A":                 "Item A",
		"a%2Cb":                    "a,b",
		"a%28b%29":                 "a(b)",
		"%25hello%25":              "%hello%",
		"%D0%BE%D0%B1":             "об",
		"plain":                    "plain",
		"%3C":                      "<",
		"%3D":                      "=",
		"%27bar%27":                "'bar'",
		"2000-01-01T00%3A00%3A00Z": "2000-01-01T00:00:00Z",
	}
	for raw, want := range cases {
		toks := scanAll(t, raw)
		got := decode(raw, toks[0], true)
		if got != want {
			t.Fatalf("decode(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestScannerPlusIsSpaceOnlyForValues(t *testing.T) {
	t.Parallel()
	const raw = "a+b"
	toks := scanAll(t, raw)
	if got := decode(raw, toks[0], true); got != "a b" {
		t.Fatalf("value decoding: got %q, want %q", got, "a b")
	}
	if got := decode(raw, toks[0], false); got != "a+b" {
		t.Fatalf("identifier decoding: got %q, want %q", got, "a+b")
	}
}

func TestScannerRejectsBadInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		src   string
		class error
	}{
		{"name=a%zz", ErrInvalidSyntax},
		{"name=a%2", ErrInvalidSyntax},
		{"name=%", ErrInvalidSyntax},
		{"a\x00b=1", ErrInvalidSyntax},
		{"a=1\x07", ErrInvalidSyntax},
		{"name!", ErrInvalidSyntax},
		{"\x7f=1", ErrInvalidSyntax},
	}
	for _, tc := range cases {
		s := newScanner(tc.src, 0)
		var err *Error
		for i := 0; i < 64; i++ {
			tok, scanErr := s.next()
			if scanErr != nil {
				err = scanErr
				break
			}
			if tok.Kind == tokEOF {
				break
			}
		}
		if err == nil {
			t.Fatalf("scan %q: expected an error", tc.src)
		}
		if !errors.Is(err, tc.class) {
			t.Fatalf("scan %q: got %v, want a %v error", tc.src, err, tc.class)
		}
	}
}

func TestScannerLimits(t *testing.T) {
	t.Parallel()
	s := newScanner("a=1&b=2&c=3", 3)
	var err *Error
	for i := 0; i < 16; i++ {
		tok, scanErr := s.next()
		if scanErr != nil {
			err = scanErr
			break
		}
		if tok.Kind == tokEOF {
			break
		}
	}
	if err == nil || !errors.Is(err, ErrTooComplex) {
		t.Fatalf("token cap: got %v, want ErrTooComplex", err)
	}

	cfg := NewConfig()
	cfg.MaxInputLen = 8
	cfg.Field("name", TypeString)
	tr := NewTranslator(cfg, Postgres{})
	if _, terr := tr.Translate(newTestContext(t), "name=abcdefghij"); !errors.Is(terr, ErrTooComplex) {
		t.Fatalf("input length cap: got %v, want ErrTooComplex", terr)
	}
}
