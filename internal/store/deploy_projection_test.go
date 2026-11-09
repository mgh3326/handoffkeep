package store

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestDeployProjectionBackfillAndPreviousWriter(t *testing.T) {
	s := schemaStore(t, "deploy_projection")
	ctx := t.Context()
	key := "deploy/svc/20261105T010000Z"
	pr := testPR(9001)
	putDeploy(t, s, key, "svc", "success", []string{pr})
	// Re-run the registered migration over a document written before it.
	for _, q := range []string{`DROP TABLE deploy_record_projection`, `DELETE FROM schema_migrations WHERE id='20261109-deploy-record-projection'`} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM deploy_record_projection WHERE key=$1`, key).Scan(&count); err != nil || count != 1 {
		t.Fatalf("backfill count=%d err=%v", count, err)
	}
	x := flowTask(t, s, s.pool, "implement", StateReviewing, true)
	if res := send(t, s, mergeEvent(x.ID, pr)); res.State != StateDeployed {
		t.Fatalf("backfilled deploy: %+v", res)
	}

	// An old binary can update the document after backfill without updating the
	// projection. A hash mismatch must use the new document, not stale data.
	oldPR := testPR(9002)
	body, _ := json.Marshal(DeployRecord{Schema: DeployRecordSchema, Service: "svc", Result: "success", IncludedPRs: []string{oldPR}})
	if _, err := s.pool.Exec(ctx, `UPDATE documents SET body=$2,sha256=$3 WHERE key=$1`, key, string(body), "old-writer-sha"); err != nil {
		t.Fatal(err)
	}
	records, err := s.ListDeployRecords(ctx, "deploy/svc/", "", 10)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	rec, _, reasons := records[0].Classify("svc")
	if len(reasons) != 0 || len(rec.IncludedPRs) != 1 || rec.IncludedPRs[0] != oldPR {
		t.Fatalf("record=%+v reasons=%v", rec, reasons)
	}
	y := flowTask(t, s, s.pool, "implement", StateReviewing, true)
	if res := send(t, s, mergeEvent(y.ID, oldPR)); res.State != StateDeployed {
		t.Fatalf("old writer: %+v", res)
	}

	// A new key from the previous release has no projection at all.
	nextKey := "deploy/svc/20261105T020000Z"
	nextPR := testPR(9003)
	body, _ = json.Marshal(DeployRecord{Schema: DeployRecordSchema, Service: "svc", Result: "success", IncludedPRs: []string{nextPR}})
	if _, err = s.pool.Exec(ctx, `INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at) SELECT $2,kind,session,job,$3,$4,created_by,created_at,updated_at FROM documents WHERE key=$1`, key, nextKey, string(body), "previous-release"); err != nil {
		t.Fatal(err)
	}
	z := flowTask(t, s, s.pool, "implement", StateReviewing, true)
	if res := send(t, s, mergeEvent(z.ID, nextPR)); res.State != StateDeployed {
		t.Fatalf("new old-writer record: %+v", res)
	}
	records, err = s.ListDeployRecords(ctx, "deploy/svc/", "", 10)
	if err != nil || len(records) != 2 || records[0].Key != nextKey {
		t.Fatalf("records=%+v err=%v", records, err)
	}
}

func TestDeployProjectionNonSuccessBeforeMerge(t *testing.T) {
	s := schemaStore(t, "deploy_projection_refusals")
	for i, result := range []string{"failed", "rolled_back"} {
		pr := testPR(9100 + i)
		key := fmt.Sprintf("deploy/svc/20261105T%02d0000Z", i+3)
		putDeploy(t, s, key, "svc", result, []string{pr})
		task := flowTask(t, s, s.pool, "implement", StateReviewing, true)
		if res := send(t, s, mergeEvent(task.ID, pr)); res.State != StateMerged {
			t.Fatalf("%s: %+v", result, res)
		}
	}
}
