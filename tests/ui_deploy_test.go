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
	"fmt"
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

// seedMergedEventsBulk inserts n merged events on one task in a single
// statement — used to fill the handler's scan window without n store calls.
func seedMergedEventsBulk(t *testing.T, db *pgx.Conn, s *store.Store, n int, at time.Time) {
	t.Helper()
	task := createUITask(t, s, uiLane(t, "deploy"), "bulk merged")
	if _, err := db.Exec(t.Context(),
		`INSERT INTO task_events(task_id,"from","to","by",note,refs,at)
		 SELECT $1,'join','merged','deploy-test','','{}'::jsonb,$2 FROM generate_series(1,$3)`,
		task.ID, at, n); err != nil {
		t.Fatal(err)
	}
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

// Regression (tester BLOCKER): "latest attempt" is selected by contract time
// (deployed_at, key-time fallback), not document key order — a failed attempt
// whose deployed_at postdates the last success must surface even when its key
// sorts earlier.
func TestUIDeployPendingLatestByContractTime(t *testing.T) {
	s := uiStore(t)
	db := deployDB(t)
	wipeDeployDocs(t, db)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	// Later key, earlier deployed_at: the success.
	seedDeployDoc(t, s, "deploy/handoffkeep/20260924T000000Z", deployFixtureBody(t, "handoffkeep", "success", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2026-09-20T00:00:00Z", nil))
	// Earlier key, later deployed_at: the more recent attempt, failed.
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T000000Z", deployFixtureBody(t, "handoffkeep", "failed", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "2026-09-25T00:00:00Z", nil))

	response := getDeployPending(t, h, assertion)
	hk := serviceView(t, response, "handoffkeep")
	if hk["current"].(map[string]any)["deployed_ref"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("current=%v", hk["current"])
	}
	latest, ok := hk["latest"].(map[string]any)
	if !ok || latest["result"] != "failed" || latest["record_key"] != "deploy/handoffkeep/20260923T000000Z" {
		t.Fatalf("newer failed attempt by deployed_at must surface as latest: %v", hk["latest"])
	}
}

// The first scan window is bounded, but a full window that holds no success
// pages deeper rather than asserting "no success" on a partial scan. 201
// failed records — the exact tester counterexample — must exhaust the space
// and clear the cap flag, since nothing was left unscanned.
func TestUIDeployPendingDocsCapped(t *testing.T) {
	s := uiStore(t)
	db := deployDB(t)
	wipeDeployDocs(t, db)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	for i := 0; i < 201; i++ {
		seedDeployDoc(t, s, "deploy/panewire-hub/20260102T"+time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Second).Format("150405")+"Z",
			deployFixtureBody(t, "panewire-hub", "failed", "", "2026-01-02T00:00:00Z", nil))
	}
	response := getDeployPending(t, h, assertion)
	pw := serviceView(t, response, "panewire-hub")
	if pw["docs_capped"] == true || pw["record_count"] != float64(201) || pw["current"] != nil {
		t.Fatalf("exhausted scan must clear docs_capped and report honest no-success: %v", pw)
	}
}

// Regression (tester SHOULD): the last success may sit beyond the first
// window — a full page of failed attempts must not hide it. The handler
// pages deeper and the oldest success becomes current.
func TestUIDeployPendingDocsDeepScanFindsOlderSuccess(t *testing.T) {
	s := uiStore(t)
	db := deployDB(t)
	wipeDeployDocs(t, db)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	// Oldest key = the only success; the 200 newer keys are all failed.
	seedDeployDoc(t, s, "deploy/handoffkeep/20260102T000000Z",
		deployFixtureBody(t, "handoffkeep", "success", "cccccccccccccccccccccccccccccccccccccccc", "2026-01-02T00:00:00Z", nil))
	for i := 1; i <= 200; i++ {
		seedDeployDoc(t, s, "deploy/handoffkeep/20260102T"+time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Second).Format("150405")+"Z",
			deployFixtureBody(t, "handoffkeep", "failed", "", "2026-01-02T00:00:00Z", nil))
	}
	response := getDeployPending(t, h, assertion)
	hk := serviceView(t, response, "handoffkeep")
	current, ok := hk["current"].(map[string]any)
	if !ok || current["deployed_ref"] != "cccccccccccccccccccccccccccccccccccccccc" || current["result"] != "success" {
		t.Fatalf("older success beyond the window must surface as current: %v", hk["current"])
	}
	if hk["docs_capped"] == true || hk["record_count"] != float64(201) {
		t.Fatalf("scan reached the end — docs_capped must clear: %v", hk)
	}
	if hk["latest"].(map[string]any)["result"] != "failed" {
		t.Fatalf("newer failed attempt must still show as latest: %v", hk["latest"])
	}
}

// Regression (tester BLOCKER r4): key order and contract time can reorder
// ACROSS pages — an older-key record may carry a later deployed_at. A full
// first window that already contains a success must still page deeper: the
// true current is the newest success by deployed_at over the whole scanned
// space, not the first page's success.
func TestUIDeployPendingDocsCrossPageContractTime(t *testing.T) {
	s := uiStore(t)
	db := deployDB(t)
	wipeDeployDocs(t, db)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	// Oldest key, but the contract-newest success (deployed_at 01-10).
	seedDeployDoc(t, s, "deploy/handoffkeep/20260101T000000Z",
		deployFixtureBody(t, "handoffkeep", "success", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "2026-01-10T00:00:00Z", nil))
	// Newer key, contract-older success (deployed_at 01-02) — lands in the
	// first window.
	seedDeployDoc(t, s, "deploy/handoffkeep/20260102T000000Z",
		deployFixtureBody(t, "handoffkeep", "success", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2026-01-02T00:00:00Z", nil))
	// 199 failed records fill the rest of the first window.
	for i := 1; i <= 199; i++ {
		seedDeployDoc(t, s, "deploy/handoffkeep/20260103T"+time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Second).Format("150405")+"Z",
			deployFixtureBody(t, "handoffkeep", "failed", "", "2026-01-03T00:00:00Z", nil))
	}
	response := getDeployPending(t, h, assertion)
	hk := serviceView(t, response, "handoffkeep")
	current, ok := hk["current"].(map[string]any)
	if !ok || current["deployed_ref"] != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("current must be the success with the latest deployed_at across pages: %v", hk["current"])
	}
	if hk["docs_capped"] == true || hk["record_count"] != float64(201) {
		t.Fatalf("scan reached the end — docs_capped must clear: %v", hk)
	}
}

// The deep scan is bounded by deployDocsHardCap — past it the flag stays up
// and the UI warns that older records may exist rather than claiming none.
func TestUIDeployPendingDocsHardCap(t *testing.T) {
	s := uiStore(t)
	db := deployDB(t)
	wipeDeployDocs(t, db)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	base := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2001; i++ {
		seedDeployDoc(t, s, "deploy/panewire-hub/20260102T"+base.Add(time.Duration(i)*time.Second).Format("150405")+"Z",
			deployFixtureBody(t, "panewire-hub", "failed", "", "2026-01-02T00:00:00Z", nil))
	}
	response := getDeployPending(t, h, assertion)
	pw := serviceView(t, response, "panewire-hub")
	if pw["docs_capped"] != true || pw["current"] != nil {
		t.Fatalf("hard-capped scan must keep docs_capped and not fabricate current: %v", pw)
	}
}

// Regression (tester NICE): the merged-events scan fetches limit+1 rows, so a
// fleet sitting exactly at the bound is NOT flagged truncated — only a row
// left unscanned is. task_events is append-only, so the test clears merged
// events via the replication role when the account allows it — keeping the
// exact-limit case deterministic on a shared DB; without the role it falls
// back to the count check.
func TestUIDeployPendingEventsCappedExactLimit(t *testing.T) {
	s := uiStore(t)
	db := deployDB(t)
	wipeDeployDocs(t, db)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	const limit = 1000 // must equal deployEventsLimit in internal/ui/deploys.go
	if _, err := db.Exec(t.Context(), `SET session_replication_role='replica'`); err == nil {
		if _, err := db.Exec(t.Context(), `DELETE FROM task_events WHERE "to"='merged'`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(t.Context(), `SET session_replication_role='origin'`); err != nil {
			t.Fatal(err)
		}
	}
	var total int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM task_events WHERE "to"='merged'`).Scan(&total); err != nil {
		t.Fatal(err)
	}

	if total < limit {
		// Fill to exactly the bound: nothing is left unscanned, so no service
		// may report events_capped regardless of where its boundary sits.
		seedMergedEventsBulk(t, db, s, limit-total, time.Now())
		seedDeployDoc(t, s, "deploy/handoffkeep/20260923T050000Z",
			deployFixtureBody(t, "handoffkeep", "success", "dddddddddddddddddddddddddddddddddddddddd", time.Now().Add(-time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339), nil))
		response := getDeployPending(t, h, assertion)
		if capped, ok := response["events_capped"].(bool); ok && capped {
			t.Fatalf("exactly %d merged events — nothing was truncated but events_capped is set", limit)
		}
		wipeDeployDocs(t, db)
	} else {
		t.Fatalf("exact-limit case unrunnable: %d merged events survive in shared DB and trigger wipe failed", total)
	}

	// A row beyond the bound is real truncation: when the oldest scanned
	// event is still after a service's boundary, the flag must fire.
	seedMergedEventsBulk(t, db, s, 1, time.Now())
	var oldest time.Time
	if err := db.QueryRow(t.Context(),
		`SELECT at FROM task_events WHERE "to"='merged' ORDER BY at DESC, id DESC OFFSET $1 LIMIT 1`, limit-1).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T060000Z",
		deployFixtureBody(t, "handoffkeep", "success", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", oldest.Add(-time.Second).UTC().Format(time.RFC3339), nil))
	response := getDeployPending(t, h, assertion)
	if response["events_capped"] != true {
		t.Fatalf("scan truncated and oldest scanned event is post-boundary — events_capped must be set: %v", response)
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

// #620 AC4 — invalid_count alone could never say which key to fix. The
// response now carries an "invalid" list: every scanned document that failed
// the record contract, with every failed check named. Fixture reproduces the
// 09-24 operational record deploy/handoffkeep/20260923T1456Z — the key lost
// its seconds AND deployed_at was not RFC3339, so one document carries both
// reasons. A malformed key was silently ignored before #620; it is a format
// error now so the operator can see and rewrite it.
func TestUIDeployPendingInvalidListReasons(t *testing.T) {
	s := uiStore(t)
	db := deployDB(t)
	wipeDeployDocs(t, db)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	// The incident document: key timestamp has no seconds and deployed_at is
	// not RFC3339 — both reasons on one record.
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T1456Z",
		deployFixtureBody(t, "handoffkeep", "success", "cccccccccccccccccccccccccccccccccccccccc", "2026-09-23T14:56", nil))
	// Bad key only — a fully valid body under a key this view cannot time.
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T1456",
		deployFixtureBody(t, "handoffkeep", "failed", "", "2026-09-23T05:00:00Z", nil))
	// Bad deployed_at only — the key is contract-shaped.
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T060000Z",
		deployFixtureBody(t, "handoffkeep", "success", "dddddddddddddddddddddddddddddddddddddddd", "yesterday", nil))
	// Bad body only — unparseable JSON under a good key.
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T070000Z", `not json`)
	// One fully valid record anchors the block and must not appear in the list.
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T080000Z",
		deployFixtureBody(t, "handoffkeep", "success", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "2026-09-23T08:00:00Z", nil))

	response := getDeployPending(t, h, assertion)
	hk := serviceView(t, response, "handoffkeep")
	if hk["record_count"] != float64(5) || hk["invalid_count"] != float64(4) {
		t.Fatalf("record/invalid counts: %v", hk)
	}
	invalid, ok := hk["invalid"].([]any)
	if !ok || len(invalid) != 4 {
		t.Fatalf("invalid list missing or wrong size: %v", hk["invalid"])
	}
	// Scan order is newest key first — assert the exact sequence so a
	// count-only or unordered payload cannot pass.
	want := []struct {
		key     string
		reasons []string
	}{
		{"deploy/handoffkeep/20260923T1456Z", []string{"key", "deployed_at"}},
		{"deploy/handoffkeep/20260923T1456", []string{"key"}},
		{"deploy/handoffkeep/20260923T070000Z", []string{"schema"}},
		{"deploy/handoffkeep/20260923T060000Z", []string{"deployed_at"}},
	}
	for i, raw := range invalid {
		row := raw.(map[string]any)
		if row["key"] != want[i].key {
			t.Fatalf("invalid[%d].key=%v, want %s (list must be newest-key-first)", i, row["key"], want[i].key)
		}
		reasons := []string{}
		for _, r := range row["reasons"].([]any) {
			reasons = append(reasons, r.(string))
		}
		if fmt.Sprintf("%v", reasons) != fmt.Sprintf("%v", want[i].reasons) {
			t.Fatalf("invalid[%d] %s reasons=%v, want %v", i, want[i].key, reasons, want[i].reasons)
		}
	}
	if hk["current"].(map[string]any)["deployed_ref"] != "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" {
		t.Fatalf("valid record must still be current: %v", hk["current"])
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
