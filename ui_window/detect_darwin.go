//go:build darwin

package ui_window

import (
	"os"
	"path/filepath"
	"syscall"
)

// macApps maps browser names to their executable inside the .app bundle.
var macApps = []struct{ name, inner string }{
	{"Google Chrome", "Google Chrome.app/Contents/MacOS/Google Chrome"},
	{"Microsoft Edge", "Microsoft Edge.app/Contents/MacOS/Microsoft Edge"},
	{"Brave Browser", "BraveBrowser.app/Contents/MacOS/Brave Browser"},
	{"Vivaldi", "Vivaldi.app/Contents/MacOS/Vivaldi"},
	{"Chromium", "Chromium.app/Contents/MacOS/Chromium"},
}

func platformCandidates() []Browser {
	var list []Browser
	roots := []string{"/Applications"}
	if home := os.Getenv("HOME"); home != "" {
		roots = append(roots, filepath.Join(home, "Applications"))
	}
	for _, app := range macApps {
		for _, root := range roots {
			p := filepath.Join(root, app.inner)
			if fileExists(p) {
				list = append(list, Browser{Name: app.name, Path: p, Kind: "chromium"})
				break
			}
		}
	}
	return list
}

func sysProcAttr() *syscall.SysProcAttr { return nil }
