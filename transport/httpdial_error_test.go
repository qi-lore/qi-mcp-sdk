package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qi-lore/qi-mcp-sdk/core"
)

// These tests pin the response-body failure model of the Streamable HTTP dialer from
// contracts/transport.md Section 4 and docs/decisions/0010-streamable-http-dialer-failure.md
// (FR-010, R-017, R-019, V20), a facet decision 0009 left implicit.
//
// transport.md Section 1 requires a transport to return a typed transport error from
// Receive rather than block forever once it can no longer produce frames. For the dialer
// that moment is not a body's normal end (EOF: the one JSON frame is delivered, the SSE
// stream is finished), but a body that fails to read or exceeds the 4 MiB bound. Such a
// failure is transport-wide — the framed Receive interface carries no request identity to
// correlate it with — so the dialer fails terminally and every later Send and Receive
// returns a typed error matchable with errors.Is(err, core.ErrTransport).
//
// This file is written first and is expected to fail until httpdial.go (T039 rework) is
// updated: today a body read failure is swallowed and Receive blocks until the caller's
// own deadline, which these tests report as the failure.

// receiveErrorWithin issues one Receive under a deadline and returns its result. It is the
// error-expecting counterpart of httpdial_test.go's receiveFrame, which fails on any error.
func receiveErrorWithin(t *testing.T, tr core.Transport, d time.Duration) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return tr.Receive(ctx)
}

// assertTransportFailure asserts that err is a typed transport error the transport itself
// surfaced — not the caller's own receive deadline, which is how a dialer that blocks on a
// swallowed failure would report.
func assertTransportFailure(t *testing.T, frame []byte, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Receive returned frame %q and no error, want a transport failure", frame)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Receive blocked until the deadline instead of surfacing the failure: %v", err)
	}
	if !errors.Is(err, core.ErrTransport) {
		t.Errorf("error = %v, want a typed transport error (errors.Is(err, core.ErrTransport))", err)
	}
}

// TestStreamableHTTPDialSurfacesBodyReadFailure pins that a response body that fails to read
// — here a connection dropped mid-body — surfaces as a typed transport error from Receive
// instead of blocking, and that the failure is transport-wide: a later Send is refused with
// the same typed error (transport.md Section 1).
func TestStreamableHTTPDialSurfacesBodyReadFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("the response writer does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Announce a JSON body far larger than what is sent, then drop the connection: the
		// client reads the header, then fails reading the truncated body.
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{\"jsonrpc\":")
	}))
	defer srv.Close()

	tr := dial(t, srv.URL)
	sendFrame(t, tr, dialRequestFrame)

	frame, err := receiveErrorWithin(t, tr, 2*time.Second)
	assertTransportFailure(t, frame, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if sendErr := tr.Send(ctx, []byte(dialRequestFrame)); !errors.Is(sendErr, core.ErrTransport) {
		t.Errorf("Send after a body failure = %v, want a typed transport error", sendErr)
	}
}

// TestStreamableHTTPDialSurfacesOversizeJSONBody pins that a JSON response body over the
// 4 MiB bound is a typed transport failure rather than a silently dropped frame (R-019,
// transport.md Section 4 Bounds).
func TestStreamableHTTPDialSurfacesOversizeJSONBody(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":"` + strings.Repeat("x", (4<<20)+64) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	tr := dial(t, srv.URL)
	sendFrame(t, tr, dialRequestFrame)

	frame, err := receiveErrorWithin(t, tr, 5*time.Second)
	assertTransportFailure(t, frame, err)
}

// TestStreamableHTTPDialSurfacesOversizeSSEEvent pins that an SSE event over the 4 MiB
// bound — the reader's own typed error (transport.md Section 5) — is surfaced as a typed
// transport failure rather than swallowed.
func TestStreamableHTTPDialSurfacesOversizeSSEEvent(t *testing.T) {
	event := "data: " + strings.Repeat("x", (4<<20)+1) + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, event)
	}))
	defer srv.Close()

	tr := dial(t, srv.URL)
	sendFrame(t, tr, dialRequestFrame)

	frame, err := receiveErrorWithin(t, tr, 5*time.Second)
	assertTransportFailure(t, frame, err)
}
