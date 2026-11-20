package core

import (
	"encoding/json"
	"testing"
)

// These tests pin the result envelope contract in wire-protocol.md Section 4,
// data-model.md Section 7, and rule V9 (FR-006): `resultType` discriminates a result
// envelope with exactly two values, an absent `resultType` decodes to "complete" and is
// never an error, and "input_required" without any requested inputs is rejected.
//
// The symbols exercised here — ResultType, ResultComplete, ResultInputRequired, and
// ResultEnvelope — belong to result.go (T023). This file is written first and is expected
// to fail to compile until that task lands, so the red is attributable to result.go.
// Decoding goes through encoding/json rather than a named function so the contract adds no
// exported entry point: the absent-means-complete rule attaches to ResultEnvelope's own
// UnmarshalJSON, which is the idiomatic Go decode hook and the path a client takes after
// core.Decode hands it a Response's raw `result`.
//
// ResultEnvelope carries []InputRequest (data-model.md Section 7), a type owned by
// mrtr.go (T025), so this file — and result.go — cannot compile until T025 lands as well.
// The tests name no symbol from another core file: they decode `inputRequests` from JSON
// and count the decoded slice, so the only identifiers this file itself names are
// result.go's four. The InputRequests field it reads pulls in mrtr.go's InputRequest
// transitively, exactly as RequestMeta's ClientCapabilities field pulls in capabilities.go
// for meta_test.go.

// The two `resultType` values are spelled out literally so that a change to the package
// constants cannot silently relax these assertions (wire-protocol.md Section 4).
const (
	wantResultComplete      = "complete"
	wantResultInputRequired = "input_required"
)

// ResultComplete must be a ResultType, not merely a string: the discriminator is typed so
// a server building a result and a client reading one cannot disagree about the spelling.
var _ ResultType = ResultComplete

// TestResultTypeConstantsMatchWireProtocol pins the only two discriminator values from
// wire-protocol.md Section 4: "complete" and "input_required". A third outcome would be a
// protocol change, so the pair must stay distinct and spelled exactly as on the wire.
func TestResultTypeConstantsMatchWireProtocol(t *testing.T) {
	if got := string(ResultComplete); got != wantResultComplete {
		t.Errorf("ResultComplete = %q, want %q", got, wantResultComplete)
	}
	if got := string(ResultInputRequired); got != wantResultInputRequired {
		t.Errorf("ResultInputRequired = %q, want %q", got, wantResultInputRequired)
	}
	if ResultComplete == ResultInputRequired {
		t.Error("ResultComplete and ResultInputRequired compare equal; the two outcomes must be distinguishable")
	}
}

// TestAbsentResultTypeDecodesAsComplete pins the first half of V9 and FR-006's edge case: a
// result that carries no `resultType` decodes to "complete" and is never an error. This is
// the legacy-interoperability path — a peer that omits the discriminator must not be
// treated as malformed, and an explicit "complete" must behave identically.
func TestAbsentResultTypeDecodesAsComplete(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty result object", `{}`},
		{"legacy payload only", `{"content":[{"type":"text","text":"hi"}]}`},
		{"explicit complete", `{"resultType":"complete"}`},
		{"explicit complete with payload", `{"resultType":"complete","content":[{"type":"text","text":"hi"}]}`},
		// V9's non-empty-inputRequests clause is scoped to "input_required"; an empty list
		// on a complete result is not a violation and must not be rejected.
		{"complete with empty inputRequests", `{"resultType":"complete","inputRequests":[]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := decodeResult(tt.body)
			if err != nil {
				t.Fatalf("decode %s returned an error; an absent resultType must never be an error (V9): %v", tt.body, err)
			}
			if env.ResultType != ResultComplete {
				t.Errorf("decoded ResultType = %q, want %q (V9)", env.ResultType, ResultComplete)
			}
		})
	}
}

// TestInputRequiredDecodesWithInputRequests pins the positive half of V9's second clause:
// "input_required" is a valid envelope when it carries at least one requested input. The
// example mirrors the wire-protocol.md Section 5.2 round trip.
func TestInputRequiredDecodesWithInputRequests(t *testing.T) {
	body := `{"resultType":"input_required","inputRequests":[{"id":"confirm-1","method":"elicitation/create","params":{"message":"Send this email?"}}]}`

	env, err := decodeResult(body)
	if err != nil {
		t.Fatalf("decode a well-formed input_required result: %v", err)
	}
	if env.ResultType != ResultInputRequired {
		t.Errorf("decoded ResultType = %q, want %q", env.ResultType, ResultInputRequired)
	}
	if len(env.InputRequests) != 1 {
		t.Errorf("decoded %d inputRequests, want 1", len(env.InputRequests))
	}
}

// TestInputRequiredWithoutInputRequestsIsRejected pins the second half of V9: an
// "input_required" result that requests nothing is malformed and is rejected rather than
// decoded. Absent, empty, and null all mean "no requested inputs". V9 assigns no error
// class to this rejection, so the assertion is deliberately class-agnostic — the envelope
// must simply not be accepted, and T023 is free to choose a wire-level or validation class.
func TestInputRequiredWithoutInputRequestsIsRejected(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"absent inputRequests", `{"resultType":"input_required"}`},
		{"empty inputRequests", `{"resultType":"input_required","inputRequests":[]}`},
		{"null inputRequests", `{"resultType":"input_required","inputRequests":null}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeResult(tt.body); err == nil {
				t.Fatalf("decode %s = nil error, want a rejection: input_required requires at least one inputRequests entry (V9)", tt.body)
			}
		})
	}
}

// decodeResult decodes one result object into a ResultEnvelope. It returns the zero
// envelope on failure so a rejection case cannot accidentally assert on partial state.
func decodeResult(body string) (ResultEnvelope, error) {
	var env ResultEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return ResultEnvelope{}, err
	}
	return env, nil
}
