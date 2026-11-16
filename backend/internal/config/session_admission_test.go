package config

import (
	"fmt"
	"testing"
)

func TestSessionAdmissionLimits(t *testing.T) {
	for _, key := range []string{"AO_FLEET_PRESET", "AO_MAX_WORKER_SESSIONS", "AO_MAX_PROJECT_WORKER_SESSIONS", "AO_PROJECT_WORKER_LIMITS"} {
		t.Setenv(key, "")
	}
	defaults, err := loadSessionAdmission()
	if err != nil || defaults.RegisteredSlots != 40 || defaults.MaxActiveWorkers != 4 || defaults.DefaultProjectWorkers != 4 {
		t.Fatalf("defaults=%+v err=%v", defaults, err)
	}
	for _, n := range FleetPresets() {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			t.Setenv("AO_FLEET_PRESET", fmt.Sprint(n))
			limits, err := loadSessionAdmission()
			if err != nil || limits.RegisteredSlots != n || limits.MaxActiveWorkers > 4 {
				t.Fatalf("preset expanded active occupancy: %+v %v", limits, err)
			}
		})
	}
	for _, key := range []string{"AO_MAX_WORKER_SESSIONS", "AO_MAX_PROJECT_WORKER_SESSIONS"} {
		t.Run(key, func(t *testing.T) {
			for _, bad := range []string{"0", "-1", "41", "unlimited"} {
				t.Setenv(key, bad)
				if _, err := loadSessionAdmission(); err == nil {
					t.Fatalf("accepted %s=%s", key, bad)
				}
			}
		})
	}
	t.Setenv("AO_PROJECT_WORKER_LIMITS", `{"red":8,"blue":4}`)
	limits, err := loadSessionAdmission()
	if err != nil || limits.ProjectWorkers["red"] != 8 {
		t.Fatalf("project caps: %+v %v", limits, err)
	}
}
