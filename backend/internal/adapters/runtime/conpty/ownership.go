package conpty

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/conpty/ptyregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const hostLaunchIDEnv = "AO_PTY_HOST_LAUNCH_ID"

// HostIdentity identifies one host process, independently of its TCP address.
// LaunchID is generated for every host spawn; RuntimeLaunchID is the manager's
// supervisor generation, if this is a supervised process. CreatedAt is the
// Windows process creation FILETIME, obtained independently by both peers.
type HostIdentity struct {
	SessionID       string `json:"sessionId"`
	LaunchID        string `json:"launchId"`
	RuntimeLaunchID string `json:"runtimeLaunchId,omitempty"`
	PID             int    `json:"pid"`
	CreatedAt       string `json:"createdAt"`
}

func (id HostIdentity) complete() bool {
	return id.SessionID != "" && id.LaunchID != "" && id.PID > 0 && id.CreatedAt != ""
}

func newHostLaunchID() (string, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce[:]), nil
}

func (s *hostSession) identities() []HostIdentity {
	if !nativeHostOwnershipRequired {
		return nil
	}
	return []HostIdentity{s.identity}
}

type ownedHostProcess interface {
	Alive() (bool, error)
	Kill() error
	Close() error
}

func ownershipError(err error) error {
	return errors.Join(ports.ErrRuntimeProbeInconclusive, err)
}

// resolveOwned never upgrades registry strings to process ownership. The OS
// creation fence is independently checked on every operation; the subsequent
// connection handshake binds the operation to the same immutable identity.
func (r *Runtime) resolveOwned(ctx context.Context, id string) (*hostSession, error) {
	sess, err := r.resolveWithEvidence(ctx, id)
	if err != nil || sess == nil || !nativeHostOwnershipRequired {
		return sess, err
	}
	process, err := r.openOwnedSession(id, sess, false)
	if err != nil {
		return nil, err
	}
	defer process.Close()
	alive, err := process.Alive()
	if err != nil || !alive {
		return nil, ownershipError(errors.Join(err, fmt.Errorf("conpty: recorded host for %q cannot be proved live", id)))
	}
	return sess, nil
}

func (r *Runtime) openOwnedSession(id string, sess *hostSession, terminate bool) (ownedHostProcess, error) {
	expected := sess.identity
	if !expected.complete() || expected.SessionID != id || expected.PID != sess.pid ||
		expected.RuntimeLaunchID != sess.launchID || sess.addr == unresolvedHostAddress {
		return nil, ownershipError(fmt.Errorf("conpty: exact host identity unavailable for %q; registry retained", id))
	}
	process, err := r.openHostProcess(expected.PID, expected.CreatedAt, terminate)
	if err != nil {
		return nil, ownershipError(fmt.Errorf("conpty: verify host process for %q: %w", id, err))
	}
	return process, nil
}

// readIdentityFrame is bounded before allocating. A legacy/foreign endpoint
// cannot make an ownership probe accumulate unbounded frame data.
func readIdentityFrame(conn net.Conn) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > 4096 {
		return 0, nil, errors.New("conpty: oversized identity frame")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func writeIdentityFrame(conn net.Conn, kind byte, id HostIdentity) error {
	payload, err := json.Marshal(id)
	if err != nil {
		return err
	}
	frame, err := EncodeMessage(kind, payload)
	if err != nil {
		return err
	}
	_, err = conn.Write(frame)
	return err
}

func dialOwnedHost(ctx context.Context, addr string, timeout time.Duration, identities ...HostIdentity) (net.Conn, error) {
	conn, err := dialHostContext(ctx, addr, timeout)
	if err != nil {
		if len(identities) > 0 {
			return nil, ownershipError(err)
		}
		return nil, err
	}
	if len(identities) == 0 {
		return conn, nil
	}
	expected := identities[0]
	if !expected.complete() {
		_ = conn.Close()
		return nil, ownershipError(errors.New("conpty: missing expected host identity"))
	}
	stop := armClientDeadline(ctx, conn, timeout)
	defer stop()
	fail := func(err error) (net.Conn, error) {
		_ = conn.Close()
		return nil, ownershipError(err)
	}
	if err := writeIdentityFrame(conn, MsgIdentityReq, expected); err != nil {
		return fail(err)
	}
	kind, payload, err := readIdentityFrame(conn)
	if err != nil {
		return fail(err)
	}
	var actual HostIdentity
	if kind != MsgIdentityRes || json.Unmarshal(payload, &actual) != nil || actual != expected {
		return fail(errors.New("conpty: responder host identity does not match recorded owner"))
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// authorizeClient runs before replay, registration, resize or any terminal
// input. An unauthenticated status request is read-only and is closed after
// returning identity metadata. The matched connection is never redialed.
func (h *host) authorizeClient(conn net.Conn) bool {
	if !h.cfg.Identity.complete() {
		return true
	} // legacy in-process/other-OS serve engine
	_ = conn.SetDeadline(time.Now().Add(isAliveTimeout))
	kind, payload, err := readIdentityFrame(conn)
	if err != nil {
		return false
	}
	if kind == MsgStatusReq && len(payload) == 0 {
		_, _ = conn.Write(h.statusFrame())
		return false
	}
	var expected HostIdentity
	if kind != MsgIdentityReq || json.Unmarshal(payload, &expected) != nil || expected != h.cfg.Identity {
		return false
	}
	if err := writeIdentityFrame(conn, MsgIdentityRes, h.cfg.Identity); err != nil {
		return false
	}
	_ = conn.SetDeadline(time.Time{})
	return true
}

func (h *host) statusFrame() []byte {
	code, exited := h.cfg.PTY.ExitCode()
	var exitCode *int
	if exited {
		exitCode = &code
	}
	return statusFrame(!exited, h.cfg.PTY.PID(), exitCode, h.cfg.Identity)
}

// destroyOwned retains the exact OS process handle across graceful shutdown,
// polling and any force-stop. It never calls FindProcess or kills by a newly
// resolved PID. A mismatched TCP responder does not authorize force-stop.
func (r *Runtime) destroyOwned(ctx context.Context, id string, sess *hostSession) error {
	r.mu.Lock()
	noEffect := sess.currentOwner && sess.noProcessEffect && sess.pid == 0 && sess.addr == unresolvedHostAddress
	r.mu.Unlock()
	if noEffect {
		return r.removeOwnedSession(ctx, id, sess)
	}
	process, err := r.openOwnedSession(id, sess, true)
	if err != nil {
		return err
	}
	defer process.Close()
	alive, err := process.Alive()
	if err != nil {
		return ownershipError(err)
	}
	if alive {
		if err := clientKillOwned(ctx, sess.addr, sess.identities()...); err != nil {
			return ownershipError(fmt.Errorf("conpty: fenced host shutdown refused: %w", err))
		}
		exited, err := r.waitForOwnedHostExit(ctx, process)
		if err != nil {
			return err
		}
		if !exited {
			if err := process.Kill(); err != nil {
				return ownershipError(err)
			}
			exited, err = r.waitForOwnedHostExit(ctx, process)
			if err != nil {
				return err
			}
			if !exited {
				return ownershipError(errors.New("conpty: exact host still alive after stop"))
			}
		}
	}
	return r.removeOwnedSession(ctx, id, sess)
}

func (r *Runtime) waitForOwnedHostExit(ctx context.Context, process ownedHostProcess) (bool, error) {
	alive, err := process.Alive()
	if err != nil || !alive {
		return !alive, err
	}
	if r.destroyWait <= 0 {
		return false, nil
	}
	poll := r.destroyPoll
	if poll <= 0 {
		poll = 25 * time.Millisecond
	}
	timer := time.NewTimer(r.destroyWait)
	defer timer.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			alive, err := process.Alive()
			return !alive, err
		case <-ticker.C:
			alive, err := process.Alive()
			if err != nil || !alive {
				return !alive, err
			}
		}
	}
}

// removeOwnedSession fences both the memory slot and the durable registry.
// The registry compare-and-swap also protects a successor created by another
// recovered Runtime instance, whose in-memory map cannot share this mutex.
func (r *Runtime) removeOwnedSession(ctx context.Context, id string, sess *hostSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[id] != sess {
		return ownershipError(ptyregistry.ErrOwnershipChanged)
	}
	expected := ptyregistry.Entry{
		SessionID: id, PtyHostPID: sess.pid, PipePath: sess.addr, LaunchID: sess.launchID,
		HostLaunchID: sess.identity.LaunchID, HostCreatedAt: sess.identity.CreatedAt,
	}
	if err := r.unregisterExact(ctx, expected); err != nil {
		return ownershipError(err)
	}
	delete(r.sessions, id)
	return nil
}
