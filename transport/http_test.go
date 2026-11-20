package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/qi-lore/qi-mcp-sdk/core"
)

// These tests pin the Streamable HTTP binding of transport.md Section 4 (FR-021,
// FR-018, V20, R-005) and the HTTP status accompaniment of wire-protocol.md
// Section 6.1, from the server side: HTTPHandler serves one endpoint that accepts
// HTTP POST; a single JSON-RPC response is answered as a JSON body
// (Content-Type: application/json) and server traffic the client will not take as
// JSON is answered as an SSE stream (Content-Type: text/event-stream), flushed
// immediately; a notification POST is answered 202 Accepted with an empty body; every
// JSON-RPC error code the SDK emits is returned with the HTTP status Section 6.1
// fixes — including the T009-confirmed 404 for -32601; and WithMaxBodyBytes bounds the
// accepted request body, with a body over the bound never reaching the core.Handler.
//
// The transport's job here is framing plus status: it decodes one JSON-RPC request and
// dispatches it to a core.Handler, then shapes the HTTP response around what came back.
// It owns no JSON-RPC semantics — the method table, name rules, and schema validation
// live in server — which is why the tests replace core.Handler with a stub instead of
// importing server (the dependency rule of FR-021 and core-api.md).
//
// This file is written first and is expected to fail to compile until http.go (T037)
// lands. It names exactly the two constructors transport.md Section 2 declares —
// HTTPHandler and WithMaxBodyBytes, plus the HTTPOption type they share — so a red
// build is attributable to http.go alone.

var (
	// HTTPHandler and WithMaxBodyBytes are pinned to the exact signatures in
	// transport.md Section 2 / core-api.md "Package transport".
	_ func(core.Handler, ...HTTPOption) http.Handler = HTTPHandler
	_ func(int64) HTTPOption                         = WithMaxBodyBytes
)

// The Accept values these tests send. acceptBoth is the header the binding requires of
// every client; the single-type values force the server's choice so each response shape
// is reachable deterministically.
const (
	acceptBoth     = "application/json, text/event-stream"
	acceptSSEOnly  = "text/event-stream"
	acceptJSONOnly = "application/json"

	// mcpEndpoint is an arbitrary path: the caller mounts the handler at the one MCP
	// endpoint, so the handler itself must not route by path.
	mcpEndpoint = "/mcp"
)

// stubHandler is the test double for core.Handler. It records every request it is
// handed and answers with a canned reply, so a test can assert both what the transport
// dispatched and how it shaped the HTTP response around the reply.
type stubHandler struct {
	mu    sync.Mutex
	reqs  []*core.Request
	reply func(ctx context.Context, req *core.Request) (*core.Response, error)
}

var _ core.Handler = (*stubHandler)(nil)

// newStubHandler returns a handler that answers through reply. A nil reply models a
// handler with nothing to send back, as a notification gets from a real server.
func newStubHandler(reply func(ctx context.Context, req *core.Request) (*core.Response, error)) *stubHandler {
	return &stubHandler{reply: reply}
}

func (h *stubHandler) Handle(ctx context.Context, req *core.Request) (*core.Response, error) {
	h.mu.Lock()
	h.reqs = append(h.reqs, req)
	reply := h.reply
	h.mu.Unlock()
	if reply == nil {
		return nil, nil
	}
	return reply(ctx, req)
}

// handled returns the requests dispatched so far, so a test can assert that a request
// was dispatched exactly once — or never.
func (h *stubHandler) handled() []*core.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*core.Request(nil), h.reqs...)
}

// replyWithResult answers every request with a successful result, echoing the request
// id. result is the raw JSON of the result member.
func replyWithResult(result string) func(context.Context, *core.Request) (*core.Response, error) {
	return func(_ context.Context, req *core.Request) (*core.Response, error) {
		return &core.Response{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(result)}, nil
	}
}

// replyWithErrorCode answers every request with a JSON-RPC error response carrying
// code, as the server package will for a rejection it decided on itself.
func replyWithErrorCode(code int) func(context.Context, *core.Request) (*core.Response, error) {
	return func(_ context.Context, req *core.Request) (*core.Response, error) {
		return &core.Response{JSONRPC: "2.0", ID: req.ID, Error: &core.Error{Code: code, Message: "rejected"}}, nil
	}
}

// replyWithError answers every request with a Go error, as a handler that rejected the
// request instead of building a response.
func replyWithError(err error) func(context.Context, *core.Request) (*core.Response, error) {
	return func(context.Context, *core.Request) (*core.Response, error) {
		return nil, err
	}
}

// doRequest serves one HTTP request through h with the headers the binding requires of
// a client — Content-Type: application/json and an Accept listing both body types —
// overridden by headers, and returns the recorded response.
func doRequest(t *testing.T, h http.Handler, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", acceptBoth)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// postRequest is doRequest for the one request method the binding defines.
func postRequest(t *testing.T, h http.Handler, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodPost, mcpEndpoint, body, headers)
}

// wireResponse is the subset of a JSON-RPC response these tests assert. ID is kept raw
// so a null id and an absent id are distinguishable byte-for-byte.
type wireResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *wireError      `json:"error"`
}

// wireError is the subset of a JSON-RPC error object these tests assert.
type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// decodeJSON decodes a response body, failing the test when it is not JSON.
func decodeJSON(t *testing.T, b []byte) wireResponse {
	t.Helper()
	var r wireResponse
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("response body %q is not JSON: %v", b, err)
	}
	return r
}

// assertJSONContentType fails unless the response declares a JSON body, which is what a
// single JSON-RPC response must be (transport.md Section 4).
func assertJSONContentType(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json for a single response", ct)
	}
}

// assertErrorResponse fails unless the body is a JSON-RPC error response carrying code
// and the expected raw id ("null" for an error that has no id to echo).
func assertErrorResponse(t *testing.T, rec *httptest.ResponseRecorder, code int, wantID string) {
	t.Helper()
	got := decodeJSON(t, rec.Body.Bytes())
	if got.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want \"2.0\"", got.JSONRPC)
	}
	if got.Error == nil {
		t.Fatalf("response %q carries no error object, want code %d", rec.Body.Bytes(), code)
	}
	if got.Error.Code != code {
		t.Errorf("error code = %d, want %d", got.Error.Code, code)
	}
	if string(got.ID) != wantID {
		t.Errorf("error id = %s, want %s", got.ID, wantID)
	}
}

// TestHTTPHandlerAnswersRequestWithJSONBody pins the JSON branch of transport.md
// Section 4: one POST carrying one JSON-RPC request is dispatched to the core.Handler,
// and a single response comes back as `200 OK` with a JSON body when the client accepts
// JSON. The second subtest pins "single endpoint": the handler serves whatever path the
// caller mounted it at and does not route on the request path.
func TestHTTPHandlerAnswersRequestWithJSONBody(t *testing.T) {
	const result = `{"tools":[]}`
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	t.Run("single response as a JSON body", func(t *testing.T) {
		handler := newStubHandler(replyWithResult(result))
		rec := postRequest(t, HTTPHandler(handler), body, nil)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.Bytes())
		}
		assertJSONContentType(t, rec)

		got := decodeJSON(t, rec.Body.Bytes())
		if got.JSONRPC != "2.0" {
			t.Errorf("jsonrpc = %q, want \"2.0\"", got.JSONRPC)
		}
		if string(got.ID) != "1" {
			t.Errorf("id = %s, want 1 (the request id must be echoed)", got.ID)
		}
		if got.Error != nil {
			t.Errorf("response carries error %+v, want a result", got.Error)
		}
		if string(got.Result) != result {
			t.Errorf("result = %s, want %s", got.Result, result)
		}

		reqs := handler.handled()
		if len(reqs) != 1 {
			t.Fatalf("handler was called %d times, want exactly 1", len(reqs))
		}
		if reqs[0].Method != "tools/list" {
			t.Errorf("dispatched method = %q, want \"tools/list\"", reqs[0].Method)
		}
		if string(reqs[0].ID) != "1" {
			t.Errorf("dispatched id = %s, want 1", reqs[0].ID)
		}
	})

	t.Run("a client that accepts only JSON still gets a JSON body", func(t *testing.T) {
		handler := newStubHandler(replyWithResult(result))
		rec := postRequest(t, HTTPHandler(handler), body, map[string]string{"Accept": acceptJSONOnly})

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.Bytes())
		}
		assertJSONContentType(t, rec)
	})

	t.Run("any path is the single endpoint", func(t *testing.T) {
		handler := newStubHandler(replyWithResult(result))
		rec := doRequest(t, HTTPHandler(handler), http.MethodPost, "/some/other/path", body, nil)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (the handler must serve the mounted endpoint, not route by path)",
				rec.Code, http.StatusOK)
		}
		if n := len(handler.handled()); n != 1 {
			t.Errorf("handler was called %d times, want exactly 1", n)
		}
	})
}

// TestHTTPHandlerAnswersWithSSEWhenJSONIsNotAccepted pins the SSE branch of
// transport.md Section 4 through the writer of Section 5: when the client will not take
// a JSON body, the response stream is `text/event-stream`, carries the JSON-RPC response
// as an event's data, and is flushed so the client receives it immediately.
func TestHTTPHandlerAnswersWithSSEWhenJSONIsNotAccepted(t *testing.T) {
	const result = `{"tools":[]}`
	body := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)

	handler := newStubHandler(replyWithResult(result))
	rec := postRequest(t, HTTPHandler(handler), body, map[string]string{"Accept": acceptSSEOnly})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.Bytes())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream when JSON is not accepted", ct)
	}

	// The body is parsed with the transport's own bounded SSE reader (Section 5), so the
	// produced stream is held to the same grammar the client side will parse.
	events := readEvents(t, newSSEReader(bytes.NewReader(rec.Body.Bytes())))
	if len(events) != 1 {
		t.Fatalf("stream carries %d events, want exactly 1 (the final response): %q", len(events), rec.Body.Bytes())
	}
	if events[0].Event != "" {
		t.Errorf("event type = %q, want the default message type (a data-only event)", events[0].Event)
	}

	got := decodeJSON(t, []byte(events[0].Data))
	if string(got.ID) != "7" {
		t.Errorf("event data id = %s, want 7 (the request id must be echoed)", got.ID)
	}
	if string(got.Result) != result {
		t.Errorf("event data result = %s, want %s", got.Result, result)
	}

	if !rec.Flushed {
		t.Error("the SSE response was not flushed; a client would wait for a buffered event")
	}
}

// TestHTTPHandlerAcceptsNotificationWith202AndEmptyBody pins the notification rule of
// transport.md Section 4 and wire-protocol.md Section 6.1: an id-less POST is still
// dispatched to the handler, and is answered `202 Accepted` with no body — never a
// response, because a notification has no id to correlate one with.
func TestHTTPHandlerAcceptsNotificationWith202AndEmptyBody(t *testing.T) {
	const body = `{"jsonrpc":"2.0","method":"notifications/initialized"}`

	t.Run("dispatched and accepted with an empty body", func(t *testing.T) {
		handler := newStubHandler(nil)
		rec := postRequest(t, HTTPHandler(handler), []byte(body), nil)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body.Bytes())
		}
		if n := rec.Body.Len(); n != 0 {
			t.Errorf("body has %d bytes, want an empty body: %q", n, rec.Body.Bytes())
		}

		reqs := handler.handled()
		if len(reqs) != 1 {
			t.Fatalf("handler was called %d times, want exactly 1 (a notification is still dispatched)", len(reqs))
		}
		if reqs[0].Method != "notifications/initialized" {
			t.Errorf("dispatched method = %q, want \"notifications/initialized\"", reqs[0].Method)
		}
		if len(reqs[0].ID) != 0 {
			t.Errorf("dispatched id = %s, want none (a notification carries no id)", reqs[0].ID)
		}
	})

	t.Run("a reply from the handler is not written", func(t *testing.T) {
		handler := newStubHandler(replyWithResult(`"ignored"`))
		rec := postRequest(t, HTTPHandler(handler), []byte(body), nil)

		if rec.Code != http.StatusAccepted {
			t.Errorf("status = %d, want %d for a notification", rec.Code, http.StatusAccepted)
		}
		if n := rec.Body.Len(); n != 0 {
			t.Errorf("body has %d bytes, want an empty body: %q", n, rec.Body.Bytes())
		}
	})
}

// TestHTTPHandlerMapsJSONRPCErrorCodeToHTTPStatus pins wire-protocol.md Section 6.1: the
// transport returns every JSON-RPC error response with the HTTP status that table fixes
// for its code. The 404 for -32601 is the T009-confirmed status of the MCP 2026-07-28
// Streamable HTTP binding, not an inference from HTTP convention.
func TestHTTPHandlerMapsJSONRPCErrorCodeToHTTPStatus(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		status int
	}{
		{"-32700 parse error", core.CodeParseError, http.StatusBadRequest},
		{"-32600 invalid request", core.CodeInvalidRequest, http.StatusBadRequest},
		{"-32602 invalid params", core.CodeInvalidParams, http.StatusBadRequest},
		{"-32601 method not found", core.CodeMethodNotFound, http.StatusNotFound},
		{"-32603 internal error", core.CodeInternalError, http.StatusInternalServerError},
		{"-32022 unsupported protocol version", core.CodeUnsupportedVersion, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newStubHandler(replyWithErrorCode(tc.code))
			rec := postRequest(t, HTTPHandler(handler), []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call"}`), nil)

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d for code %d: %s", rec.Code, tc.status, tc.code, rec.Body.Bytes())
			}
			assertJSONContentType(t, rec)
			assertErrorResponse(t, rec, tc.code, "9")
		})
	}
}

// TestHTTPHandlerRejectsMalformedBodyWithParseError pins the -32700 row of
// wire-protocol.md Section 6.1 and FR-018: a body that is not valid JSON is answered
// `400 Bad Request` with a parse error whose id is null — and is never dispatched.
func TestHTTPHandlerRejectsMalformedBodyWithParseError(t *testing.T) {
	cases := map[string]string{
		"truncated":  `{"jsonrpc":"2.0","id":1,`,
		"empty body": "",
		"not JSON":   `tools/list`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			handler := newStubHandler(replyWithResult(`null`))
			rec := postRequest(t, HTTPHandler(handler), []byte(body), nil)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.Bytes())
			}
			assertErrorResponse(t, rec, core.CodeParseError, "null")
			if n := len(handler.handled()); n != 0 {
				t.Errorf("handler was called %d times for a malformed body, want 0", n)
			}
		})
	}
}

// TestHTTPHandlerRejectsNonRequestEnvelopeWithoutDispatching pins the -32600 row of
// wire-protocol.md Section 6.1 and FR-018: valid JSON that is not exactly one JSON-RPC
// request is answered `400 Bad Request` and never dispatched. The response-envelope case
// follows the binding's rule that a client MUST NOT send JSON-RPC responses, and the
// batch case that the body MUST be a single request or notification.
func TestHTTPHandlerRejectsNonRequestEnvelopeWithoutDispatching(t *testing.T) {
	cases := map[string]string{
		"not a JSON-RPC envelope": `{"hello":"world"}`,
		"wrong jsonrpc version":   `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`,
		"a JSON-RPC response":     `{"jsonrpc":"2.0","id":1,"result":{}}`,
		"a JSON-RPC batch":        `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			handler := newStubHandler(replyWithResult(`null`))
			rec := postRequest(t, HTTPHandler(handler), []byte(body), nil)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.Bytes())
			}
			got := decodeJSON(t, rec.Body.Bytes())
			if got.Error == nil || got.Error.Code != core.CodeInvalidRequest {
				t.Fatalf("error = %+v, want code %d: %s", got.Error, core.CodeInvalidRequest, rec.Body.Bytes())
			}
			if n := len(handler.handled()); n != 0 {
				t.Errorf("handler was called %d times for an invalid request, want 0", n)
			}
		})
	}
}

// TestHTTPHandlerMapsHandlerErrorToJSONRPCError pins how a rejected dispatch becomes an
// HTTP response. core.Handler reports method-not-found as errors.Is(err,
// ErrMethodNotFound) and version trouble as the typed errors of wire-protocol.md
// Section 7; every other failure is an internal error. The statuses come from
// Section 6.1, and the -32022 error carries the data shape Section 7.1 fixes.
func TestHTTPHandlerMapsHandlerErrorToJSONRPCError(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call"}`)

	t.Run("errors.Is(err, ErrMethodNotFound) is -32601 and 404", func(t *testing.T) {
		rec := postRequest(t, HTTPHandler(newStubHandler(replyWithError(core.ErrMethodNotFound))), body, nil)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusNotFound, rec.Body.Bytes())
		}
		assertJSONContentType(t, rec)
		assertErrorResponse(t, rec, core.CodeMethodNotFound, "3")
	})

	t.Run("UnsupportedProtocolVersionError is -32022 and 400", func(t *testing.T) {
		err := &core.UnsupportedProtocolVersionError{Requested: "2026-06-01", Supported: []string{"2026-07-28"}}
		rec := postRequest(t, HTTPHandler(newStubHandler(replyWithError(err))), body, nil)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.Bytes())
		}
		assertErrorResponse(t, rec, core.CodeUnsupportedVersion, "3")

		got := decodeJSON(t, rec.Body.Bytes())
		var data struct {
			Supported []string `json:"supported"`
			Requested string   `json:"requested"`
		}
		if err := json.Unmarshal(got.Error.Data, &data); err != nil {
			t.Fatalf("error data %s is not the Section 7.1 shape: %v", got.Error.Data, err)
		}
		if data.Requested != err.Requested {
			t.Errorf("error data requested = %q, want %q", data.Requested, err.Requested)
		}
		if len(data.Supported) != 1 || data.Supported[0] != err.Supported[0] {
			t.Errorf("error data supported = %v, want %v", data.Supported, err.Supported)
		}
	})

	t.Run("NegotiationError is -32022 and 400", func(t *testing.T) {
		err := &core.NegotiationError{Era: core.EraModern, Local: []string{"2026-07-28"}, Remote: []string{"2025-11-25"}}
		rec := postRequest(t, HTTPHandler(newStubHandler(replyWithError(err))), body, nil)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.Bytes())
		}
		assertErrorResponse(t, rec, core.CodeUnsupportedVersion, "3")
	})

	t.Run("any other error is -32603 and 500", func(t *testing.T) {
		rec := postRequest(t, HTTPHandler(newStubHandler(replyWithError(errors.New("boom")))), body, nil)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusInternalServerError, rec.Body.Bytes())
		}
		assertErrorResponse(t, rec, core.CodeInternalError, "3")
	})
}

// TestHTTPHandlerRejectsNonPostMethods pins "a single endpoint accepting HTTP POST"
// (transport.md Section 4). The modern binding removed the GET stream endpoint and
// directs a server that supports only this revision to answer GET or DELETE with
// `405 Method Not Allowed`.
func TestHTTPHandlerRejectsNonPostMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			handler := newStubHandler(replyWithResult(`null`))
			rec := doRequest(t, HTTPHandler(handler), method, mcpEndpoint, nil, nil)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
			}
			if n := len(handler.handled()); n != 0 {
				t.Errorf("%s reached the handler %d times, want 0", method, n)
			}
		})
	}
}

// paddedRequestOfLen builds a valid JSON-RPC request of exactly n bytes: a short request
// followed by trailing whitespace, which JSON permits. An implementation that merely
// truncated the body at the bound instead of rejecting it would decode the surviving
// prefix as a valid request and dispatch it, so this body distinguishes an enforced bound
// from an incidental decode failure.
func paddedRequestOfLen(n int) []byte {
	const req = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if n < len(req) {
		panic(fmt.Sprintf("paddedRequestOfLen(%d): n is too small", n))
	}
	body := make([]byte, n)
	copy(body, req)
	for i := len(req); i < n; i++ {
		body[i] = ' '
	}
	return body
}

// TestHTTPHandlerEnforcesMaxBodyBytes pins the request-body bound of transport.md
// Section 4 and FR-021: WithMaxBodyBytes caps the accepted body (4 MiB by default), a
// non-positive value leaves that default in place, and a body over the bound is rejected
// without being dispatched (FR-018).
//
// The exact status for an over-bound body is deliberately not pinned:
// wire-protocol.md Section 6.1 fixes a code and status for each JSON-RPC error class but
// records none for an oversized body, so this test requires only a 4xx client error.
func TestHTTPHandlerEnforcesMaxBodyBytes(t *testing.T) {
	assertClientError := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code < 400 || rec.Code >= 500 {
			t.Fatalf("status = %d, want a 4xx client error for a body over the bound: %s", rec.Code, rec.Body.Bytes())
		}
	}

	t.Run("a body under the bound is served", func(t *testing.T) {
		const bound = 1024
		body := paddedRequestOfLen(bound - 64)
		handler := newStubHandler(replyWithResult(`null`))
		rec := postRequest(t, HTTPHandler(handler, WithMaxBodyBytes(bound)), body, nil)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.Bytes())
		}
		if n := len(handler.handled()); n != 1 {
			t.Errorf("handler was called %d times, want 1", n)
		}
	})

	t.Run("a body over the bound is rejected and never dispatched", func(t *testing.T) {
		const bound = 1024
		body := paddedRequestOfLen(bound + 64)
		handler := newStubHandler(replyWithResult(`null`))
		rec := postRequest(t, HTTPHandler(handler, WithMaxBodyBytes(bound)), body, nil)

		assertClientError(t, rec)
		if n := len(handler.handled()); n != 0 {
			t.Errorf("handler was called %d times for an over-bound body, want 0", n)
		}
	})

	t.Run("a non-positive bound leaves the 4 MiB default in place", func(t *testing.T) {
		set := map[string]HTTPOption{
			"zero":     WithMaxBodyBytes(0),
			"negative": WithMaxBodyBytes(-1),
		}
		for name, opt := range set {
			t.Run(name, func(t *testing.T) {
				body := paddedRequestOfLen(4096) // far over a literal 0 bound, far under 4 MiB
				handler := newStubHandler(replyWithResult(`null`))
				rec := postRequest(t, HTTPHandler(handler, opt), body, nil)

				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want %d (a non-positive bound must leave the 4 MiB default): %s",
						rec.Code, http.StatusOK, rec.Body.Bytes())
				}
			})
		}
	})

	t.Run("default bound accepts just under 4 MiB", func(t *testing.T) {
		const fourMiB = 4 << 20
		body := paddedRequestOfLen(fourMiB - 4096)
		handler := newStubHandler(replyWithResult(`null`))
		rec := postRequest(t, HTTPHandler(handler), body, nil)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d for a %d-byte body under the default bound: %s",
				rec.Code, http.StatusOK, len(body), rec.Body.Bytes())
		}
	})

	t.Run("default bound rejects just over 4 MiB", func(t *testing.T) {
		const fourMiB = 4 << 20
		body := paddedRequestOfLen(fourMiB + 4096)
		handler := newStubHandler(replyWithResult(`null`))
		rec := postRequest(t, HTTPHandler(handler), body, nil)

		assertClientError(t, rec)
		if n := len(handler.handled()); n != 0 {
			t.Errorf("handler was called %d times for an over-bound body, want 0", n)
		}
	})
}
