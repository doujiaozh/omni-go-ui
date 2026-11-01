// Package ui_core is the rendering kernel of Omni-Go-UI: it embeds the frontend
// bundle into the binary and serves it over a loopback-only HTTP server whose
// lifetime is the lifetime of the desktop app.
//
// Design constraints (see docs/ARCHITECTURE.md):
//
//   - pure Go, zero CGO, zero third-party modules;
//   - assets live in an embed.FS, so the shipped artifact is one executable;
//   - the listener binds 127.0.0.1 with an ephemeral port, which keeps Windows
//     from showing a firewall prompt and keeps the app unreachable from the LAN;
//   - every request must present a per-run random token (query string on first
//     navigation, HttpOnly cookie afterwards), so another web page in the same
//     browser cannot talk to the app.
package ui_core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// Version of the rendering kernel. The wire protocol version lives in ui_bind.
const Version = "1.0.0"

const cookieName = "omni_id"

// csp is deliberately strict: the app may not load anything from the network.
// Inline *styles* are allowed because dashboards legitimately set style
// attributes from JS; inline scripts are not, so the JS bundle must be a file.
const csp = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; " +
	"font-src 'self' data:; " +
	"connect-src 'self' ws://127.0.0.1:* ws://localhost:*; " +
	"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Options configures a Server.
type Options struct {
	// Assets is the application's frontend bundle (index.html at the root).
	// When nil the framework's placeholder page is served.
	Assets fs.FS

	// Title is reported by /-/health and used by the console banner.
	Title string

	// DevMode disables the token gate (so a normal browser tab can be used for
	// hot reload) and marks every response no-store. Never ship with it on.
	DevMode bool

	// Address is the listen address. Default "127.0.0.1:0".
	Address string

	// Token overrides the per-run random session token. Normally empty.
	Token string

	// LowEnd switches the frontend to its lite rendering profile. It is handed
	// to ui_bind (hello.perf) and exposed via /-/health.
	LowEnd bool

	// Version overrides the reported framework version.
	Version string

	// OnLog receives diagnostics. Defaults to the standard logger.
	OnLog func(format string, args ...any)
}

// Server serves an embedded frontend on 127.0.0.1 and enforces the token gate.
type Server struct {
	opts   Options
	token  string
	mux    *http.ServeMux
	ln     net.Listener
	http   *http.Server
	port   int
	origin string
	start  time.Time

	mu     sync.Mutex
	closed bool
}

// New builds a Server. Call Start to bind the socket.
func New(opts Options) *Server {
	if opts.Address == "" {
		opts.Address = "127.0.0.1:0"
	}
	if opts.Version == "" {
		opts.Version = Version
	}
	if opts.Title == "" {
		opts.Title = "Omni-Go-UI"
	}
	token := opts.Token
	if token == "" {
		token = randomToken()
	}
	s := &Server{
		opts:  opts,
		token: token,
		mux:   http.NewServeMux(),
	}
	// Catch-all: the app's static bundle plus SPA fallback.
	s.mux.HandleFunc("/", s.serveAsset)
	return s
}

// Handle registers an application handler, e.g. srv.Handle("/ws", hub.WebSocketHandler()).
// Longer patterns win over the catch-all static handler.
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// Start binds the socket and begins serving in the background.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return errors.New("ui_core: server already started")
	}
	ln, err := net.Listen("tcp", s.opts.Address)
	if err != nil {
		return fmt.Errorf("ui_core: listen on %s: %w", s.opts.Address, err)
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return fmt.Errorf("ui_core: unexpected listener address %s", ln.Addr())
	}
	s.ln = ln
	s.port = tcp.Port
	s.origin = fmt.Sprintf("http://127.0.0.1:%d", s.port)
	s.start = time.Now()
	s.http = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout: WebSockets are long-lived; hijacked conns are excluded
		// from these timeouts anyway.
	}
	go func(srv *http.Server, l net.Listener) {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logf("ui_core: serve loop stopped: %v", err)
		}
	}(s.http, ln)
	s.logf("serving %q on %s (token-gated)", s.opts.Title, s.origin)
	return nil
}

// Shutdown stops the server, waiting for in-flight handlers up to ctx's deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	srv := s.http
	s.mu.Unlock()
	if srv == nil {
		if s.ln != nil {
			return s.ln.Close()
		}
		return nil
	}
	return srv.Shutdown(ctx)
}

// Origin returns the base URL without the token, e.g. http://127.0.0.1:51234.
func (s *Server) Origin() string { return s.origin }

// URL returns the tokenised URL the window should open.
func (s *Server) URL() string {
	if s.token == "" || s.opts.DevMode {
		return s.origin + "/"
	}
	return s.origin + "/?k=" + s.token
}

// Port returns the bound TCP port (0 before Start).
func (s *Server) Port() int { return s.port }

// Token returns the per-run session token.
func (s *Server) Token() string { return s.token }

// DevMode reports whether the token gate is disabled.
func (s *Server) DevMode() bool { return s.opts.DevMode }

// ServeHTTP implements the whole request pipeline: security headers, the
// public framework namespace, the token gate, then the app mux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", csp)

	p := r.URL.Path

	// Public namespace: the framework bundle, the health probe, favicon.
	if strings.HasPrefix(p, "/_omni/") || p == "/favicon.ico" {
		s.serveAsset(w, r)
		return
	}
	if p == "/-/health" {
		s.serveHealth(w)
		return
	}

	if !s.authorized(w, r) {
		s.serveForbidden(w)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// authorized validates the token from the query string or the session cookie,
// planting the cookie the first time the window navigates with ?k=.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if s.opts.DevMode || s.token == "" {
		return true
	}
	if c, err := r.Cookie(cookieName); err == nil && equal(c.Value, s.token) {
		return true
	}
	if k := r.URL.Query().Get("k"); k != "" && equal(k, s.token) {
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    s.token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		return true
	}
	return false
}

func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// serveAsset resolves a path against the app bundle first and the framework
// bundle second, with an index.html SPA fallback for extension-less paths.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	raw := r.URL.Path
	if raw == "" {
		raw = "/"
	}
	clean := path.Clean("/" + raw)
	name := strings.TrimPrefix(clean, "/")
	if name == "" || strings.HasSuffix(raw, "/") {
		name = path.Join(name, "index.html")
	}

	// Framework bundle: /_omni/x.js -> webassets/x.js
	if strings.HasPrefix(clean, "/_omni/") {
		fw := strings.TrimPrefix(clean, "/_omni/")
		if s.writeAsset(w, r, Assets(), fw) {
			return
		}
		http.NotFound(w, r)
		return
	}

	if s.opts.Assets != nil && s.writeAsset(w, r, s.opts.Assets, name) {
		return
	}

	// SPA fallback: unknown extension-less paths render the app shell.
	if path.Ext(name) == "" && s.opts.Assets != nil {
		if s.writeAsset(w, r, s.opts.Assets, "index.html") {
			return
		}
	}

	// Nothing registered at all: show the framework's placeholder page.
	if s.writeAsset(w, r, Assets(), "index.html") {
		return
	}
	http.NotFound(w, r)
}

// writeAsset streams one file out of fsys. It reports whether it served anything;
// a missing/unreadable file is a normal "not here, try the next bundle" signal.
func (s *Server) writeAsset(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	if fsys == nil {
		return false
	}
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		name = "index.html"
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	data, err := fs.ReadFile(fsys, clean)
	if err != nil {
		return false
	}

	if ct := contentType(clean); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if s.opts.DevMode || clean == "index.html" {
		// The shell must never be stale; in dev nothing may be cached at all.
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
	} else {
		// Assets are baked into the binary, so they can be cached hard.
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	// ServeContent gives us Range/HEAD handling for free.
	http.ServeContent(w, r, clean, time.Time{}, bytes.NewReader(data))
	return true
}

var mimeOverrides = map[string]string{
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".html":  "text/html; charset=utf-8",
	".htm":   "text/html; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".woff2": "font/woff2",
	".woff":  "font/woff",
	".ico":   "image/x-icon",
	".wasm":  "application/wasm",
}

func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := mimeOverrides[ext]; ok {
		return ct
	}
	return mime.TypeByExtension(ext)
}

// serveHealth is the only unauthenticated JSON endpoint: it carries no secrets,
// just enough to prove the app is alive from a script or a monitoring tool.
func (s *Server) serveHealth(w http.ResponseWriter) {
	perf := "full"
	if s.opts.LowEnd {
		perf = "lite"
	}
	body := map[string]any{
		"ok":        true,
		"framework": "omni-go-ui",
		"version":   s.opts.Version,
		"title":     s.opts.Title,
		"port":      s.port,
		"perf":      perf,
		"dev":       s.opts.DevMode,
		"uptimeSec": int(time.Since(s.start).Seconds()),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(body)
}

// serveForbidden explains the launch flow instead of leaking a bare 403.
func (s *Server) serveForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8">
<title>403 · Omni-Go-UI</title>
<style>
 html{background:#070b14;color:#eaf2ff;font:14px/1.7 system-ui,"Microsoft YaHei UI",sans-serif;
      display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}
 .card{max-width:520px;padding:32px 36px;border:1px solid rgba(255,255,255,.09);
       border-radius:16px;background:rgba(255,255,255,.04)}
 h1{margin:0 0 8px;font-size:20px;letter-spacing:.5px}
 code{color:#35e0c9;font-family:Consolas,monospace}
 p{margin:10px 0;color:#9db0cc}
</style>
<div class="card">
  <h1>403 · 需要启动令牌 / token required</h1>
  <p>本窗口必须由 <code>omnigoui.exe</code> 打开：它会带上一次性令牌。</p>
  <p>This page only serves a token-authenticated app window opened by
     <code>omnigoui.exe</code>. Launch the binary instead of typing the URL.</p>
</div>`))
}

func (s *Server) logf(format string, args ...any) {
	if s.opts.OnLog != nil {
		s.opts.OnLog(format, args...)
		return
	}
	log.Printf("omni: "+format, args...)
}

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is fatal-ish for security; fall back to a
		// time-seeded value so the app still runs rather than refusing to boot.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
