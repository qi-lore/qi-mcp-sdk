package core

import (
	"encoding/json"
	"slices"
	"testing"
)

// These tests pin the MRTR trio described in data-model.md Section 8 and wire-protocol.md
// Section 5, and rule V10 (FR-007): an inputResponse answers an inputRequest by matching
// its `id`, and a requestState is opaque — it round-trips byte-for-byte and the SDK never
// interprets or mutates it.
//
// The symbols exercised here — InputRequest, InputResponse, RequestState, and the
// correlation entry point ValidateInputResponses — belong to mrtr.go (T025). This file is
// written first and is expected to fail to compile until that task lands, so the red is
// attributable to mrtr.go. It names no symbol owned by another core file: every wire shape
// is decoded from a JSON literal, so the only identifiers it spells are mrtr.go's.
//
// InputRequest and InputResponse follow contracts/core-api.md ("Result envelope and MRTR
// types") and the wire-protocol.md Section 5 examples — `id`/`method`/`params` and
// `id`/`result` — which are newer than data-model.md Section 8's sketch (`kind`/`prompt`
// and `value`/`error`). As with the capability types (T020/T021 findings), the contract is
// authoritative where the two disagree.

// ValidateInputResponses is the mrtr.go validation entry point this file defines for T025:
// it reports nil when every response answers one of requests (matched by id) and an error
// otherwise (V10, FR-007).
var _ func([]InputRequest, []InputResponse) error = ValidateInputResponses

// TestInputRequestWireShapeMatchesProtocol pins the inputRequests entry shape from
// wire-protocol.md Section 5.2: `id`, `method`, and `params`, and no other member. A
// request that drifts from this shape cannot be matched to the response that answers it.
func TestInputRequestWireShapeMatchesProtocol(t *testing.T) {
	const entry = `{"id":"confirm-1","method":"elicitation/create","params":{"message":"Send this email?"}}`

	var req InputRequest
	if err := json.Unmarshal([]byte(entry), &req); err != nil {
		t.Fatalf("decode inputRequests entry %s: %v", entry, err)
	}
	if req.ID != "confirm-1" {
		t.Errorf("ID = %q, want %q", req.ID, "confirm-1")
	}
	if req.Method != "elicitation/create" {
		t.Errorf("Method = %q, want %q", req.Method, "elicitation/create")
	}
	const wantParams = `{"message":"Send this email?"}`
	if got := string(req.Params); got != wantParams {
		t.Errorf("Params = %s, want %s", got, wantParams)
	}
	if got, want := jsonMemberNames(t, req), []string{"id", "method", "params"}; !slices.Equal(got, want) {
		t.Errorf("InputRequest encodes members %v, want %v", got, want)
	}
}

// TestInputResponseWireShapeMatchesProtocol pins the inputResponses entry shape from
// wire-protocol.md Section 5.3: `id` and `result`, and no other member. The `id` is the
// member V10 matches against an inputRequest.
func TestInputResponseWireShapeMatchesProtocol(t *testing.T) {
	const entry = `{"id":"confirm-1","result":{"action":"accept","content":{"allow":true}}}`

	var resp InputResponse
	if err := json.Unmarshal([]byte(entry), &resp); err != nil {
		t.Fatalf("decode inputResponses entry %s: %v", entry, err)
	}
	if resp.ID != "confirm-1" {
		t.Errorf("ID = %q, want %q", resp.ID, "confirm-1")
	}
	const wantResult = `{"action":"accept","content":{"allow":true}}`
	if got := string(resp.Result); got != wantResult {
		t.Errorf("Result = %s, want %s", got, wantResult)
	}
	if got, want := jsonMemberNames(t, resp), []string{"id", "result"}; !slices.Equal(got, want) {
		t.Errorf("InputResponse encodes members %v, want %v", got, want)
	}
}

// TestInputResponsesMustMatchRequests pins the first half of V10: every inputResponse.id
// must match an inputRequest.id before the client retries, and a response that answers
// nothing is rejected rather than sent. V10 assigns no error class to the rejection, so
// the assertion is deliberately class-agnostic.
func TestInputResponsesMustMatchRequests(t *testing.T) {
	requests := []InputRequest{{ID: "confirm-1"}, {ID: "confirm-2"}}

	tests := []struct {
		name      string
		requests  []InputRequest
		responses []InputResponse
		wantErr   bool
	}{
		{"every response answers a request", requests, []InputResponse{{ID: "confirm-1"}, {ID: "confirm-2"}}, false},
		{"one of several requests answered", requests, []InputResponse{{ID: "confirm-2"}}, false},
		{"no responses", requests, nil, false},
		{"unknown response id", requests, []InputResponse{{ID: "confirm-9"}}, true},
		{"one response in the set is unmatched", requests, []InputResponse{{ID: "confirm-1"}, {ID: "nope"}}, true},
		{"responses but no requests", nil, []InputResponse{{ID: "confirm-1"}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateInputResponses(tt.requests, tt.responses)
			switch {
			case tt.wantErr && err == nil:
				t.Fatal("ValidateInputResponses = nil, want an error: every inputResponse.id must match an inputRequest.id (V10)")
			case !tt.wantErr && err != nil:
				t.Fatalf("ValidateInputResponses = %v, want nil", err)
			}
		})
	}
}

// TestRequestStateRoundTripsByteForByte pins the second half of V10: a requestState is
// opaque. It is preserved exactly — including values that are not objects, which a value
// the SDK had interpreted could not be — and re-encoding it yields the input byte-for-byte,
// so the client can echo it verbatim on the retry (wire-protocol.md Section 5.3).
func TestRequestStateRoundTripsByteForByte(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"opaque string", `"b3BhcXVlLXJlc3VtZS10b2tlbg"`},
		{"object with members the SDK must not interpret", `{"step":7,"cursor":"abc","nested":{"a":[1,2,3]}}`},
		{"array", `[1,"two",{"three":3}]`},
		{"number", `42`},
		{"boolean", `true`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"requestState":` + tt.raw + `}`

			var wrapper struct {
				RequestState *RequestState `json:"requestState,omitempty"`
			}
			if err := json.Unmarshal([]byte(body), &wrapper); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
			if wrapper.RequestState == nil {
				t.Fatal("requestState decoded to nil; an opaque state must be preserved")
			}

			got, err := json.Marshal(wrapper)
			if err != nil {
				t.Fatalf("re-encode requestState: %v", err)
			}
			if string(got) != body {
				t.Errorf("requestState did not round-trip byte-for-byte:\n got %s\nwant %s", got, body)
			}

			// "never mutated": re-encoding the same value changes nothing.
			again, err := json.Marshal(wrapper)
			if err != nil {
				t.Fatalf("re-encode requestState a second time: %v", err)
			}
			if string(again) != string(got) {
				t.Errorf("requestState changed between encodes: %s -> %s", got, again)
			}
		})
	}
}

// jsonMemberNames returns the sorted top-level member names of v's JSON object encoding,
// so a test can assert a type's wire members without depending on field order.
func jsonMemberNames(t *testing.T, v any) []string {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil {
		t.Fatalf("decode %T as a JSON object: %v (encoded %s)", v, err, b)
	}
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
