package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qi-lore/qi-mcp-sdk/core"
)

// These tests pin the client side of the Streamable HTTP binding from transport.md
// Section 4 (FR-010, FR-021, R-005, R-016), the mirror of http_test.go's server side:
// StreamableHTTPDial returns a core.Transport that targets one endpoint over HTTP POST,
// sends the binding's Accept header (application/json, text/event-stream), carries no
// session-identifier header in modern mode, delivers a JSON response body as one frame
// and an SSE response body as one frame per event (streaming, not fully buffering), and
// uses the *http.Client supplied through WithHTTPClient.
//
// The Transport interface is a framed channel — Send writes one frame, Receive yields
// the next — so the dialer maps Send to one HTTP POST carrying that frame and makes the
// response body's frames available through Receive. This correlation is the only mapping
// consistent with a request/response HTTP binding, and it is what lets client.Session
// drive a request and read its reply without knowing the transport is HTTP.
//
// This file is written first and is expected to fail to compile until httpdial.go (T039)
// lands. It names exactly the two constructors transport.md Section 2 / core-api.md
// "Package transport" declare — StreamableHTTPDial and WithHTTPClient, plus the
// HTTPDialOption type they share — so a red build is attributable to httpdial.go alone.

var (
	// StreamableHTTPDial and WithHTTPClient are pinned to the exact signatures in
	// transport.md Section 2 / core-api.md "Package transport".
	_ func(string, ...HTTPDialOption) (core.Transport, error) = StreamableHTTPDial
	_ func(*http.Client) HTTPDialOption                       = WithHTTPClient
)

const (
	// dialAccept is the Accept value the binding requires of a client: it negotiates a
	// JSON body or an SSE body (transport.md Section 4).
	dialAccept = "application/json, text/event-stream"

	// dialRequestFrame is a well-formed JSON-RPC request the dialer POSTs as its body.
	dialRequestFrame = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`

	// dialNotificationFrame is an id-less JSON-RPC request, which the binding answers
	// 202 Accepted with an empty body.
	dialNotificationFrame = `{"jsonrpc":"2.0","method":"notifications/initialized"}`

	// dialSessionHeader is the legacy session-identifier header a modern request must
	// not carry (wire-protocol.md Section 9, FR-003, FR-055).
	dialSessionHeader = "Mcp-Session-Id"
)

// dial returns a transport over endpoint and registers its Close, so every test
// releases the transport it opened. A nil transport is a failure in itself.
func dial(t *testing.T, endpoint string, opts ...HTTPDialOption) core.Transport {
	t.Helper()
	tr, err := StreamableHTTPDial(endpoint, opts...)
	if err != nil {
		t.Fatalf("StreamableHTTPDial(%q): %v", endpoint, err)
	}
	if tr == nil {
		t.Fatalf("StreamableHTTPDial(%q) returned a nil transport and no error", endpoint)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// sendFrame issues one Send under a deadline, so a transport that never accepts the
// frame fails the test instead of hanging it.
func sendFrame(t *testing.T, tr core.Transport, frame string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tr.Send(ctx, []byte(frame)); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// receiveFrame returns the next frame, failing the test if none arrives before the
// deadline. A transport that delivers no frame where one is required therefore fails
// rather than blocking the suite.
func receiveFrame(t *testing.T, tr core.Transport) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frame, err := tr.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	return frame
}

// receiveResponse returns the next frame decoded as a JSON-RPC response, so a test can
// assert on the envelope the server sent rather than on exact bytes.
func receiveResponse(t *testing.T, tr core.Transport) *core.Response {
	t.Helper()
	frame := receiveFrame(t, tr)
	decoded, err := core.Decode(frame)
	if err != nil {
		t.Fatalf("core.Decode(%s): %v", frame, err)
	}
	resp, ok := decoded.(*core.Response)
	if !ok {
		t.Fatalf("frame decoded to %T, want *core.Response: %s", decoded, frame)
	}
	return resp
}

// capturedRequest is a snapshot of one inbound HTTP request the dialer sent, so a test
// can assert on what went over the wire without racing the server goroutine.
type capturedRequest struct {
	Method string
	URL    *url.URL
	Header http.Header
	Body   []byte
}

// replyFunc writes the canned HTTP response for one request.
type replyFunc func(w http.ResponseWriter, r *http.Request, body []byte)

// replyWithContentType answers every request with status and a body of the given content
// type, the shape a Streamable HTTP server uses for a JSON or SSE response.
func replyWithContentType(status int, contentType, body string) replyFunc {
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// replyJSON answers with a JSON body, the shape of a single JSON-RPC response.
func replyJSON(status int, body string) replyFunc {
	return replyWithContentType(status, "application/json", body)
}

// replySSE answers with an SSE body, the shape of multi-part or server-initiated traffic.
func replySSE(status int, body string) replyFunc {
	return replyWithContentType(status, "text/event-stream", body)
}

// captureServer starts an httptest.Server that records every request it receives and
// answers through reply. Each recorded request is delivered on the returned channel, so a
// test can wait for the server to have seen the POST regardless of whether the dialer
// sends synchronously or streams the response body in the background.
func captureServer(reply replyFunc) (*httptest.Server, <-chan capturedRequest) {
	ch := make(chan capturedRequest, 16)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u := *r.URL
		ch <- capturedRequest{Method: r.Method, URL: &u, Header: r.Header.Clone(), Body: body}
		reply(w, r, body)
	})
	return httptest.NewServer(h), ch
}

// waitRequest returns the first request the server recorded, failing the test if the
// dialer sent none within the deadline.
func waitRequest(t *testing.T, ch <-chan capturedRequest) capturedRequest {
	t.Helper()
	select {
	case req := <-ch:
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("the dialer sent no HTTP request")
		return capturedRequest{}
	}
}

// TestStreamableHTTPDialSendsPOSTWithRequiredHeaders pins the request the dialer issues:
// one HTTP POST carrying the frame as the body, with the Accept header the binding
// requires of a client (both application/json and text/event-stream) and a JSON content
// type (transport.md Section 4, R-005).
func TestStreamableHTTPDialSendsPOSTWithRequiredHeaders(t *testing.T) {
	srv, ch := captureServer(replyJSON(http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{}}`))
	defer srv.Close()

	tr := dial(t, srv.URL)
	sendFrame(t, tr, dialRequestFrame)
	_ = receiveFrame(t, tr) // let the exchange complete before inspecting the request

	req := waitRequest(t, ch)
	if req.Method != http.MethodPost {
		t.Errorf("request method = %s, want POST", req.Method)
	}
	if got := req.Header.Get("Accept"); !strings.Contains(got, "application/json") || !strings.Contains(got, "text/event-stream") {
		t.Errorf("Accept = %q, want it to list both application/json and text/event-stream", got)
	}
	if got := req.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if string(req.Body) != dialRequestFrame {
		t.Errorf("request body = %s, want %s", req.Body, dialRequestFrame)
	}
}

// TestStreamableHTTPDialSendsNoSessionHeaderInModernMode pins FR-003/FR-055 over HTTP:
// a modern request carries no session-identifier header — all protocol context is in
// per-request _meta — so the dialer must never emit Mcp-Session-Id (transport.md
// Section 4, wire-protocol.md Section 9).
func TestStreamableHTTPDialSendsNoSessionHeaderInModernMode(t *testing.T) {
	srv, ch := captureServer(replyJSON(http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{}}`))
	defer srv.Close()

	tr := dial(t, srv.URL)
	sendFrame(t, tr, dialRequestFrame)
	_ = receiveFrame(t, tr)

	req := waitRequest(t, ch)
	for name := range req.Header {
		if strings.EqualFold(name, dialSessionHeader) {
			t.Errorf("modern request carries a session-identifier header %s: %q", name, req.Header.Get(name))
		}
	}
}

// TestStreamableHTTPDialParsesJSONResponseBody pins the JSON body shape: a response sent
// with Content-Type: application/json is delivered as exactly one frame through Receive
// (transport.md Section 4).
func TestStreamableHTTPDialParsesJSONResponseBody(t *testing.T) {
	const body = `{"jsonrpc":"2.0","id":1,"result":{"echo":"hi"}}`
	srv, _ := captureServer(replyJSON(http.StatusOK, body))
	defer srv.Close()

	tr := dial(t, srv.URL)
	sendFrame(t, tr, dialRequestFrame)

	resp := receiveResponse(t, tr)
	if string(resp.ID) != "1" {
		t.Errorf("response id = %s, want 1", resp.ID)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error response: %+v", resp.Error)
	}
	if got := string(resp.Result); got != `{"echo":"hi"}` {
		t.Errorf("result = %s, want {\"echo\":\"hi\"}", got)
	}
}

// TestStreamableHTTPDialParsesSSEResponseBody pins the SSE body shape: a response sent
// with Content-Type: text/event-stream is delivered as one frame per event, in order —
// each event's data is one JSON-RPC envelope (transport.md Section 4).
func TestStreamableHTTPDialParsesSSEResponseBody(t *testing.T) {
	const body = "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"n\":1}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"n\":2}}\n\n"
	srv, _ := captureServer(replySSE(http.StatusOK, body))
	defer srv.Close()

	tr := dial(t, srv.URL)
	sendFrame(t, tr, dialRequestFrame)

	first := receiveResponse(t, tr)
	if string(first.ID) != "1" {
		t.Errorf("first event id = %s, want 1", first.ID)
	}
	second := receiveResponse(t, tr)
	if string(second.ID) != "2" {
		t.Errorf("second event id = %s, want 2", second.ID)
	}
}

// TestStreamableHTTPDialStreamsSSEBodyWithoutBuffering pins R-016's "responses stream
// where the binding permits": the first SSE event must be delivered while the stream is
// still open. The server writes the first event, then blocks until the test releases it
// before writing the second, so a dialer that buffers the whole body before yielding any
// frame cannot pass the first Receive.
func TestStreamableHTTPDialStreamsSSEBodyWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		ev := newSSEWriter(w)
		if err := ev.Write(sseEvent{Data: `{"jsonrpc":"2.0","id":1,"result":{"n":1}}`}); err != nil {
			return
		}
		<-release // hold the stream open until the test has read the first event
		_ = ev.Write(sseEvent{Data: `{"jsonrpc":"2.0","id":2,"result":{"n":2}}`})
	})
	srv := httptest.NewServer(h)
	defer srv.Close() // runs after unblock, so the blocked handler cannot wedge Close
	defer unblock()   // release the handler even if an assertion fails first

	tr := dial(t, srv.URL)
	sendFrame(t, tr, dialRequestFrame)

	first := receiveResponse(t, tr) // must arrive while the stream is open
	if string(first.ID) != "1" {
		t.Errorf("first streamed event id = %s, want 1", first.ID)
	}

	unblock()
	second := receiveResponse(t, tr)
	if string(second.ID) != "2" {
		t.Errorf("second streamed event id = %s, want 2", second.ID)
	}
}

// TestStreamableHTTPDialQueuesNoFrameForNotification pins the notification rule from the
// client side: a POST answered 202 Accepted with an empty body yields no frame, so the
// next Receive returns the response to the following request rather than a stray frame.
func TestStreamableHTTPDialQueuesNoFrameForNotification(t *testing.T) {
	srv, _ := captureServer(func(w http.ResponseWriter, _ *http.Request, body []byte) {
		decoded, err := core.Decode(body)
		if req, ok := decoded.(*core.Request); err == nil && ok && len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted) // a notification: empty body, no reply
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	})
	defer srv.Close()

	tr := dial(t, srv.URL)
	sendFrame(t, tr, dialNotificationFrame)
	sendFrame(t, tr, dialRequestFrame)

	resp := receiveResponse(t, tr)
	if string(resp.ID) != "1" {
		t.Errorf("first frame id = %s, want 1 (the request's response, not the notification)", resp.ID)
	}
}

// recordingRoundTripper is an http.RoundTripper that answers without a network, recording
// how many times it was used and for which URL — the evidence that WithHTTPClient's client
// carried the dialer's request.
type recordingRoundTripper struct {
	mu    sync.Mutex
	calls int
	url   string
	body  string
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.calls++
	rt.url = req.URL.String()
	rt.mu.Unlock()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)),
		Request:    req,
	}
	return resp, nil
}

func (rt *recordingRoundTripper) count() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.calls
}

func (rt *recordingRoundTripper) lastURL() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.url
}

// TestStreamableHTTPDialHonoursHTTPClient pins WithHTTPClient: the dialer issues its POST
// through the supplied *http.Client, so a client whose transport attaches a bearer token
// (the core.TokenSource use case in transport.md Section 2) is what carries the request.
func TestStreamableHTTPDialHonoursHTTPClient(t *testing.T) {
	const endpoint = "http://dial.invalid/mcp"
	rt := &recordingRoundTripper{}
	client := &http.Client{Transport: rt}

	tr := dial(t, endpoint, WithHTTPClient(client))
	sendFrame(t, tr, dialRequestFrame)

	resp := receiveResponse(t, tr)
	if string(resp.ID) != "1" {
		t.Errorf("response id = %s, want 1", resp.ID)
	}
	if got := rt.count(); got != 1 {
		t.Errorf("the supplied client was used %d times, want 1", got)
	}
	if got := rt.lastURL(); got != endpoint {
		t.Errorf("the supplied client saw URL %q, want %q", got, endpoint)
	}
}

// TestStreamableHTTPDialRejectsAnUnusableEndpoint pins the error return of the
// constructor's signature: an endpoint that is not a URL is rejected up front rather than
// yielding a transport that only fails when it is first used.
func TestStreamableHTTPDialRejectsAnUnusableEndpoint(t *testing.T) {
	tr, err := StreamableHTTPDial("://missing-scheme")
	if err == nil {
		t.Cleanup(func() { _ = tr.Close() })
		t.Fatal("StreamableHTTPDial accepted an endpoint that is not a URL; want an error")
	}
}
