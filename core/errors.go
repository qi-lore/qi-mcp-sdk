package core

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors, usable with errors.Is (NFR-005). Callers branch on the broad
// failure class with these and read the typed detail — when there is one — with
// errors.As (core-api.md "Error taxonomy", R-018).
var (
	// ErrTransport marks a transport-level failure: a broken connection, a closed
	// pipe, or a frame that could not be read or written.
	ErrTransport = errors.New("core: transport failure")
	// ErrValidation marks a schema or argument validation failure (FR-008).
	ErrValidation = errors.New("core: schema validation failure")
	// ErrAuth marks an authorization failure. The accompanying error never embeds
	// credential material (FR-027).
	ErrAuth = errors.New("core: authorization failure")
	// ErrRegistration marks a feature-registration failure: a name-rule violation or
	// a duplicate registration (FR-013).
	ErrRegistration = errors.New("core: registration failure")
	// ErrMethodNotFound marks a request for a method this side does not serve (FR-018).
	ErrMethodNotFound = errors.New("core: method not found")
)

// JSON-RPC 2.0 error codes used by the SDK (wire-protocol.md Section 6).
//
// CodeUnsupportedVersion and CodeNegotiationFailure share the value -32022 because
// the MCP 2026-07-28 revision defines exactly one version-mismatch code (T007). The
// two conditions — a request for an unsupported revision, and era/version negotiation
// that cannot select a mutually supported revision — stay distinct only at the Go
// level, through UnsupportedProtocolVersionError and NegotiationError.
const (
	CodeParseError         = -32700
	CodeInvalidRequest     = -32600
	CodeMethodNotFound     = -32601
	CodeInvalidParams      = -32602
	CodeInternalError      = -32603
	CodeUnsupportedVersion = -32022
	CodeNegotiationFailure = -32022
)

// UnsupportedProtocolVersionError reports that a peer requested a protocol revision
// the local side does not support (FR-005). It carries the revision the peer asked
// for and the revisions this side supports, matching the wire data shape in
// wire-protocol.md Section 7.1.
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type UnsupportedProtocolVersionError struct {
	Requested string   // the revision the peer asked for
	Supported []string // revisions this side supports
}

func (e *UnsupportedProtocolVersionError) Error() string {
	return fmt.Sprintf("core: unsupported protocol version %q (supported: %s)", e.Requested, joinVersions(e.Supported))
}

// NegotiationError reports that era or version negotiation could not select a
// mutually supported revision — the two sides' supported sets are disjoint, or a
// legacy peer offered a revision other than the one supported legacy revision
// (FR-054). Both sides' supported sets are carried so the caller can decide whether
// to retry.
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type NegotiationError struct {
	Era    Era      // the era in which negotiation was attempted
	Local  []string // revisions the local side supports
	Remote []string // revisions the peer supports
}

func (e *NegotiationError) Error() string {
	return fmt.Sprintf("core: protocol negotiation failed (era %v): local supports %s, remote supports %s",
		e.Era, joinVersions(e.Local), joinVersions(e.Remote))
}

// RegistrationError reports a name-rule violation or a duplicate registration. The
// previously registered feature is left unchanged (FR-013).
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type RegistrationError struct {
	Kind   string // "tool", "resource", or "prompt"
	Name   string
	Reason string // for example "invalid name" or "duplicate name"
}

func (e *RegistrationError) Error() string {
	return fmt.Sprintf("core: %s registration failed for %q: %s", e.Kind, e.Name, e.Reason)
}

// AuthError reports an authorization failure without embedding the token (FR-027).
// It carries a reason only; there is deliberately nowhere for credential material to
// live.
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type AuthError struct {
	Reason string // for example "expired", "wrong audience", "unknown key"
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("core: authorization failure: %s", e.Reason)
}

// joinVersions renders a supported-revision set for an error message. An empty set is
// rendered as "(none)" rather than as an empty string, so the message stays readable.
func joinVersions(versions []string) string {
	if len(versions) == 0 {
		return "(none)"
	}
	return strings.Join(versions, ", ")
}
