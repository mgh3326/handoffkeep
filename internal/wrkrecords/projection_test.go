package wrkrecords_test

import (
	"encoding/base64"
	"reflect"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	. "github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

func shadowDB(t *testing.T) contractDB {
	t.Helper()
	db := contractStore(t)
	v := activeFixture(t)
	v.Given.Records = v.Given.Records[:1]
	db.seed(t, v, contentCatalog(t, loadVectors(t)))
	return db
}

func completionBytes() Object {
	b := []byte("canonical completion report\n")
	return Object{"report_b64": base64.StdEncoding.EncodeToString(b), "report_sha256": SHA(b)}
}

func requireProjection(t *testing.T, rows []store.RelayEvent, rec Record) store.RelayEvent {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("projection count %d", len(rows))
	}
	x := rows[0]
	receipt := rec.Receipt()
	if x.JobID != rec.JobID || x.Kind != "job."+rec.Kind || x.Head != Text(rec.Payload, "head") || x.PR != Text(rec.Payload, "pr") || x.ReportSHA != Text(rec.Payload, "report_sha256") || !reflect.DeepEqual(x.WrkReceipt, &receipt) {
		t.Fatalf("projection differs from canonical receipt: %+v", x)
	}
	return x
}

func TestWrkPlantedProjectionReplacedAndHydrated(t *testing.T) {
	db := shadowDB(t)
	ctx := store.WithWrkPrincipal(t.Context(), "desktop", nil)
	at := time.Now().Add(-time.Hour)
	planted, created, e := db.runtime.AppendRelayEvent(ctx, store.RelayEvent{Kind: "job.completed", JobID: "job-a", OwnerLane: "planted-lane", ReportPath: "wrk:job-a:3", Head: h1, PR: "https://example.invalid/planted", ReportSHA: SHA([]byte("planted report")), Machine: "planted-host", PaneID: "planted-pane", ReportLastLine: "planted line", Question: "planted question", EventID: "planted-event", EventTime: &at})
	if e != nil || !created || planted.WrkReceipt != nil || planted.ReportSHA != "" {
		t.Fatalf("shadow planting setup: %v created=%v", e, created)
	}
	if _, e = db.runtime.MarkRelayEventDelivered(t.Context(), planted.ID, "planted", "pane"); e != nil {
		t.Fatal(e)
	}
	appendDB(t, db, "director-1", "job-a", "activate", Object{"seal_seq": nil, "reason_code": "enforce"})
	completed := appendDB(t, db, "desktop", "job-a", "completed", completionBytes())
	if completed.Seq != 3 {
		t.Fatal("plant did not target the canonical key")
	}
	rows, e := db.runtime.ListRelayEvents(t.Context(), "b2400-a", true, 10)
	if e != nil {
		t.Fatal(e)
	}
	x := requireProjection(t, rows, completed)
	if x.ID != planted.ID || x.OwnerLane != "b2400-a" || x.ReportPath != "wrk:job-a:3" || x.DeliveredAt != nil || x.DeliveredTo != "" || x.Attempts != 0 || x.Machine != "" || x.PaneID != "" || x.ReportLastLine != "" || x.Question != "" || x.EventID != "" || x.EventTime != nil {
		t.Fatal("canonical upsert retained planted metadata or delivery")
	}
	rows, e = db.runtime.ListRelayEventsPage(t.Context(), "", "job.completed", false, 0, 10)
	if e != nil {
		t.Fatal(e)
	}
	requireProjection(t, rows, completed)
	rows, e = db.runtime.ListRelayEventsTimeline(t.Context(), "", "job.completed", nil, nil, 0, 10)
	if e != nil {
		t.Fatal(e)
	}
	requireProjection(t, rows, completed)
	for range 2 { // First delivery and the idempotent delivery read both hydrate.
		marked, e := db.runtime.MarkRelayEventDelivered(t.Context(), x.ID, "fixture", "pane")
		if e != nil {
			t.Fatal(e)
		}
		requireProjection(t, []store.RelayEvent{marked}, completed)
	}
	receipt := completed.Receipt()
	duplicate, created, e := db.runtime.AppendRelayEvent(ctx, store.RelayEvent{Kind: "job.completed", JobID: "job-a", ReportSHA: Text(completed.Payload, "report_sha256"), WrkReceipt: &receipt})
	if e != nil || created {
		t.Fatalf("protected legacy retrieval: %v created=%v", e, created)
	}
	requireProjection(t, []store.RelayEvent{duplicate}, completed)

	// A malformed or missing canonical target never hides the notification and
	// never carries a receipt, regardless of its delivered status.
	for _, tc := range []struct{ field, bad, good string }{
		{"owner_lane", "wrong", "b2400-a"}, {"head", h1, ""}, {"pr", "https://example.invalid/wrong", ""},
		{"job_id", "wrong-job", "job-a"}, {"kind", "job.joined", "job.completed"},
		{"report_path", "wrk:job-a:999", "wrk:job-a:3"}, {"report_path", "wrk:job-a:03", "wrk:job-a:3"},
		{"reason", "wrong", ""},
	} {
		t.Run(tc.field+"/"+tc.bad, func(t *testing.T) {
			if _, e := db.owner.Exec(t.Context(), `UPDATE relay_events SET `+tc.field+`=$1 WHERE id=$2`, tc.bad, x.ID); e != nil {
				t.Fatal(e)
			}
			rows, e := db.runtime.ListRelayEvents(t.Context(), "", false, 10)
			if e != nil || len(rows) != 1 || rows[0].WrkReceipt != nil || rows[0].ReportSHA != "" {
				t.Fatal("mismatching projection did not fail closed while preserving row")
			}
			if _, e := db.owner.Exec(t.Context(), `UPDATE relay_events SET `+tc.field+`=$1 WHERE id=$2`, tc.good, x.ID); e != nil {
				t.Fatal(e)
			}
		})
	}
	if _, e = db.owner.Exec(t.Context(), `UPDATE wrk_contents SET content_sha256=$1 WHERE job_id='job-a' AND seq=$2`, SHA([]byte("wrong digest")), completed.Seq); e != nil {
		t.Fatal(e)
	}
	rows, e = db.runtime.ListRelayEvents(t.Context(), "", false, 10)
	if e != nil || len(rows) != 1 || rows[0].WrkReceipt != nil || rows[0].ReportSHA != "" {
		t.Fatal("report digest mismatch gained a receipt or hid its row")
	}
}

func TestWrkShadowObservationNeverBecomesCompletionReceipt(t *testing.T) {
	db := shadowDB(t)
	appendDB(t, db, "director-1", "reviewer", "claim", Object{"job_id": "reviewer", "owner_lane": "b2400-a", "parent_lane": "b2400-a", "task_id": 2400, "host_client_id": "desktop", "role": "tester", "tier": "T2", "review_of": Object{"job_id": "job-a", "round_no": 1}, "predecessor_job_id": nil, "supersedes": nil})
	old := appendDB(t, db, "desktop", "job-a", "completed", completionBytes())
	reviewer := appendDB(t, db, "desktop", "reviewer", "completed", completionBytes())
	rows, e := db.runtime.ListRelayEvents(t.Context(), "", false, 10)
	if e != nil {
		t.Fatal(e)
	}
	requireProjection(t, rows, reviewer) // P1 reviewer completion is already canonical.
	ctx := store.WithWrkPrincipal(t.Context(), "desktop", nil)
	planted, _, e := db.runtime.AppendRelayEvent(ctx, store.RelayEvent{Kind: "job.completed", JobID: "job-a", OwnerLane: "b2400-a", ReportPath: "wrk:job-a:2"})
	if e != nil || planted.WrkReceipt != nil {
		t.Fatal("ordinary shadow observation became an authority projection")
	}
	begin := appendDB(t, db, "director-1", "job-a", "import_begin", Object{"reason_code": "freeze"})
	manifest, _ := Digest([]Object{})
	seal := appendDB(t, db, "director-1", "job-a", "import_seal", Object{"begin_seq": begin.Seq, "expected_import_count": 0, "manifest_sha256": manifest, "reason_code": "seal"})
	appendDB(t, db, "director-1", "job-a", "activate", Object{"seal_seq": seal.Seq, "reason_code": "enforce"})
	receipt := old.Receipt()
	if _, _, e = db.runtime.AppendRelayEvent(ctx, store.RelayEvent{Kind: "job.completed", JobID: "job-a", ReportSHA: Text(old.Payload, "report_sha256"), WrkReceipt: &receipt}); Code(e) != "wrk_receipt_required" {
		t.Fatalf("preactivation completion used after activate: %v", e)
	}
	rows, e = db.runtime.ListRelayEvents(t.Context(), "", false, 10)
	if e != nil || len(rows) != 2 {
		t.Fatal("phase refusal hid or created a projection")
	}
	for _, x := range rows {
		if x.JobID == "job-a" && (x.WrkReceipt != nil || x.ReportSHA != "") {
			t.Fatal("planted observation acquired a receipt at activation")
		}
		if x.JobID == "reviewer" {
			requireProjection(t, []store.RelayEvent{x}, reviewer)
		}
	}
	current := appendDB(t, db, "desktop", "job-a", "completed", completionBytes())
	receipt = current.Receipt()
	x, _, e := db.runtime.AppendRelayEvent(ctx, store.RelayEvent{Kind: "job.completed", JobID: "job-a", ReportSHA: Text(current.Payload, "report_sha256"), WrkReceipt: &receipt})
	if e != nil {
		t.Fatal(e)
	}
	requireProjection(t, []store.RelayEvent{x}, current)
}
