// Package demo is the shipped sample application: a bilingual system
// monitor dashboard. It doubles as the reference wiring of the framework —
// how an app embeds its frontend, exposes Go methods to the page, and pushes
// live data into every open window.
//
// The Go side here is deliberately dependency-free: on Windows the sampler
// reads the kernel through raw syscall (GetSystemTimes, GlobalMemoryStatusEx,
// Toolhelp32) — same "no CGO, no cgo-exploit" rule as the rest of the
// framework — and on Linux it reads /proc. The frontend is in web/.
package demo

import (
	"embed"
	"fmt"
	"io/fs"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/omnigo/omni-go-ui/ui_bind"
)

//go:embed web
var webFS embed.FS

// Assets returns the demo dashboard bundle (index.html, app.css, app.js).
// main.go passes it to the core server as the application's frontend.
func Assets() fs.FS {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return webFS
	}
	return sub
}

// Stats is the shape of both the sys.stats RPC result and the sys:stats event.
type Stats struct {
	CPU        float64 `json:"cpu"`        // percent 0-100
	Memory     float64 `json:"memory"`     // percent 0-100
	MemTotal   uint64  `json:"memTotal"`   // bytes
	MemUsed    uint64  `json:"memUsed"`    // bytes
	SwapTotal  uint64  `json:"swapTotal"`  // bytes
	SwapUsed   uint64  `json:"swapUsed"`   // bytes
	Procs      int     `json:"procs"`      // process count
	NumCPU     int     `json:"numCPU"`     // logical cores
	GoRoutines int     `json:"goroutines"` // live goroutines
	Uptime     int64   `json:"uptimeSec"`  // app uptime
	UpdatedAt  int64   `json:"updatedAt"`  // unix seconds
}

// Process is one row of the dashboard's process table.
type Process struct {
	PID     uint32  `json:"pid"`
	Name    string  `json:"name"`
	Threads uint32  `json:"threads"`
	CPU     float64 `json:"cpu"`    // percent of one core
	Memory  uint64  `json:"memory"` // resident bytes
}

// Sample is one point of the rolling chart, kept for late-joining windows.
type Sample struct {
	CPU    float64 `json:"cpu"`
	Memory float64 `json:"memory"`
	At     int64   `json:"at"`
}

// Alert is the sys:alert payload: bilingual, level-coloured, deduplicated.
type Alert struct {
	Kind  string  `json:"kind"`  // "cpu" | "memory"
	Level string  `json:"level"` // "warn" | "crit"
	Value float64 `json:"value"`
	Zh    string  `json:"zh"`
	En    string  `json:"en"`
}

// Sampling / alerting knobs. One tick per second keeps the dashboard alive
// without lighting up a whole core of an already old machine.
const (
	tickInterval = time.Second
	alertCPUPct  = 85.0
	alertMemPct  = 90.0
	alertStreak  = 3
	seriesCap    = 300
	processLimit = 8
)

var startedAt = time.Now()

// NewSampler builds a sampler bound to a hub: it ticks once per second,
// caches the latest stats for RPC, and — only while some window is actually
// connected — broadcasts sys:stats, raising sys:alert when the box stays in
// the red.
func NewSampler(hub *ui_bind.Hub) *Sampler {
	return &Sampler{
		hub:       hub,
		interval:  tickInterval,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		series:    make([]Sample, 0, seriesCap),
		alertOpen: map[string]bool{},
	}
}

// Sampler is the one-second heartbeat of the demo.
type Sampler struct {
	hub       *ui_bind.Hub
	interval  time.Duration
	stop      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once

	mu        sync.RWMutex
	current   Stats
	procs     []Process
	series    []Sample
	prev      *machine
	prevProcs map[uint32]*procSample
	cpuStreak int
	memStreak int
	alertOpen map[string]bool
}

// Start begins ticking; idempotent. Stop ends the loop.
func (s *Sampler) Start() {
	s.startOnce.Do(func() { go s.loop() })
}

func (s *Sampler) Stop() {
	s.stopOnce.Do(func() { close(s.stop); <-s.done })
}

func (s *Sampler) loop() {
	defer close(s.done)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.tick()
		}
	}
}

// tick pulls one machine sample, derives per-second rates, records history,
// and pushes to clients if any are watching.
func (s *Sampler) tick() {
	cur, err := sampleMachine()
	if err != nil {
		// A failed sample is not fatal; keep the last known values on screen.
		s.hub.Broadcast("sys:alert", Alert{
			Kind: "sampler", Level: "warn", Value: 0,
			Zh: fmt.Sprintf("采样失败：%v", err),
			En: fmt.Sprintf("sample failed: %v", err),
		})
		return
	}
	procs := processSamples()

	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		cpu float64
		dt  float64
	)
	if s.prev != nil {
		dt = cur.ts.Sub(s.prev.ts).Seconds()
		if dt > 0 {
			cpu = clampPct(cpuPercent(*s.prev, cur, dt))
		}
	}

	stats := Stats{
		CPU:        clampPct(cpu),
		Memory:     clampPct(float64(cur.memLoad)),
		MemTotal:   cur.memTotal,
		MemUsed:    cur.memTotal - cur.memAvail,
		SwapTotal:  cur.swapTotal,
		SwapUsed:   cur.swapTotal - cur.swapAvail,
		Procs:      len(procs),
		NumCPU:     runtime.NumCPU(),
		GoRoutines: runtime.NumGoroutine(),
		Uptime:     int64(time.Since(startedAt).Seconds()),
		UpdatedAt:  cur.ts.Unix(),
	}
	s.current = stats
	curp := cur
	s.prev = &curp

	// Per-process CPU uses the same window the totals do, so first-tick
	// processes (no previous sample) read as 0%, which is honest.
	s.procs = s.deriveProcs(procs, dt)

	s.series = append(s.series, Sample{CPU: stats.CPU, Memory: stats.Memory, At: stats.UpdatedAt})
	if len(s.series) > seriesCap {
		s.series = s.series[len(s.series)-seriesCap:]
	}

	alerts := s.evaluate(stats)

	// Idle networks stay idle: a dashboards nobody is looking at costs a
	// syscall storm, not a broadcast storm.
	if s.hub == nil || s.hub.Clients() == 0 {
		return
	}
	s.hub.Broadcast("sys:stats", stats)
	for _, a := range alerts {
		s.hub.Broadcast("sys:alert", a)
	}
}

// deriveProcs computes per-process CPU from the previous tick's totals and
// keeps the table ordered by memory.
func (s *Sampler) deriveProcs(cur []procSample, dt float64) []Process {
	prev := s.prevProcs
	next := make(map[uint32]*procSample, len(cur))
	out := make([]Process, 0, len(cur))
	cores := float64(runtime.NumCPU())
	if cores < 1 {
		cores = 1
	}
	for i := range cur {
		p := &cur[i]
		next[p.pid] = p
		cpu := 0.0
		if dt > 0 {
			if old, ok := prev[p.pid]; ok {
				busy := float64(p.total-old.total) / 1e7 // 100ns ticks → seconds
				cpu = busy / dt / cores * 100
			}
		}
		out = append(out, Process{
			PID:     p.pid,
			Name:    p.name,
			Threads: p.threads,
			CPU:     clampPct(cpu),
			Memory:  p.mem,
		})
	}
	s.prevProcs = next
	sort.Slice(out, func(i, j int) bool { return out[i].Memory > out[j].Memory })
	if len(out) > processLimit*4 {
		out = out[:processLimit*4]
	}
	return out
}

// evaluate raises an alert after alertStreak consecutive hot samples, and
// clears the dedup lock once the machine recovers.
func (s *Sampler) evaluate(st Stats) []Alert {
	var alerts []Alert

	if st.CPU >= alertCPUPct {
		s.cpuStreak++
	} else {
		s.cpuStreak = 0
		s.alertOpen["cpu"] = false
	}
	if st.Memory >= alertMemPct {
		s.memStreak++
	} else {
		s.memStreak = 0
		s.alertOpen["memory"] = false
	}

	if s.cpuStreak == alertStreak && !s.alertOpen["cpu"] {
		s.alertOpen["cpu"] = true
		alerts = append(alerts, Alert{
			Kind: "cpu", Level: "crit", Value: st.CPU,
			Zh: fmt.Sprintf("CPU 连续 %d 秒超过 %.0f%%，当前 %.1f%%", alertStreak, alertCPUPct, st.CPU),
			En: fmt.Sprintf("CPU above %.0f%% for %d seconds, now %.1f%%", alertCPUPct, alertStreak, st.CPU),
		})
	}
	if s.memStreak == alertStreak && !s.alertOpen["memory"] {
		s.alertOpen["memory"] = true
		alerts = append(alerts, Alert{
			Kind: "memory", Level: "crit", Value: st.Memory,
			Zh: fmt.Sprintf("内存连续 %d 秒超过 %.0f%%，当前 %.1f%%", alertStreak, alertMemPct, st.Memory),
			En: fmt.Sprintf("Memory above %.0f%% for %d seconds, now %.1f%%", alertMemPct, alertStreak, st.Memory),
		})
	}
	return alerts
}

// Stats returns the latest cached snapshot (the sys.stats RPC result).
func (s *Sampler) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Processes returns the top-N processes by memory (the sys.processes RPC
// result). limit<=0 means "give me everything you sampled".
func (s *Sampler) Processes(limit int) []Process {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.procs) {
		limit = len(s.procs)
	}
	out := make([]Process, limit)
	copy(out, s.procs[:limit])
	return out
}

// Series returns the last `seconds` samples oldest-first (the sys.series RPC
// result), so a window that just opened can draw a warm chart, not an empty
// one.
func (s *Sampler) Series(seconds int) []Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if seconds <= 0 {
		seconds = 90
	}
	if seconds > len(s.series) {
		seconds = len(s.series)
	}
	out := make([]Sample, seconds)
	copy(out, s.series[len(s.series)-seconds:])
	return out
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
