package core

import (
	"fmt"
	"strings"
)

// This file implements the required per-request metadata contract from core-api.md
// ("Required per-request metadata") and data-model.md rules V3/V4 (FR-002, FR-003). A
// modern-era request carries its context in params._meta instead of inheriting it from a
// connection, so the metadata is supplied and validated fresh on every request.

// MetaKeyProtocolVersion is the _meta key carrying the selected protocol revision
// (FR-002). The value is spelled exactly as required on the wire, so it doubles as the
// ProtocolVersion struct tag on RequestMeta.
const MetaKeyProtocolVersion = "io.modelcontextprotocol/protocolVersion"

// MetaKeyClientCapabilities is the _meta key carrying the client's declared feature set
// (FR-002). It likewise doubles as a RequestMeta struct tag.
const MetaKeyClientCapabilities = "io.modelcontextprotocol/clientCapabilities"

// RequestMeta is the per-request context that replaces connection state (FR-002, FR-003).
// It is decoded from a request's params._meta and is supplied fresh on every request:
// nothing about one request's metadata is carried into the next.
//
// Both members are required. A nil ClientCapabilities, or a ProtocolVersion that is empty
// once trimmed, means the member was absent from the wire — or malformed — and validation
// rejects the request (V3).
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type RequestMeta struct {
	ProtocolVersion    string              `json:"io.modelcontextprotocol/protocolVersion"`
	ClientCapabilities *ClientCapabilities `json:"io.modelcontextprotocol/clientCapabilities"`
}

// ValidateRequestMeta reports nil when a decoded _meta carries every required member, and
// a validation error otherwise (FR-002, FR-003, V3, V4). A nil meta — a request with no
// params._meta at all — and a meta missing either required key are rejected with an error
// matchable with errors.Is(err, ErrValidation); the error names the offending key so a
// server can populate the wire error's data.field (wire-protocol.md Section 1.3). The
// numeric CodeInvalidParams (-32602) that class maps to is assigned by server and
// transport (docs/decisions/0002-codec-validation.md).
//
// Validation is a pure function of its argument: it reads and writes no package state and
// never mutates the meta it is handed, so per-request metadata cannot be inherited from a
// prior request (V4).
func ValidateRequestMeta(meta *RequestMeta) error {
	if meta == nil {
		return fmt.Errorf("%w: missing _meta", ErrValidation)
	}
	if strings.TrimSpace(meta.ProtocolVersion) == "" {
		return fmt.Errorf("%w: missing %s", ErrValidation, MetaKeyProtocolVersion)
	}
	if meta.ClientCapabilities == nil {
		return fmt.Errorf("%w: missing %s", ErrValidation, MetaKeyClientCapabilities)
	}
	return nil
}
