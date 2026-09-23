package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// The console fixture was produced by marshaling store.BenchCatalogEntry — the
// deployed GET /v1/bench/catalog wire type (#592, hk f0681e5) — with rows
// hand-picked and ordered to exercise the states the screen renders (a retired
// row, a consult_only row, an ESTIMATED_ annotation, a null score, a default
// effort row); it is not a verbatim server capture and not in ladder order.
// This test pins the shape: the committed fixture must round-trip through the
// real Go type with no field loss.
func TestBenchCatalogFixtureMatchesGoType(t *testing.T) {
	raw, err := os.ReadFile("../../web/console/src/queue-proto/grades/fixtures/catalog.response.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var response benchCatalogResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("fixture does not decode as benchCatalogResponse: %v", err)
	}
	if len(response.Catalog) == 0 || response.GeneratedAt.IsZero() {
		t.Fatalf("fixture is empty or missing generated_at: %s", raw)
	}
	back, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var original, roundtrip any
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(back, &roundtrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, roundtrip) {
		t.Fatalf("fixture loses fields through store.BenchCatalogEntry\noriginal: %s\nroundtrip: %s", raw, back)
	}
	var retired, consultOnly, estimated, nullScore, defaultEffort bool
	for _, entry := range response.Catalog {
		retired = retired || entry.RetiredAt != nil
		consultOnly = consultOnly || entry.Gate == "consult_only"
		if entry.BenchmarkAnnotation != nil && len(*entry.BenchmarkAnnotation) >= len("ESTIMATED") && (*entry.BenchmarkAnnotation)[:len("ESTIMATED")] == "ESTIMATED" {
			estimated = true
		}
		nullScore = nullScore || entry.Score == nil
		defaultEffort = defaultEffort || entry.Effort == ""
	}
	for name, seen := range map[string]bool{"retired": retired, "consult_only": consultOnly, "estimated": estimated, "null score": nullScore, "default effort": defaultEffort} {
		if !seen {
			t.Errorf("fixture has no %s row — the screen's %s path is untested", name, name)
		}
	}
}

func TestBenchCatalogBFFRefusesServiceIdentity(t *testing.T) {
	h := &Handler{hub: newHubProxy("", "", nil)}
	request := httptest.NewRequest(http.MethodGet, "/ui/api/bench/catalog", nil)
	recorder := httptest.NewRecorder()
	h.serveAPI(recorder, request, cfaccess.Identity{ServiceName: "glance-fixture"})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("service identity: status=%d", recorder.Code)
	}
}

func TestBenchCatalogBFFHasNoWriteRoute(t *testing.T) {
	h := &Handler{hub: newHubProxy("", "", nil)}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		request := httptest.NewRequest(method, "/ui/api/bench/catalog", nil)
		recorder := httptest.NewRecorder()
		h.serveAPI(recorder, request, cfaccess.Identity{Email: "op@example.com"})
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /ui/api/bench/catalog: status=%d, want 405", method, recorder.Code)
		}
	}
}

func testCatalogStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL catalog tests")
	}
	s, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestBenchCatalogBFFReadOnly(t *testing.T) {
	s := testCatalogStore(t)
	ctx := context.Background()
	// Unique pools per run: the test DB may carry other rows, so assertions
	// look at the filtered ladder instead of global counts. Every seed uses a
	// non-empty effort on purpose — effort='' rows mirror into bench_grades
	// and would pollute the e2e grade-list assertions in tests/ that share
	// this database. Rows are still cleaned up for tidiness.
	poolX := fmt.Sprintf("tpool-x-%d", time.Now().UnixNano())
	poolY := fmt.Sprintf("tpool-y-%d", time.Now().UnixNano())
	prefix := fmt.Sprintf("t597prof-%d-", time.Now().UnixNano())
	decided := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	retired := decided.Add(24 * time.Hour)
	reason := "advisory only"
	rows := []store.BenchCatalogEntry{
		{Profile: prefix + "a", Effort: "low", ModelID: "model-a", Pool: poolX, Grade: "S", Gate: "default", DeviationRef: "dev-a", DecidedAt: decided, DecidedBy: "test"},
		{Profile: prefix + "b", Effort: "low", ModelID: "model-b", Pool: poolX, Grade: "A", Gate: "consult_only", GateReason: &reason, DeviationRef: "dev-b", DecidedAt: decided, DecidedBy: "test"},
		{Profile: prefix + "d", Effort: "low", ModelID: "model-d", Pool: poolX, Grade: "C", Gate: "default", DeviationRef: "dev-d", DecidedAt: decided, DecidedBy: "test", RetiredAt: &retired},
		{Profile: prefix + "c", Effort: "low", ModelID: "model-c", Pool: poolY, Grade: "B", Gate: "default", DeviationRef: "dev-c", DecidedAt: decided, DecidedBy: "test"},
	}
	if n, err := s.UpsertBenchCatalog(ctx, rows); err != nil || n != len(rows) {
		t.Fatalf("seed: n=%d err=%v", n, err)
	}
	t.Cleanup(func() {
		db, err := pgx.Connect(context.Background(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
		if err != nil {
			return
		}
		defer db.Close(context.Background())
		_, _ = db.Exec(context.Background(), `DELETE FROM bench_catalog WHERE profile LIKE $1`, prefix+"%")
	})
	h := &Handler{store: s}

	get := func(path string) benchCatalogResponse {
		t.Helper()
		recorder := httptest.NewRecorder()
		h.benchCatalog(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
		var response benchCatalogResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("GET %s: undecodable %s", path, recorder.Body.String())
		}
		return response
	}
	profiles := func(rows []store.BenchCatalogEntry) []string {
		out := []string{}
		for _, row := range rows {
			out = append(out, row.Profile)
		}
		return out
	}

	ladder := get("/ui/api/bench/catalog?pool=" + poolX)
	if got := profiles(ladder.Catalog); !reflect.DeepEqual(got, []string{prefix + "a"}) {
		t.Fatalf("pool ladder must drop consult_only and retired: %v", got)
	}
	full := get("/ui/api/bench/catalog?pool=" + poolX + "&include_retired=1")
	if got := profiles(full.Catalog); !reflect.DeepEqual(got, []string{prefix + "a", prefix + "d"}) {
		t.Fatalf("pool+include_retired must keep grade order and still drop consult_only: %v", got)
	}
	all := get("/ui/api/bench/catalog")
	foundConsultOnly := false
	for _, row := range all.Catalog {
		if row.Profile == prefix+"b" {
			foundConsultOnly = true
		}
	}
	if !foundConsultOnly || all.GeneratedAt.IsZero() {
		t.Fatalf("unfiltered must keep consult_only and stamp generated_at: %+v", all)
	}
	// The pool key is verbatim catalog semantics — only length/NUL are
	// rejected, so a spacey name is a valid (empty) ladder, not a 400.
	bad := httptest.NewRecorder()
	h.benchCatalog(bad, httptest.NewRequest(http.MethodGet, "/ui/api/bench/catalog?pool="+strings.Repeat("x", 201), nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("over-limit pool: status=%d", bad.Code)
	}
}
