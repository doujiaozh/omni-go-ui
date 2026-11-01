package ui_core

import (
	"embed"
	"io/fs"
)

// frameworkAssets is the frontend bundle that ships with the framework itself:
//
//	webassets/omni.js   client RPC runtime (window.omni)
//	webassets/omni.css  reset + design tokens + perf fallbacks + titlebar
//	webassets/index.html placeholder page shown when an app registers no assets
//
// The whole thing is compiled into the binary. No files are read from disk at
// runtime, which is what makes a single static .exe possible.
//
//go:embed webassets
var frameworkAssets embed.FS

// Assets returns the framework's own frontend bundle rooted at "webassets".
// Mount it in a browser to inspect what a running app serves at /_omni/.
func Assets() fs.FS {
	sub, err := fs.Sub(frameworkAssets, "webassets")
	if err != nil {
		// Cannot happen: the directory is embedded at compile time.
		panic("ui_core: embedded assets are broken: " + err.Error())
	}
	return sub
}

// Embedded reports whether the framework bundle was picked up at build time.
// A zero value means //go:embed failed silently, which never happens in
// practice but is cheap to assert in tests.
func Embedded() bool {
	entries, err := frameworkAssets.ReadDir("webassets")
	return err == nil && len(entries) > 0
}
