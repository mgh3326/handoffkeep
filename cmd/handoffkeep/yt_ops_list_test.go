package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `youtrack ops` prints last_error for an op, and --kind reaches the server as
// the kind query parameter.
func TestYTOpsListPrintsLastErrorAndFiltersKind(t *testing.T) {
	t.Setenv("HANDOFFKEEP_URL", "")
	t.Setenv("HANDOFFKEEP_TOKEN", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/yt/ops" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("kind"); got != "issue_fields" {
			t.Errorf("kind query = %q, want issue_fields", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ops": []any{map[string]any{
			"id": 9, "kind": "issue_fields", "state": "rejected", "status": "rejected",
			"reason": "http_400", "last_error": "youtrack answered 400: bad field",
		}}, "next_after_id": nil})
	}))
	t.Cleanup(server.Close)

	var out strings.Builder
	if err := run([]string{"yt", "ops", "--state", "rejected", "--kind", "issue_fields", "--url", server.URL, "--token", "t"}, &out, io.Discard); err != nil {
		t.Fatalf("ops --kind: %v", err)
	}
	if !strings.Contains(out.String(), `"last_error":"youtrack answered 400: bad field"`) {
		t.Fatalf("ops output lacks last_error: %s", out.String())
	}
}

func TestYTOpsListRefusesUnknownKind(t *testing.T) {
	err := run([]string{"yt", "ops", "--kind", "not_a_kind"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--kind") {
		t.Fatalf("yt ops with an unknown kind: %v", err)
	}
}
