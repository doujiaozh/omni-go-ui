// Package ui_bind is the bidirectional data-binding layer of Omni-Go-UI:
// a from-scratch RFC 6455 WebSocket server plus a tiny JSON-RPC dispatcher,
// with zero third-party dependencies.
//
// Why hand-roll the protocol instead of using gorilla/websocket? Because the
// framework's whole reason to exist is "one static .exe, no toolchain, no
// runtime": every external module is a build-time dependency, a supply-chain
// surface and a Win7 compatibility question. RFC 6455 for a server that only
// needs text frames on loopback is about 200 lines of straightforward code.
package ui_bind

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	wsGUID            = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	defaultMaxFrame   = 1 << 20 // 1 MiB in each direction (docs/PROTOCOL.md §1)
	maxControlPayload = 125     // RFC 6455 §5.5
	clientSendBuffer  = 64      // queued frames per socket before backpressure kicks in
)

// WebSocket opcodes (RFC 6455 §5.2).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xa
)

var (
	// errHijacked marks a failure that happened after the connection was
	// hijacked: the caller must not touch the http.ResponseWriter anymore.
	errHijacked = errors.New("ui_bind: connection already hijacked")
	errProtocol = errors.New("ui_bind: websocket protocol error")
	errClosed   = errors.New("ui_bind: connection closed")
	errTooLarge = errors.New("ui_bind: frame exceeds the maximum size")
)

func isControl(op byte) bool { return op >= opClose }

// acceptKey computes the Sec-WebSocket-Accept handshake value (RFC 6455 §1.3):
// base64(SHA-1(Sec-WebSocket-Key + GUID)).
func acceptKey(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key)
	_, _ = io.WriteString(h, wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// headerHasToken checks a comma-separated header for a case-insensitive token.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// Conn is a minimal server-side WebSocket connection: text frames in, text
// frames out. It intentionally implements neither compression nor
// sub-protocols: nothing in this framework needs them.
type Conn struct {
	conn      net.Conn
	br        *bufio.Reader
	maxFrame  int64
	wmu       sync.Mutex
	closeOnce sync.Once
	closed    bool
}

// Upgrade negotiates the handshake on an http request. On success the caller
// owns the connection and must Close it. Two error kinds are possible:
// pre-hijack errors (safe to answer with http.Error) and errHijacked (the
// ResponseWriter is dead, the socket is already closed).
func Upgrade(w http.ResponseWriter, r *http.Request, maxFrame int64) (*Conn, error) {
	if maxFrame <= 0 {
		maxFrame = defaultMaxFrame
	}
	if r.Method != http.MethodGet {
		return nil, errors.New("ui_bind: websocket upgrade requires GET")
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!headerHasToken(r.Header, "Connection", "upgrade") {
		return nil, errors.New("ui_bind: not a websocket upgrade request")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, errors.New("ui_bind: unsupported Sec-WebSocket-Version (need 13)")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, errors.New("ui_bind: missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("ui_bind: response writer does not support hijacking")
	}
	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("ui_bind: hijack: %w", err)
	}
	// Whatever deadlines the HTTP server applied no longer apply: this socket
	// will live for the whole app session.
	_ = netConn.SetDeadline(time.Time{})

	handshake := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n" +
		"\r\n"
	if _, err := netConn.Write([]byte(handshake)); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: %v", errHijacked, err)
	}

	// brw.Reader may already hold bytes the browser sent right after the
	// handshake, so every read must drain it first.
	return &Conn{conn: netConn, br: brw.Reader, maxFrame: maxFrame}, nil
}

// RemoteAddr returns the peer address (mainly "127.0.0.1:port").
func (c *Conn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// ReadMessage returns the next complete (defragmented) message and its opcode.
// Ping/pong/close are handled inside; a closed socket surfaces as errClosed.
func (c *Conn) ReadMessage() (opcode byte, payload []byte, err error) {
	var msg []byte
	var msgOp byte
	for {
		fin, op, p, rerr := c.readFrame()
		if rerr != nil {
			return 0, nil, rerr
		}
		switch op {
		case opPing:
			// Control frames may arrive mid-fragmentation; answer immediately.
			_ = c.WriteControl(opPong, p)
			continue
		case opPong:
			continue
		case opClose:
			_ = c.WriteControl(opClose, closeEcho(p))
			return 0, nil, errClosed
		case opContinuation:
			if msg == nil {
				return 0, nil, errProtocol
			}
			msg = append(msg, p...)
		case opText, opBinary:
			if msg != nil {
				return 0, nil, errProtocol
			}
			msgOp, msg = op, append(msg, p...)
		default:
			return 0, nil, errProtocol
		}
		if int64(len(msg)) > c.maxFrame {
			return 0, nil, errTooLarge
		}
		if fin {
			return msgOp, msg, nil
		}
	}
}

// readFrame parses exactly one frame, unmasking the payload.
func (c *Conn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, err
	}

	fin = head[0]&0x80 != 0
	if head[0]&0x70 != 0 { // RSV1..3: no extensions were negotiated
		return false, 0, nil, errProtocol
	}
	opcode = head[0] & 0x0f

	if head[1]&0x80 == 0 {
		// RFC 6455 §5.1: the client MUST mask every frame.
		return false, 0, nil, errProtocol
	}
	length := int64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		v := binary.BigEndian.Uint64(ext[:])
		if v>>63 != 0 {
			return false, 0, nil, errProtocol
		}
		length = int64(v)
	}

	if isControl(opcode) && (!fin || length > maxControlPayload) {
		return false, 0, nil, errProtocol
	}
	if length > c.maxFrame {
		return false, 0, nil, errTooLarge
	}

	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return false, 0, nil, err
	}
	if length > 0 {
		payload = make([]byte, length)
		if _, err = io.ReadFull(c.br, payload); err != nil {
			return false, 0, nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return fin, opcode, payload, nil
}

// WriteMessage sends a complete text message.
func (c *Conn) WriteMessage(payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeLocked(opText, payload)
}

// WriteControl sends a control frame (ping/pong/close).
func (c *Conn) WriteControl(opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeLocked(opcode, payload)
}

// writeLocked emits one frame. Servers never mask (RFC 6455 §5.1) and always
// set FIN: this layer does not fragment outgoing messages.
func (c *Conn) writeLocked(opcode byte, payload []byte) error {
	if c.closed {
		return errClosed
	}
	if isControl(opcode) && len(payload) > maxControlPayload {
		return errProtocol
	}

	var head [10]byte
	head[0] = 0x80 | opcode
	n := 2
	switch l := len(payload); {
	case l < 126:
		head[1] = byte(l)
	case l <= 0xffff:
		head[1] = 126
		binary.BigEndian.PutUint16(head[2:], uint16(l))
		n = 4
	default:
		head[1] = 127
		binary.BigEndian.PutUint64(head[2:], uint64(l))
		n = 10
	}

	// One buffer, one syscall per frame: a 1 Hz dashboard on a Win7 box has
	// better things to do than generate writev swallowtails.
	buf := make([]byte, n+len(payload))
	copy(buf, head[:n])
	copy(buf[n:], payload)
	if _, err := c.conn.Write(buf); err != nil {
		return err
	}
	return nil
}

// Close sends a normal-closure frame and tears the socket down. Idempotent.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.wmu.Lock()
		c.closed = true
		_ = c.writeLocked(opClose, []byte{0x03, 0xE8}) // 1000 Normal Closure
		c.wmu.Unlock()
		err = c.conn.Close()
	})
	return err
}

// closeEcho extracts the status code a client sent so we can mirror it.
func closeEcho(p []byte) []byte {
	if len(p) >= 2 {
		return p[:2]
	}
	return []byte{0x03, 0xE8}
}
