package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// benchAreaClean drops the exception rows and their catalog fixtures this
// file's tests write, so reruns against a shared CI database stay clean.
func benchAreaClean(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	for _, query := range []string{
		`DELETE FROM bench_area_exceptions WHERE profile LIKE 'bench-exc-%'`,
		`DELETE FROM bench_catalog WHERE profile LIKE 'bench-exc-%'`,
		`DELETE FROM bench_grades WHERE profile LIKE 'bench-exc-%'`,
	} {
		if _, err := p.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
}

// benchAreaSeedCatalog writes the catalog rows the exceptions reference: the
// exception path refuses a (profile, effort) with no catalog row, live or
// retired.
func benchAreaSeedCatalog(t *testing.T, p *pgxpool.Pool, profiles ...string) {
	t.Helper()
	for _, profile := range profiles {
		for _, effort := range []string{"", "high"} {
			if _, err := p.Exec(t.Context(), `INSERT INTO bench_catalog(profile,effort,model_id,pool,grade,deviation_ref,decided_at,decided_by) VALUES($1,$2,'m-'||$1,'test','A','decision/2026-10-24/seed',now(),'ops') ON CONFLICT (profile,effort) DO UPDATE SET retired_at=NULL`, profile, effort); err != nil {
				t.Fatal(err)
			}
		}
	}
	// bench-exc-retired-catalog: a retired catalog row still admits exceptions.
	if _, err := p.Exec(t.Context(), `INSERT INTO bench_catalog(profile,effort,model_id,pool,grade,deviation_ref,decided_at,decided_by,retired_at) VALUES('bench-exc-retired-catalog','','m-ret','test','B','decision/2026-10-24/seed',now(),'ops',now()) ON CONFLICT (profile,effort) DO UPDATE SET retired_at=now()`); err != nil {
		t.Fatal(err)
	}
}

func benchAreaException(profile, effort, area, action string, refs []string) store.BenchAreaException {
	return store.BenchAreaException{
		Profile:            profile,
		Effort:             effort,
		Area:               area,
		Action:             action,
		EvidenceRefs:       refs,
		DecisionRef:        "hk:task/1944",
		DecidedBy:          "operator",
		DecidedAt:          time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC),
		ExpiresWithModelID: "m-" + profile,
	}
}

// TestBenchAreaExceptionsStore covers the DB contract: upsert by key,
// retirement through retired_at, include_retired listing, the (profile,
// effort) catalog gate, the unique key, and the same-key prefer-plus-exclude
// refusal.
func TestBenchAreaExceptionsStore(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	benchAreaClean(t, p)
	benchAreaSeedCatalog(t, p, "bench-exc-alpha")
	ctx := context.Background()

	// An exception for a (profile, effort) with no catalog row is refused.
	if _, err := s.UpsertBenchAreaExceptions(ctx, []store.BenchAreaException{
		benchAreaException("bench-exc-missing", "", "blender", "exclude", []string{"hk:task/1836"}),
	}); !errors.Is(err, store.ErrBenchAreaExceptionProfileUnknown) {
		t.Fatalf("unknown profile err=%v", err)
	}
	// A known profile but a rung with no catalog row is refused too.
	if _, err := s.UpsertBenchAreaExceptions(ctx, []store.BenchAreaException{
		benchAreaException("bench-exc-alpha", "max", "blender", "exclude", []string{"hk:task/1836"}),
	}); !errors.Is(err, store.ErrBenchAreaExceptionProfileUnknown) {
		t.Fatalf("unknown rung err=%v", err)
	}
	// A retired catalog row still backs an exception.
	if _, err := s.UpsertBenchAreaExceptions(ctx, []store.BenchAreaException{
		benchAreaException("bench-exc-retired-catalog", "", "docs", "prefer", []string{"srv:1031"}),
	}); err != nil {
		t.Fatalf("retired-catalog exception err=%v", err)
	}

	// Upsert creates and re-writes one row per key.
	x := benchAreaException("bench-exc-alpha", "high", "blender", "exclude", []string{"hk:task/1836", "hk:task/1847"})
	if n, err := s.UpsertBenchAreaExceptions(ctx, []store.BenchAreaException{x}); err != nil || n != 1 {
		t.Fatalf("create n=%d err=%v", n, err)
	}
	x.EvidenceRefs = []string{"hk:task/1836", "hk:task/1847", "srv:990"}
	x.DecisionRef = "hk:doc/decision/2026-10-24/blender-exclude"
	if n, err := s.UpsertBenchAreaExceptions(ctx, []store.BenchAreaException{x}); err != nil || n != 1 {
		t.Fatalf("update n=%d err=%v", n, err)
	}
	if got := benchCount(t, p, `SELECT count(*) FROM bench_area_exceptions WHERE profile='bench-exc-alpha'`); got != 1 {
		t.Fatalf("unique key violated: rows=%d", got)
	}
	live, err := s.ListBenchAreaExceptions(ctx, "", false)
	if err != nil || len(live) != 2 {
		t.Fatalf("live list=%+v err=%v", live, err)
	}
	var found *store.BenchAreaException
	for i := range live {
		if live[i].Profile == "bench-exc-alpha" {
			found = &live[i]
		}
	}
	if found == nil || len(found.EvidenceRefs) != 3 || found.DecisionRef != "hk:doc/decision/2026-10-24/blender-exclude" || found.RetiredAt != nil {
		t.Fatalf("updated row=%+v", found)
	}

	// prefer and exclude cannot both be live for one key: the flip is
	// refused until the live row is retired.
	prefer := benchAreaException("bench-exc-alpha", "high", "blender", "prefer", []string{"hk:task/1850"})
	if _, err = s.UpsertBenchAreaExceptions(ctx, []store.BenchAreaException{prefer}); !errors.Is(err, store.ErrBenchAreaExceptionConflict) {
		t.Fatalf("live flip err=%v", err)
	}
	// Same refusal inside one batch: two entries for one key cannot both
	// leave a live row, so the batch is rejected whole.
	other := benchAreaException("bench-exc-alpha", "", "docs", "exclude", []string{"srv:12"})
	if _, err = s.UpsertBenchAreaExceptions(ctx, []store.BenchAreaException{prefer, x, other}); !errors.Is(err, store.ErrBenchAreaExceptionConflict) {
		t.Fatalf("intra-batch flip err=%v", err)
	}
	if got := benchCount(t, p, `SELECT count(*) FROM bench_area_exceptions WHERE profile='bench-exc-alpha'`); got != 1 {
		t.Fatalf("refused batch wrote rows=%d", got)
	}
	// Retire, then the prefer lands.
	retire := x
	now := time.Now().UTC()
	retire.RetiredAt = &now
	if _, err = s.UpsertBenchAreaExceptions(ctx, []store.BenchAreaException{retire}); err != nil {
		t.Fatalf("retire err=%v", err)
	}
	live, err = s.ListBenchAreaExceptions(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range live {
		if row.Profile == "bench-exc-alpha" && row.Area == "blender" {
			t.Fatalf("retired row still listed: %+v", row)
		}
	}
	withRetired, err := s.ListBenchAreaExceptions(ctx, "", true)
	if err != nil {
		t.Fatal(err)
	}
	var retiredRow *store.BenchAreaException
	for i := range withRetired {
		if withRetired[i].Profile == "bench-exc-alpha" && withRetired[i].Area == "blender" {
			retiredRow = &withRetired[i]
		}
	}
	if retiredRow == nil || retiredRow.RetiredAt == nil {
		t.Fatalf("include_retired missing the retired row: %+v", withRetired)
	}
	if _, err = s.UpsertBenchAreaExceptions(ctx, []store.BenchAreaException{prefer}); err != nil {
		t.Fatalf("prefer after retire err=%v", err)
	}
	if got := benchCount(t, p, `SELECT count(*) FROM bench_area_exceptions WHERE profile='bench-exc-alpha' AND area='blender' AND retired_at IS NULL AND action='prefer'`); got != 1 {
		t.Fatalf("prefer after retire rows=%d", got)
	}

	// The area filter is exact and validated.
	docs, err := s.ListBenchAreaExceptions(ctx, "docs", false)
	if err != nil || len(docs) != 2 {
		t.Fatalf("area=docs list=%+v err=%v", docs, err)
	}
	if _, err = s.ListBenchAreaExceptions(ctx, "not-an-area", false); !errors.Is(err, store.ErrTaskAreaUnknown) {
		t.Fatalf("unknown area filter err=%v", err)
	}
}

// TestBenchAreaExceptionConcurrentKey fires two writes at one key: a live
// prefer racing a live exclude lets exactly one land (the loser gets the
// action conflict), and a retire racing a same-action upsert never errors —
// in every order the table keeps exactly one row.
func TestBenchAreaExceptionConcurrentKey(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	benchAreaClean(t, p)
	benchAreaSeedCatalog(t, p, "bench-exc-race")
	ctx := context.Background()

	race := func(a, b store.BenchAreaException) (error, error) {
		t.Helper()
		start := make(chan struct{})
		out := make(chan error, 2)
		var wg sync.WaitGroup
		for _, x := range []store.BenchAreaException{a, b} {
			wg.Add(1)
			go func(x store.BenchAreaException) {
				defer wg.Done()
				<-start
				_, err := s.UpsertBenchAreaExceptions(ctx, []store.BenchAreaException{x})
				out <- err
			}(x)
		}
		close(start)
		wg.Wait()
		return <-out, <-out
	}

	exc := benchAreaException("bench-exc-race", "", "unity", "exclude", []string{"hk:task/1"})
	pre := benchAreaException("bench-exc-race", "", "unity", "prefer", []string{"hk:task/2"})
	errA, errB := race(exc, pre)
	conflicts, oks := 0, 0
	for _, err := range []error{errA, errB} {
		switch {
		case err == nil:
			oks++
		case errors.Is(err, store.ErrBenchAreaExceptionConflict):
			conflicts++
		default:
			t.Fatalf("unexpected race error %v", err)
		}
	}
	if oks != 1 || conflicts != 1 {
		t.Fatalf("prefer-vs-exclude race: ok=%d conflicts=%d", oks, conflicts)
	}
	if got := benchCount(t, p, `SELECT count(*) FROM bench_area_exceptions WHERE profile='bench-exc-race'`); got != 1 {
		t.Fatalf("race rows=%d", got)
	}
	var liveActions int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM bench_area_exceptions WHERE profile='bench-exc-race' AND retired_at IS NULL`).Scan(&liveActions); err != nil || liveActions != 1 {
		t.Fatalf("live rows after race=%d err=%v", liveActions, err)
	}

	// A retire racing a same-action upsert: both are valid writes; whichever
	// lands last decides retired_at, and neither errors.
	retire := exc
	ts := time.Now().UTC()
	retire.RetiredAt = &ts
	errA, errB = race(exc, retire)
	if errA != nil || errB != nil {
		t.Fatalf("retire-vs-upsert race errors: %v / %v", errA, errB)
	}
	if got := benchCount(t, p, `SELECT count(*) FROM bench_area_exceptions WHERE profile='bench-exc-race'`); got != 1 {
		t.Fatalf("retire race rows=%d", got)
	}
}

func benchAreaExceptionBody(t *testing.T, rows ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"exceptions": rows})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func benchAreaExceptionRow(profile, effort, area, action string, refs []string) map[string]any {
	return map[string]any{
		"profile":               profile,
		"effort":                effort,
		"area":                  area,
		"action":                action,
		"evidence_refs":         refs,
		"decision_ref":          "hk:task/1944",
		"expires_with_model_id": "m-" + profile,
	}
}

func benchAreaExceptionsGet(t *testing.T, h *httptest.Server, token, query string) []map[string]any {
	t.Helper()
	resp := benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/area-exceptions"+query, token, nil)
	defer resp.Body.Close()
	var got struct {
		Exceptions []map[string]any `json:"exceptions"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&got) != nil {
		t.Fatalf("area-exceptions get %s status=%d", query, resp.StatusCode)
	}
	return got.Exceptions
}

// TestBenchAreaExceptionsAPI covers the HTTP contract: GET for any
// authenticated client, PUT restricted to the operator client with the same
// decode strictness as the catalog (unknown fields named), and every
// validation refusal surfacing as a 400 — or 409 for the same-key
// prefer-plus-exclude conflict.
func TestBenchAreaExceptionsAPI(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	benchAreaClean(t, p)
	benchAreaSeedCatalog(t, p, "bench-exc-api")
	h := benchServer(s)
	defer h.Close()

	row := benchAreaExceptionRow("bench-exc-api", "high", "blender", "exclude", []string{"hk:task/1836", "srv:990"})
	put := func(token string, body []byte) *http.Response {
		return benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/area-exceptions", token, body)
	}

	// The operator boundary: unauthenticated 401, any other client 403
	// operator_required — identical to the catalog writer rule.
	resp := put("", benchAreaExceptionBody(t, row))
	if resp.StatusCode != http.StatusUnauthorized || benchJSON(t, resp)["error"] != "unauthorized" {
		t.Fatalf("unauthenticated put status=%d", resp.StatusCode)
	}
	for _, token := range []string{"test-token", "peer-token"} {
		resp = put(token, benchAreaExceptionBody(t, row))
		if resp.StatusCode != http.StatusForbidden || benchJSON(t, resp)["error"] != "operator_required" {
			t.Fatalf("client-token put status=%d", resp.StatusCode)
		}
	}
	// GET is open to every authenticated client — and refused without one.
	resp = benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/area-exceptions", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Fatalf("unauthenticated get status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Decode strictness: an unknown field is a 400 that names the field.
	bad := benchAreaExceptionRow("bench-exc-api", "high", "blender", "exclude", []string{"hk:task/1"})
	bad["bogus_field"] = true
	resp = put("operator-token", benchAreaExceptionBody(t, bad))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d", resp.StatusCode)
	}
	body := benchJSON(t, resp)
	if body["error"] != "invalid_bench_area_exceptions_json" || !strings.Contains(body["detail"].(string), "bogus_field") {
		t.Fatalf("unknown field body=%v", body)
	}
	// An empty batch and a malformed body are refused as well.
	resp = put("operator-token", benchAreaExceptionBody(t))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty batch status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = put("operator-token", []byte(`{"exceptions":[{"profile":`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("truncated body status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Validation refusals reach the API as 400s naming the reason.
	for _, tc := range []struct {
		name string
		row  map[string]any
		want string
	}{
		{"area outside vocabulary", benchAreaExceptionRow("bench-exc-api", "high", "go", "exclude", []string{"hk:task/1"}), "invalid_request"},
		{"closed action", benchAreaExceptionRow("bench-exc-api", "high", "blender", "require", []string{"hk:task/1"}), "invalid_context"},
		{"empty evidence", benchAreaExceptionRow("bench-exc-api", "high", "blender", "exclude", nil), "invalid_context"},
		{"bad ref shape", benchAreaExceptionRow("bench-exc-api", "high", "blender", "exclude", []string{"local:12"}), "invalid_context"},
		{"missing decision_ref", func() map[string]any {
			r := benchAreaExceptionRow("bench-exc-api", "high", "blender", "exclude", []string{"hk:task/1"})
			delete(r, "decision_ref")
			return r
		}(), "decision_ref_required"},
		{"missing expires_with_model_id", func() map[string]any {
			r := benchAreaExceptionRow("bench-exc-api", "high", "blender", "exclude", []string{"hk:task/1"})
			delete(r, "expires_with_model_id")
			return r
		}(), "invalid_context"},
		{"profile not in catalog", benchAreaExceptionRow("bench-exc-unknown", "", "blender", "exclude", []string{"hk:task/1"}), "bench_area_exception_profile_unknown"},
	} {
		resp = put("operator-token", benchAreaExceptionBody(t, tc.row))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status=%d", tc.name, resp.StatusCode)
		}
		if body = benchJSON(t, resp); body["error"] != tc.want {
			t.Fatalf("%s error=%v want=%s", tc.name, body, tc.want)
		}
	}

	// decided_by and decided_at are server-set: a caller-supplied value is
	// replaced, never trusted.
	spoof := benchAreaExceptionRow("bench-exc-api", "high", "blender", "exclude", []string{"hk:task/1836", "srv:990"})
	spoof["decided_by"] = "spoofed"
	spoof["decided_at"] = "2001-01-01T00:00:00Z"
	resp = put("operator-token", benchAreaExceptionBody(t, spoof))
	if resp.StatusCode != http.StatusOK || benchJSON(t, resp)["upserted"] != float64(1) {
		t.Fatalf("operator put status=%d", resp.StatusCode)
	}
	var decidedBy string
	var decidedAt time.Time
	if err := p.QueryRow(t.Context(), `SELECT decided_by,decided_at FROM bench_area_exceptions WHERE profile='bench-exc-api'`).Scan(&decidedBy, &decidedAt); err != nil {
		t.Fatal(err)
	}
	if decidedBy != "operator" || decidedAt.Year() < 2026 {
		t.Fatalf("server-set fields decided_by=%q decided_at=%v", decidedBy, decidedAt)
	}

	// GET returns the row to any client; the area filter narrows it.
	list := benchAreaExceptionsGet(t, h, "peer-token", "")
	if len(list) != 1 || list[0]["profile"] != "bench-exc-api" || list[0]["action"] != "exclude" || list[0]["decided_by"] != "operator" {
		t.Fatalf("list=%v", list)
	}
	if got := benchAreaExceptionsGet(t, h, "test-token", "?area=docs"); len(got) != 0 {
		t.Fatalf("area=docs list=%v", got)
	}
	resp = benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/area-exceptions?area=not-an-area", "peer-token", nil)
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("unknown area filter status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// prefer on the live exclude key is a 409 conflict; retiring through the
	// API frees the key and include_retired shows the history row.
	preRow := benchAreaExceptionRow("bench-exc-api", "high", "blender", "prefer", []string{"hk:task/1850"})
	resp = put("operator-token", benchAreaExceptionBody(t, preRow))
	if resp.StatusCode != http.StatusConflict || benchJSON(t, resp)["error"] != "bench_area_exception_action_conflict" {
		t.Fatalf("live flip status=%d", resp.StatusCode)
	}
	retRow := benchAreaExceptionRow("bench-exc-api", "high", "blender", "exclude", []string{"hk:task/1836", "srv:990"})
	retRow["retired_at"] = "2026-10-24T01:00:00Z"
	resp = put("operator-token", benchAreaExceptionBody(t, retRow))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("retire put status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	if got := benchAreaExceptionsGet(t, h, "peer-token", ""); len(got) != 0 {
		t.Fatalf("retired still listed: %v", got)
	}
	if got := benchAreaExceptionsGet(t, h, "peer-token", "?include_retired=1"); len(got) != 1 || got[0]["retired_at"] == nil {
		t.Fatalf("include_retired list=%v", got)
	}
	resp = put("operator-token", benchAreaExceptionBody(t, preRow))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prefer after retire status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	list = benchAreaExceptionsGet(t, h, "peer-token", "?area=blender")
	if len(list) != 1 || list[0]["action"] != "prefer" {
		t.Fatalf("post-retire list=%v", list)
	}
}
