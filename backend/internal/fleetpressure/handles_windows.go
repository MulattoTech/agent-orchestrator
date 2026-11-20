//go:build windows

package fleetpressure

import (
	"context"
	"fmt"
	"os"
	"unsafe"

	"github.com/aoagents/agent-orchestrator/backend/internal/procmem"
	"golang.org/x/sys/windows"
)

var processHandleCount = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")

// windowsHandleOps are passed explicitly so tests never open native process
// handles. The production sampler supplies only limited-query process rights.
type windowsHandleOps struct {
	open     func(uint32, bool, uint32) (windows.Handle, error)
	creation func(windows.Handle) (uint64, error)
	count    func(windows.Handle) (uint32, error)
	close    func(windows.Handle) error
}

func queryWindowsHandleCreationTime(h windows.Handle) (uint64, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, fmt.Errorf("fleetpressure: query process creation time: %w", err)
	}
	return uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), nil
}

func queryWindowsHandleCount(h windows.Handle) (uint32, error) {
	var count uint32
	ok, _, callErr := processHandleCount.Call(uintptr(h), uintptr(unsafe.Pointer(&count)))
	if ok == 0 {
		return 0, fmt.Errorf("fleetpressure: process handle sample failed: %v", callErr)
	}
	return count, nil
}

// An incomplete tree read is unknown, rather than a zero-handle observation.
func readProcessTreeHandles(ctx context.Context) (int, error) {
	table, err := procmem.Snapshot(ctx, nil)
	if err != nil {
		return 0, err
	}
	tree := table.Tree(os.Getpid())
	return readWindowsTreeHandleCounts(ctx, tree.Processes, windowsHandleOps{
		open:     windows.OpenProcess,
		creation: queryWindowsHandleCreationTime,
		count:    queryWindowsHandleCount,
		close:    windows.CloseHandle,
	})
}

// readWindowsTreeHandleCounts runs only after a process-table snapshot. All
// process handles are re-opened by PID; their identity must be rechecked before
// attributing counters to the snapshot's descendants.
func readWindowsTreeHandleCounts(ctx context.Context, processes []procmem.Process, ops windowsHandleOps) (int, error) {
	total := 0
	for _, p := range processes {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		h, err := ops.open(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.PID))
		if err != nil {
			return 0, err
		}
		// Windows may reuse a PID between snapshot enumeration and reopen.
		// Identity is not proven if either creation timestamp is unknown.
		count, err := func() (uint32, error) {
			defer func() { _ = ops.close(h) }()
			if p.CreationTime == 0 {
				return 0, fmt.Errorf("fleetpressure: missing snapshot identity for PID %d", p.PID)
			}
			creation, e := ops.creation(h)
			if e != nil {
				return 0, e
			}
			if creation == 0 || creation != p.CreationTime {
				return 0, fmt.Errorf("fleetpressure: changed or unavailable process identity for PID %d", p.PID)
			}
			return ops.count(h)
		}()
		if err != nil {
			// Returning unknown is safer than attributing unrelated handles
			// to this daemon's process tree.
			return 0, err
		}
		if uint64(total)+uint64(count) > uint64(^uint(0)>>1) {
			return 0, fmt.Errorf("fleetpressure: process handle total overflow")
		}
		total += int(count)
	}
	return total, nil
}
