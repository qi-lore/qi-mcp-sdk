package transport

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/qi-lore/qi-mcp-sdk/core"
)

// This file implements the server side of the Streamable HTTP binding from
// contracts/transport.md Section 4 (FR-021, FR-018, R-005, V20), with the
// response-shape, method, bound, and auth choices recorded in
// docs/decisions/0007-http-response-shape.md:
//
//   - Single endpoint, POST only. Any other method is answered 405 Method Not Allowed
//     with an Allow: POST header and is never dispatched — including GET and DELETE,
//     the legacy stream and session termination this revision removed.
//   - HTTPHandler decodes the request body into a *core.Request and dispatches it to
//     the injected core.Handler. It imports no server package: it programs against the
//     Handler interface, which is what lets transport stay independent of server
//     (FR-021, core-api.md "Package transport").
//   - Body shape follows the request's Accept (decision 0007): a JSON body
//     (Content-Type: application/json) when the client accepts application/json, and
//     otherwise a flushed, data-only SSE event (Content-Type: text/event-stream)
//     carrying the response, through the bounded writer of Section 5.
//   - A notification — an id-less request — is still dispatched to the handler and
//     answered 202 Accepted with an empty body; a reply the handler returns for it is
//     never written, because a notification has no id to correlate a response with.
//   - Errors are shaped per wire-protocol.md Section 6.1: a body that is not valid JSON
//     is -32700/400; valid JSON that is not exactly one JSON-RPC request is -32600/400;
//     a handler rejection is mapped by class (method-not-found -32601/404, version
//     trouble -32022/400 carrying the Section 7.1 data, auth 401/403, anything else
//     -32603/500).
//   - The accepted request body is bounded by WithMaxBodyBytes (default 4 MiB); a body
//     over the bound is rejected with a 4xx status and never dispatched (FR-018).
//
// The handler writes nothing else: it never writes diagnostics to the response, so a
// caller sees only protocol bytes (FR-057, NFR-011).

// defaultMaxBodyBytes is the default maximum accepted request-body size (R-019): 4 MiB,
// the same bound applied uniformly across the stdio, HTTP, and SSE transports.
const defaultMaxBodyBytes int64 = 4 << 20

// httpConfig holds the resolved Streamable HTTP options. It is unexported because
// HTTPOption is the only public knob (contracts/transport.md Section 2).
type httpConfig struct {
	maxBodyBytes int64
}

// HTTPOption configures the handler HTTPHandler returns.
type HTTPOption func(*httpConfig)

// WithMaxBodyBytes sets the maximum accepted request-body size in bytes (V20). The
// default is 4 MiB; a value that is not positive leaves the default in place (decision
// 0007, mirroring decision 0004).
func WithMaxBodyBytes(n int64) HTTPOption {
	return func(c *httpConfig) {
		c.maxBodyBytes = n
	}
}

// HTTPHandler returns an http.Handler serving the Streamable HTTP binding for h,
// typically a *server.Server, without importing the server package (FR-021). The
// returned handler is safe for concurrent use: it holds no mutable state and dispatches
// each request to h.
func HTTPHandler(h core.Handler, opts ...HTTPOption) http.Handler {
	cfg := httpConfig{maxBodyBytes: defaultMaxBodyBytes}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.maxBodyBytes <= 0 {
		cfg.maxBodyBytes = defaultMaxBodyBytes
	}
	return &httpHandler{handler: h, cfg: cfg}
}

// httpHandler is the Streamable HTTP server. It is stateless beyond its configuration,
// so one value serves every request concurrently.
type httpHandler struct {
	handler core.Handler
	cfg     httpConfig
}

// ServeHTTP implements http.Handler: it enforces the single-endpoint POST contract,
// bounds and decodes the body, dispatches one request to the core.Handler, and shapes
// the HTTP response around the result (transport.md Section 4).
func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// A single endpoint answers POST only; every other method — GET and DELETE
		// included — is refused and never dispatched (decision 0007).
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, ok := h.readBody(w, r)
	if !ok {
		return
	}

	req, code := decodeHTTPRequest(body)
	if req == nil {
		// Not a single JSON-RPC request: a parse error or an invalid request. Neither
		// is ever dispatched (FR-018); a parse error carries a null id because there is
		// no request to correlate it with.
		h.writeResponse(w, r, errorResponse(nil, code, invalidRequestMessage(code)), http.StatusBadRequest)
		return
	}

	if len(req.ID) == 0 {
		// A notification: dispatch it, but never write a reply — it has no id for one.
		if _, err := h.handler.Handle(r.Context(), req); err != nil {
			h.writeHandlerError(w, r, nil, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp, err := h.handler.Handle(r.Context(), req)
	if err != nil {
		h.writeHandlerError(w, r, req.ID, err)
		return
	}
	if resp == nil {
		// A request with no response and no error is a handler fault: report it as an
		// internal error rather than writing an empty 200.
		h.writeResponse(w, r, errorResponse(req.ID, core.CodeInternalError, "Internal error"), http.StatusInternalServerError)
		return
	}
	// A handler that decided on a JSON-RPC error itself returns it inside a response;
	// the status that accompanies its code is the one Section 6.1 fixes, not 200.
	h.writeResponse(w, r, resp, statusForResponse(resp))
}

// statusForResponse returns the HTTP status accompanying a handler-produced response:
// 200 for a success, and the wire-protocol.md Section 6.1 status for its error code
// otherwise.
func statusForResponse(resp *core.Response) int {
	if resp.Error == nil {
		return http.StatusOK
	}
	return statusForCode(resp.Error.Code)
}

// statusForCode maps a JSON-RPC error code to the HTTP status of wire-protocol.md
// Section 6.1. A code the table does not classify is surfaced as an internal error (500),
// matching how the transport already treats an unclassifiable handler error. Note that
// the unsupported-version and negotiation-failure codes share the value -32022, so they
// are one case here.
func statusForCode(code int) int {
	switch code {
	case core.CodeParseError, core.CodeInvalidRequest, core.CodeInvalidParams, core.CodeUnsupportedVersion:
		return http.StatusBadRequest
	case core.CodeMethodNotFound:
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

// readBody reads the request body, bounded by the configured maximum. It returns
// (body, true) when the body is within the bound, and otherwise writes the appropriate
// error response and returns (nil, false). A body over the bound is rejected with 413
// and never read in full or dispatched (FR-018, decision 0007).
func (h *httpHandler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	// Read one byte past the bound: a body exactly at the bound is accepted, so the
	// limit must not be mistaken for "one short". The over-bound case is detected by
	// the length actually read, not by an incidental decode failure.
	body, err := io.ReadAll(io.LimitReader(r.Body, h.cfg.maxBodyBytes+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, false
	}
	if int64(len(body)) > h.cfg.maxBodyBytes {
		// Section 6.1 fixes no code and status for an oversized body, so the status is
		// not contractual; 413 is the conventional choice (decision 0007).
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return body, true
}

// decodeHTTPRequest decodes one request body into a *core.Request. It returns (req, 0)
// on success and (nil, code) with the JSON-RPC error code to report otherwise: -32700
// for a body that is not valid JSON, and -32600 for valid JSON that is not exactly one
// JSON-RPC request — a wrong or missing `jsonrpc`, a client-sent response, a batch, or
// any non-object JSON (wire-protocol.md Section 6.1).
func decodeHTTPRequest(frame []byte) (*core.Request, int) {
	// Valid JSON is the parse-error boundary: everything below this line is a shape
	// problem, not a framing one.
	if !json.Valid(frame) {
		return nil, core.CodeParseError
	}
	// The codec validates the envelope: a non-"2.0" `jsonrpc`, a non-object, and a
	// response without exactly one of result/error are all rejected here. Because the
	// body is already known to be valid JSON, any error is an invalid request.
	decoded, err := core.Decode(frame)
	if err != nil {
		return nil, core.CodeInvalidRequest
	}
	req, ok := decoded.(*core.Request)
	if !ok {
		// A *core.Response: a client MUST NOT send a JSON-RPC response.
		return nil, core.CodeInvalidRequest
	}
	return req, 0
}

// writeResponse writes one JSON-RPC response with the given HTTP status, choosing the
// body shape from the request's Accept (decision 0007): a JSON body when the client
// accepts application/json, and otherwise a flushed, data-only SSE event carrying the
// same JSON.
func (h *httpHandler) writeResponse(w http.ResponseWriter, r *http.Request, resp *core.Response, status int) {
	frame, err := core.Encode(resp)
	if err != nil {
		// The handler produced an envelope the codec rejects; that is a server fault.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if acceptsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(frame)
		return
	}

	// The client will not take a JSON body: stream the response as the data of a
	// data-only event — no `event:` field, so it keeps the SSE default `message` type —
	// and flush it (Section 5's writer).
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(status)
	_ = newSSEWriter(w).Write(sseEvent{Data: string(frame)})
}

// writeHandlerError turns a handler rejection into an HTTP response, mapping the error
// class to the status and JSON-RPC error of wire-protocol.md Section 6.1. An auth
// rejection carries no JSON-RPC code (it is a typed error) and is answered 401; the
// table names both 401 and 403, and this implementation uses 401 for both auth shapes.
func (h *httpHandler) writeHandlerError(w http.ResponseWriter, r *http.Request, id core.ID, err error) {
	var authErr *core.AuthError
	if errors.As(err, &authErr) || errors.Is(err, core.ErrAuth) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	if errors.Is(err, core.ErrMethodNotFound) {
		h.writeResponse(w, r, errorResponse(id, core.CodeMethodNotFound, "Method not found"), http.StatusNotFound)
		return
	}

	var unsupported *core.UnsupportedProtocolVersionError
	if errors.As(err, &unsupported) {
		h.writeResponse(w, r, versionErrorResponse(id, unsupported), http.StatusBadRequest)
		return
	}

	var negotiation *core.NegotiationError
	if errors.As(err, &negotiation) {
		// Section 7.2's data shape (era/supported/requested) is not pinned by the
		// contract for the modern path; the code and status are.
		h.writeResponse(w, r, errorResponse(id, core.CodeUnsupportedVersion, "Unsupported protocol version"), http.StatusBadRequest)
		return
	}

	h.writeResponse(w, r, errorResponse(id, core.CodeInternalError, "Internal error"), http.StatusInternalServerError)
}

// errorResponse builds a JSON-RPC error response carrying code and message. A nil id
// marshals to `"id":null`, which is what a parse or invalid-request error carries.
func errorResponse(id core.ID, code int, message string) *core.Response {
	return &core.Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &core.Error{Code: code, Message: message},
	}
}

// versionErrorResponse builds the -32022 error for an unsupported protocol version,
// carrying the Section 7.1 data shape: the requested revision and the supported set.
func versionErrorResponse(id core.ID, e *core.UnsupportedProtocolVersionError) *core.Response {
	data, err := json.Marshal(struct {
		Supported []string `json:"supported"`
		Requested string   `json:"requested"`
	}{Supported: e.Supported, Requested: e.Requested})
	if err != nil {
		// A []string and a string always marshal; this is unreachable in practice.
		return errorResponse(id, core.CodeUnsupportedVersion, "Unsupported protocol version")
	}
	return &core.Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &core.Error{Code: core.CodeUnsupportedVersion, Message: "Unsupported protocol version", Data: data},
	}
}

// invalidRequestMessage is the human-readable message for the two framing-level errors
// decodeHTTPRequest reports. The message text is not part of the contract.
func invalidRequestMessage(code int) string {
	switch code {
	case core.CodeParseError:
		return "Parse error"
	default:
		return "Invalid Request"
	}
}

// acceptsJSON reports whether the request's Accept lists application/json, which selects
// a JSON body over an SSE body (decision 0007). Media types are case-insensitive.
func acceptsJSON(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json")
}
