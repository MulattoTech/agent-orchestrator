//go:build !windows

package herdr

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func platformSocketPath(dataDir string) string {
	return filepath.Join(dataDir, "run", "fx-herdr.sock")
}

func listenEndpoint(dataDir string, logger *slog.Logger) (net.Listener, string, os.FileInfo, error) {
	path := SocketPath(dataDir)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", nil, fmt.Errorf("create fx Herdr directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, "", nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, "", nil, errors.New("fx Herdr run path is not a directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // A private directory needs owner traversal; this is not a regular file.
		return nil, "", nil, err
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, "", nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, "", nil, err
	}
	listener.SetUnlinkOnClose(false)
	info, err = os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, "", nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		removeOwnedSocket(path, info, logger)
		return nil, "", nil, err
	}
	return listener, path, info, nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("fx Herdr path is not a Unix socket: %s", path)
	}
	conn, dialErr := net.DialTimeout("unix", path, 150*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return errors.New("fx Herdr socket is already listening")
	}
	// Only connection refused proves a dead listener. Permission failures and
	// timeouts must never authorize unlinking another process's live endpoint.
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return fmt.Errorf("probe fx Herdr socket: %w", dialErr)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSocket == 0 || !os.SameFile(info, current) {
		return errors.New("fx Herdr socket changed during stale check")
	}
	return os.Remove(path)
}

func removeOwnedSocket(path string, original os.FileInfo, logger *slog.Logger) {
	current, err := os.Lstat(path)
	if err == nil && current.Mode()&os.ModeSocket != 0 && os.SameFile(original, current) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.Warn("remove fx Herdr socket", "error", err)
		}
	}
}
