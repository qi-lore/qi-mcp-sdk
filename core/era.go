package core

// Era selects the protocol era for a session. Selection is explicit and is never
// inferred from observed traffic. The zero value is the modern era (FR-051, R-004).
//
// The era constants (EraModern, EraLegacy), the String method, the protocol-revision
// constants, SupportedVersions, and the VersionNegotiation record are added by the
// era task (T017); this declaration exists so the error taxonomy that references Era
// — NegotiationError — compiles.
type Era int
