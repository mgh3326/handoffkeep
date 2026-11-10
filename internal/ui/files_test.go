package ui

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

type fileRouteObjects struct {
	disposition string
	location    string
}

func (*fileRouteObjects) Stat(context.Context, string) error                          { return nil }
func (*fileRouteObjects) Put(context.Context, string, io.Reader, int64, string) error { return nil }
func (*fileRouteObjects) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*fileRouteObjects) Presign(context.Context, string, time.Duration) (string, error) {
	return "https://objects.example/file", nil
}
func (o *fileRouteObjects) PresignDownload(_ context.Context, _ string, _ time.Duration, disposition string) (string, error) {
	o.disposition = disposition
	return o.location, nil
}
func (*fileRouteObjects) PresignPut(context.Context, string, time.Duration, int64) (string, error) {
	return "", nil
}
func (*fileRouteObjects) Head(context.Context, string) (attachments.ObjectInfo, error) {
	return attachments.ObjectInfo{}, nil
}
func (*fileRouteObjects) Delete(context.Context, string) error { return nil }
func (*fileRouteObjects) List(context.Context, string) ([]attachments.ObjectInfo, error) {
	return nil, nil
}

func TestUIFileDownload(t *testing.T) {
	ctx := context.Background()
	_, dbURL := testdb.Schema(t, "ui_file_download")
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	shaHot := strings.Repeat("a", 64)
	shaArchived := strings.Repeat("b", 64)
	shaLegacy := strings.Repeat("d", 64)
	config := attachments.Config{Endpoint: "http://objects.example", Bucket: "files"}
	for _, x := range []store.Attachment{
		{SHA256: shaHot, SizeBytes: 1, MIME: "text/plain", OriginalName: "한글 file.txt", CreatedBy: "ui-test", RefKind: "task", RefID: "1", ObjectLocation: attachments.ObjectLocation(config)},
		{SHA256: shaArchived, SizeBytes: 1, MIME: "text/plain", OriginalName: "old.txt", CreatedBy: "ui-test", RefKind: "task", RefID: "2", ObjectLocation: "archive/2026"},
		{SHA256: shaLegacy, SizeBytes: 1, MIME: "text/plain", OriginalName: "legacy.txt", CreatedBy: "ui-test", RefKind: "task", RefID: "3"},
	} {
		if _, _, err := st.PutAttachment(ctx, x, 100, 100); err != nil {
			t.Fatal(err)
		}
	}
	objects := &fileRouteObjects{location: "http://objects.example/file?signature=fresh"}
	am := &attachments.Manager{Store: st, Objects: objects, Config: config, Enabled: true}
	h := &Handler{store: st, attachments: am}
	var logs strings.Builder
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })
	call := func(sha string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/ui/files/"+sha, nil)
		w := httptest.NewRecorder()
		h.fileDownload(w, r, "operator@example.com", sha)
		return w
	}
	if w := call(shaHot); w.Code != http.StatusFound || w.Header().Get("Location") != objects.location {
		t.Fatalf("hot response: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if !strings.Contains(objects.disposition, "attachment") || !strings.Contains(objects.disposition, "filename") || !strings.Contains(objects.disposition, "file.txt") {
		t.Fatalf("download disposition %q", objects.disposition)
	}
	if w := call(shaArchived); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "archived at archive/2026") || !strings.Contains(w.Body.String(), "hk 3680") {
		t.Fatalf("archived response: %d %s", w.Code, w.Body.String())
	}
	if w := call(shaLegacy); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "unknown location (legacy metadata)") {
		t.Fatalf("legacy location response: %d %s", w.Code, w.Body.String())
	}
	if w := call(strings.Repeat("c", 64)); w.Code != http.StatusNotFound {
		t.Fatalf("unknown response: %d", w.Code)
	}
	for _, expected := range []string{"sha=\"" + shaHot + "\" result=\"redirect\"", "sha=\"" + shaArchived + "\" result=\"archived\"", "result=\"not_found\""} {
		if !strings.Contains(logs.String(), expected) {
			t.Errorf("missing log %q in %s", expected, logs.String())
		}
	}
}
