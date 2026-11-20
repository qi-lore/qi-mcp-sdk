package core

import "encoding/json"

// This file implements the handler payloads and shared value types from data-model.md
// Section 6b: the decoded requests a server hands to the tool, resource, and prompt
// handlers (FR-015) and the results those handlers return (FR-013). The resource and prompt
// results follow the authoritative MCP `2026-07-28` `resources/read` and `prompts/get`
// result objects, because §6b records them only in prose. The registration surface that
// names the handlers lives in `server`; this file owns only the payload shapes and the
// shared ContentBlock, so a handler and the wire contract cannot drift apart.

// ToolCall is the decoded invocation passed to a ToolHandler. Name must match a registered
// tool and Arguments is kept as raw JSON so the server can validate it against the tool's
// InputSchema before the handler runs (FR-008); the handler decodes it itself.
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      *RequestMeta    `json:"_meta,omitempty"`
}

// ToolResult is what a ToolHandler returns (FR-013). Content carries at least one block;
// the MRTR members — ResultType, InputRequests, and RequestState — are populated only when
// the handler asks the client for input and the flow must resume (FR-006, FR-007). An
// empty ResultType means a complete result.
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type ToolResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
	ResultType        ResultType      `json:"resultType,omitempty"`
	InputRequests     []InputRequest  `json:"inputRequests,omitempty"`
	RequestState      *RequestState   `json:"requestState,omitempty"`
}

// ContentBlock is one returned content item; Type discriminates the variant and only the
// fields meaningful for Type are set (data-model.md Section 6b). A text block uses Type and
// Text; an image block uses Type, Data, and MimeType; a resource block uses Type, URI, and
// MimeType. Vacant optional members are omitted rather than encoded as empty strings.
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// TextContent returns a text ContentBlock. It exists so callers never hand-build the most
// common block and get the discriminator wrong (data-model.md Section 6b).
func TextContent(text string) ContentBlock {
	return ContentBlock{Type: "text", Text: text}
}

// ResourceRead is the decoded request passed to a ResourceHandler: the URI the client asked
// for, which the server resolved against a registered resource or resource template before
// dispatch (FR-013, FR-015).
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type ResourceRead struct {
	URI  string       `json:"uri"`
	Meta *RequestMeta `json:"_meta,omitempty"`
}

// PromptGet is the decoded request passed to a PromptHandler: the prompt Name and its
// already-substituted Arguments. Arguments are strings because prompt arguments are textual
// (data-model.md Section 6b).
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type PromptGet struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments,omitempty"`
	Meta      *RequestMeta      `json:"_meta,omitempty"`
}

// ResourceContent is one item of a resource read (FR-013, FR-015). It mirrors the
// authoritative MCP `2026-07-28` `resources/read` contents entry: URI identifies the
// resource, MimeType is its declared media type when known, and exactly the applicable one
// of Text (inline UTF-8 text) or Blob (base64-encoded binary) carries the data. A vacant
// member is omitted rather than encoded as an empty string.
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// ResourceResult is what a ResourceHandler returns (FR-013, FR-015). Contents carries the
// resource contents read; a single read may return more than one item, for example when the
// resolved resource is a directory. When the handler cannot finish without client input it
// marks the result input_required and fills the MRTR members instead, reusing the same trio
// as ToolResult (FR-006, FR-007).
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type ResourceResult struct {
	Contents      []ResourceContent `json:"contents"`
	ResultType    ResultType        `json:"resultType,omitempty"`
	InputRequests []InputRequest    `json:"inputRequests,omitempty"`
	RequestState  *RequestState     `json:"requestState,omitempty"`
}

// PromptMessage is one message of a rendered prompt (FR-013, FR-015). Role is the speaker —
// "user" or "assistant" in the authoritative MCP `2026-07-28` revision — and Content is the
// message body, a shared ContentBlock. Role is not validated here; the SDK does not rewrite
// or normalize what a server supplies.
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type PromptMessage struct {
	Role    string       `json:"role"`
	Content ContentBlock `json:"content"`
}

// PromptResult is what a PromptHandler returns (FR-013, FR-015). Description is optional
// human-readable context and Messages is the rendered conversation. As with ResourceResult,
// an unfinished prompt reports input_required and fills the shared MRTR members
// (FR-006, FR-007).
//
// It is not safe for concurrent mutation; the caller owns its lifetime.
type PromptResult struct {
	Description   string          `json:"description,omitempty"`
	Messages      []PromptMessage `json:"messages"`
	ResultType    ResultType      `json:"resultType,omitempty"`
	InputRequests []InputRequest  `json:"inputRequests,omitempty"`
	RequestState  *RequestState   `json:"requestState,omitempty"`
}
