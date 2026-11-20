package core

import (
	"encoding/json"
	"errors"
	"fmt"
)

// This file implements the result envelope from contracts/core-api.md ("Result envelope
// and MRTR types") and wire-protocol.md Section 4, together with rule V9 (FR-006): a
// result carries a `resultType` discriminator with exactly two values, an absent
// discriminator is treated as "complete" for legacy interoperability and is never an
// error, and an "input_required" result that requests nothing is malformed.
//
// ResultEnvelope is the generic envelope a client reads after core.Decode hands it a
// Response's raw `result`. The absent-means-complete rule attaches to the envelope's own
// UnmarshalJSON — the idiomatic Go decode hook — so every decode path, including
// encoding/json's, applies it without a separate entry point. Operation-specific result
// types (for example the server package's DiscoverResult) carry their own payload fields
// alongside a ResultType; this envelope is the shared shape they build on.

// ResultType discriminates a result envelope (FR-006). Its zero value is the empty
// string; a decoded envelope never retains that zero, because UnmarshalJSON resolves an
// absent discriminator to ResultComplete.
type ResultType string

const (
	// ResultComplete marks a terminal result whose payload is final. It is also what an
	// absent `resultType` decodes to (FR-006, V9).
	ResultComplete ResultType = "complete"
	// ResultInputRequired marks a result the server cannot finish until the client
	// supplies the requested inputs (MRTR; FR-007, V9).
	ResultInputRequired ResultType = "input_required"
)

// ResultEnvelope is the typed result of a request (FR-006, FR-007). On the wire the
// operation-specific payload members sit alongside `resultType` in one JSON object;
// InputRequests and RequestState are populated only on an input_required round trip.
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type ResultEnvelope struct {
	// ResultType discriminates the envelope. A decoded envelope is never left empty: an
	// absent discriminator becomes ResultComplete (FR-006, V9).
	ResultType ResultType `json:"resultType,omitempty"`
	// InputRequests is what the server needs before it can finish. It is required and
	// non-empty when ResultType is ResultInputRequired (FR-007, V9).
	InputRequests []InputRequest `json:"inputRequests,omitempty"`
	// RequestState is the opaque, server-issued state that lets the server resume; it is
	// meaningful only with ResultInputRequired (FR-007).
	RequestState *RequestState `json:"requestState,omitempty"`
}

// UnmarshalJSON decodes a result object and applies rule V9 (FR-006, FR-007): an absent
// `resultType` is resolved to ResultComplete — the legacy-interoperability path, which is
// never an error — and an `input_required` result that carries no inputRequests is
// rejected rather than decoded.
//
// The rejection wraps ErrValidation, so it is matchable with errors.Is (NFR-005); the
// numeric code it maps to is assigned by server and transport
// (docs/decisions/0002-codec-validation.md).
func (e *ResultEnvelope) UnmarshalJSON(data []byte) error {
	if e == nil {
		return errors.New("core: ResultEnvelope: UnmarshalJSON on nil *ResultEnvelope")
	}

	// A distinct type drops the method set, so decoding does not recurse back into this
	// UnmarshalJSON; the fields are otherwise identical.
	type plainResultEnvelope ResultEnvelope
	var decoded plainResultEnvelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	if decoded.ResultType == "" {
		decoded.ResultType = ResultComplete
	}
	if decoded.ResultType == ResultInputRequired && len(decoded.InputRequests) == 0 {
		return fmt.Errorf("%w: input_required result carries no inputRequests (V9)", ErrValidation)
	}

	*e = ResultEnvelope(decoded)
	return nil
}
