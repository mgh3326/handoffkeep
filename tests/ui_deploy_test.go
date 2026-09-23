package tests

// #529 — /ui/api/deploy-pending: per-service last deploy record + tasks merged
// after it. Fixture bodies are byte-for-byte the deploy-record/v0 shape of the
// real backfill records in production hk (deploy/handoffkeep/20260923T033049Z
// id 2646 · deploy/panewire-hub/20260923T033051Z id 2647 ·
// deploy/auto_trader/20260923T033052Z id 2648) — only the service/key/ref
// values needed by each case are changed.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/store"
)

type deployRecordFixture struct {
	Schema      string  `json:"schema"`
	Target      string  `json:"target"`
	HeadSHA     *string `json:"head_sha"`
	PreviousRef *string `json:"previous_ref"`
	Result      string  `json:"result"`
	FailedStep  *int    `json:"failed_step"`
	JobID       *string `json:"job_id"`
	ApprovalRef *string `json:"approval_ref"`
	IncludedPRs any     `json:"included_prs"`
	Source      string  `json:"source"`
	Service     string  `json:"service"`
	DeployedRef *string `json:"deployed_ref"`
	DeployedAt  *string `json:"deployed_at"`
	Evidence    any     `json:"evidence"`
}

func deployFixtureBody(t *testing.T, service, result, deployedRef, deployedAt string, failedStep *int) string {
	t.Helper()
	rec := deployRecordFixture{
		Schema:     "deploy-record/v0",
		Target:     "ncp",
		Result:     result,
		FailedStep: failedStep,
		Source:     "backfill",
		Service:    service,
		Evidence:   map[string]string{"deployed_ref": "test fixture — real shape, synthetic values"},
	}
	if deployedRef != "" {
		rec.DeployedRef = &deployedRef
	}
	if deployedAt != "" {
		rec.DeployedAt = &deployedAt
	}
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func seedDeployDoc(t *testing.T, s *store.Store, key, body string) {
	t.Helper()
	if _, _, err := s.PutDocument(t.Context(), store.Document{Key: key, Kind: "report", Session: "deploy-test", Body: body, CreatedBy: "deploy-test"}); err != nil {
		t.Fatal(err)
	}
}

// deployDB opens a direct connection for the two things the store API does
// not expose on purpose: wiping deploy/ fixtures and inserting a merged
// task_event at a controlled timestamp with a controlled refs snapshot.
func deployDB(t *testing.T) *pgx.Conn {
	t.Helper()
	db, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })
	return db
}

func wipeDeployDocs(t *testing.T, db *pgx.Conn) {
	t.Helper()
	if _, err := db.Exec(t.Context(), `DELETE FROM documents WHERE key LIKE 'deploy/%'`); err != nil {
		t.Fatal(err)
	}
}

func seedMergedEvent(t *testing.T, db *pgx.Conn, s *store.Store, title, pr string, at time.Time) int64 {
	t.Helper()
	task := createUITask(t, s, uiLane(t, "deploy"), title)
	var refs []byte
	var err error
	if pr != "" {
		refs, err = json.Marshal(map[string]string{"pr": pr})
	} else {
		refs, err = json.Marshal(map[string]string{})
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `INSERT INTO task_events(task_id,"from","to","by",note,refs,at) VALUES($1,'join','merged','deploy-test','',$2::jsonb,$3)`, task.ID, string(refs), at); err != nil {
		t.Fatal(err)
	}
	return task.ID
}

func getDeployPending(t *testing.T, h *httptest.Server, assertion string) map[string]any {
	t.Helper()
	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/deploy-pending", assertion, "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("deploy-pending status=%d body=%s", response.StatusCode, responseText(t, response))
	}
	var out map[string]any
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func serviceView(t *testing.T, response map[string]any, service string) map[string]any {
	t.Helper()
	services, ok := response["services"].([]any)
	if !ok {
		t.Fatalf("services missing: %v", response)
	}
	for _, raw := range services {
		svc := raw.(map[string]any)
		if svc["service"] == service {
			return svc
		}
	}
	t.Fatalf("service %s not in response", service)
	return nil
}

func mergedIDs(svc map[string]any) []float64 {
	out := []float64{}
	for _, raw := range svc["merged_since"].([]any) {
		out = append(out, raw.(map[string]any)["task_id"].(float64))
	}
	return out
}

func TestUIDeployPendingServiceLines(t *testing.T) {
	s := uiStore(t)
	db := deployDB(t)
	wipeDeployDocs(t, db)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	// The boundary is derived from now() so each run's events rank newest in
	// the DESC scan window — a fixed date decays once the shared test DB
	// accumulates more merged events than the handler's scan limit.
	deployedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339)
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T033049Z", deployFixtureBody(t, "handoffkeep", "success", "92ee9e2d0ddc47593677adcc9e83040dd9873463", deployedAt, nil))
	// auto_trader: the production backfill has deployed_at=null — the merged
	// boundary must report "unrecorded", never fall back to record time.
	seedDeployDoc(t, s, "deploy/auto_trader/20260923T033052Z", deployFixtureBody(t, "auto_trader", "success", "sha256:e0580273a4c31cd56a1028389212a1e101c40f130231f6dd2c49b3b5bc0214f6", "", nil))

	base, err := time.Parse(time.RFC3339, deployedAt)
	if err != nil {
		t.Fatal(err)
	}
	// Boundary: strictly after deployed_at. Same-second and earlier merges
	// belong to the deployed build and must not appear. task_events is
	// append-only, so earlier runs' merges legitimately coexist — assert the
	// boundary ids by membership, not set equality.
	included := seedMergedEvent(t, db, s, "post-deploy merge", "https://github.com/mgh3326/handoffkeep/pull/38", base.Add(time.Hour))
	excluded := []int64{
		seedMergedEvent(t, db, s, "same-second merge", "https://github.com/mgh3326/handoffkeep/pull/39", base),
		seedMergedEvent(t, db, s, "pre-deploy merge", "https://github.com/mgh3326/handoffkeep/pull/40", base.Add(-time.Hour)),
		// Repo attribution is exact: a repo whose name only contains the
		// service repo, a different owner, and a non-URL pr all stay
		// unattributed.
		seedMergedEvent(t, db, s, "repo prefix", "https://github.com/mgh3326/handoffkeep2/pull/1", base.Add(2*time.Hour)),
		seedMergedEvent(t, db, s, "other owner", "https://github.com/other/handoffkeep/pull/9", base.Add(2*time.Hour)),
		seedMergedEvent(t, db, s, "non-url pr", "handoffkeep#41", base.Add(2*time.Hour)),
		seedMergedEvent(t, db, s, "no pr", "", base.Add(2*time.Hour)),
	}
	otherRepo := seedMergedEvent(t, db, s, "auto_trader merge", "https://github.com/mgh3326/auto_trader/pull/7", base.Add(2*time.Hour))

	response := getDeployPending(t, h, assertion)

	hk := serviceView(t, response, "handoffkeep")
	current := hk["current"].(map[string]any)
	if current["deployed_ref"] != "92ee9e2d0ddc47593677adcc9e83040dd9873463" || current["deployed_at"] != deployedAt || current["result"] != "success" {
		t.Fatalf("handoffkeep current=%v", current)
	}
	if hk["latest"] != nil {
		t.Fatalf("handoffkeep latest should be absent when it equals current: %v", hk["latest"])
	}
	if hk["merged_boundary"] != "deployed_at" {
		t.Fatalf("handoffkeep merged_boundary=%v", hk["merged_boundary"])
	}
	ids := mergedIDs(hk)
	found := false
	for _, id := range ids {
		if id == float64(included) {
			found = true
		}
	}
	if !found {
		t.Fatalf("handoffkeep merged_since ids=%v missing %d", ids, included)
	}
	for _, id := range excluded {
		for _, got := range ids {
			if got == float64(id) {
				t.Fatalf("handoffkeep merged_since contains excluded task %d: %v", id, ids)
			}
		}
	}

	at := serviceView(t, response, "auto_trader")
	if at["merged_boundary"] != "unrecorded" || len(mergedIDs(at)) != 0 {
		t.Fatalf("auto_trader boundary=%v merged=%v — must not list task %d without a deployed_at boundary", at["merged_boundary"], mergedIDs(at), otherRepo)
	}

	pw := serviceView(t, response, "panewire-hub")
	if pw["record_count"] != float64(0) || pw["current"] != nil || pw["merged_boundary"] != "no_current" {
		t.Fatalf("panewire-hub should be record-less: %v", pw)
	}
	if response["pr_source"] != "refs.pr" {
		t.Fatalf("pr_source=%v", response["pr_source"])
	}
}

func TestUIDeployPendingFailedAndRolledBack(t *testing.T) {
	s := uiStore(t)
	db := deployDB(t)
	wipeDeployDocs(t, db)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	// handoffkeep: an older success plus a newer failed attempt that stopped
	// after the deploy step — current stays the success, latest carries the
	// failed record and the serving-maybe-changed caveat.
	seedDeployDoc(t, s, "deploy/handoffkeep/20260920T010000Z", deployFixtureBody(t, "handoffkeep", "success", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2026-09-20T01:00:00Z", nil))
	step := 7
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T020000Z", deployFixtureBody(t, "handoffkeep", "failed", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "2026-09-23T02:00:00Z", &step))
	// auto_trader: rolled_back latest — current stays the prior success.
	seedDeployDoc(t, s, "deploy/auto_trader/20260920T010000Z", deployFixtureBody(t, "auto_trader", "success", "sha256:0000000000000000000000000000000000000000000000000000000000000000", "2026-09-20T01:00:00Z", nil))
	seedDeployDoc(t, s, "deploy/auto_trader/20260923T030000Z", deployFixtureBody(t, "auto_trader", "rolled_back", "", "2026-09-23T03:00:00Z", &step))
	// panewire-hub: only a failed record — no success means no current and no
	// merged list, but the failure itself is still visible.
	seedDeployDoc(t, s, "deploy/panewire-hub/20260923T040000Z", deployFixtureBody(t, "panewire-hub", "failed", "", "2026-09-23T04:00:00Z", nil))

	response := getDeployPending(t, h, assertion)

	hk := serviceView(t, response, "handoffkeep")
	if hk["current"].(map[string]any)["deployed_ref"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("current must stay the last success: %v", hk["current"])
	}
	latest := hk["latest"].(map[string]any)
	if latest["result"] != "failed" || latest["serving_maybe_changed"] != true {
		t.Fatalf("handoffkeep latest=%v", latest)
	}

	at := serviceView(t, response, "auto_trader")
	if at["latest"].(map[string]any)["result"] != "rolled_back" {
		t.Fatalf("auto_trader latest=%v", at["latest"])
	}
	if at["current"].(map[string]any)["result"] != "success" {
		t.Fatalf("auto_trader current=%v", at["current"])
	}

	pw := serviceView(t, response, "panewire-hub")
	if pw["current"] != nil || pw["latest"].(map[string]any)["result"] != "failed" || pw["merged_boundary"] != "no_current" {
		t.Fatalf("panewire-hub failed-only view wrong: %v", pw)
	}
}

func TestUIDeployPendingInvalidRecords(t *testing.T) {
	s := uiStore(t)
	db := deployDB(t)
	wipeDeployDocs(t, db)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	// Malformed body, wrong schema, and a service/key mismatch all count as
	// invalid — never silently parsed into a deploy line.
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T010000Z", `{"schema":"deploy-record/v0","service":"auto_trader","result":"success"}`)
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T020000Z", `{"schema":"other/v9","service":"handoffkeep","result":"success"}`)
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T030000Z", `not json`)
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T040000Z", deployFixtureBody(t, "handoffkeep", "success", "cccccccccccccccccccccccccccccccccccccccc", "2026-09-23T04:00:00Z", nil))

	response := getDeployPending(t, h, assertion)
	hk := serviceView(t, response, "handoffkeep")
	if hk["record_count"] != float64(4) || hk["invalid_count"] != float64(3) {
		t.Fatalf("record/invalid counts: %v", hk)
	}
	if hk["current"].(map[string]any)["deployed_ref"] != "cccccccccccccccccccccccccccccccccccccccc" {
		t.Fatalf("current=%v", hk["current"])
	}
}

func TestUIDeployPendingAuthBoundary(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/deploy-pending", "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.StatusCode)
	}
	response.Body.Close()
	// The endpoint is GET-only: an authenticated POST never reaches it.
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	response = uiRequest(t, h.Client(), http.MethodPost, h.URL+"/ui/api/deploy-pending", assertion, "")
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", response.StatusCode)
	}
	response.Body.Close()

	// An allowlisted service principal is authenticated but not an operator —
	// the deploy view is document-derived reading material, same 403 as
	// /ui/api/board/doc.
	service := newP3UITestServer(t, s, fixture, "", "", []string{"glance-fixture"})
	defer service.Close()
	serviceAssertion := p3ServiceAssertion(t, fixture)
	response = p3Request(t, service.Client(), http.MethodGet, service.URL+"/ui/api/deploy-pending", serviceAssertion, nil, "")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("service principal status=%d", response.StatusCode)
	}
	response.Body.Close()
}
