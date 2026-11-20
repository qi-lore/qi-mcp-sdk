package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests pin the protocol-era contract in core-api.md ("Protocol era and version
// constants") and data-model.md Section 10, together with the era rules V12/V13 and
// FR-005/FR-051: the zero value of Era is the modern era, each era has exactly one
// supported revision, and the era is caller-selected — never inferred from observed
// traffic.
//
// Every symbol exercised here — EraModern, EraLegacy, the two ProtocolVersion*
// constants, SupportedVersions, and the String method — belongs to era.go (T017). This
// file is written first and is expected to fail to compile until that task lands, so
// the red is attributable to era.go alone. It names no symbol owned by another core
// test file.

// TestEraZeroValueIsModern pins the first era rule: an Era that was never set is the
// modern era. Because selection is explicit, "nothing observed yet" can only mean
// modern — the zero value never drifts to legacy (FR-051, V12).
func TestEraZeroValueIsModern(t *testing.T) {
	var zero Era
	if zero != EraModern {
		t.Errorf("zero value of Era = %v, want EraModern (%v)", zero, EraModern)
	}
	if zero == EraLegacy {
		t.Errorf("zero value of Era compares equal to EraLegacy; the default must be modern")
	}

	// The default's supported set is modern-only: an unset era offers the caller the
	// modern revision and never the legacy one on its own (V12).
	got := SupportedVersions(zero)
	want := []string{ProtocolVersionModern}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("SupportedVersions(zero Era) = %v, want %v", got, want)
	}
}

// TestEraString pins the String method from core-api.md: "modern" for the default and
// "legacy" for the opt-in era, so diagnostics and the NegotiationError message stay
// readable (FR-051).
func TestEraString(t *testing.T) {
	tests := []struct {
		name string
		era  Era
		want string
	}{
		{"EraModern", EraModern, "modern"},
		{"EraLegacy", EraLegacy, "legacy"},
		{"zero value", Era(0), "modern"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.era.String(); got != tt.want {
				t.Errorf("%s.String() = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestProtocolVersionConstants pins the two revisions in FR-005: the modern era's
// 2026-07-28 is primary, and 2025-11-25 is the single supported legacy revision. The
// literals are spelled out rather than derived, so changing a constant cannot silently
// relax the assertion.
func TestProtocolVersionConstants(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"ProtocolVersionModern", ProtocolVersionModern, "2026-07-28"},
		{"ProtocolVersionLegacy", ProtocolVersionLegacy, "2025-11-25"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

// TestSupportedVersionsPerEra pins core-api.md: an era supports exactly one revision —
// modern yields {ProtocolVersionModern}, legacy yields {ProtocolVersionLegacy}. The
// word "exactly" is load-bearing: only 2025-11-25 is accepted from the legacy era
// (FR-005, FR-051, V12, V13).
func TestSupportedVersionsPerEra(t *testing.T) {
	tests := []struct {
		name string
		era  Era
		want string
	}{
		{"EraModern", EraModern, ProtocolVersionModern},
		{"EraLegacy", EraLegacy, ProtocolVersionLegacy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SupportedVersions(tt.era)
			if len(got) != 1 {
				t.Fatalf("SupportedVersions(%s) = %v, want exactly one revision", tt.name, got)
			}
			if got[0] != tt.want {
				t.Errorf("SupportedVersions(%s) = %v, want [%q]", tt.name, got, tt.want)
			}
		})
	}
}

// TestSupportedVersionsAreDisjointPerEra pins V12/V13 across the two eras: the modern
// and legacy supported sets share no revision. Because a revision belongs to exactly
// one era, observing a revision on the wire can never be ambiguous — which is what
// makes inferring the era from traffic both unnecessary and forbidden (FR-051).
func TestSupportedVersionsAreDisjointPerEra(t *testing.T) {
	modern := SupportedVersions(EraModern)
	legacy := SupportedVersions(EraLegacy)

	for _, m := range modern {
		for _, l := range legacy {
			if m == l {
				t.Errorf("revision %q is supported by both eras; the sets must be disjoint", m)
			}
		}
	}

	if containsRevision(modern, ProtocolVersionLegacy) {
		t.Errorf("modern era supports the legacy revision %q; a modern session must never select it", ProtocolVersionLegacy)
	}
	if containsRevision(legacy, ProtocolVersionModern) {
		t.Errorf("legacy era supports the modern revision %q; the legacy path is 2025-11-25 only", ProtocolVersionModern)
	}
}

// TestEraIsNeverInferredFromTraffic pins FR-051/V12 structurally: no function or method
// declared in package core returns an Era. Era selection is a connection-level,
// caller-supplied configuration value (R-004), so there is deliberately no entry point
// that derives an Era from a revision string, a peer's handshake, or any other observed
// traffic. The guard lives here so that a future era-producing function cannot quietly
// reintroduce sniffing.
func TestEraIsNeverInferredFromTraffic(t *testing.T) {
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
	var parsedFiles, parsedFuncs int
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
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			parsedFuncs++
			if resultsNameEra(fn.Type.Results) {
				t.Errorf("%s returns an Era: era selection is caller-explicit and never inferred from traffic (FR-051, V12)", fn.Name.Name)
			}
		}
	}

	// Guard the guard: an empty package walk would make this test vacuous.
	if parsedFiles == 0 {
		t.Fatal("no non-test .go files found; the inference guard inspected nothing")
	}
	if parsedFuncs == 0 {
		t.Fatal("no function declarations found; the inference guard inspected nothing")
	}
}

// containsRevision reports whether a supported-revision set holds want.
func containsRevision(set []string, want string) bool {
	for _, v := range set {
		if v == want {
			return true
		}
	}
	return false
}

// resultsNameEra reports whether a result list names Era, directly or through a pointer.
func resultsNameEra(results *ast.FieldList) bool {
	if results == nil {
		return false
	}
	for _, field := range results.List {
		if exprNamesEra(field.Type) {
			return true
		}
	}
	return false
}

// exprNamesEra reports whether a type expression is Era or *Era.
func exprNamesEra(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "Era"
	case *ast.StarExpr:
		return exprNamesEra(t.X)
	default:
		return false
	}
}
