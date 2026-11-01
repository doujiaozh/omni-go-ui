package ui_bind

import "errors"

// Wire error codes (docs/PROTOCOL.md §5). The client maps these onto
// err.code, which lets UI code distinguish "you called it wrong" from
// "the backend broke".
const (
	CodeBadRequest = "BAD_REQUEST" // malformed frame / unknown frame type
	CodeNotFound   = "NOT_FOUND"   // no such method
	CodeBadParams  = "BAD_PARAMS"  // argument failed to decode / validate
	CodeInternal   = "INTERNAL"    // handler error or panic
	CodeTooLarge   = "TOO_LARGE"   // frame above the negotiated maximum
	CodeOffline    = "OFFLINE"     // client-side only: no socket, no reply possible
)

// rpcErrorBody is the serialized shape of frame.error.
type rpcErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RPCError is an error a handler can return to attach a wire error code.
// Returning a plain error surfaces as INTERNAL.
func codeOf(err error) string {
	var re *RPCError
	if errors.As(err, &re) {
		return re.Code
	}
	return CodeInternal
}

func messageOf(err error) string {
	if err == nil {
		return ""
	}
	var re *RPCError
	if errors.As(err, &re) {
		return re.Message
	}
	return err.Error()
}

// RPCError is the error type returned by NewError; it satisfies error.
type RPCError struct {
	Code    string
	Message string
}

// NewError builds a coded error for a handler to return, for example
// `return nil, ui_bind.NewError(ui_bind.CodeBadParams, "limit must be > 0")`.
func NewError(code, message string) *RPCError {
	return &RPCError{Code: code, Message: message}
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Code + ": " + e.Message
}
