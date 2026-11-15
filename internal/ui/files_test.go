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

	"github.com/jackc/pgx/v5"
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
func (*fileRouteObjects) Copy(context.Context, string, string) error {
	return nil
}
func (*fileRouteObjects) List(context.Context, string) ([]attachments.ObjectInfo, error) {
	return nil, nil
}

func TestAttachmentDispositionInlineAllowlist(t *testing.T) {
	cases := []struct {
		name   string
		mime   string
		query  string
		prefix string
	}{
		{"png inline", "image/png", "?inline=1", "inline"},
		{"jpeg inline", "image/jpeg", "?inline=1", "inline"},
		{"webp inline", "image/webp", "?inline=1", "inline"},
		{"gif inline", "image/gif", "?inline=1", "inline"},
		{"mp4 inline", "video/mp4", "?inline=1", "inline"},
		{"webm inline", "video/webm", "?inline=1", "inline"},
		{"type parameters", "image/png; charset=binary", "?inline=1", "inline"},
		{"svg stays a download", "image/svg+xml", "?inline=1", "attachment"},
		{"html stays a download", "text/html", "?inline=1", "attachment"},
		{"pdf stays a download", "application/pdf", "?inline=1", "attachment"},
		{"plain text stays a download", "text/plain", "?inline=1", "attachment"},
		{"unknown type stays a download", "", "?inline=1", "attachment"},
		{"no query keeps the download", "image/png", "", "attachment"},
		{"another inline value keeps the download", "image/png", "?inline=true", "attachment"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/ui/files/"+strings.Repeat("a", 64)+c.query, nil)
			got := attachmentDisposition(r, store.Attachment{MIME: c.mime, OriginalName: "한글 file.png"})
			if !strings.HasPrefix(got, c.prefix+";") {
				t.Fatalf("disposition %q, want %s", got, c.prefix)
			}
			if !strings.Contains(got, "filename") {
				t.Fatalf("disposition %q lost the filename", got)
			}
		})
	}
}

func TestConsoleCSPAddsTheMediaOrigin(t *testing.T) {
	if got := consoleCSP(""); got != ConsoleCSP {
		t.Fatalf("no origin changed the baseline policy: %q", got)
	}
	got := consoleCSP("https://objects.example")
	for _, want := range []string{"img-src 'self' data: https://objects.example;", "media-src 'self' https://objects.example;"} {
		if !strings.Contains(got, want) {
			t.Fatalf("policy %q missing %q", got, want)
		}
	}
	if got := mediaOrigin(&attachments.Manager{Config: attachments.Config{Endpoint: "https://ns.compat.objectstorage.example/path?x=1"}}); got != "https://ns.compat.objectstorage.example" {
		t.Fatalf("mediaOrigin %q", got)
	}
	for _, endpoint := range []string{"", "objects.example", "ftp://objects.example"} {
		if got := mediaOrigin(&attachments.Manager{Config: attachments.Config{Endpoint: endpoint}}); got != "" {
			t.Fatalf("mediaOrigin(%q) = %q, want empty", endpoint, got)
		}
	}
	if got := mediaOrigin(nil); got != "" {
		t.Fatalf("mediaOrigin(nil) = %q", got)
	}
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
	shaArchiveNew := strings.Repeat("3", 64)
	shaLegacy := strings.Repeat("d", 64)
	shaPNG := strings.Repeat("e", 64)
	shaSVG := strings.Repeat("f", 64)
	shaHTML := strings.Repeat("1", 64)
	shaPDF := strings.Repeat("2", 64)
	config := attachments.Config{Endpoint: "http://objects.example", Bucket: "files"}
	for _, x := range []store.Attachment{
		{SHA256: shaHot, SizeBytes: 1, MIME: "text/plain", OriginalName: "한글 file.txt", CreatedBy: "ui-test", RefKind: "task", RefID: "1", ObjectLocation: attachments.ObjectLocation(config)},
		{SHA256: shaArchived, SizeBytes: 1, MIME: "text/plain", OriginalName: "old.txt", CreatedBy: "ui-test", RefKind: "task", RefID: "2", ObjectLocation: "archive/2026"},
		{SHA256: shaArchiveNew, SizeBytes: 1, MIME: "text/plain", OriginalName: "moved.txt", CreatedBy: "ui-test", RefKind: "task", RefID: "8", ArchiveLocation: "fleet-artifacts/archive/tasks/9/attachments/moved.txt"},
		{SHA256: shaLegacy, SizeBytes: 1, MIME: "text/plain", OriginalName: "legacy.txt", CreatedBy: "ui-test", RefKind: "task", RefID: "3"},
		{SHA256: shaPNG, SizeBytes: 1, MIME: "image/png", OriginalName: "card.png", CreatedBy: "ui-test", RefKind: "task", RefID: "4", ObjectLocation: attachments.ObjectLocation(config)},
		{SHA256: shaSVG, SizeBytes: 1, MIME: "image/svg+xml", OriginalName: "drawing.svg", CreatedBy: "ui-test", RefKind: "task", RefID: "5", ObjectLocation: attachments.ObjectLocation(config)},
		{SHA256: shaHTML, SizeBytes: 1, MIME: "text/html", OriginalName: "page.html", CreatedBy: "ui-test", RefKind: "task", RefID: "6", ObjectLocation: attachments.ObjectLocation(config)},
		{SHA256: shaPDF, SizeBytes: 1, MIME: "application/pdf", OriginalName: "plan.pdf", CreatedBy: "ui-test", RefKind: "task", RefID: "7", ObjectLocation: attachments.ObjectLocation(config)},
	} {
		if _, _, err := st.PutAttachment(ctx, x, 100, 100); err != nil {
			t.Fatal(err)
		}
	}
	// The archive record is written by the archive route, not by a put; seed
	// it here so the page is exercised against the recorded columns.
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, `UPDATE attachments SET archived_at=now(), archive_location=$2 WHERE sha256=$1`, shaArchiveNew, "fleet-artifacts/archive/tasks/9/attachments/moved.txt"); err != nil {
		t.Fatal(err)
	}
	objects := &fileRouteObjects{location: "http://objects.example/file?signature=fresh"}
	am := &attachments.Manager{Store: st, Objects: objects, Config: config, Enabled: true}
	h := &Handler{store: st, attachments: am}
	var logs strings.Builder
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })
	call := func(sha, query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/ui/files/"+sha+query, nil)
		w := httptest.NewRecorder()
		h.fileDownload(w, r, "operator@example.com", sha)
		return w
	}
	if w := call(shaHot, ""); w.Code != http.StatusFound || w.Header().Get("Location") != objects.location {
		t.Fatalf("hot response: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if !strings.Contains(objects.disposition, "attachment") || !strings.Contains(objects.disposition, "filename") || !strings.Contains(objects.disposition, "file.txt") {
		t.Fatalf("download disposition %q", objects.disposition)
	}
	if w := call(shaPNG, "?inline=1"); w.Code != http.StatusFound || !strings.HasPrefix(objects.disposition, "inline;") || !strings.Contains(objects.disposition, "card.png") {
		t.Fatalf("inline image: %d %q", w.Code, objects.disposition)
	}
	if w := call(shaPNG, "?inline=1"); w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("inline image headers: %v", w.Header())
	}
	for _, c := range []struct{ sha, query string }{
		{shaSVG, "?inline=1"}, {shaHTML, "?inline=1"}, {shaPDF, "?inline=1"}, {shaPNG, ""},
	} {
		if w := call(c.sha, c.query); w.Code != http.StatusFound || !strings.HasPrefix(objects.disposition, "attachment;") {
			t.Fatalf("%s%s disposition %q (%d)", c.sha, c.query, objects.disposition, w.Code)
		}
	}
	if w := call(shaArchived, "?inline=1"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "보관됨 · archive/2026") || !strings.Contains(w.Body.String(), "hk 3680") {
		t.Fatalf("archived response: %d %s", w.Code, w.Body.String())
	}
	if w := call(shaArchiveNew, ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "보관됨 · fleet-artifacts/archive/tasks/9/attachments/moved.txt") {
		t.Fatalf("recorded archive response: %d %s", w.Code, w.Body.String())
	}
	if w := call(shaLegacy, "?inline=1"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "unknown location (legacy metadata)") {
		t.Fatalf("legacy location response: %d %s", w.Code, w.Body.String())
	}
	if w := call(strings.Repeat("c", 64), "?inline=1"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown response: %d", w.Code)
	}
	for _, expected := range []string{"sha=\"" + shaHot + "\" result=\"redirect\"", "sha=\"" + shaArchived + "\" result=\"archived\"", "result=\"not_found\""} {
		if !strings.Contains(logs.String(), expected) {
			t.Errorf("missing log %q in %s", expected, logs.String())
		}
	}
}
