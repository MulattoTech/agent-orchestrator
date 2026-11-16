package sessionmanager

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSessionHealthDetectsLostChildWithoutMutatingRows(t *testing.T) {
	f := newAdmissionFixture(t, 4, false)
	ctx := context.Background()
	first, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("blue")); err != nil {
		t.Fatal(err)
	}
	healthy, err := f.manager.HealthSnapshot(ctx)
	if err != nil || healthy.Status != "HEALTHY" || healthy.AliveControllers != 2 || healthy.VerifiedRuntimeProcesses != 2 || healthy.RunningToolProcesses != nil {
		t.Fatal(healthy, err)
	}
	if err := f.rt.Destroy(ctx, ports.RuntimeHandle{ID: first.Metadata.RuntimeHandleID}); err != nil {
		t.Fatal(err)
	}
	before, found, err := f.store.GetSession(ctx, first.ID)
	if err != nil || !found {
		t.Fatal(err)
	}
	broken, err := f.manager.HealthSnapshot(ctx)
	if err != nil || broken.Status != "DEGRADED" || broken.MissingControllers != 1 || broken.AliveControllers != 1 || broken.VerifiedRuntimeProcesses != 1 {
		t.Fatal(broken, err)
	}
	after, found, err := f.store.GetSession(ctx, first.ID)
	if err != nil || !found || !reflect.DeepEqual(before, after) {
		t.Fatal("health observation mutated durable session", err)
	}
	// Readiness of the service/store and durable occupancy are separate from
	// liveness: the rows still exist even though one exact owned child is absent.
	occupancy, err := f.manager.AdmissionSnapshot(ctx)
	if err != nil || occupancy.ActiveWorkerSessions != 2 {
		t.Fatal(occupancy, err)
	}
	if live, external := f.rt.counts(); live != 1 || !external {
		t.Fatal("health probe changed processes", live, external)
	}
}

func TestSessionHealthFencesUnknownGeneration(t *testing.T) {
	f := newAdmissionFixture(t, 4, false)
	ctx := context.Background()
	row, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
	if err != nil {
		t.Fatal(err)
	}
	f.rt.mu.Lock()
	f.rt.generation[row.Metadata.RuntimeHandleID] = "other-generation"
	f.rt.mu.Unlock()
	snap, err := f.manager.HealthSnapshot(ctx)
	if err != nil || snap.Status != "UNKNOWN" || snap.UnknownControllers != 1 || snap.MissingControllers != 0 || snap.VerifiedRuntimeProcesses != 0 || snap.Sessions[0].Evidence != "generation_mismatch" {
		t.Fatal(snap, err)
	}
}

func TestSessionHealthDistinguishesChatAttachmentFromProcessProof(t *testing.T) {
	m, st, _, _ := newManager()
	rec := mkLive("mer-1")
	rec.Mode = domain.SessionModeChat
	rec.Metadata.ControllerGeneration = "controller-1"
	st.sessions[rec.ID] = rec
	launcher := &recordingLauncher{live: true}
	m.chat = launcher
	snap, err := m.HealthSnapshot(context.Background())
	if err != nil || snap.Status != "UNKNOWN" || snap.AliveControllers != 1 || snap.UnknownProcesses != 1 || snap.VerifiedRuntimeProcesses != 0 || snap.RunningToolProcesses != nil {
		t.Fatal(snap, err)
	}
	launcher.live = false
	snap, err = m.HealthSnapshot(context.Background())
	if err != nil || snap.Status != "DEGRADED" || snap.MissingControllers != 1 {
		t.Fatal(snap, err)
	}
	if len(launcher.started) != 0 || len(launcher.preflighted) != 0 || len(launcher.turns) != 0 {
		t.Fatal("health check attempted provider activity")
	}
}

func TestSessionHealthLeavesStoppedSessionIdle(t *testing.T) {
	m, st, _, _ := newManager()
	rec := mkLive("mer-1")
	rec.Activity.State = domain.ActivityExited
	st.sessions[rec.ID] = rec
	snap, err := m.HealthSnapshot(context.Background())
	if err != nil || snap.Status != "IDLE" || snap.StoppedSessions != 1 || snap.ExpectedControllers != 0 {
		t.Fatal(snap, err)
	}
}

type stallingHealthRuntime struct{ *mtechFleetRuntime }

func (*stallingHealthRuntime) ProbeFencedRuntime(ctx context.Context, _ ports.FencedRuntimeRef) ports.FencedProbeResult {
	<-ctx.Done()
	return ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonProbeFailed}
}
func TestSessionHealthBoundsProbeAndHonorsCancellation(t *testing.T) {
	f := newAdmissionFixture(t, 4, false)
	ctx := context.Background()
	if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red")); err != nil {
		t.Fatal(err)
	}
	f.manager.runtime = &stallingHealthRuntime{f.rt}
	start := time.Now()
	snap, err := f.manager.HealthSnapshot(ctx)
	if err != nil || snap.Status != "UNKNOWN" || snap.UnknownControllers != 1 || time.Since(start) > 3*time.Second {
		t.Fatal(snap, err, time.Since(start))
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.manager.HealthSnapshot(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
