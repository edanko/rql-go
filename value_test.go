package rql

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestParseLiteralKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		kind ValueKind
	}{
		{"abc", KindString},
		{"true", KindBool},
		{"FALSE", KindBool},
		{"null", KindNull},
		{"NULL", KindNull},
		{"undefined", KindNull},
		{"42", KindInt},
		{"-7", KindInt},
		{"1.5", KindFloat},
		{"01234", KindString}, // leading zero stays a string so nothing is lost
		{"2000-01-01T00:00:00Z", KindTime},
		{"2000-01-01", KindTime},
		{"string:3", KindString},
		{"number:3", KindInt},
		{"number:1.25", KindFloat},
		{"boolean:true", KindBool},
		{"date:2000-01-01T00:00:00Z", KindTime},
		{"isodate:2000-01-01", KindTime},
		{"epoch:946684800000", KindTime},
		{"'quoted'", KindString},
	}
	for _, tc := range cases {
		v, err := parseLiteral(tc.text, 0)
		if err != nil {
			t.Fatalf("parseLiteral(%q): %v", tc.text, err)
		}
		if v.Kind != tc.kind {
			t.Fatalf("parseLiteral(%q) kind = %v, want %v", tc.text, v.Kind, tc.kind)
		}
	}
}

func TestParseLiteralValues(t *testing.T) {
	t.Parallel()
	if v, err := parseLiteral("string:3", 0); err != nil || v.Str != "3" {
		t.Fatalf("string:3 = %v, %v", v, err)
	}
	if v, err := parseLiteral("'bar'", 0); err != nil || v.Str != "bar" {
		t.Fatalf("quoted value = %v, %v", v, err)
	}
	if v, err := parseLiteral("epoch:946684800000", 0); err != nil {
		t.Fatalf("epoch: %v", err)
	} else if !v.Time.Equal(time.UnixMilli(946684800000).UTC()) {
		t.Fatalf("epoch value = %v", v.Time)
	}
	if v, err := parseLiteral("2000-01-01T00:00:00Z", 0); err != nil {
		t.Fatalf("iso date: %v", err)
	} else if !v.Time.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("iso date value = %v", v.Time)
	}
}

func TestParseLiteralInt64Precision(t *testing.T) {
	t.Parallel()
	const text = "9007199254740993" // 2^53+1: not representable as float64
	v, err := parseLiteral(text, 0)
	if err != nil {
		t.Fatalf("parseLiteral: %v", err)
	}
	if v.Kind != KindInt || v.Int != 9007199254740993 {
		t.Fatalf("got %v (%d), want the exact int64", v.Kind, v.Int)
	}
	if got := v.Any(); got != int64(9007199254740993) {
		t.Fatalf("Any() = %#v", got)
	}
	if got := v.String(); got != text {
		t.Fatalf("String() = %q, want %q", got, text)
	}
}

func TestParseLiteralRejectsUnsupportedConverters(t *testing.T) {
	t.Parallel()
	// Any alphabetic prefix before ':' is a converter reference, so an unknown
	// one is rejected rather than silently bound as text (the documented rule).
	for _, text := range []string{"re:^ab", "RE:ab", "glob:ab*", "unknown:1", "regex:x", "regexp:x", "a:b-c"} {
		_, err := parseLiteral(text, 3)
		if err == nil {
			t.Fatalf("parseLiteral(%q): expected an error", text)
		}
		if !errors.Is(err, ErrUnsupportedConverter) {
			t.Fatalf("parseLiteral(%q): got %v, want ErrUnsupportedConverter", text, err)
		}
		if err.Pos != 3 {
			t.Fatalf("parseLiteral(%q): position %d, want 3", text, err.Pos)
		}
	}
}

func TestParseLiteralRejectsParamRef(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"$1", "$42"} {
		if _, err := parseLiteral(text, 0); !errors.Is(err, ErrUnsupportedOperator) {
			t.Fatalf("parseLiteral(%q): got %v, want ErrUnsupportedOperator", text, err)
		}
	}
	// A '$' that does not start a digit reference is an ordinary string.
	if v, err := parseLiteral("$abc", 0); err != nil || v.Str != "$abc" {
		t.Fatalf("$abc = %v, %v", v, err)
	}
}

func TestCoerceByFieldType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  Value
		typ  FieldType
		kind ValueKind
		want any
	}{
		{"int from string", Value{Kind: KindString, Str: "42"}, TypeInt, KindInt, int64(42)},
		{"int from float", Value{Kind: KindFloat, Float: 3}, TypeInt, KindInt, int64(3)},
		{"string from int", Value{Kind: KindInt, Int: 3}, TypeString, KindString, "3"},
		{"string from bool", Value{Kind: KindBool, Bool: true}, TypeString, KindString, "true"},
		{"float from int", Value{Kind: KindInt, Int: 2}, TypeFloat, KindFloat, float64(2)},
		{"float from string", Value{Kind: KindString, Str: "2.5"}, TypeFloat, KindFloat, 2.5},
		{"bool from string", Value{Kind: KindString, Str: "TRUE"}, TypeBool, KindBool, true},
		{"uuid", Value{Kind: KindString, Str: "8A05D590-E085-4245-A164-AD9A5D4654B3"}, TypeUUID, KindUUID, "8a05d590-e085-4245-a164-ad9a5d4654b3"},
		{"time from string", Value{Kind: KindString, Str: "2021-06-09T11:00:00Z"}, TypeTime, KindTime, nil},
		{"null stays null", Value{Kind: KindNull}, TypeInt, KindNull, nil},
	}
	for _, tc := range cases {
		got, err := Coerce(tc.val, tc.typ)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Kind != tc.kind {
			t.Fatalf("%s: kind = %v, want %v", tc.name, got.Kind, tc.kind)
		}
		if tc.want != nil && got.Any() != tc.want {
			t.Fatalf("%s: value = %#v, want %#v", tc.name, got.Any(), tc.want)
		}
	}
}

func TestCoerceRejectsBadValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  Value
		typ  FieldType
	}{
		{"non-numeric int", Value{Kind: KindString, Str: "abc"}, TypeInt},
		{"fractional int", Value{Kind: KindFloat, Float: 1.5}, TypeInt},
		{"bool as int", Value{Kind: KindBool, Bool: true}, TypeInt},
		{"non-numeric float", Value{Kind: KindString, Str: "abc"}, TypeFloat},
		{"non-bool", Value{Kind: KindString, Str: "maybe"}, TypeBool},
		{"bad date", Value{Kind: KindString, Str: "not-a-date"}, TypeTime},
		{"short uuid", Value{Kind: KindString, Str: "not-a-uuid"}, TypeUUID},
		{"scalar for array", Value{Kind: KindString, Str: "x"}, TypeStringArray},
	}
	for _, tc := range cases {
		if _, err := Coerce(tc.val, tc.typ); err == nil {
			t.Fatalf("%s: expected an error", tc.name)
		} else if !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("%s: got %v, want ErrInvalidValue", tc.name, err)
		}
	}
}

func TestCoerceArrayElementTypes(t *testing.T) {
	t.Parallel()
	in := Value{Kind: KindArray, Array: []Value{
		{Kind: KindString, Str: "1"},
		{Kind: KindString, Str: "2"},
	}}
	got, err := Coerce(in, TypeStringArray)
	if err != nil {
		t.Fatalf("string array: %v", err)
	}
	if items, ok := got.Any().([]string); !ok || len(items) != 2 || items[0] != "1" {
		t.Fatalf("string array value = %#v", got.Any())
	}

	bad := Value{Kind: KindArray, Array: []Value{{Kind: KindString, Str: "not-a-uuid"}}}
	if _, err := Coerce(bad, TypeUUIDArray); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("uuid array: got %v, want ErrInvalidValue", err)
	}
}

func TestValueAnyShapes(t *testing.T) {
	t.Parallel()
	ints := Value{Kind: KindArray, Array: []Value{{Kind: KindInt, Int: 1}, {Kind: KindInt, Int: 2}}}
	if got, ok := ints.Any().([]int64); !ok || len(got) != 2 {
		t.Fatalf("int array = %#v", ints.Any())
	}
	mixed := Value{Kind: KindArray, Array: []Value{{Kind: KindInt, Int: 1}, {Kind: KindString, Str: "x"}}}
	if got, ok := mixed.Any().([]any); !ok || len(got) != 2 {
		t.Fatalf("mixed array = %#v", mixed.Any())
	}
	if (Value{Kind: KindNull}).Any() != nil {
		t.Fatal("null should bind as nil")
	}
	if (Value{Kind: KindFloat, Float: math.MaxFloat64}).Any() != math.MaxFloat64 {
		t.Fatal("float round-trip failed")
	}
}

func TestParseValueEntryPoint(t *testing.T) {
	t.Parallel()
	v, err := ParseValue("42", TypeInt)
	if err != nil {
		t.Fatalf("ParseValue: %v", err)
	}
	if v.Kind != KindInt || v.Int != 42 {
		t.Fatalf("ParseValue = %v", v)
	}
	if _, err := ParseValue("abc", TypeInt); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("ParseValue(abc, int): got %v", err)
	}
}

func TestCanonicalUUID(t *testing.T) {
	t.Parallel()
	if _, ok := canonicalUUID("8a05d590-e085-4245-a164-ad9a5d4654b3"); !ok {
		t.Fatal("valid UUID rejected")
	}
	for _, bad := range []string{
		"", "8a05d590", "8a05d590-e085-4245-a164-ad9a5d4654b",
		"8a05d590e0854245a164ad9a5d4654b3x", "8a05d590-e085-4245-a164-ad9a5d4654b3x",
		"8a05d590_e085_4245_a164_ad9a5d4654b3",
	} {
		if _, ok := canonicalUUID(bad); ok {
			t.Fatalf("accepted invalid UUID %q", bad)
		}
	}
}

// TestIsodatePartialThroughTranslator covers the spec's partial-date converter end to end:
// the year is padded to four digits and the remainder defaults to January 1st. The reference
// implementation corrupts inputs longer than four digits, so those are rejected instead.
//
// It goes through Translate rather than the value parser because converter prefixes are
// resolved by the parser, which is the surface users actually exercise.
func TestIsodatePartialThroughTranslator(t *testing.T) {
	t.Parallel()

	cfg := NewConfig()
	cfg.Field("d", TypeTime)
	tr := NewTranslator(cfg, Postgres{})

	tests := []struct {
		name    string
		query   string
		want    string
		wantErr bool
	}{
		{name: "four digit year", query: "d=isodate:2024", want: "2024-01-01T00:00:00Z"},
		{name: "three digit year padded", query: "d=isodate:999", want: "0999-01-01T00:00:00Z"},
		{name: "year and month", query: "d=isodate:2024-06", want: "2024-06-01T00:00:00Z"},
		{name: "year month day hour", query: "d=isodate:2024-06-09T10", want: "2024-06-09T10:00:00Z"},
		{name: "year month day hour minute", query: "d=isodate:2024-06-09T10:30", want: "2024-06-09T10:30:00Z"},
		{name: "full date accepted unchanged", query: "d=isodate:2024-05-06T07:08:09Z", want: "2024-05-06T07:08:09Z"},
		{name: "two digit year is year 24 not 1924", query: "d=isodate:24", want: "0024-01-01T00:00:00Z"},
		// Deliberate deviations from the reference, both fail-closed: the reference rolls the
		// month over into 2025 and mangles compact dates, which is the "silently wrong" class
		// docs/DESIGN.md §3 forbids.
		{name: "out of range month rejected", query: "d=isodate:2024-13", wantErr: true},
		{name: "compact digits rejected", query: "d=isodate:20240101", wantErr: true},
		{name: "not a number rejected", query: "d=isodate:20x4", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := tr.Translate(t.Context(), tt.query)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Translate(%q) = %#v, want error", tt.query, res)
				}
				return
			}
			if err != nil {
				t.Fatalf("Translate(%q): %v", tt.query, err)
			}
			if len(res.Args) != 1 {
				t.Fatalf("Translate(%q) args = %v, want 1 argument", tt.query, res.Args)
			}
			tm, ok := res.Args[0].(time.Time)
			if !ok {
				t.Fatalf("Translate(%q) arg type = %T, want time.Time", tt.query, res.Args[0])
			}
			if got := tm.UTC().Format("2006-01-02T15:04:05Z"); got != tt.want {
				t.Fatalf("Translate(%q) = %s, want %s", tt.query, got, tt.want)
			}
		})
	}
}
