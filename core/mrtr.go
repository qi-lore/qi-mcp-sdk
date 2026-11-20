package core

import (
	"encoding/json"
	"errors"
	"fmt"
)

// This file implements the MRTR trio from core-api.md ("Result envelope and MRTR types")
// and wire-protocol.md Section 5, together with rule V10 (FR-007): an inputResponse
// answers an inputRequest by matching its `id`, and a requestState is opaque — the SDK
// round-trips it byte-for-byte and never interprets or mutates it. The client MRTR driver
// and the server resume path are the callers; this file owns only the shared types and the
// correlation rule.

// InputRequest is one item the server needs from the client before it can finish a
// request (FR-007). It is one entry of a result's `inputRequests` array: Method names the
// operation that obtains the input (for example "elicitation/create") and Params carries
// that method's arguments, whose shape is defined by Method (wire-protocol.md Section 5.2).
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type InputRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// InputResponse is the client's answer to one InputRequest (FR-007). It is one entry of
// the `inputResponses` array a client sends on the retry: ID is the member that correlates
// it with the InputRequest it answers (V10), and Result carries the answer, whose shape is
// defined by the answered request's Method (wire-protocol.md Section 5.3).
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type InputResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
}

// RequestState is opaque, server-issued state that lets a server resume an MRTR flow
// (FR-007). The client round-trips it verbatim and never interprets it: a decoded state
// re-encodes to exactly the bytes the server sent, so the retry carries the server's own
// value back (V10; wire-protocol.md Section 5.3). Value is whatever JSON the server chose —
// it is not required to be an object, and the SDK attaches no meaning to it.
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type RequestState struct {
	Value json.RawMessage
}

// MarshalJSON emits the opaque value exactly as received, never wrapped in an object of the
// SDK's own making, so a client's retry echoes the original bytes (V10). A state with no
// value encodes as JSON null. The receiver is not mutated.
func (s RequestState) MarshalJSON() ([]byte, error) {
	if len(s.Value) == 0 {
		return []byte("null"), nil
	}
	return s.Value, nil
}

// UnmarshalJSON captures the value verbatim, whatever its JSON type, so the SDK never
// interprets it and can re-encode it unchanged (V10). The bytes are copied rather than
// aliased, so the state does not borrow the lifetime of the buffer it was decoded from.
func (s *RequestState) UnmarshalJSON(data []byte) error {
	if s == nil {
		return errors.New("core: RequestState: UnmarshalJSON on nil *RequestState")
	}
	s.Value = append(s.Value[:0], data...)
	return nil
}

// ValidateInputResponses reports nil when every inputResponse answers one of the requests —
// matched by `id` — and a validation error otherwise (V10, FR-007). It is the rule the
// client applies before retrying, so a response the server asked nothing for is rejected
// rather than sent. Requests and responses that are both empty are valid: there is nothing
// to correlate.
//
// The failure wraps ErrValidation, so it is matchable with errors.Is (NFR-005); the numeric
// code it maps to is assigned by server and transport
// (docs/decisions/0002-codec-validation.md). Validation reads and writes no package state
// and never mutates its arguments, so it is a pure function.
func ValidateInputResponses(requests []InputRequest, responses []InputResponse) error {
	if len(responses) == 0 {
		return nil
	}

	asked := make(map[string]struct{}, len(requests))
	for _, req := range requests {
		asked[req.ID] = struct{}{}
	}
	for _, resp := range responses {
		if _, ok := asked[resp.ID]; !ok {
			return fmt.Errorf("%w: inputResponse %q answers no inputRequest (V10)", ErrValidation, resp.ID)
		}
	}
	return nil
}
