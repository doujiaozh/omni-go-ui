//go:build !windows

// Non-Windows browser discovery and the no-op stubs that keep window control
// compiling everywhere. On X11/macOS the window opens but win.minimize etc.
// report ErrUnsupported until a native implementation exists; the frontend
// hides those controls when hello.features lacks "frameless".

package ui_window

func (w *Window) trackWindow() {}

func (w *Window) minimize() error { return ErrUnsupported }

func (w *Window) maximize() (bool, error) { return false, ErrUnsupported }

func (w *Window) restore() error { return ErrUnsupported }

func (w *Window) snapshot() (WindowState, error) {
	return WindowState{Frameless: w.opts.Frameless}, ErrUnsupported
}

func (w *Window) drag(phase string) error { return nil }

func (w *Window) cancelDrag() {}
