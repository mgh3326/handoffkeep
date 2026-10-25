package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// benchAreaServer records requests and answers the area-exceptions routes
// with the minimal bodies the client expects.
func benchAreaServer(t *testing.T) (*httptest.Server, *[]relaneRequest) {
	t.Helper()
	var seen []relaneRequest
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := relaneRequest{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization")}
		if r.URL.RawQuery != "" {
			x.Path += "?" + r.URL.RawQuery
		}
		_ = json.NewDecoder(r.Body).Decode(&x.Body)
		seen = append(seen, x)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut:
			_, _ = w.Write([]byte(`{"upserted":1}`))
		default:
			_, _ = w.Write([]byte(`{"exceptions":[{"profile":"devin-swe2","effort":"high","area":"blender","action":"exclude","evidence_refs":["hk:task/1836"],"decision_ref":"hk:doc/d","decided_by":"operator","decided_at":"2026-10-24T00:00:00Z","expires_with_model_id":"m1","retired_at":null}]}`))
		}
	}))
	t.Cleanup(h.Close)
	return h, &seen
}

func benchAreaRun(t *testing.T, h *httptest.Server, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(append(args, "--url", h.URL, "--token", "tok"), &out, io.Discard)
	return out.String(), err
}

func TestBenchAreaExceptionsListCmd(t *testing.T) {
	h, seen := benchAreaServer(t)
	out, err := benchAreaRun(t, h, "bench", "area-exceptions", "list")
	if err != nil {
		t.Fatal(err)
	}
	req := (*seen)[0]
	if req.Method != "GET" || req.Path != "/v1/bench/area-exceptions" || req.Auth != "Bearer tok" {
		t.Fatalf("list request=%+v", req)
	}
	var got struct {
		Exceptions []map[string]any `json:"exceptions"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Exceptions) != 1 || got.Exceptions[0]["action"] != "exclude" {
		t.Fatalf("list output=%q err=%v", out, err)
	}

	*seen = nil
	if _, err = benchAreaRun(t, h, "bench", "area-exceptions", "list", "--area", "blender", "--include-retired"); err != nil {
		t.Fatal(err)
	}
	path := (*seen)[0].Path
	if !strings.Contains(path, "area=blender") || !strings.Contains(path, "include_retired=1") {
		t.Fatalf("filtered list path=%q", path)
	}
	// An out-of-vocabulary --area is refused locally, before any request.
	*seen = nil
	if _, err = benchAreaRun(t, h, "bench", "area-exceptions", "list", "--area", "Blender"); err == nil || !strings.Contains(err.Error(), "blender, unity") {
		t.Fatalf("bad --area err=%v, want refusal naming the vocabulary", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("refused list reached the server: %+v", *seen)
	}
}

func TestBenchAreaExceptionsPutCmd(t *testing.T) {
	h, seen := benchAreaServer(t)
	dir := t.TempDir()
	valid := filepath.Join(dir, "exceptions.json")
	body := `{"exceptions":[{"profile":"devin-swe2","effort":"high","area":"blender","action":"exclude","evidence_refs":["hk:task/1836"],"decision_ref":"hk:task/1944","expires_with_model_id":"m1"}]}`
	if err := os.WriteFile(valid, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := benchAreaRun(t, h, "bench", "area-exceptions", "put", "--file", valid)
	if err != nil {
		t.Fatal(err)
	}
	req := (*seen)[0]
	if req.Method != "PUT" || req.Path != "/v1/bench/area-exceptions" || req.Auth != "Bearer tok" {
		t.Fatalf("put request=%+v", req)
	}
	var wire map[string]any
	if err := json.Unmarshal([]byte(out), &wire); err != nil || wire["upserted"] != float64(1) {
		t.Fatalf("put output=%q err=%v", out, err)
	}
	var rows []map[string]any
	raw, err := json.Marshal(req.Body["exceptions"])
	if err != nil || json.Unmarshal(raw, &rows) != nil || len(rows) != 1 || rows[0]["area"] != "blender" {
		t.Fatalf("put body=%v", req.Body)
	}

	// A file with an unknown field is refused locally with the field named.
	*seen = nil
	unknown := filepath.Join(dir, "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"exceptions":[{"profile":"devin-swe2","effort":"","area":"docs","action":"prefer","evidence_refs":["srv:9"],"decision_ref":"hk:doc/d","expires_with_model_id":"m","bogus":1}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = benchAreaRun(t, h, "bench", "area-exceptions", "put", "--file", unknown); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown-field file err=%v, want the field named", err)
	}
	// A non-object or empty batch is refused locally as well.
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"exceptions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = benchAreaRun(t, h, "bench", "area-exceptions", "put", "--file", empty); err == nil {
		t.Fatal("empty batch accepted")
	}
	if len(*seen) != 0 {
		t.Fatalf("refused puts reached the server: %+v", *seen)
	}
	// A missing --file is a usage error.
	if _, err = benchAreaRun(t, h, "bench", "area-exceptions", "put"); err == nil {
		t.Fatal("put without --file accepted")
	}
}

// A non-operator credential gets the server's own 403 verbatim — the CLI
// never pre-approves the writer check locally.
func TestBenchAreaExceptionsPutForbidden(t *testing.T) {
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"operator_required"}`))
	}))
	t.Cleanup(denied.Close)
	dir := t.TempDir()
	f := filepath.Join(dir, "x.json")
	if err := os.WriteFile(f, []byte(`{"exceptions":[{"profile":"p","effort":"","area":"docs","action":"prefer","evidence_refs":["srv:9"],"decision_ref":"hk:doc/d","expires_with_model_id":"m"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := benchAreaRun(t, denied, "bench", "area-exceptions", "put", "--file", f); err == nil || !strings.Contains(err.Error(), "operator_required") {
		t.Fatalf("403 err=%v, want operator_required", err)
	}
}
