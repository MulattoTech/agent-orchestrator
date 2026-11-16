package sessionmanager

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type admissionWorkspaceIdentity struct {
	path   string
	info   os.FileInfo
	anchor os.FileInfo
	suffix string
}

// Resolve existing junction/symlink ancestors even when the final workspace
// does not exist yet. Native file identity also catches aliases not apparent
// in normalized strings. Unreadable/cyclic/dangling aliases fail closed.
func identifyAdmissionWorkspace(path string) (admissionWorkspaceIdentity, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return admissionWorkspaceIdentity{}, err
	}
	cursor := filepath.Clean(absolute)
	var suffix []string
	resolved := ""
	for {
		resolved, err = filepath.EvalSymlinks(cursor)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return admissionWorkspaceIdentity{}, err
		}
		if entry, lstatErr := os.Lstat(cursor); lstatErr == nil && entry.Mode()&os.ModeSymlink != 0 {
			return admissionWorkspaceIdentity{}, fmt.Errorf("dangling workspace alias cannot prove ownership")
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return admissionWorkspaceIdentity{}, err
		}
		suffix = append(suffix, filepath.Base(cursor))
		cursor = parent
	}
	anchor, err := os.Stat(resolved)
	if err != nil {
		return admissionWorkspaceIdentity{}, err
	}
	relative := ""
	for i := len(suffix) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, suffix[i])
		relative = filepath.Join(relative, suffix[i])
	}
	info, err := os.Stat(resolved)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return admissionWorkspaceIdentity{}, err
	}
	return admissionWorkspaceIdentity{path: filepath.Clean(resolved), info: info, anchor: anchor, suffix: relative}, nil
}

func sameAdmissionWorkspace(a, b admissionWorkspaceIdentity) bool {
	if a.path == b.path || (runtime.GOOS == "windows" && strings.EqualFold(a.path, b.path)) {
		return true
	}
	if a.info != nil && b.info != nil && os.SameFile(a.info, b.info) {
		return true
	}
	// Windows junctions may preserve an alias spelling even after EvalSymlinks.
	// The existing ancestor file ID plus the uncreated suffix is authoritative.
	sameSuffix := a.suffix == b.suffix || (runtime.GOOS == "windows" && strings.EqualFold(a.suffix, b.suffix))
	return sameSuffix && a.anchor != nil && b.anchor != nil && os.SameFile(a.anchor, b.anchor)
}
