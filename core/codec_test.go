package core

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// These tests pin wire-protocol.md Section 1 and the V1/V2 validation rules: every
// envelope's `jsonrpc` member is "2.0", a Response carries exactly one of
// result/error, a notification carries no id, and a malformed body is rejected
// rather than decoded (FR-001, FR-018).
//
// Encode is exercised as the validating marshaler the contract now defines: given a
// well-formed envelope it puts a well-formed frame on the wire, and given an envelope
// that violates V1 or V2 it reports an error and returns no frame bytes. That
// sending-side policy is a project decision rather than a reading of V1/V2 on their own
// (V1 rejects at dispatch, FR-018 at the server): docs/decisions/0002-codec-validation.md.

// jsonRPC20 is the only accepted value of the envelope's `jsonrpc` member (V1).
// It is spelled out here instead of reusing a package constant so that changing
// the codec's own constant cannot silently relax these assertions.
const jsonRPC20 = "2.0"

// JSON-RPC error codes in this file are literals on purpose: the named constants
// (CodeInvalidParams and friends) are owned by errors.go (T015), and this file
// must compile — and fail — against the codec alone.

func TestEncodeDecodeRoundTripRequest(t *testing.T) {
	tests := []struct {
		name string
		in   *Request
	}{
		{
			name: "integer id and no params",
			in:   &Request{JSONRPC: jsonRPC20, ID: json.RawMessage("1"), Method: "tools/list"},
		},
		{
			name: "string id and params",
			in: &Request{
				JSONRPC: jsonRPC20,
				ID:      json.RawMessage(`"req-1"`),
				Method:  "tools/call",
				Params:  json.RawMessage(`{"name":"echo","arguments":{"text":"hello"}}`),
			},
		},
		{
			// data-model.md Section 1: an id's raw form is preserved so ids round-trip
			// byte-for-byte. This value is not representable as a float64.
			name: "id beyond float64 precision",
			in:   &Request{JSONRPC: jsonRPC20, ID: json.RawMessage("9007199254740993"), Method: "server/discover"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeRequest(t, mustEncode(t, tt.in))
			if !reflect.DeepEqual(got, tt.in) {
				t.Errorf("round trip changed the request: got %+v, want %+v", got, tt.in)
			}
		})
	}
}

func TestEncodeDecodeRoundTripResponse(t *testing.T) {
	tests := []struct {
		name string
		in   *Response
	}{
		{
			name: "result",
			in: &Response{
				JSONRPC: jsonRPC20,
				ID:      json.RawMessage("1"),
				Result:  json.RawMessage(`{"resultType":"complete","content":[{"type":"text","text":"hello"}]}`),
			},
		},
		{
			name: "error with data",
			in: &Response{
				JSONRPC: jsonRPC20,
				ID:      json.RawMessage("2"),
				Error: &Error{
					Code:    -32602,
					Message: "invalid params: missing io.modelcontextprotocol/protocolVersion",
					Data:    json.RawMessage(`{"field":"io.modelcontextprotocol/protocolVersion"}`),
				},
			},
		},
		{
			name: "error without data",
			in: &Response{
				JSONRPC: jsonRPC20,
				ID:      json.RawMessage(`"req-2"`),
				Error:   &Error{Code: -32601, Message: "method not found"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame := mustEncode(t, tt.in)

			members := membersOf(t, frame)
			_, hasResult := members["result"]
			_, hasError := members["error"]
			if hasResult == hasError {
				t.Errorf("encoded response has result=%t and error=%t, want exactly one (V2): %s",
					hasResult, hasError, frame)
			}

			got := decodeResponse(t, frame)
			if !reflect.DeepEqual(got, tt.in) {
				t.Errorf("round trip changed the response: got %+v, want %+v", got, tt.in)
			}
		})
	}
}

func TestEncodeDecodeRoundTripNotification(t *testing.T) {
	tests := []struct {
		name string
		in   *Notification
	}{
		{
			name: "no params",
			in:   &Notification{JSONRPC: jsonRPC20, Method: "notifications/tools/list_changed"},
		},
		{
			name: "with params",
			in: &Notification{
				JSONRPC: jsonRPC20,
				Method:  "notifications/resources/updated",
				Params:  json.RawMessage(`{"uri":"file:///tmp/notes.txt"}`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeRequest(t, mustEncode(t, tt.in))
			if len(got.ID) != 0 {
				t.Errorf("round trip gave the notification id %s, want none", got.ID)
			}
			if got.Method != tt.in.Method {
				t.Errorf("round trip changed the method: got %q, want %q", got.Method, tt.in.Method)
			}
			if !bytes.Equal(got.Params, tt.in.Params) {
				t.Errorf("round trip changed the params: got %s, want %s", got.Params, tt.in.Params)
			}
		})
	}
}

// TestNotificationCarriesNoID covers wire-protocol.md Section 1.4: an encoded
// notification MUST NOT contain id, result, or error, and an id-less frame is a
// notification — decoded as a Request whose ID is nil (core-api.md, Request.ID).
func TestNotificationCarriesNoID(t *testing.T) {
	in := &Notification{JSONRPC: jsonRPC20, Method: "notifications/tools/list_changed"}
	frame := mustEncode(t, in)

	for _, forbidden := range []string{"id", "result", "error"} {
		if _, ok := membersOf(t, frame)[forbidden]; ok {
			t.Errorf("encoded notification carries %q: %s", forbidden, frame)
		}
	}

	got := decodeRequest(t, frame)
	if len(got.ID) != 0 {
		t.Errorf("decoded notification carries id %s, want none", got.ID)
	}
	if got.Method != in.Method {
		t.Errorf("decoded notification has method %q, want %q", got.Method, in.Method)
	}
}

func TestEncodeAlwaysEmitsJSONRPC20(t *testing.T) {
	tests := []struct {
		name string
		in   any
	}{
		{
			name: "request",
			in:   &Request{JSONRPC: jsonRPC20, ID: json.RawMessage("1"), Method: "tools/list"},
		},
		{
			name: "response with result",
			in:   &Response{JSONRPC: jsonRPC20, ID: json.RawMessage("1"), Result: json.RawMessage(`{"resultType":"complete"}`)},
		},
		{
			name: "response with error",
			in:   &Response{JSONRPC: jsonRPC20, ID: json.RawMessage("1"), Error: &Error{Code: -32601, Message: "method not found"}},
		},
		{
			name: "notification",
			in:   &Notification{JSONRPC: jsonRPC20, Method: "notifications/tools/list_changed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame := mustEncode(t, tt.in)
			if got := membersOf(t, frame)["jsonrpc"]; !bytes.Equal(got, json.RawMessage(`"2.0"`)) {
				t.Errorf("encoded %s carries jsonrpc %s, want %q (V1): %s", tt.name, got, jsonRPC20, frame)
			}
		})
	}
}

// TestEncodeRejectsEnvelopeWithoutJSONRPC20 pins V1 in the sending direction
// (decision 0002): the codec MUST NOT put an envelope on the wire whose jsonrpc member
// is anything but "2.0". A caller's explicit, wrong version is an error rather than
// silently rewritten, so the field never lies about what was sent.
func TestEncodeRejectsEnvelopeWithoutJSONRPC20(t *testing.T) {
	tests := []struct {
		name string
		in   any
	}{
		{
			name: "request with no jsonrpc member",
			in:   &Request{ID: json.RawMessage("1"), Method: "tools/list"},
		},
		{
			name: "request with another JSON-RPC version",
			in:   &Request{JSONRPC: "1.0", ID: json.RawMessage("1"), Method: "tools/list"},
		},
		{
			name: "request whose jsonrpc carries an MCP revision",
			in:   &Request{JSONRPC: "2026-07-28", ID: json.RawMessage("1"), Method: "tools/list"},
		},
		{
			name: "response with no jsonrpc member",
			in:   &Response{ID: json.RawMessage("1"), Result: json.RawMessage(`{"resultType":"complete"}`)},
		},
		{
			name: "notification with another JSON-RPC version",
			in:   &Notification{JSONRPC: "1.0", Method: "notifications/tools/list_changed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame, err := Encode(tt.in)
			if err == nil {
				t.Fatalf("Encode(%+v) = %s, want an error: a non-%q envelope must not reach the wire",
					tt.in, frame, jsonRPC20)
			}
			if len(frame) != 0 {
				t.Errorf("Encode returned %d bytes alongside its error: %s", len(frame), frame)
			}
		})
	}
}

// TestEncodeRejectsResponseWithoutExactlyOneOfResultOrError pins V2 in the sending
// direction (decision 0002): a Response carrying both members, or neither, is rejected
// rather than emitted.
func TestEncodeRejectsResponseWithoutExactlyOneOfResultOrError(t *testing.T) {
	tests := []struct {
		name string
		in   *Response
	}{
		{
			name: "both result and error",
			in: &Response{
				JSONRPC: jsonRPC20,
				ID:      json.RawMessage("1"),
				Result:  json.RawMessage(`{"resultType":"complete"}`),
				Error:   &Error{Code: -32603, Message: "internal error"},
			},
		},
		{
			name: "neither result nor error",
			in:   &Response{JSONRPC: jsonRPC20, ID: json.RawMessage("1")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame, err := Encode(tt.in)
			if err == nil {
				t.Fatalf("Encode(%+v) = %s, want an error: a Response carries exactly one of result/error (V2)",
					tt.in, frame)
			}
			if len(frame) != 0 {
				t.Errorf("Encode returned %d bytes alongside its error: %s", len(frame), frame)
			}
		})
	}
}

// TestDecodeRequiresJSONRPC20 pins V1: a frame whose jsonrpc member is missing, is
// another version, or is not a string is not a JSON-RPC 2.0 envelope
// (wire-protocol.md Section 6, "-32600 ... bad/missing jsonrpc").
func TestDecodeRequiresJSONRPC20(t *testing.T) {
	tests := []struct {
		name  string
		frame string
	}{
		{"no jsonrpc member", `{"id":1,"method":"tools/list"}`},
		{"empty jsonrpc", `{"jsonrpc":"","id":1,"method":"tools/list"}`},
		{"another JSON-RPC version", `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`},
		{"MCP revision used as the version", `{"jsonrpc":"2026-07-28","id":1,"method":"tools/list"}`},
		{"jsonrpc as a number", `{"jsonrpc":2,"id":1,"method":"tools/list"}`},
		{"jsonrpc as null", `{"jsonrpc":null,"id":1,"method":"tools/list"}`},
		{"jsonrpc in a response", `{"jsonrpc":"1.0","id":1,"result":{"resultType":"complete"}}`},
		{"no jsonrpc in a notification", `{"method":"notifications/tools/list_changed"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertDecodeError(t, tt.frame)
		})
	}
}

// TestDecodeRequiresExactlyOneOfResultOrError pins V2: a frame carrying both
// members, or neither, is never decoded into a Response.
func TestDecodeRequiresExactlyOneOfResultOrError(t *testing.T) {
	rejected := []struct {
		name  string
		frame string
	}{
		{
			name:  "both result and error",
			frame: `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete"},"error":{"code":-32603,"message":"internal error"}}`,
		},
		{
			// Neither a Response (no result, no error) nor a Request (no method).
			name:  "neither result nor error",
			frame: `{"jsonrpc":"2.0","id":1}`,
		},
	}

	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			assertDecodeError(t, tt.frame)
		})
	}

	t.Run("result only decodes to a response without an error", func(t *testing.T) {
		frame := `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete"}}`
		got := decodeResponse(t, frame)
		if got.Error != nil {
			t.Errorf("Decode(%s) set Error = %+v, want none", frame, got.Error)
		}
		if len(got.Result) == 0 {
			t.Errorf("Decode(%s) left Result empty, want the result member", frame)
		}
	})

	t.Run("error only decodes to a response without a result", func(t *testing.T) {
		frame := `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`
		got := decodeResponse(t, frame)
		if len(got.Result) != 0 {
			t.Errorf("Decode(%s) set Result = %s, want none", frame, got.Result)
		}
		if got.Error == nil {
			t.Fatalf("Decode(%s) left Error nil, want the error member", frame)
		}
		if got.Error.Code != -32601 || got.Error.Message != "method not found" {
			t.Errorf("Decode(%s) decoded error %+v, want code %d and message %q",
				frame, got.Error, -32601, "method not found")
		}
	})
}

// TestDecodeRejectsMalformedBody pins the first half of V1 — malformed bodies are
// rejected and never dispatched — and wire-protocol.md Section 1: every message is
// a single JSON object, so a frame that is not one (including a batch array, which
// MCP does not frame) is malformed.
func TestDecodeRejectsMalformedBody(t *testing.T) {
	tests := []struct {
		name  string
		frame string
	}{
		{"empty frame", ``},
		{"whitespace only", `   `},
		{"not JSON at all", `{jsonrpc: 2.0}`},
		{"truncated JSON", `{"jsonrpc":"2.0","id":1,"method":"tools/list"`},
		{"JSON string", `"tools/list"`},
		{"JSON number", `42`},
		{"JSON boolean", `true`},
		{"JSON null", `null`},
		{"JSON array", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertDecodeError(t, tt.frame)
		})
	}
}

// mustEncode encodes v and fails the test if the codec rejects it.
func mustEncode(t *testing.T, v any) string {
	t.Helper()

	frame, err := Encode(v)
	if err != nil {
		t.Fatalf("Encode(%+v) returned an unexpected error: %v", v, err)
	}
	if len(frame) == 0 {
		t.Fatalf("Encode(%+v) returned an empty frame", v)
	}
	return string(frame)
}

// decodeRequest decodes frame and fails the test unless it holds a *Request. An
// id-less frame is a notification and decodes to a *Request whose ID is nil
// (core-api.md, Request.ID).
func decodeRequest(t *testing.T, frame string) *Request {
	t.Helper()

	msg, err := Decode([]byte(frame))
	if err != nil {
		t.Fatalf("Decode(%s) returned an unexpected error: %v", frame, err)
	}
	req, ok := msg.(*Request)
	if !ok {
		t.Fatalf("Decode(%s) returned %T, want *Request", frame, msg)
	}
	return req
}

// decodeResponse decodes frame and fails the test unless it holds a *Response.
func decodeResponse(t *testing.T, frame string) *Response {
	t.Helper()

	msg, err := Decode([]byte(frame))
	if err != nil {
		t.Fatalf("Decode(%s) returned an unexpected error: %v", frame, err)
	}
	resp, ok := msg.(*Response)
	if !ok {
		t.Fatalf("Decode(%s) returned %T, want *Response", frame, msg)
	}
	return resp
}

// assertDecodeError requires Decode to reject frame with an error and no message:
// a malformed body is rejected rather than decoded (V1, FR-018).
func assertDecodeError(t *testing.T, frame string) {
	t.Helper()

	msg, err := Decode([]byte(frame))
	if err == nil {
		t.Fatalf("Decode(%q) = %#v, nil; want an error: a malformed body is rejected, never decoded", frame, msg)
	}
	if msg != nil {
		t.Errorf("Decode(%q) returned %#v alongside its error; want no message", frame, msg)
	}
}

// membersOf returns the frame's top-level members, failing the test if the frame
// is not a JSON object.
func membersOf(t *testing.T, frame string) map[string]json.RawMessage {
	t.Helper()

	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(frame), &members); err != nil {
		t.Fatalf("encoded frame is not a JSON object: %s (%v)", frame, err)
	}
	if members == nil {
		t.Fatalf("encoded frame is not a JSON object: %s", frame)
	}
	return members
}
