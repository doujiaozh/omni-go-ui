package ui_bind

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version of the binding layer and the wire protocol (docs/PROTOCOL.md §1).
const Version = "1.0.0"

// inFrame is the client -> server envelope. Params are held raw so each
// method decodes exactly its own argument type.
type inFrame struct {
	Type   string            `json:"type"`
	ID     json.RawMessage   `json:"id,omitempty"`
	Method string            `json:"method,omitempty"`
	Params []json.RawMessage `json:"params,omitempty"`
}

// out frames.
type resultFrame struct {
	Type string          `json:"type"`
	ID   json.RawMessage `json:"id,omitempty"`
	Data any             `json:"data"`
}

type errorFrame struct {
	Type  string          `json:"type"`
	ID    json.RawMessage `json:"id,omitempty"`
	Error rpcErrorBody    `json:"error"`
}

type eventFrame struct {
	Type  string `json:"type"`
	Event string `json:"event"`
	Data  any    `json:"data"`
}

type pongFrame struct {
	Type string          `json:"type"`
	ID   json.RawMessage `json:"id,omitempty"`
}

// Hello is the first frame on every connection: it tells the frontend which
// methods exist, what platform it is running on, and which perf profile to use.
type Hello struct {
	Type      string   `json:"type"`
	Version   string   `json:"version"`
	ClientID  string   `json:"clientID"`
	Methods   []string `json:"methods"`
	Platform  string   `json:"platform"`
	Arch      string   `json:"arch"`
	GoVersion string   `json:"goVersion"`
	Title     string   `json:"title"`
	Perf      string   `json:"perf"`
	Features  []string `json:"features,omitempty"`
	StartedAt int64    `json:"startedAt"`
}

// Context is the per-call view handed to handlers that take *Context. It
// identifies the caller and lets a handler push data back to that one client.
type Context struct {
	// ClientID is the opaque id of the calling window.
	ClientID string
	// Remote is the peer address, e.g. 127.0.0.1:54321.
	Remote string

	client *Client
}

// Emit pushes an event to the caller's window only. Returns whether the
// delivery was queued. Use Hub.Broadcast to reach every connected window.
func (c *Context) Emit(event string, data any) bool {
	if c == nil || c.client == nil {
		return false
	}
	return c.client.sendJSON(eventFrame{Type: "event", Event: event, Data: data})
}

// Hub is the RPC registry and connection manager. Create it with New, wire it
// to the core server with srv.Handle("/ws", hub.WebSocketHandler()),
// register Go methods with hub.Handle and push updates with hub.Broadcast.
type Hub struct {
	opts hubOptions

	mu           sync.RWMutex
	methods      map[string]*method
	order        []string
	clients      map[*Client]struct{}
	onConnect    []func(*Context)
	onDisconnect []func(*Context)
	closed       bool
	nextID       uint64
}

type hubOptions struct {
	debug       bool
	maxFrame    int64
	title       string
	perf        string
	features    []string
	checkOrigin func(*http.Request) bool
	logf        func(string, ...any)
	hello       func(*Hello)
	startedAt   time.Time
}

// Option configures a Hub.
type Option func(*hubOptions)

// WithDebug enables verbose protocol logging.
func WithDebug(debug bool) Option {
	return func(o *hubOptions) { o.debug = debug }
}

// WithMaxFrame overrides the 1 MiB frame ceiling in each direction.
func WithMaxFrame(bytes int64) Option {
	return func(o *hubOptions) { o.maxFrame = bytes }
}

// WithTitle is reported through hello.title and the console banner.
func WithTitle(title string) Option {
	return func(o *hubOptions) { o.title = title }
}

// WithPerf selects the perf profile announced to the frontend ("full"/"lite").
func WithPerf(perf string) Option {
	return func(o *hubOptions) { o.perf = perf }
}

// WithFeature adds a capability name advertised in hello.features.
// The demo uses it to tell the frontend whether the frameless window landed.
func WithFeature(features ...string) Option {
	return func(o *hubOptions) { o.features = append(o.features, features...) }
}

// WithCheckOrigin overrides the default Origin gate. Return true to accept.
func WithCheckOrigin(f func(*http.Request) bool) Option {
	return func(o *hubOptions) { o.checkOrigin = f }
}

// WithLogger replaces the standard logger used for diagnostics.
func WithLogger(f func(format string, args ...any)) Option {
	return func(o *hubOptions) { o.logf = f }
}

// WithHello mutates the hello frame just before it is sent, so an
// application can inject its own fields without rebuilding the hub.
func WithHello(f func(*Hello)) Option {
	return func(o *hubOptions) { o.hello = f }
}

// New builds an empty Hub. Methods are registered afterwards with Handle.
func New(opts ...Option) *Hub {
	o := hubOptions{
		title:       "Omni-Go-UI",
		perf:        "full",
		maxFrame:    defaultMaxFrame,
		startedAt:   time.Now(),
		checkOrigin: defaultOriginCheck,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return &Hub{
		opts:    o,
		methods: make(map[string]*method),
		clients: make(map[*Client]struct{}),
	}
}

// defaultOriginCheck accepts only requests whose Origin host equals the Host
// header. The whole server is loopback-only; this stops a web page opened in
// another tab from speaking to the app (docs/PROTOCOL.md §2).
func defaultOriginCheck(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser clients (curl, scripts) have no Origin; they were never
		// under the token-cookie model anyway, and the token gate already
		// covers this path. Accept, matching WebSocket's own default.
		return true
	}
	u := origin
	if i := strings.Index(origin, "://"); i >= 0 {
		u = origin[i+3:]
	}
	if j := strings.IndexAny(u, "/?#"); j >= 0 {
		u = u[:j]
	}
	return strings.EqualFold(u, r.Host)
}

// Handle registers an RPC method. Signature rules: see newMethod. Duplicate
// names replace the earlier handler. Bad signatures panic at startup, which is
// exactly when they are fixable.
func (h *Hub) Handle(name string, fn any) {
	m, err := newMethod(name, fn)
	if err != nil {
		panic("ui_bind: Handle(" + name + "): " + err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		panic("ui_bind: Handle(" + name + ") after Close")
	}
	if _, dup := h.methods[name]; !dup {
		h.order = append(h.order, name)
	}
	h.methods[name] = m
}

// Methods returns the registered method names in registration order.
func (h *Hub) Methods() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	names := make([]string, len(h.order))
	copy(names, h.order)
	return names
}

// OnConnect / OnDisconnect observe client lifecycle. The hooks receive a
// Context bound to the client; Emit on it is still valid inside OnDisconnect
// because the socket is drained afterwards.
func (h *Hub) OnConnect(fn func(*Context)) {
	h.mu.Lock()
	h.onConnect = append(h.onConnect, fn)
	h.mu.Unlock()
}

func (h *Hub) OnDisconnect(fn func(*Context)) {
	h.mu.Lock()
	h.onDisconnect = append(h.onDisconnect, fn)
	h.mu.Unlock()
}

// Clients reports the number of connected windows, so background samplers can
// stay idle when nobody is looking.
func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Broadcast pushes an event to every connected window and returns how many
// received it. The frame is marshalled once; each client gets a cheap copy.
func (h *Hub) Broadcast(event string, data any) int {
	buf, err := json.Marshal(eventFrame{Type: "event", Event: event, Data: data})
	if err != nil {
		h.logf("broadcast %s: %v", event, err)
		return 0
	}
	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return 0
	}
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	n := 0
	for _, c := range clients {
		if c.send(buf) {
			n++
		}
	}
	return n
}

// Close drops every client. The hub can no longer be extended with Handle.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	for _, c := range clients {
		c.shutdown()
	}
	return nil
}

// WebSocketHandler returns the http.Handler a core server mounts at /ws.
func (h *Hub) WebSocketHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.opts.checkOrigin(r) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		conn, err := Upgrade(w, r, h.opts.maxFrame)
		if err != nil {
			if !errors.Is(err, errHijacked) {
				http.Error(w, "websocket upgrade failed: "+err.Error(), http.StatusBadRequest)
			}
			return
		}
		h.serve(conn, r)
	})
}

// serve runs the read/write loops of one socket until it dies.
func (h *Hub) serve(conn *Conn, r *http.Request) {
	c := &Client{
		id:     h.clientID(),
		remote: r.RemoteAddr,
		hub:    h,
		conn:   conn,
		out:    make(chan []byte, clientSendBuffer),
	}
	c.ctx = &Context{ClientID: c.id, Remote: r.RemoteAddr, client: c}

	// Register before starting the writer so hello is truly the first frame.
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = conn.Close()
		return
	}
	h.clients[c] = struct{}{}
	hooks := append([]func(*Context){}, h.onConnect...)
	h.mu.Unlock()

	for _, fn := range hooks {
		safeCall(fn, c.ctx, "onConnect")
	}

	go c.writeLoop()
	_ = conn.WriteMessage(h.helloFor(c))
	h.readLoop(c)

	// Socket is gone: run hooks, drop the registration, close everything.
	h.mu.Lock()
	delete(h.clients, c)
	hooks = append([]func(*Context){}, h.onDisconnect...)
	h.mu.Unlock()
	for _, fn := range hooks {
		safeCall(fn, c.ctx, "onDisconnect")
	}
	c.shutdown()
}

func safeCall(fn func(*Context), ctx *Context, what string) {
	defer func() {
		if r := recover(); r != nil {
			// A broken observer must not take the connection down with it.
			if ctx != nil && ctx.client != nil && ctx.client.hub != nil {
				ctx.client.hub.logf("%s hook panicked: %v", what, r)
			}
		}
	}()
	fn(ctx)
}

// readLoop consumes frames until the client leaves.
func (h *Hub) readLoop(c *Client) {
	for {
		op, payload, err := c.conn.ReadMessage()
		if err != nil {
			if !errors.Is(err, errClosed) && !errors.Is(err, errTooLarge) {
				h.logf("read: %v", err)
			}
			return
		}
		if op != opText {
			c.sendJSON(errorFrame{Error: rpcErrorBody{
				Code: CodeBadRequest, Message: "binary frames are not supported; use text",
			}})
			continue
		}
		h.dispatch(c, payload)
	}
}

// dispatch decodes one client frame and routes it to a call handler or to the
// transport-level ping.
func (h *Hub) dispatch(c *Client, payload []byte) {
	var f inFrame
	if err := json.Unmarshal(payload, &f); err != nil {
		c.sendJSON(errorFrame{
			Error: rpcErrorBody{Code: CodeBadRequest, Message: "malformed json frame: " + err.Error()},
		})
		return
	}
	switch f.Type {
	case "call":
		h.handleCall(c, &f)
	case "ping":
		c.sendJSON(pongFrame{Type: "pong", ID: f.ID})
	default:
		c.sendJSON(errorFrame{
			ID: f.ID,
			Error: rpcErrorBody{
				Code:    CodeBadRequest,
				Message: "unknown frame type " + fmt.Sprintf("%q", f.Type),
			},
		})
	}
}

func (h *Hub) handleCall(c *Client, f *inFrame) {
	h.mu.RLock()
	m, ok := h.methods[f.Method]
	h.mu.RUnlock()
	if !ok {
		c.sendJSON(errorFrame{
			ID:    f.ID,
			Error: rpcErrorBody{Code: CodeNotFound, Message: "unknown method: " + f.Method},
		})
		return
	}
	var raw []byte
	if len(f.Params) > 0 {
		raw = f.Params[0]
	}
	res, err := m.call(c.ctx, raw)
	if err != nil {
		c.sendJSON(errorFrame{
			ID:    f.ID,
			Error: rpcErrorBody{Code: codeOf(err), Message: messageOf(err)},
		})
		return
	}
	// Handlers returning nothing still produce {"data":null}, which keeps the
	// JS side a single shape: omni.call(...) always yields an object or null.
	c.sendJSON(resultFrame{Type: "result", ID: f.ID, Data: res})
}

// helloFor builds the first frame for a fresh client.
func (h *Hub) helloFor(c *Client) []byte {
	h.mu.RLock()
	methods := append([]string{}, h.order...)
	features := append([]string{}, h.opts.features...)
	title, perf := h.opts.title, h.opts.perf
	h.mu.RUnlock()

	hl := Hello{
		Type:      "hello",
		Version:   Version,
		ClientID:  c.id,
		Methods:   methods,
		Platform:  runtime.GOOS,
		Arch:      runtime.GOARCH,
		GoVersion: runtime.Version(),
		Title:     title,
		Perf:      perf,
		Features:  features,
		StartedAt: h.opts.startedAt.Unix(),
	}
	if hook := h.opts.hello; hook != nil {
		safeHelloHook(hook, &hl)
	}
	b, err := json.Marshal(hl)
	if err != nil {
		return []byte(`{"type":"hello","version":"` + Version + `","error":"encode failed"}`)
	}
	return b
}

func safeHelloHook(fn func(*Hello), hl *Hello) {
	defer func() {
		if r := recover(); r != nil {
			_ = r
		}
	}()
	fn(hl)
}

func (h *Hub) clientID() string {
	n := atomic.AddUint64(&h.nextID, 1)
	return fmt.Sprintf("c%d", n)
}

// Client is one connected window.
type Client struct {
	id        string
	remote    string
	hub       *Hub
	conn      *Conn
	out       chan []byte
	ctx       *Context
	closed    atomic.Bool
	closeOnce sync.Once
}

// ID returns the client identifier announced in hello.clientID.
func (c *Client) ID() string { return c.id }

// send queues a pre-encoded frame; undefined payloads are rejected upstream.
func (c *Client) send(buf []byte) bool {
	if c.closed.Load() {
		return false
	}
	select {
	case c.out <- buf:
		return true
	default:
		// A client that stops draining (tab hidden, machine wedged) must never
		// wedge the broadcaster: drop the socket instead.
		c.hub.logf("client %s saturated (queued > %d), closing", c.id, clientSendBuffer)
		c.shutdown()
		return false
	}
}

// sendJSON marshals then sends.
func (c *Client) sendJSON(v any) bool {
	buf, err := json.Marshal(v)
	if err != nil {
		c.hub.logf("encode: %v", err)
		return false
	}
	return c.send(buf)
}

// shutdown closes both loops and the socket. Idempotent.
func (c *Client) shutdown() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.out) // writeLoop sees the channel close and returns
		_ = c.conn.Close()
	})
}

// writeLoop drains the send queue. Only goroutine writing to the socket, so
// the frame writer needs no internal locking here.
func (c *Client) writeLoop() {
	for buf := range c.out {
		if err := c.conn.WriteMessage(buf); err != nil {
			c.hub.logf("write to %s: %v", c.id, err)
			return
		}
	}
}

func (h *Hub) logf(format string, args ...any) {
	if f := h.opts.logf; f != nil {
		f(format, args...)
		return
	}
	if h.opts.debug {
		log.Printf("ui_bind: "+format, args...)
	}
}
