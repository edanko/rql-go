package rql

import (
	"context"
	"strings"
	"unicode"
)

// FieldType declares the type of a configured field. It drives value coercion
// and validation, the default set of allowed operators, and the SQL cast applied
// to JSONB path extraction.
type FieldType uint8

// Supported field types.
const (
	// TypeString is a text column.
	TypeString FieldType = iota
	// TypeInt is a 64-bit integer column.
	TypeInt
	// TypeFloat is a floating point column.
	TypeFloat
	// TypeBool is a boolean column.
	TypeBool
	// TypeTime is a timestamp (with or without time zone) column.
	TypeTime
	// TypeDate is a date column.
	TypeDate
	// TypeUUID is a UUID column.
	TypeUUID
	// TypeArray is an array column of unspecified element type.
	TypeArray
	// TypeStringArray is a text array column.
	TypeStringArray
	// TypeUUIDArray is a UUID array column.
	TypeUUIDArray
	// TypeJSON is a jsonb column.
	TypeJSON
	numFieldTypes
)

// String returns the field type name, for diagnostics.
func (t FieldType) String() string {
	switch t {
	case TypeString:
		return "string"
	case TypeInt:
		return "int"
	case TypeFloat:
		return "float"
	case TypeBool:
		return "bool"
	case TypeTime:
		return "time"
	case TypeDate:
		return "date"
	case TypeUUID:
		return "uuid"
	case TypeArray:
		return "array"
	case TypeStringArray:
		return "string array"
	case TypeUUIDArray:
		return "uuid array"
	case TypeJSON:
		return "json"
	default:
		return "unknown"
	}
}

// OperatorKind selects the syntax a registered operator accepts.
type OperatorKind uint8

// Operator kinds.
const (
	// UnaryOp is called with the normalized form, e.g. `myOp(a,b)`.
	UnaryOp OperatorKind = iota
	// BinaryOp is called with the infix form, e.g. `field=myOp=value`, or with
	// the bare form `myOp=value` when the name is not a configured field.
	BinaryOp
)

// OpFunc implements a custom operator. It receives an OpContext for resolving
// columns, binding parameters and setting out-of-band result flags, plus the
// operator arguments (the raw field name first for BinaryOp).
//
// The returned SQL is a boolean expression for the WHERE clause. Returning an
// empty string means "this operator contributes no predicate" (used by
// flag-only operators); the node is then dropped from its conjunction.
type OpFunc func(ctx *OpContext, args []any) (string, error)

// defaultMaxDepth bounds query nesting.
const defaultMaxDepth = 32

// Default hard limits on input size.
const (
	defaultMaxInputLen = 1 << 20
	defaultMaxTokens   = 1 << 16
)

// field is a configured field: the RQL name, the SQL column it maps to, its
// type, the operators allowed on it, and per-operator column/operator overrides.
type field struct {
	name       string
	parts      []string // [column, jsonPath...] used for every operator
	typ        FieldType
	allowed    map[string]struct{}
	opMap      map[string]string
	opCols     map[string][]string
	opTypes    map[string]FieldType
	sortable   bool
	selectable bool
	// dynamic marks a root whose JSONB leaves need not be registered.
	dynamic bool
	// validator runs after value coercion; the context is the one passed to Translate, so a
	// check can read request-scoped values (tenant, request id) as well as the value.
	validator func(context.Context, any) error
	regErr    *Error
}

// allows reports whether the operator may be used on the field.
func (f *field) allows(op string) bool {
	if f.allowed == nil {
		return false
	}
	_, ok := f.allowed[op]
	return ok
}

// remap returns the effective operator name after per-field remapping.
func (f *field) remap(op string) string {
	if f.opMap != nil {
		if target, ok := f.opMap[op]; ok {
			return target
		}
	}
	return op
}

// typeFor returns the field type used to coerce values for an operator, which a
// per-operator override may change.
func (f *field) typeFor(op string) FieldType {
	if f.opTypes != nil {
		if t, ok := f.opTypes[op]; ok {
			return t
		}
	}
	return f.typ
}

// columnFor returns the column parts to use for an operator.
func (f *field) columnFor(op string) []string {
	if f.opCols != nil {
		if parts, ok := f.opCols[op]; ok {
			return parts
		}
	}
	return f.parts
}

// customOp is a registered operator implementation.
type customOp struct {
	name string
	kind OperatorKind
	fn   OpFunc
}

// Config describes which RQL fields exist, how they map to SQL columns, which
// operators they accept, and which custom operators are registered.
//
// A Config is read-only while translating: build it once, then share the
// resulting Translator across goroutines.
type Config struct {
	byName map[string]*field
	custom map[string]*customOp

	// MaxDepth bounds how deeply query groups may nest. Zero selects the
	// default (32).
	MaxDepth int
	// MaxInputLen bounds the query length in bytes. Zero selects the default
	// (1 MiB).
	MaxInputLen int
	// MaxTokens bounds the number of lexical tokens. Zero selects the default
	// (65536).
	MaxTokens int
}

// NewConfig returns an empty configuration. Call Field to declare the fields a
// query may reference; unregistered fields are rejected.
func NewConfig() *Config {
	return &Config{
		byName:      make(map[string]*field),
		custom:      make(map[string]*customOp),
		MaxDepth:    defaultMaxDepth,
		MaxInputLen: defaultMaxInputLen,
		MaxTokens:   defaultMaxTokens,
	}
}

// maxDepth returns the effective depth limit.
func (c *Config) maxDepth() int {
	if c == nil || c.MaxDepth <= 0 {
		return defaultMaxDepth
	}
	return c.MaxDepth
}

// maxInputLen returns the effective input length limit.
func (c *Config) maxInputLen() int {
	if c == nil || c.MaxInputLen <= 0 {
		return defaultMaxInputLen
	}
	return c.MaxInputLen
}

// maxTokens returns the effective token limit.
func (c *Config) maxTokens() int {
	if c == nil || c.MaxTokens <= 0 {
		return defaultMaxTokens
	}
	return c.MaxTokens
}

// Field declares an RQL field of the given type and returns a builder for its
// options. The SQL column defaults to the snake_case spelling of the field name;
// for a nested name such as "a.b" or "a/b" only the first segment is converted
// and the rest become JSONB path segments.
func (c *Config) Field(name string, t FieldType) *FieldBuilder {
	f := &field{
		name:    name,
		parts:   deriveColumn(name),
		typ:     t,
		allowed: defaultOps(t),
	}
	if root, ok := pathRoot(f.parts); !ok {
		f.regErr = errNoPos(ErrInvalidPath,
			"field "+quote(name)+" has an empty root segment")
	} else {
		_ = root
		for _, seg := range f.parts[1:] {
			if !isPathSegment(seg) {
				f.regErr = errNoPos(ErrInvalidPath,
					"field "+quote(name)+" has a path segment "+quote(seg)+" that is not a plain identifier")
				break
			}
		}
	}
	if name == "" {
		f.regErr = errNoPos(ErrUnknownField, "field name must not be empty")
	}
	c.byName[name] = f
	return &FieldBuilder{cfg: c, f: f}
}

// AllowDynamicPaths declares JSONB roots whose leaves need not be registered
// individually: a query may then address `root/segment` or `root.segment`
// without declaring the leaf. Each root is registered as a string-typed field
// (use Field to give it another type), every path segment is still validated
// against the identifier alphabet, and the segments are still resolved from the
// configuration rather than from user text.
func (c *Config) AllowDynamicPaths(roots ...string) *Config {
	for _, root := range roots {
		if f, ok := c.byName[root]; ok {
			f.dynamic = true
			continue
		}
		c.Field(root, TypeString).Dynamic()
	}
	return c
}

// RegisterOp registers (or replaces) a custom operator. A registered operator
// shadows a built-in operator of the same name for this configuration.
func (c *Config) RegisterOp(name string, kind OperatorKind, fn OpFunc) {
	if fn == nil {
		return
	}
	c.custom[strings.ToLower(name)] = &customOp{name: name, kind: kind, fn: fn}
}

// RegisterUnaryOp registers a function-style custom operator, e.g. `myOp(a,b)`.
func (c *Config) RegisterUnaryOp(name string, fn OpFunc) {
	c.RegisterOp(name, UnaryOp, fn)
}

// RegisterBinaryOp registers an infix-style custom operator, usable as
// `field=myOp=value` or as `myOp=value` when the name is not a field.
func (c *Config) RegisterBinaryOp(name string, fn OpFunc) {
	c.RegisterOp(name, BinaryOp, fn)
}

// DeleteOp removes a custom operator registration. Removing an operator that is
// not registered is a no-op.
func (c *Config) DeleteOp(name string) {
	delete(c.custom, strings.ToLower(name))
}

// lookupCustom returns the custom operator registered under the lower-case name.
func (c *Config) lookupCustom(name string) *customOp {
	if c == nil || c.custom == nil {
		return nil
	}
	return c.custom[name]
}

// lookupField returns the field registered under the exact RQL name.
func (c *Config) lookupField(name string) *field {
	if c == nil || c.byName == nil {
		return nil
	}
	return c.byName[name]
}

// FieldBuilder configures one field. Every method returns the builder so calls
// chain, e.g. cfg.Field("id", rql.TypeUUID).Allow("eq","in").Sortable().
type FieldBuilder struct {
	cfg *Config
	f   *field
}

// Column overrides the SQL column name. Dots introduce JSONB path segments, so
// Column("config.enabled") maps to `"config"->>'enabled'`.
func (b *FieldBuilder) Column(column string) *FieldBuilder {
	b.f.parts = deriveExplicitColumn(column)
	if _, ok := pathRoot(b.f.parts); !ok {
		b.f.regErr = errNoPos(ErrInvalidPath, "column "+quote(column)+" has an empty root segment")
		return b
	}
	if len(b.f.parts) > 1 {
		b.f.regErr = nil
		for _, seg := range b.f.parts[1:] {
			if !isPathSegment(seg) {
				b.f.regErr = errNoPos(ErrInvalidPath,
					"field "+quote(b.f.name)+" has a path segment "+quote(seg)+" that is not a plain identifier")
				break
			}
		}
	}
	return b
}

// Allow enables operators for this field, in addition to the type defaults.
func (b *FieldBuilder) Allow(ops ...string) *FieldBuilder {
	for _, op := range ops {
		b.f.allowed[strings.ToLower(op)] = struct{}{}
	}
	return b
}

// Forbid disables operators for this field.
func (b *FieldBuilder) Forbid(ops ...string) *FieldBuilder {
	for _, op := range ops {
		delete(b.f.allowed, strings.ToLower(op))
	}
	return b
}

// Sortable allows the field in the sort() operator.
func (b *FieldBuilder) Sortable() *FieldBuilder {
	b.f.sortable = true
	return b
}

// Dynamic marks the field as a JSONB container whose leaves may be addressed
// without registering each one (`config/theme`, `config.theme`). Every segment is
// still validated against the identifier alphabet.
func (b *FieldBuilder) Dynamic() *FieldBuilder {
	b.f.dynamic = true
	return b
}

// Selectable allows the field in the select() operator.
func (b *FieldBuilder) Selectable() *FieldBuilder {
	b.f.selectable = true
	return b
}

// Validator installs a custom value check. It runs after the value has been
// coerced to the field type; returning an error rejects the query.
func (b *FieldBuilder) Validator(fn func(any) error) *FieldBuilder {
	if fn == nil {
		b.f.validator = nil
		return b
	}
	b.f.validator = func(_ context.Context, v any) error { return fn(v) }
	return b
}

// ValidatorContext is Validator with access to the request context, for checks that depend on
// request-scoped values — a tenant code, a request id, the authenticated caller — or on the
// caller's deadline:
//
//	cfg.Field("status", rql.TypeString).ValidatorContext(func(ctx context.Context, v any) error {
//		status, _ := v.(string)
//		if !allowedStatusFor(ctx, status) {
//			return rql.ErrInvalidValue
//		}
//		return nil
//	})
//
// It runs after the value has been coerced to the field type, for each element of an array
// value, and returning an error rejects the query. The context is never nil.
func (b *FieldBuilder) ValidatorContext(fn func(context.Context, any) error) *FieldBuilder {
	b.f.validator = fn
	return b
}

// MapOp remaps an operator to another one for this field, which is how virtual
// fields are expressed (e.g. MapOp("eq","ge") for a "createdAtFrom" bound).
// Both operator names are enabled implicitly.
func (b *FieldBuilder) MapOp(from, to string) *FieldBuilder {
	from = strings.ToLower(from)
	to = strings.ToLower(to)
	if b.f.opMap == nil {
		b.f.opMap = make(map[string]string, 2)
	}
	b.f.opMap[from] = to
	b.f.allowed[from] = struct{}{}
	b.f.allowed[to] = struct{}{}
	return b
}

// OpColumn maps one operator onto a different column for this field, which is
// how a single RQL field can address different columns per operator.
func (b *FieldBuilder) OpColumn(op, column string) *FieldBuilder {
	op = strings.ToLower(op)
	if b.f.opCols == nil {
		b.f.opCols = make(map[string][]string, 2)
	}
	b.f.opCols[op] = deriveExplicitColumn(column)
	b.f.allowed[op] = struct{}{}
	return b
}

// OpType overrides the value type used for one operator on this field, for
// configurations where a single RQL field carries differently typed values per
// operator (the legacy translator stores a type per (field, operator) pair).
func (b *FieldBuilder) OpType(op string, t FieldType) *FieldBuilder {
	op = strings.ToLower(op)
	if b.f.opTypes == nil {
		b.f.opTypes = make(map[string]FieldType, 2)
	}
	b.f.opTypes[op] = t
	b.f.allowed[op] = struct{}{}
	return b
}

// Field declares another field on the same configuration and returns its builder, so a
// complete configuration can be written as a single chained expression:
//
//	cfg := rql.NewConfig().
//		Field("id", rql.TypeInt).Allow("in").
//		Field("name", rql.TypeString).Column("full_name").
//		Config()
//
// It is shorthand for b.Config().Field(name, t).
func (b *FieldBuilder) Field(name string, t FieldType) *FieldBuilder {
	return b.cfg.Field(name, t)
}

// Config returns the configuration the field belongs to, for the setup that is not
// field-specific (RegisterOp, AllowDynamicPaths, limits). It is the way back from a
// chained Field declaration.
func (b *FieldBuilder) Config() *Config { return b.cfg }

// defaultOps returns the operators enabled by default for a field type.
func defaultOps(t FieldType) map[string]struct{} {
	ops := make(map[string]struct{}, 10)
	add := func(names ...string) {
		for _, n := range names {
			ops[n] = struct{}{}
		}
	}
	switch t {
	case TypeString:
		add("eq", "ne", "gt", "ge", "lt", "le", "like", "ilike", "in", "out")
	case TypeInt, TypeFloat, TypeTime, TypeDate, TypeUUID:
		add("eq", "ne", "gt", "ge", "lt", "le", "in", "out")
	case TypeBool:
		add("eq", "ne")
	case TypeArray, TypeStringArray, TypeUUIDArray:
		add("eq", "ne", "contains", "containsall", "overlaps", "excludes", "in", "out")
	case TypeJSON:
		add("eq", "ne", "containsall")
	default:
		// Unknown types get the comparison set, matching the most permissive
		// scalar behaviour.
		add("eq", "ne", "gt", "ge", "lt", "le", "in", "out")
	}
	return ops
}

// splitPath splits a dotted or slashed field/column name into its segments.
//
// Separators are preserved as segment boundaries rather than collapsed, so
// "a//b", "a/./b", "a/../b" and "a..b" all yield an empty or dotted segment that
// validation then rejects. Silently normalising them away would make a query
// address a different JSONB path than the caller wrote, which this package never
// does.
func splitPath(name string) []string {
	if !strings.ContainsAny(name, "./") {
		return []string{name}
	}
	segs := make([]string, 0, 4)
	start := 0
	for i := 0; i < len(name); i++ {
		if name[i] != '.' && name[i] != '/' {
			continue
		}
		segs = append(segs, name[start:i])
		start = i + 1
	}
	return append(segs, name[start:])
}

// deriveColumn returns the default SQL column parts for a field name: the first
// segment is snake_cased, later segments are kept verbatim as JSONB path keys.
func deriveColumn(name string) []string {
	return deriveColumnWith(name, true)
}

// deriveExplicitColumn returns the column parts for an explicit column name, with
// no case conversion applied.
func deriveExplicitColumn(name string) []string {
	return deriveColumnWith(name, false)
}

func deriveColumnWith(name string, snake bool) []string {
	segs := splitPath(name)
	if len(segs) == 0 {
		return []string{name}
	}
	if snake {
		segs[0] = toSnakeCase(segs[0])
	}
	return segs
}

// pathRoot returns the first path segment and reports whether it is usable as a
// column name: an empty root (".x", "/x") is rejected rather than silently
// dropped.
func pathRoot(segs []string) (string, bool) {
	if len(segs) == 0 || segs[0] == "" {
		return "", false
	}
	return segs[0], true
}

// isPathSegment reports whether s may be used as a JSONB path key. This is the
// security boundary for attacker-controlled path input.
func isPathSegment(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_',
			c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// toSnakeCase converts camelCase to snake_case by inserting '_' before every
// upper-case rune. The algorithm intentionally matches the legacy library,
// including its handling of acronyms (organizationID -> organization_i_d), so
// that column names do not change when migrating.
func toSnakeCase(s string) string {
	needs := false
	for i, r := range s {
		if unicode.IsUpper(r) && i > 0 {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
