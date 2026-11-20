package core

// Era selects the protocol era for a session. Selection is explicit and is never
// inferred from observed traffic. The zero value is the modern era (FR-051, R-004).
type Era int

const (
	// EraModern selects MCP 2026-07-28: per-request _meta, server/discover, and no
	// initialize handshake or session identifier. It is the default and primary era.
	EraModern Era = iota
	// EraLegacy selects MCP 2025-11-25: the initialize/initialized handshake and a
	// server-issued session identifier. It is enabled only by explicit selection.
	EraLegacy
)

// String returns "modern" or "legacy" so diagnostics and the NegotiationError
// message stay readable (FR-051).
func (e Era) String() string {
	switch e {
	case EraModern:
		return "modern"
	case EraLegacy:
		return "legacy"
	default:
		return "unknown"
	}
}

// Protocol revisions supported by this release (FR-005).
const (
	// ProtocolVersionModern is the primary and default revision.
	ProtocolVersionModern = "2026-07-28"
	// ProtocolVersionLegacy is the single supported legacy revision.
	ProtocolVersionLegacy = "2025-11-25"
)

// SupportedVersions returns the revisions this SDK accepts for an era. For EraModern
// it returns {ProtocolVersionModern}; for EraLegacy it returns {ProtocolVersionLegacy}.
// The sets are disjoint, so a revision observed on the wire can never be ambiguous
// (FR-005, FR-051, V12, V13).
func SupportedVersions(era Era) []string {
	switch era {
	case EraLegacy:
		return []string{ProtocolVersionLegacy}
	default:
		return []string{ProtocolVersionModern}
	}
}

// VersionNegotiation records how the active revision was chosen (FR-005, FR-054).
// It is a record of the outcome, not a policy: negotiation selects exactly one
// revision per connection.
type VersionNegotiation struct {
	Era       Era
	Selected  string   // the revision in force
	Supported []string // the local supported set
	Requested string   // what the peer asked for
	Method    string   // "server/discover" (modern) or "initialize" (legacy)
}
