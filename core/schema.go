package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file implements contracts/core-api.md "JSON Schema validation" (FR-008) and the
// JSONSchema type from data-model.md Section 6b, together with rule V15 and research
// decision R-007: a documented, bounded subset of JSON Schema used to enforce a tool's
// InputSchema. The subset is type, properties, required, additionalProperties (boolean or
// schema), items and prefixItems, enum, const, minimum/maximum and the exclusive forms,
// minLength/maxLength, pattern, minItems/maxItems, uniqueItems, allOf/anyOf/oneOf/not, and
// local $ref resolution into $defs/definitions with cycle detection. Unknown keywords are
// ignored rather than rejected, and external $ref resolution is disabled by default.
//
// Validation is bounded (NFR-007, V15): a nesting-depth budget, a subschema-application
// budget, a pattern-evaluation budget, and a per-validation deadline checked cooperatively
// at node boundaries. The numeric budgets are the implementation's choice — the spec fixes
// only that they exist — so they are named constants here rather than pinned by a contract.
// Every failure, including an exhausted budget, a resolved-cycle, or an unsupported
// external $ref, wraps ErrValidation so callers branch on the class with errors.Is (NFR-005).

// JSONSchema is a JSON Schema document validated by the bounded subset validator
// (FR-008, R-007). It is retained verbatim: validation never rewrites the document. It
// marshals and unmarshals as the raw JSON document itself, never wrapped in a struct of the
// SDK's own making (data-model.md Section 6b).
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type JSONSchema struct {
	Raw json.RawMessage // the schema as supplied by the caller
}

// MarshalJSON emits the retained document exactly as supplied, so a schema field on the
// wire is the document inline rather than an object the SDK invented. A schema with no
// document encodes as JSON null, matching the RequestState precedent (V10). The receiver is
// not mutated.
func (s *JSONSchema) MarshalJSON() ([]byte, error) {
	if s == nil || len(s.Raw) == 0 {
		return []byte("null"), nil
	}
	return s.Raw, nil
}

// UnmarshalJSON captures the document verbatim, whatever its JSON type, so validation never
// sees a rewritten copy. The bytes are copied rather than aliased, so the schema does not
// borrow the lifetime of the buffer it was decoded from (the same rule RequestState follows
// for V10).
func (s *JSONSchema) UnmarshalJSON(data []byte) error {
	if s == nil {
		return errors.New("core: JSONSchema: UnmarshalJSON on nil *JSONSchema")
	}
	s.Raw = append(s.Raw[:0], data...)
	return nil
}

// Validation budgets (NFR-007, V15). The values are deliberately generous — they exist to
// bound worst-case work, not to constrain realistic tool schemas — and are the
// implementation's choice, not a contract figure. The pattern budget is kept below the
// subschema budget because a pattern evaluation is the more expensive operation, so a
// document that overshoots either bound is still rejected by the tighter one.
const (
	// maxSchemaDepth bounds how deeply the validator may descend into nested subschemas.
	maxSchemaDepth = 1024
	// maxSchemaSubschemas bounds how many schema applications one validation may perform.
	maxSchemaSubschemas = 20000
	// maxSchemaPatterns bounds how many regular-expression evaluations one validation may
	// perform.
	maxSchemaPatterns = 10000
)

// errSchemaLimit marks a validation that stopped because a budget was reached rather than
// because the value violated the schema. It is unexported and is only used to let the
// combinator keywords (anyOf/oneOf/not) distinguish "this branch did not match" from "the
// validator gave up", so a budget failure is never swallowed as a non-match.
var errSchemaLimit = errors.New("core: schema validation limit reached")

// schemaLimitError builds a budget failure that is matchable both as ErrValidation (the
// public class) and as errSchemaLimit (the internal "gave up" signal).
func schemaLimitError(format string, args ...any) error {
	return fmt.Errorf("%w: %w: %s", ErrValidation, errSchemaLimit, fmt.Sprintf(format, args...))
}

// ValidateSchema validates value against schema using the documented bounded subset
// (R-007). Validation has bounded nesting depth, bounded subschema count, bounded pattern
// evaluations, and a cooperative per-validation deadline; external $ref resolution is
// disabled by default. It returns a validation error (errors.Is(err, ErrValidation)) on
// failure (contracts/core-api.md "JSON Schema validation", FR-008, NFR-007, V15).
//
// A context that is already cancelled or past its deadline fails immediately; the deadline
// is re-checked at every node boundary so a caller's cancellation propagates promptly. The
// function reads and writes no package state and never mutates its arguments.
func ValidateSchema(ctx context.Context, schema, value json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: validation context: %v", ErrValidation, err)
	}
	root, err := decodeJSONValue(schema)
	if err != nil {
		return fmt.Errorf("%w: invalid schema document: %v", ErrValidation, err)
	}
	instance, err := decodeJSONValue(value)
	if err != nil {
		return fmt.Errorf("%w: invalid value document: %v", ErrValidation, err)
	}
	v := &validator{ctx: ctx, root: root}
	return v.validate(root, instance)
}

// validator carries the state of one ValidateSchema call: the root document for local $ref
// resolution, the budgets already consumed, and the stack of $refs followed at the current
// value level for cycle detection.
type validator struct {
	ctx        context.Context
	root       interface{} // the decoded schema document, for $ref resolution
	depth      int         // current nesting depth
	subschemas int         // schema applications performed so far
	patterns   int         // pattern evaluations performed so far
	refStack   []string    // $refs followed at the current value level (cycle detection)
	reCache    map[string]*regexp.Regexp
}

// decodeJSONValue decodes a single JSON document into the generic representation the
// validator walks. Numbers are kept as json.Number so integer-ness is decided by the
// document's own text (7.0 is integral, 7.5 is not) and so large integers do not lose
// precision through a float64 round trip.
func decodeJSONValue(data []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// validate applies one schema node to one value, charging the depth and subschema budgets
// and re-checking the deadline at this node boundary.
func (v *validator) validate(schema, value interface{}) error {
	v.depth++
	defer func() { v.depth-- }()
	if v.depth > maxSchemaDepth {
		return schemaLimitError("schema nesting depth exceeds budget (%d)", maxSchemaDepth)
	}
	v.subschemas++
	if v.subschemas > maxSchemaSubschemas {
		return schemaLimitError("schema subschema count exceeds budget (%d)", maxSchemaSubschemas)
	}
	if err := v.ctx.Err(); err != nil {
		return schemaLimitError("validation cancelled: %v", err)
	}

	switch s := schema.(type) {
	case bool:
		// A boolean schema is valid JSON Schema: true accepts everything, false nothing.
		if !s {
			return fmt.Errorf("%w: schema is false", ErrValidation)
		}
		return nil
	case map[string]interface{}:
		return v.validateObject(s, value)
	default:
		// A schema that is neither an object nor a boolean constrains nothing; unknown
		// schema shapes are ignored rather than rejected.
		return nil
	}
}

// validateObject applies the keywords of an object schema to value. Keywords are applied
// only when the value has the type they govern, matching JSON Schema's semantics (for
// example, minimum is ignored for a non-numeric value rather than failing it).
func (v *validator) validateObject(s map[string]interface{}, value interface{}) error {
	// A $ref replaces the schema it appears in, so resolve it and validate the target. The
	// sibling keywords are ignored, as in JSON Schema's reference semantics.
	if ref, ok := s["$ref"]; ok {
		refStr, ok := ref.(string)
		if !ok {
			return fmt.Errorf("%w: $ref must be a string", ErrValidation)
		}
		return v.validateRef(refStr, value)
	}

	if t, ok := s["type"]; ok && !v.checkType(t, value) {
		return fmt.Errorf("%w: value does not match type %v", ErrValidation, t)
	}
	if e, ok := s["enum"]; ok && !v.checkEnum(e, value) {
		return fmt.Errorf("%w: value is not one of the enum values", ErrValidation)
	}
	if c, ok := s["const"]; ok && !jsonEqual(c, value) {
		return fmt.Errorf("%w: value is not the const value", ErrValidation)
	}

	if err := v.checkNumber(s, value); err != nil {
		return err
	}
	if err := v.checkString(s, value); err != nil {
		return err
	}
	if err := v.checkObject(s, value); err != nil {
		return err
	}
	if err := v.checkArray(s, value); err != nil {
		return err
	}
	return v.checkCombinators(s, value)
}

// validateRef resolves a local $ref and validates value against its target. A $ref already
// being followed at the same value level is a cycle: it consumes no value and would recurse
// forever, so it is reported as a validation failure rather than allowed to hang.
func (v *validator) validateRef(ref string, value interface{}) error {
	for _, seen := range v.refStack {
		if seen == ref {
			return schemaLimitError("circular $ref %q", ref)
		}
	}
	target, err := v.resolveRef(ref)
	if err != nil {
		return err
	}
	v.refStack = append(v.refStack, ref)
	err = v.validate(target, value)
	v.refStack = v.refStack[:len(v.refStack)-1]
	return err
}

// resolveRef resolves a local JSON Pointer of the form #/... against the root document.
// Anything that is not a local pointer — an http(s) or relative URI — is an external $ref
// and is rejected, because external resolution is disabled by default (R-007).
func (v *validator) resolveRef(ref string) (interface{}, error) {
	if !strings.HasPrefix(ref, "#") {
		return nil, fmt.Errorf("%w: external $ref %q is not supported", ErrValidation, ref)
	}
	pointer := ref[1:]
	if pointer == "" {
		return v.root, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("%w: unsupported $ref %q", ErrValidation, ref)
	}
	current := v.root
	for _, raw := range strings.Split(pointer[1:], "/") {
		// RFC 6901 escapes: ~1 is "/" and ~0 is "~"; ~1 must be decoded first.
		segment := strings.ReplaceAll(raw, "~1", "/")
		segment = strings.ReplaceAll(segment, "~0", "~")
		switch node := current.(type) {
		case map[string]interface{}:
			next, ok := node[segment]
			if !ok {
				return nil, fmt.Errorf("%w: $ref %q does not resolve", ErrValidation, ref)
			}
			current = next
		case []interface{}:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(node) {
				return nil, fmt.Errorf("%w: $ref %q does not resolve", ErrValidation, ref)
			}
			current = node[index]
		default:
			return nil, fmt.Errorf("%w: $ref %q does not resolve", ErrValidation, ref)
		}
	}
	return current, nil
}

// validateChild applies a child schema to a value reached through a value-consuming keyword
// (an object property, an array element, or an additional property). The $ref stack is
// cleared for the child because the value changed: a recursive $ref that re-enters the same
// schema at a smaller value is legitimate, not a cycle.
func (v *validator) validateChild(schema, value interface{}) error {
	saved := v.refStack
	v.refStack = nil
	err := v.validate(schema, value)
	v.refStack = saved
	return err
}

// checkType reports whether value satisfies the type keyword, which may be a single type
// name or an array of them. An unrecognised type spec constrains nothing.
func (v *validator) checkType(t, value interface{}) bool {
	switch tv := t.(type) {
	case string:
		return matchesType(tv, value)
	case []interface{}:
		for _, item := range tv {
			if name, ok := item.(string); ok && matchesType(name, value) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// matchesType reports whether value is of the named JSON type. "integer" accepts a JSON
// number whose value is integral, so 7.0 satisfies integer while 7.5 does not.
func matchesType(name string, value interface{}) bool {
	switch name {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		n, ok := value.(json.Number)
		return ok && isIntegral(n)
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "object":
		_, ok := value.(map[string]interface{})
		return ok
	case "array":
		_, ok := value.([]interface{})
		return ok
	default:
		return true
	}
}

// isIntegral reports whether a JSON number's value has no fractional part. The text form is
// parsed so 1e2 and 7.0 count as integral while 7.5 does not.
func isIntegral(n json.Number) bool {
	f, err := n.Float64()
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return false
	}
	return f == math.Trunc(f)
}

// checkEnum reports whether value equals one of the enum values.
func (v *validator) checkEnum(e, value interface{}) bool {
	values, ok := e.([]interface{})
	if !ok {
		return true
	}
	for _, candidate := range values {
		if jsonEqual(candidate, value) {
			return true
		}
	}
	return false
}

// checkNumber applies the numeric keywords, but only when the value is a number; JSON Schema
// ignores them for every other type.
func (v *validator) checkNumber(s map[string]interface{}, value interface{}) error {
	n, ok := value.(json.Number)
	if !ok {
		return nil
	}
	f, err := n.Float64()
	if err != nil {
		return nil
	}
	if limit, ok := numberValue(s["minimum"]); ok && f < limit {
		return fmt.Errorf("%w: %v is below minimum %v", ErrValidation, f, limit)
	}
	if limit, ok := numberValue(s["maximum"]); ok && f > limit {
		return fmt.Errorf("%w: %v is above maximum %v", ErrValidation, f, limit)
	}
	if limit, ok := numberValue(s["exclusiveMinimum"]); ok && f <= limit {
		return fmt.Errorf("%w: %v is not above exclusiveMinimum %v", ErrValidation, f, limit)
	}
	if limit, ok := numberValue(s["exclusiveMaximum"]); ok && f >= limit {
		return fmt.Errorf("%w: %v is not below exclusiveMaximum %v", ErrValidation, f, limit)
	}
	return nil
}

// numberValue reads a numeric keyword's value as a float64. A non-numeric or absent keyword
// reports false, so the keyword is ignored.
func numberValue(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// checkString applies the string keywords, but only when the value is a string. Lengths are
// counted in Unicode code points, as JSON Schema specifies. The pattern budget is charged
// here, so a document that forces many evaluations is rejected before it can run long.
func (v *validator) checkString(s map[string]interface{}, value interface{}) error {
	str, ok := value.(string)
	if !ok {
		return nil
	}
	length := utf8.RuneCountInString(str)
	if limit, ok := intValue(s["minLength"]); ok && length < limit {
		return fmt.Errorf("%w: string length %d is below minLength %d", ErrValidation, length, limit)
	}
	if limit, ok := intValue(s["maxLength"]); ok && length > limit {
		return fmt.Errorf("%w: string length %d is above maxLength %d", ErrValidation, length, limit)
	}
	pattern, ok := s["pattern"].(string)
	if !ok {
		return nil
	}
	v.patterns++
	if v.patterns > maxSchemaPatterns {
		return schemaLimitError("pattern evaluation count exceeds budget (%d)", maxSchemaPatterns)
	}
	re, err := v.compilePattern(pattern)
	if err != nil {
		return fmt.Errorf("%w: invalid pattern %q: %v", ErrValidation, pattern, err)
	}
	if !re.MatchString(str) {
		return fmt.Errorf("%w: string does not match pattern %q", ErrValidation, pattern)
	}
	return nil
}

// compilePattern compiles and caches a pattern so repeated evaluations against the same
// schema do not recompile it.
func (v *validator) compilePattern(pattern string) (*regexp.Regexp, error) {
	if re, ok := v.reCache[pattern]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if v.reCache == nil {
		v.reCache = make(map[string]*regexp.Regexp)
	}
	v.reCache[pattern] = re
	return re, nil
}

// checkObject applies the object keywords, but only when the value is an object. Nested
// property values are validated through validateChild so a recursive $ref at a smaller value
// is not mistaken for a cycle.
func (v *validator) checkObject(s map[string]interface{}, value interface{}) error {
	obj, ok := value.(map[string]interface{})
	if !ok {
		return nil
	}
	if req, ok := s["required"].([]interface{}); ok {
		for _, item := range req {
			name, ok := item.(string)
			if !ok {
				continue
			}
			if _, present := obj[name]; !present {
				return fmt.Errorf("%w: required property %q is missing", ErrValidation, name)
			}
		}
	}

	properties, _ := s["properties"].(map[string]interface{})
	for name, propSchema := range properties {
		if childValue, present := obj[name]; present {
			if err := v.validateChild(propSchema, childValue); err != nil {
				return err
			}
		}
	}

	switch additional := s["additionalProperties"].(type) {
	case bool:
		if additional {
			return nil
		}
		for name := range obj {
			if _, declared := properties[name]; !declared {
				return fmt.Errorf("%w: additional property %q is not allowed", ErrValidation, name)
			}
		}
	case map[string]interface{}:
		for name, childValue := range obj {
			if _, declared := properties[name]; declared {
				continue
			}
			if err := v.validateChild(additional, childValue); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkArray applies the array keywords, but only when the value is an array. prefixItems is
// applied positionally; items applies to every element.
func (v *validator) checkArray(s map[string]interface{}, value interface{}) error {
	arr, ok := value.([]interface{})
	if !ok {
		return nil
	}
	if limit, ok := intValue(s["minItems"]); ok && len(arr) < limit {
		return fmt.Errorf("%w: array length %d is below minItems %d", ErrValidation, len(arr), limit)
	}
	if limit, ok := intValue(s["maxItems"]); ok && len(arr) > limit {
		return fmt.Errorf("%w: array length %d is above maxItems %d", ErrValidation, len(arr), limit)
	}
	if unique, ok := s["uniqueItems"].(bool); ok && unique {
		for i := 0; i < len(arr); i++ {
			for j := i + 1; j < len(arr); j++ {
				if jsonEqual(arr[i], arr[j]) {
					return fmt.Errorf("%w: array items are not unique", ErrValidation)
				}
			}
		}
	}
	if prefixes, ok := s["prefixItems"].([]interface{}); ok {
		for i, prefixSchema := range prefixes {
			if i >= len(arr) {
				break
			}
			if err := v.validateChild(prefixSchema, arr[i]); err != nil {
				return err
			}
		}
	}
	if items, ok := s["items"]; ok {
		switch itemsSchema := items.(type) {
		case map[string]interface{}, bool:
			for _, element := range arr {
				if err := v.validateChild(itemsSchema, element); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkCombinators applies allOf/anyOf/oneOf/not. These keywords apply their subschemas to
// the same value, so they keep the $ref stack (unlike value-consuming keywords) — that is
// what makes an allOf whose only branch is a self-$ref a detectable cycle. A budget failure
// from a branch is propagated rather than treated as a non-match.
func (v *validator) checkCombinators(s map[string]interface{}, value interface{}) error {
	if branches, ok := s["allOf"].([]interface{}); ok {
		for _, branch := range branches {
			if err := v.validate(branch, value); err != nil {
				return err
			}
		}
	}
	if branches, ok := s["anyOf"].([]interface{}); ok {
		matched := false
		for _, branch := range branches {
			err := v.validate(branch, value)
			if err == nil {
				matched = true
				break
			}
			if errors.Is(err, errSchemaLimit) {
				return err
			}
		}
		if !matched {
			return fmt.Errorf("%w: value matches no anyOf branch", ErrValidation)
		}
	}
	if branches, ok := s["oneOf"].([]interface{}); ok {
		matches := 0
		for _, branch := range branches {
			err := v.validate(branch, value)
			if err == nil {
				matches++
				continue
			}
			if errors.Is(err, errSchemaLimit) {
				return err
			}
		}
		if matches != 1 {
			return fmt.Errorf("%w: value matches %d oneOf branches, want exactly 1", ErrValidation, matches)
		}
	}
	if negated, ok := s["not"]; ok {
		err := v.validate(negated, value)
		switch {
		case err == nil:
			return fmt.Errorf("%w: value matches the not schema", ErrValidation)
		case errors.Is(err, errSchemaLimit):
			return err
		}
	}
	return nil
}

// intValue reads a non-negative integer keyword's value. A non-numeric or absent keyword
// reports false, so the keyword is ignored.
func intValue(v interface{}) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 0 {
			return 0, false
		}
		return int(i), true
	case float64:
		if n < 0 || n != math.Trunc(n) {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}

// jsonEqual reports whether two decoded JSON values are equal. Numbers are compared by value
// so 42 and 42.0 are equal; everything else uses structural equality. Both sides are decoded
// with json.Number, so the numeric path is the common one.
func jsonEqual(a, b interface{}) bool {
	an, aok := a.(json.Number)
	bn, bok := b.(json.Number)
	if aok && bok {
		af, aerr := an.Float64()
		bf, berr := bn.Float64()
		if aerr == nil && berr == nil {
			return af == bf
		}
		return an.String() == bn.String()
	}
	return reflect.DeepEqual(a, b)
}
