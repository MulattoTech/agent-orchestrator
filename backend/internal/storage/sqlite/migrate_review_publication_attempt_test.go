package sqlite

import "testing"

func TestMigrationPublicationBackfillsLegacyResultsAsUncertain(t *testing.T) {
	db := openMigratedDatabaseCopyNoForeignKeys(t, 20261116040000629)
	insert := func(id, status, verdict, body, receipt string) {
		t.Helper()
		_, err := db.Exec(`INSERT INTO review_run
          (id,review_id,session_id,harness,pr_url,target_sha,status,verdict,body,github_review_id,created_at)
          VALUES (?,'reviewer','worker','codex','https://github.com/o/r/pull/1',?,?, ?,?,?, '2026-11-16T00:00:00Z')`,
			id, id+"-sha", status, verdict, body, receipt)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("complete", "complete", "approved", "saved exact body", "")
	insert("delivered", "delivered", "changes_requested", "saved second body", "991")
	insert("failed", "failed", "", "failure", "")
	insert("running", "running", "", "", "")
	upTo(t, db, 20261116060406021)
	var count int
	if err := db.QueryRow("SELECT count(*) FROM review_publication_attempt").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("backfilled=%d, want only completed verdicts", count)
	}
	for _, id := range []string{"complete", "delivered"} {
		var state, claim, body, receipt string
		var attempts int
		if err := db.QueryRow(`SELECT state,claim_id,body,provider_review_id,attempt_count
            FROM review_publication_attempt WHERE run_id=?`, id).Scan(&state, &claim, &body, &receipt, &attempts); err != nil {
			t.Fatal(err)
		}
		if state != "uncertain" || claim != "legacy:"+id || attempts != 0 {
			t.Fatalf("legacy gained POST permission: %s %s %d", state, claim, attempts)
		}
		expectedBody, expectedReceipt := "saved exact body", ""
		if id == "delivered" {
			expectedBody, expectedReceipt = "saved second body", "991"
		}
		if body != expectedBody || receipt != expectedReceipt {
			t.Fatal("legacy binding or receipt changed")
		}
	}
}
