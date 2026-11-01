// Command omnigoui is the demo application entry point, and the shortest
// readable example of wiring the whole framework together: embed a frontend,
// serve it on loopback, open it in the system Chromium as an app window,
// and bridge it with the WebSocket RPC hub.
//
// Build:
//
//	go build -ldflags "-s -w -H windowsgui" -o omnigoui.exe .
//
// Environment:
//
//	OMNI_DEV=1        dev mode: any GET / mints a token, verbose logging
//	OMNI_LOWEND=1     lite theme: no glass, no shadows, software GL
//	OMNI_BROWSER=xyz  absolute path, or an alias: chrome, edge, brave,
//	                  vivaldi, opera, 360, qq, sogou
//	OMNI_NOFRAME=1    keep the OS chrome instead of stripping it
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	"github.com/omnigo/omni-go-ui/demo"
	"github.com/omnigo/omni-go-ui/ui_bind"
	"github.com/omnigo/omni-go-ui/ui_core"
	"github.com/omnigo/omni-go-ui/ui_window"
)

const appVersion = "1.0.0"

// defaultSize is the dashboard's designed-for size; Chrome sizes the --app
// window to it and the layout scales down to 1000x640 without scrolling.
const (
	defaultWidth  = 1280
	defaultHeight = 820
)

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)

	devMode := os.Getenv("OMNI_DEV") == "1"
	lowEnd := os.Getenv("OMNI_LOWEND") == "1"
	browserHint := os.Getenv("OMNI_BROWSER")
	frameless := os.Getenv("OMNI_NOFRAME") != "1"

	logf := func(format string, args ...any) { log.Printf(format, args...) }

	// 1. Core: embed the demo bundle and serve it on a random loopback port,
	//    behind a single-use token that the forwarded browser URL plants in
	//    a cookie. Nothing leaves this machine.
	srv := ui_core.New(ui_core.Options{
		Assets:  demo.Assets(),
		Title:   "Omni System Monitor",
		DevMode: devMode,
		LowEnd:  lowEnd,
		Version: appVersion,
		OnLog:   logf,
	})
	if err := srv.Start(); err != nil {
		log.Fatalf("core: start: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	// 2. Bridge: the RPC hub. The frontend calls these methods through
	//    window.omni.call(...); Go pushes events through hub.Broadcast.
	perf := "full"
	if lowEnd {
		perf = "lite"
	}
	features := []string{}
	if frameless {
		features = append(features, "frameless")
	}
	hub := ui_bind.New(
		ui_bind.WithDebug(devMode),
		ui_bind.WithTitle("Omni System Monitor"),
		ui_bind.WithPerf(perf),
		ui_bind.WithFeature(features...),
		ui_bind.WithLogger(logf),
	)
	sampler := demo.NewSampler(hub)
	sampler.Start()
	hub.Handle("sys.stats", sampler.Stats)
	hub.Handle("sys.processes", sampler.Processes)
	hub.Handle("sys.series", sampler.Series)
	srv.Handle("/ws", hub.WebSocketHandler())

	// 3. Window: the system Chromium, in app mode, on its own profile so the
	//    user's real browser session never leaks into the app.
	var browser ui_window.Browser
	if browserHint != "" {
		b, err := ui_window.DetectBrowser(browserHint)
		if err != nil {
			log.Fatalf("window: OMNI_BROWSER=%q: %v", browserHint, err)
		}
		browser = b
	} else {
		b, err := ui_window.Detect()
		if err != nil {
			log.Fatalf("window: no usable Chromium browser: %v\n"+
				"set OMNI_BROWSER to a Chrome/Edge/Brave/Vivaldi/360/QQ path, "+
				"or rebuild after installing one", err)
		}
		browser = b
	}
	winOpts := ui_window.Options{
		URL:               srv.URL(),
		Title:             "Omni System Monitor · 系统监视器",
		Width:             defaultWidth,
		Height:            defaultHeight,
		Frameless:         frameless,
		Browser:           browserHint,
		Debug:             devMode,
		SoftwareRendering: lowEnd,
		ProfileDir:        "", // fresh temp profile per launch
		OnLog:             logf,
	}
	win, err := ui_window.Open(winOpts)
	if err != nil {
		// No window is not a crash: show the URL and keep serving so the
		// page is still reachable by hand, or from a CI harness.
		log.Printf("window: %v", err)
		log.Printf("headless: open %s", srv.URL())
	} else {
		// win.* RPC: the frontend drives the OS chrome through the same
		// socket it uses for data.
		win.RegisterRPC(hub)
		logf("window: %s %s (pid %d)", browser.Name, browser.Version, win.PID())
	}

	// 4. app.* RPC: the small lifecycle surface.
	appInfo := func() map[string]any {
		info := map[string]any{
			"framework": "omni-go-ui",
			"version":   appVersion,
			"title":     "Omni System Monitor · 系统监视器",
			"go":        runtime.Version(),
			"perf":      perf,
			"frameless": frameless,
		}
		if win != nil {
			b := win.Browser()
			info["browser"] = map[string]any{
				"name": b.Name, "path": b.Path, "kind": b.Kind, "version": b.Version,
			}
		}
		return info
	}
	hub.Handle("app.info", appInfo)
	hub.Handle("app.ping", func() map[string]int64 {
		return map[string]int64{"t": time.Now().UnixMilli()}
	})
	hub.Handle("app.echo", func(v any) any { return v })
	hub.Handle("app.quit", func() map[string]string {
		go func() { _ = win.Close() }() // Wait() below will do the teardown
		return map[string]string{"status": "ok"}
	})

	// 5. Lifecycle: the app lives exactly as long as its window.
	closed := make(chan struct{}, 1)
	if win != nil {
		go func() {
			_ = win.Wait()
			select {
			case closed <- struct{}{}:
			default:
			}
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	select {
	case <-closed:
		logf("lifecycle: window closed")
	case <-sig:
		signal.Stop(sig)
		logf("lifecycle: signal received")
	}

	// 6. Teardown in dependency order: browser first (it holds the socket),
	//    then the data pipeline, then the server, then the hub.
	if win != nil {
		_ = win.Close()
	}
	sampler.Stop()
	_ = srv.Shutdown(context.Background())
	_ = hub.Close()
}

// init keeps the log lines honest about which file they came from.
func init() {
	log.SetPrefix(fmt.Sprintf("%s ", filepath.Base(os.Args[0])))
}
