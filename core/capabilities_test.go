package core

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// These tests pin the capability and implementation types in core-api.md ("Capability and
// implementation types") and data-model.md rule V5 (FR-002, FR-014), together with the
// advertisement shape in wire-protocol.md Section 3: capability objects round-trip through
// JSON, an absent member means the feature is not offered, and the sub-capability flags
// listChanged and subscribe are additive.
//
// Every symbol exercised here — Implementation, ElicitationCapability, ClientCapabilities,
// ToolsCapability, ResourcesCapability, PromptsCapability, ServerCapabilities — belongs to
// capabilities.go (T021). This file is written first and is expected to fail to compile
// until that task lands, so the red is attributable to capabilities.go. It names no symbol
// owned by another core file.
//
// The client/server split (ClientCapabilities for the client's elicitation; ServerCapabilities
// for the server's tools/resources/prompts) follows contracts/core-api.md, whose shape the
// T021 task text names explicitly, and the empty elicitation object matches the
// wire-protocol.md Section 3 worked example. data-model.md Section 3's unified CapabilitySet
// is an earlier shape this package does not implement.

// Capability JSON member names are spelled out literally so that a change to a struct tag
// cannot silently relax these assertions (wire-protocol.md Section 3).
const (
	memberTools       = "tools"
	memberResources   = "resources"
	memberPrompts     = "prompts"
	memberElicitation = "elicitation"
	memberListChanged = "listChanged"
	memberSubscribe   = "subscribe"
	memberName        = "name"
	memberVersion     = "version"
	memberTitle       = "title"
)

// TestCapabilityObjectsRoundTrip pins the round-trip half of FR-014/V5 for every capability
// and implementation type: a value encoded to JSON and decoded back is unchanged, so a
// capability advertised by one side and read by the other cannot drift.
func TestCapabilityObjectsRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{"ClientCapabilities zero", ClientCapabilities{}},
		{"ClientCapabilities with elicitation", ClientCapabilities{Elicitation: &ElicitationCapability{}}},
		{"ServerCapabilities zero", ServerCapabilities{}},
		{"ServerCapabilities with every member", ServerCapabilities{
			Tools:     &ToolsCapability{ListChanged: true},
			Resources: &ResourcesCapability{Subscribe: true, ListChanged: true},
			Prompts:   &PromptsCapability{ListChanged: true},
		}},
		{"ToolsCapability", ToolsCapability{ListChanged: true}},
		{"ResourcesCapability", ResourcesCapability{Subscribe: true, ListChanged: true}},
		{"PromptsCapability", PromptsCapability{ListChanged: true}},
		{"ElicitationCapability", ElicitationCapability{}},
		{"Implementation", Implementation{Name: "echo-server", Version: "0.1.0"}},
		{"Implementation with title", Implementation{Name: "echo-server", Version: "0.1.0", Title: "Echo Server"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := roundTripJSON(t, tt.value)
			if !reflect.DeepEqual(got, tt.value) {
				t.Errorf("round trip of %s = %+v, want %+v", tt.name, got, tt.value)
			}
		})
	}
}

// TestAbsentCapabilityMemberIsNotOffered pins the V5 half of FR-014: a capability member that
// is absent means the feature is not offered. An empty set emits no member at all, decoding a
// member-less object yields nil members, and a set with a single member never invents the
// others.
func TestAbsentCapabilityMemberIsNotOffered(t *testing.T) {
	// An empty capability set advertises nothing.
	if members := jsonMembers(t, ServerCapabilities{}); len(members) != 0 {
		t.Errorf("ServerCapabilities{} encodes members %v, want none", keys(members))
	}
	if members := jsonMembers(t, ClientCapabilities{}); len(members) != 0 {
		t.Errorf("ClientCapabilities{} encodes members %v, want none", keys(members))
	}

	// Decoding a member-less object leaves every member nil — nothing offered.
	var server ServerCapabilities
	if err := json.Unmarshal([]byte(`{}`), &server); err != nil {
		t.Fatalf("decode {} into ServerCapabilities: %v", err)
	}
	if server.Tools != nil || server.Resources != nil || server.Prompts != nil {
		t.Errorf("decoding {} left a ServerCapabilities member non-nil: %+v", server)
	}
	var client ClientCapabilities
	if err := json.Unmarshal([]byte(`{}`), &client); err != nil {
		t.Fatalf("decode {} into ClientCapabilities: %v", err)
	}
	if client.Elicitation != nil {
		t.Errorf("decoding {} left ClientCapabilities.Elicitation non-nil: %+v", client)
	}

	// A single offered feature does not pull in the others.
	onlyTools := jsonMembers(t, ServerCapabilities{Tools: &ToolsCapability{}})
	if len(onlyTools) != 1 {
		t.Fatalf("ServerCapabilities with only tools encodes members %v, want exactly [tools]", keys(onlyTools))
	}
	if _, ok := onlyTools[memberTools]; !ok {
		t.Errorf("ServerCapabilities with only tools encodes %v, want member %q", keys(onlyTools), memberTools)
	}
}

// TestSubCapabilityFlagsAreAdditive pins the wire-protocol.md Section 3 rule that the
// sub-capability flags are additive: listChanged applies to tools, resources, and prompts;
// subscribe applies to resources; and each flag coexists with the other rather than
// replacing it.
func TestSubCapabilityFlagsAreAdditive(t *testing.T) {
	tests := []struct {
		name        string
		value       any
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:        "tools with listChanged",
			value:       ToolsCapability{ListChanged: true},
			wantPresent: []string{memberListChanged},
		},
		{
			name:       "tools with no flag",
			value:      ToolsCapability{},
			wantAbsent: []string{memberListChanged},
		},
		{
			name:        "resources with both flags",
			value:       ResourcesCapability{Subscribe: true, ListChanged: true},
			wantPresent: []string{memberSubscribe, memberListChanged},
		},
		{
			name:        "resources with only subscribe",
			value:       ResourcesCapability{Subscribe: true},
			wantPresent: []string{memberSubscribe},
			wantAbsent:  []string{memberListChanged},
		},
		{
			name:        "resources with only listChanged",
			value:       ResourcesCapability{ListChanged: true},
			wantPresent: []string{memberListChanged},
			wantAbsent:  []string{memberSubscribe},
		},
		{
			name:        "prompts with listChanged",
			value:       PromptsCapability{ListChanged: true},
			wantPresent: []string{memberListChanged},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			members := jsonMembers(t, tt.value)
			for _, m := range tt.wantPresent {
				if _, ok := members[m]; !ok {
					t.Errorf("%s encodes members %v, want member %q present", tt.name, keys(members), m)
				}
			}
			for _, m := range tt.wantAbsent {
				if _, ok := members[m]; ok {
					t.Errorf("%s encodes members %v, want member %q absent", tt.name, keys(members), m)
				}
			}
		})
	}
}

// TestElicitationCapabilityIsAnEmptyObject pins the client-side elicitation shape from the
// wire-protocol.md Section 3 example: present means offered, and it carries no members. This
// is the shape FR-002's client _meta relies on ("elicitation": {}).
func TestElicitationCapabilityIsAnEmptyObject(t *testing.T) {
	members := jsonMembers(t, ClientCapabilities{Elicitation: &ElicitationCapability{}})
	if len(members) != 1 {
		t.Fatalf("ClientCapabilities with elicitation encodes members %v, want exactly [elicitation]", keys(members))
	}
	raw, ok := members[memberElicitation]
	if !ok {
		t.Fatalf("member %q absent, want present", memberElicitation)
	}
	if string(raw) != `{}` {
		t.Errorf("elicitation encodes %s, want {}", raw)
	}
}

// TestImplementationMembers pins the identity object from core-api.md: name and version are
// always present; title is optional and omitted when empty (FR-028 implementation metadata).
func TestImplementationMembers(t *testing.T) {
	tests := []struct {
		name        string
		value       Implementation
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:        "name and version",
			value:       Implementation{Name: "echo-server", Version: "0.1.0"},
			wantPresent: []string{memberName, memberVersion},
			wantAbsent:  []string{memberTitle},
		},
		{
			name:        "with title",
			value:       Implementation{Name: "echo-server", Version: "0.1.0", Title: "Echo Server"},
			wantPresent: []string{memberName, memberVersion, memberTitle},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			members := jsonMembers(t, tt.value)
			for _, m := range tt.wantPresent {
				if _, ok := members[m]; !ok {
					t.Errorf("%s encodes members %v, want member %q present", tt.name, keys(members), m)
				}
			}
			for _, m := range tt.wantAbsent {
				if _, ok := members[m]; ok {
					t.Errorf("%s encodes members %v, want member %q absent", tt.name, keys(members), m)
				}
			}
		})
	}
}

// roundTripJSON marshals in, decodes it back into a fresh value of the same type, and returns
// the decoded value, so a caller can compare it with the original.
func roundTripJSON(t *testing.T, in any) any {
	t.Helper()

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal %T: %v", in, err)
	}
	out := reflect.New(reflect.TypeOf(in)).Interface()
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal %s into %T: %v", b, out, err)
	}
	return reflect.ValueOf(out).Elem().Interface()
}

// jsonMembers marshals v and returns its JSON object members keyed by name.
func jsonMembers(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil {
		t.Fatalf("unmarshal object %s: %v", b, err)
	}
	return members
}

// keys returns the member names of m in sorted order for stable error messages.
func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
