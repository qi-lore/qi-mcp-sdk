package core

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// These tests pin the shared interface set that core owns (R-003, core-api.md "Shared
// interfaces", FR-019, FR-024, FR-025, FR-057) and the logging silence rule (V19):
//
//  1. Compile-time conformance of Transport, Handler, Hook, TokenSource, TokenValidator,
//     and Logger, plus SlogLogger's exact signature. Every symbol exercised here — the six
//     interfaces and SlogLogger — belongs to interfaces.go (T027). This file is written
//     first and is expected to fail to compile until that task lands, so the red is
//     attributable to interfaces.go alone; apart from those symbols it names only the
//     already-landed Request and Response.
//
//  2. SlogLogger emits nothing through a wrapped logger whose handler reports the level
//     disabled. That is the mechanism by which the SDK stays silent unless the caller
//     supplies an enabled sink — it never writes diagnostics to standard output or
//     standard error on its own (FR-057, NFR-011, V19).

// Compile-time conformance. The method-expression pins fix each interface method's exact
// signature — receiver, parameter types, and results — and the fake values prove a concrete
// type with that method set satisfies the interface. A signature drift in interfaces.go
// fails the build here rather than surfacing as a runtime surprise in transport, middleware,
// auth, client, or server. SlogLogger's own type is pinned the same way.
var (
	_ func(Transport, context.Context, []byte) error   = Transport.Send
	_ func(Transport, context.Context) ([]byte, error) = Transport.Receive
	_ func(Transport) error                            = Transport.Close

	_ func(Handler, context.Context, *Request) (*Response, error) = Handler.Handle

	_ func(Hook, Handler) Handler = Hook.Intercept

	_ func(TokenSource, context.Context, string) (string, error) = TokenSource.Token

	_ func(TokenValidator, context.Context, string, string) error = TokenValidator.Validate

	_ func(Logger, context.Context, slog.Level) bool = Logger.Enabled
	_ func(Logger, context.Context, slog.Record)     = Logger.Log

	_ func(*slog.Logger) Logger = SlogLogger

	_ Transport      = fakeTransport{}
	_ Handler        = fakeHandler{}
	_ Hook           = fakeHook{}
	_ TokenSource    = fakeTokenSource{}
	_ TokenValidator = fakeTokenValidator{}
	_ Logger         = fakeLogger{}
)

// fakeTransport is a minimal Transport; it exists only to assert interface conformance.
type fakeTransport struct{}

func (fakeTransport) Send(context.Context, []byte) error      { return nil }
func (fakeTransport) Receive(context.Context) ([]byte, error) { return nil, nil }
func (fakeTransport) Close() error                            { return nil }

// fakeHandler is a minimal Handler; it exists only to assert interface conformance.
type fakeHandler struct{}

func (fakeHandler) Handle(context.Context, *Request) (*Response, error) { return nil, nil }

// fakeHook is a minimal Hook; it exists only to assert interface conformance.
type fakeHook struct{}

func (fakeHook) Intercept(next Handler) Handler { return next }

// fakeTokenSource is a minimal TokenSource; it exists only to assert interface conformance.
type fakeTokenSource struct{}

func (fakeTokenSource) Token(context.Context, string) (string, error) { return "", nil }

// fakeTokenValidator is a minimal TokenValidator; it exists only to assert interface conformance.
type fakeTokenValidator struct{}

func (fakeTokenValidator) Validate(context.Context, string, string) error { return nil }

// fakeLogger is a minimal Logger; it exists only to assert interface conformance.
type fakeLogger struct{}

func (fakeLogger) Enabled(context.Context, slog.Level) bool { return false }
func (fakeLogger) Log(context.Context, slog.Record)         {}

// TestSlogLoggerEmitsNothingWhenDisabled pins the V19 silence rule for the adapter: when the
// wrapped logger's handler reports the record's level disabled, SlogLogger reports Enabled
// false and writes nothing to the handler, so a caller who supplies a disabled logger gets
// no output at all.
func TestSlogLoggerEmitsNothingWhenDisabled(t *testing.T) {
	handler := &recordingHandler{disabled: true}
	logger := SlogLogger(slog.New(handler))
	ctx := context.Background()

	if logger.Enabled(ctx, slog.LevelInfo) {
		t.Error("SlogLogger.Enabled(Info) = true for a disabled wrapped logger, want false")
	}
	if logger.Enabled(ctx, slog.LevelError) {
		t.Error("SlogLogger.Enabled(Error) = true for a disabled wrapped logger, want false")
	}

	logger.Log(ctx, slog.NewRecord(time.Time{}, slog.LevelInfo, "must not be emitted", 0))
	logger.Log(ctx, slog.NewRecord(time.Time{}, slog.LevelError, "must not be emitted either", 0))

	if got := handler.count(); got != 0 {
		t.Errorf("SlogLogger emitted %d record(s) through a disabled logger, want 0", got)
	}
}

// TestSlogLoggerMirrorsWrappedEnabled pins that SlogLogger.Enabled is not a constant: it
// reflects the wrapped logger's own enablement for each level. Without this, the silence test
// above could pass for an adapter that simply never emits.
func TestSlogLoggerMirrorsWrappedEnabled(t *testing.T) {
	// A handler that enables only Warn and above.
	handler := &recordingHandler{minLevel: slog.LevelWarn}
	logger := SlogLogger(slog.New(handler))
	ctx := context.Background()

	tests := []struct {
		level slog.Level
		want  bool
	}{
		{slog.LevelDebug, false},
		{slog.LevelInfo, false},
		{slog.LevelWarn, true},
		{slog.LevelError, true},
	}
	for _, tt := range tests {
		if got := logger.Enabled(ctx, tt.level); got != tt.want {
			t.Errorf("SlogLogger.Enabled(%v) = %v, want %v", tt.level, got, tt.want)
		}
	}
}

// TestSlogLoggerDeliversEnabledRecordsAndGatesByLevel pins the positive half of the adapter:
// an enabled record is delivered to the wrapped handler with its message and level intact,
// and a record below the handler's level is dropped. Together with the silence test this
// shows the adapter gates on enablement rather than either always writing or never writing.
func TestSlogLoggerDeliversEnabledRecordsAndGatesByLevel(t *testing.T) {
	handler := &recordingHandler{minLevel: slog.LevelWarn}
	logger := SlogLogger(slog.New(handler))
	ctx := context.Background()

	logger.Log(ctx, slog.NewRecord(time.Time{}, slog.LevelInfo, "below the level", 0))
	if got := handler.count(); got != 0 {
		t.Fatalf("below-level record was delivered (%d record(s)), want 0", got)
	}

	logger.Log(ctx, slog.NewRecord(time.Time{}, slog.LevelError, "observable", 0))
	records := handler.records()
	if len(records) != 1 {
		t.Fatalf("enabled record delivery count = %d, want 1", len(records))
	}
	if got := records[0].Message; got != "observable" {
		t.Errorf("delivered record message = %q, want %q", got, "observable")
	}
	if got := records[0].Level; got != slog.LevelError {
		t.Errorf("delivered record level = %v, want %v", got, slog.LevelError)
	}
}

// recordingHandler is a slog.Handler that records every record handed to Handle. When
// disabled is set, Enabled reports false for every level; otherwise it enables minLevel and
// above. It deliberately does not re-check Enabled inside Handle, so it detects an adapter
// that bypasses the enablement check and writes to the handler directly.
type recordingHandler struct {
	disabled bool
	minLevel slog.Level
	received []slog.Record
}

func (h *recordingHandler) Enabled(_ context.Context, level slog.Level) bool {
	if h.disabled {
		return false
	}
	return level >= h.minLevel
}

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.received = append(h.received, r)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// records returns the records delivered to Handle.
func (h *recordingHandler) records() []slog.Record { return h.received }

// count returns how many records were delivered to Handle.
func (h *recordingHandler) count() int { return len(h.received) }
