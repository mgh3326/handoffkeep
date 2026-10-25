package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// TestBenchAreaExceptionsWireShape pins the request contract: GET carries
// area and include_retired only as query params, PUT sends the
// {"exceptions":[...]} envelope and returns the upserted count.
func TestBenchAreaExceptionsWireShape(t *testing.T) {
	var path, method string
	var body map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		if r.URL.RawQuery != "" {
			path += "?" + r.URL.RawQuery
		}
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			_, _ = w.Write([]byte(`{"upserted":2}`))
			return
		}
		_, _ = w.Write([]byte(`{"exceptions":[{"profile":"devin-swe2","effort":"high","area":"blender","action":"exclude","evidence_refs":["hk:task/1836"],"decision_ref":"hk:doc/decision/1","decided_by":"operator","decided_at":"2026-10-24T00:00:00Z","expires_with_model_id":"swe2-m1","retired_at":null}]}`))
	}))
	defer srv.Close()
	c := Client{URL: srv.URL, Token: "t"}

	xs, err := c.ListBenchAreaExceptions(context.Background(), "blender", true)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || !strings.HasPrefix(path, "/v1/bench/area-exceptions?") || !strings.Contains(path, "area=blender") || !strings.Contains(path, "include_retired=1") {
		t.Fatalf("list request %s %s", method, path)
	}
	if len(xs) != 1 || xs[0].Profile != "devin-swe2" || xs[0].Action != "exclude" || xs[0].ExpiresWithModelID != "swe2-m1" || len(xs[0].EvidenceRefs) != 1 || xs[0].RetiredAt != nil {
		t.Fatalf("list row=%+v", xs)
	}
	// No filters: bare path.
	if _, err = c.ListBenchAreaExceptions(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/bench/area-exceptions" {
		t.Fatalf("unfiltered list path=%s", path)
	}

	retired := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	n, err := c.PutBenchAreaExceptions(context.Background(), []store.BenchAreaException{
		{Profile: "devin-swe2", Effort: "high", Area: "blender", Action: "exclude", EvidenceRefs: []string{"hk:task/1"}, DecisionRef: "hk:doc/d", ExpiresWithModelID: "m"},
		{Profile: "codex-luna", Effort: "", Area: "docs", Action: "prefer", EvidenceRefs: []string{"srv:9"}, DecisionRef: "hk:doc/d", ExpiresWithModelID: "m2", RetiredAt: &retired},
	})
	if err != nil || n != 2 {
		t.Fatalf("put n=%d err=%v", n, err)
	}
	if method != http.MethodPut || path != "/v1/bench/area-exceptions" {
		t.Fatalf("put request %s %s", method, path)
	}
	var rows []map[string]any
	if err := json.Unmarshal(body["exceptions"], &rows); err != nil || len(rows) != 2 {
		t.Fatalf("put body=%v err=%v", body, err)
	}
	if rows[0]["profile"] != "devin-swe2" || rows[0]["retired_at"] != nil || rows[1]["retired_at"] == nil {
		t.Fatalf("put rows=%v", rows)
	}
}

// TestBenchAreaExceptionsOldServer maps a pre-route server (404/405) to the
// unsupported error and lets the operator-required 403 through untouched.
func TestBenchAreaExceptionsOldServer(t *testing.T) {
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer gone.Close()
	c := Client{URL: gone.URL, Token: "t"}
	if _, err := c.ListBenchAreaExceptions(context.Background(), "", false); err == nil || !strings.Contains(err.Error(), "bench_area_exceptions_unsupported") {
		t.Fatalf("404 list err=%v", err)
	}
	if _, err := c.PutBenchAreaExceptions(context.Background(), []store.BenchAreaException{{Profile: "p"}}); err == nil || !strings.Contains(err.Error(), "bench_area_exceptions_unsupported") {
		t.Fatalf("404 put err=%v", err)
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"operator_required"}`))
	}))
	defer denied.Close()
	c = Client{URL: denied.URL, Token: "t"}
	_, err := c.PutBenchAreaExceptions(context.Background(), []store.BenchAreaException{{Profile: "p"}})
	var he *HTTPError
	if err == nil || !errors.As(err, &he) || he.Status != http.StatusForbidden || he.Code != "operator_required" {
		t.Fatalf("403 put err=%v, want HTTPError operator_required", err)
	}
}
