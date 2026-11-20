package core

import (
	"context"
	"log/slog"
)

// This file implements the shared interface set from contracts/core-api.md ("Shared
// interfaces"): the contracts every other package in the SDK programs against. They live
// in core, and only in core, so that client and server can accept a transport, middleware
// hook, token source, token validator, or logger by injection while importing core alone —
// the R-003 dependency rule (Constitution IV, FR-019, FR-024, FR-025, FR-057).
//
// Every blocking method takes context.Context as its first parameter (NFR-004). The
// concrete implementations live elsewhere: transport provides Transport, middleware
// provides Hook, auth provides TokenValidator (and its TokenValidator is what the client
// uses to obtain tokens), and the caller supplies the Logger. This file defines the
// contracts and, for logging, the one standard-library adapter the whole SDK shares.

// Transport is a bidirectional, framed channel carrying JSON-RPC envelopes (FR-019,
// R-003). Only core defines this interface; client and server program against it, and
// transport provides the implementations. Frames are opaque []byte so that every adapter —
// stdio, Streamable HTTP, and the out-of-tree gRPC and NATS adapters — forwards MCP
// envelopes verbatim without the interface taking a message-typing decision.
//
// A Transport is safe for concurrent use: Send calls are serialized so frames cannot
// interleave, and Receive may be called from at most one goroutine at a time. The transport
// owns its internal goroutines and releases them on Close or on context cancellation
// (R-017, V20).
type Transport interface {
	// Send writes one complete JSON-RPC frame, blocking until it is accepted or ctx is
	// done.
	Send(ctx context.Context, frame []byte) error
	// Receive blocks until the next complete frame is available or ctx is done. It
	// returns a typed transport error — matchable with errors.Is(err, ErrTransport) —
	// after the channel is closed.
	Receive(ctx context.Context) (frame []byte, err error)
	// Close releases resources and unblocks pending Send and Receive calls. It is
	// idempotent.
	Close() error
}

// Handler processes one decoded request and produces a response or a typed error
// (FR-015). A non-nil error is converted to a JSON-RPC error response by the caller; the
// method-not-found case is reported with an error matchable by
// errors.Is(err, ErrMethodNotFound). The server package's *Server implements Handler, so a
// transport dispatches to the server through this interface.
//
// A Handler must be safe to call concurrently when its caller dispatches concurrently; the
// implementation documents its own concurrency safety.
type Handler interface {
	// Handle processes req and returns the response to send, or an error.
	Handle(ctx context.Context, req *Request) (*Response, error)
}

// Hook is one ordered middleware stage (FR-024, R-009). A Hook receives the next Handler
// and returns a Handler, so a chain composes by nesting: the outermost hook's request-side
// logic runs first and its response-side logic runs last. A Hook may reject by returning a
// typed error without calling next, which short-circuits dispatch and prevents the wrapped
// handler from running. Hooks must not implicitly mutate request _meta; a hook that intends
// to add or change _meta does so explicitly.
//
// A nil-safe, stateless Hook — one holding no mutable state — is safe for concurrent use.
type Hook interface {
	// Intercept wraps next and returns the Handler to dispatch through. Request-side
	// logic runs before next; response-side logic runs after it returns.
	Intercept(next Handler) Handler
}

// TokenSource supplies a bearer token for outbound HTTP requests (FR-011). The client
// installs one on a Streamable HTTP transport. resource is the RFC 8707 resource the token
// is scoped to, so an implementation can select an audience-appropriate token rather than a
// single global credential.
//
// A TokenSource is safe for concurrent use. The returned error must not embed the token
// value (FR-027).
type TokenSource interface {
	// Token returns a bearer token scoped to resource, or an error.
	Token(ctx context.Context, resource string) (string, error)
}

// TokenValidator validates an inbound bearer token for an endpoint (FR-025). It is the
// pluggable contract the auth middleware wraps and the auth package's default validator
// implements; a caller needing a different strategy — for example RFC 7662 introspection —
// supplies its own. resource is the RFC 8707 resource the endpoint serves, matched against
// the token's audience.
//
// A TokenValidator is safe for concurrent use. The returned error is a typed auth error
// matchable with errors.Is(err, ErrAuth), and implementations must not include the token
// value in it (FR-027).
type TokenValidator interface {
	// Validate checks token against resource and returns nil when it is acceptable.
	Validate(ctx context.Context, token string, resource string) error
}

// Logger is the injected diagnostics contract, expressed in terms of log/slog (FR-057). It
// is the only observability contract in the first release. The SDK is silent unless a
// Logger is supplied, and it never writes diagnostics directly to standard output or
// standard error — under stdio, stdout carries the protocol, so a stray write would corrupt
// the stream (NFR-011, V19).
//
// Implementations must be safe for concurrent use and must not receive credential material
// (FR-027).
type Logger interface {
	// Enabled reports whether a record at level would be emitted.
	Enabled(ctx context.Context, level slog.Level) bool
	// Log emits record.
	Log(ctx context.Context, record slog.Record)
}

// SlogLogger adapts a *slog.Logger to the Logger contract (FR-057). It is the adapter the
// SDK's own components use to reach a caller's sink, and the one place the slog dependency
// is bound.
//
// The adapter gates on the wrapped logger's own enablement: Enabled mirrors
// (*slog.Logger).Enabled, and Log first consults it and drops the record when the level is
// disabled, delivering through the handler only what the wrapped logger would emit. This is
// the mechanism behind the SDK's silence by default (V19, NFR-011): a caller who supplies a
// disabled logger — or, as the common case, no logger at all — gets no output.
//
// l must be non-nil. The returned Logger is safe for concurrent use exactly as the wrapped
// *slog.Logger is.
func SlogLogger(l *slog.Logger) Logger {
	return slogLogger{logger: l}
}

// slogLogger is the unexported adapter SlogLogger returns: it forwards the Logger contract
// to a *slog.Logger, checking enablement before emitting so the wrapper never writes a
// record the wrapped logger's handler would have filtered out.
type slogLogger struct {
	logger *slog.Logger
}

// Enabled delegates to the wrapped logger, so the adapter reports enablement for the level
// rather than a constant.
func (s slogLogger) Enabled(ctx context.Context, level slog.Level) bool {
	return s.logger.Enabled(ctx, level)
}

// Log delivers record through the wrapped logger's handler only when the record's level is
// enabled, and otherwise emits nothing (V19). It goes through Handler rather than a
// level-and-attributes entry point because the Logger contract carries a whole slog.Record,
// which has no equivalent on *slog.Logger.
func (s slogLogger) Log(ctx context.Context, record slog.Record) {
	if !s.logger.Enabled(ctx, record.Level) {
		return
	}
	// The Logger contract has no error result, so a handler error is intentionally
	// dropped: diagnostics must never affect control flow.
	_ = s.logger.Handler().Handle(ctx, record)
}
