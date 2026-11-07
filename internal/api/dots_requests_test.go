package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestDotsRequestAPI(t *testing.T) {
	_, url := testdb.Schema(t, "dots_request_api")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(Server{Tokens: Tokens{"caller": "test-token"}, Service: Service{Store: st}}.Handler())
	defer h.Close()
	input := map[string]any{"title": "Research", "body": "Investigate"}
	code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/dots/requests", "test-token", input)
	task, _ := out["task"].(map[string]any)
	if code != http.StatusCreated || task["lane"] != "dots" || task["parent_lane"] != "caller" || task["source"] != "dots" {
		t.Fatalf("status=%d body=%v", code, out)
	}
	code, _ = projectRequest(t, h.URL, http.MethodPost, "/v1/dots/requests", "", input)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", code)
	}
}
