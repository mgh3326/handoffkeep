package wrkrecords_test

import (
	"encoding/json"
	"reflect"
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
		path = "/v1/tasks/dispositions/2400/apply"
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
	at := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	options := append([]store.DecisionOption{}, store.DispositionOptionLabels...)
	options[0].Recommended = true
	refs := store.TaskRefs{JobID: "job-a", OriginPR: "https://github.com/example/repo/pull/1", DecisionOptions: &store.DecisionOptions{Options: options}, Disposition: &store.Disposition{Schema: store.DispositionSchema, Recommended: "A", Facts: store.DispositionFacts{MergeSHA: strings.Repeat("a", 40), MergedAt: &at, FactsSource: "gh-pr-view", FactsAsOf: at, Install: store.DispositionInstall{State: "not_applicable"}}, Answer: &store.DispositionAnswer{Key: "A", By: "operator:fixture@example.com", EventID: "fixture-disposition-answer", Gen: 1, At: at}}}
	raw, e := json.Marshal(refs)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.owner.Exec(t.Context(), `UPDATE tasks SET kind='decide',state='claimed',refs=$1::jsonb WHERE id=2400`, string(raw)); e != nil {
		t.Fatal(e)
	}
}

// Only these four incomplete M1 histories may bypass startup graph validation
// in test setup. Every request still executes the actual API and asserts the
// pack's static refusal. The set must become empty after M1 adds its references.
var fixture_incomplete = map[string]bool{
	"r4-scoped-claim.json/scoped-mint-reviewer-as-target-claim-conflict": true,
	"r4-review-lifecycle.json/r-import-begin-scope-denied":               true,
	"r4-review-lifecycle.json/r-import-scope-denied":                     true,
	"r4-review-lifecycle.json/r-import-seal-scope-denied":                true,
}

func TestFixtureIncompletePermissionCases(t *testing.T) {
	want := map[string]bool{
		"r4-review-lifecycle.json/r-import-begin-scope-denied":               true,
		"r4-review-lifecycle.json/r-import-scope-denied":                     true,
		"r4-review-lifecycle.json/r-import-seal-scope-denied":                true,
		"r4-scoped-claim.json/scoped-mint-reviewer-as-target-claim-conflict": true,
	}
	if !reflect.DeepEqual(fixture_incomplete, want) {
		t.Fatal("fixture_incomplete must be exactly the four director-ruled cases until M1 repairs them")
	}
	seen := map[string]bool{}
	for _, v := range loadVectors(t) {
		key := v.file + "/" + v.Name
		if !fixture_incomplete[key] {
			continue
		}
		seen[key] = true
		t.Run(v.Name, func(t *testing.T) {
			snap, p := seedVector(t, v, contentCatalog(t, loadVectors(t)))
			if v.file == "r4-scoped-claim.json" {
				target := snap.Claim(Text(Obj(Obj(Obj(v.Request, "body"), "payload"), "review_of"), "job_id"))
				if !Review(target) || Num(v.Expect, "http_status") != 409 || Text(v.Expect, "error") != "claim_conflict" {
					t.Fatal("review-role target static refusal changed")
				}
				root := snap.Claim(Text(Obj(target.Payload, "review_of"), "job_id"))
				if root == nil || root.ReceivedAt <= target.ReceivedAt {
					t.Fatal("M1 repaired review-before-subject history; empty fixture_incomplete entry")
				}
				return
			}
			claim := snap.Claim(v.Given.Job)
			if !Review(claim) || snap.Claim(Text(Obj(claim.Payload, "review_of"), "job_id")) != nil {
				t.Fatal("M1 repaired subject reference; empty fixture_incomplete entry")
			}
			var wire Object
			_ = json.Unmarshal(v.Given.Policy, &wire)
			if _, ok := Obj(wire, "lanes")[Text(claim.Payload, "owner_lane")]; ok {
				t.Fatal("M1 repaired lane reference; empty fixture_incomplete entry")
			}
			if Num(v.Expect, "http_status") != 403 || Text(v.Expect, "error") != "scope_denied" {
				t.Fatal("incomplete lifecycle exemption is permission-only")
			}
			permission := map[string]string{"import_begin": "import.begin", "import": "import.write", "import_seal": "import.seal"}[Text(Obj(v.Request, "body"), "kind")]
			if permission == "" || p.Allows(Text(v.Request, "actor"), permission) {
				t.Fatal("request can reach target lookup; no incomplete fixture exemption")
			}
		})
	}
	if len(seen) != len(fixture_incomplete) {
		t.Fatal("stale/unknown fixture_incomplete entry")
	}
}
