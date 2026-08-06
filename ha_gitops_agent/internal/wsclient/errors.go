package wsclient

import (
	"encoding/json"
	"fmt"
)

// Error is the only error type Client.Dial and Client.Cmd let escape;
// every transport, protocol and Home Assistant failure becomes one.
type Error struct {
	// Code is machine-readable: "timeout" (a client-side wait expired),
	// "protocol_error" (unexpected handshake message, or a response that
	// is not a JSON object), "transport" (any socket-level failure), or a
	// Home Assistant error.code passed through verbatim.
	Code string
	// Message is human-readable and safe to log or show in the status UI:
	// it never contains the token (see (*Client).authenticate).
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func newError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// parseMessage parses one incoming frame as a JSON object, returning a
// "protocol_error" *Error for invalid JSON or anything that is not an
// object - the API sends neither, but neither may escape as a raw decode
// error or a panic from treating a non-map as one.
func parseMessage(raw []byte) (map[string]any, *Error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, newError("protocol_error", fmt.Sprintf("non-JSON message: %v", err))
	}
	obj, ok := decoded.(map[string]any)
	if !ok {
		return nil, newError("protocol_error", fmt.Sprintf("expected a JSON object, got %s", jsonTypeName(decoded)))
	}
	return obj, nil
}

// jsonTypeName names v's JSON type, for protocol_error messages.
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	default:
		return fmt.Sprintf("%T", v)
	}
}
