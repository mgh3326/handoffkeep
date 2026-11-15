package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func archiveTestServer(t *testing.T, mode attachments.ArchiveMode) (*httptest.Server, *uploadDB, *uploadObjects) {
	t.Helper()
	objs := &uploadObjects{objects: map[string][]byte{}}
	allow := map[string]bool{}
	for _, x := range strings.Split(attachments.DefaultMIMEAllow, ",") {
		allow[x] = true
	}
	db := &uploadDB{rows: map[string]store.Attachment{}, uploads: map[string]store.AttachmentUpload{}}
	m := &attachments.Manager{Store: db, Objects: objs, Enabled: true, ArchiveMode: mode, Config: attachments.Config{MaxBytes: 1 << 20, StorageCap: 1 << 30, PutCap: 1000, GetCap: 1000, Allow: allow}}
	h := httptest.NewServer(Server{Service: Service{Attachments: m}, Tokens: Tokens{"node": "node-token"}}.Handler())
	t.Cleanup(h.Close)
	return h, db, objs
}

// archiveRow seeds one stored blob with its hot object, and makes it a
// candidate.
func archiveRow(t *testing.T, db *uploadDB, objs *uploadObjects, sha string) {
	t.Helper()
	db.rows[sha] = store.Attachment{SHA256: sha, SizeBytes: 3, MIME: "text/plain", OriginalName: "a.txt", CreatedBy: "node"}
	objs.objects["hk/"+sha[:2]+"/"+sha] = []byte("abc")
	db.candidates = append(db.candidates, store.AttachmentArchiveCandidate{
		SHA256: sha, SizeBytes: 3, MIME: "text/plain", OriginalName: "a.txt", TaskID: 7,
		Tasks: []store.AttachmentArchiveCandidateTask{{ID: 7, State: store.StateMerged, ClosedAt: time.Now().UTC()}},
	})
}

func TestAttachmentArchiveHTTPRecord(t *testing.T) {
	sha := strings.Repeat("a", 64)
	const location = "fleet-artifacts/archive/tasks/7/attachments/a-a.txt"
	h, db, objs := archiveTestServer(t, attachments.ArchiveModeRecord)
	archiveRow(t, db, objs, sha)

	if status, _ := uploadCall(t, h, http.MethodPost, "/v1/attachments/"+sha+"/archive", "", map[string]any{"location": location, "size_bytes": 3, "sha256": sha}); status != http.StatusUnauthorized {
		t.Fatalf("archive without a token status=%d", status)
	}

	status, page := uploadCall(t, h, http.MethodGet, "/v1/attachments/archive-candidates?limit=10", "node-token", nil)
	if status != http.StatusOK || page["next"] != "" {
		t.Fatalf("candidates status=%d body=%v", status, page)
	}
	if xs, _ := page["candidates"].([]any); len(xs) != 1 {
		t.Fatalf("candidates=%v", page["candidates"])
	}

	// The body sha must be the row's; a mismatch is refused before any write.
	status, body := uploadCall(t, h, http.MethodPost, "/v1/attachments/"+sha+"/archive", "node-token", map[string]any{"location": location, "size_bytes": 3, "sha256": strings.Repeat("b", 64)})
	if status != http.StatusConflict || body["error"] != "attachment_archive_mismatch" {
		t.Fatalf("sha mismatch status=%d body=%v", status, body)
	}
	status, body = uploadCall(t, h, http.MethodPost, "/v1/attachments/"+sha+"/archive", "node-token", map[string]any{"location": location, "size_bytes": 4, "sha256": sha})
	if status != http.StatusConflict || body["error"] != "attachment_archive_mismatch" {
		t.Fatalf("size mismatch status=%d body=%v", status, body)
	}

	status, body = uploadCall(t, h, http.MethodPost, "/v1/attachments/"+sha+"/archive", "node-token", map[string]any{"location": location, "size_bytes": 3, "sha256": sha})
	if status != http.StatusOK || body["location"] != location || body["hot_deleted"] != false {
		t.Fatalf("record status=%d body=%v", status, body)
	}
	first := body["archived_at"]
	status, body = uploadCall(t, h, http.MethodPost, "/v1/attachments/"+sha+"/archive", "node-token", map[string]any{"location": location, "size_bytes": 3, "sha256": sha})
	if status != http.StatusOK || body["archived_at"] != first || body["location"] != location {
		t.Fatalf("idempotent repeat status=%d body=%v first=%v", status, body, first)
	}
	if _, hot := objs.objects["hk/"+sha[:2]+"/"+sha]; !hot {
		t.Fatal("record mode deleted the hot object")
	}

	// Reads of an archived blob answer 410 with the location it names.
	status, body = uploadCall(t, h, http.MethodGet, "/v1/attachments/"+sha, "node-token", nil)
	if status != http.StatusGone || body["error"] != "attachment_archived" || body["location"] != location || body["sha256"] != sha {
		t.Fatalf("download status=%d body=%v", status, body)
	}
	status, body = uploadCall(t, h, http.MethodGet, "/v1/attachments/"+sha+"?presign=1", "node-token", nil)
	if status != http.StatusGone || body["error"] != "attachment_archived" || body["location"] != location {
		t.Fatalf("presign status=%d body=%v", status, body)
	}

	// The metadata list keeps the row and adds the archive fields.
	status, page = uploadCall(t, h, http.MethodGet, "/v1/attachments", "node-token", nil)
	if status != http.StatusOK {
		t.Fatalf("list status=%d body=%v", status, page)
	}
	xs, _ := page["attachments"].([]any)
	if len(xs) != 1 {
		t.Fatalf("list=%v", page["attachments"])
	}
	row, _ := xs[0].(map[string]any)
	if row["sha256"] != sha || row["archive_location"] != location || row["archived_at"] == nil {
		t.Fatalf("listed row=%v", row)
	}
}

func TestAttachmentArchiveHTTPModeOffRefuses(t *testing.T) {
	sha := strings.Repeat("c", 64)
	h, db, objs := archiveTestServer(t, attachments.ArchiveModeOff)
	archiveRow(t, db, objs, sha)
	status, body := uploadCall(t, h, http.MethodPost, "/v1/attachments/"+sha+"/archive", "node-token", map[string]any{"location": "fleet-artifacts/archive/tasks/7/attachments/c.txt", "size_bytes": 3, "sha256": sha})
	if status != http.StatusConflict || body["error"] != "attachment_archive_off" {
		t.Fatalf("mode off status=%d body=%v", status, body)
	}
	if db.rows[sha].ArchivedAt != nil {
		t.Fatal("mode off recorded an archive")
	}
	// The candidate list stays readable: the dry run that precedes the canary
	// reads it.
	if status, _ := uploadCall(t, h, http.MethodGet, "/v1/attachments/archive-candidates", "node-token", nil); status != http.StatusOK {
		t.Fatalf("candidates in mode off status=%d", status)
	}
}

func TestAttachmentArchiveHTTPModeOnDeletes(t *testing.T) {
	sha := strings.Repeat("d", 64)
	const location = "fleet-artifacts/archive/tasks/7/attachments/d.txt"
	h, db, objs := archiveTestServer(t, attachments.ArchiveModeOn)
	archiveRow(t, db, objs, sha)
	status, body := uploadCall(t, h, http.MethodPost, "/v1/attachments/"+sha+"/archive", "node-token", map[string]any{"location": location, "size_bytes": 3, "sha256": sha})
	if status != http.StatusOK || body["hot_deleted"] != true {
		t.Fatalf("mode on status=%d body=%v", status, body)
	}
	if _, hot := objs.objects["hk/"+sha[:2]+"/"+sha]; hot {
		t.Fatal("mode on kept the hot object")
	}
	if db.rows[sha].HotDeletedAt == nil {
		t.Fatal("hot delete not recorded")
	}
}
