package core

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

// These tests pin the handler payloads in data-model.md Section 6b: the types the
// registration handlers of `server` are declared to take and return
// (`ToolHandler func(ctx, *ToolCall) (*ToolResult, error)`, and its resource and prompt
// counterparts in contracts/core-api.md), plus the shared ContentBlock and the
// TextContent constructor (FR-013, FR-015).
//
// The symbols exercised here — ToolCall, ToolResult, ContentBlock, TextContent,
// ResourceRead, and PromptGet — belong to payload.go (T031). This file is written first and
// is expected to fail to compile until that task lands, so the red is attributable to
// payload.go. It names no symbol owned by another core file: the `_meta` members it reads
// are typed *RequestMeta (meta.go, T016) and the `resultType` member is typed ResultType
// (result.go, T023), but both are referenced only through the payload struct fields this
// file is pinning, exactly as meta_test.go reaches ClientCapabilities transitively.
//
// The shapes are asserted from JSON literals rather than from the SDK's own encoders, so a
// struct tag and the wire contract cannot drift together unnoticed.

// The declarations below pin the Section 6b field names and Go types at compile time: a
// renamed, retyped, or dropped field is a compile error in this file, not a silently
// ignored runtime difference.
var (
	_ = ToolCall{Name: "", Arguments: json.RawMessage(nil), Meta: (*RequestMeta)(nil)}
	_ = ToolResult{
		Content:           []ContentBlock{},
		StructuredContent: json.RawMessage(nil),
		IsError:           false,
		ResultType:        ResultComplete,
		InputRequests:     []InputRequest{},
		RequestState:      (*RequestState)(nil),
	}
	_ = ContentBlock{Type: "", Text: "", Data: "", MimeType: "", URI: ""}
	_ = ResourceRead{URI: "", Meta: (*RequestMeta)(nil)}
	_ = PromptGet{Name: "", Arguments: map[string]string(nil), Meta: (*RequestMeta)(nil)}

	// TextContent is a constructor, not a method: callers build the common block without
	// hand-writing the discriminator (data-model.md Section 6b).
	_ func(string) ContentBlock = TextContent
)

// TestToolCallWireShapeMatchesDataModel pins the decoded invocation a ToolHandler receives
// (data-model.md Section 6b): `name`, `arguments` kept as raw JSON so the server can
// validate it against InputSchema before the handler runs, and the optional `_meta` that
// every modern-era request carries (FR-002).
func TestToolCallWireShapeMatchesDataModel(t *testing.T) {
	const call = `{"name":"echo","arguments":{"text":"hi"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`

	var got ToolCall
	if err := json.Unmarshal([]byte(call), &got); err != nil {
		t.Fatalf("decode tool call %s: %v", call, err)
	}
	if got.Name != "echo" {
		t.Errorf("Name = %q, want %q", got.Name, "echo")
	}
	const wantArguments = `{"text":"hi"}`
	if string(got.Arguments) != wantArguments {
		t.Errorf("Arguments = %s, want %s (arguments must stay raw for InputSchema validation)", got.Arguments, wantArguments)
	}
	if got.Meta == nil {
		t.Fatal("Meta = nil, want the decoded _meta: a modern-era request carries it in params._meta")
	}
	if want := "2026-07-28"; got.Meta.ProtocolVersion != want {
		t.Errorf("Meta.ProtocolVersion = %q, want %q", got.Meta.ProtocolVersion, want)
	}
}

// TestPayloadMetaIsOptionalOnTheWire pins the one optional member the four payload types
// share: `_meta` is present when the request carries it and absent — not null — when it
// does not, because legacy mode has none (FR-051, data-model.md Section 6b constraint).
func TestPayloadMetaIsOptionalOnTheWire(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  []string
	}{
		{"tool call", ToolCall{Name: "echo", Arguments: json.RawMessage(`{}`)}, []string{"arguments", "name"}},
		{"resource read", ResourceRead{URI: "file:///tmp/a.txt"}, []string{"uri"}},
		{"prompt get", PromptGet{Name: "summarize"}, []string{"name"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jsonMemberNames(t, tt.value); !slices.Equal(got, tt.want) {
				t.Errorf("encodes members %v, want %v (an absent _meta must be omitted, not encoded as null)", got, tt.want)
			}
		})
	}
}

// TestToolResultWireShapeMatchesDataModel pins what a ToolHandler returns (data-model.md
// Section 6b): the mandatory `content` array, the optional `structuredContent`, `isError`,
// `resultType`, and the MRTR pair `inputRequests`/`requestState` that is populated only
// when the handler asks for client input (FR-006, FR-007).
func TestToolResultWireShapeMatchesDataModel(t *testing.T) {
	const body = `{"content":[{"type":"text","text":"sent"}],` +
		`"structuredContent":{"id":"msg-1"},` +
		`"isError":true,` +
		`"resultType":"input_required",` +
		`"inputRequests":[{"id":"confirm-1","method":"elicitation/create","params":{"message":"Send?"}}],` +
		`"requestState":{"step":2}}`

	var got ToolResult
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode tool result %s: %v", body, err)
	}

	if len(got.Content) != 1 {
		t.Fatalf("Content has %d blocks, want 1", len(got.Content))
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "sent" {
		t.Errorf("Content[0] = %+v, want a text block reading %q", got.Content[0], "sent")
	}
	const wantStructured = `{"id":"msg-1"}`
	if string(got.StructuredContent) != wantStructured {
		t.Errorf("StructuredContent = %s, want %s", got.StructuredContent, wantStructured)
	}
	if !got.IsError {
		t.Error("IsError = false, want true")
	}
	if got.ResultType != ResultInputRequired {
		t.Errorf("ResultType = %q, want %q", got.ResultType, ResultInputRequired)
	}
	if len(got.InputRequests) != 1 || got.InputRequests[0].ID != "confirm-1" {
		t.Errorf("InputRequests = %+v, want one request with id %q", got.InputRequests, "confirm-1")
	}
	if got.RequestState == nil {
		t.Fatal("RequestState = nil, want the decoded state")
	}
	if want := `{"step":2}`; string(got.RequestState.Value) != want {
		t.Errorf("RequestState.Value = %s, want %s", got.RequestState.Value, want)
	}
}

// TestToolResultAlwaysCarriesContentMember pins the "at least one block" constraint of
// data-model.md Section 6b at the level this type can enforce: `content` is the one member
// a ToolResult must always encode, so a result can never reach the wire with no place to
// put its blocks. Every optional member — the MRTR pair, structuredContent, isError, and
// the resultType discriminator — is omitted when empty, so a complete result is `content`
// alone.
func TestToolResultAlwaysCarriesContentMember(t *testing.T) {
	if got := jsonMemberNames(t, ToolResult{}); !slices.Equal(got, []string{"content"}) {
		t.Errorf("zero ToolResult encodes members %v, want [content]: the content member is mandatory and every other member is optional", got)
	}

	// A result that does carry blocks round-trips them in order, so the mandatory member is
	// a real array and not a placeholder.
	want := []ContentBlock{TextContent("first"), TextContent("second")}
	encoded, err := json.Marshal(ToolResult{Content: want})
	if err != nil {
		t.Fatalf("encode ToolResult: %v", err)
	}

	var back ToolResult
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("decode ToolResult %s: %v", encoded, err)
	}
	if len(back.Content) != len(want) {
		t.Fatalf("Content has %d blocks, want %d (encoded %s)", len(back.Content), len(want), encoded)
	}
	for i, block := range back.Content {
		if block != want[i] {
			t.Errorf("Content[%d] = %+v, want %+v", i, block, want[i])
		}
	}
}

// TestContentBlockVariantsWireShape pins the ContentBlock fields of data-model.md
// Section 6b as the documented variants use them: only the members meaningful for `type`
// are set, and the rest are omitted rather than encoded as empty strings.
func TestContentBlockVariantsWireShape(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantBlock   ContentBlock
		wantMembers []string
	}{
		{
			name:        "text",
			body:        `{"type":"text","text":"hello"}`,
			wantBlock:   ContentBlock{Type: "text", Text: "hello"},
			wantMembers: []string{"text", "type"},
		},
		{
			name:        "image",
			body:        `{"type":"image","data":"iVBORw0KGgo=","mimeType":"image/png"}`,
			wantBlock:   ContentBlock{Type: "image", Data: "iVBORw0KGgo=", MimeType: "image/png"},
			wantMembers: []string{"data", "mimeType", "type"},
		},
		{
			name:        "resource",
			body:        `{"type":"resource","uri":"file:///tmp/a.txt","mimeType":"text/plain"}`,
			wantBlock:   ContentBlock{Type: "resource", URI: "file:///tmp/a.txt", MimeType: "text/plain"},
			wantMembers: []string{"mimeType", "type", "uri"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got ContentBlock
			if err := json.Unmarshal([]byte(tt.body), &got); err != nil {
				t.Fatalf("decode content block %s: %v", tt.body, err)
			}
			if got != tt.wantBlock {
				t.Errorf("decoded %+v, want %+v", got, tt.wantBlock)
			}

			// Re-encoding the decoded block reproduces the wire members exactly: no vacant
			// optional field leaks into the output.
			if members := jsonMemberNames(t, got); !slices.Equal(members, tt.wantMembers) {
				t.Errorf("encodes members %v, want %v", members, tt.wantMembers)
			}
		})
	}
}

// TestTextContentBuildsTextBlock pins the constructor and its exact encoding, which is the
// example wire-protocol.md Section 3 shows: `{"type":"text","text":...}`. Hand-building the
// block is exactly what the constructor exists to prevent, so its discriminator must be
// right by construction (FR-013).
func TestTextContentBuildsTextBlock(t *testing.T) {
	block := TextContent("hello")

	if block.Type != "text" {
		t.Errorf("Type = %q, want %q", block.Type, "text")
	}
	if block.Text != "hello" {
		t.Errorf("Text = %q, want %q", block.Text, "hello")
	}

	encoded, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("encode TextContent result: %v", err)
	}
	if got, want := string(encoded), `{"type":"text","text":"hello"}`; got != want {
		t.Errorf("TextContent(\"hello\") encodes %s, want %s", got, want)
	}
}

// TestResourceReadWireShapeMatchesDataModel pins the decoded read a ResourceHandler
// receives (data-model.md Section 6b): the `uri` the client asked for, which the server
// resolved against a registered resource or resource template (FR-013, FR-015).
func TestResourceReadWireShapeMatchesDataModel(t *testing.T) {
	const read = `{"uri":"file:///tmp/a.txt"}`

	var got ResourceRead
	if err := json.Unmarshal([]byte(read), &got); err != nil {
		t.Fatalf("decode resource read %s: %v", read, err)
	}
	if want := "file:///tmp/a.txt"; got.URI != want {
		t.Errorf("URI = %q, want %q", got.URI, want)
	}
}

// TestPromptGetWireShapeMatchesDataModel pins the decoded get a PromptHandler receives
// (data-model.md Section 6b): the prompt `name` and its already-substituted `arguments`,
// which are strings because prompt arguments are textual (FR-013, FR-015).
func TestPromptGetWireShapeMatchesDataModel(t *testing.T) {
	const get = `{"name":"summarize","arguments":{"text":"hi","style":"terse"}}`

	var got PromptGet
	if err := json.Unmarshal([]byte(get), &got); err != nil {
		t.Fatalf("decode prompt get %s: %v", get, err)
	}
	if got.Name != "summarize" {
		t.Errorf("Name = %q, want %q", got.Name, "summarize")
	}
	want := map[string]string{"text": "hi", "style": "terse"}
	if !maps.Equal(got.Arguments, want) {
		t.Errorf("Arguments = %v, want %v", got.Arguments, want)
	}
}
