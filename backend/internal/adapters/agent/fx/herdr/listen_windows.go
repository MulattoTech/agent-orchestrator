//go:build windows

package herdr

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/Microsoft/go-winio"
)

// Use the complete canonical root, not its basename: production and an
// isolated test installation with identically named leaf directories must
// never share an endpoint. The bounded name avoids sockaddr_un/MAX_PATH.
func platformSocketPath(dataDir string) string {
	root, err := filepath.Abs(dataDir)
	if err != nil {
		root = filepath.Clean(dataDir)
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(root))))
	return `\\.\pipe\ao-fx-herdr-` + hex.EncodeToString(sum[:16])
}

func listenEndpoint(dataDir string, _ *slog.Logger) (net.Listener, string, os.FileInfo, error) {
	path := SocketPath(dataDir)
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{
		// Match the browser runtime boundary: owner and LocalSystem only.
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;OW)",
	})
	return listener, path, nil, err
}

// Named pipes are kernel-owned and disappear when the owning handles close.
// Never unlink a filesystem object for a Windows IPC endpoint.
func removeOwnedSocket(string, os.FileInfo, *slog.Logger) {}
