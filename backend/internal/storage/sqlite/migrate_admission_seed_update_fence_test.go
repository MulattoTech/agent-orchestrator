package sqlite

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// The additive UPDATE fence must apply to databases that already ran the
// INSERT-only migration. Test upgrade, rollback, and reapplication using
// a scratch snapshot and historical queue rows, never a live AO database.
func TestAdmissionSeedUpdateFenceMigrationPreservesHistoryAndRollback(t *testing.T) {
	const previousVersion = 20261120235000001
	ctx := context.Background()
	db := openMigratedDatabaseCopy(t, previousVersion)

	for _, project := range []string{"blue", "green"} {
		if _, err := db.ExecContext(ctx, "INSERT INTO projects (id,path,registered_at) VALUES (?,?,?)",
			project, "/scratch/"+project, "2026-11-21T01:10:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	// Insert four hidden preparations with empty client keys before the
	// upgrade. The preexisting INSERT-side trigger cannot claim the keys.
	for i := 1; i <= 4; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO sessions
          (id,project_id,num,activity_last_at,created_at,updated_at,is_task_preparation)
          VALUES (?, 'blue', ?, '2026-11-21T01:10:00Z', '2026-11-21T01:10:00Z',
                    '2026-11-21T01:10:00Z', 1)`, "blue-"+string(rune('0'+i)), i); err != nil {
			t.Fatal(err)
		}
	}
	type queued struct{ id, project, state, payload string }
	history := []queued{
		{"blocked-blue", "green", "queued", `{"ClientRequestID":"blocked-key","ClientRequestHash":"other"}`},
		{"matching-blue", "blue", "running", `{"ClientRequestID":"matching-key","ClientRequestHash":"same"}`},
		{"also-blocked", "blue", "queued", `{"ClientRequestID":"other-blocked-key","ClientRequestHash":"same"}`},
		{"malformed", "blue", "cancelled", `{"ClientRequestID":`},
	}
	for _, item := range history {
		if _, err := db.ExecContext(ctx, `INSERT INTO session_admission_queue
          (id,project_id,operation,payload,state,created_at,updated_at,available_at)
          VALUES (?,?, 'spawn',?,?,'2026-11-21T01:10:00Z',
                  '2026-11-21T01:10:00Z','2026-11-21T01:10:00Z')`,
			item.id, item.project, item.payload, item.state); err != nil {
			t.Fatal(err)
		}
	}
	readQueue := func() []queued {
		t.Helper()
		rows, err := db.QueryContext(ctx,
			"SELECT id,project_id,state,payload FROM session_admission_queue ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var found []queued
		for rows.Next() {
			var item queued
			if err := rows.Scan(&item.id, &item.project, &item.state, &item.payload); err != nil {
				t.Fatal(err)
			}
			found = append(found, item)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return found
	}
	before := readQueue()
	unchanged := func(stage string) {
		t.Helper()
		if got := readQueue(); !reflect.DeepEqual(got, before) {
			t.Fatalf("%s mutated historical queued commands: before=%+v after=%+v",
				stage, before, got)
		}
	}
	triggerCount := func() int {
		t.Helper()
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE
             type='trigger' AND name='session_admission_client_key_seed_update_fence'`).
			Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	updateKey := func(sessionID, key, hash string) error {
		_, err := db.ExecContext(ctx,
			"UPDATE sessions SET client_request_id=?, client_request_hash=? WHERE id=?",
			key, hash, sessionID)
		return err
	}
	reject := func(stage, sessionID, key, hash string) {
		t.Helper()
		err := updateKey(sessionID, key, hash)
		if err == nil || !strings.Contains(err.Error(), "admission queued key conflicts with session seed") {
			t.Errorf("%s: conflicting UPDATE not fenced, error=%v", stage, err)
		}
	}
	if triggerCount() != 0 {
		t.Fatal("UPDATE fence already existed before migration")
	}
	if err := migrate(db); err != nil {
		t.Fatalf("additive UPDATE migration failed: %v", err)
	}
	if triggerCount() != 1 {
		t.Fatal("UPDATE fence missing after migration")
	}
	unchanged("upgrade")
	reject("different project queued", "blue-1", "blocked-key", "other")
	reject("same project queued", "blue-3", "other-blocked-key", "same")
	if err := updateKey("blue-2", "matching-key", "same"); err != nil {
		t.Fatalf("legitimate running dispatcher promotion rejected: %v", err)
	}
	var keyAfterReject string
	if err := db.QueryRowContext(ctx,
		"SELECT client_request_id FROM sessions WHERE id='blue-1'").Scan(&keyAfterReject); err != nil || keyAfterReject != "" {
		t.Fatalf("rejected update modified row: id=%q err=%v", keyAfterReject, err)
	}
	unchanged("after rejected update and allowed dispatcher")

	// A Down migration only removes the UPDATE fence; it does not change
	// the previous INSERT fence or the durable historical rows.
	func() {
		gooseMu.Lock()
		defer gooseMu.Unlock()
		goose.SetBaseFS(migrationsFS)
		if err := goose.DownTo(db, "migrations", previousVersion); err != nil {
			t.Fatalf("scratch down-migration failed: %v", err)
		}
	}()
	if triggerCount() != 0 {
		t.Fatal("UPDATE trigger survived Down")
	}
	unchanged("Down")
	if err := updateKey("blue-1", "blocked-key", "other"); err != nil {
		t.Fatalf("Down did not restore old permissive UPDATE path: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("reapply UPDATE fence failed: %v", err)
	}
	if triggerCount() != 1 {
		t.Fatal("UPDATE trigger missing after reapply")
	}
	unchanged("reapply")
	if err := db.QueryRowContext(ctx,
		"SELECT client_request_id FROM sessions WHERE id='blue-1'").Scan(&keyAfterReject); err != nil || keyAfterReject != "blocked-key" {
		t.Fatalf("reapplication overwrote historical conflicting key: id=%q err=%v", keyAfterReject, err)
	}
	reject("post-reapply cross-project", "blue-4", "blocked-key", "other")
	t.Log("SYNTHETIC_UPDATE_FENCE_UP_DOWN_REAPPLY preserved all old data and legitimate running dispatcher")
}
