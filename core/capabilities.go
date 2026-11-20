package core

// This file implements the capability and implementation types from core-api.md
// ("Capability and implementation types") — the declared feature sets a client and a server
// advertise (FR-002, FR-014). The representation carries the V5 rule: an absent member means
// the feature is not offered, so an unadvertised operation is never dispatched as if present
// (FR-014, FR-018). The sub-capability flags listChanged and subscribe are additive.

// Implementation identifies a client or server (FR-002, FR-028). Title is an optional
// human-readable display label and is omitted from the wire when empty.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

// ElicitationCapability advertises client support for elicitation. It carries no members:
// presence alone means the feature is offered (wire-protocol.md Section 3).
type ElicitationCapability struct{}

// ClientCapabilities is the client's declared feature set, carried in every request's
// io.modelcontextprotocol/clientCapabilities _meta member (FR-002). A nil member means the
// feature is not offered.
type ClientCapabilities struct {
	Elicitation *ElicitationCapability `json:"elicitation,omitempty"`
}

// ToolsCapability is the server's tools sub-capability (FR-014). ListChanged reports whether
// the server offers change notifications for its tool list.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability is the server's resources sub-capability (FR-014). Subscribe and
// ListChanged are additive: either, both, or neither may be set, and setting one never
// implies the other.
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// PromptsCapability is the server's prompts sub-capability (FR-014). ListChanged reports
// whether the server offers change notifications for its prompt list.
type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ServerCapabilities is the server's advertised feature set, returned in server/discover and
// initialize results (FR-014). An absent member means the feature is not offered.
type ServerCapabilities struct {
	Tools     *ToolsCapability     `json:"tools,omitempty"`
	Resources *ResourcesCapability `json:"resources,omitempty"`
	Prompts   *PromptsCapability   `json:"prompts,omitempty"`
}
