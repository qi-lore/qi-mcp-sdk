package core

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// These tests pin the error taxonomy in core-api.md ("Error taxonomy") and
// data-model.md Section 17: every sentinel is matchable with errors.Is through a %w
// wrap, every typed error is reachable with errors.As through one, the wire error
// codes carry the values fixed by wire-protocol.md Section 6 — with T007's correction
// of the provisional -32001/-32002 to -32022 — and no error string embeds credential
// material (FR-018, FR-054, NFR-005; R-018; FR-027).
//
// The sentinels, code constants, and typed errors belong to errors.go (T015); this file
// is written first and is expected to fail to compile until that task lands. Like
// codec_test.go, it deliberately names no symbol owned by another core file — in
// particular it leaves NegotiationError.Era at its zero value rather than reaching for
// era.go (T017) — so the red is attributable to errors.go alone.

// credentialMaterial is a syntactically plausible JWT: a credential-shaped value that
// must never appear in an error string this package produces (FR-027, SC-007). Using a
// realistic value keeps the leak scan meaningful rather than a search for a string that
// could never appear anyway.
const credentialMaterial = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"

// Compile-time conformance: each typed error implements error (FR-018, FR-054).
var (
	_ error = (*UnsupportedProtocolVersionError)(nil)
	_ error = (*NegotiationError)(nil)
	_ error = (*RegistrationError)(nil)
	_ error = (*AuthError)(nil)
)

// TestSentinelErrorsMatchThroughWrappedError pins NFR-005: a core sentinel is usable
// with errors.Is even after the caller wraps it with context.
func TestSentinelErrorsMatchThroughWrappedError(t *testing.T) {
	sentinels := []struct {
		name     string
		sentinel error
	}{
		{"ErrTransport", ErrTransport},
		{"ErrValidation", ErrValidation},
		{"ErrAuth", ErrAuth},
		{"ErrRegistration", ErrRegistration},
		{"ErrMethodNotFound", ErrMethodNotFound},
	}

	for _, tt := range sentinels {
		t.Run(tt.name, func(t *testing.T) {
			if tt.sentinel == nil {
				t.Fatalf("%s is nil, want a sentinel error", tt.name)
			}
			if tt.sentinel.Error() == "" {
				t.Errorf("%s has an empty message, want a non-empty one", tt.name)
			}

			wrapped := fmt.Errorf("operation failed: %w", tt.sentinel)
			if !errors.Is(wrapped, tt.sentinel) {
				t.Errorf("errors.Is(wrapped, %s) = false, want true (NFR-005)", tt.name)
			}
			if !errors.Is(tt.sentinel, tt.sentinel) {
				t.Errorf("errors.Is(%s, %s) = false, want true", tt.name, tt.name)
			}
		})
	}
}

// TestSentinelErrorsAreMutuallyDistinct pins that the five sentinels are separate
// values: errors.Is on one never reports a match for another, so a caller can tell
// "transport" from "validation" from "auth" (FR-018, NFR-005).
func TestSentinelErrorsAreMutuallyDistinct(t *testing.T) {
	sentinels := []error{ErrTransport, ErrValidation, ErrAuth, ErrRegistration, ErrMethodNotFound}

	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("errors.Is(sentinel[%d], sentinel[%d]) = true, want false: the sentinels must not alias", i, j)
			}
		}
	}
}

// TestTypedErrorsMatchThroughWrappedError pins the errors.As half of NFR-005: each
// typed error is extracted from a %w wrap with its data intact (FR-005, FR-054, FR-013).
func TestTypedErrorsMatchThroughWrappedError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		target any // **T, the value handed to errors.As
		want   any // *T, the value errors.As must extract
	}{
		{
			// wire-protocol.md Section 7.1: the version-mismatch error carries the
			// supported set and the requested revision.
			name:   "UnsupportedProtocolVersionError",
			err:    &UnsupportedProtocolVersionError{Requested: "2026-06-01", Supported: []string{"2026-07-28"}},
			target: new(*UnsupportedProtocolVersionError),
			want:   &UnsupportedProtocolVersionError{Requested: "2026-06-01", Supported: []string{"2026-07-28"}},
		},
		{
			// FR-054: a disjoint supported set carries both sides. Era is left at its
			// zero value on purpose — the Era type and its constants are owned by
			// era.go (T017), and this file must not depend on another core file.
			name:   "NegotiationError",
			err:    &NegotiationError{Local: []string{"2025-11-25"}, Remote: []string{"2025-06-18"}},
			target: new(*NegotiationError),
			want:   &NegotiationError{Local: []string{"2025-11-25"}, Remote: []string{"2025-06-18"}},
		},
		{
			// FR-013: a registration failure names the feature kind, the offending name
			// unchanged, and the reason.
			name:   "RegistrationError",
			err:    &RegistrationError{Kind: "tool", Name: "bad name", Reason: "invalid name"},
			target: new(*RegistrationError),
			want:   &RegistrationError{Kind: "tool", Name: "bad name", Reason: "invalid name"},
		},
		{
			name:   "AuthError",
			err:    &AuthError{Reason: "expired"},
			target: new(*AuthError),
			want:   &AuthError{Reason: "expired"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Error() == "" {
				t.Fatalf("%s has an empty message, want a non-empty one", tt.name)
			}

			wrapped := fmt.Errorf("request failed: %w", tt.err)
			if !errors.As(wrapped, tt.target) {
				t.Fatalf("errors.As(wrapped, *%s) = false, want true (NFR-005)", tt.name)
			}
			got := reflect.ValueOf(tt.target).Elem().Interface()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("errors.As extracted %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestSentinelAndTypedErrorSurviveTheSameWrapChain pins the pairing the contract draws
// in data-model.md Section 17 — the ErrAuth sentinel with a wrapped *AuthError — so a
// caller can branch on the sentinel and read the typed detail from one error.
func TestSentinelAndTypedErrorSurviveTheSameWrapChain(t *testing.T) {
	inner := &AuthError{Reason: "wrong audience"}
	mid := fmt.Errorf("%w: %w", ErrAuth, inner)
	outer := fmt.Errorf("client: token rejected: %w", mid)

	if !errors.Is(outer, ErrAuth) {
		t.Errorf("errors.Is(outer, ErrAuth) = false, want true")
	}

	var got *AuthError
	if !errors.As(outer, &got) {
		t.Fatalf("errors.As(outer, *AuthError) = false, want true")
	}
	if got != inner {
		t.Errorf("errors.As extracted %p, want the original %p", got, inner)
	}
}

// TestErrorCodesMatchWireProtocol pins wire-protocol.md Section 6: the standard
// JSON-RPC codes keep their values, and the version-mismatch conditions both map to
// -32022 (FR-018, FR-054).
func TestErrorCodesMatchWireProtocol(t *testing.T) {
	tests := []struct {
		name string
		code int
		want int
	}{
		{"CodeParseError", CodeParseError, -32700},
		{"CodeInvalidRequest", CodeInvalidRequest, -32600},
		{"CodeMethodNotFound", CodeMethodNotFound, -32601},
		{"CodeInvalidParams", CodeInvalidParams, -32602},
		{"CodeInternalError", CodeInternalError, -32603},
		{"CodeUnsupportedVersion", CodeUnsupportedVersion, -32022},
		{"CodeNegotiationFailure", CodeNegotiationFailure, -32022},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.code != tt.want {
				t.Errorf("%s = %d, want %d", tt.name, tt.code, tt.want)
			}
		})
	}
}

// TestVersionMismatchCodesRejectTheProvisionalValues guards T007's correction
// (wire-protocol.md Section 6): -32001 sits in the legacy sub-range the 2026-07-28
// revision never assigns, and -32002 is "resource not found", which the SDK MUST NOT
// emit. Neither may creep back in as a version-mismatch code.
func TestVersionMismatchCodesRejectTheProvisionalValues(t *testing.T) {
	for _, rejected := range []int{-32001, -32002} {
		if CodeUnsupportedVersion == rejected {
			t.Errorf("CodeUnsupportedVersion = %d, want -32022", rejected)
		}
		if CodeNegotiationFailure == rejected {
			t.Errorf("CodeNegotiationFailure = %d, want -32022", rejected)
		}
	}

	if CodeMethodNotFound == -32002 {
		t.Errorf("CodeMethodNotFound = -32002, want -32601: -32002 is reserved for resource not found")
	}
}

// TestErrorStringsNeverEmbedCredentialMaterial pins FR-027 for every error this file
// builds: no Error() string carries credential material.
func TestErrorStringsNeverEmbedCredentialMaterial(t *testing.T) {
	errs := []struct {
		name string
		err  error
	}{
		{"ErrTransport", ErrTransport},
		{"ErrValidation", ErrValidation},
		{"ErrAuth", ErrAuth},
		{"ErrRegistration", ErrRegistration},
		{"ErrMethodNotFound", ErrMethodNotFound},
		{"UnsupportedProtocolVersionError", &UnsupportedProtocolVersionError{Requested: "2026-06-01", Supported: []string{"2026-07-28"}}},
		{"NegotiationError", &NegotiationError{Local: []string{"2025-11-25"}, Remote: []string{"2025-06-18"}}},
		{"RegistrationError", &RegistrationError{Kind: "tool", Name: "bad name", Reason: "invalid name"}},
		{"AuthError", &AuthError{Reason: "expired"}},
		{"wrapped auth failure", fmt.Errorf("authorization failed: %w", &AuthError{Reason: "expired"})},
	}

	for _, tt := range errs {
		t.Run(tt.name, func(t *testing.T) {
			msg := tt.err.Error()
			if strings.Contains(msg, credentialMaterial) {
				t.Errorf("%s message embeds credential material: %q", tt.name, msg)
			}
		})
	}
}

// TestAuthErrorExposesNoCredentialBearingField pins FR-027 structurally: AuthError
// carries a reason, never the token, so there is nowhere for credential material to
// live. The guard stops a later field from quietly reopening the leak.
func TestAuthErrorExposesNoCredentialBearingField(t *testing.T) {
	credentialish := []string{"token", "credential", "secret", "password", "bearer", "authorization"}

	typ := reflect.TypeOf(AuthError{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		for _, bad := range credentialish {
			if strings.Contains(name, bad) {
				t.Errorf("AuthError field %q looks credential-bearing, want a reason only (FR-027)", typ.Field(i).Name)
			}
		}
	}
}
