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

// An incomplete tree read is unknown, rather than a zero-handle observation.
func readProcessTreeHandles(ctx context.Context) (int, error) {
	table, err := procmem.Snapshot(ctx, nil)
	if err != nil {
		return 0, err
	}
	tree := table.Tree(os.Getpid())
	total := 0
	for _, p := range tree.Processes {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		h, e := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.PID))
		if e != nil {
			return 0, e
		}
		var count uint32
		ok, _, callErr := processHandleCount.Call(uintptr(h), uintptr(unsafe.Pointer(&count)))
		_ = windows.CloseHandle(h)
		if ok == 0 {
			return 0, fmt.Errorf("process handle sample: %w", callErr)
		}
		total += int(count)
	}
	return total, nil
}
