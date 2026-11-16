package sessionmanager

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestAdmissionControlsPersistAcrossRestartAndPreserveWorkers(t *testing.T) {
	f := newAdmissionFixture(t, 4, true)
	ctx := context.Background()
	for _, project := range []domain.ProjectID{"red", "blue", "green"} {
		if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn(project)); err != nil {
			t.Fatal(err)
		}
	}
	limits := domain.SessionAdmissionLimits{RegisteredSlots: 2, MaxActiveWorkers: 1, DefaultProjectWorkers: 1, ProjectWorkers: map[domain.ProjectID]int{"red": 1}, QueueEnabled: false}
	if err := f.manager.UpdateAdmissionLimits(ctx, limits); err != nil {
		t.Fatal(err)
	}
	limits.ProjectWorkers["red"] = 2 // caller cannot mutate manager state after commit
	snap, err := f.manager.AdmissionSnapshot(ctx)
	if err != nil || snap.Limits.ProjectWorkers["red"] != 1 || snap.ActiveWorkerSessions != 3 {
		t.Fatal(snap, err)
	}
	snap.Limits.ProjectWorkers["red"] = 2 // snapshot cannot mutate manager state either
	if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("blue")); !errors.Is(err, ErrWorkerCapacity) {
		t.Fatal(err)
	}
	if live, _ := f.rt.counts(); live != 3 {
		t.Fatal("lowering controls killed workers", live)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = sqlite.Open(f.data)
	if err != nil {
		t.Fatal(err)
	}
	f.rebuild()
	if err := f.manager.LoadAdmissionLimits(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err = f.manager.AdmissionSnapshot(ctx)
	if err != nil || snap.Limits.RegisteredSlots != 2 || snap.Limits.MaxActiveWorkers != 1 || snap.Limits.QueueEnabled || snap.Limits.ProjectWorkers["red"] != 1 || snap.ActiveWorkerSessions != 3 {
		t.Fatal(snap, err)
	}
	if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("green")); !errors.Is(err, ErrWorkerCapacity) {
		t.Fatal(err)
	}
	if live, _ := f.rt.counts(); live != 3 {
		t.Fatal("restart altered workers", live)
	}
}

func TestAdmissionControlsRejectInvalidOrCancelledUpdates(t *testing.T) {
	f := newAdmissionFixture(t, 4, true)
	ctx := context.Background()
	valid := f.limits
	if err := f.manager.UpdateAdmissionLimits(ctx, valid); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*domain.SessionAdmissionLimits){
		func(l *domain.SessionAdmissionLimits) { l.RegisteredSlots = 41 },
		func(l *domain.SessionAdmissionLimits) { l.MaxActiveWorkers = 0 },
		func(l *domain.SessionAdmissionLimits) { l.MaxActiveWorkers = 41 },
		func(l *domain.SessionAdmissionLimits) { l.DefaultProjectWorkers = 0 },
		func(l *domain.SessionAdmissionLimits) { l.ProjectWorkers = map[domain.ProjectID]int{"red": 41} },
	} {
		candidate := cloneAdmissionLimits(valid)
		change(&candidate)
		if err := f.manager.UpdateAdmissionLimits(ctx, candidate); err == nil {
			t.Fatal("invalid controls accepted", candidate)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	candidate := cloneAdmissionLimits(valid)
	candidate.MaxActiveWorkers = 8
	if err := f.manager.UpdateAdmissionLimits(cancelled, candidate); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	persisted, found, err := f.store.GetSessionAdmissionLimits(ctx)
	if err != nil || !found || persisted.MaxActiveWorkers != 4 {
		t.Fatal(persisted, found, err)
	}
	snap, err := f.manager.AdmissionSnapshot(ctx)
	if err != nil || snap.Limits.MaxActiveWorkers != 4 {
		t.Fatal(snap, err)
	}
}

func TestAdmissionControlsPauseQueueBeforeClaim(t *testing.T) {
	f := newAdmissionFixture(t, 1, true)
	ctx := context.Background()
	occupied, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("blue")); !errors.Is(err, ErrSessionQueued) {
		t.Fatal(err)
	}
	limits := f.limits
	limits.QueueEnabled = false
	if err := f.manager.UpdateAdmissionLimits(ctx, limits); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Kill(ctx, occupied.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.DispatchAdmissionQueue(ctx, 64); err != nil {
		t.Fatal(err)
	}
	requests, err := f.manager.ListAdmissionQueue(ctx)
	if err != nil || len(requests) != 1 || requests[0].Generation != 0 || requests[0].State != "queued" {
		t.Fatal(requests, err)
	}
	if live, _ := f.rt.counts(); live != 0 {
		t.Fatal("disabled queue started worker")
	}
	limits.QueueEnabled = true
	if err := f.manager.UpdateAdmissionLimits(ctx, limits); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.DispatchAdmissionQueue(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if live, _ := f.rt.counts(); live != 1 {
		t.Fatal("enabled queue did not start exactly one worker")
	}
}

func TestAdmissionControlsLowerWhileReservationsExist(t *testing.T) {
	f := newAdmissionFixture(t, 4, false)
	ctx := context.Background()
	var releases []func()
	for range 4 {
		_, _, release, err := f.manager.reserveSessionAdmission(ctx, "red", domain.KindWorker, "")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	limits := f.limits
	limits.MaxActiveWorkers = 2
	if err := f.manager.UpdateAdmissionLimits(ctx, limits); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.manager.reserveSessionAdmission(ctx, "blue", domain.KindWorker, ""); !errors.Is(err, ErrWorkerCapacity) {
		t.Fatal(err)
	}
	for _, release := range releases {
		release()
	}
	snap, err := f.manager.AdmissionSnapshot(ctx)
	if err != nil || snap.ReservedWorkers != 0 || snap.Limits.MaxActiveWorkers != 2 {
		t.Fatal(snap, err)
	}
}
