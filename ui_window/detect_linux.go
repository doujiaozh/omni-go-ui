//go:build linux

package ui_window

import (
	"os/exec"
	"syscall"
)

// platformCandidates looks up the Chromium-family binaries on PATH. Any
// Chrome/Edge/Brave/Vivaldi installed through the distro's package manager
// lands here.
func platformCandidates() []Browser {
	names := []string{
		"chromium",
		"chromium-browser",
		"google-chrome",
		"google-chrome-stable",
		"google-chrome-beta",
		"brave-browser",
		"microsoft-edge",
		"vivaldi",
		"vivaldi-stable",
	}
	var list []Browser
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			list = append(list, Browser{Name: name, Path: path, Kind: "chromium"})
		}
	}
	return list
}

func sysProcAttr() *syscall.SysProcAttr { return nil }
