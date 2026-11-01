//go:build linux

// Linux machine sampling for the demo, via plain /proc reads —
// zero CGO, zero cgo-detection, just the files the kernel already
// publishes for us.
package demo

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// jiffiesPerSecond is USER_HZ; every mainline distro exposes 100, and
// converting jiffies below assumes it. Windows reports process CPU in
// 100ns units, so Linux ticks are rescaled to that same unit on read.
const (
	jiffiesPerSecond = 100
	jiffyTo100ns     = int64(1e9/jiffiesPerSecond) / 100
)

// machine holds /proc/stat's first line plus /proc/meminfo's headline numbers.
type machine struct {
	ts                                                    time.Time
	user, nice, system, idle, iowait, irq, softirq, steal int64
	memLoad                                               uint32
	memTotal, memAvail, swapTotal, swapAvail              uint64
}

func sampleMachine() (machine, error) {
	var m machine
	m.ts = time.Now()

	statData, err := os.ReadFile("/proc/stat")
	if err != nil {
		return m, fmt.Errorf("read /proc/stat: %w", err)
	}
	// "cpu  user nice system idle iowait irq softirq steal guest guest_nice"
	fields := strings.Fields(strings.SplitN(string(statData), "\n", 2)[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return m, fmt.Errorf("parse /proc/stat")
	}
	vals := make([]int64, 0, 8)
	for _, f := range fields[1:] {
		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			break
		}
		vals = append(vals, v)
	}
	get := func(i int) int64 {
		if i < len(vals) {
			return vals[i]
		}
		return 0
	}
	m.user, m.nice, m.system = get(0), get(1), get(2)
	m.idle, m.iowait = get(3), get(4)
	m.irq, m.softirq, m.steal = get(5), get(6), get(7)

	memData, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return m, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	var kb uint64
	for _, line := range strings.Split(string(memData), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			m.memTotal, _ = atokb(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			m.memAvail, _ = atokb(line)
		case strings.HasPrefix(line, "SwapTotal:"):
			m.swapTotal, _ = atokb(line)
		case strings.HasPrefix(line, "SwapFree:"):
			m.swapAvail, _ = atokb(line)
		}
		_ = kb
	}
	if m.memTotal > 0 {
		m.memLoad = uint32(float64(m.memTotal-m.memAvail) / float64(m.memTotal) * 100)
	}
	return m, nil
}

// atokb parses a "MemTotal:      16384000 kB" line into bytes.
func atokb(line string) (uint64, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("bad meminfo line")
	}
	v, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return v * 1024, nil
}

// totalTicks is the sum of every /proc/stat counter; busy = total - idle - iowait.
func (m machine) totalTicks() int64 {
	return m.user + m.nice + m.system + m.idle + m.iowait + m.irq + m.softirq + m.steal
}

func (m machine) idleTicks() int64 { return m.idle + m.iowait }

// cpuPercent is kernel-independent arithmetic on counter deltas.
func cpuPercent(prev, cur machine, dt float64) float64 {
	if dt <= 0 {
		return 0
	}
	total := float64(cur.totalTicks() - prev.totalTicks())
	if total <= 0 {
		return 0
	}
	return (1 - float64(cur.idleTicks()-prev.idleTicks())/total) * 100
}

type procSample struct {
	pid     uint32
	name    string
	threads uint32
	total   int64 // kernel+user, 100ns units
	mem     uint64
}

// processSamples walks /proc/<pid>/stat (CPU + thread count) and
// /proc/<pid>/statm (resident pages). Unreadable entries are skipped:
// processes come and go between ticks, and a vanished pid is not an error.
func processSamples() []procSample {
	entries, err := filepath.Glob("/proc/[0-9]*")
	if err != nil {
		return nil
	}
	out := make([]procSample, 0, len(entries))
	for _, dir := range entries {
		statData, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		s := string(statData)
		open := strings.LastIndexByte(s, ')')
		if open < 2 {
			continue
		}
		name := s[strings.IndexByte(s, '(')+1 : open]
		fields := strings.Fields(s[open+1:])
		if len(fields) < 18 {
			continue
		}
		utime, err1 := strconv.ParseInt(fields[11], 10, 64)
		stime, err2 := strconv.ParseInt(fields[12], 10, 64)
		threads, err3 := strconv.ParseUint(fields[17], 10, 32)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}

		var mem uint64
		if statm, err := os.ReadFile(filepath.Join(dir, "statm")); err == nil {
			if parts := strings.Fields(string(statm)); len(parts) >= 2 {
				if pages, err := strconv.ParseUint(parts[1], 10, 64); err == nil {
					mem = pages * uint64(os.Getpagesize())
				}
			}
		}

		out = append(out, procSample{
			pid:     uint32(pidFromDir(dir)),
			name:    name,
			threads: uint32(threads),
			total:   (utime + stime) * jiffyTo100ns,
			mem:     mem,
		})
	}
	return out
}

func pidFromDir(dir string) int {
	base := filepath.Base(dir)
	pid, _ := strconv.Atoi(base)
	return pid
}
