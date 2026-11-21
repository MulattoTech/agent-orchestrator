package sqlite

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// Test upgrades and reversible downgrades on scratch SQLite only.
// Historical duplicates, malformed JSON and completed rows are never rewritten.
func TestAdmissionSeedFenceMigrationPreservesLegacyAndRollsBack(t *testing.T) {
	const previousVersion = 20261120081333842
	ctx := context.Background()
	db := openMigratedDatabaseCopy(t, previousVersion)
	for _, project := range []string{"blue", "green", "red"} {
		if _, err := db.ExecContext(ctx, "INSERT INTO projects (id,path,registered_at) VALUES (?,?,?)",
			project, "/scratch/"+project, "2026-11-20T12:00:00Z"); err != nil {
			t.Fatalf("create project %s: %v", project, err)
		}
	}
	type row struct{ id, project, state, payload string }
	history := []row{
		{"legacy-blue", "blue", "cancelled", `{"ClientRequestID":"legacy-key","ClientRequestHash":"hash-blue"}`},
		{"legacy-green", "green", "blocked", `{"ClientRequestID":"legacy-key","ClientRequestHash":"hash-green"}`},
		{"malformed", "red", "blocked", `{"ClientRequestID":`},
		{"running-match", "blue", "running", `{"ClientRequestID":"matching-key","ClientRequestHash":"matching-hash"}`},
		{"queued-same", "blue", "queued", `{"ClientRequestID":"queued-key","ClientRequestHash":"queued-hash"}`},
		{"running-wrong-hash", "blue", "running", `{"ClientRequestID":"wrong-hash-key","ClientRequestHash":"hash-a"}`},
		{"running-wrong-project", "green", "running", `{"ClientRequestID":"wrong-project-key","ClientRequestHash":"hash-p"}`},
		{"rollback-case", "red", "queued", `{"ClientRequestID":"rollback-key","ClientRequestHash":"hash-rollback"}`},
	}
	for _, q := range history {
		if _, err := db.ExecContext(ctx, `INSERT INTO session_admission_queue
   (id,project_id,operation,payload,state,created_at,updated_at,available_at)
   VALUES (?,?,'spawn',?,?,'2026-11-20T12:00:00Z','2026-11-20T12:00:00Z','2026-11-20T12:00:00Z')`,
			q.id, q.project, q.payload, q.state); err != nil {
			t.Fatalf("historical row %s: %v", q.id, err)
		}
	}
	readRows := func() []row {
		t.Helper()
		rows, err := db.QueryContext(ctx, "SELECT id,project_id,state,payload FROM session_admission_queue ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var found []row
		for rows.Next() {
			var q row
			if err := rows.Scan(&q.id, &q.project, &q.state, &q.payload); err != nil {
				t.Fatal(err)
			}
			found = append(found, q)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return found
	}
	before := readRows()
	if len(before) != len(history) {
		t.Fatalf("missing historical fixture rows: %d", len(before))
	}
	unchanged := func(stage string) {
		t.Helper()
		if got := readRows(); !reflect.DeepEqual(got, before) {
			t.Fatalf("%s changed history: before=%+v after=%+v", stage, before, got)
		}
	}
	triggerCount := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='session_admission_client_key_seed_fence'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if triggerCount() != 0 {
		t.Fatal("seed fence existed before migration")
	}
	if err := migrate(db); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if triggerCount() != 1 {
		t.Fatal("missing trigger after upgrade")
	}
	unchanged("upgrade")
	seed := func(id, project, key, hash string, num int) error {
		_, err := db.ExecContext(ctx, `INSERT INTO sessions
   (id,project_id,num,activity_last_at,created_at,updated_at,client_request_id,client_request_hash)
   VALUES (?,?,?,'2026-11-20T12:00:00Z','2026-11-20T12:00:00Z','2026-11-20T12:00:00Z',?,?)`,
			id, project, num, key, hash)
		return err
	}
	reject := func(label, id, project, key, hash string, num int) {
		t.Helper()
		err := seed(id, project, key, hash, num)
		if err == nil || !strings.Contains(err.Error(), "admission queued key conflicts with session seed") {
			t.Errorf("%s not rejected with seed fence: %v", label, err)
		}
	}
	reject("legacy collisions", "blue-1", "blue", "legacy-key", "hash-blue", 1)
	reject("queued, same project and hash", "blue-2", "blue", "queued-key", "queued-hash", 2)
	reject("running different hash", "blue-3", "blue", "wrong-hash-key", "changed-hash", 3)
	reject("running different project", "blue-4", "blue", "wrong-project-key", "hash-p", 4)
	if err := seed("blue-5", "blue", "matching-key", "matching-hash", 5); err != nil {
		t.Fatalf("legitimate matching running dispatcher refused: %v", err)
	}
	unchanged("matching running dispatch")
	func() {
		gooseMu.Lock()
		defer gooseMu.Unlock()
		goose.SetBaseFS(migrationsFS)
		if err := goose.DownTo(db, "migrations", previousVersion); err != nil {
			t.Fatalf("migration rollback: %v", err)
		}
	}()
	if triggerCount() != 0 {
		t.Fatal("seed trigger survived rollback")
	}
	unchanged("rollback")
	if err := seed("red-1", "red", "rollback-key", "hash-rollback", 1); err != nil {
		t.Fatalf("rollback failed to restore original insert semantics: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("reapply: %v", err)
	}
	if triggerCount() != 1 {
		t.Fatal("seed trigger missing after reapply")
	}
	unchanged("reapply")
	var preserved int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE client_request_id='rollback-key'").Scan(&preserved); err != nil || preserved != 1 {
		t.Fatalf("reapply deleted historical conflicting seed: count=%d err=%v", preserved, err)
	}
	reject("after reapply", "green-1", "green", "rollback-key", "hash-rollback", 1)
	t.Log("SYNTHETIC_SEED_FENCE_UP_DOWN_REAPPLY preserved historical duplicates, malformed payloads and matching dispatcher")
}
