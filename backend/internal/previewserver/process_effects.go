package previewserver

import (
	"errors"
	"os/exec"
)

// ProcessEffects names every native process effect used by Manager. An
// explicit caller supplies all fields; there is no partial override fallback.
type ProcessEffects struct {
	Command      func(string, ...string) (*exec.Cmd, error)
	StartTime    func(int) string
	Terminate    func(*exec.Cmd, string) error
	ForceKill    func(*exec.Cmd, string) error
	ForceKillPID func(int, string) error
}

func (e ProcessEffects) validate() error {
	if e.Command == nil || e.StartTime == nil || e.Terminate == nil || e.ForceKill == nil || e.ForceKillPID == nil {
		return errors.New("preview process effects must all be supplied")
	}
	return nil
}

func productionProcessEffects() ProcessEffects {
	return ProcessEffects{
		Command:      func(name string, args ...string) (*exec.Cmd, error) { return previewCommand(name, args...), nil },
		StartTime:    previewProcessStartTime,
		Terminate:    terminatePreviewProcess,
		ForceKill:    forceKillPreviewProcess,
		ForceKillPID: forceKillPreviewPID,
	}
}
