package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestDotsInboxReadsRoute(t *testing.T) {
	_, url := testdb.Schema(t, "dots_inbox_reads_api")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	ctx := t.Context()
	if err := st.RecordDotsInboxRead(ctx, "operator:a@example.com", 2, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDotsInboxRead(ctx, "svc-x", 0, "boom"); err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(Server{Tokens: Tokens{"caller": "test-token"}, Service: Service{Store: st}}.Handler())
	defer h.Close()

	code, out := projectRequest(t, h.URL, http.MethodGet, "/v1/dots/inbox-reads", "test-token", nil)
	reads, _ := out["reads"].([]any)
	if code != http.StatusOK || len(reads) != 2 {
		t.Fatalf("list code=%d body=%v", code, out)
	}
	first, _ := reads[0].(map[string]any)
	if first["principal"] != "svc-x" || first["result"] != "boom" || first["listed"] != float64(0) {
		t.Fatalf("newest read = %v", first)
	}

	code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/dots/inbox-reads?limit=1", "test-token", nil)
	if reads, _ = out["reads"].([]any); code != http.StatusOK || len(reads) != 1 {
		t.Fatalf("limit code=%d body=%v", code, out)
	}
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/dots/inbox-reads?since="+future, "test-token", nil)
	if reads, _ = out["reads"].([]any); code != http.StatusOK || len(reads) != 0 {
		t.Fatalf("since code=%d body=%v", code, out)
	}
	for _, q := range []string{"?since=not-a-time", "?limit=0", "?limit=501"} {
		if code, _ = projectRequest(t, h.URL, http.MethodGet, "/v1/dots/inbox-reads"+q, "test-token", nil); code != http.StatusBadRequest {
			t.Fatalf("%s code=%d, want 400", q, code)
		}
	}
	if code, _ = projectRequest(t, h.URL, http.MethodGet, "/v1/dots/inbox-reads", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated code=%d", code)
	}
}
