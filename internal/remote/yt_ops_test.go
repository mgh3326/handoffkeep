package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

func TestResolveYTOpUsesResolveEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/yt/ops/17/resolve" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		if in["applied"] != true || in["yt_id"] != "c-17" || in["by"] != "operator" {
			t.Fatalf("request body = %v", in)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"op_id": 17, "status": "applied", "state": "applied", "retry_after": "0s", "yt_id": "c-17"})
	}))
	defer server.Close()

	client := Client{URL: server.URL, Token: "token"}
	result, err := client.ResolveYTOp(context.Background(), 17, ytwrite.Resolution{Applied: true, YTID: "c-17", By: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if result.OpID != 17 || result.State != ytwrite.StateApplied || result.YTID != "c-17" {
		t.Fatalf("result = %+v", result)
	}
}
