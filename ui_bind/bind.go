package ui_bind

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

var (
	ctxType = reflect.TypeOf((*Context)(nil))
	errType = reflect.TypeOf((*error)(nil)).Elem()
)

// method is one registered RPC handler plus everything reflection needs to
// invoke it without re-inspecting the signature on every call.
type method struct {
	name    string
	fn      reflect.Value
	argType reflect.Type // nil when the handler takes no argument
	hasCtx  bool         // first parameter is *ui_bind.Context
	hasArg  bool
	resIdx  int // index of the result value, -1 when none
	errIdx  int // index of the error value, -1 when none
}

// newMethod inspects a handler func and pins how to call it. Accepted shapes:
//
//	func ()
//	func (T)
//	func () R
//	func () error
//	func (T) R
//	func (T) error
//	func () (R, error)
//	func (*Context, ...)
//	func (*Context, ...) (R, error)
//
// T must be JSON-decodable; R must be JSON-encodable. Registration errors are
// programming mistakes, so they panic (like http.HandleFunc).
func newMethod(name string, fn any) (*method, error) {
	if name == "" {
		return nil, errors.New("empty method name")
	}
	if fn == nil {
		return nil, errors.New("nil handler")
	}
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		return nil, errors.New("handler must be a func")
	}
	t := v.Type()

	m := &method{
		name:   name,
		fn:     v,
		resIdx: -1,
		errIdx: -1,
	}
	if n := t.NumIn(); n > 2 {
		return nil, errors.New("too many parameters: want (), (T), (*Context) or (*Context, T)")
	}
	if n := t.NumOut(); n > 2 {
		return nil, errors.New("too many return values: want (), (R), (error) or (R, error)")
	}
	if t.NumIn() > 0 && t.In(0) == ctxType {
		m.hasCtx = true
	}
	m.hasArg = t.NumIn()-boolToInt(m.hasCtx) == 1
	if m.hasArg {
		m.argType = t.In(boolToInt(m.hasCtx))
	}
	switch t.NumOut() {
	case 2:
		if t.Out(1) != errType {
			return nil, errors.New("the second return value must be error")
		}
		m.resIdx = 0
		m.errIdx = 1
	case 1:
		if t.Out(0) == errType {
			m.errIdx = 0
		} else {
			m.resIdx = 0
		}
	}
	return m, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// call invokes the handler with the decoded JSON argument and turns a panic
// into a coded INTERNAL error: one broken handler must not kill the socket.
func (m *method) call(ctx *Context, raw []byte) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &RPCError{
				Code:    CodeInternal,
				Message: fmt.Sprintf("handler %q panicked: %v", m.name, r),
			}
			logRecover(ctx, m.name, r)
		}
	}()

	var in []reflect.Value
	if m.hasCtx {
		if ctx == nil {
			return nil, &RPCError{Code: CodeInternal, Message: "missing call context"}
		}
		in = append(in, reflect.ValueOf(ctx))
	}
	if m.hasArg {
		ptr := reflect.New(m.argType)
		// Missing or null parameters decode to the zero value of T, so
		// handler signatures read naturally even for omni.call("m").
		if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			if err := json.Unmarshal(raw, ptr.Interface()); err != nil {
				return nil, &RPCError{
					Code:    CodeBadParams,
					Message: fmt.Sprintf("decode argument for %s: %v", m.name, err),
				}
			}
		}
		in = append(in, ptr.Elem())
	}

	out := m.fn.Call(in)
	var res any
	if m.resIdx >= 0 {
		res = out[m.resIdx].Interface()
	}
	if m.errIdx >= 0 {
		if e, ok := out[m.errIdx].Interface().(error); ok && e != nil {
			var re *RPCError
			if errors.As(e, &re) {
				return nil, e
			}
			return nil, &RPCError{Code: CodeInternal, Message: e.Error()}
		}
	}
	return res, nil
}

// logRecover routes panics somewhere visible without depending on the hub.
func logRecover(ctx *Context, name string, r any) {
	if ctx != nil && ctx.client != nil && ctx.client.hub != nil {
		ctx.client.hub.logf("%s panicked: %v", name, r)
	}
}
