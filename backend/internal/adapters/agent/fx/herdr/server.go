package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

const (
	maxMessageBytes = 16 * 1024
	maxConnections  = 32
	clientDuration  = 2 * time.Second
	// Accept failures such as EMFILE/ENFILE are transient; back off like
	// net/http rather than abandoning every future fx report.
	minAcceptBackoff = 5 * time.Millisecond
	maxAcceptBackoff = time.Second
)

// Server owns a private platform-native IPC endpoint and submits native reports exclusively to
// the lifecycle write boundary. Stop closes active clients and waits for all
// callbacks before removing the socket that this server created.
type Server struct {
	sink       activitySink
	logger     *slog.Logger
	listener   net.Listener
	path       string
	socketInfo os.FileInfo
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	stopOnce   sync.Once
	mu         sync.Mutex
	clients    map[net.Conn]struct{}
	workers    sync.WaitGroup
}

// Start prepares the endpoint synchronously so a startup failure can be logged
// by the daemon. Cancellation and Stop both gracefully stop the server.
func Start(ctx context.Context, dataDir string, sink activitySink, logger *slog.Logger) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dataDir == "" || sink == nil {
		return nil, errors.New("fx Herdr requires data directory and lifecycle sink")
	}
	if logger == nil {
		logger = slog.Default()
	}
	listener, path, info, err := listenEndpoint(dataDir, logger)
	if err != nil {
		return nil, err
	}
	serverCtx, cancel := context.WithCancel(ctx)
	s := &Server{sink: sink, logger: logger, listener: listener, path: path, socketInfo: info, ctx: serverCtx, cancel: cancel, done: make(chan struct{}), clients: make(map[net.Conn]struct{})}
	go s.serve()
	go func() {
		select {
		case <-serverCtx.Done():
			s.shutdown()
		case <-s.done:
		}
	}()
	return s, nil
}

func (s *Server) serve() {
	defer close(s.done)
	defer removeOwnedSocket(s.path, s.socketInfo, s.logger)
	defer s.workers.Wait()
	var backoff time.Duration
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			backoff = min(max(2*backoff, minAcceptBackoff), maxAcceptBackoff)
			s.logger.Warn("accept fx Herdr client; retrying", "error", err, "backoff", backoff)
			timer := time.NewTimer(backoff)
			select {
			case <-s.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		backoff = 0
		s.mu.Lock()
		if s.ctx.Err() != nil {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		if len(s.clients) >= maxConnections {
			s.mu.Unlock()
			_ = conn.Close()
			continue
		}
		s.clients[conn] = struct{}{}
		s.workers.Add(1)
		s.mu.Unlock()
		go s.serveClient(conn)
	}
}

func (s *Server) serveClient(conn net.Conn) {
	defer s.workers.Done()
	defer func() { _ = conn.Close(); s.mu.Lock(); delete(s.clients, conn); s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(s.ctx, clientDuration)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return
	}
	reader := bufio.NewReaderSize(conn, maxMessageBytes+1)
	writer := json.NewEncoder(conn)
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil || len(line) > maxMessageBytes {
			return
		}
		if err := writer.Encode(s.handle(ctx, line)); err != nil {
			return
		}
	}
}

func (s *Server) shutdown() {
	s.stopOnce.Do(func() {
		s.cancel()
		_ = s.listener.Close()
		s.mu.Lock()
		for conn := range s.clients {
			_ = conn.Close()
		}
		s.mu.Unlock()
	})
}

// Stop is safe to call repeatedly, with or without prior context cancellation.
func (s *Server) Stop() { s.shutdown(); <-s.done }
