//go:build windows

package gitworktree

import (
	"context"
	"fmt"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// Apply long-path support to the exact Git process, including capture and
// restore operations. Never rely on or mutate the user's global Git config.
func platformGitArgs(args []string) []string {
	out := make([]string, 0, len(args)+2)
	out = append(out, "-c", "core.longpaths=true")
	return append(out, args...)
}

// configurePlatformLongPaths persists the repository-local setting Git for
// Windows actually reads when extensions.worktreeConfig is enabled. Git
// 2.56.0.windows.4 ignores -c / GIT_CONFIG_COUNT for long-path checkout in that
// mode. This is scoped to the registered repository; no global config or OS
// registry change is made. Failure must abort preparation before checkout.
func configurePlatformLongPaths(ctx context.Context, binary, repo string) error {
	args := []string{"-C", repo, "config", "--local", "core.longpaths", "true"}
	cmd := aoprocess.CommandContext(ctx, binary, platformGitArgs(args)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gitworktree: enable repository long paths: %w", commandError{args: append([]string{binary}, args...), output: string(out), err: err})
	}
	return nil
}
