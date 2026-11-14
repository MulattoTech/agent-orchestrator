package process

import (
	"context"
	"os/exec"
)

func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	configureHidden(cmd)
	return cmd
}

func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // Callers intentionally select argv; project hooks execute user-authored shell commands.
	configureHidden(cmd)
	return cmd
}

func ConfigureTreeCancellation(cmd *exec.Cmd) {
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessTree(cmd) }
}
