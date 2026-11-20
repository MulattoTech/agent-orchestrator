//go:build windows

package procmem

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// getProcessMemoryInfo binds psapi.dll's GetProcessMemoryInfo; x/sys/windows
// wraps kernel32 and advapi32 but not psapi, so this is a hand-written binding
// like the MoveFileExW one in internal/runfile/rename_windows.go.
var (
	psapi                    = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS. Only the fields this
// package reads are named; the rest are padding of the right size so the
// struct layout matches what the DLL writes.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

func getProcessMemoryInfo(h windows.Handle) (workingSetSize uint64, ok bool) {
	var counters processMemoryCounters
	counters.cb = uint32(unsafe.Sizeof(counters))
	ret, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&counters)), uintptr(counters.cb))
	if ret == 0 {
		return 0, false
	}
	return uint64(counters.workingSetSize), true
}

// windowsSnapshotOps is supplied per call so synthetic tests never use the
// native adapter or mutable package-wide hooks.
type windowsSnapshotOps struct {
	captureTime func(*windows.Filetime)
	create      func(uint32, uint32) (windows.Handle, error)
	first       func(windows.Handle, *windows.ProcessEntry32) error
	next        func(windows.Handle, *windows.ProcessEntry32) error
	open        func(uint32, bool, uint32) (windows.Handle, error)
	memory      func(windows.Handle) (uint64, bool)
	times       func(windows.Handle, *windows.Filetime, *windows.Filetime, *windows.Filetime, *windows.Filetime) error
	close       func(windows.Handle) error
}

// Snapshot reads the current process table through Windows' own process
// list rather than a subprocess; the run parameter exists only so this
// signature matches the Unix path, and is unused here.
func Snapshot(ctx context.Context, run Runner) (*Table, error) {
	return snapshotWindows(ctx, windowsSnapshotOps{
		captureTime: windows.GetSystemTimePreciseAsFileTime,
		create:      windows.CreateToolhelp32Snapshot,
		first:       windows.Process32First,
		next:        windows.Process32Next,
		open:        windows.OpenProcess,
		memory:      getProcessMemoryInfo,
		times:       windows.GetProcessTimes,
		close:       windows.CloseHandle,
	})
}

func snapshotWindows(ctx context.Context, ops windowsSnapshotOps) (*Table, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// This cutoff can reject a known later instance, but equal/unknown times
	// and wall-clock adjustments still cannot establish process identity.
	var captured windows.Filetime
	ops.captureTime(&captured)
	capturedAt := filetimeTicks(captured)
	snap, err := ops.create(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		// Not ErrUnsupported: that verdict is permanent for the run, and this
		// call can fail transiently under handle pressure.
		return nil, fmt.Errorf("procmem: CreateToolhelp32Snapshot: %w", err)
	}
	defer func() { _ = ops.close(snap) }()

	t := &Table{byPID: map[int]Process{}, children: map[int][]int{}}
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = ops.first(snap, &entry); err == nil; err = ops.next(snap, &entry) {
		pid, ppid := int(entry.ProcessID), int(entry.ParentProcessID)
		p := Process{PID: pid, PPID: ppid, Command: windows.UTF16ToString(entry.ExeFile[:])}
		if len(p.Command) > maxCommandLen {
			p.Command = p.Command[:maxCommandLen]
		}
		// A process the daemon cannot open — a protected system process, or
		// one that exited between the snapshot and this query — contributes
		// zero memory and CPU rather than dropping out of the tree, the same
		// tolerance the ps-based path already has for a row that vanishes
		// mid-parse. PROCESS_QUERY_LIMITED_INFORMATION is the version of this
		// handle that does not need administrator rights.
		if h, openErr := ops.open(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)); openErr == nil {
			if rss, ok := ops.memory(h); ok {
				p.RSSBytes = rss
			}
			var creation, exit, kernel, user windows.Filetime
			if ops.times(h, &creation, &exit, &kernel, &user) == nil {
				p.CreationTime = filetimeTicks(creation)
				p.CPUSeconds = filetimeSeconds(kernel) + filetimeSeconds(user)
			}
			_ = ops.close(h)
			if capturedAt != 0 && p.CreationTime > capturedAt {
				// Dropping only this row would make uncertainty look like absence
				// to consumers of All(). This can also reject a process created
				// during capture; report a transient whole-snapshot failure.
				return nil, fmt.Errorf("procmem: process %d was created after snapshot capture began", pid)
			}
		}
		t.byPID[pid] = p
		t.children[ppid] = append(t.children[ppid], pid)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("procmem: enumerate process snapshot: %w", err)
	}
	return t, nil
}

// filetimeSeconds converts a FILETIME (100-nanosecond ticks) to seconds.
// filetimeTicks itself lives in system_windows.go, shared by both readers.
func filetimeSeconds(ft windows.Filetime) float64 {
	return float64(filetimeTicks(ft)) / 1e7
}
