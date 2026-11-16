//go:build windows

package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/aoagents/agent-orchestrator/backend/internal/browserruntime"
	"github.com/aoagents/agent-orchestrator/backend/internal/browserstream"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemon/supervisor"
	"github.com/aoagents/agent-orchestrator/backend/internal/localipc"
)

// Binds only disposable native pipes and exchanges one byte per owned pipe.
// No daemon Run, browser process, supervisor Serve, account or provider exists.
func TestWindowsPrivateIPCListenersSeparateStateRoots(t *testing.T) {
	factories := []struct {
		purpose string
		listen  func(string) (net.Listener, string, error)
	}{
		{"browser-runtime-browser", browserruntime.Listen},
		{"browser-runtime-chrome", func(path string) (net.Listener, string, error) { return browserruntime.ListenNamed(path, "chrome") }},
		{"browser-stream", browserstream.Listen},
		{"supervise", supervisor.Listen},
	}
	root := t.TempDir()
	for _, factory := range factories {
		t.Run(factory.purpose, func(t *testing.T) {
			var previous string
			for i, label := range []string{"first", "second"} {
				path := filepath.Join(root, label, "state", "running.json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				ln, address, err := factory.listen(path)
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				expected, err := localipc.PipeName(path, factory.purpose)
				if err != nil || address != expected || address == previous {
					t.Fatalf("listener address=%q expected=%q previous=%q err=%v", address, expected, previous, err)
				}
				previous = address
				done := make(chan error, 1)
				payload := byte(i + 1)
				go func() {
					conn, err := ln.Accept()
					if err != nil {
						done <- err
						return
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
					buffer := make([]byte, 1)
					_, err = io.ReadFull(conn, buffer)
					if err == nil && buffer[0] != payload {
						err = fmt.Errorf("wrong owned pipe payload %d", buffer[0])
					}
					if err == nil {
						_, err = conn.Write(buffer)
					}
					done <- err
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				conn, err := winio.DialPipeContext(ctx, address)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err = conn.Write([]byte{payload}); err != nil {
					conn.Close()
					t.Fatal(err)
				}
				got := make([]byte, 1)
				_, err = io.ReadFull(conn, got)
				conn.Close()
				if err != nil || got[0] != payload {
					t.Fatalf("owned pipe response=%v err=%v", got, err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("owned native pipe did not finish")
				}
			}
		})
	}
}
