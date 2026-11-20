package core

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests pin the required per-request metadata contract in core-api.md ("Required
// per-request metadata") and data-model.md rules V3/V4 (FR-002, FR-003): every request
// carries both io.modelcontextprotocol/protocolVersion and
// io.modelcontextprotocol/clientCapabilities under params._meta, a request missing either
// is rejected with a validation error, and metadata is fresh for every request — never
// inherited from a prior one.
//
// The symbols exercised here — the two MetaKey* constants, the RequestMeta type, and its
// validation entry point ValidateRequestMeta — belong to meta.go (T019). This file is
// written first and is expected to fail to compile until that task lands, so the red is
// attributable to meta.go. It deliberately names no symbol owned by another core file: the
// positive case is decoded from JSON rather than built by naming capabilities.go's
// ClientCapabilities (T021), so the only cross-file symbols it touches are the already
// implemented ErrValidation sentinel (errors.go, T015) and ProtocolVersionModern (era.go,
// T017). RequestMeta's own ClientCapabilities field makes meta.go — and therefore this
// file — transitively unable to compile without capabilities.go; that dependency is a
// property of the contract's RequestMeta, not of these tests.

// The two required _meta key spellings are spelled out literally so that changing the
// package constants cannot silently relax these assertions (wire-protocol.md Section 2).
const (
	wantMetaKeyProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	wantMetaKeyClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
)

// ValidateRequestMeta is the meta.go validation entry point this file defines for T019:
// it reports nil when a decoded _meta carries every required member, and an error
// matchable with errors.Is(err, ErrValidation) otherwise.
var _ func(*RequestMeta) error = ValidateRequestMeta

// validMeta builds a RequestMeta carrying both required keys. It decodes from JSON rather
// than naming the capabilities.go type, so this file depends on meta.go alone. The empty
// capability object is exactly the "{}" wire-protocol.md Section 2 requires when the
// client offers no optional features.
func validMeta(t *testing.T) *RequestMeta {
	t.Helper()

	body := `{"` + MetaKeyProtocolVersion + `":"` + ProtocolVersionModern + `","` + MetaKeyClientCapabilities + `":{}}`
	var m RequestMeta
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode canonical _meta %s: %v", body, err)
	}
	return &m
}

// TestMetaKeyConstantsMatchWireProtocol pins the frozen key spellings from
// wire-protocol.md Section 2. The values are a project record of an MCP 2026-07-28
// detail, so they are asserted against literals rather than against each other (FR-002).
func TestMetaKeyConstantsMatchWireProtocol(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"MetaKeyProtocolVersion", MetaKeyProtocolVersion, wantMetaKeyProtocolVersion},
		{"MetaKeyClientCapabilities", MetaKeyClientCapabilities, wantMetaKeyClientCapabilities},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

// TestRequestMetaJSONKeysMatchConstants pins the RequestMeta struct tags to the same key
// spellings as the constants: a request's _meta decodes into RequestMeta only when the
// wire keys match, so a drift between tag and constant would silently drop a required
// member (FR-002, V3). Decoding raw JSON also proves the capability object is reachable
// without naming its type from capabilities.go.
func TestRequestMetaJSONKeysMatchConstants(t *testing.T) {
	body := `{"` + wantMetaKeyProtocolVersion + `":"` + ProtocolVersionModern + `","` + wantMetaKeyClientCapabilities + `":{}}`

	var m RequestMeta
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode _meta %s: %v", body, err)
	}
	if m.ProtocolVersion != ProtocolVersionModern {
		t.Errorf("ProtocolVersion = %q, want %q: the struct tag must match %s", m.ProtocolVersion, ProtocolVersionModern, wantMetaKeyProtocolVersion)
	}
	if m.ClientCapabilities == nil {
		t.Errorf("ClientCapabilities = nil after decoding a present %s: the struct tag must match the constant", wantMetaKeyClientCapabilities)
	}
}

// TestValidateRequestMetaRequiresBothKeys pins V3: a request must carry both required
// _meta keys, and a request missing either — or missing _meta altogether — is rejected
// with a validation error. The error message names the offending key so a server can
// populate the wire error's data.field (wire-protocol.md Section 1.3).
func TestValidateRequestMetaRequiresBothKeys(t *testing.T) {
	tests := []struct {
		name       string
		build      func(t *testing.T) *RequestMeta
		wantErr    bool
		missingKey string // the key the message must name, "" when it need not name one
	}{
		{
			name:    "both keys present",
			build:   validMeta,
			wantErr: false,
		},
		{
			name:    "no _meta at all",
			build:   func(*testing.T) *RequestMeta { return nil },
			wantErr: true,
		},
		{
			name: "missing protocolVersion",
			build: func(t *testing.T) *RequestMeta {
				m := validMeta(t)
				m.ProtocolVersion = ""
				return m
			},
			wantErr:    true,
			missingKey: MetaKeyProtocolVersion,
		},
		{
			name: "missing clientCapabilities",
			build: func(t *testing.T) *RequestMeta {
				m := validMeta(t)
				m.ClientCapabilities = nil
				return m
			},
			wantErr:    true,
			missingKey: MetaKeyClientCapabilities,
		},
		{
			// A present-but-blank revision is malformed, and wire-protocol.md Section 6
			// groups "missing or malformed" _meta under the same rejection (V3).
			name: "blank protocolVersion",
			build: func(t *testing.T) *RequestMeta {
				m := validMeta(t)
				m.ProtocolVersion = "   "
				return m
			},
			wantErr:    true,
			missingKey: MetaKeyProtocolVersion,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRequestMeta(tt.build(t))
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ValidateRequestMeta(valid) = %v, want nil", err)
				}
				return
			}

			if err == nil {
				t.Fatal("ValidateRequestMeta(missing key) = nil, want a validation error (V3)")
			}
			if !errors.Is(err, ErrValidation) {
				t.Errorf("errors.Is(err, ErrValidation) = false, want true (FR-002, V3)")
			}
			if tt.missingKey != "" && !strings.Contains(err.Error(), tt.missingKey) {
				t.Errorf("error %q does not name the missing key %q", err.Error(), tt.missingKey)
			}
		})
	}
}

// TestMissingMetaKeyIsAnInvalidParamsFailure pins V3's code mapping at the core layer.
// wire-protocol.md Section 6 and Section 6.1 assign a missing or incomplete _meta exactly
// one JSON-RPC code, -32602 (CodeInvalidParams); core expresses that class as
// ErrValidation, and the numeric code itself is the peer-facing concern of server and
// transport (docs/decisions/0002-codec-validation.md). This test therefore asserts the
// returned error belongs to the ErrValidation — hence -32602 — class and to no other, so
// a server cannot mistake a missing key for a method-not-found or an internal failure.
func TestMissingMetaKeyIsAnInvalidParamsFailure(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T) *RequestMeta
	}{
		{"no _meta", func(*testing.T) *RequestMeta { return nil }},
		{"missing protocolVersion", func(t *testing.T) *RequestMeta {
			m := validMeta(t)
			m.ProtocolVersion = ""
			return m
		}},
		{"missing clientCapabilities", func(t *testing.T) *RequestMeta {
			m := validMeta(t)
			m.ClientCapabilities = nil
			return m
		}},
	}

	others := []struct {
		name     string
		sentinel error
	}{
		{"ErrTransport", ErrTransport},
		{"ErrAuth", ErrAuth},
		{"ErrRegistration", ErrRegistration},
		{"ErrMethodNotFound", ErrMethodNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRequestMeta(tt.build(t))
			if err == nil {
				t.Fatalf("ValidateRequestMeta = nil, want the CodeInvalidParams (%d) class (V3)", CodeInvalidParams)
			}
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("errors.Is(err, ErrValidation) = false, want true: missing _meta is -32602, not any other class")
			}
			for _, other := range others {
				if errors.Is(err, other.sentinel) {
					t.Errorf("errors.Is(err, %s) = true, want false: missing _meta is only the CodeInvalidParams class", other.name)
				}
			}
		})
	}
}

// TestRequestMetaValidationIsFreshPerRequest pins V4/FR-003 behaviourally: validation is a
// pure function of the meta it is handed, so nothing about one request's _meta is carried
// into the next. A second, self-contained request that omits a key is rejected even though
// the first was valid, and validation never mutates its input.
func TestRequestMetaValidationIsFreshPerRequest(t *testing.T) {
	first := validMeta(t)
	if err := ValidateRequestMeta(first); err != nil {
		t.Fatalf("first valid request rejected: %v", err)
	}
	firstBefore := marshalMeta(t, first)

	// A second request over the same connection that omits clientCapabilities must be
	// rejected on its own terms — there is no fallback to the first request's _meta.
	second := &RequestMeta{ProtocolVersion: ProtocolVersionModern}
	err := ValidateRequestMeta(second)
	if err == nil {
		t.Fatal("a second request missing clientCapabilities was accepted; _meta must be fresh per request (V4, FR-003)")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("errors.Is(err, ErrValidation) = false, want true")
	}

	// Validation is read-only: the valid meta is unchanged, and re-validating it still
	// succeeds. A validator that cached or mutated state would break one of these.
	if after := marshalMeta(t, first); after != firstBefore {
		t.Errorf("ValidateRequestMeta mutated its input: %s -> %s", firstBefore, after)
	}
	if err := ValidateRequestMeta(first); err != nil {
		t.Errorf("re-validating the first meta failed: %v", err)
	}
}

// TestRequestMetaIsNeverHeldAtPackageScope pins V4 structurally: package core holds no
// package-scope RequestMeta (or *RequestMeta) variable, so per-request metadata cannot be
// cached between requests. The guard lives here so a future package-level "last seen meta"
// cannot quietly reintroduce inheritance. It mirrors the era inference guard in
// era_test.go.
func TestRequestMetaIsNeverHeldAtPackageScope(t *testing.T) {
	// Resolve the package directory from this test file's own path rather than the
	// process working directory, so the guard is independent of how the test is run.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate the test file")
	}
	dir := filepath.Dir(thisFile)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package directory %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	var parsedFiles, parsedVars int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsedFiles++

		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				parsedVars++
				if exprNamesRequestMeta(vs.Type) {
					t.Errorf("%s: package-scope var of type RequestMeta caches per-request metadata (V4, FR-003)", name)
				}
				for _, value := range vs.Values {
					if exprNamesRequestMeta(value) {
						t.Errorf("%s: package-scope var initialised from a RequestMeta literal caches per-request metadata (V4, FR-003)", name)
					}
				}
			}
		}
	}

	// Guard the guard: an empty package walk would make this test vacuous.
	if parsedFiles == 0 {
		t.Fatal("no non-test .go files found; the metadata-caching guard inspected nothing")
	}
	if parsedVars == 0 {
		t.Fatal("no package-scope var declarations found; the metadata-caching guard inspected nothing")
	}
}

// marshalMeta renders a RequestMeta as JSON so a test can compare it before and after a
// call without naming any of its field types.
func marshalMeta(t *testing.T, m *RequestMeta) string {
	t.Helper()

	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal RequestMeta: %v", err)
	}
	return string(b)
}

// exprNamesRequestMeta reports whether a type expression denotes RequestMeta, directly or
// through a pointer, an address-of operator, or a composite literal.
func exprNamesRequestMeta(expr ast.Expr) bool {
	switch t := expr.(type) {
	case nil:
		return false
	case *ast.Ident:
		return t.Name == "RequestMeta"
	case *ast.StarExpr:
		return exprNamesRequestMeta(t.X)
	case *ast.UnaryExpr:
		return exprNamesRequestMeta(t.X)
	case *ast.CompositeLit:
		return exprNamesRequestMeta(t.Type)
	default:
		return false
	}
}
