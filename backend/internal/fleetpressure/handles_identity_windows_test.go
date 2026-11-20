//go:build windows

package fleetpressure

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/procmem"
	"golang.org/x/sys/windows"
)

// Every handle is an invented numeric fixture. No Windows process APIs, AO
// runtime, daemon or real provider are invoked by these tests.
func TestWindowsHandleCountRequiresSnapshotIdentity(t *testing.T) {
	const (
		pidA = uint32(101)
		pidB = uint32(202)
	)
	a := procmem.Process{PID: int(pidA), CreationTime: 1001}
	b := procmem.Process{PID: int(pidB), CreationTime: 2002}
	cases := []struct {
		name          string
		rows          []procmem.Process
		creation      map[uint32]uint64
		counts        map[uint32]uint32
		openFails     uint32
		creationFails uint32
		countFails    uint32
		cancelBefore  bool
		cancelAfter   uint32
		wantCount     int
		wantError     bool
		wantOpens     []windows.Handle
		wantCountOps  int
	}{
		{name: "same_process_identity", rows: []procmem.Process{a, b},
			creation: map[uint32]uint64{pidA: 1001, pidB: 2002},
			counts:   map[uint32]uint32{pidA: 3, pidB: 8}, wantCount: 11,
			wantOpens: []windows.Handle{1101, 1202}, wantCountOps: 2},
		{name: "reused_PID_must_not_attribute_unrelated_handles", rows: []procmem.Process{a, b},
			creation: map[uint32]uint64{pidA: 1001, pidB: 3003},
			counts:   map[uint32]uint32{pidA: 3, pidB: 100000}, wantError: true,
			wantOpens: []windows.Handle{1101, 1202}, wantCountOps: 1},
		{name: "missing_snapshot_identity_is_unknown", rows: []procmem.Process{{PID: int(pidA)}},
			creation: map[uint32]uint64{pidA: 1001},
			counts:   map[uint32]uint32{pidA: 3}, wantError: true,
			wantOpens: []windows.Handle{1101}, wantCountOps: 0},
		{name: "reopened_identity_unavailable_is_unknown", rows: []procmem.Process{a},
			creation: map[uint32]uint64{pidA: 0},
			counts:   map[uint32]uint32{pidA: 3}, wantError: true,
			wantOpens: []windows.Handle{1101}, wantCountOps: 0},
		{name: "creation_query_failure_is_unknown", rows: []procmem.Process{a},
			creationFails: pidA, counts: map[uint32]uint32{pidA: 3},
			wantError: true, wantOpens: []windows.Handle{1101}, wantCountOps: 0},
		{name: "open_failure_is_unknown", rows: []procmem.Process{a},
			openFails: pidA, wantError: true},
		{name: "count_failure_is_unknown", rows: []procmem.Process{a},
			creation:   map[uint32]uint64{pidA: 1001},
			countFails: pidA, wantError: true,
			wantOpens: []windows.Handle{1101}, wantCountOps: 1},
		{name: "already_cancelled_touches_no_handles", rows: []procmem.Process{a},
			cancelBefore: true, wantError: true},
		{name: "cancel_after_first_closes_first_and_stops", rows: []procmem.Process{a, b},
			creation: map[uint32]uint64{pidA: 1001, pidB: 2002},
			counts:   map[uint32]uint32{pidA: 3, pidB: 8}, cancelAfter: pidA,
			wantError: true, wantOpens: []windows.Handle{1101}, wantCountOps: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}
			var opened, closed []windows.Handle
			countOperations := 0
			ops := windowsHandleOps{
				open: func(rights uint32, inherit bool, pid uint32) (windows.Handle, error) {
					if rights != windows.PROCESS_QUERY_LIMITED_INFORMATION || inherit {
						t.Fatalf("unexpected authority rights=%d inherit=%v", rights, inherit)
					}
					if tc.openFails == pid {
						return 0, errors.New("synthetic process open failure")
					}
					h := windows.Handle(pid + 1000)
					opened = append(opened, h)
					return h, nil
				},
				creation: func(h windows.Handle) (uint64, error) {
					pid := uint32(h) - 1000
					if tc.creationFails == pid {
						return 0, errors.New("synthetic creation query failure")
					}
					return tc.creation[pid], nil
				},
				count: func(h windows.Handle) (uint32, error) {
					pid := uint32(h) - 1000
					countOperations++
					if tc.countFails == pid {
						return 0, errors.New("synthetic handle query failure")
					}
					if tc.cancelAfter == pid {
						cancel()
					}
					return tc.counts[pid], nil
				},
				close: func(h windows.Handle) error {
					closed = append(closed, h)
					return nil
				},
			}
			got, err := readWindowsTreeHandleCounts(ctx, tc.rows, ops)
			if tc.wantError {
				if err == nil || got != 0 {
					t.Errorf("unsafe/unknown snapshot produced count=%d err=%v; require unknown error", got, err)
				}
			} else if err != nil || got != tc.wantCount {
				t.Errorf("stable process counts returned count=%d err=%v; want %d", got, err, tc.wantCount)
			}
			if countOperations != tc.wantCountOps {
				t.Errorf("queried %d count operations; expected %d", countOperations, tc.wantCountOps)
			}
			if !reflect.DeepEqual(opened, tc.wantOpens) {
				t.Errorf("opened synthetic handles %v; want %v", opened, tc.wantOpens)
			}
			if !reflect.DeepEqual(opened, closed) {
				t.Errorf("synthetic handles not closed exactly once: opened=%v closed=%v", opened, closed)
			}
		})
	}
}
