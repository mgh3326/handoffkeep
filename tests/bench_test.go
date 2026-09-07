package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

const benchScoresFixture = `{
  "scores": [
    {
      "model_id": "claude-opus-5",
      "effort": "high",
      "harness": "claude-code",
      "source": "AA-agent",
      "metric": "agentic",
      "score": 13.4,
      "rank": 18,
      "captured_at": "2026-07-31T00:00:00Z",
      "time_per_task_min": 13.4,
      "cost_per_task_usd": 3.8,
      "provenance": "operator-approved manual import 2026-09-07",
      "updated_by": "bench-client",
      "updated_at": "2026-09-07T04:30:00Z"
    },
    {
      "model_id": "kimi-k3",
      "effort": "",
      "harness": "",
      "source": "AA-model",
      "metric": "coding_index",
      "score": 72.0,
      "rank": null,
      "captured_at": "2026-09-07T00:00:00Z",
      "time_per_task_min": null,
      "cost_per_task_usd": null,
      "provenance": "",
      "updated_by": "bench-client",
      "updated_at": "2026-09-07T04:30:00Z"
    }
  ]
}`

const benchRepFixture = `{
  "reps": [
    {
      "id": 1,
      "origin_id": 568,
      "profile": "codex-terra",
      "model_id": "gpt-5.6-terra",
      "task_ref": "PR#42",
      "tier": "T2",
      "role": "impl",
      "rounds": 1,
      "blockers_found": 0,
      "completed": 1,
      "input_tokens": 120000,
      "output_tokens": 8000,
      "notes": "토큰 미상",
      "recorded_at": "2026-09-01T10:00:00Z",
      "effort": "max",
      "grade": "A+",
      "table_grade": "A+",
      "created_by": "bench-client",
      "created_at": "2026-09-07T04:30:00Z"
    }
  ]
}`

const benchGradeFixture = `{
  "grades": [
    {
      "profile": "codex-terra-max",
      "grade": "S",
      "boundary_version": "2026-09-07",
      "deviation_ref": "deviation-2026-09-07-bench-canonical",
      "decided_at": "2026-09-07T04:30:00Z",
      "decided_by": "bench-client"
    }
  ]
}`

func benchStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL benchmark tests")
	}
	s, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func benchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL benchmark tests")
	}
	p, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func benchServer(s *store.Store) *httptest.Server {
	return httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"bench-client": "test-token", "bench-peer": "peer-token"}}.Handler())
}

func benchRequest(t *testing.T, client *http.Client, method, url, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func benchJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func benchItem(t *testing.T, fixture, key string) map[string]any {
	t.Helper()
	var body map[string][]map[string]any
	if err := json.Unmarshal([]byte(fixture), &body); err != nil {
		t.Fatal(err)
	}
	return body[key][0]
}

func benchBody(t *testing.T, key string, item map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{key: []map[string]any{item}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func benchCount(t *testing.T, p *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func benchClean(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	for _, query := range []string{
		`DELETE FROM bench_scores WHERE model_id IN ('claude-opus-5','kimi-k3','guard-model','identity-score') OR model_id LIKE 'bench-limit-%' OR model_id IN ('benchlm-model','bench-invalid-score')`,
		`DELETE FROM bench_reps WHERE origin_id IN (568,569,570,571,572,573) AND created_by IN ('bench-client','bench-peer')`,
		`DELETE FROM bench_grades WHERE profile LIKE 'bench-grade-%' OR profile='codex-terra-max'`,
	} {
		if _, err := p.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBenchSchemaV9IsAdditiveAndIdempotent(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	for _, table := range []string{"bench_scores", "bench_reps", "bench_grades"} {
		var name *string
		if err := p.QueryRow(t.Context(), `SELECT to_regclass($1)`, table).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == nil {
			t.Fatalf("table %s is missing", table)
		}
	}
	var version int
	if err := p.QueryRow(t.Context(), `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != 9 {
		t.Fatalf("max schema version=%d err=%v", version, err)
	}
	if got := benchCount(t, p, `SELECT count(*) FROM schema_version WHERE version=9`); got != 1 {
		t.Fatalf("version 9 rows=%d", got)
	}
	s.Close()
	s2, err := store.Open(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if got := benchCount(t, p, `SELECT count(*) FROM schema_version WHERE version=9`); got != 1 {
		t.Fatalf("version 9 rows after second open=%d", got)
	}
	if _, err := p.Exec(t.Context(), `DELETE FROM schema_version WHERE version=9`); err != nil {
		t.Fatal(err)
	}
	s3, err := store.Open(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	s3.Close()
	if got := benchCount(t, p, `SELECT count(*) FROM schema_version WHERE version=9`); got != 1 {
		t.Fatalf("version 9 rows after gate replay=%d", got)
	}
}

func TestBenchCanonicalAPI(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	benchClean(t, p)
	h := benchServer(s)
	defer h.Close()

	// Scores: idempotency, five-column identity, null normalization, filters,
	// ordering, structural validation, server identity, guard, and limits.
	resp := benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/scores", "test-token", []byte(benchScoresFixture))
	if resp.StatusCode != http.StatusOK || benchJSON(t, resp)["upserted"] != float64(2) {
		t.Fatalf("first score put status=%d", resp.StatusCode)
	}
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/scores", "test-token", []byte(benchScoresFixture))
	if resp.StatusCode != http.StatusOK || benchJSON(t, resp)["upserted"] != float64(2) || benchCount(t, p, `SELECT count(*) FROM bench_scores WHERE model_id IN ('claude-opus-5','kimi-k3')`) != 2 {
		t.Fatalf("idempotent score put status=%d", resp.StatusCode)
	}
	third := benchItem(t, benchScoresFixture, "scores")
	third["effort"] = "low"
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/scores", "test-token", benchBody(t, "scores", third))
	if resp.StatusCode != http.StatusOK || benchCount(t, p, `SELECT count(*) FROM bench_scores WHERE model_id='claude-opus-5' AND source='AA-agent' AND metric='agentic'`) != 2 {
		t.Fatalf("effort did not remain in score identity status=%d", resp.StatusCode)
	}
	resp = benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/scores", "test-token", nil)
	var scoreList struct {
		Scores []map[string]any `json:"scores"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&scoreList) != nil || len(scoreList.Scores) != 3 || scoreList.Scores[0]["source"] != "AA-agent" || scoreList.Scores[0]["effort"] != "high" {
		resp.Body.Close()
		t.Fatalf("score ordering/list status=%d rows=%v", resp.StatusCode, scoreList.Scores)
	}
	resp.Body.Close()
	for _, query := range []string{"?model_id=kimi-k3", "?source=AA-model", "?limit=1"} {
		resp = benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/scores"+query, "test-token", nil)
		var got struct {
			Scores []map[string]any `json:"scores"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&got) != nil || len(got.Scores) != 1 {
			resp.Body.Close()
			t.Fatalf("score query %s status=%d rows=%v", query, resp.StatusCode, got.Scores)
		}
		resp.Body.Close()
	}
	resp = benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/scores?model_id=kimi-k3", "test-token", nil)
	var sparse struct {
		Scores []map[string]any `json:"scores"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&sparse) != nil || sparse.Scores[0]["effort"] != "" || sparse.Scores[0]["harness"] != "" || sparse.Scores[0]["provenance"] != "" || sparse.Scores[0]["rank"] != nil {
		resp.Body.Close()
		t.Fatalf("score empty-string/null contract status=%d rows=%v", resp.StatusCode, sparse.Scores)
	}
	resp.Body.Close()
	unknownSource := benchItem(t, benchScoresFixture, "scores")
	unknownSource["model_id"] = "benchlm-model"
	unknownSource["source"] = "benchlm"
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/scores", "test-token", benchBody(t, "scores", unknownSource))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("unknown source status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	badScore := benchItem(t, benchScoresFixture, "scores")
	badScore["model_id"] = "bench-invalid-score"
	badScore["score"] = 101
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/scores", "test-token", benchBody(t, "scores", badScore))
	if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "invalid_context" {
		t.Fatalf("bad score status=%d", resp.StatusCode)
	}
	identityScore := benchItem(t, benchScoresFixture, "scores")
	identityScore["model_id"] = "identity-score"
	identityScore["updated_by"] = "someone-else"
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/scores", "test-token", benchBody(t, "scores", identityScore))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("score identity put status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/scores?model_id=identity-score", "test-token", nil)
	var identityResult struct {
		Scores []map[string]any `json:"scores"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&identityResult) != nil || len(identityResult.Scores) != 1 || identityResult.Scores[0]["updated_by"] != "bench-client" {
		resp.Body.Close()
		t.Fatalf("score server identity status=%d rows=%v", resp.StatusCode, identityResult.Scores)
	}
	resp.Body.Close()
	guarded := benchItem(t, benchScoresFixture, "scores")
	guarded["model_id"] = "guard-model"
	guarded["provenance"] = "sk-abcdefghijklmnopqrstuvwxyz"
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/scores", "test-token", benchBody(t, "scores", guarded))
	if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "secret_like_content" || benchCount(t, p, `SELECT count(*) FROM bench_scores WHERE model_id='guard-model'`) != 0 {
		t.Fatalf("score guard status=%d", resp.StatusCode)
	}
	for _, n := range []int{0, 1001, 1000} {
		items := make([]map[string]any, n)
		for i := range items {
			items[i] = map[string]any{"model_id": fmt.Sprintf("bench-limit-%d", i), "effort": "", "harness": "", "source": "limit", "metric": "agentic", "score": 1, "rank": 1, "captured_at": "2026-09-07T00:00:00Z", "time_per_task_min": nil, "cost_per_task_usd": nil, "provenance": "", "updated_by": "someone-else", "updated_at": "2026-09-07T04:30:00Z"}
		}
		body, err := json.Marshal(map[string]any{"scores": items})
		if err != nil {
			t.Fatal(err)
		}
		resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/scores", "test-token", body)
		want := http.StatusOK
		if n != 1000 {
			want = http.StatusBadRequest
		}
		if resp.StatusCode != want {
			resp.Body.Close()
			t.Fatalf("score batch size=%d status=%d want=%d", n, resp.StatusCode, want)
		}
		resp.Body.Close()
	}

	// Repetitions: all carried columns, nullable values, origin identity, and
	// the server-owned creator field.
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/reps", "test-token", []byte(benchRepFixture))
	if resp.StatusCode != http.StatusOK || benchJSON(t, resp)["upserted"] != float64(1) {
		t.Fatalf("rep fixture put status=%d", resp.StatusCode)
	}
	resp = benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/reps?profile=codex-terra", "test-token", nil)
	var reps struct {
		Reps []map[string]any `json:"reps"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&reps) != nil || len(reps.Reps) < 1 {
		resp.Body.Close()
		t.Fatalf("rep fixture get status=%d rows=%v", resp.StatusCode, reps.Reps)
	}
	resp.Body.Close()
	wantRep := map[string]any{"origin_id": float64(568), "profile": "codex-terra", "model_id": "gpt-5.6-terra", "task_ref": "PR#42", "tier": "T2", "role": "impl", "rounds": float64(1), "blockers_found": float64(0), "completed": float64(1), "input_tokens": float64(120000), "output_tokens": float64(8000), "notes": "토큰 미상", "recorded_at": "2026-09-01T10:00:00Z", "effort": "max", "grade": "A+", "table_grade": "A+"}
	var repRow map[string]any
	for _, row := range reps.Reps {
		if row["origin_id"] == float64(568) && row["created_by"] == "bench-client" {
			repRow = row
		}
	}
	if repRow == nil {
		t.Fatal("fixture rep was not recorded by bench-client")
	}
	for key, want := range wantRep {
		if repRow[key] != want {
			t.Fatalf("rep %s=%v want=%v", key, repRow[key], want)
		}
	}
	updatedRep := benchItem(t, benchRepFixture, "reps")
	updatedRep["model_id"] = "gpt-5.6-terra-updated"
	updatedRep["created_by"] = "someone-else"
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/reps", "test-token", benchBody(t, "reps", updatedRep))
	if resp.StatusCode != http.StatusOK || benchCount(t, p, `SELECT count(*) FROM bench_reps WHERE origin_id=568 AND created_by='bench-client'`) != 1 {
		t.Fatalf("same-client rep upsert status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/reps", "peer-token", benchBody(t, "reps", updatedRep))
	if resp.StatusCode != http.StatusOK || benchCount(t, p, `SELECT count(*) FROM bench_reps WHERE origin_id=568`) != 2 {
		t.Fatalf("cross-client rep identity status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	for _, origin := range []any{nil, float64(0), float64(-1)} {
		invalid := benchItem(t, benchRepFixture, "reps")
		invalid["origin_id"] = origin
		if origin == nil {
			delete(invalid, "origin_id")
		}
		resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/reps", "test-token", benchBody(t, "reps", invalid))
		if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "invalid_context" {
			t.Fatalf("invalid origin=%v status=%d", origin, resp.StatusCode)
		}
	}
	withoutNullable := benchItem(t, benchRepFixture, "reps")
	withoutNullable["origin_id"] = 569
	delete(withoutNullable, "notes")
	delete(withoutNullable, "rounds")
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/reps", "test-token", benchBody(t, "reps", withoutNullable))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("nullable rep put status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/reps?profile=codex-terra", "test-token", nil)
	var nullableResult struct {
		Reps []map[string]any `json:"reps"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&nullableResult) != nil {
		resp.Body.Close()
		t.Fatalf("nullable rep get status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	for _, row := range nullableResult.Reps {
		if row["origin_id"] == float64(569) && (row["notes"] != nil || row["rounds"] != nil) {
			t.Fatalf("omitted nullable fields were not null: %v", row)
		}
	}

	// Grades: closed vocabulary, deviation requirement, atomic batches, and
	// server identity.
	for _, ref := range []any{nil, "", "   "} {
		grade := benchItem(t, benchGradeFixture, "grades")
		grade["profile"] = fmt.Sprintf("bench-grade-required-%d", len(fmt.Sprint(ref)))
		if ref == nil {
			delete(grade, "deviation_ref")
		} else {
			grade["deviation_ref"] = ref
		}
		resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/grades", "test-token", benchBody(t, "grades", grade))
		if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "deviation_ref_required" {
			t.Fatalf("deviation ref=%v status=%d", ref, resp.StatusCode)
		}
	}
	good := benchItem(t, benchGradeFixture, "grades")
	good["profile"] = "bench-grade-atomic-good"
	bad := benchItem(t, benchGradeFixture, "grades")
	bad["profile"] = "bench-grade-atomic-bad"
	bad["deviation_ref"] = ""
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/grades", "test-token", func() []byte {
		body, err := json.Marshal(map[string]any{"grades": []map[string]any{good, bad}})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}())
	if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "deviation_ref_required" || benchCount(t, p, `SELECT count(*) FROM bench_grades WHERE profile IN ('bench-grade-atomic-good','bench-grade-atomic-bad')`) != 0 {
		t.Fatalf("atomic grade rejection status=%d", resp.StatusCode)
	}
	invalidGrade := benchItem(t, benchGradeFixture, "grades")
	invalidGrade["profile"] = "bench-grade-vocabulary"
	invalidGrade["grade"] = "Z"
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/grades", "test-token", benchBody(t, "grades", invalidGrade))
	if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "invalid_context" {
		t.Fatalf("invalid grade status=%d", resp.StatusCode)
	}
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/grades", "test-token", []byte(benchGradeFixture))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("grade fixture put status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/grades", "test-token", nil)
	var grades struct {
		Grades []map[string]any `json:"grades"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&grades) != nil || len(grades.Grades) != 1 || grades.Grades[0]["decided_by"] != "bench-client" {
		resp.Body.Close()
		t.Fatalf("grade list identity status=%d rows=%v", resp.StatusCode, grades.Grades)
	}
	resp.Body.Close()

	// Every bench route is authenticated independently.
	routes := []struct {
		method string
		path   string
		body   []byte
	}{
		{http.MethodGet, "/v1/bench/scores", nil},
		{http.MethodPut, "/v1/bench/scores", []byte(benchScoresFixture)},
		{http.MethodGet, "/v1/bench/reps", nil},
		{http.MethodPut, "/v1/bench/reps", []byte(benchRepFixture)},
		{http.MethodGet, "/v1/bench/grades", nil},
		{http.MethodPut, "/v1/bench/grades", []byte(benchGradeFixture)},
	}
	for _, route := range routes {
		resp = benchRequest(t, h.Client(), route.method, h.URL+route.path, "", route.body)
		if resp.StatusCode != http.StatusUnauthorized || benchJSON(t, resp)["error"] != "unauthorized" {
			t.Fatalf("unauthorized %s %s status=%d", route.method, route.path, resp.StatusCode)
		}
	}

}
