package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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
	return httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"bench-client": "test-token", "bench-peer": "peer-token", "operator": "operator-token"}}.Handler())
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

func benchRepRow(t *testing.T, p *pgxpool.Pool, originID int64, createdBy string) map[string]any {
	t.Helper()
	rows, err := p.Query(t.Context(), `SELECT id,origin_id,profile,model_id,task_ref,tier,role,rounds,blockers_found,completed,input_tokens,output_tokens,notes,recorded_at,effort,grade,table_grade,created_by,created_at FROM bench_reps WHERE origin_id=$1 AND created_by=$2`, originID, createdBy)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil
	}
	values, err := rows.Values()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for i, field := range rows.FieldDescriptions() {
		out[field.Name] = values[i]
	}
	if rows.Next() {
		t.Fatalf("more than one bench_reps row for origin_id=%d created_by=%s", originID, createdBy)
	}
	return out
}

// sameRepRow compares two scanned bench_reps rows column-by-column, treating
// timestamps as instants (the same instant may scan with different location
// pointers across queries).
func sameRepRow(a, b map[string]any) bool {
	return maps.EqualFunc(a, b, func(x, y any) bool {
		if xt, ok := x.(time.Time); ok {
			yt, ok := y.(time.Time)
			return ok && xt.Equal(yt)
		}
		return x == y
	})
}

func benchClean(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	for _, query := range []string{
		`DELETE FROM bench_scores WHERE model_id IN ('claude-opus-5','kimi-k3','guard-model','identity-score') OR model_id LIKE 'bench-limit-%' OR model_id IN ('benchlm-model','bench-invalid-score')`,
		`DELETE FROM bench_reps WHERE origin_id IN (568,569,570,571,572,573) AND created_by IN ('bench-client','bench-peer')`,
		`DELETE FROM bench_catalog WHERE profile LIKE 'bench-%' OR profile IN ('codex-terra-max','codex-sol','kiro-sol','bench-grade-cat-compat')`,
		`DELETE FROM bench_grades WHERE profile LIKE 'bench-%' OR profile IN ('codex-terra-max','codex-sol','kiro-sol')`,
	} {
		if _, err := p.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBenchSchemaV9IsAdditiveAndIdempotent(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	for _, table := range []string{"bench_scores", "bench_reps", "bench_grades", "bench_catalog"} {
		var name *string
		if err := p.QueryRow(t.Context(), `SELECT to_regclass($1)`, table).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == nil {
			t.Fatalf("table %s is missing", table)
		}
	}
	var version int
	if err := p.QueryRow(t.Context(), `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != 17 {
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
	for _, v := range []int{9, 12} {
		if _, err := p.Exec(t.Context(), `DELETE FROM schema_version WHERE version=$1`, v); err != nil {
			t.Fatal(err)
		}
		s3, err := store.Open(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
		if err != nil {
			t.Fatal(err)
		}
		s3.Close()
		if got := benchCount(t, p, `SELECT count(*) FROM schema_version WHERE version=$1`, v); got != 1 {
			t.Fatalf("version %d rows after gate replay=%d", v, got)
		}
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
	wantScore := map[string]any{
		"score":             13.4,
		"rank":              float64(18),
		"captured_at":       "2026-07-31T00:00:00Z",
		"time_per_task_min": 13.4,
		"cost_per_task_usd": 3.8,
		"provenance":        "operator-approved manual import 2026-09-07",
	}
	for key, want := range wantScore {
		if got := scoreList.Scores[0][key]; got != want {
			t.Fatalf("score %s=%v want=%v", key, got, want)
		}
	}
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
		if n == 1000 {
			respBody := benchJSON(t, resp)
			if respBody["upserted"] != float64(1000) {
				t.Fatalf("score batch upserted=%v want=1000", respBody["upserted"])
			}
			if got := benchCount(t, p, `SELECT count(*) FROM bench_scores WHERE model_id LIKE 'bench-limit-%'`); got != 1000 {
				t.Fatalf("score batch db count=%d want=1000", got)
			}
			continue
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
	// A same-key write with different content is a collision, not an update:
	// 409, and the stored row keeps the original content byte-identical.
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("same-client rep overwrite status=%d want=409", resp.StatusCode)
	}
	conflictBody := benchJSON(t, resp)
	if conflictBody["error"] != "bench_rep_conflict" {
		t.Fatalf("same-client rep overwrite error=%v", conflictBody["error"])
	}
	conflicts, ok := conflictBody["conflicts"].([]any)
	if !ok || len(conflicts) != 1 {
		t.Fatalf("conflict body=%v", conflictBody)
	}
	first := conflicts[0].(map[string]any)
	if first["index"] != float64(0) || first["origin_id"] != float64(568) || first["conflict_server_id"] != repRow["id"] {
		t.Fatalf("conflict detail=%v want index=0 origin_id=568 conflict_server_id=%v", first, repRow["id"])
	}
	if row := benchRepRow(t, p, 568, "bench-client"); row["model_id"] != "gpt-5.6-terra" {
		t.Fatalf("overwritten rep row changed: %v", row)
	}
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/reps", "test-token", []byte(benchRepFixture))
	resendBody := benchJSON(t, resp)
	resendIDs, _ := resendBody["ids"].([]any)
	if resp.StatusCode != http.StatusOK || resendBody["upserted"] != float64(1) || len(resendIDs) != 1 || resendIDs[0] != repRow["id"] || benchCount(t, p, `SELECT count(*) FROM bench_reps WHERE origin_id=568 AND created_by='bench-client'`) != 1 {
		t.Fatalf("same-client rep resend status=%d body=%v", resp.StatusCode, resendBody)
	}
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
		{http.MethodGet, "/v1/bench/catalog", nil},
		{http.MethodPut, "/v1/bench/catalog", []byte(`{"catalog":[]}`)},
	}
	for _, route := range routes {
		resp = benchRequest(t, h.Client(), route.method, h.URL+route.path, "", route.body)
		if resp.StatusCode != http.StatusUnauthorized || benchJSON(t, resp)["error"] != "unauthorized" {
			t.Fatalf("unauthorized %s %s status=%d", route.method, route.path, resp.StatusCode)
		}
	}

}

// TestBenchRouteMethodAndUnknownPath guards against a "/" catch-all handler,
// which in Go's ServeMux matches every request and silently degrades a
// method-mismatched request on an existing route from 405 (with an Allow
// header) to 404. Unknown paths must keep the router's standard 404.
func TestBenchRouteMethodAndUnknownPath(t *testing.T) {
	s := benchStore(t)
	h := benchServer(s)
	defer h.Close()

	resp := benchRequest(t, h.Client(), http.MethodPost, h.URL+"/v1/bench/scores", "test-token", []byte(benchScoresFixture))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/bench/scores status=%d want=%d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "PUT") {
		t.Fatalf("POST /v1/bench/scores Allow=%q want to contain PUT", allow)
	}

	resp2 := benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/definitely-unknown", "test-token", nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /v1/definitely-unknown status=%d want=%d", resp2.StatusCode, http.StatusNotFound)
	}
}

func benchCatalogRow(profile, effort, pool, grade string, extra map[string]any) map[string]any {
	row := map[string]any{
		"profile":       profile,
		"effort":        effort,
		"model_id":      "m-" + profile,
		"pool":          pool,
		"grade":         grade,
		"gate":          "default",
		"deviation_ref": "decision/2026-09-23/catalog-seed",
		"decided_by":    "ops-review-592",
		"decided_at":    "2026-09-23T00:00:00Z",
	}
	for k, v := range extra {
		row[k] = v
	}
	return row
}

func benchCatalogBody(t *testing.T, rows ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"catalog": rows})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func benchCatalogGet(t *testing.T, h *httptest.Server, query string) []map[string]any {
	t.Helper()
	resp := benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/catalog"+query, "peer-token", nil)
	defer resp.Body.Close()
	var got struct {
		Catalog []map[string]any `json:"catalog"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&got) != nil {
		t.Fatalf("catalog get %s status=%d", query, resp.StatusCode)
	}
	return got.Catalog
}

// TestBenchCatalogAPI covers the operator-only write boundary, the required
// decided_by provenance, per-profile effort monotonicity, the Sol S+ rule,
// consult_only ladder exclusion, retirement filtering, idempotent upserts, and
// the legacy /v1/bench/grades projection. The 400 paths double as mutation
// coverage: dropping any of the validations makes a case here fail.
func TestBenchCatalogAPI(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	benchClean(t, p)
	h := benchServer(s)
	defer h.Close()

	row := benchCatalogRow("bench-cat-apex", "", "codex", "S+", nil)
	put := func(token string, rows ...map[string]any) *http.Response {
		return benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/catalog", token, benchCatalogBody(t, rows...))
	}

	// The operator boundary: no token and ordinary client tokens are refused
	// before the body is even read.
	resp := put("", row)
	if resp.StatusCode != http.StatusUnauthorized || benchJSON(t, resp)["error"] != "unauthorized" {
		t.Fatalf("unauthenticated catalog put status=%d", resp.StatusCode)
	}
	for _, token := range []string{"test-token", "peer-token"} {
		resp = put(token, row)
		if resp.StatusCode != http.StatusForbidden || benchJSON(t, resp)["error"] != "operator_required" {
			t.Fatalf("client-token catalog put status=%d", resp.StatusCode)
		}
	}
	resp = benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/catalog", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Fatalf("unauthenticated catalog get status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Required provenance and closed vocabularies.
	missingDecidedBy := benchCatalogRow("bench-cat-nodecided", "", "codex", "A", nil)
	delete(missingDecidedBy, "decided_by")
	resp = put("operator-token", missingDecidedBy)
	if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "decided_by_required" {
		t.Fatalf("missing decided_by status=%d", resp.StatusCode)
	}
	blankDecidedBy := benchCatalogRow("bench-cat-nodecided", "", "codex", "A", map[string]any{"decided_by": "  "})
	resp = put("operator-token", blankDecidedBy)
	if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "decided_by_required" {
		t.Fatalf("blank decided_by status=%d", resp.StatusCode)
	}
	missingRef := benchCatalogRow("bench-cat-noref", "", "codex", "A", nil)
	delete(missingRef, "deviation_ref")
	resp = put("operator-token", missingRef)
	if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "deviation_ref_required" {
		t.Fatalf("missing deviation_ref status=%d", resp.StatusCode)
	}
	for _, invalid := range []map[string]any{
		benchCatalogRow("bench-cat-badgrade", "", "codex", "Z", nil),
		benchCatalogRow("bench-cat-badgate", "", "codex", "A", map[string]any{"gate": "sometimes"}),
		benchCatalogRow("bench-cat-nopool", "", "", "A", nil),
	} {
		resp = put("operator-token", invalid)
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Fatalf("invalid catalog row %v status=%d", invalid["profile"], resp.StatusCode)
		}
		resp.Body.Close()
	}

	// The Sol rule moved to the server: Sol profiles accept only S+.
	resp = put("operator-token", benchCatalogRow("codex-sol", "max", "codex", "A+", nil))
	if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "bench_catalog_sol_grade" {
		t.Fatalf("sol non-S+ status=%d", resp.StatusCode)
	}

	// Monotonicity is enforced on the merged state, and the batch is atomic:
	// the violating pair rolls back together with the innocent mirror row.
	resp = put("operator-token", benchCatalogRow("bench-cat-mono", "low", "codex", "A", nil))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("monotonic base put status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = put("operator-token",
		benchCatalogRow("bench-cat-mono-mirror", "", "codex", "A", nil),
		benchCatalogRow("bench-cat-mono", "high", "codex", "B", nil))
	if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "bench_catalog_not_monotonic" {
		t.Fatalf("monotonic violation status=%d", resp.StatusCode)
	}
	if got := benchCount(t, p, `SELECT count(*) FROM bench_catalog WHERE profile IN ('bench-cat-mono-mirror','bench-cat-mono') AND effort IN ('','high')`); got != 0 {
		t.Fatalf("violating batch was not atomic, catalog rows=%d", got)
	}
	if got := benchCount(t, p, `SELECT count(*) FROM bench_grades WHERE profile='bench-cat-mono-mirror'`); got != 0 {
		t.Fatalf("violating batch leaked a grades mirror row=%d", got)
	}
	resp = put("operator-token", benchCatalogRow("bench-cat-mono", "high", "codex", "A+", nil))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("monotonic improvement status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Seed the catalog: a per-pool ladder with a consult_only row and a
	// retired row.
	seed := []map[string]any{
		benchCatalogRow("bench-cat-apex", "", "codex", "S+", nil),
		benchCatalogRow("bench-cat-apex", "high", "codex", "S+", map[string]any{"score": 67.0}),
		benchCatalogRow("bench-cat-mid", "high", "codex", "A+", nil),
		benchCatalogRow("bench-cat-low", "medium", "codex", "A", nil),
		benchCatalogRow("bench-cat-fable", "", "codex", "S", map[string]any{"gate": "consult_only", "gate_reason": "subscription advisory only"}),
		benchCatalogRow("bench-cat-opus", "", "claude", "S+", nil),
		benchCatalogRow("bench-cat-rungs", "medium", "claude", "S", nil),
		benchCatalogRow("bench-cat-rungs", "high", "claude", "S", nil),
		benchCatalogRow("bench-cat-retired", "", "codex", "B", map[string]any{"retired_at": "2026-09-20T00:00:00Z"}),
	}
	resp = put("operator-token", seed...)
	if resp.StatusCode != http.StatusOK || benchJSON(t, resp)["upserted"] != float64(len(seed)) {
		t.Fatalf("seed put status=%d", resp.StatusCode)
	}
	resp = put("operator-token", seed...)
	if resp.StatusCode != http.StatusOK || benchJSON(t, resp)["upserted"] != float64(len(seed)) || benchCount(t, p, `SELECT count(*) FROM bench_catalog WHERE profile LIKE 'bench-cat-%'`) != len(seed)+2 {
		t.Fatalf("idempotent seed status=%d", resp.StatusCode)
	}

	// Full listing keeps consult_only and drops retired; the pool ladder drops
	// consult_only too and sorts grade-descending, profile, effort rung.
	full := benchCatalogGet(t, h, "")
	foundFable, foundRetired := false, false
	for _, x := range full {
		if x["profile"] == "bench-cat-fable" {
			foundFable = true
		}
		if x["profile"] == "bench-cat-retired" {
			foundRetired = true
		}
	}
	if !foundFable || foundRetired {
		t.Fatalf("full catalog consult_only=%t retired=%t", foundFable, foundRetired)
	}
	ladder := benchCatalogGet(t, h, "?pool=codex")
	var ladderKeys []string
	for _, x := range ladder {
		ladderKeys = append(ladderKeys, fmt.Sprintf("%s/%s/%s", x["profile"], x["effort"], x["grade"]))
	}
	want := []string{"bench-cat-apex//S+", "bench-cat-apex/high/S+", "bench-cat-mid/high/A+", "bench-cat-mono/high/A+", "bench-cat-low/medium/A", "bench-cat-mono/low/A"}
	if fmt.Sprint(ladderKeys) != fmt.Sprint(want) {
		t.Fatalf("codex ladder=%v want=%v", ladderKeys, want)
	}
	withRetired := benchCatalogGet(t, h, "?pool=codex&include_retired=1")
	if len(withRetired) != len(ladder)+1 || withRetired[len(withRetired)-1]["profile"] != "bench-cat-retired" {
		t.Fatalf("include_retired rows=%v", withRetired)
	}
	// Same-grade rungs of one profile order by rung, not alphabetically:
	// medium precedes high here even though "high" < "medium" as strings.
	claudeLadder := benchCatalogGet(t, h, "?pool=claude")
	var claudeKeys []string
	for _, x := range claudeLadder {
		claudeKeys = append(claudeKeys, fmt.Sprintf("%s/%s/%s", x["profile"], x["effort"], x["grade"]))
	}
	wantClaude := []string{"bench-cat-opus//S+", "bench-cat-rungs/medium/S", "bench-cat-rungs/high/S"}
	if fmt.Sprint(claudeKeys) != fmt.Sprint(wantClaude) {
		t.Fatalf("claude ladder=%v want=%v", claudeKeys, wantClaude)
	}

	// Retired rungs do not constrain monotonicity: a retired high=B must not
	// block a live low=A on the same profile.
	resp = put("operator-token", benchCatalogRow("bench-cat-retmono", "high", "misc", "B", map[string]any{"retired_at": "2026-09-24T00:00:00Z"}))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("retired rung put status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = put("operator-token", benchCatalogRow("bench-cat-retmono", "low", "misc", "A", nil))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("retired rung must not constrain monotonicity status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// A legacy grades write updates an existing catalog default row in place:
	// catalog-only fields (model_id, pool, score) survive the mirror.
	resp = put("operator-token", benchCatalogRow("bench-cat-legupd", "", "misc", "A", map[string]any{"score": 50.0}))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("catalog default put status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/grades", "test-token", benchBody(t, "grades", map[string]any{
		"profile": "bench-cat-legupd", "grade": "C", "deviation_ref": "deviation-compat-592",
	}))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("legacy update status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	var legGrade, legModel, legPool string
	var legScore float64
	if err := p.QueryRow(t.Context(), `SELECT grade,model_id,pool,score FROM bench_catalog WHERE profile='bench-cat-legupd' AND effort=''`).Scan(&legGrade, &legModel, &legPool, &legScore); err != nil || legGrade != "C" || legModel != "m-bench-cat-legupd" || legPool != "misc" || legScore != 50 {
		t.Fatalf("legacy update lost catalog fields grade=%q model_id=%q pool=%q score=%v err=%v", legGrade, legModel, legPool, legScore, err)
	}

	// Compatibility projection: catalog effort='' rows surface on the legacy
	// grades route with the unchanged response shape, and retiring hides them.
	resp = benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/grades", "peer-token", nil)
	var grades struct {
		Grades []map[string]any `json:"grades"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&grades) != nil {
		resp.Body.Close()
		t.Fatalf("grades projection status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	legacy := map[string]map[string]any{}
	for _, g := range grades.Grades {
		legacy[g["profile"].(string)] = g
		for key := range g {
			switch key {
			case "profile", "grade", "boundary_version", "deviation_ref", "decided_at", "decided_by":
			default:
				t.Fatalf("legacy grades shape changed: unexpected key %s", key)
			}
		}
	}
	if legacy["bench-cat-apex"] == nil || legacy["bench-cat-apex"]["grade"] != "S+" || legacy["bench-cat-apex"]["decided_by"] != "ops-review-592" {
		t.Fatalf("catalog row missing from grades projection: %v", legacy["bench-cat-apex"])
	}
	if legacy["bench-cat-retired"] != nil {
		t.Fatalf("retired catalog row leaked into grades projection")
	}
	// The legacy write path enforces the same Sol rule as the catalog — the
	// mirror must not admit rows the catalog would reject.
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/grades", "test-token", benchBody(t, "grades", map[string]any{
		"profile": "codex-sol", "grade": "A", "deviation_ref": "deviation-compat-592",
	}))
	if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "bench_catalog_sol_grade" {
		t.Fatalf("legacy sol bypass status=%d", resp.StatusCode)
	}
	// The legacy write path mirrors into the catalog's default row.
	resp = benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/grades", "test-token", benchBody(t, "grades", map[string]any{
		"profile": "bench-grade-cat-compat", "grade": "A", "boundary_version": "2026-09-23",
		"deviation_ref": "deviation-compat-592", "decided_at": "2026-09-23T00:00:00Z", "decided_by": "spoofed",
	}))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("compat grades put status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	var effort, decidedBy string
	if err := p.QueryRow(t.Context(), `SELECT effort,decided_by FROM bench_catalog WHERE profile='bench-grade-cat-compat'`).Scan(&effort, &decidedBy); err != nil || effort != "" || decidedBy != "bench-client" {
		t.Fatalf("legacy write mirror effort=%q decided_by=%q err=%v", effort, decidedBy, err)
	}
	// Retiring through the catalog removes the legacy row.
	resp = put("operator-token", benchCatalogRow("bench-cat-apex", "", "codex", "S+", map[string]any{"retired_at": "2026-09-24T00:00:00Z"}))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("retire put status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	if got := benchCount(t, p, `SELECT count(*) FROM bench_grades WHERE profile='bench-cat-apex'`); got != 0 {
		t.Fatalf("retire did not clear legacy row=%d", got)
	}
	resp = benchRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/bench/grades", "peer-token", nil)
	grades.Grades = nil
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&grades) != nil {
		resp.Body.Close()
		t.Fatalf("grades after retire status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	for _, g := range grades.Grades {
		if g["profile"] == "bench-cat-apex" {
			t.Fatalf("retired profile still in grades projection")
		}
	}
}

// benchSeedGolden loads the golden copy of the scopefuel catalog seed,
// stripping the // header comment the file carries (JSON has no comment
// syntax, so the rev and provenance note live in comment lines).
func benchSeedGolden(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/bench_catalog_seed_scopefuel_3759199.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "//") {
		lines = lines[1:]
	}
	return []byte(strings.Join(lines, "\n"))
}

// TestBenchCatalogScopefuelSeed replays the operator-approved scopefuel
// catalog seed (rev 3759199, the handoffkeep #955 step-2 activation body)
// through the real PUT handler. Its two codex-sol E6 measurement rungs ride
// the placeholder exception — grade C with score null makes no grade claim —
// while every Sol row that does claim a grade below S+ still fails, and a
// date-only decided_at fails decode with the named catalog error rather than
// invalid_context.
func TestBenchCatalogScopefuelSeed(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	benchClean(t, p)
	h := benchServer(s)
	defer h.Close()

	golden := benchSeedGolden(t)
	var seed struct {
		Catalog []map[string]any `json:"catalog"`
	}
	if err := json.Unmarshal(golden, &seed); err != nil {
		t.Fatalf("golden seed does not parse: %v", err)
	}
	if len(seed.Catalog) != 59 {
		t.Fatalf("golden seed rows=%d want=59", len(seed.Catalog))
	}
	// The seed carries real profile names; clean them before and after so the
	// shared test database sees no leftovers whichever way the test ends.
	// context.Background because t.Context() is already canceled by the time
	// Cleanup functions run.
	profiles := make([]string, 0, len(seed.Catalog))
	for _, row := range seed.Catalog {
		profiles = append(profiles, row["profile"].(string))
	}
	cleanSeed := func() {
		for _, query := range []string{
			`DELETE FROM bench_catalog WHERE profile = ANY($1)`,
			`DELETE FROM bench_grades WHERE profile = ANY($1)`,
		} {
			if _, err := p.Exec(context.Background(), query, profiles); err != nil {
				t.Fatal(err)
			}
		}
	}
	cleanSeed()
	t.Cleanup(cleanSeed)

	put := func(body []byte) *http.Response {
		return benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/catalog", "operator-token", body)
	}

	// The whole seed is accepted as one batch.
	resp := put(golden)
	if resp.StatusCode != http.StatusOK || benchJSON(t, resp)["upserted"] != float64(len(seed.Catalog)) {
		t.Fatalf("scopefuel seed put status=%d", resp.StatusCode)
	}
	// A read-back lists the two codex-sol placeholder rungs unchanged: grade C
	// with score null at the medium and high efforts, alongside the scored S+
	// rungs the seed also carries.
	solRows := map[string]map[string]any{}
	for _, x := range benchCatalogGet(t, h, "") {
		if x["profile"] == "codex-sol" {
			solRows[x["effort"].(string)] = x
		}
	}
	for _, effort := range []string{"medium", "high"} {
		row := solRows[effort]
		if row == nil || row["grade"] != "C" || row["score"] != nil {
			t.Fatalf("codex-sol %s placeholder changed on write: %v", effort, row)
		}
	}
	for _, effort := range []string{"xhigh", "max"} {
		if row := solRows[effort]; row == nil || row["grade"] != "S+" {
			t.Fatalf("codex-sol %s rung changed on write: %v", effort, row)
		}
	}

	// The exception covers only rows that make no grade claim. A scored C, an
	// unscored A, and — on the second Sol profile — a scored S each claim a
	// grade below S+ and must still fail.
	for _, row := range []map[string]any{
		benchCatalogRow("codex-sol", "low", "codex", "C", map[string]any{"score": 10.0}),
		benchCatalogRow("codex-sol", "low", "codex", "A", nil),
		benchCatalogRow("kiro-sol", "low", "kiro", "S", map[string]any{"score": 60.0}),
	} {
		resp = put(benchCatalogBody(t, row))
		if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "bench_catalog_sol_grade" {
			t.Fatalf("sol grade claim %s/%s status=%d", row["profile"], row["grade"], resp.StatusCode)
		}
	}
	// The placeholder itself round-trips: an unscored C on the other Sol
	// profile is accepted the same way the seed rows are.
	resp = put(benchCatalogBody(t, benchCatalogRow("kiro-sol", "low", "kiro", "C", nil)))
	if resp.StatusCode != http.StatusOK || benchJSON(t, resp)["upserted"] != float64(1) {
		t.Fatalf("kiro-sol placeholder status=%d", resp.StatusCode)
	}

	// The raw emit's date-only decided_at fails decode with the named error,
	// and the detail names the field — never a bare invalid_context.
	raw := bytes.Replace(golden, []byte(`"decided_at": "2026-09-27T00:00:00Z"`), []byte(`"decided_at": "2026-09-27"`), 1)
	if !bytes.Contains(raw, []byte(`"decided_at": "2026-09-27"`)) {
		t.Fatal("raw seed variant construction failed")
	}
	resp = put(raw)
	body := benchJSON(t, resp)
	if resp.StatusCode != http.StatusBadRequest || body["error"] != "invalid_bench_catalog_json" {
		t.Fatalf("date-only decided_at status=%d body=%v", resp.StatusCode, body)
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "decided_at") {
		t.Fatalf("date-only decided_at detail=%q does not name the field", detail)
	}
	// An unknown field fails the same named way, with the field named.
	unknown := benchCatalogRow("bench-cat-unknown", "", "codex", "A", map[string]any{"surprise": 1})
	resp = put(benchCatalogBody(t, unknown))
	body = benchJSON(t, resp)
	if resp.StatusCode != http.StatusBadRequest || body["error"] != "invalid_bench_catalog_json" || !strings.Contains(fmt.Sprint(body["detail"]), "surprise") {
		t.Fatalf("unknown field status=%d body=%v", resp.StatusCode, body)
	}
	// The batch-size failure keeps its own name.
	resp = put([]byte(`{"catalog":[]}`))
	if resp.StatusCode != http.StatusBadRequest || benchJSON(t, resp)["error"] != "invalid_context" {
		t.Fatalf("empty batch status=%d", resp.StatusCode)
	}
}

// Task hk#1384: shared-token hosts assign per-host origin ids, so
// (created_by, origin_id) collisions are routine. The reps write path is
// insert-only: a same-key write with identical content is an idempotent
// resend and returns the stored server id; any differing field is a 409 that
// rejects the whole batch without touching the stored row.
func TestBenchRepsConflictContract(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	benchClean(t, p)
	h := benchServer(s)
	defer h.Close()

	putReps := func(items []map[string]any) *http.Response {
		t.Helper()
		body, err := json.Marshal(map[string]any{"reps": items})
		if err != nil {
			t.Fatal(err)
		}
		return benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/reps", "test-token", body)
	}
	rep := func(origin int64) map[string]any {
		item := benchItem(t, benchRepFixture, "reps")
		item["origin_id"] = float64(origin)
		return item
	}
	seqLast := func() int64 {
		t.Helper()
		var n int64
		if err := p.QueryRow(t.Context(), `SELECT last_value FROM bench_reps_id_seq`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// ids come back aligned to the input order.
	batch := []map[string]any{rep(570), rep(571), rep(572)}
	resp := putReps(batch)
	body := benchJSON(t, resp)
	ids, _ := body["ids"].([]any)
	if resp.StatusCode != http.StatusOK || body["upserted"] != float64(3) || len(ids) != 3 {
		t.Fatalf("batch put status=%d body=%v", resp.StatusCode, body)
	}
	seenIDs := map[float64]bool{}
	for i, item := range batch {
		row := benchRepRow(t, p, int64(item["origin_id"].(float64)), "bench-client")
		if row == nil || row["id"].(int64) != int64(ids[i].(float64)) {
			t.Fatalf("ids[%d]=%v does not name origin_id=%v row=%v", i, ids[i], item["origin_id"], row)
		}
		if seenIDs[ids[i].(float64)] {
			t.Fatalf("duplicate id in ids=%v", ids)
		}
		seenIDs[ids[i].(float64)] = true
	}
	before := benchRepRow(t, p, 570, "bench-client")
	beforeID := float64(before["id"].(int64))

	// Idempotent resend: identical content returns the stored id. Also sent
	// with recorded_at spelled in a different offset — same instant, same id.
	resend := rep(570)
	resend["recorded_at"] = "2026-09-01T12:00:00+02:00"
	resp = putReps([]map[string]any{resend})
	body = benchJSON(t, resp)
	resendIDs, _ := body["ids"].([]any)
	if resp.StatusCode != http.StatusOK || len(resendIDs) != 1 || resendIDs[0] != beforeID {
		t.Fatalf("resend status=%d body=%v want id=%v", resp.StatusCode, body, beforeID)
	}
	if benchCount(t, p, `SELECT count(*) FROM bench_reps WHERE origin_id=570 AND created_by='bench-client'`) != 1 {
		t.Fatal("resend inserted a duplicate row")
	}

	// Every carried field participates in the identity comparison: a write
	// differing in exactly one field must still 409, and the stored row must
	// stay byte-identical.
	mutations := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"profile", func(m map[string]any) { m["profile"] = "codex-luna" }},
		{"model_id", func(m map[string]any) { m["model_id"] = "gpt-5.6-luna" }},
		{"task_ref", func(m map[string]any) { m["task_ref"] = "PR#43" }},
		{"tier", func(m map[string]any) { m["tier"] = "T3" }},
		{"role", func(m map[string]any) { m["role"] = "verify" }},
		{"rounds", func(m map[string]any) { m["rounds"] = float64(2) }},
		{"blockers_found", func(m map[string]any) { m["blockers_found"] = float64(1) }},
		{"completed", func(m map[string]any) { m["completed"] = float64(0) }},
		{"input_tokens", func(m map[string]any) { m["input_tokens"] = float64(1) }},
		{"output_tokens", func(m map[string]any) { m["output_tokens"] = float64(1) }},
		{"notes", func(m map[string]any) { m["notes"] = "different" }},
		{"notes_null", func(m map[string]any) { delete(m, "notes") }},
		{"recorded_at", func(m map[string]any) { m["recorded_at"] = "2026-09-01T10:00:01Z" }},
		{"effort", func(m map[string]any) { m["effort"] = "high" }},
		{"grade", func(m map[string]any) { m["grade"] = "A" }},
		{"table_grade", func(m map[string]any) { m["table_grade"] = "B" }},
		{"grade_null", func(m map[string]any) { delete(m, "grade") }},
	}
	for _, mutation := range mutations {
		mutated := rep(570)
		mutation.mutate(mutated)
		resp = putReps([]map[string]any{mutated})
		body = benchJSON(t, resp)
		if resp.StatusCode != http.StatusConflict || body["error"] != "bench_rep_conflict" {
			t.Fatalf("%s mutation status=%d body=%v want=409", mutation.name, resp.StatusCode, body)
		}
		list, _ := body["conflicts"].([]any)
		if len(list) != 1 || list[0].(map[string]any)["index"] != float64(0) || list[0].(map[string]any)["origin_id"] != float64(570) || list[0].(map[string]any)["conflict_server_id"] != beforeID {
			t.Fatalf("%s mutation conflicts=%v want index=0 origin_id=570 conflict_server_id=%v", mutation.name, body["conflicts"], beforeID)
		}
		if after := benchRepRow(t, p, 570, "bench-client"); !sameRepRow(after, before) {
			t.Fatalf("%s mutation changed the stored row: %v -> %v", mutation.name, before, after)
		}
	}

	// One conflicting row rejects the whole batch atomically: nothing new is
	// written, and the response names every conflicting row.
	beforeSeq := seqLast()
	okRep := rep(573)
	conflictA, conflictB := rep(570), rep(571)
	conflictA["notes"] = "clobber A"
	conflictB["notes"] = "clobber B"
	resp = putReps([]map[string]any{okRep, conflictA, conflictB})
	body = benchJSON(t, resp)
	if resp.StatusCode != http.StatusConflict || body["error"] != "bench_rep_conflict" {
		t.Fatalf("mixed batch status=%d body=%v want=409", resp.StatusCode, body)
	}
	list, _ := body["conflicts"].([]any)
	if len(list) != 2 {
		t.Fatalf("mixed batch conflicts=%v want two rows named", body["conflicts"])
	}
	gotOrigins := map[float64]float64{}
	for _, c := range list {
		e := c.(map[string]any)
		gotOrigins[e["origin_id"].(float64)] = e["index"].(float64)
	}
	if gotOrigins[570] != 1 || gotOrigins[571] != 2 {
		t.Fatalf("mixed batch conflicts=%v want origin 570 at index 1 and 571 at index 2", body["conflicts"])
	}
	if benchCount(t, p, `SELECT count(*) FROM bench_reps WHERE origin_id=573 AND created_by='bench-client'`) != 0 {
		t.Fatal("rejected batch still wrote its non-conflicting row")
	}
	// Rejected inserts still consume BIGSERIAL candidates: id gaps are the
	// visible trace of refused collisions, not lost rows.
	if after := seqLast(); after-beforeSeq < 3 {
		t.Fatalf("sequence did not advance past the rejected batch: %d -> %d", beforeSeq, after)
	}

	// An old client that reads only "upserted" is unaffected: the field is
	// still present and counts accepted rows (including idempotent resends).
	resp = putReps([]map[string]any{rep(570)})
	body = benchJSON(t, resp)
	if resp.StatusCode != http.StatusOK || body["upserted"] != float64(1) {
		t.Fatalf("legacy upserted field status=%d body=%v", resp.StatusCode, body)
	}
}

// Two transactions racing on the same (created_by, origin_id) key: identical
// payloads both return the same server id; different payloads let exactly one
// write win and the loser takes *BenchRepConflictError naming the winner's id.
// The conflict check rides the ON CONFLICT row lock, so the loser's content
// comparison is evaluated against the winner's committed row.
func TestBenchRepsConcurrentSameKey(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	benchClean(t, p)

	recordedAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	raceRep := func(origin int64, notes string) store.BenchRep {
		model, taskRef, tier, role, grade := "gpt-5.6-race", "hk:task/9999", "T2", "impl", "A"
		rounds := 1
		return store.BenchRep{
			OriginID:   origin,
			Profile:    "race-impl",
			ModelID:    &model,
			TaskRef:    &taskRef,
			Tier:       &tier,
			Role:       &role,
			Rounds:     &rounds,
			Notes:      &notes,
			RecordedAt: recordedAt,
			Grade:      &grade,
			CreatedBy:  "race-client",
		}
	}
	type outcome struct {
		ids []int64
		err error
	}
	race := func(a, b store.BenchRep) (outcome, outcome) {
		t.Helper()
		start := make(chan struct{})
		out := make(chan outcome, 2)
		var wg sync.WaitGroup
		for _, rep := range []store.BenchRep{a, b} {
			wg.Add(1)
			go func(r store.BenchRep) {
				defer wg.Done()
				<-start
				ids, err := s.UpsertBenchReps(context.Background(), []store.BenchRep{r})
				out <- outcome{ids: ids, err: err}
			}(rep)
		}
		close(start)
		wg.Wait()
		x, y := <-out, <-out
		return x, y
	}

	// Identical payloads: both racers get the same id, one row total.
	resA, resB := race(raceRep(580, "identical"), raceRep(580, "identical"))
	if resA.err != nil || resB.err != nil {
		t.Fatalf("identical race errors: %v / %v", resA.err, resB.err)
	}
	if len(resA.ids) != 1 || len(resB.ids) != 1 || resA.ids[0] != resB.ids[0] {
		t.Fatalf("identical race ids: %v / %v", resA.ids, resB.ids)
	}
	if benchCount(t, p, `SELECT count(*) FROM bench_reps WHERE origin_id=580 AND created_by='race-client'`) != 1 {
		t.Fatal("identical race wrote more than one row")
	}

	// Different payloads: exactly one wins; the loser gets a conflict naming
	// the winner's server id, and the stored row holds the winner's content.
	win, lose := race(raceRep(581, "alpha content"), raceRep(581, "beta content"))
	var winner, loser outcome
	switch {
	case win.err == nil && lose.err != nil:
		winner, loser = win, lose
	case lose.err == nil && win.err != nil:
		winner, loser = lose, win
	default:
		t.Fatalf("different-payload race: win=%v/%v lose=%v/%v", win.ids, win.err, lose.ids, lose.err)
	}
	var repErr *store.BenchRepConflictError
	if !errors.As(loser.err, &repErr) || len(repErr.Conflicts) != 1 {
		t.Fatalf("loser error %v is not one BenchRepConflict", loser.err)
	}
	if repErr.Conflicts[0].Index != 0 || repErr.Conflicts[0].OriginID != 581 || repErr.Conflicts[0].ServerID != winner.ids[0] {
		t.Fatalf("loser conflict=%v want index=0 origin_id=581 conflict_server_id=%d", repErr.Conflicts[0], winner.ids[0])
	}
	row := benchRepRow(t, p, 581, "race-client")
	if row == nil || row["id"].(int64) != winner.ids[0] {
		t.Fatalf("winner row=%v want id=%d", row, winner.ids[0])
	}
}

// A (created_by, origin_id) key repeated inside one batch is a resend only
// when the repeated rows are identical: differing repeats are an invalid
// batch rejected before the transaction opens, so no row lands and no
// conflict_server_id is coined for an insert that rolls back with the batch.
func TestBenchRepsIntraBatchDuplicateKey(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	benchClean(t, p)
	if _, err := p.Exec(t.Context(), `DELETE FROM bench_reps WHERE created_by IN ('bench-client','dup-client') AND origin_id BETWEEN 590 AND 599`); err != nil {
		t.Fatal(err)
	}
	h := benchServer(s)
	defer h.Close()

	putReps := func(items []map[string]any) *http.Response {
		t.Helper()
		body, err := json.Marshal(map[string]any{"reps": items})
		if err != nil {
			t.Fatal(err)
		}
		return benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/reps", "test-token", body)
	}
	rep := func(origin int64) map[string]any {
		item := benchItem(t, benchRepFixture, "reps")
		item["origin_id"] = float64(origin)
		return item
	}

	// Identical repeats are resends: both rows return the stored id and
	// exactly one row lands.
	resp := putReps([]map[string]any{rep(595), rep(595)})
	body := benchJSON(t, resp)
	ids, _ := body["ids"].([]any)
	if resp.StatusCode != http.StatusOK || len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("identical repeats status=%d body=%v", resp.StatusCode, body)
	}
	if benchCount(t, p, `SELECT count(*) FROM bench_reps WHERE origin_id=595 AND created_by='bench-client'`) != 1 {
		t.Fatal("identical repeats inserted more than one row")
	}

	// Interleaved identical repeats keep ids aligned to the input order.
	resp = putReps([]map[string]any{rep(596), rep(597), rep(596), rep(597)})
	body = benchJSON(t, resp)
	ids, _ = body["ids"].([]any)
	if resp.StatusCode != http.StatusOK || len(ids) != 4 || ids[0] != ids[2] || ids[1] != ids[3] || ids[0] == ids[1] {
		t.Fatalf("interleaved repeats status=%d body=%v", resp.StatusCode, body)
	}

	// Repeats that spell the same stored instant — a different offset, and
	// sub-microsecond detail timestamptz drops — still dedup to one row.
	a, b := rep(598), rep(598)
	a["recorded_at"] = "2026-09-01T10:00:00.123456Z"
	b["recorded_at"] = "2026-09-01T12:00:00.123456789+02:00"
	resp = putReps([]map[string]any{a, b})
	body = benchJSON(t, resp)
	ids, _ = body["ids"].([]any)
	if resp.StatusCode != http.StatusOK || len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("instant-equal repeats status=%d body=%v", resp.StatusCode, body)
	}
	if benchCount(t, p, `SELECT count(*) FROM bench_reps WHERE origin_id=598 AND created_by='bench-client'`) != 1 {
		t.Fatal("instant-equal repeats inserted more than one row")
	}

	// Differing repeats are an invalid batch: 400, nothing written — not a
	// 409 naming a conflict_server_id that rolls back with the batch.
	x, y := rep(592), rep(592)
	x["notes"], y["notes"] = "one", "two"
	resp = putReps([]map[string]any{x, y})
	body = benchJSON(t, resp)
	if resp.StatusCode != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("differing repeats status=%d body=%v want=400 invalid_context", resp.StatusCode, body)
	}
	if benchCount(t, p, `SELECT count(*) FROM bench_reps WHERE origin_id=592 AND created_by='bench-client'`) != 0 {
		t.Fatal("differing repeats wrote rows")
	}

	// The whole batch is rejected: the fresh row ahead of the differing
	// pair does not land either.
	c, d := rep(594), rep(594)
	d["notes"] = "changed"
	resp = putReps([]map[string]any{rep(593), c, d})
	body = benchJSON(t, resp)
	if resp.StatusCode != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("mixed differing batch status=%d body=%v want=400 invalid_context", resp.StatusCode, body)
	}
	if benchCount(t, p, `SELECT count(*) FROM bench_reps WHERE origin_id IN (593,594) AND created_by='bench-client'`) != 0 {
		t.Fatal("rejected batch wrote its non-repeated row")
	}

	// The store error names both offending positions and is not the
	// conflict path — a repeat differs from a row the batch itself wrote,
	// never from a stored row.
	notes := func(v string) *string { return &v }
	repRow := func(origin int64, n *string) store.BenchRep {
		return store.BenchRep{OriginID: origin, Profile: "dup-prof", Notes: n, RecordedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC), CreatedBy: "dup-client"}
	}
	_, err := s.UpsertBenchReps(t.Context(), []store.BenchRep{repRow(591, notes("a")), repRow(590, notes("a")), repRow(591, notes("b"))})
	if err == nil || !strings.Contains(err.Error(), "rows 0 and 2") {
		t.Fatalf("differing repeat error=%v want rows 0 and 2 named", err)
	}
	var conflictErr *store.BenchRepConflictError
	if errors.As(err, &conflictErr) {
		t.Fatalf("differing repeat surfaced as conflict error %v", err)
	}
}

// Every rejected row of a 409 batch is logged exactly once, with the fields
// needed to find the colliding stored row: created_by, the row's index in
// the request, its origin_id, and conflict_server_id.
func TestBenchRepsConflictLogged(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	benchClean(t, p)
	if _, err := p.Exec(t.Context(), `DELETE FROM bench_reps WHERE created_by='bench-client' AND origin_id BETWEEN 585 AND 589`); err != nil {
		t.Fatal(err)
	}
	h := benchServer(s)
	defer h.Close()

	putReps := func(items []map[string]any) *http.Response {
		t.Helper()
		body, err := json.Marshal(map[string]any{"reps": items})
		if err != nil {
			t.Fatal(err)
		}
		return benchRequest(t, h.Client(), http.MethodPut, h.URL+"/v1/bench/reps", "test-token", body)
	}
	rep := func(origin int64) map[string]any {
		item := benchItem(t, benchRepFixture, "reps")
		item["origin_id"] = float64(origin)
		return item
	}

	resp := putReps([]map[string]any{rep(585), rep(586)})
	body := benchJSON(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed status=%d body=%v", resp.StatusCode, body)
	}
	id585 := benchRepRow(t, p, 585, "bench-client")["id"].(int64)
	id586 := benchRepRow(t, p, 586, "bench-client")["id"].(int64)

	a, b := rep(585), rep(586)
	a["notes"], b["notes"] = "clobber a", "clobber b"
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	resp = putReps([]map[string]any{rep(587), a, b})
	log.SetOutput(old)
	body = benchJSON(t, resp)
	if resp.StatusCode != http.StatusConflict || body["error"] != "bench_rep_conflict" {
		t.Fatalf("conflict batch status=%d body=%v want=409", resp.StatusCode, body)
	}
	got := buf.String()
	if n := strings.Count(got, "bench_rep_conflict"); n != 2 {
		t.Fatalf("log lines for two conflicts=%d want 2:\n%s", n, got)
	}
	for _, want := range []string{
		fmt.Sprintf("created_by=bench-client index=1 origin_id=585 conflict_server_id=%d", id585),
		fmt.Sprintf("created_by=bench-client index=2 origin_id=586 conflict_server_id=%d", id586),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("log missing %q in:\n%s", want, got)
		}
	}
}

// Two batches covering the same keys in opposite orders deadlock; the loser
// is aborted by Postgres (SQLSTATE 40P01) and must surface as a retryable
// 503 — not a 400 that wrongly blames the rows. The deadlocked batch writes
// nothing, so resending the identical batch lands cleanly afterwards.
func TestBenchRepsDeadlockRetryable(t *testing.T) {
	s := benchStore(t)
	p := benchPool(t)
	benchClean(t, p)
	if _, err := p.Exec(t.Context(), `DELETE FROM bench_reps WHERE created_by='bench-client' AND origin_id >= 30000`); err != nil {
		t.Fatal(err)
	}
	h := benchServer(s)
	defer h.Close()

	rep := func(origin int64) map[string]any {
		item := benchItem(t, benchRepFixture, "reps")
		item["origin_id"] = float64(origin)
		return item
	}
	type outcome struct {
		status int
		errMsg string
		batch  []map[string]any
	}
	doPut := func(items []map[string]any) outcome {
		body, err := json.Marshal(map[string]any{"reps": items})
		if err != nil {
			return outcome{status: -1, errMsg: err.Error(), batch: items}
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, h.URL+"/v1/bench/reps", bytes.NewReader(body))
		if err != nil {
			return outcome{status: -1, errMsg: err.Error(), batch: items}
		}
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.Client().Do(req)
		if err != nil {
			return outcome{status: -1, errMsg: err.Error(), batch: items}
		}
		defer resp.Body.Close()
		var parsed map[string]any
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &parsed)
		return outcome{status: resp.StatusCode, errMsg: fmt.Sprint(parsed["error"]), batch: items}
	}

	const batchSize = 300
	saw503 := false
	for attempt := 0; attempt < 8 && !saw503; attempt++ {
		base := int64(30000 + attempt*batchSize)
		asc := make([]map[string]any, 0, batchSize)
		for i := 0; i < batchSize; i++ {
			asc = append(asc, rep(base+int64(i)))
		}
		desc := make([]map[string]any, 0, batchSize)
		for i := batchSize - 1; i >= 0; i-- {
			desc = append(desc, asc[i])
		}
		start := make(chan struct{})
		res := make(chan outcome, 2)
		go func() { <-start; res <- doPut(asc) }()
		go func() { <-start; res <- doPut(desc) }()
		close(start)
		first, second := <-res, <-res

		var loser outcome
		var ok bool
		switch {
		case first.status == http.StatusServiceUnavailable && second.status == http.StatusOK:
			loser, ok = first, true
		case second.status == http.StatusServiceUnavailable && first.status == http.StatusOK:
			loser, ok = second, true
		}
		if !ok {
			if first.status == http.StatusOK && second.status == http.StatusOK {
				continue // the batches interleaved without a deadlock this round
			}
			t.Fatalf("attempt %d: unexpected outcomes %d(%q) / %d(%q)", attempt, first.status, first.errMsg, second.status, second.errMsg)
		}
		saw503 = true
		if loser.errMsg != "bench_reps_retryable" {
			t.Fatalf("deadlocked batch status=%d error=%q want 503 bench_reps_retryable", loser.status, loser.errMsg)
		}
		// Exactly the winner's batch is on disk: the deadlocked batch wrote
		// nothing (its rows would have pushed the count to 2*batchSize).
		if n := benchCount(t, p, `SELECT count(*) FROM bench_reps WHERE created_by='bench-client' AND origin_id >= $1 AND origin_id < $2`, base, base+batchSize); n != batchSize {
			t.Fatalf("rows after deadlock=%d want %d (the winning batch only)", n, batchSize)
		}
		// The error is retryable: the identical batch lands on resend.
		if retry := doPut(loser.batch); retry.status != http.StatusOK {
			t.Fatalf("retry of deadlocked batch status=%d error=%q", retry.status, retry.errMsg)
		}
	}
	if !saw503 {
		t.Fatal("no deadlock observed in 8 opposite-order attempts")
	}
}
