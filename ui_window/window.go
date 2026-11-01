// Package ui_window launches the application frontend in the operating
// system's native WebView: a real browser process in --app mode, which gives
// the window a genuine Chromium compositor without the framework shipping a
// runtime.
//
// Why a system browser instead of an embedded WebView2/CEF? Because both of
// those need runtime downloads (WebView2 bootstrapper) or a gigabyte of
// bundled binaries (CEF), and neither runs on a stock Win7 box. Every Windows
// 7 machine that can open a web page already has a Chromium-grade --app mode
// (Chrome 109 was the last Win7 build; 360se ships the same engine for the
// Chinese market).
//
// Lifetime contract: the window's lifetime IS the application's lifetime.
// Launch it, then block on Wait(); when the user closes the window, the app
// is expected to shut down.
package ui_window

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omnigo/omni-go-ui/ui_bind"
)

// Default window geometry: chosen to fit a 1366x768 budget (the most common
// panel on the Win7 hardware this framework targets) while staying roomy.
const (
	DefaultWidth  = 1280
	DefaultHeight = 820
	watchTimeout  = 20 * time.Second // how long to wait for the window to appear
)

var (
	// ErrUnsupported is returned by window-control calls on platforms that
	// have no native implementation yet.
	ErrUnsupported = errors.New("ui_window: window control is not supported on this platform")
	// ErrNoWindow is returned when the browser window has not appeared yet
	// (or has already gone).
	ErrNoWindow = errors.New("ui_window: browser window not available yet")
)

// Browser describes a discovered browser installation.
type Browser struct {
	// Name is the executable base name, e.g. "chrome.exe".
	Name string
	// Path is the executable path.
	Path string
	// Kind is the engine family: "chromium", "gecko" or "ie".
	// Only "chromium" can open --app windows.
	Kind string
	// Version is the detected version, when known.
	Version string
}

// Options configures Open.
type Options struct {
	// URL the window loads, usually the tokenised core URL.
	URL string

	// Title is informational: the visible title comes from the page's <title>.
	Title string

	// Width/Height of the window. Defaults 1280x820.
	Width  int
	Height int

	// X/Y place the window; (0,0) means centre.
	X int
	Y int

	// Frameless strips the native title bar and makes the page responsible
	// for window controls (Windows only). The page detects it through
	// hello.features containing "frameless".
	Frameless bool

	// Browser names a browser to use instead of the auto-detected one: an
	// absolute path, or a base name like "msedge.exe" / "chrome".
	Browser string

	// ProfileDir is the Chromium user-data-dir. Empty = a throwaway temp
	// profile, removed when the process exits.
	ProfileDir string

	// Debug opens DevTools with the window and logs the browser's stderr.
	Debug bool

	// SoftwareRendering disables GPU compositing inside the browser, for the
	// pre-2010 GPUs that crash or crawl on this Win7 class of hardware.
	SoftwareRendering bool

	// Kiosk opens borderless at the full screen size instead of --app mode.
	Kiosk bool

	// OnLog receives diagnostics, defaults to the standard logger.
	OnLog func(format string, args ...any)
}

// Window is a running browser process presenting the app.
type Window struct {
	opts    Options
	browser Browser
	cmd     *exec.Cmd

	profile     string
	ownsProfile bool

	exit    chan struct{}
	exitErr error

	stateMu sync.Mutex
	hwnd    uintptr // native window handle (Windows), 0 elsewhere
	closed  atomic.Bool

	dragMu     sync.Mutex
	dragCancel chan struct{}

	logf func(format string, args ...any)

	once sync.Once
}

// Detect finds a Chromium-based browser. It consults the App Paths registry
// keys first (that is where per-user Chrome/Edge live), then standard paths.
func Detect() (Browser, error) {
	for _, b := range platformCandidates() {
		if b.Kind != "chromium" || !fileExists(b.Path) {
			continue
		}
		return b, nil
	}
	return Browser{}, errors.New("ui_window: no Chromium-based browser found; install Chrome/Edge/Brave or set OMNI_BROWSER to a chrome.exe path")
}

// DetectBrowser resolves one explicit browser: an absolute path, a base name
// ("chrome.exe"), or a short alias ("edge", "chrome", "brave", "vivaldi",
// "360", "qq"). Only Chromium-family browsers are accepted, because only they
// implement --app mode.
func DetectBrowser(want string) (Browser, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return Detect()
	}

	if strings.ContainsAny(want, `/\`) && fileExists(want) {
		return Browser{Name: filepath.Base(want), Path: want, Kind: classify(want), Version: ""}, nil
	}

	if alias, ok := browserAliases[strings.ToLower(want)]; ok {
		want = alias
	}
	base := strings.ToLower(want)
	for _, b := range platformCandidates() {
		bn := strings.ToLower(filepath.Base(b.Path))
		if bn == base || strings.Contains(bn, base) {
			if b.Kind != "chromium" {
				return Browser{}, fmt.Errorf("ui_window: %s cannot open app windows (need a Chromium-based browser)", bn)
			}
			if fileExists(b.Path) {
				return b, nil
			}
		}
	}
	return Browser{}, fmt.Errorf("ui_window: browser %q not found", want)
}

// browserAliases map friendly names to executable base names.
var browserAliases = map[string]string{
	"edge":    "msedge.exe",
	"chrome":  "chrome.exe",
	"brave":   "brave.exe",
	"vivaldi": "vivaldi.exe",
	"opera":   "opera.exe",
	"360":     "360se.exe",
	"qq":      "QQBrowser.exe",
	"sogou":   "SogouExplorer.exe",
}

// classify names the engine family of an executable.
func classify(exe string) string {
	base := strings.ToLower(filepath.Base(exe))
	switch {
	case strings.Contains(base, "iexplore"):
		return "ie"
	case strings.Contains(base, "firefox"):
		return "gecko"
	default:
		return "chromium"
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// Open launches the browser and returns the window. The window is already
// running when this returns; Wait for its exit at your convenience.
func Open(opts Options) (*Window, error) {
	if opts.URL == "" {
		return nil, errors.New("ui_window: Options.URL is required")
	}
	if opts.Width <= 0 {
		opts.Width = DefaultWidth
	}
	if opts.Height <= 0 {
		opts.Height = DefaultHeight
	}

	browser, err := resolveBrowser(opts.Browser)
	if err != nil {
		return nil, err
	}

	w := &Window{
		opts:    opts,
		browser: browser,
		exit:    make(chan struct{}),
	}
	if opts.OnLog != nil {
		w.logf = opts.OnLog
	} else {
		w.logf = func(format string, args ...any) {
			log.Printf("ui_window: "+format, args...)
		}
	}

	w.profile = opts.ProfileDir
	if w.profile == "" {
		w.profile, err = os.MkdirTemp("", "omni-go-ui-")
		if err != nil {
			return nil, fmt.Errorf("ui_window: create temp profile: %w", err)
		}
		w.ownsProfile = true
	}

	cmd := exec.Command(browser.Path, w.args()...)
	cmd.SysProcAttr = sysProcAttr()
	if opts.Debug {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		if w.ownsProfile {
			_ = os.RemoveAll(w.profile)
		}
		return nil, fmt.Errorf("ui_window: start %s: %w", browser.Path, err)
	}
	w.cmd = cmd
	w.logf("opened %s (pid %d) at %s", browser.Path, cmd.Process.Pid, maskURL(opts.URL))

	go func() {
		w.exitErr = cmd.Wait()
		close(w.exit)
		w.cleanupProfile()
	}()

	// Track the native handle even when not stripping the frame: window
	// controls (win.minimize etc.) work either way.
	go w.trackWindow()

	return w, nil
}

// resolveBrowser picks the browser: explicit choice, then detection.
func resolveBrowser(prefer string) (Browser, error) {
	if prefer == "" {
		return Detect()
	}
	b, err := DetectBrowser(prefer)
	if err != nil {
		return Browser{}, err
	}
	return b, nil
}

// args builds the Chromium command line. The flags are deliberately the union
// of "make it fast", "make it quiet" and "make it survive a Win7 panel":
// no first-run flows, no background network, no session-restore bubbles.
func (w *Window) args() []string {
	o := w.opts
	args := []string{
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-first-run",
		"--disable-session-crashed-bubble",
		"--disable-features=Translate,MediaRouter,OptimizationHints,DialMediaRouteProvider,DesktopScreenshotCapturer",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-sync",
		"--disable-extensions",
		"--disable-default-apps",
		"--metrics-recording-only",
		"--no-service-autorun",
		"--disable-renderer-backgrounding",
	}
	if o.Kiosk {
		args = append([]string{"--kiosk", o.URL}, args...)
	} else {
		args = append([]string{"--app=" + o.URL}, args...)
	}
	if w.profile != "" {
		args = append(args, "--user-data-dir="+w.profile)
	}
	if !o.Kiosk {
		if o.Width > 0 && o.Height > 0 {
			args = append(args, "--window-size="+strconv.Itoa(o.Width)+","+strconv.Itoa(o.Height))
		}
		if o.X > 0 || o.Y > 0 {
			args = append(args, "--window-position="+strconv.Itoa(o.X)+","+strconv.Itoa(o.Y))
		}
	}
	if o.SoftwareRendering {
		// The Win7 red line: no GPU compositing path at all.
		args = append(args, "--disable-gpu", "--disable-gpu-compositing", "--swiftshader-webgl")
	}
	if o.Debug {
		args = append(args, "--auto-open-devtools-for-tabs", "--enable-logging=stderr", "--v=0")
	}
	return args
}

// Browser reports which browser is hosting the app.
func (w *Window) Browser() Browser { return w.browser }

// PID is the browser process id, or 0.
func (w *Window) PID() int {
	if w == nil || w.cmd == nil || w.cmd.Process == nil {
		return 0
	}
	return w.cmd.Process.Pid
}

// Wait blocks until the browser process exits and returns its error (nil for
// a clean exit). Every window exits eventually; this is the app's off switch.
func (w *Window) Wait() error {
	<-w.exit
	return w.exitErr
}

// Close terminates the browser process. It is safe to call more than once and
// safe to call before Wait.
func (w *Window) Close() error {
	w.once.Do(func() {
		w.closed.Store(true)
		w.cancelDrag()
		if p := w.process(); p != nil {
			if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				w.logf("kill: %v", err)
			}
		}
	})
	return nil
}

func (w *Window) process() *os.Process {
	if w == nil || w.cmd == nil {
		return nil
	}
	return w.cmd.Process
}

// cleanupProfile removes the throwaway user-data-dir after the process dies.
func (w *Window) cleanupProfile() {
	if !w.ownsProfile || w.profile == "" {
		return
	}
	_ = os.RemoveAll(w.profile)
}

// Minimize hides the window without closing it.
func (w *Window) Minimize() error { return w.minimize() }

// Maximize toggles the maximised state and reports the new value.
func (w *Window) Maximize() (bool, error) { return w.maximize() }

// Restore returns a maximised window to its normal geometry.
func (w *Window) Restore() error { return w.restore() }

// State describes the current window geometry and flags.
func (w *Window) State() (WindowState, error) { return w.snapshot() }

// Drag starts/stops an emulated titlebar drag: the frontend calls
// win.drag("start") on mousedown over its custom titlebar and
// win.drag("end") on mouseup.
func (w *Window) Drag(phase string) error { return w.drag(phase) }

// WindowState is the shape of the win.state RPC result.
type WindowState struct {
	Frameless bool `json:"frameless"`
	Maximized bool `json:"maximized"`
	Minimized bool `json:"minimized,omitempty"`
	Width     int  `json:"width"`
	Height    int  `json:"height"`
	Left      int  `json:"left,omitempty"`
	Top       int  `json:"top,omitempty"`
}

// RegisterRPC mounts the standard window methods on a hub:
//
//	win.minimize  ()                 -> null
//	win.maximize  ()                 -> {"maximized": bool}
//	win.close     ()                 -> {"status": "closing"}
//	win.drag      ("start"|"end")    -> null
//	win.state     ()                 -> WindowState
//
// All of them no-op gracefully on platforms without native support.
func (w *Window) RegisterRPC(h *ui_bind.Hub) {
	h.Handle("win.minimize", func() error { return w.Minimize() })
	h.Handle("win.maximize", func() (map[string]bool, error) {
		m, err := w.Maximize()
		if err != nil {
			return nil, err
		}
		return map[string]bool{"maximized": m}, nil
	})
	h.Handle("win.close", func() map[string]string {
		go w.Close()
		return map[string]string{"status": "closing"}
	})
	h.Handle("win.drag", func(phase string) error { return w.Drag(phase) })
	h.Handle("win.state", func() (WindowState, error) { return w.State() })
}

// maskURL hides the session token before echoing a URL into the log.
func maskURL(u string) string {
	if i := strings.Index(u, "?k="); i >= 0 {
		return u[:i+3] + strings.Repeat("*", 6)
	}
	return u
}
