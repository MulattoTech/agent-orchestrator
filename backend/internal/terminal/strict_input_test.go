package terminal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
)

func TestStrictPolicyRawTerminalRemainsReadOnlyWithValidChatPolicy(t *testing.T) {
	roles := map[string]inferenceguard.RolePolicy{}
	for _, role := range []string{"orchestrator", "implementation", "complex", "reviewer", "lightweight"} {
		roles[role] = inferenceguard.RolePolicy{DefaultModel: "synthetic-medium", Models: []string{"synthetic-medium"}}
	}
	policy := inferenceguard.Policy{Version: 1, AccountKey: "fake-account", AuthorizationEpoch: "fake-window", MaximumEffort: "medium", MaximumInference: 1, MaximumPerProject: 1, MaximumReviewers: 1, TurnBudget: 2, ReserveTurns: 1, Roles: roles}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AO_INFERENCE_POLICY", path)
	if err = inferenceguard.CheckLaunch("codex", "chat"); err != nil {
		t.Fatalf("test must allow governed Chat: %v", err)
	}
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound", true: "permitted-chat-lease"}[bound], func(t *testing.T) {
			pty := newFakePTY()
			pty.writeStarted = make(chan struct{}, 1)
			src := &fakeSource{alive: true, spawner: &fakeSpawner{ptys: []*fakePTY{pty}}}
			mgr := NewManager(src, nil, testLogger(), WithHeartbeat(0))
			defer mgr.Close()
			if bound {
				mgr.SetSessionInputLease(fixedSessionInputLease(true))
			}
			conn := newFakeConn()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go mgr.Serve(ctx, conn)
			conn.in <- clientMsg{Ch: chTerminal, ID: "codex-chat-stale-terminal", Type: msgOpen}
			recv(t, conn, chTerminal, msgOpened, time.Second)
			pty.push([]byte("retained output"))
			frame := recv(t, conn, chTerminal, msgData, time.Second)
			output, err := base64.StdEncoding.DecodeString(frame.Data)
			if err != nil || string(output) != "retained output" {
				t.Fatal("read-only terminal output lost")
			}
			conn.in <- clientMsg{Ch: chTerminal, ID: "codex-chat-stale-terminal", Type: msgData, Data: base64.StdEncoding.EncodeToString([]byte("bypass policy\r"))}
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			denied := false
			for !denied {
				select {
				case <-pty.writeStarted:
					t.Fatal("raw terminal bytes bypassed governed Chat policy")
				case frame := <-conn.out:
					denied = frame.Ch == chTerminal && frame.Type == msgError && frame.Error != ""
				case <-deadline.C:
					t.Fatal("raw input refusal not returned")
				}
			}
			if len(pty.writtenBytes()) != 0 {
				t.Fatal("raw bytes reached terminal")
			}
			conn.in <- clientMsg{Ch: chTerminal, ID: "codex-chat-stale-terminal", Type: msgResize, Rows: 30, Cols: 100}
			eventually(t, time.Second, func() bool { rs := pty.resizeCalls(); return len(rs) == 1 && rs[0] == [2]uint16{30, 100} })
		})
	}
}
