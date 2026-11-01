//go:build windows

// Windows machine sampling for the demo, in raw syscalls.
//
// Keeping to the zero-CGO rule means reading the kernel the hard way:
//
//   - GetSystemTimes gives (idle, kernel, user) in 100ns units. The
//     steady-state formula is cpu% = 1 - idleΔ/(kernelΔ + userΔ), the same
//     one Task Manager uses on boot.
//   - GlobalMemoryStatusEx gives the load percentage plus totals in one
//     call, page file included.
//   - CreateToolhelp32Snapshot + Process32First/Next walks every process;
//     per process we OpenProcess once and pull GetProcessTimes (to derive
//     CPU between ticks) and GetProcessMemoryInfo (working set). Processes
//     we can't open (system PID 4 and friends) just show no CPU.
package demo

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes           = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx     = kernel32.NewProc("GlobalMemoryStatusEx")
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW          = kernel32.NewProc("Process32FirstW")
	procProcess32NextW           = kernel32.NewProc("Process32NextW")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
	procGetProcessTimes          = kernel32.NewProc("GetProcessTimes")

	psapi                    = syscall.NewLazyDLL("psapi.dll")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

const (
	th32csSnapProcess uint32 = 0x00000002

	// The access rights a mere observer needs: query the process and peek at
	// its counters. PROCESS_VM_READ stays in for GetProcessTimes' sake on
	// older builds; it costs nothing we don't already pay.
	processQueryLimitedInfo uint32 = 0x1000
	processQueryInfo        uint32 = 0x0400
	processVMRead           uint32 = 0x0010

	winMaxPath = 260
)

type filetime struct{ Low, High uint32 }

// ticks returns the FILETIME in its native 100-nanosecond units; converting
// to time.Duration is a float division away, but every consumer here wants
// deltas, which are exact in this unit.
func (f filetime) ticks() int64 { return int64(f.High)<<32 | int64(f.Low) }

// machine is one tick's raw kernel figures. memoryStatusEx mirrors
// MEMORYSTATUSEX — Go's own alignment rules land every field on the same
// offset the C struct does: DWORD, DWORD, then DWORDLONGs at 8/16/24….
type machine struct {
	ts                   time.Time
	idle, kernel, user   int64
	memLoad              uint32
	memTotal, memAvail   uint64
	swapTotal, swapAvail uint64
}

func sampleMachine() (machine, error) {
	var m machine
	m.ts = time.Now()

	var idle, krnl, usr filetime
	if call0(procGetSystemTimes,
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&krnl)),
		uintptr(unsafe.Pointer(&usr))) == 0 {
		return m, fmt.Errorf("getSystemTimes failed")
	}
	m.idle, m.kernel, m.user = idle.ticks(), krnl.ticks(), usr.ticks()

	var mem memoryStatusEx
	mem.length = uint32(unsafe.Sizeof(mem))
	if call0(procGlobalMemoryStatusEx, uintptr(unsafe.Pointer(&mem))) == 0 {
		return m, fmt.Errorf("globalMemoryStatusEx failed")
	}
	m.memLoad = mem.memoryLoad
	m.memTotal = mem.totalPhys
	m.memAvail = mem.availPhys
	m.swapTotal = mem.totalPageFile
	m.swapAvail = mem.availPageFile
	return m, nil
}

// cpuPercent derives total CPU between two machine samples. kernel is
// inclusive of idle here, so the denominator is the full elapsed busy+idle
// span and the remaining share is genuinely unaccounted core time.
func cpuPercent(prev, cur machine, dt float64) float64 {
	if dt <= 0 {
		return 0
	}
	total := float64(cur.kernel-prev.kernel) + float64(cur.user-prev.user)
	if total <= 0 {
		return 0
	}
	idle := float64(cur.idle - prev.idle)
	if idle < 0 {
		idle = 0
	}
	return (1 - idle/total) * 100
}

type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// procSample is one process at one tick: enough to derive CPU across ticks
// and memory on the spot.
type procSample struct {
	pid     uint32
	name    string
	threads uint32
	total   int64 // kernel+user, 100ns units
	mem     uint64
}

// processEntry32W mirrors PROCESSENTRY32W; the exe-name buffer is last so
// the trailing MAX_PATH array needs no padding games.
type processEntry32W struct {
	size            uint32
	usage           uint32
	processID       uint32
	defaultHeapID   uintptr
	moduleID        uint32
	threads         uint32
	parentProcessID uint32
	priClassBase    int32
	flags           uint32
	exeFile         [winMaxPath]uint16
}

type processMemoryCountersEx struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uint64
	workingSetSize             uint64
	quotaPeakPagedPoolUsage    uint64
	quotaPagedPoolUsage        uint64
	quotaPeakNonPagedPoolUsage uint64
	quotaNonPagedPoolUsage     uint64
	pagefileUsage              uint64
	peakPagefileUsage          uint64
	privateUsage               uint64
}

// call0 keeps the 90% of Win32 calls that are plain BOOLs honest about their
// (r1, r2, errno) return shape.
func call0(p *syscall.LazyProc, args ...uintptr) uintptr {
	r, _, _ := p.Call(args...)
	return r
}

func processSamples() []procSample {
	snap, _, _ := procCreateToolhelp32Snapshot.Call(uintptr(th32csSnapProcess), 0)
	if snap == 0 {
		return nil
	}
	defer procCloseHandle.Call(snap)

	var entry processEntry32W
	entry.size = uint32(unsafe.Sizeof(entry))
	out := make([]procSample, 0, 128)
	if call0(procProcess32FirstW, snap, uintptr(unsafe.Pointer(&entry))) == 0 {
		return out
	}
	for {
		total, mem := procStats(entry.processID)
		out = append(out, procSample{
			pid:     entry.processID,
			name:    syscall.UTF16ToString(entry.exeFile[:]),
			threads: entry.threads,
			total:   total,
			mem:     mem,
		})
		if call0(procProcess32NextW, snap, uintptr(unsafe.Pointer(&entry))) == 0 {
			break
		}
	}
	return out
}

// procStats opens the process once and pulls both its CPU time and its
// working set through the same handle — halving the handle churn per tick,
// which matters with ~150 processes and a one-second sampler.
func procStats(pid uint32) (total int64, mem uint64) {
	if pid == 0 || pid == 4 {
		return 0, 0
	}
	h, _, _ := procOpenProcess.Call(uintptr(processQueryInfo|processVMRead|processQueryLimitedInfo), 0, uintptr(pid))
	if h == 0 {
		// Fall back to the fewer-privileged ask; some hardened processes
		// grant QUERY_LIMITED_INFORMATION but not VM_READ.
		h, _, _ = procOpenProcess.Call(uintptr(processQueryLimitedInfo), 0, uintptr(pid))
		if h == 0 {
			return 0, 0
		}
	}
	defer procCloseHandle.Call(h)

	var created, exited, krnl, usr filetime
	if call0(procGetProcessTimes, h,
		uintptr(unsafe.Pointer(&created)),
		uintptr(unsafe.Pointer(&exited)),
		uintptr(unsafe.Pointer(&krnl)),
		uintptr(unsafe.Pointer(&usr))) != 0 {
		total = krnl.ticks() + usr.ticks()
	}

	var pmc processMemoryCountersEx
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	if call0(procGetProcessMemoryInfo, h, uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb)) != 0 {
		mem = pmc.workingSetSize
	}
	return total, mem
}
