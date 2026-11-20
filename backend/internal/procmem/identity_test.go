package procmem

import (
	"fmt"
	"math"
	"reflect"
	"testing"
)

// withCreationTimes also runs against the pre-fix Process type. An absent
// metadata field leaves the original Tree/CPUPercent behavior intact, so the
// baseline fails on its results rather than on test setup or compilation.
func withCreationTimes(t *testing.T, rows string, times map[int]uint64) *Table {
	t.Helper()
	table, err := Parse(rows)
	if err != nil {
		t.Fatal(err)
	}
	for pid, ticks := range times {
		process, ok := table.byPID[pid]
		if !ok {
			t.Fatalf("fixture PID %d is missing", pid)
		}
		field := reflect.ValueOf(&process).Elem().FieldByName("CreationTime")
		if field.IsValid() {
			field.SetUint(ticks)
		}
		table.byPID[pid] = process
	}
	return table
}

func TestTreeUsesKnownProcessCreationOrder(t *testing.T) {
	const rows = "100 1 10 00:10 parent.exe\n200 100 20 00:20 child.exe\n300 200 30 00:30 grandchild.exe\n"
	cases := []struct {
		name     string
		times    [3]uint64
		roots    []int
		wantPIDs []int
		wantKiB  uint64
		wantCPU  float64
	}{
		{"ordered_ancestry", [3]uint64{10, 20, 30}, []int{100}, []int{100, 200, 300}, 60, 60},
		{"parent_newer_than_child", [3]uint64{30, 20, 40}, []int{100}, []int{100}, 10, 10},
		{"intermediate_parent_newer_than_child", [3]uint64{10, 30, 20}, []int{100}, []int{100, 200}, 30, 30},
		{"equal_timestamps", [3]uint64{20, 20, 20}, []int{100}, []int{100, 200, 300}, 60, 60},
		{"parent_timestamp_unavailable", [3]uint64{0, 20, 30}, []int{100}, []int{100, 200, 300}, 60, 60},
		{"child_timestamp_unavailable", [3]uint64{10, 0, 30}, []int{100}, []int{100, 200, 300}, 60, 60},
		{"all_timestamps_unavailable", [3]uint64{}, []int{100}, []int{100, 200, 300}, 60, 60},
		{"rejected_child_remains_an_independent_root", [3]uint64{30, 20, 40}, []int{100, 200}, []int{100, 200, 300}, 60, 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := withCreationTimes(t, rows, map[int]uint64{100: tc.times[0], 200: tc.times[1], 300: tc.times[2]})
			tree := table.Tree(tc.roots...)
			if tree.RSSBytes != tc.wantKiB*1024 || tree.CPUSeconds != tc.wantCPU {
				t.Errorf("tree totals RSS=%d CPU=%v; want RSS=%d CPU=%v", tree.RSSBytes, tree.CPUSeconds, tc.wantKiB*1024, tc.wantCPU)
			}
			gotPIDs := make([]int, len(tree.Processes))
			for i, process := range tree.Processes {
				gotPIDs[i] = process.PID
			}
			if !reflect.DeepEqual(gotPIDs, tc.wantPIDs) {
				t.Errorf("tree PIDs=%v; want %v", gotPIDs, tc.wantPIDs)
			}
			if len(table.All()) != 3 {
				t.Fatal("filtering an ancestry edge removed a process from the snapshot")
			}
		})
	}
}

func TestCPUPercentUsesProcessCreationIdentity(t *testing.T) {
	cases := []struct {
		name         string
		previousTime uint64
		currentTime  uint64
		previousCPU  int
		currentCPU   int
		elapsed      float64
		absentBefore bool
		noPrevious   bool
		validPeer    bool
		want         float64
	}{
		{name: "same_identity_growth", previousTime: 10, currentTime: 10, previousCPU: 10, currentCPU: 14, elapsed: 5, want: 80},
		{name: "same_identity_unchanged", previousTime: 10, currentTime: 10, previousCPU: 10, currentCPU: 10, elapsed: 5, want: 0},
		{name: "reused_pid_lower_total", previousTime: 10, currentTime: 20, previousCPU: 10, currentCPU: 2, elapsed: 5, want: 40},
		{name: "reused_pid_higher_total", previousTime: 10, currentTime: 20, previousCPU: 1, currentCPU: 4, elapsed: 5, want: 80},
		{name: "reused_pid_equal_total", previousTime: 10, currentTime: 20, previousCPU: 4, currentCPU: 4, elapsed: 5, want: 80},
		{name: "known_identity_counter_rollback", previousTime: 10, currentTime: 10, previousCPU: 10, currentCPU: 2, elapsed: 5, want: 0},
		{name: "current_identity_unavailable", previousTime: 10, currentTime: 0, previousCPU: 1, currentCPU: 4, elapsed: 5, want: 0},
		{name: "previous_identity_unavailable", previousTime: 0, currentTime: 10, previousCPU: 1, currentCPU: 4, elapsed: 5, want: 0},
		{name: "both_identities_unavailable_growth", previousCPU: 10, currentCPU: 14, elapsed: 5, want: 80},
		{name: "both_identities_unavailable_counter_reset", previousCPU: 10, currentCPU: 2, elapsed: 5, want: 40},
		{name: "new_pid_known_identity", currentTime: 20, currentCPU: 2, elapsed: 5, absentBefore: true, want: 40},
		{name: "new_pid_unavailable_identity", currentCPU: 2, elapsed: 5, absentBefore: true, want: 40},
		{name: "first_snapshot", currentTime: 20, currentCPU: 2, elapsed: 5, noPrevious: true, want: 0},
		{name: "zero_elapsed", previousTime: 10, currentTime: 10, previousCPU: 10, currentCPU: 14, want: 0},
		{name: "negative_elapsed", previousTime: 10, currentTime: 10, previousCPU: 10, currentCPU: 14, elapsed: -1, want: 0},
		{name: "unavailable_identity_keeps_valid_peer", previousTime: 0, currentTime: 10, previousCPU: 1, currentCPU: 4, elapsed: 5, validPeer: true, want: 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previousPID := 300
			if tc.absentBefore {
				previousPID = 999
			}
			previousRows := fmt.Sprintf("%d 1 1 00:%02d process.exe\n", previousPID, tc.previousCPU)
			currentRows := fmt.Sprintf("300 1 1 00:%02d process.exe\n", tc.currentCPU)
			previousTimes := map[int]uint64{previousPID: tc.previousTime}
			currentTimes := map[int]uint64{300: tc.currentTime}
			if tc.validPeer {
				previousRows += "301 1 1 00:20 peer.exe\n"
				currentRows += "301 1 1 00:25 peer.exe\n"
				previousTimes[301], currentTimes[301] = 70, 70
			}
			previous := withCreationTimes(t, previousRows, previousTimes)
			current := withCreationTimes(t, currentRows, currentTimes)
			if tc.noPrevious {
				previous = nil
			}
			got := CPUPercent(current.Tree(300, 301).Processes, previous, tc.elapsed)
			if math.IsNaN(got) || math.IsInf(got, 0) || math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("CPUPercent=%v; want %v", got, tc.want)
			}
		})
	}
}
