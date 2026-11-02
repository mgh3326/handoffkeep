package api

import (
	"encoding/json"
	"fmt"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSyncAuthAndQueryGuards(t *testing.T) {
	h := Server{Tokens: Tokens{"reader": "secret"}}.Handler()
	for _, path := range []string{"/v1/sync/delta", "/v1/sync/bootstrap?model=document"} {
		for _, token := range []string{"", "wrong"} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s status=%d", path, w.Code)
			}
		}
	}
	for _, path := range []string{"/v1/sync/delta?after=x", "/v1/sync/delta?after=9223372036854775808", "/v1/sync/delta?limit=0", "/v1/sync/delta?limit=1001", "/v1/sync/bootstrap?limit=1001"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d", path, w.Code)
		}
	}
}

func TestSyncHTTPPagesAndReset(t *testing.T) {
	_, dbURL := testdb.Schema(t, "sync_http")
	st, err := store.Open(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	doc, _, err := st.PutDocument(t.Context(), store.Document{Key: "sync/http", Kind: "note", Body: "body", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	h := Server{Service: Service{Store: st}, Tokens: Tokens{"reader": "secret"}}.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	page := get("/v1/sync/bootstrap?model=document&limit=1")
	var bootstrap store.SyncBootstrap
	if page.Code != http.StatusOK || json.Unmarshal(page.Body.Bytes(), &bootstrap) != nil || len(bootstrap.Rows) != 1 || bootstrap.Rows[0].Key != doc.Key {
		t.Fatalf("bootstrap=%s", page.Body)
	}
	doc.Body = "changed"
	if _, _, err = st.PutDocument(t.Context(), doc); err != nil {
		t.Fatal(err)
	}
	page = get(fmt.Sprintf("/v1/sync/delta?after=%d&limit=1", bootstrap.MaxSyncID))
	if page.Code != http.StatusOK || page.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("delta status=%d headers=%v", page.Code, page.Header())
	}
	lines := strings.Split(strings.TrimSpace(page.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("delta=%s", page.Body)
	}
	var change store.SyncChange
	var trailer struct {
		Metadata store.SyncMetadata `json:"_metadata_"`
	}
	if json.Unmarshal([]byte(lines[0]), &change) != nil || change.Action != "U" || json.Unmarshal([]byte(lines[1]), &trailer) != nil || trailer.Metadata.LastSyncID != change.SyncID {
		t.Fatalf("delta=%s", page.Body)
	}
	for _, path := range []string{"/v1/sync/delta?after=-1", "/v1/sync/bootstrap?model=unknown", "/v1/sync/bootstrap?model=document&cursor=bad"} {
		if w := get(path); w.Code != http.StatusBadRequest {
			t.Fatalf("%s=%d %s", path, w.Code, w.Body)
		}
	}
	if _, err = st.PruneSync(t.Context(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	page = get("/v1/sync/delta?after=0")
	var reset struct {
		Reset     bool
		MinSyncID int64 `json:"min_sync_id"`
	}
	if page.Code != http.StatusGone || json.Unmarshal(page.Body.Bytes(), &reset) != nil || !reset.Reset || reset.MinSyncID <= change.SyncID {
		t.Fatalf("reset=%d %s", page.Code, page.Body)
	}
}
