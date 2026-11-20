package core

import (
	"encoding/json"
	"fmt"
)

// jsonRPCVersion is the only value the envelope's `jsonrpc` member may carry (V1).
// It is unexported: the contract's public surface (core-api.md) lists no version
// constant, and decision 0002 adds no new identifier.
const jsonRPCVersion = "2.0"

// ID is a JSON-RPC request identifier: a JSON string or number. It is an alias for
// json.RawMessage so the raw form survives a round trip byte-for-byte — including ids
// that are not representable as a float64 (data-model.md Section 1).
type ID = json.RawMessage

// Request is a decoded JSON-RPC 2.0 request.
//
// A frame that carries a method but no id is a notification; it decodes to a Request
// whose ID is nil (wire-protocol.md Section 1.4).
type Request struct {
	JSONRPC string          `json:"jsonrpc"` // MUST be "2.0"
	ID      ID              `json:"id"`      // correlation id; nil for a notification
	Method  string          `json:"method"`  // e.g. "tools/call", "server/discover"
	Params  json.RawMessage `json:"params,omitempty"`
}

// Notification is an id-less message; no response is ever returned for it (FR-001, FR-009).
// It MUST NOT carry id, result, or error (wire-protocol.md Section 1.4).
type Notification struct {
	JSONRPC string          `json:"jsonrpc"` // MUST be "2.0"
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a decoded JSON-RPC 2.0 response. Exactly one of Result and Error is set —
// never both, never neither (V2).
type Response struct {
	JSONRPC string          `json:"jsonrpc"` // MUST be "2.0"
	ID      ID              `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is the JSON-RPC 2.0 error object (FR-001, FR-018).
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Encode marshals a request, response, or notification to a single JSON frame. It is
// validating rather than lenient: an envelope whose `jsonrpc` member is not exactly
// "2.0", or a Response that does not carry exactly one of result/error, is reported as an
// error and no frame bytes are returned. Nothing is normalised — the caller-supplied
// version is never rewritten (V1, V2; docs/decisions/0002-codec-validation.md).
//
// The value must be one of the codec's own envelope types (Request, *Request, Response,
// *Response, Notification, or *Notification); any other type is a programming error and
// is rejected rather than marshalled.
func Encode(v any) ([]byte, error) {
	switch env := v.(type) {
	case *Request:
		if err := validateJSONRPC(env.JSONRPC); err != nil {
			return nil, err
		}
	case Request:
		if err := validateJSONRPC(env.JSONRPC); err != nil {
			return nil, err
		}
	case *Response:
		if err := validateJSONRPC(env.JSONRPC); err != nil {
			return nil, err
		}
		if err := validateResultOrError(env); err != nil {
			return nil, err
		}
	case Response:
		if err := validateJSONRPC(env.JSONRPC); err != nil {
			return nil, err
		}
		if err := validateResultOrError(&env); err != nil {
			return nil, err
		}
	case *Notification:
		if err := validateJSONRPC(env.JSONRPC); err != nil {
			return nil, err
		}
	case Notification:
		if err := validateJSONRPC(env.JSONRPC); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("core: Encode: unsupported envelope type %T", v)
	}

	frame, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("core: Encode: %w", err)
	}
	return frame, nil
}

// Decode parses one JSON frame into a *Request or *Response, returned in an `any`. A
// frame that carries a method decodes to a *Request (an id-less one is a notification,
// whose ID is nil); a frame that carries exactly one of result/error decodes to a
// *Response. A malformed body — not a single JSON object, `jsonrpc` missing or not
// exactly "2.0", or a Response without exactly one of result/error — is rejected with an
// error and no message (V1, V2, FR-018).
func Decode(frame []byte) (any, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(frame, &members); err != nil {
		return nil, fmt.Errorf("core: Decode: malformed body: %w", err)
	}
	// json.Unmarshal accepts `null` as a nil map and an empty object as an empty map;
	// neither is a JSON-RPC envelope.
	if members == nil {
		return nil, fmt.Errorf("core: Decode: malformed body: not a JSON object")
	}

	// V1: the `jsonrpc` member MUST be present and exactly "2.0". A non-string value
	// fails to unmarshal into a string; an absent or `null` one yields "".
	var version string
	if raw, ok := members["jsonrpc"]; ok {
		if err := json.Unmarshal(raw, &version); err != nil {
			return nil, fmt.Errorf("core: Decode: jsonrpc must be a string: %w", err)
		}
	}
	if err := validateJSONRPC(version); err != nil {
		return nil, err
	}

	_, hasMethod := members["method"]
	if hasMethod {
		req := new(Request)
		if err := json.Unmarshal(frame, req); err != nil {
			return nil, fmt.Errorf("core: Decode: %w", err)
		}
		return req, nil
	}

	// No method: this can only be a Response, which carries exactly one of result/error (V2).
	_, hasResult := members["result"]
	_, hasError := members["error"]
	if hasResult == hasError {
		return nil, fmt.Errorf("core: Decode: response must carry exactly one of result/error (V2)")
	}

	resp := new(Response)
	if err := json.Unmarshal(frame, resp); err != nil {
		return nil, fmt.Errorf("core: Decode: %w", err)
	}
	return resp, nil
}

// validateJSONRPC reports an error unless version is exactly "2.0".
func validateJSONRPC(version string) error {
	if version != jsonRPCVersion {
		return fmt.Errorf("core: jsonrpc must be exactly %q, got %q (V1)", jsonRPCVersion, version)
	}
	return nil
}

// validateResultOrError reports an error unless r carries exactly one of result/error (V2).
func validateResultOrError(r *Response) error {
	hasResult := len(r.Result) > 0
	hasError := r.Error != nil
	if hasResult == hasError {
		return fmt.Errorf("core: response must carry exactly one of result/error, not both or neither (V2)")
	}
	return nil
}
