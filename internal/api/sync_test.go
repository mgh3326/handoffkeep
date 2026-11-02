package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestSyncAuthAndQueryGuards(t *testing.T) {
	h := Server{Tokens: Tokens{"reader": "secret"}}.Handler()
	for _, path := range []string{"/v1/sync/delta", "/v1/sync/bootstrap?model=document", "/v1/sync/rows/document/1", "/v1/sync/rows/decision/request-one"} {
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
	for _, path := range []string{"/v1/sync/delta?after=x", "/v1/sync/delta?after=9223372036854775808", "/v1/sync/delta?limit=0", "/v1/sync/delta?limit=1001", "/v1/sync/bootstrap?limit=1001", "/v1/sync/rows/task/nope", "/v1/sync/rows/unknown/1"} {
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

func TestSyncRowsResolveDeltaIDs(t *testing.T) {
	_, dbURL := testdb.Schema(t, "sync_rows")
	ctx := t.Context()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task, err := st.CreateTask(ctx, store.Task{Lane: "sync-rows", Title: "raw task title", Kind: "implement", CreatedBy: "test", Project: projectStr("experiment")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.CreateTaskComment(ctx, task.ID, "test", "raw comment body"); err != nil {
		t.Fatal(err)
	}
	doc, _, err := st.PutDocument(ctx, store.Document{Key: "sync/rows", Kind: "note", Body: "raw document body", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	// Seed a complete request and resolution through SQL to isolate the row
	// endpoint contract from decision workflow validation.
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `UPDATE tasks SET refs='{"decision_request":{"id":"row-request","status":"answered","question":"raw question","resolution":{"kind":"answered","text":"raw resolution"}},"decision_options":{"options":[]}}' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	h := Server{Service: Service{Store: st}, Tokens: Tokens{"reader": "secret"}}.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	changes, _, err := st.SyncDelta(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, change := range changes {
		w := get("/v1/sync/rows/" + change.Model + "/" + change.ModelID)
		if w.Code != http.StatusOK {
			t.Fatalf("%+v: %d %s", change, w.Code, w.Body)
		}
		var raw map[string]json.RawMessage
		if err = json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		switch change.Model {
		case "document":
			if string(raw["body"]) != `"raw document body"` {
				t.Fatal(w.Body)
			}
		case "task":
			if string(raw["title"]) != `"raw task title"` {
				t.Fatal(w.Body)
			}
		case "task_comment":
			if string(raw["body"]) != `"raw comment body"` {
				t.Fatal(w.Body)
			}
		case "decision":
			if string(raw["question"]) != `"raw question"` || !strings.Contains(string(raw["resolution"]), "raw resolution") || raw["options"] == nil {
				t.Fatal(w.Body)
			}
		}
		seen[change.Model] = true
	}
	if len(seen) != 4 {
		t.Fatal(seen)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM documents WHERE id=$1`, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE tasks SET refs='{}' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fmt.Sprintf("/v1/sync/rows/document/%d", doc.ID), "/v1/sync/rows/decision/row-request", "/v1/sync/rows/task/9223372036854775807", "/v1/sync/rows/task_comment/9223372036854775807"} {
		w := get(path)
		if w.Code != http.StatusNotFound || strings.TrimSpace(w.Body.String()) != `{"deleted":true}` {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	for _, path := range []string{"/v1/sync/rows/unknown/1", "/v1/sync/rows/task/nope", "/v1/sync/rows/document/-1"} {
		if w := get(path); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
}
