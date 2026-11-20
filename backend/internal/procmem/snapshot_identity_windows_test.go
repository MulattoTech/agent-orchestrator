//go:build windows

package procmem

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const syntheticCaptureTime = uint64(1)<<32 + 100

type snapshotProcessFixture struct {
	creation uint64
	openErr  error
	timesErr error
	memoryOK bool
}

type snapshotFixture struct {
	captureTime uint64
	rows        []Process
	processes   map[uint32]snapshotProcessFixture
	createErr   error
	firstErr    error
	nextErr     error
	nextErrAt   int
}

// All eight operations are synthetic. Handles are numeric fixture keys, and
// this helper never calls Snapshot or any native Windows operation.
func runSnapshotFixture(t *testing.T, ctx context.Context, fixture snapshotFixture) (*Table, error) {
	t.Helper()
	var opened, closed []windows.Handle
	calls := 0
	index := 0
	const snapshotHandle = windows.Handle(7)
	processFor := func(handle windows.Handle) snapshotProcessFixture {
		t.Helper()
		process, ok := fixture.processes[uint32(handle)-1000]
		if !ok {
			t.Fatalf("unexpected synthetic process handle %d", handle)
		}
		return process
	}
	fillEntry := func(entry *windows.ProcessEntry32) {
		t.Helper()
		if entry.Size == 0 {
			t.Fatal("process entry size was not initialized")
		}
		row := fixture.rows[index]
		entry.ProcessID, entry.ParentProcessID = uint32(row.PID), uint32(row.PPID)
		entry.ExeFile = [260]uint16{}
		copy(entry.ExeFile[:], utf16.Encode([]rune(row.Command)))
	}
	filetime := func(ticks uint64) windows.Filetime {
		return windows.Filetime{LowDateTime: uint32(ticks), HighDateTime: uint32(ticks >> 32)}
	}
	table, err := snapshotWindows(ctx, windowsSnapshotOps{
		captureTime: func(value *windows.Filetime) {
			calls++
			*value = filetime(fixture.captureTime)
		},
		create: func(flags, pid uint32) (windows.Handle, error) {
			calls++
			if flags != windows.TH32CS_SNAPPROCESS || pid != 0 {
				t.Fatalf("unexpected synthetic snapshot flags=%d pid=%d", flags, pid)
			}
			if fixture.createErr != nil {
				return windows.InvalidHandle, fixture.createErr
			}
			opened = append(opened, snapshotHandle)
			return snapshotHandle, nil
		},
		first: func(handle windows.Handle, entry *windows.ProcessEntry32) error {
			calls++
			if handle != snapshotHandle {
				t.Fatalf("unexpected snapshot handle %d", handle)
			}
			if fixture.firstErr != nil {
				return fixture.firstErr
			}
			if len(fixture.rows) == 0 {
				return windows.ERROR_NO_MORE_FILES
			}
			fillEntry(entry)
			return nil
		},
		next: func(handle windows.Handle, entry *windows.ProcessEntry32) error {
			calls++
			if handle != snapshotHandle {
				t.Fatalf("unexpected snapshot handle %d", handle)
			}
			index++
			if fixture.nextErr != nil && index == fixture.nextErrAt {
				return fixture.nextErr
			}
			if index >= len(fixture.rows) {
				return windows.ERROR_NO_MORE_FILES
			}
			fillEntry(entry)
			return nil
		},
		open: func(access uint32, inherit bool, pid uint32) (windows.Handle, error) {
			calls++
			if access != windows.PROCESS_QUERY_LIMITED_INFORMATION || inherit {
				t.Fatal("unexpected process-open authority")
			}
			handle := windows.Handle(pid + 1000)
			process := processFor(handle)
			if process.openErr != nil {
				return 0, process.openErr
			}
			opened = append(opened, handle)
			return handle, nil
		},
		memory: func(handle windows.Handle) (uint64, bool) {
			calls++
			return 64, processFor(handle).memoryOK
		},
		times: func(handle windows.Handle, creation, exit, kernel, user *windows.Filetime) error {
			calls++
			process := processFor(handle)
			*creation, *kernel, *user = filetime(process.creation), filetime(2*1e7), filetime(3*1e7)
			return process.timesErr
		},
		close: func(handle windows.Handle) error {
			calls++
			closed = append(closed, handle)
			return nil
		},
	})
	sort.Slice(opened, func(i, j int) bool { return opened[i] < opened[j] })
	sort.Slice(closed, func(i, j int) bool { return closed[i] < closed[j] })
	if !reflect.DeepEqual(opened, closed) {
		t.Errorf("opened handles %v were not each closed exactly once: %v", opened, closed)
	}
	if ctx.Err() != nil && calls != 0 {
		t.Errorf("cancelled context reached %d injected operations", calls)
	}
	return table, err
}

func TestWindowsSnapshotRejectsUncertainHandoff(t *testing.T) {
	row := Process{PID: 100, PPID: 10, Command: "worker.exe"}
	stable := snapshotProcessFixture{creation: syntheticCaptureTime - 10, memoryOK: true}
	cases := []struct {
		name        string
		captureTime uint64
		rows        []Process
		processes   map[uint32]snapshotProcessFixture
		want        []Process
		wantError   bool
	}{
		{name: "older_creation_keeps_row", captureTime: syntheticCaptureTime, rows: []Process{row}, processes: map[uint32]snapshotProcessFixture{100: stable}, want: []Process{{PID: 100, PPID: 10, Command: "worker.exe", CreationTime: syntheticCaptureTime - 10, RSSBytes: 64, CPUSeconds: 5}}},
		{name: "equal_creation_keeps_row", captureTime: syntheticCaptureTime, rows: []Process{row}, processes: map[uint32]snapshotProcessFixture{100: {creation: syntheticCaptureTime, memoryOK: true}}, want: []Process{{PID: 100, PPID: 10, Command: "worker.exe", CreationTime: syntheticCaptureTime, RSSBytes: 64, CPUSeconds: 5}}},
		{name: "later_creation_rejects_same_name_row", captureTime: syntheticCaptureTime, rows: []Process{row}, processes: map[uint32]snapshotProcessFixture{100: {creation: syntheticCaptureTime + 1, memoryOK: true}}, wantError: true},
		{name: "later_creation_rejects_whole_table", captureTime: syntheticCaptureTime, rows: []Process{row, {PID: 200, PPID: 100, Command: "worker.exe"}}, processes: map[uint32]snapshotProcessFixture{100: stable, 200: {creation: syntheticCaptureTime + 1, memoryOK: true}}, wantError: true},
		{name: "unavailable_capture_keeps_row", rows: []Process{row}, processes: map[uint32]snapshotProcessFixture{100: stable}, want: []Process{{PID: 100, PPID: 10, Command: "worker.exe", CreationTime: syntheticCaptureTime - 10, RSSBytes: 64, CPUSeconds: 5}}},
		{name: "unavailable_creation_keeps_row", captureTime: syntheticCaptureTime, rows: []Process{row}, processes: map[uint32]snapshotProcessFixture{100: {memoryOK: true}}, want: []Process{{PID: 100, PPID: 10, Command: "worker.exe", RSSBytes: 64, CPUSeconds: 5}}},
		{name: "open_failure_keeps_row", captureTime: syntheticCaptureTime, rows: []Process{row}, processes: map[uint32]snapshotProcessFixture{100: {openErr: windows.ERROR_ACCESS_DENIED}}, want: []Process{row}},
		{name: "times_failure_keeps_rss_only", captureTime: syntheticCaptureTime, rows: []Process{row}, processes: map[uint32]snapshotProcessFixture{100: {creation: syntheticCaptureTime + 1, timesErr: windows.ERROR_ACCESS_DENIED, memoryOK: true}}, want: []Process{{PID: 100, PPID: 10, Command: "worker.exe", RSSBytes: 64}}},
		{name: "memory_failure_keeps_cpu_only", captureTime: syntheticCaptureTime, rows: []Process{row}, processes: map[uint32]snapshotProcessFixture{100: {creation: syntheticCaptureTime - 10}}, want: []Process{{PID: 100, PPID: 10, Command: "worker.exe", CreationTime: syntheticCaptureTime - 10, CPUSeconds: 5}}},
		{name: "unknown_process_keeps_ancestry_and_peer", captureTime: syntheticCaptureTime, rows: []Process{row, {PID: 200, PPID: 100, Command: "child.exe"}}, processes: map[uint32]snapshotProcessFixture{100: {openErr: windows.ERROR_ACCESS_DENIED}, 200: stable}, want: []Process{row, {PID: 200, PPID: 100, Command: "child.exe", CreationTime: syntheticCaptureTime - 10, RSSBytes: 64, CPUSeconds: 5}}},
		{name: "long_command_is_capped", captureTime: syntheticCaptureTime, rows: []Process{{PID: 100, PPID: 10, Command: strings.Repeat("x", maxCommandLen+20)}}, processes: map[uint32]snapshotProcessFixture{100: stable}, want: []Process{{PID: 100, PPID: 10, Command: strings.Repeat("x", maxCommandLen), CreationTime: syntheticCaptureTime - 10, RSSBytes: 64, CPUSeconds: 5}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table, err := runSnapshotFixture(t, context.Background(), snapshotFixture{captureTime: tc.captureTime, rows: tc.rows, processes: tc.processes})
			if tc.wantError {
				if err == nil || table != nil {
					t.Errorf("incoherent handoff returned table=%v error=%v; want nil table and uncertainty error", table, err)
				}
				if errors.Is(err, ErrUnsupported) {
					t.Error("snapshot uncertainty became permanent unsupported capability")
				}
				return
			}
			if err != nil || table == nil {
				t.Fatalf("compatible observation returned table=%v error=%v", table, err)
			}
			got := table.All()
			sort.Slice(got, func(i, j int) bool { return got[i].PID < got[j].PID })
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("snapshot rows=%+v; want %+v", got, tc.want)
			}
		})
	}
}

func TestWindowsSnapshotEnumerationFailuresRemainUnknown(t *testing.T) {
	row := Process{PID: 100, PPID: 10, Command: "worker.exe"}
	rows := []Process{row, {PID: 200, PPID: 100, Command: "child.exe"}}
	processes := map[uint32]snapshotProcessFixture{100: {creation: syntheticCaptureTime - 10, memoryOK: true}, 200: {creation: syntheticCaptureTime - 5, memoryOK: true}}
	cases := []struct {
		name      string
		fixture   snapshotFixture
		cancelled bool
		wantError error
		wantRows  int
	}{
		{name: "create_failure", fixture: snapshotFixture{createErr: windows.ERROR_ACCESS_DENIED}, wantError: windows.ERROR_ACCESS_DENIED},
		{name: "first_failure", fixture: snapshotFixture{firstErr: windows.ERROR_BAD_LENGTH}, wantError: windows.ERROR_BAD_LENGTH},
		{name: "next_failure_after_one_row", fixture: snapshotFixture{rows: rows, nextErrAt: 1, nextErr: windows.ERROR_BAD_LENGTH}, wantError: windows.ERROR_BAD_LENGTH},
		{name: "next_failure_after_two_rows", fixture: snapshotFixture{rows: rows, nextErrAt: 2, nextErr: windows.ERROR_ACCESS_DENIED}, wantError: windows.ERROR_ACCESS_DENIED},
		{name: "empty_snapshot"},
		{name: "complete_snapshot", fixture: snapshotFixture{rows: rows}, wantRows: 2},
		{name: "cancelled_before_capture", cancelled: true, wantError: context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			tc.fixture.captureTime, tc.fixture.processes = syntheticCaptureTime, processes
			table, err := runSnapshotFixture(t, ctx, tc.fixture)
			if tc.wantError != nil {
				if table != nil || !errors.Is(err, tc.wantError) {
					t.Errorf("failed observation returned table=%v error=%v; want nil table and error %v", table, err, tc.wantError)
				}
				if errors.Is(err, ErrUnsupported) {
					t.Error("transient observation failure became permanent unsupported capability")
				}
				return
			}
			if err != nil || table == nil {
				t.Fatalf("complete enumeration returned table=%v error=%v", table, err)
			}
			if len(table.All()) != tc.wantRows {
				t.Errorf("enumerated %d rows; want %d", len(table.All()), tc.wantRows)
			}
		})
	}
}
