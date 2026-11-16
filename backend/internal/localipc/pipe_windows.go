//go:build windows

// Package localipc derives local listener names from their durable state root.
// Locators are not credentials; each protocol retains its own authentication.
package localipc

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/windows"
)

var pipePurpose = regexp.MustCompile("^[a-z][a-z0-9-]{0,47}$")

// PipeName binds a service to the complete existing physical run-file directory.
// Equal basenames in different roots must never share a listener. Directory
// identity also distinguishes case-sensitive sibling directories on Windows.
// Callers publish the returned locator; clients do not reimplement this hash.
func PipeName(runFilePath, purpose string) (string, error) {
	if !pipePurpose.MatchString(purpose) || strings.HasSuffix(purpose, "-") {
		return "", errors.New("local IPC purpose is invalid")
	}
	if runFilePath == "" || !filepath.IsAbs(runFilePath) {
		return "", errors.New("local IPC requires an absolute run-file path")
	}
	root, err := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(runFilePath)))
	if err != nil {
		return "", fmt.Errorf("resolve local IPC state root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", errors.New("local IPC state root is not an existing directory")
	}
	name, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return "", fmt.Errorf("encode local IPC state root: %w", err)
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", fmt.Errorf("open local IPC state root identity: %w", err)
	}
	defer windows.CloseHandle(handle)
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &identity); err != nil {
		return "", fmt.Errorf("read local IPC state root identity: %w", err)
	}
	if identity.FileIndexHigh == 0 && identity.FileIndexLow == 0 {
		return "", errors.New("local IPC state root has no qualified directory identity")
	}
	canonical := strings.ToLower(filepath.Clean(root))
	if strings.HasPrefix(canonical, "\\\\?\\unc\\") {
		canonical = "\\\\" + strings.TrimPrefix(canonical, "\\\\?\\unc\\")
	} else {
		canonical = strings.TrimPrefix(canonical, "\\\\?\\")
	}
	authority := fmt.Sprintf("ao-local-ipc-v2\x00%s\x00%08x:%08x%08x",
		canonical, identity.VolumeSerialNumber, identity.FileIndexHigh, identity.FileIndexLow)
	digest := sha256.Sum256([]byte(authority))
	return "\\\\.\\pipe\\ao-" + purpose + "-v2-" + hex.EncodeToString(digest[:]), nil
}
