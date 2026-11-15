package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/client"
)

func TestArchiveCandidatesWireShape(t *testing.T) {
	sha := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)
	hk := newFakeHK(t)
	hk.on("GET", "/v1/attachments/archive-candidates?limit=2", http.StatusOK,
		`{"candidates":[{"sha256":"`+sha+`","size_bytes":3,"mime":"text/plain","original_name":"a.txt","task_id":7,`+
			`"tasks":[{"id":7,"state":"merged","closed_at":"2026-11-01T00:00:00Z"},{"id":9,"state":"dropped","closed_at":"2026-11-02T00:00:00Z"}]}],"next":"`+sha+`"}`, nil)
	hk.on("GET", "/v1/attachments/archive-candidates?after="+sha+"&limit=2", http.StatusOK,
		`{"candidates":[{"sha256":"`+other+`","size_bytes":5,"mime":"image/png","original_name":"b.png","task_id":9,"tasks":[{"id":9,"state":"dropped","closed_at":"2026-11-02T00:00:00Z"}]}],"next":""}`, nil)
	c := client.Client{URL: hk.URL, Token: fixtureToken}

	page, err := c.ArchiveCandidates(context.Background(), 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if page.Next != sha || len(page.Candidates) != 1 {
		t.Fatalf("page=%+v", page)
	}
	x := page.Candidates[0]
	if x.SHA256 != sha || x.SizeBytes != 3 || x.MIME != "text/plain" || x.OriginalName != "a.txt" || x.TaskID != 7 {
		t.Fatalf("candidate=%+v", x)
	}
	if len(x.Tasks) != 2 || x.Tasks[0].ID != 7 || x.Tasks[1].State != "dropped" || !x.Tasks[1].ClosedAt.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("tasks=%+v", x.Tasks)
	}

	next, err := c.ArchiveCandidates(context.Background(), 2, page.Next)
	if err != nil {
		t.Fatal(err)
	}
	if next.Next != "" || len(next.Candidates) != 1 || next.Candidates[0].SHA256 != other {
		t.Fatalf("next page=%+v", next)
	}
	if got := hk.requests(); len(got) != 2 || got[0].method != http.MethodGet || got[0].auth != "Bearer "+fixtureToken {
		t.Fatalf("requests=%+v", got)
	}
}

func TestRecordAttachmentArchiveWireShape(t *testing.T) {
	sha := strings.Repeat("c", 64)
	const location = "fleet-artifacts/archive/tasks/7/attachments/c.txt"
	hk := newFakeHK(t)
	hk.on("POST", "/v1/attachments/"+sha+"/archive", http.StatusOK,
		`{"archived_at":"2026-11-16T03:00:00Z","location":"`+location+`","hot_deleted":true}`, nil)
	c := client.Client{URL: hk.URL, Token: fixtureToken}
	rec, err := c.RecordAttachmentArchive(context.Background(), sha, location, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	if !rec.ArchivedAt.Equal(want) || rec.Location != location || !rec.HotDeleted {
		t.Fatalf("record=%+v", rec)
	}
	got := hk.requests()
	if len(got) != 1 || got[0].contentType != "application/json" {
		t.Fatalf("requests=%+v", got)
	}
	var body map[string]any
	if err := json.Unmarshal(got[0].body, &body); err != nil {
		t.Fatalf("body %q: %v", got[0].body, err)
	}
	if body["location"] != location || body["size_bytes"] != float64(3) || body["sha256"] != sha {
		t.Fatalf("body=%v", body)
	}
}

func TestRecordAttachmentArchiveRefusalIsCode(t *testing.T) {
	sha := strings.Repeat("d", 64)
	hk := newFakeHK(t)
	hk.on("POST", "/v1/attachments/"+sha+"/archive", http.StatusConflict, `{"error":"attachment_not_archivable"}`, nil)
	c := client.Client{URL: hk.URL, Token: fixtureToken}
	_, err := c.RecordAttachmentArchive(context.Background(), sha, "x", 3)
	var he *client.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "attachment_not_archivable" {
		t.Fatalf("err=%v (%#v)", err, he)
	}
}
