package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestWriteDocumentDownloadServesBodyAsAttachment(t *testing.T) {
	const body = `{"tables":[{"name":"tasks"}]}`
	rec := httptest.NewRecorder()
	writeDocumentDownload(rec, "tbls/oci/mysql/brewdial/schema.json", body)
	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type %q, want application/json", got)
	}
	if got := res.Header.Get("Content-Disposition"); got != "attachment; filename=schema.json" {
		t.Fatalf("Content-Disposition %q, want attachment with the key's last segment", got)
	}
	if got := rec.Body.String(); got != body {
		t.Fatalf("body %q, want the stored bytes unchanged", got)
	}
}

func TestBoardDocDownloadRejectsMalformedKey(t *testing.T) {
	// The key is refused before the store is touched, so no database is needed.
	h := &Handler{}
	for _, key := range []string{"", "/abs/schema.json", "tbls/../schema.json", "has space"} {
		rec := httptest.NewRecorder()
		h.boardDocDownload(rec, downloadRequest(key))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("key %q: status %d, want 400", key, rec.Code)
		}
	}
}

func TestBoardDocDownloadServesCurrentBodyOrNotFound(t *testing.T) {
	ctx := t.Context()
	_, url := testdb.Schema(t, "ui_doc_download")
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	const key = "tbls/example/mysql/download/schema.json"
	const body = `{"tables":[]}`
	if _, _, err := st.PutDocument(ctx, store.Document{Key: key, Kind: "report", Body: body, CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st}

	rec := httptest.NewRecorder()
	h.boardDocDownload(rec, downloadRequest(key))
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("status %d body %q, want 200 with the stored body", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); got != "attachment; filename=schema.json" {
		t.Fatalf("Content-Disposition %q", got)
	}

	rec = httptest.NewRecorder()
	h.boardDocDownload(rec, downloadRequest("tbls/none/schema.json"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing document: status %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Disposition"); got != "" {
		t.Fatalf("a missing document must not answer as an attachment, got %q", got)
	}
}

func downloadRequest(key string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/ui/api/board/doc/download?"+url.Values{"key": {key}}.Encode(), nil)
}
