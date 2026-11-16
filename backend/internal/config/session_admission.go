package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// FleetPresets register team size; selecting one never expands paid inference.
func FleetPresets() []int { return []int{2, 4, 8, 16, 20, 30, 40} }

func loadSessionAdmission() (domain.SessionAdmissionLimits, error) {
	limits := domain.SessionAdmissionLimits{
		RegisteredSlots: 40, MaxActiveWorkers: 4, DefaultProjectWorkers: 4, QueueEnabled: true,
	}
	if raw := strings.TrimSpace(os.Getenv("AO_FLEET_PRESET")); raw != "" {
		n, err := strconv.Atoi(raw)
		valid := false
		for _, preset := range FleetPresets() {
			valid = valid || n == preset
		}
		if err != nil || !valid {
			return limits, fmt.Errorf("invalid AO_FLEET_PRESET: select 2, 4, 8, 16, 20, 30 or 40")
		}
		limits.RegisteredSlots = n
		limits.MaxActiveWorkers = min(4, n)
		limits.DefaultProjectWorkers = min(4, n)
	}
	for _, option := range []struct {
		name   string
		target *int
	}{
		{"AO_MAX_WORKER_SESSIONS", &limits.MaxActiveWorkers},
		{"AO_MAX_PROJECT_WORKER_SESSIONS", &limits.DefaultProjectWorkers},
	} {
		if raw := strings.TrimSpace(os.Getenv(option.name)); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > limits.RegisteredSlots {
				return limits, fmt.Errorf("invalid %s: expected 1..%d", option.name, limits.RegisteredSlots)
			}
			*option.target = n
		}
	}
	if raw := strings.TrimSpace(os.Getenv("AO_PROJECT_WORKER_LIMITS")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &limits.ProjectWorkers); err != nil {
			return limits, fmt.Errorf("invalid AO_PROJECT_WORKER_LIMITS: %w", err)
		}
		for id, n := range limits.ProjectWorkers {
			if strings.TrimSpace(string(id)) == "" || n < 1 || n > limits.RegisteredSlots {
				return limits, fmt.Errorf("invalid AO_PROJECT_WORKER_LIMITS: each project needs a limit in 1..%d", limits.RegisteredSlots)
			}
		}
	}
	return limits, limits.Validate()
}
