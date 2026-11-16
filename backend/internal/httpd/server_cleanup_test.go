package httpd

import (
	"context"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func TestServerCloseBeforeRunReleasesReservedLoopbackPort(t *testing.T) {
	srv, err := NewWithDeps(config.Config{Host: "127.0.0.1", Port: 0, ShutdownTimeout: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, APIDeps{})
	if err != nil {
		t.Fatal(err)
	}
	addr := srv.Addr().String()
	srv.Close()
	srv.Close()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener leaked: %v", err)
	}
	defer ln.Close()
	_ = context.Background()
}
