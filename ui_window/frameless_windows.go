//go:build windows

// Frameless window control on Windows.
//
// The plan:
//
//  1. launch the browser with --app=<url> (Chromium builds a normal
//     top-level window whose client area hosts the web view);
//  2. poll for that window by (pid, class, visible, has-title, largest-area)
//     — the class filter is what separates the --app window from Chrome's
//     hidden intermediates;
//  3. clear WS_CAPTION/WS_SYSMENU/WS_THICKFRAME + friends and set WS_POPUP,
//     then SetWindowPos(SWP_FRAMECHANGED) so the change actually applies;
//  4. expose minimize/maximize/drag/state over the RPC bridge, all
//     implemented through ShowWindowAsync and a cursor-driven drag loop,
//     because with the caption gone nothing else will move the window.
//
// Everything here runs on a locked OS thread: EnumWindows delivers its
// callback synchronously through a C trampoline, and mixing threads with
// SetWindowPos during a drag would tear the window across monitors.

package ui_window

import (
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

const (
	gwlStyle   int32 = -16
	gwlExStyle int32 = -20

	wsPopup        uintptr = 0x80000000
	wsCaption      uintptr = 0x00C00000
	wsSysMenu      uintptr = 0x00080000
	wsThickFrame   uintptr = 0x00040000
	wsBorder       uintptr = 0x00800000
	wsDlgFrame     uintptr = 0x00040000
	wsMinimizeBox  uintptr = 0x00020000
	wsMaximizeBox  uintptr = 0x00010000
	wsMinimize     uintptr = 0x20000000
	wsMaximize     uintptr = 0x01000000
	wsExWindowEdge uintptr = 0x00000100

	swpNoSize       uintptr = 0x0001
	swpNoMove       uintptr = 0x0002
	swpNoZOrder     uintptr = 0x0004
	swpNoActivate   uintptr = 0x0010
	swpFrameChanged uintptr = 0x0020
	swpShowWindow   uintptr = 0x0040

	swMinimize uintptr = 6
	swMaximize uintptr = 3
	swRestore  uintptr = 9

	smCXScreen uintptr = 0
	smCYScreen uintptr = 1

	vkLButton uintptr = 0x01

	chromeWindowClass = "Chrome_WidgetWin_1"
)

type rect struct{ Left, Top, Right, Bottom int32 }
type point struct{ X, Y int32 }

var (
	user32 = syscall.NewLazyDLL("user32.dll")

	procEnumWindows          = user32.NewProc("EnumWindows")
	procIsWindow             = user32.NewProc("IsWindow")
	procIsWindowVisible      = user32.NewProc("IsWindowVisible")
	procGetWindowTextLengthW = user32.NewProc("GetWindowTextLengthW")
	procGetClassNameW        = user32.NewProc("GetClassNameW")
	procGetWindowRect        = user32.NewProc("GetWindowRect")
	procGetWindowThreadPID   = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowLongPtr     = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtr     = user32.NewProc("SetWindowLongPtrW")
	procGetWindowLong        = user32.NewProc("GetWindowLongW")
	procSetWindowLong        = user32.NewProc("SetWindowLongW")
	procSetWindowPos         = user32.NewProc("SetWindowPos")
	procShowWindowAsync      = user32.NewProc("ShowWindowAsync")
	procGetCursorPos         = user32.NewProc("GetCursorPos")
	procGetAsyncKeyState     = user32.NewProc("GetAsyncKeyState")
	procGetSystemMetrics     = user32.NewProc("GetSystemMetrics")
)

// call0 invokes a proc and keeps only its first result, for the 90% of Win32
// calls here that are "BOOL, ignore the rest".
func call0(p *syscall.LazyProc, args ...uintptr) uintptr {
	r, _, _ := p.Call(args...)
	return r
}

func (w *Window) trackWindow() {
	deadline := time.Now().Add(watchTimeout)
	pid := uint32(w.PID())
	if pid == 0 {
		return
	}
	for {
		hwnd := findMainWindow(pid)
		if hwnd != 0 {
			w.stateMu.Lock()
			w.hwnd = uintptr(hwnd)
			w.stateMu.Unlock()
			if w.opts.Frameless && !w.opts.Kiosk {
				w.stripFrame(hwnd)
			}
			w.logf("native window 0x%x (pid %d), frameless=%v", uintptr(hwnd), pid, w.opts.Frameless)
			return
		}
		select {
		case <-w.exit:
			return
		default:
		}
		if time.Now().After(deadline) {
			w.logf("window for pid %d did not appear within %s", pid, watchTimeout)
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// findMainWindow returns the browser's main --app window: the visible,
// titled, Chrome-class, largest-area top-level window owned by our pid.
func findMainWindow(pid uint32) syscall.Handle {
	var best struct {
		hwnd syscall.Handle
		area int32
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	enumProc := syscall.NewCallback(func(hwnd syscall.Handle, lparam uintptr) uintptr {
		if call0(procIsWindowVisible, uintptr(hwnd)) == 0 {
			return 1
		}
		var winPID uint32
		_, _, _ = procGetWindowThreadPID.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&winPID)))
		if winPID != pid {
			return 1
		}
		if call0(procGetWindowTextLengthW, uintptr(hwnd)) == 0 {
			return 1
		}
		var class [512]uint16
		n, _, _ := procGetClassNameW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
		if n == 0 || syscall.UTF16ToString(class[:]) != chromeWindowClass {
			return 1
		}
		var r rect
		if call0(procGetWindowRect, uintptr(hwnd), uintptr(unsafe.Pointer(&r))) != 0 &&
			r.Right > r.Left && r.Bottom > r.Top {
			if area := (r.Right - r.Left) * (r.Bottom - r.Top); area > best.area {
				best.hwnd, best.area = hwnd, area
			}
		}
		return 1
	})

	_, _, _ = procEnumWindows.Call(enumProc, 0)
	return best.hwnd
}

// stripFrame rebuilds the window as a borderless popup.
func (w *Window) stripFrame(hwnd syscall.Handle) {
	if hwnd == 0 {
		return
	}
	style := getWindowLong(hwnd, gwlStyle)
	style &^= wsCaption | wsSysMenu | wsThickFrame | wsMinimizeBox | wsMaximizeBox | wsBorder | wsDlgFrame
	style |= wsPopup
	setWindowLong(hwnd, gwlStyle, style)

	ex := getWindowLong(hwnd, gwlExStyle)
	ex &^= wsExWindowEdge
	setWindowLong(hwnd, gwlExStyle, ex)

	// Apply the new metrics and re-seat the rect: without SWP_FRAMECHANGED
	// the non-client area is never recomputed and the window keeps the ghost
	// of its old frame.
	x, y, width, height := w.geometry()
	_, _, _ = procSetWindowPos.Call(
		uintptr(hwnd), 0,
		uintptr(x), uintptr(y), uintptr(width), uintptr(height),
		swpFrameChanged|swpNoZOrder|swpShowWindow|swpNoActivate,
	)
}

// geometry returns the target window rect, centring when X/Y were not given.
func (w *Window) geometry() (x, y, width, height int) {
	width, height = w.opts.Width, w.opts.Height
	if width <= 0 {
		width = DefaultWidth
	}
	if height <= 0 {
		height = DefaultHeight
	}
	x, y = w.opts.X, w.opts.Y
	if x > 0 && y > 0 {
		return
	}
	cx := int(call0(procGetSystemMetrics, smCXScreen))
	cy := int(call0(procGetSystemMetrics, smCYScreen))
	x = (cx - width) / 2
	y = int(float64(cy-height) * 0.42) // slightly above optical centre
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return
}

// windowHandle returns the live window handle, re-resolving it if the tracked
// one went stale (the browser may rebuild its window).
func (w *Window) windowHandle() syscall.Handle {
	w.stateMu.Lock()
	h := syscall.Handle(w.hwnd)
	w.stateMu.Unlock()
	if h != 0 && call0(procIsWindow, uintptr(h)) != 0 {
		return h
	}
	h = findMainWindow(uint32(w.PID()))
	if h != 0 {
		w.stateMu.Lock()
		w.hwnd = uintptr(h)
		w.stateMu.Unlock()
	}
	return h
}

// getWindowLong reads a window long; the & 0xFFFFFFFF removes the sign
// extension Windows applies to 32-bit LONG results on x64.
func getWindowLong(hwnd syscall.Handle, index int32) uintptr {
	proc := procGetWindowLongPtr
	if unsafe.Sizeof(uintptr(0)) == 4 {
		proc = procGetWindowLong
	}
	r, _, _ := proc.Call(uintptr(hwnd), uintptr(index))
	return r & 0xFFFFFFFF
}

func setWindowLong(hwnd syscall.Handle, index int32, value uintptr) {
	proc := procSetWindowLongPtr
	if unsafe.Sizeof(uintptr(0)) == 4 {
		proc = procSetWindowLong
	}
	_, _, _ = proc.Call(uintptr(hwnd), uintptr(index), value)
}

func (w *Window) minimize() error {
	hwnd := w.windowHandle()
	if hwnd == 0 {
		return ErrNoWindow
	}
	_, _, _ = procShowWindowAsync.Call(uintptr(hwnd), swMinimize)
	return nil
}

func (w *Window) maximize() (bool, error) {
	hwnd := w.windowHandle()
	if hwnd == 0 {
		return false, ErrNoWindow
	}
	if getWindowLong(hwnd, gwlStyle)&wsMaximize != 0 {
		_, _, _ = procShowWindowAsync.Call(uintptr(hwnd), swRestore)
		return false, nil
	}
	_, _, _ = procShowWindowAsync.Call(uintptr(hwnd), swMaximize)
	return true, nil
}

func (w *Window) restore() error {
	hwnd := w.windowHandle()
	if hwnd == 0 {
		return ErrNoWindow
	}
	_, _, _ = procShowWindowAsync.Call(uintptr(hwnd), swRestore)
	return nil
}

func (w *Window) snapshot() (WindowState, error) {
	st := WindowState{Frameless: w.opts.Frameless}
	hwnd := w.windowHandle()
	if hwnd == 0 {
		return st, ErrNoWindow
	}
	style := getWindowLong(hwnd, gwlStyle)
	st.Maximized = style&wsMaximize != 0
	st.Minimized = style&wsMinimize != 0
	var r rect
	if call0(procGetWindowRect, uintptr(hwnd), uintptr(unsafe.Pointer(&r))) != 0 {
		st.Left, st.Top = int(r.Left), int(r.Top)
		st.Width, st.Height = int(r.Right-r.Left), int(r.Bottom-r.Top)
	}
	return st, nil
}

// drag implements an emulated caption drag: the caller tells us where the
// window and cursor were at mousedown, and we keep the window glued to the
// cursor until the button is released.
func (w *Window) drag(phase string) error {
	if phase != "start" {
		return nil // the loop self-terminates on button release
	}
	hwnd := w.windowHandle()
	if hwnd == 0 {
		return nil
	}
	var r rect
	if call0(procGetWindowRect, uintptr(hwnd), uintptr(unsafe.Pointer(&r))) == 0 {
		return nil
	}
	var p point
	if call0(procGetCursorPos, uintptr(unsafe.Pointer(&p))) == 0 {
		return nil
	}
	w.startDrag(hwnd, r.Left, r.Top, p.X, p.Y)
	return nil
}

func (w *Window) cancelDrag() {
	w.dragMu.Lock()
	defer w.dragMu.Unlock()
	if w.dragCancel != nil {
		close(w.dragCancel)
		w.dragCancel = nil
	}
}

func (w *Window) startDrag(hwnd syscall.Handle, winX, winY, origX, origY int32) {
	w.cancelDrag()
	cancel := make(chan struct{})
	w.dragMu.Lock()
	w.dragCancel = cancel
	w.dragMu.Unlock()

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		ticker := time.NewTicker(time.Second / 90)
		defer ticker.Stop()
		for {
			select {
			case <-cancel:
				return
			case <-w.exit:
				return
			case <-ticker.C:
			}
			if w.closed.Load() {
				return
			}
			// Nothing will send us a mouseup: poll the button state instead.
			if call0(procGetAsyncKeyState, vkLButton)&0x8000 == 0 {
				return
			}
			var p point
			if call0(procGetCursorPos, uintptr(unsafe.Pointer(&p))) == 0 {
				return
			}
			_, _, _ = procSetWindowPos.Call(
				uintptr(hwnd), 0,
				uintptr(winX+(p.X-origX)), uintptr(winY+(p.Y-origY)),
				0, 0,
				swpNoSize|swpNoZOrder|swpNoActivate,
			)
		}
	}()
}
