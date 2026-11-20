//go:build windows

package sessionmanager

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/procmem"
)

// selectNativeWorkerMetrics counts only the registered leaf-worker roots.
// PPID-linked descendants remain diagnostic: a process-table relationship alone
// does not establish ownership by this fixture.
func selectNativeWorkerMetrics(table *procmem.Table, pids []int, parentPID int, helperName string) (procmem.Tree, []procmem.Process, error) {
	expected := make(map[int]bool, len(pids))
	var invalid error
	for _, pid := range pids {
		if pid <= 0 {
			if invalid == nil {
				invalid = fmt.Errorf("owned root PID must be positive: %d", pid)
			}
			continue
		}
		if expected[pid] && invalid == nil {
			invalid = fmt.Errorf("duplicate owned root PID: %d", pid)
		}
		expected[pid] = true
	}
	var additional []procmem.Process
	for _, process := range table.Tree(pids...).Processes {
		if !expected[process.PID] {
			additional = append(additional, process)
		}
	}
	if invalid != nil {
		return procmem.Tree{}, additional, invalid
	}
	byPID := make(map[int]procmem.Process, len(pids))
	for _, process := range table.All() {
		if expected[process.PID] {
			byPID[process.PID] = process
		}
	}
	var metrics procmem.Tree
	for _, pid := range pids {
		process, ok := byPID[pid]
		if !ok {
			return procmem.Tree{}, additional, fmt.Errorf("owned root PID missing from snapshot: %d", pid)
		}
		if process.PPID != parentPID {
			return procmem.Tree{}, additional, fmt.Errorf("owned root PID %d has parent %d, want %d", pid, process.PPID, parentPID)
		}
		if !strings.EqualFold(process.Command, helperName) {
			return procmem.Tree{}, additional, fmt.Errorf("owned root PID %d has helper %q, want %q", pid, process.Command, helperName)
		}
		metrics.Processes = append(metrics.Processes, process)
		metrics.RSSBytes += process.RSSBytes
		metrics.CPUSeconds += process.CPUSeconds
	}
	return metrics, additional, nil
}

func TestAdmissionNativeWorkerSnapshotSelection(t *testing.T) {
	const helper = "ao-offline-worker.exe"
	const roots = "101 50 2 00:01.50 ao-offline-worker.exe\n102 50 3 00:02.25 ao-offline-worker.exe\n"
	const extra = "203 101 40 00:09.25 unrelated.exe\n"
	extraProcess := procmem.Process{PID: 203, PPID: 101, RSSBytes: 40 * 1024, CPUSeconds: 9.25, Command: "unrelated.exe"}
	cases := []struct {
		name      string
		rows      string
		pids      []int
		wantError bool
		wantExtra []procmem.Process
	}{
		{name: "exact_owned_roots_and_metrics", rows: roots, pids: []int{102, 101}},
		{
			name: "extra_ppid_row_is_diagnostic_only", rows: roots + extra + "999 888 100 00:20.00 elsewhere.exe\n",
			pids: []int{102, 101}, wantExtra: []procmem.Process{extraProcess},
		},
		{
			name: "extra_ppid_row_cannot_replace_missing_root",
			rows: "101 50 2 00:01.50 ao-offline-worker.exe\n" + extra,
			pids: []int{101, 102}, wantError: true, wantExtra: []procmem.Process{extraProcess},
		},
		{name: "duplicate_root", rows: roots, pids: []int{101, 101}, wantError: true},
		{name: "zero_root", rows: roots, pids: []int{101, 0}, wantError: true},
		{name: "negative_root", rows: roots, pids: []int{101, -1}, wantError: true},
		{
			name: "wrong_parent",
			rows: "101 50 2 00:01.50 ao-offline-worker.exe\n102 51 3 00:02.25 ao-offline-worker.exe\n",
			pids: []int{101, 102}, wantError: true,
		},
		{
			name: "wrong_helper",
			rows: "101 50 2 00:01.50 ao-offline-worker.exe\n102 50 3 00:02.25 unrelated.exe\n",
			pids: []int{101, 102}, wantError: true,
		},
		{
			name: "helper_name_case",
			rows: "101 50 2 00:01.50 AO-OFFLINE-WORKER.EXE\n102 50 3 00:02.25 ao-offline-worker.exe\n",
			pids: []int{101, 102},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Parse consumes only these synthetic rows. This test does not take
			// an OS snapshot, create a fixture, or launch any process.
			table, err := procmem.Parse(tc.rows)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "extra_ppid_row_cannot_replace_missing_root" && len(table.Tree(tc.pids...).Processes) != len(tc.pids) {
				t.Fatal("fixture must hide the missing root behind a compensating PPID-linked row")
			}
			metrics, additional, err := selectNativeWorkerMetrics(table, tc.pids, 50, helper)
			if tc.wantError {
				if err == nil {
					t.Errorf("invalid owned root identity accepted: pids=%v metrics=%+v", tc.pids, metrics)
				}
				if len(metrics.Processes) != 0 || metrics.RSSBytes != 0 || metrics.CPUSeconds != 0 {
					t.Errorf("invalid owned roots returned verified totals: %+v", metrics)
				}
			} else if err != nil {
				t.Fatalf("owned root selection: %v", err)
			}
			if len(additional) != len(tc.wantExtra) {
				t.Errorf("additional PPID rows=%+v want=%+v", additional, tc.wantExtra)
			} else {
				for i := range tc.wantExtra {
					if additional[i] != tc.wantExtra[i] {
						t.Errorf("additional row=%+v want=%+v", additional[i], tc.wantExtra[i])
					}
				}
			}
			if tc.wantError {
				return
			}
			if len(metrics.Processes) != 2 || metrics.RSSBytes != 5*1024 || metrics.CPUSeconds != 3.75 {
				t.Fatalf("owned totals=%+v; want two roots, RSS=5120 bytes, cumulative CPU=3.75 seconds", metrics)
			}
			for i, pid := range tc.pids {
				if metrics.Processes[i].PID != pid {
					t.Errorf("owned root order/identity=%v want=%v", metrics.Processes, tc.pids)
				}
			}
		})
	}
}
