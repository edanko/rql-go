package rql

import (
	"strconv"
	"strings"
	"time"
)

// ValueKind enumerates the kinds a query value can have.
type ValueKind uint8

// Value kinds produced by the RQL converters.
const (
	// KindNull is the RQL null (and undefined) literal.
	KindNull ValueKind = iota
	// KindString is a string value.
	KindString
	// KindInt is an integer value, kept as int64 so no precision is lost.
	KindInt
	// KindFloat is a floating point value.
	KindFloat
	// KindBool is a boolean value.
	KindBool
	// KindTime is a date or timestamp value.
	KindTime
	// KindUUID is a validated UUID value, carried as its canonical text.
	KindUUID
	// KindArray is an array of values, as written with (a,b,c).
	KindArray
)

// String returns a short name for the value kind, for diagnostics.
func (k ValueKind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindString:
		return "string"
	case KindInt:
		return "int"
	case KindFloat:
		return "float"
	case KindBool:
		return "bool"
	case KindTime:
		return "time"
	case KindUUID:
		return "uuid"
	case KindArray:
		return "array"
	default:
		return "unknown"
	}
}

// Value is a parsed RQL value.
//
// It is a tagged struct rather than an interface so that parsing and coercion
// stay allocation free. Exactly one of the payload fields is meaningful, chosen
// by Kind.
type Value struct {
	// Kind selects which payload field is meaningful.
	Kind ValueKind
	// Str holds a string or canonical UUID value.
	Str string
	// Int holds an integer value.
	Int int64
	// Float holds a floating point value.
	Float float64
	// Bool holds a boolean value.
	Bool bool
	// Time holds a date or timestamp value.
	Time time.Time
	// Array holds the elements of an array value.
	Array []Value
}

// isNull reports whether the value is SQL NULL.
func (v Value) isNull() bool { return v.Kind == KindNull }

// isArray reports whether the value is an array.
func (v Value) isArray() bool { return v.Kind == KindArray }

// String renders the value for diagnostics.
func (v Value) String() string {
	switch v.Kind {
	case KindNull:
		return "null"
	case KindString:
		return v.Str
	case KindInt:
		return strconv.FormatInt(v.Int, 10)
	case KindFloat:
		return strconv.FormatFloat(v.Float, 'g', -1, 64)
	case KindBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case KindTime:
		return v.Time.Format(time.RFC3339Nano)
	case KindUUID:
		return v.Str
	case KindArray:
		var b strings.Builder
		b.WriteByte('(')
		for i := range v.Array {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(v.Array[i].String())
		}
		b.WriteByte(')')
		return b.String()
	default:
		return "unknown"
	}
}

// Any returns the Go value to bind as a SQL parameter: nil, string, int64,
// float64, bool, time.Time, or a slice for array values (a typed slice when the
// elements share a kind, otherwise []any).
func (v Value) Any() any {
	switch v.Kind {
	case KindNull:
		return nil
	case KindString, KindUUID:
		return v.Str
	case KindInt:
		return v.Int
	case KindFloat:
		return v.Float
	case KindBool:
		return v.Bool
	case KindTime:
		return v.Time
	case KindArray:
		return v.arrayAny()
	default:
		return nil
	}
}

// arrayAny converts the elements to the narrowest typed slice that can carry
// them, which lets pgx encode arrays without an explicit cast.
func (v Value) arrayAny() any {
	if len(v.Array) == 0 {
		return []any{}
	}
	kind := v.Array[0].Kind
	for i := 1; i < len(v.Array); i++ {
		if v.Array[i].Kind != kind {
			return v.anySlice()
		}
	}
	switch kind {
	case KindString, KindUUID:
		out := make([]string, len(v.Array))
		for i := range v.Array {
			out[i] = v.Array[i].Str
		}
		return out
	case KindInt:
		out := make([]int64, len(v.Array))
		for i := range v.Array {
			out[i] = v.Array[i].Int
		}
		return out
	case KindFloat:
		out := make([]float64, len(v.Array))
		for i := range v.Array {
			out[i] = v.Array[i].Float
		}
		return out
	case KindBool:
		out := make([]bool, len(v.Array))
		for i := range v.Array {
			out[i] = v.Array[i].Bool
		}
		return out
	case KindTime:
		out := make([]time.Time, len(v.Array))
		for i := range v.Array {
			out[i] = v.Array[i].Time
		}
		return out
	default:
		return v.anySlice()
	}
}

// anySlice converts the elements to []any.
func (v Value) anySlice() []any {
	out := make([]any, len(v.Array))
	for i := range v.Array {
		out[i] = v.Array[i].Any()
	}
	return out
}

// scalarString returns the value rendered as a string, as a string-typed column
// would see it. It is used when a non-string value is coerced to a string field.
func (v Value) scalarString() string {
	switch v.Kind {
	case KindString, KindUUID:
		return v.Str
	case KindInt:
		return strconv.FormatInt(v.Int, 10)
	case KindFloat:
		return strconv.FormatFloat(v.Float, 'g', -1, 64)
	case KindBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case KindTime:
		return v.Time.Format(time.RFC3339Nano)
	default:
		return ""
	}
}

// timeLayouts are the accepted timestamp spellings, most specific first.
var timeLayouts = [...]string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"15:04:05",
}

// looksLikeTime reports whether text starts with an ISO date, so that the
// comparatively expensive time.Parse calls only run on plausible input.
func looksLikeTime(text string) bool {
	if len(text) < 10 {
		return false
	}
	if text[4] != '-' || text[7] != '-' {
		return false
	}
	for _, i := range [...]int{0, 1, 2, 3, 5, 6, 8, 9} {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// parseTimeText parses an ISO-8601 date or timestamp.
func parseTimeText(text string) (time.Time, bool) {
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, text); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// isIntegerLiteral reports whether text is a base-10 integer literal without a
// leading zero, so that values like "01234" survive as strings (a string column
// must not lose its leading zero).
func isIntegerLiteral(text string) bool {
	if text == "" {
		return false
	}
	i := 0
	if text[0] == '-' {
		i = 1
		if len(text) == 1 {
			return false
		}
	}
	digits := text[i:]
	if len(digits) > 1 && digits[0] == '0' {
		return false
	}
	for j := 0; j < len(digits); j++ {
		if digits[j] < '0' || digits[j] > '9' {
			return false
		}
	}
	return true
}

// looksLikeNumber reports whether text could be a number. It exists so that
// parsing a plain string value does not pay for the error values strconv
// allocates when a conversion fails.
func looksLikeNumber(text string) bool {
	if text == "" {
		return false
	}
	i := 0
	if text[0] == '-' || text[0] == '+' {
		i = 1
	}
	if i >= len(text) {
		return false
	}
	c := text[i]
	return c >= '0' && c <= '9'
}

// hasLeadingZeroInteger reports whether text is a base-10 integer written with
// a redundant leading zero, such as a postal code. Such a value stays a string so
// that no significant digit is lost.
func hasLeadingZeroInteger(text string) bool {
	i := 0
	if i < len(text) && (text[i] == '-' || text[i] == '+') {
		i++
	}
	if i+1 >= len(text) || text[i] != '0' {
		return false
	}
	for j := i + 1; j < len(text); j++ {
		if text[j] < '0' || text[j] > '9' {
			return false
		}
	}
	return true
}

// parseNumberLiteral parses text as an integer (int64, exact) or a float.
func parseNumberLiteral(text string) (Value, bool) {
	if !looksLikeNumber(text) || hasLeadingZeroInteger(text) {
		return Value{}, false
	}
	if isIntegerLiteral(text) {
		v, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return Value{}, false
		}
		return Value{Kind: KindInt, Int: v}, true
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return Value{Kind: KindFloat, Float: f}, true
	}
	return Value{}, false
}

// converterSplit returns the index of the ':' that ends a converter prefix, or
// -1 when text does not start with an alphabetic converter prefix.
func converterSplit(text string) int {
	idx := strings.IndexByte(text, ':')
	if idx <= 0 {
		return -1
	}
	for i := 0; i < idx; i++ {
		c := text[i]
		isLower := c >= 'a' && c <= 'z'
		isUpper := c >= 'A' && c <= 'Z'
		if !isLower && !isUpper {
			return -1
		}
	}
	return idx
}

// parseLiteral converts decoded value text into a Value, applying the RQL
// converter prefixes, literal keywords, single-quote stripping and the
// date/number/string inference rules.
func parseLiteral(text string, pos int) (Value, *Error) {
	// A $n parameter reference has no channel in this API; binding the literal
	// text would hide the mistake, so it is rejected.
	if isParamRefText(text) {
		return Value{}, errAt(ErrUnsupportedOperator, pos,
			"parameter reference "+quote(text)+" is not supported; values must be inlined")
	}
	// Single-quote stripping (jsonQueryCompatible behaviour): a value wrapped in
	// single quotes is taken literally.
	if len(text) >= 2 && text[0] == '\'' && text[len(text)-1] == '\'' {
		return Value{Kind: KindString, Str: text[1 : len(text)-1]}, nil
	}
	if idx := converterSplit(text); idx > 0 {
		return applyConverter(text[:idx], text[idx+1:], pos)
	}
	if v, ok := parseKeyword(text); ok {
		return v, nil
	}
	if looksLikeTime(text) {
		if t, ok := parseTimeText(text); ok {
			return Value{Kind: KindTime, Time: t}, nil
		}
	}
	if v, ok := parseNumberLiteral(text); ok {
		return v, nil
	}
	return Value{Kind: KindString, Str: text}, nil
}

// parseKeyword recognises the literal keywords accepted in value position.
func parseKeyword(text string) (Value, bool) {
	switch len(text) {
	case 4:
		if strings.EqualFold(text, "true") {
			return Value{Kind: KindBool, Bool: true}, true
		}
		if strings.EqualFold(text, "null") {
			return Value{Kind: KindNull}, true
		}
	case 5:
		if strings.EqualFold(text, "false") {
			return Value{Kind: KindBool, Bool: false}, true
		}
	case 9:
		// The specification lists undefined as a value; this package maps it to
		// SQL NULL rather than binding the literal text "undefined".
		if strings.EqualFold(text, "undefined") {
			return Value{Kind: KindNull}, true
		}
	}
	return Value{}, false
}

// isParamRefText reports whether text is a $n parameter reference.
func isParamRefText(text string) bool {
	if len(text) < 2 || text[0] != '$' {
		return false
	}
	for i := 1; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// applyConverter applies an explicit converter prefix.
func applyConverter(prefix, rest string, pos int) (Value, *Error) {
	switch strings.ToLower(prefix) {
	case "string":
		return Value{Kind: KindString, Str: rest}, nil
	case "number":
		if v, ok := parseNumberLiteral(rest); ok {
			return v, nil
		}
		return Value{}, errAt(ErrInvalidValue, pos, "number converter requires a numeric value: "+quote(rest))
	case "boolean", "bool":
		switch {
		case strings.EqualFold(rest, "true"):
			return Value{Kind: KindBool, Bool: true}, nil
		case strings.EqualFold(rest, "false"):
			return Value{Kind: KindBool, Bool: false}, nil
		}
		return Value{}, errAt(ErrInvalidValue, pos, "boolean converter requires true or false: "+quote(rest))
	case "date", "time", "datetime":
		if t, ok := parseTimeText(rest); ok {
			return Value{Kind: KindTime, Time: t}, nil
		}
		return Value{}, errAt(ErrInvalidValue, pos, "date converter requires an ISO-8601 value: "+quote(rest))
	case "isodate":
		return convertISODate(rest, pos)
	case "epoch":
		ms, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return Value{}, errAt(ErrInvalidValue, pos, "epoch converter requires integer milliseconds: "+quote(rest))
		}
		return Value{Kind: KindTime, Time: time.UnixMilli(ms).UTC()}, nil
	case "auto":
		if v, ok := parseKeyword(rest); ok {
			return v, nil
		}
		if looksLikeTime(rest) {
			if t, ok := parseTimeText(rest); ok {
				return Value{Kind: KindTime, Time: t}, nil
			}
		}
		if v, ok := parseNumberLiteral(rest); ok {
			return v, nil
		}
		return Value{Kind: KindString, Str: rest}, nil
	case "re", "regex", "regexp", "glob":
		// A regular expression cannot be bound as a SQL parameter safely, and
		// binding the literal text (the previous behaviour) hides the mistake.
		return Value{}, errAt(ErrUnsupportedConverter, pos,
			"converter "+quote(prefix)+" is not supported because regular expressions cannot be bound as SQL parameters")
	default:
		return Value{}, errAt(ErrUnsupportedConverter, pos, "unknown converter "+quote(prefix))
	}
}

// elemType returns the element type of an array field type, or the type itself.
func (t FieldType) elemType() FieldType {
	switch t {
	case TypeStringArray:
		return TypeString
	case TypeUUIDArray:
		return TypeUUID
	case TypeArray:
		return TypeArray
	default:
		return t
	}
}

// isArrayType reports whether the field type holds multiple values.
func (t FieldType) isArrayType() bool {
	switch t {
	case TypeArray, TypeStringArray, TypeUUIDArray:
		return true
	default:
		return false
	}
}

// Coerce converts v to the field type t, validating it exactly as the legacy
// translator did: a non-numeric value for an integer field, an ill-formed UUID,
// a non-boolean value for a boolean field and an unparseable date are all
// errors, never silently bound.
func Coerce(v Value, t FieldType) (Value, *Error) {
	return coerceAt(v, t, -1)
}

// coerceAt is Coerce with an error position.
func coerceAt(v Value, t FieldType, pos int) (Value, *Error) {
	if t.isArrayType() {
		if v.Kind != KindArray {
			return Value{}, errAt(ErrInvalidValue, pos, "expected an array value, got "+v.Kind.String())
		}
		elem := t.elemType()
		if elem == t {
			// TypeArray carries no element type: keep the elements as parsed.
			return v, nil
		}
		out := make([]Value, len(v.Array))
		for i := range v.Array {
			coerced, err := coerceAt(v.Array[i], elem, pos)
			if err != nil {
				return Value{}, err
			}
			out[i] = coerced
		}
		return Value{Kind: KindArray, Array: out}, nil
	}
	if v.Kind == KindNull {
		return v, nil
	}
	switch t {
	case TypeString:
		return coerceString(v, pos)
	case TypeInt:
		return coerceInt(v, pos)
	case TypeFloat:
		return coerceFloat(v, pos)
	case TypeBool:
		return coerceBool(v, pos)
	case TypeTime:
		return coerceTime(v, pos, false)
	case TypeDate:
		return coerceTime(v, pos, true)
	case TypeUUID:
		return coerceUUID(v, pos)
	case TypeArray, TypeStringArray, TypeUUIDArray:
		// Array types were handled above; a scalar cannot fill one.
		return Value{}, errAt(ErrInvalidValue, pos, "expected an array value, got "+v.Kind.String())
	default:
		// TypeJSON and any future type: bind the value as parsed.
		return v, nil
	}
}

// coerceString converts any scalar to its text form.
func coerceString(v Value, pos int) (Value, *Error) {
	switch v.Kind {
	case KindString, KindUUID:
		return Value{Kind: KindString, Str: v.Str}, nil
	case KindArray:
		return Value{}, errAt(ErrInvalidValue, pos, "expected a scalar value, got "+v.String())
	default:
		return Value{Kind: KindString, Str: v.scalarString()}, nil
	}
}

// coerceInt converts v to int64, rejecting fractional and non-numeric values.
func coerceInt(v Value, pos int) (Value, *Error) {
	switch v.Kind {
	case KindInt:
		return v, nil
	case KindFloat:
		if v.Float != float64(int64(v.Float)) {
			return Value{}, errAt(ErrInvalidValue, pos, "expected an integer, got "+v.String())
		}
		return Value{Kind: KindInt, Int: int64(v.Float)}, nil
	case KindString:
		n, err := strconv.ParseInt(v.Str, 10, 64)
		if err != nil {
			return Value{}, errAt(ErrInvalidValue, pos, "expected an integer, got "+quote(v.Str))
		}
		return Value{Kind: KindInt, Int: n}, nil
	default:
		return Value{}, errAt(ErrInvalidValue, pos, "expected an integer, got "+v.Kind.String())
	}
}

// coerceFloat converts v to float64.
func coerceFloat(v Value, pos int) (Value, *Error) {
	switch v.Kind {
	case KindFloat:
		return v, nil
	case KindInt:
		return Value{Kind: KindFloat, Float: float64(v.Int)}, nil
	case KindString:
		f, err := strconv.ParseFloat(v.Str, 64)
		if err != nil {
			return Value{}, errAt(ErrInvalidValue, pos, "expected a number, got "+quote(v.Str))
		}
		return Value{Kind: KindFloat, Float: f}, nil
	default:
		return Value{}, errAt(ErrInvalidValue, pos, "expected a number, got "+v.Kind.String())
	}
}

// coerceBool converts v to a boolean.
func coerceBool(v Value, pos int) (Value, *Error) {
	switch v.Kind {
	case KindBool:
		return v, nil
	case KindString:
		switch {
		case strings.EqualFold(v.Str, "true"):
			return Value{Kind: KindBool, Bool: true}, nil
		case strings.EqualFold(v.Str, "false"):
			return Value{Kind: KindBool, Bool: false}, nil
		}
		return Value{}, errAt(ErrInvalidValue, pos, "expected a boolean, got "+quote(v.Str))
	default:
		return Value{}, errAt(ErrInvalidValue, pos, "expected a boolean, got "+v.Kind.String())
	}
}

// coerceTime converts v to a timestamp, truncating to a date when dateOnly.
func coerceTime(v Value, pos int, dateOnly bool) (Value, *Error) {
	switch v.Kind {
	case KindTime:
		if dateOnly {
			return Value{Kind: KindTime, Time: truncateDate(v.Time)}, nil
		}
		return v, nil
	case KindString:
		t, ok := parseTimeText(v.Str)
		if !ok {
			want := "an ISO-8601 timestamp"
			if dateOnly {
				want = "an ISO-8601 date"
			}
			return Value{}, errAt(ErrInvalidValue, pos, "expected "+want+", got "+quote(v.Str))
		}
		if dateOnly {
			t = truncateDate(t)
		}
		return Value{Kind: KindTime, Time: t}, nil
	default:
		want := "a timestamp"
		if dateOnly {
			want = "a date"
		}
		return Value{}, errAt(ErrInvalidValue, pos, "expected "+want+", got "+v.Kind.String())
	}
}

// coerceUUID validates a canonical UUID, rejecting every other spelling.
func coerceUUID(v Value, pos int) (Value, *Error) {
	var text string
	switch v.Kind {
	case KindString, KindUUID:
		text = v.Str
	default:
		return Value{}, errAt(ErrInvalidValue, pos, "expected a UUID, got "+v.Kind.String())
	}
	canonical, ok := canonicalUUID(text)
	if !ok {
		return Value{}, errAt(ErrInvalidValue, pos, "invalid UUID "+quote(text))
	}
	return Value{Kind: KindUUID, Str: canonical}, nil
}

// valueType returns the field type implied by a value's kind. It is used to cast
// text extracted from an untyped (json) leaf so that it compares with a typed
// parameter.
func valueType(v Value) FieldType {
	switch v.Kind {
	case KindInt:
		return TypeInt
	case KindFloat:
		return TypeFloat
	case KindBool:
		return TypeBool
	case KindTime:
		return TypeTime
	case KindUUID:
		return TypeUUID
	default:
		return TypeString
	}
}

// sameValue reports whether two coerced values are identical, which lets
// coerceElements skip building a new slice when nothing changed.
func sameValue(a, b Value) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case KindNull:
		return true
	case KindString, KindUUID:
		return a.Str == b.Str
	case KindInt:
		return a.Int == b.Int
	case KindFloat:
		return a.Float == b.Float
	case KindBool:
		return a.Bool == b.Bool
	case KindTime:
		return a.Time.Equal(b.Time)
	default:
		return false
	}
}

// truncateDate zeroes the time-of-day of a date value.
func truncateDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// canonicalUUID validates the canonical 8-4-4-4-12 hexadecimal UUID form and
// returns its lower-case spelling. It deliberately accepts no other spelling, so
// a malformed identifier cannot reach the database.
func canonicalUUID(s string) (string, bool) {
	if len(s) != 36 {
		return "", false
	}
	var buf [36]byte
	for i := 0; i < 36; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return "", false
			}
			buf[i] = '-'
			continue
		}
		if !isHexDigit(c) {
			return "", false
		}
		if c >= 'A' && c <= 'F' {
			c += 'a' - 'A'
		}
		buf[i] = c
	}
	return string(buf[:]), true
}

// ParseValue converts raw text into a value of the given field type. It is the
// entry point for callers that build values outside a query string.
func ParseValue(text string, t FieldType) (Value, error) {
	v, err := parseLiteral(text, -1)
	if err != nil {
		return Value{}, err
	}
	cv, err := coerceAt(v, t, -1)
	if err != nil {
		return Value{}, err
	}
	return cv, nil
}

// isDecimal reports whether s is a non-empty run of ASCII digits.
func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// convertISODate implements the spec's partial-date converter. A complete ISO-8601 value is
// accepted unchanged; shorter values are completed from a template, so "isodate:2024" is
// 2024-01-01T00:00:00Z, "isodate:2024-06" is 2024-06-01T00:00:00Z and "isodate:2024-06-09T10"
// is 10:00:00Z. That matches the reference implementation, which completes any truncated
// prefix up to a full timestamp.
//
// Two deliberate deviations from the reference, both because it is silently wrong there:
//   - "isodate:24" means the year 24 (ISO 8601), not 1924 (a JavaScript Date artefact).
//   - an out-of-range value such as "isodate:2024-13" is rejected instead of rolling over
//     into the next year, per the fail-closed rule in docs/DESIGN.md §3.
func convertISODate(rest string, pos int) (Value, *Error) {
	if t, ok := parseTimeText(rest); ok {
		return Value{Kind: KindTime, Time: t}, nil
	}

	const tpl = "0000-01-01T00:00:00Z"
	padded := rest
	if len(padded) < 4 {
		// Zero-pad a bare year, matching '0000'.substr(0, 4-len) in the reference.
		padded = strings.Repeat("0", 4-len(padded)) + padded
	}
	if len(padded) <= len(tpl) {
		if t, ok := parseTimeText(padded + tpl[len(padded):]); ok {
			return Value{Kind: KindTime, Time: t}, nil
		}
	}
	return Value{}, errAt(ErrInvalidValue, pos,
		"isodate converter requires a partial or full ISO-8601 date: "+quote(rest))
}
