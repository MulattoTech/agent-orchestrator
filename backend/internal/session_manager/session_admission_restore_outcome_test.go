package sessionmanager

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type admissionRestoreCrashLifecycle struct {
	lifecycleRecorder
	target domain.SessionID
	crash  error
}

func (l admissionRestoreCrashLifecycle) MarkSpawned(ctx context.Context, id domain.SessionID, metadata domain.SessionMetadata) error {
	if err := l.lifecycleRecorder.MarkSpawned(ctx, id, metadata); err != nil {
		return err
	}
	if id == l.target {
		panic(l.crash)
	}
	return nil
}

func admissionRestoreOutcomeManager(f *admissionFixture, messenger *fakeMessenger) *Manager {
	return New(Deps{
		Runtime: f.rt, Workspace: f.ws, Store: f.store,
		Agents:    singleAgent{agent: afterStartAgent{recordingAgent: &recordingAgent{}}},
		Messenger: messenger, Lifecycle: lifecycle.New(f.store, nil),
		DataDir: f.data, SessionAdmission: f.limits,
		LookPath: func(string) (string, error) { return os.Executable() },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestAdmissionRestoreRecoveryRequiresCompletionProof(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		restart                bool
		wantBlocked            bool
		wantPrompts            int
		wantAdditionalLaunches int
	}{
		{name: "normal_dispatch", wantPrompts: 1, wantAdditionalLaunches: 1},
		{name: "crash_before_launch", restart: true, wantPrompts: 1, wantAdditionalLaunches: 1},
		{name: "crash_after_mark_spawned", restart: true, wantBlocked: true},
		{name: "restore_finished_before_ack", restart: true, wantBlocked: true, wantPrompts: 1},
		{name: "manual_restore_before_dispatch", wantBlocked: true, wantPrompts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newAdmissionFixture(t, 1, true)
			archived, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
			if err != nil {
				t.Fatal(err)
			}
			dirty := filepath.Join(archived.Metadata.WorkspacePath, "owner-dirty.txt")
			dirtyContent := []byte("restore recovery must preserve this work")
			if err := os.WriteFile(dirty, dirtyContent, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.manager.Kill(ctx, archived.ID); err != nil {
				t.Fatal(err)
			}
			occupied, _, _, err := f.manager.Spawn(ctx, admissionSpawn("blue"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.manager.RestoreWithMode(ctx, archived.ID)
			var queued *SessionQueuedError
			if !errors.As(err, &queued) {
				t.Fatalf("restore was not queued: %v", err)
			}
			requestID := queued.RequestID
			if _, err := f.manager.Kill(ctx, occupied.ID); err != nil {
				t.Fatal(err)
			}
			messenger := &fakeMessenger{}
			f.manager = admissionRestoreOutcomeManager(f, messenger)

			switch tc.name {
			case "crash_before_launch", "restore_finished_before_ack":
				request, found, err := f.store.ClaimSessionAdmission(ctx, f.manager.admission.owner, time.Now().UTC())
				if err != nil || !found || request.ID != requestID {
					t.Fatalf("claim=%+v found=%t err=%v", request, found, err)
				}
				if tc.name == "restore_finished_before_ack" {
					commandCtx := context.WithValue(ctx, admissionQueueContextKey{}, request.ID)
					if _, err := f.manager.RestoreWithMode(commandCtx, archived.ID); err != nil {
						t.Fatal(err)
					}
				}
			case "crash_after_mark_spawned":
				crash := errors.New("synthetic crash after durable MarkSpawned")
				f.manager.lcm = admissionRestoreCrashLifecycle{
					lifecycleRecorder: f.manager.lcm, target: archived.ID, crash: crash,
				}
				var recovered any
				var dispatchErr error
				func() {
					defer func() { recovered = recover() }()
					dispatchErr = f.manager.DispatchAdmissionQueue(ctx, 1)
				}()
				if recovered != crash {
					t.Fatalf("crash boundary not reached: panic=%v err=%v", recovered, dispatchErr)
				}
				// The unwind only releases in-memory locks, reservations and the
				// lifecycle launch latch. No failure return or queue ack runs.
				// Assert the durable/runtime effects survived before reopening.
				record, found, err := f.store.GetSession(ctx, archived.ID)
				if err != nil || !found || record.IsTerminated || record.Metadata.RuntimeHandleID == "" {
					t.Fatalf("crash lost published restore: record=%+v found=%t err=%v", record, found, err)
				}
				if live, external := f.rt.counts(); live != 1 || !external {
					t.Fatalf("crash unwound runtime effects: live=%d external=%t", live, external)
				}
				if len(messenger.msgs) != 0 {
					t.Fatalf("crash occurred after prompt delivery: %v", messenger.msgs)
				}
				mtechAssertDirtyContent(t, dirty, dirtyContent)
			case "manual_restore_before_dispatch":
				// A live row from a separate/manual restore is not a durable
				// completion receipt for the still-queued command either.
				if _, err := f.manager.RestoreWithMode(ctx, archived.ID); err != nil {
					t.Fatal(err)
				}
			}

			before, found, err := f.store.GetSession(ctx, archived.ID)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			f.rt.mu.Lock()
			launchesBefore := f.rt.sequence
			f.rt.mu.Unlock()
			if tc.restart {
				requests, err := f.manager.ListAdmissionQueue(ctx)
				if err != nil || len(requests) != 1 || requests[0].ID != requestID || requests[0].State != "running" {
					t.Fatalf("crash must leave an unacknowledged claim: requests=%+v err=%v", requests, err)
				}
				if err := f.store.Close(); err != nil {
					t.Fatal(err)
				}
				f.store, err = sqlite.Open(f.data)
				if err != nil {
					t.Fatal(err)
				}
				f.manager = admissionRestoreOutcomeManager(f, messenger)
				if err := f.store.RecoverSessionAdmission(ctx, f.manager.admission.owner, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				if !before.IsTerminated {
					// Startup observation may preserve a live runtime; it cannot
					// prove or replay the missing after-start prompt.
					if err := f.manager.checkSessionHealth(ctx, before); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := f.manager.DispatchAdmissionQueue(ctx, 1); err != nil {
				t.Fatal(err)
			}
			requests, err := f.manager.ListAdmissionQueue(ctx)
			if err != nil || len(requests) != 1 || requests[0].ID != requestID {
				t.Fatalf("requests=%+v err=%v", requests, err)
			}
			request := requests[0]
			dependencyErr := f.manager.admissionDependenciesReady(ctx, []string{requestID})
			if tc.wantBlocked {
				if request.State != "blocked" || request.FailureCode != "OUTCOME_REQUIRES_RECONCILIATION" {
					t.Errorf("restore without completion proof was acknowledged: state=%q failure=%q", request.State, request.FailureCode)
				}
				if dependencyErr == nil {
					t.Error("unresolved restore allowed a dependent command to advance")
				}
			} else if request.State != "completed" || request.SessionID != archived.ID || dependencyErr != nil {
				t.Errorf("proven restore did not complete: request=%+v dependencyErr=%v", request, dependencyErr)
			}
			after, found, err := f.store.GetSession(ctx, archived.ID)
			if err != nil || !found || after.IsTerminated {
				t.Fatalf("restored session was not preserved: record=%+v found=%t err=%v", after, found, err)
			}
			if tc.wantBlocked && (after.Metadata.RuntimeHandleID != before.Metadata.RuntimeHandleID ||
				after.Metadata.RuntimeLaunchID != before.Metadata.RuntimeLaunchID || after.CleanupGeneration != before.CleanupGeneration) {
				t.Error("unresolved restore changed the existing runtime or lifecycle generation")
			}
			if len(messenger.msgs) != tc.wantPrompts {
				t.Errorf("prompts=%d want=%d: %v", len(messenger.msgs), tc.wantPrompts, messenger.msgs)
			}
			f.rt.mu.Lock()
			launchesAfter := f.rt.sequence
			f.rt.mu.Unlock()
			if launchesAfter != launchesBefore+tc.wantAdditionalLaunches {
				t.Errorf("runtime launches=%d want=%d", launchesAfter, launchesBefore+tc.wantAdditionalLaunches)
			}
			if live, external := f.rt.counts(); live != 1 || !external {
				t.Errorf("runtime ownership changed: live=%d external=%t", live, external)
			}
			mtechAssertDirtyContent(t, dirty, dirtyContent)
			if f.ws.forceCalls.Load() != 0 {
				t.Error("restore recovery attempted forced workspace removal")
			}
			// Completed and blocked commands are terminal for dispatch. Repeating
			// dispatch must preserve their generation and never repeat effects.
			if err := f.manager.DispatchAdmissionQueue(ctx, 1); err != nil {
				t.Fatal(err)
			}
			again, err := f.manager.ListAdmissionQueue(ctx)
			if err != nil || len(again) != 1 || again[0].State != request.State || again[0].Generation != request.Generation {
				t.Fatalf("settled request changed: requests=%+v err=%v", again, err)
			}
			f.rt.mu.Lock()
			launchesAgain := f.rt.sequence
			f.rt.mu.Unlock()
			if launchesAgain != launchesAfter || len(messenger.msgs) != tc.wantPrompts {
				t.Error("repeated dispatch replayed a launch or prompt")
			}
		})
	}
}
