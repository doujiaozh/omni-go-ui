//go:build windows

// Browser discovery on Windows.
//
// Two sources are merged, best-first:
//  1. the App Paths registry keys — where the shell records browser paths,
//     and the only place a per-user Chrome/Edge install (which lives in
//     %LOCALAPPDATA%) is discoverable;
//  2. the well-known install paths of the Chromium-family browsers sold into
//     this market, including 360se which is the default browser on many of
//     the Win7 machines this framework targets.

package ui_window

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	advapi32            = syscall.NewLazyDLL("advapi32.dll")
	procRegOpenKeyEx    = advapi32.NewProc("RegOpenKeyExW")
	procRegQueryValueEx = advapi32.NewProc("RegQueryValueExW")
	procRegCloseKey     = advapi32.NewProc("RegCloseKey")
)

// appPathNames are the executable names looked up under
// HKLM/HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths.
var appPathNames = []string{
	"msedge.exe",
	"chrome.exe",
	"brave.exe",
	"vivaldi.exe",
	"opera.exe",
	"360se.exe",
	"QQBrowser.exe",
}

func platformCandidates() []Browser {
	var list []Browser

	// Registry first: it reflects where the browser actually lives now,
	// including per-user installs that moved out of Program Files.
	for _, name := range appPathNames {
		if p, ok := regAppPath(name); ok && fileExists(p) {
			list = append(list, Browser{Name: name, Path: p, Kind: "chromium"})
		}
	}

	prog := os.Getenv("ProgramFiles")
	prog86 := os.Getenv("ProgramFiles(x86)")
	if prog86 == "" {
		prog86 = prog
	}
	local := os.Getenv("LOCALAPPDATA")
	roaming := os.Getenv("APPDATA")

	for _, p := range []string{
		filepath.Join(local, "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(prog, "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(prog86, "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(local, "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(roaming, "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(prog86, "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(prog, "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(local, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
		filepath.Join(prog86, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
		filepath.Join(local, "Vivaldi", "Application", "vivaldi.exe"),
		filepath.Join(prog86, "Vivaldi", "Application", "vivaldi.exe"),
		filepath.Join(roaming, "360se6", "Application", "360se.exe"),
		filepath.Join(local, "360Chrome", "Chrome", "Application", "360chrome.exe"),
		filepath.Join(prog86, "360Chrome", "Chrome", "Application", "360chrome.exe"),
		filepath.Join(prog86, "Tencent", "QQBrowser", "QQBrowser.exe"),
		filepath.Join(roaming, "Tencent", "QQBrowser", "QQBrowser.exe"),
	} {
		if p == "" {
			continue
		}
		if fileExists(p) {
			list = append(list, Browser{Name: filepath.Base(p), Path: p, Kind: classify(p)})
		}
	}
	return list
}

// sysProcAttr hides the console of the spawned browser so a GUI-mode
// omnigoui.exe does not flash a black box behind the window.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true}
}

// regAppPath reads the (Default) value of an App Paths key: the browser's
// install path exactly as the shell would launch it.
func regAppPath(exe string) (string, bool) {
	const keyPrefix = `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\`

	sub, err := syscall.UTF16PtrFromString(keyPrefix + exe)
	if err != nil {
		return "", false
	}

	const (
		keyRead     uintptr = 0x20019
		keyWow64_64 uintptr = 0x0100
		keyWow64_32 uintptr = 0x0020
	)

	var handle syscall.Handle
	open := func(root uintptr, wow64 uintptr) uintptr {
		ret, _, _ := procRegOpenKeyEx.Call(
			root,
			uintptr(unsafe.Pointer(sub)),
			0,
			keyRead|wow64,
			uintptr(unsafe.Pointer(&handle)),
		)
		return ret
	}
	// 64-bit view, then 32-bit, then HKCU for per-user installs.
	if open(uintptr(syscall.HKEY_LOCAL_MACHINE), keyWow64_64) != 0 &&
		open(uintptr(syscall.HKEY_LOCAL_MACHINE), keyWow64_32) != 0 &&
		open(uintptr(syscall.HKEY_CURRENT_USER), keyWow64_64) != 0 {
		return "", false
	}
	defer procRegCloseKey.Call(uintptr(handle))

	bufLen := uint32(512)
	for attempt := 0; attempt < 2; attempt++ {
		buf := make([]uint16, bufLen/2)
		var valType uint32
		ret, _, _ := procRegQueryValueEx.Call(
			uintptr(handle),
			0, // NULL: the key's default value
			0,
			uintptr(unsafe.Pointer(&valType)),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&bufLen)),
		)
		switch ret {
		case 0: // ERROR_SUCCESS
			if valType != syscall.REG_SZ && valType != syscall.REG_EXPAND_SZ {
				return "", false
			}
			return shellValuePath(syscall.UTF16ToString(buf))
		case 234: // ERROR_MORE_DATA: registry told us the real length, retry
			continue
		default:
			return "", false
		}
	}
	return "", false
}

// shellValuePath keeps the path portion of an App Paths value, which the
// shell sometimes stores as "C:\...\chrome.exe" "%1".
func shellValuePath(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' {
		if i := strings.IndexByte(s[1:], '"'); i >= 0 {
			return strings.TrimSpace(s[1 : 1+i]), true
		}
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}
