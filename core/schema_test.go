package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// These tests pin the bounded JSON Schema validator described by FR-008, NFR-007, R-007
// and rule V15, together with the verbatim JSONSchema document type from
// contracts/core-api.md ("JSON Schema validation") and data-model.md Section 6.
//
// The symbols exercised here — ValidateSchema, JSONSchema and its JSON methods — belong to
// schema.go (T029). This file is written first and is expected to fail to compile until that
// task lands, so the red is attributable to schema.go alone. It names no symbol owned by
// another core file: every schema and value is a JSON literal, so the only identifiers it
// spells are schema.go's plus the already-landed ErrValidation sentinel (errors.go).
//
// What R-007 fixes: the supported keyword subset is type, properties, required,
// additionalProperties, items, prefixItems, enum, const, minimum/maximum and the exclusive
// forms, minLength/maxLength, pattern, minItems/maxItems, uniqueItems, allOf/anyOf/oneOf/not,
// and local $ref resolution into $defs/definitions. Unknown keywords are ignored rather than
// rejected, and external $ref resolution is disabled by default.
//
// What V15/NFR-007 fix: validation is bounded by a nesting-depth budget, a subschema-count
// budget, a pattern-evaluation budget, and a per-validation deadline checked cooperatively.
// The budgets have no documented numeric value — they are the implementation's to choose — so
// these tests do not pin an exact figure. Instead each budget test crafts a document that
// overshoots any plausible budget by a wide margin and asserts rejection, which is the weakest
// assertion that still proves the budget exists. Each such test therefore enforces an upper
// bound on its budget (depth <= 4096, subschema count <= 100000, pattern evaluations <= 100000);
// a smaller, more realistic budget passes unchanged. The same reasoning makes the budget tests
// independent of the numeric choice T029 makes.
//
// Every failure is asserted to satisfy errors.Is(err, ErrValidation): core-api.md states that
// ValidateSchema "returns a validation error (errors.Is(err, ErrValidation)) on failure", so
// the class is the same for a keyword mismatch, an unresolved or external $ref, a detected
// cycle, and each exhausted budget.

// ValidateSchema's signature is pinned by assignment so a drifted parameter or result type
// fails the build here rather than in server, which is the caller that enforces a tool's
// InputSchema (FR-008, NFR-004: context.Context first on a blocking call).
var _ func(context.Context, json.RawMessage, json.RawMessage) error = ValidateSchema

// JSONSchema keeps the caller's document verbatim, so it is a JSON marshaler/unmarshaler
// whose methods live on the pointer receiver (data-model.md Section 6 declares
// `func (s *JSONSchema) MarshalJSON` / `UnmarshalJSON`).
var (
	_ json.Marshaler   = (*JSONSchema)(nil)
	_ json.Unmarshaler = (*JSONSchema)(nil)
	_                  = JSONSchema{Raw: json.RawMessage(nil)}
)

// validateSchema runs the validator with a live background context.
func validateSchema(t *testing.T, schema, value string) error {
	t.Helper()
	return ValidateSchema(context.Background(), json.RawMessage(schema), json.RawMessage(value))
}

// assertSchemaAccepts fails the test unless ValidateSchema accepts value against schema.
func assertSchemaAccepts(t *testing.T, schema, value string) {
	t.Helper()
	if err := validateSchema(t, schema, value); err != nil {
		t.Errorf("ValidateSchema(%s, %s) = %v, want nil", schema, value, err)
	}
}

// assertSchemaRejects fails the test unless ValidateSchema rejects value against schema with
// an error in the ErrValidation class.
func assertSchemaRejects(t *testing.T, schema, value string) {
	t.Helper()
	err := validateSchema(t, schema, value)
	if err == nil {
		t.Errorf("ValidateSchema(%s, %s) = nil, want a validation error", schema, value)
		return
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("ValidateSchema(%s, %s) = %v, want errors.Is(err, ErrValidation)", schema, value, err)
	}
}

// TestSchemaKeywordSubsetValidates pins the R-007 keyword subset: for each supported keyword a
// value that satisfies it is accepted and one that violates it is rejected. A keyword that
// silently stopped being enforced, or that started rejecting valid input, fails here.
func TestSchemaKeywordSubsetValidates(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		value  string
		accept bool
	}{
		{"type string accepts a string", `{"type":"string"}`, `"x"`, true},
		{"type string rejects a number", `{"type":"string"}`, `1`, false},
		{"type integer accepts an integer", `{"type":"integer"}`, `7`, true},
		{"type integer accepts an integral float", `{"type":"integer"}`, `7.0`, true},
		{"type integer rejects a fraction", `{"type":"integer"}`, `7.5`, false},
		{"type integer rejects a string", `{"type":"integer"}`, `"7"`, false},
		{"type number accepts a fraction", `{"type":"number"}`, `1.5`, true},
		{"type number rejects a string", `{"type":"number"}`, `"1.5"`, false},
		{"type boolean accepts a boolean", `{"type":"boolean"}`, `true`, true},
		{"type boolean rejects a number", `{"type":"boolean"}`, `1`, false},
		{"type null accepts null", `{"type":"null"}`, `null`, true},
		{"type null rejects a string", `{"type":"null"}`, `"null"`, false},
		{"type object accepts an object", `{"type":"object"}`, `{}`, true},
		{"type object rejects an array", `{"type":"object"}`, `[]`, false},
		{"type array accepts an array", `{"type":"array"}`, `[]`, true},
		{"type array rejects an object", `{"type":"array"}`, `{}`, false},

		{"properties accepts a matching member", `{"type":"object","properties":{"a":{"type":"string"}}}`, `{"a":"x"}`, true},
		{"properties rejects a mismatched member", `{"type":"object","properties":{"a":{"type":"string"}}}`, `{"a":1}`, false},
		{"absent additionalProperties permits extras", `{"type":"object","properties":{"a":{"type":"string"}}}`, `{"a":"x","b":1}`, true},
		{"required accepts when present", `{"type":"object","required":["a"]}`, `{"a":1}`, true},
		{"required rejects when absent", `{"type":"object","required":["a"]}`, `{}`, false},
		{"additionalProperties false accepts the declared member", `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`, `{"a":"x"}`, true},
		{"additionalProperties false rejects an extra member", `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`, `{"a":"x","b":1}`, false},
		{"additionalProperties schema accepts a conforming extra", `{"type":"object","additionalProperties":{"type":"string"}}`, `{"b":"y"}`, true},
		{"additionalProperties schema rejects a non-conforming extra", `{"type":"object","additionalProperties":{"type":"string"}}`, `{"b":1}`, false},

		{"items accepts conforming elements", `{"type":"array","items":{"type":"integer"}}`, `[1,2,3]`, true},
		{"items rejects a non-conforming element", `{"type":"array","items":{"type":"integer"}}`, `[1,"x"]`, false},
		{"prefixItems accepts a conforming prefix", `{"type":"array","prefixItems":[{"type":"string"},{"type":"integer"}]}`, `["a",1]`, true},
		{"prefixItems rejects a swapped prefix", `{"type":"array","prefixItems":[{"type":"string"},{"type":"integer"}]}`, `[1,"a"]`, false},

		{"enum accepts a listed value", `{"enum":["a","b"]}`, `"a"`, true},
		{"enum rejects an unlisted value", `{"enum":["a","b"]}`, `"c"`, false},
		{"const accepts the constant", `{"const":42}`, `42`, true},
		{"const rejects a different value", `{"const":42}`, `41`, false},

		{"minimum accepts the boundary", `{"minimum":1}`, `1`, true},
		{"minimum rejects below it", `{"minimum":1}`, `0`, false},
		{"maximum accepts the boundary", `{"maximum":5}`, `5`, true},
		{"maximum rejects above it", `{"maximum":5}`, `6`, false},
		{"exclusiveMinimum rejects the boundary", `{"exclusiveMinimum":1}`, `1`, false},
		{"exclusiveMinimum accepts above it", `{"exclusiveMinimum":1}`, `2`, true},
		{"exclusiveMaximum rejects the boundary", `{"exclusiveMaximum":5}`, `5`, false},
		{"exclusiveMaximum accepts below it", `{"exclusiveMaximum":5}`, `4`, true},

		{"minLength accepts at the boundary", `{"minLength":2}`, `"ab"`, true},
		{"minLength rejects below it", `{"minLength":2}`, `"a"`, false},
		{"maxLength accepts at the boundary", `{"maxLength":2}`, `"ab"`, true},
		{"maxLength rejects above it", `{"maxLength":2}`, `"abc"`, false},
		{"pattern accepts a match", `{"pattern":"^a+$"}`, `"aaa"`, true},
		{"pattern rejects a non-match", `{"pattern":"^a+$"}`, `"b"`, false},

		{"minItems accepts at the boundary", `{"minItems":1}`, `[1]`, true},
		{"minItems rejects below it", `{"minItems":1}`, `[]`, false},
		{"maxItems accepts at the boundary", `{"maxItems":1}`, `[1]`, true},
		{"maxItems rejects above it", `{"maxItems":1}`, `[1,2]`, false},
		{"uniqueItems accepts distinct elements", `{"uniqueItems":true}`, `[1,2,3]`, true},
		{"uniqueItems rejects a duplicate", `{"uniqueItems":true}`, `[1,2,2]`, false},

		{"allOf accepts when every branch holds", `{"allOf":[{"type":"integer"},{"minimum":1}]}`, `2`, true},
		{"allOf rejects when a branch fails", `{"allOf":[{"type":"integer"},{"minimum":1}]}`, `0`, false},
		{"anyOf accepts when a branch holds", `{"anyOf":[{"type":"string"},{"type":"integer"}]}`, `"x"`, true},
		{"anyOf rejects when no branch holds", `{"anyOf":[{"type":"string"},{"type":"integer"}]}`, `true`, false},
		{"oneOf accepts exactly one match", `{"oneOf":[{"type":"string"},{"type":"integer"}]}`, `"x"`, true},
		{"oneOf rejects when none match", `{"oneOf":[{"type":"string"},{"type":"integer"}]}`, `true`, false},
		{"oneOf rejects when several match", `{"oneOf":[{"type":"integer"},{"type":"number"}]}`, `1`, false},
		{"not accepts a non-match", `{"not":{"type":"string"}}`, `1`, true},
		{"not rejects a match", `{"not":{"type":"string"}}`, `"x"`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.accept {
				assertSchemaAccepts(t, tt.schema, tt.value)
				return
			}
			assertSchemaRejects(t, tt.schema, tt.value)
		})
	}
}

// TestSchemaUnknownKeywordsAreIgnored pins the R-007 rule that unknown keywords are ignored
// rather than rejected. A vendor extension, a JSON Schema annotation, or a keyword the subset
// does not implement must not turn a valid value into a failure — including when the unknown
// keyword is the only member of the schema, which then accepts anything.
func TestSchemaUnknownKeywordsAreIgnored(t *testing.T) {
	assertSchemaAccepts(t, `{"type":"string","x-vendor":"ignored","$comment":"ignored"}`, `"x"`)
	assertSchemaAccepts(t, `{"x-vendor":1}`, `{"anything":[1,2,3]}`)
	assertSchemaAccepts(t, `{"type":"object","properties":{"a":{"type":"string","format":"email","x-foo":1}}}`, `{"a":"not-an-email"}`)
}

// TestSchemaLocalRefResolvesIntoDefs pins the R-007 rule that a local $ref into $defs resolves
// to the referenced subschema: the value is validated against the target, so a value that
// violates the target is rejected.
func TestSchemaLocalRefResolvesIntoDefs(t *testing.T) {
	const schema = `{"$defs":{"positive":{"type":"integer","minimum":1}},"$ref":"#/$defs/positive"}`
	assertSchemaAccepts(t, schema, `5`)
	assertSchemaRejects(t, schema, `0`)
	assertSchemaRejects(t, schema, `"x"`)
}

// TestSchemaDefinitionsKeywordResolves pins that the older `definitions` container is resolved
// too, since R-007 names both `$defs` and `definitions`.
func TestSchemaDefinitionsKeywordResolves(t *testing.T) {
	const schema = `{"definitions":{"word":{"type":"string"}},"$ref":"#/definitions/word"}`
	assertSchemaAccepts(t, schema, `"hi"`)
	assertSchemaRejects(t, schema, `1`)
}

// TestSchemaRefCycleIsDetected pins R-007's cycle detection: a $ref chain that resolves only to
// itself consumes no value and would recurse forever, so it must be detected and reported as a
// validation failure rather than hanging or exhausting the stack.
func TestSchemaRefCycleIsDetected(t *testing.T) {
	assertSchemaRejects(t, `{"$defs":{"a":{"$ref":"#/$defs/a"}},"$ref":"#/$defs/a"}`, `1`)
	assertSchemaRejects(t, `{"$defs":{"a":{"allOf":[{"$ref":"#/$defs/a"}]}},"$ref":"#/$defs/a"}`, `1`)
}

// TestSchemaRecursiveRefIsNotACycle pins the other side of cycle detection: a recursive schema
// is legitimate. A node that refers to itself through a value-consuming step (`properties.next`)
// is not a cycle, so it is accepted, and the recursion is bounded by the finite instance rather
// than by rejecting the schema outright.
func TestSchemaRecursiveRefIsNotACycle(t *testing.T) {
	const schema = `{"$defs":{"node":{"type":"object","properties":{"value":{"type":"integer"},"next":{"$ref":"#/$defs/node"}},"required":["value"]}},"$ref":"#/$defs/node"}`
	assertSchemaAccepts(t, schema, `{"value":1}`)
	assertSchemaAccepts(t, schema, `{"value":1,"next":{"value":2}}`)
	assertSchemaRejects(t, schema, `{"value":1,"next":{"value":"x"}}`)
}

// TestSchemaExternalRefIsDisabledByDefault pins the R-007 default: a $ref that is not a local
// JSON Pointer into this document — an http(s) or relative URI — is rejected instead of being
// fetched. A local pointer that does not resolve is rejected too, rather than silently ignored.
func TestSchemaExternalRefIsDisabledByDefault(t *testing.T) {
	assertSchemaRejects(t, `{"$ref":"https://example.com/schema.json"}`, `1`)
	assertSchemaRejects(t, `{"$ref":"other.json#/$defs/x"}`, `1`)
	assertSchemaRejects(t, `{"$ref":"#/$defs/missing"}`, `1`)
}

// TestSchemaDepthBudgetIsEnforced pins the nesting-depth budget (NFR-007, V15). A shallow
// schema is accepted; a schema nested far past any plausible budget is rejected. The accepted
// case is deliberately shallow, so this asserts only that the depth budget is finite and at
// most 4096 — the weakest assertion that still proves the budget exists.
func TestSchemaDepthBudgetIsEnforced(t *testing.T) {
	assertSchemaAccepts(t, nestedAllOfSchema(3), `1`)
	assertSchemaRejects(t, nestedAllOfSchema(4096), `1`)
}

// TestSchemaSubschemaBudgetIsEnforced pins the subschema-count budget (NFR-007, V15). The wide
// schema is flat (depth 2) but carries 100000 subschema branches, so only the subschema budget
// can reject it. A small allOf is accepted.
func TestSchemaSubschemaBudgetIsEnforced(t *testing.T) {
	assertSchemaAccepts(t, wideAllOfSchema(4), `1`)
	assertSchemaRejects(t, wideAllOfSchema(100_000), `1`)
}

// TestSchemaPatternBudgetIsEnforced pins the pattern-evaluation budget (NFR-007, V15). The
// schema has two subschema nodes but its `items` subschema applies `pattern` once per array
// element, so a 100000-element array forces 100000 pattern evaluations with almost no
// subschemas or depth — isolating this budget. A short array is accepted.
func TestSchemaPatternBudgetIsEnforced(t *testing.T) {
	const schema = `{"type":"array","items":{"type":"string","pattern":"x"}}`
	assertSchemaAccepts(t, schema, arrayValueJSON(4))
	assertSchemaRejects(t, schema, arrayValueJSON(100_000))
}

// TestSchemaDeadlineBudgetIsEnforced pins the per-validation deadline (NFR-007, V15): a
// context that is already cancelled or whose deadline has passed makes validation fail with an
// ErrValidation-class error instead of proceeding. The document is otherwise valid, so the
// context is the only reason for failure.
func TestSchemaDeadlineBudgetIsEnforced(t *testing.T) {
	const (
		schema = `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`
		value  = `{"a":"x"}`
	)

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assertSchemaRejectsContext(t, ctx, schema, value)
	})

	t.Run("expired deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		assertSchemaRejectsContext(t, ctx, schema, value)
	})
}

// assertSchemaRejectsContext is assertSchemaRejects with a caller-supplied context.
func assertSchemaRejectsContext(t *testing.T, ctx context.Context, schema, value string) {
	t.Helper()
	err := ValidateSchema(ctx, json.RawMessage(schema), json.RawMessage(value))
	if err == nil {
		t.Errorf("ValidateSchema with an expired context = nil, want a validation error")
		return
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("ValidateSchema with an expired context = %v, want errors.Is(err, ErrValidation)", err)
	}
}

// TestJSONSchemaRetainsDocumentVerbatim pins the data-model.md Section 6 contract for
// JSONSchema: it holds the caller's document unchanged and marshals and unmarshals as that
// document itself, never wrapped in a struct of the SDK's own making. The source buffer is
// mutated after decoding to prove the retained bytes are a copy, not an alias.
func TestJSONSchemaRetainsDocumentVerbatim(t *testing.T) {
	const doc = `{"type":"object","properties":{"message":{"type":"string"}},"x-note":"kept"}`

	source := []byte(doc)
	var s JSONSchema
	if err := json.Unmarshal(source, &s); err != nil {
		t.Fatalf("decode %s into JSONSchema: %v", doc, err)
	}
	if got := string(s.Raw); got != doc {
		t.Errorf("Raw = %s, want the document verbatim %s", got, doc)
	}

	source[0] = 'X' // a retained slice would now read back corrupted
	if got := string(s.Raw); got != doc {
		t.Errorf("Raw = %s after mutating the source buffer, want the retained copy %s", got, doc)
	}

	out, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal JSONSchema: %v", err)
	}
	if string(out) != doc {
		t.Errorf("Marshal = %s, want the document verbatim %s", out, doc)
	}
}

// TestJSONSchemaEmbedsAsTheRawDocument pins that a JSONSchema field marshals as the schema
// document inline — the shape a tool's inputSchema takes on the wire — and that an empty
// document encodes as JSON null, matching the RequestState precedent (V10).
func TestJSONSchemaEmbedsAsTheRawDocument(t *testing.T) {
	type wrapper struct {
		InputSchema *JSONSchema `json:"inputSchema"`
	}

	out, err := json.Marshal(wrapper{InputSchema: &JSONSchema{Raw: json.RawMessage(`{"type":"string"}`)}})
	if err != nil {
		t.Fatalf("marshal wrapper: %v", err)
	}
	const want = `{"inputSchema":{"type":"string"}}`
	if string(out) != want {
		t.Errorf("wrapper = %s, want %s", out, want)
	}

	empty, err := json.Marshal(&JSONSchema{})
	if err != nil {
		t.Fatalf("marshal empty JSONSchema: %v", err)
	}
	if string(empty) != "null" {
		t.Errorf("empty JSONSchema = %s, want null", empty)
	}
}

// nestedAllOfSchema returns a schema nested levels deep through allOf, so the validator must
// recurse levels times to reach the innermost `{"type":"integer"}`.
func nestedAllOfSchema(levels int) string {
	var b strings.Builder
	for i := 0; i < levels; i++ {
		b.WriteString(`{"allOf":[`)
	}
	b.WriteString(`{"type":"integer"}`)
	for i := 0; i < levels; i++ {
		b.WriteString(`]}`)
	}
	return b.String()
}

// wideAllOfSchema returns a flat schema with branches allOf branches, each a single subschema.
func wideAllOfSchema(branches int) string {
	var b strings.Builder
	b.WriteString(`{"allOf":[`)
	for i := 0; i < branches; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"integer"}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// arrayValueJSON returns a JSON array of items copies of the string "x", the value the pattern
// budget test validates.
func arrayValueJSON(items int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < items; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"x"`)
	}
	b.WriteByte(']')
	return b.String()
}
