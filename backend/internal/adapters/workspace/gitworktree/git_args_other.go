//go:build !windows

package gitworktree

import "context"

func platformGitArgs(args []string) []string { return args }

func configurePlatformLongPaths(context.Context, string, string) error { return nil }
