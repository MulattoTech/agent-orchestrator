//go:build windows

package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type windowsSignalSink chan signalRecord

func (s windowsSignalSink) ApplyActivitySignal(ctx context.Context, id domain.SessionID, signal ports.ActivitySignal) error {
	select {
	case s <- signalRecord{id, signal}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestWindowsHerdrPipeSurvivesLongRootAndRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("long-root-", 35))
	path := SocketPath(root)
	if len(path) > 80 || !strings.HasPrefix(path, `\\.\pipe\ao-fx-herdr-`) {
		t.Fatalf("unbounded/non-native endpoint: %q", path)
	}
	if path == SocketPath(filepath.Join(t.TempDir(), filepath.Base(root))) {
		t.Fatal("two installations share a pipe")
	}
	if path != SocketPath(strings.ToUpper(root)) {
		t.Fatal("Windows root casing changed endpoint")
	}
	sink := make(windowsSignalSink, 8)
	for cycle := 0; cycle < 3; cycle++ {
		ctx, cancel := context.WithCancel(context.Background())
		server, err := Start(ctx, root, sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		duplicate, duplicateErr := Start(ctx, root, sink, nil)
		if duplicateErr == nil {
			duplicate.Stop()
			server.Stop()
			cancel()
			t.Fatal("duplicate listener accepted")
		}
		dialCtx, done := context.WithTimeout(ctx, time.Second)
		conn, err := winio.DialPipeContext(dialCtx, path)
		done()
		if err != nil {
			server.Stop()
			cancel()
			t.Fatal(err)
		}
		if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		_, err = fmt.Fprintln(conn, `{"id":"7","method":"pane.report_agent","params":{"pane_id":"ao:1:bWVyLTE:bGF1bmNoLTE","source":"custom:fx","agent":"fx","state":"working"}}`)
		if err != nil {
			t.Fatal(err)
		}
		var reply response
		if err := json.NewDecoder(conn).Decode(&reply); err != nil || !reply.OK {
			t.Fatalf("reply=%+v err=%v", reply, err)
		}
		select {
		case signal := <-sink:
			if signal.id != "mer-1" || signal.signal.LaunchID != "launch-1" {
				t.Fatalf("lost ownership: %+v", signal)
			}
		case <-time.After(time.Second):
			t.Fatal("report callback missing")
		}
		_ = conn.Close()
		cancel()
		server.Stop()
		server.Stop()
	}
}
