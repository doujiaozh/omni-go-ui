//go:build darwin

// Darwin machine sampling: not implemented. The framework itself is
// cross-platform; the demo's sampler hooks three kernel-specific surfaces
// (per-OS cpu percentages being genuinely Mach-shaped), so on macOS it
// reports zeros rather than dragging in cgo or a Mach bridging layer just
// for the sample app. A window still opens, the dashboard still renders —
// the numbers just stay local until someone adds the sysctls.
package demo

import "time"

// machine mirrors the cross-platform contract with everything zeroed.
type machine struct {
	ts                   time.Time
	idle, kernel, user   int64
	memLoad              uint32
	memTotal, memAvail   uint64
	swapTotal, swapAvail uint64
}

func sampleMachine() (machine, error) {
	// A valid timestamp keeps the delta machinery in sys.go honest: the
	// next platform that implements this gets a working two-sample CPU
	// without having to touch the orchestration code.
	return machine{ts: time.Now()}, nil
}

func cpuPercent(prev, cur machine, dt float64) float64 { return 0 }

type procSample struct {
	pid     uint32
	name    string
	threads uint32
	total   int64
	mem     uint64
}

func processSamples() []procSample { return nil }
