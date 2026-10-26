package wrkrecords_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	. "github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

// Binding director-1 esc-2 #19 / post-M1r ruling: legacy bodies are schematic.
// These named adapters supply today's route fields, never a production rule
// that recognises schematic input. Pack expectations remain unchanged.
func legacyVectorRequest(t *testing.T, v vector) (string, []byte) {
	t.Helper()
	path := Text(v.Request, "path")
	body := Clone(Obj(v.Request, "body"))
	mapping := ""
	switch v.file + "/" + v.Name {
	case "legacy-route-fence.json/tasks-create-with-o3-refs-denied", "legacy-route-fence.json/tasks-create-unrelated-legacy-compatible":
		body["lane"], body["kind"], body["project"] = "b2400-a", "implement", "experiment"
		mapping = "real task creation fields; preserve supplied refs"
	case "legacy-route-fence.json/tasks-next-claim-protected-denied":
		body = Object{"lane": "b2400-a", "claimed_by": "director-1", "job_id": "job-a"}
		mapping = "real lane plus explicit protected job_id (S1 #14)"
	case "legacy-route-fence.json/tasks-claim-protected-denied":
		body = Object{"claimed_by": "director-1", "job_id": "job-z"}
		mapping = "claimed_by and job_id fields; authenticated actor remains desktop"
	case "legacy-route-fence.json/tasks-rebind-protected-denied":
		body = Object{"from_job": "job-a", "to_job": "job-z", "by": "director-1", "reason": "fixture rebind"}
		mapping = "real from_job/to_job/by/reason"
	case "legacy-route-fence.json/tasks-transition-binding-change-denied", "legacy-route-fence.json/tasks-transition-state-only-without-receipt-denied":
		mapping = "existing transition fields, unchanged"
	case "legacy-route-fence.json/tasks-project-or-relane-protected-denied":
		path = "/v1/tasks/2400/project"
		body = Object{"project": "experiment", "note": "fixture project attempt"}
		mapping = "project branch of the project-or-relane row, real project route"
	case "legacy-route-fence.json/decision-request-resolve-protected-denied":
		body = Object{"request_id": "dr-2400-1", "kind": "answered", "option": "A"}
		mapping = "real structured decision resolve fields"
	case "legacy-route-fence.json/dispositions-apply-binding-denied":
		if len(v.Given.Legacy.Dispositions) != 1 {
			t.Fatal("M1r2 disposition prerequisite missing")
		}
		path = "/v1/tasks/dispositions/" + strconv.FormatInt(v.Given.Legacy.Dispositions[0].TaskID, 10) + "/apply"
		body = Object{"note": "apply fixture disposition"}
		mapping = "real disposition on protected subject task 2400; real note field"
	case "legacy-route-fence.json/decisions-resolve-protected-denied":
		body = Object{"type": "task", "id": 2400, "by": "director-1", "answer": "A"}
		mapping = "real generic decision resolve type/id/by/answer"
	case "transition.json/legacy-task-no-job-bypass-denied":
		body["no_job"] = "fixture no-job exception"
		mapping = "real string no_job reason"
	case "r4-review-lifecycle.json/r-end-receipt-cannot-drop-subject-task":
		found := false
		for _, r := range v.Given.Records {
			if r.JobID == "job-a-t1" && r.Kind == "end" {
				body["wrk_receipt"] = r.Receipt()
				found = true
				break
			}
		}
		if !found {
			t.Fatal("wrong-subject end receipt fixture missing")
		}
		delete(body, "receipt")
		mapping = "full four-field wrk_receipt from the given reviewer end; subject task stays 2400"
	case "failure-abuse.json/deleted-local-events-canonical-unchanged", "failure-abuse.json/legacy-joined-direct-or-task-relink-fence", "legacy-route-fence.json/relay-terminal-without-receipt-denied", "transition.json/legacy-relay-completed-no-receipt", "r4-review-lifecycle.json/r-legacy-receiptless-terminal-relay":
		body["owner_lane"], body["report_path"] = "b2400-a", "/synthetic/report.md"
		mapping = "real relay owner_lane/report_path; no receipt added"
	default:
		t.Fatalf("unmapped schematic legacy vector: %s/%s", v.file, v.Name)
	}
	t.Logf("legacy_mapping %s/%s: %s", v.file, v.Name, mapping)
	raw, e := json.Marshal(body)
	if e != nil {
		t.Fatal(e)
	}
	return path, raw
}
func (db contractDB) seedLegacyMapping(t *testing.T, v vector) {
	t.Helper()
	if v.file+"/"+v.Name != "legacy-route-fence.json/dispositions-apply-binding-denied" {
		return
	}
	if len(v.Given.Legacy.Dispositions) != 1 || v.Given.Legacy.Dispositions[0].ID != 77 || v.Given.Legacy.Dispositions[0].TaskID != 2400 {
		t.Fatal("M1r2 logical disposition mapping changed")
	}
	disposition := v.Given.Legacy.Dispositions[0]
	at := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	options := append([]store.DecisionOption{}, store.DispositionOptionLabels...)
	options[0].Recommended = true
	refs := store.TaskRefs{JobID: "job-a", OriginPR: "https://github.com/example/repo/pull/1", DecisionOptions: &store.DecisionOptions{Options: options}, Disposition: &store.Disposition{Schema: store.DispositionSchema, Recommended: "A", Facts: store.DispositionFacts{MergeSHA: strings.Repeat("a", 40), MergedAt: &at, FactsSource: "gh-pr-view", FactsAsOf: at, Install: store.DispositionInstall{State: "not_applicable"}}, Answer: &store.DispositionAnswer{Key: "A", By: "operator:fixture@example.com", EventID: "fixture-disposition-answer", Gen: 1, At: at}}}
	raw, e := json.Marshal(refs)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.owner.Exec(t.Context(), `UPDATE tasks SET kind='decide',state=$2,refs=$1::jsonb WHERE id=$3`, string(raw), disposition.State, disposition.TaskID); e != nil {
		t.Fatal(e)
	}
}

// M1r2 repaired all four fixtures. No setup bypass remains: every policy graph
// is validated after its given records are inserted, exactly as normal startup.
var fixture_incomplete = map[string]bool{}

func TestFixtureIncompletePermissionCases(t *testing.T) {
	if len(fixture_incomplete) != 0 {
		t.Fatal("M1r2 fixture_incomplete must stay empty")
	}
	repaired := map[string]bool{
		"r4-review-lifecycle.json/r-import-begin-scope-denied":               true,
		"r4-review-lifecycle.json/r-import-scope-denied":                     true,
		"r4-review-lifecycle.json/r-import-seal-scope-denied":                true,
		"r4-scoped-claim.json/scoped-mint-reviewer-as-target-claim-conflict": true,
	}
	seen := map[string]bool{}
	for _, v := range loadVectors(t) {
		key := v.file + "/" + v.Name
		if !repaired[key] {
			continue
		}
		seen[key] = true
		t.Run(v.Name, func(t *testing.T) {
			snap, p := seedVector(t, v, contentCatalog(t, loadVectors(t)))
			if e := p.ValidateGraph(snap, nil); e != nil {
				t.Fatalf("repaired fixture failed normal startup graph validation: %v", e)
			}
		})
	}
	if len(seen) != len(repaired) {
		t.Fatal("missing repaired fixture")
	}
}
