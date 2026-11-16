package systeminstall

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type explicitHostEffects struct{ calls int }

func (h *explicitHostEffects) denied() error {
	h.calls++
	return errors.New("fixture external operation")
}
func (h *explicitHostEffects) LookPath(string) (string, error) { return "", h.denied() }
func (h *explicitHostEffects) Run(context.Context, []string, io.Writer, io.Writer) error {
	return h.denied()
}
func (h *explicitHostEffects) RunInstall(context.Context, ports.InstallCommand, io.Writer, io.Writer) error {
	return h.denied()
}
func (h *explicitHostEffects) RunInstallScript(context.Context, ports.InstallScriptCommand, io.Writer, io.Writer) (ports.InstallScriptResult, error) {
	return ports.InstallScriptResult{}, h.denied()
}
func (h *explicitHostEffects) InstallManagedBinary(context.Context, ports.ManagedBinaryDownload) (string, error) {
	return "", h.denied()
}
func (h *explicitHostEffects) Probe(context.Context) (ports.InstallCapabilities, error) {
	return ports.InstallCapabilities{}, h.denied()
}
func (h *explicitHostEffects) PathWritable(context.Context, string) (bool, error) {
	return false, h.denied()
}

func TestExplicitInstallerEffectsRejectMissingWithoutObservation(t *testing.T) {
	for _, missing := range []string{"host", "managed", "official", "ownership", "complete"} {
		t.Run(missing, func(t *testing.T) {
			host := &explicitHostEffects{}
			observations := 0
			effects := ExternalEffects{
				ManagedVersion:   func(context.Context, Plan) (string, error) { observations++; return "fixture-managed", nil },
				OfficialVersion:  func(context.Context, Target) (string, error) { observations++; return "fixture-official", nil },
				OwnsInstallation: func(context.Context, string, string, string, bool) (bool, error) { observations++; return true, nil },
			}
			var finder ports.ExecutableFinder = host
			switch missing {
			case "host":
				finder = nil
			case "managed":
				effects.ManagedVersion = nil
			case "official":
				effects.OfficialVersion = nil
			case "ownership":
				effects.OwnsInstallation = nil
			}
			service, err := NewWithExternalEffects(finder, host, Deps{}, effects)
			if missing != "complete" {
				if err == nil || service != nil {
					t.Fatal("missing effect accepted")
				}
			} else {
				if err != nil || service == nil {
					t.Fatalf("complete effects rejected: %v", err)
				}
				defer service.stop()
				if observations != 0 || host.calls != 0 {
					t.Fatal("constructor performed external observation")
				}
				version, err := service.officialVersion(context.Background(), TargetCodex)
				if err != nil || version != "fixture-official" || observations != 1 {
					t.Fatalf("explicit release effect not used: %q %v count%d", version, err, observations)
				}
				observations--
			}
			if host.calls != 0 || observations != 0 {
				t.Fatalf("unexpected effects: host=%d observation=%d", host.calls, observations)
			}
		})
	}
}
