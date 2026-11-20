package core

import "encoding/json"

// This file implements the handler payloads and shared value types from data-model.md
// Section 6b: the decoded requests a server hands to the tool, resource, and prompt
// handlers (FR-015) and the tool result those handlers return (FR-013). The registration
// surface that names the handlers lives in `server`; this file owns only the payload
// shapes and the shared ContentBlock, so a handler and the wire contract cannot drift
// apart.

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
