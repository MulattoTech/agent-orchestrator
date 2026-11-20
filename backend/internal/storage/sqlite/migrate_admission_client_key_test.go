package sqlite

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
	"github.com/pressly/goose/v3"
)

func TestAdmissionClientKeyIndexUpgradePreservesLegacyRows(t *testing.T) {
	const previousVersion = 20261116060406021
	db := openMigratedDatabaseCopy(t, previousVersion)
	ctx := context.Background()
	store := sqlitestore.NewStore(db, db)
	for _, fixture := range []struct {
		id, project, operation, payload, state string
	}{
		{"duplicate-blue", "blue", "spawn", `{"ClientRequestID":"legacy-key","ProjectID":"blue"}`, "cancelled"},
		{"duplicate-green", "green", "spawn", `{"ClientRequestID":"legacy-key","ProjectID":"green"}`, "blocked"},
		{"single-blue", "blue", "spawn", `{"ClientRequestID":"single-key"}`, "queued"},
		{"standalone", "", "spawn", `{"ClientRequestID":"standalone-key"}`, "completed"},
		{"malformed", "red", "spawn", `{"ClientRequestID":`, "blocked"},
		{"no-client-key", "red", "spawn", `{}`, "queued"},
		{"restore-only", "red", "restore", `{"ClientRequestID":"restore-key"}`, "queued"},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO session_admission_queue
			(id,project_id,operation,payload,state,created_at,updated_at,available_at)
			VALUES (?,?,?,?,?,'2026-11-20T08:00:00Z','2026-11-20T08:00:00Z','2026-11-20T08:00:00Z')`,
			fixture.id, fixture.project, fixture.operation, fixture.payload, fixture.state)
		if err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.ListSessionAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("upgrade with legacy duplicate/malformed rows: %v", err)
	}
	assertUnchanged := func() {
		t.Helper()
		after, err := store.ListSessionAdmission(ctx)
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("index migration changed durable rows: before=%+v after=%+v err=%v", before, after, err)
		}
	}
	assertUnchanged()

	for _, tc := range []struct {
		name, key, project string
		conflict           bool
	}{
		{"legacy_blue_conflicts", "legacy-key", "blue", true},
		{"legacy_green_conflicts", "legacy-key", "green", true},
		{"legacy_third_project_conflicts", "legacy-key", "red", true},
		{"same_project_allowed", "single-key", "blue", false},
		{"other_project_conflicts", "single-key", "green", true},
		{"standalone_same_scope", "standalone-key", "", false},
		{"standalone_other_scope", "standalone-key", "blue", true},
		{"restore_does_not_claim_spawn_key", "restore-key", "blue", false},
		{"unknown_key", "unknown-key", "blue", false},
		{"empty_key", "", "blue", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conflict, err := store.HasSessionAdmissionClientKeyConflict(ctx, tc.key, domain.ProjectID(tc.project))
			if err != nil || conflict != tc.conflict {
				t.Fatalf("conflict=%t want=%t err=%v", conflict, tc.conflict, err)
			}
		})
	}

	// This is the production lookup shape, including the partial-index
	// predicates. Qualify indexed key lookup rather than a history scan.
	rows, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT 1 FROM session_admission_queue
		WHERE operation='spawn' AND json_valid(payload)
		AND json_extract(payload,'$.ClientRequestID')=? AND project_id<>? LIMIT 1`, "legacy-key", "red")
	if err != nil {
		t.Fatal(err)
	}
	var plans []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		plans = append(plans, detail)
	}
	scanErr := rows.Err()
	_ = rows.Close()
	if scanErr != nil {
		t.Fatal(scanErr)
	}
	plan := strings.Join(plans, "; ")
	if !strings.Contains(plan, "SEARCH session_admission_queue USING INDEX session_admission_spawn_client_project") ||
		!strings.Contains(plan, "<expr>=?") || strings.Contains(plan, "SCAN session_admission_queue") {
		t.Fatalf("client-key lookup does not use its key index: %s", plan)
	}
	t.Logf("ADMISSION_CLIENT_KEY_QUERY_PLAN %s", plan)

	// Rolling back only this additive index preserves all legacy commands;
	// reapplying it must remain safe with the same duplicates and invalid JSON.
	func() {
		gooseMu.Lock()
		defer gooseMu.Unlock()
		goose.SetBaseFS(migrationsFS)
		if err := goose.DownTo(db, "migrations", previousVersion); err != nil {
			t.Fatalf("roll back client-key index: %v", err)
		}
	}()
	assertUnchanged()
	var indexes int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='session_admission_spawn_client_project'").Scan(&indexes); err != nil || indexes != 0 {
		t.Fatalf("index remained after rollback: count=%d err=%v", indexes, err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("reapply client-key index: %v", err)
	}
	assertUnchanged()
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='session_admission_spawn_client_project'").Scan(&indexes); err != nil || indexes != 1 {
		t.Fatalf("index missing after reapply: count=%d err=%v", indexes, err)
	}
}
