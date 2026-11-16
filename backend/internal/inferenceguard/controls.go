package inferenceguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type Controls struct {
	Pause             *bool `json:"pause,omitempty"`
	EmergencyStop     *bool `json:"emergency_stop,omitempty"`
	MaximumInference  *int  `json:"maximum_inference,omitempty"`
	MaximumPerProject *int  `json:"maximum_per_project,omitempty"`
	MaximumReviewers  *int  `json:"maximum_reviewers,omitempty"`
}

// UpdateControls atomically persists owner-selected dispatch limits. It cannot
// change account identity, grant a higher reasoning exception, clear a quota
// latch, or reset a budget epoch. Lowering limits never kills local tool processes.
func (g *Gate) UpdateControls(ctx context.Context, c Controls) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	p, err := g.current()
	if err != nil {
		return err
	}
	original, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if c.Pause != nil {
		p.Pause = *c.Pause
	}
	if c.EmergencyStop != nil {
		p.EmergencyStop = *c.EmergencyStop
	}
	if c.MaximumInference != nil {
		p.MaximumInference = *c.MaximumInference
	}
	if c.MaximumPerProject != nil {
		p.MaximumPerProject = *c.MaximumPerProject
	}
	if c.MaximumReviewers != nil {
		p.MaximumReviewers = *c.MaximumReviewers
	}
	if err = p.Validate(); err != nil {
		return err
	}
	path := os.Getenv(PolicyEnv)
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("owner policy file is not configured")
	}
	existing, err := ReadPolicy()
	if err != nil {
		return err
	}
	latest, marshalErr := json.Marshal(existing)
	if marshalErr != nil {
		return marshalErr
	}
	if !bytes.Equal(original, latest) {
		return errors.New("owner policy changed during control update; reload before retry")
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ao-policy-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}
