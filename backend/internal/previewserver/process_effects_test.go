package previewserver

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func explicitPreviewEffects(calls *int) ProcessEffects {
	denied := errors.New("fixture process boundary")
	return ProcessEffects{
		Command:      func(string, ...string) (*exec.Cmd, error) { *calls++; return nil, denied },
		StartTime:    func(int) string { *calls++; return "fixture" },
		Terminate:    func(*exec.Cmd, string) error { *calls++; return denied },
		ForceKill:    func(*exec.Cmd, string) error { *calls++; return denied },
		ForceKillPID: func(int, string) error { *calls++; return denied },
	}
}

func TestExplicitPreviewEffectsRejectMissingBeforeRecovery(t *testing.T) {
	for _, missing := range []string{"command", "identity", "terminate", "force", "orphan"} {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, processRegistryFile)
			original := []byte("[{}]")
			if err := os.WriteFile(registry, original, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			effects := explicitPreviewEffects(&calls)
			switch missing {
			case "command":
				effects.Command = nil
			case "identity":
				effects.StartTime = nil
			case "terminate":
				effects.Terminate = nil
			case "force":
				effects.ForceKill = nil
			case "orphan":
				effects.ForceKillPID = nil
			}
			if manager, err := NewWithProcessEffects(nil, dir, effects); err == nil || manager != nil {
				t.Fatal("missing effect accepted")
			}
			content, err := os.ReadFile(registry)
			if err != nil || string(content) != string(original) || calls != 0 {
				t.Fatalf("validation allowed effects: calls=%d read=%v content=%q", calls, err, content)
			}
		})
	}
}

func TestExplicitPreviewEffectsReconcileThroughInjectedIdentity(t *testing.T) {
	dir := t.TempDir()
	record := []persistedProcess{{SessionID: "fixture-session", PID: 77777, StartTime: "fixture-birth-identity"}}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, processRegistryFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	calls, kills := 0, 0
	effects := explicitPreviewEffects(&calls)
	effects.ForceKillPID = func(pid int, identity string) error {
		kills++
		if pid != record[0].PID || identity != record[0].StartTime {
			t.Fatalf("wrong orphan identity: %d %q", pid, identity)
		}
		return nil
	}
	if _, err := NewWithProcessEffects(nil, dir, effects); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || kills != 1 {
		t.Fatalf("unexpected effects: other=%d injected_orphan=%d", calls, kills)
	}
	if _, err := os.Stat(filepath.Join(dir, processRegistryFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registry not reconciled: %v", err)
	}
}
