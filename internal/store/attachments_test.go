package store

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// testAttachmentStore runs in its own schema: the attachment tests count
// usage rows, and truncating the shared tables raced other packages' tests.
func testAttachmentStore(t *testing.T) *Store { return schemaStore(t, "attachments") }
func attachmentFor(i int) Attachment {
	return Attachment{SHA256: fmt.Sprintf("%064x", i+1), SizeBytes: 1, MIME: "text/plain", OriginalName: fmt.Sprintf("%d.txt", i), CreatedBy: "node", RefKind: "none"}
}

func TestAttachmentPutSerializesMonthlyFuse(t *testing.T) {
	s := testAttachmentStore(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, rejected := 0, 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, e := s.PutAttachment(t.Context(), attachmentFor(i), 100, 2)
			mu.Lock()
			defer mu.Unlock()
			if e == nil {
				ok++
			} else if e == ErrAttachmentPutCap {
				rejected++
			} else {
				t.Errorf("put %d: %v", i, e)
			}
		}(i)
	}
	wg.Wait()
	if ok != 2 || rejected != 8 {
		t.Fatalf("ok=%d rejected=%d", ok, rejected)
	}
	rows, e := s.ListAttachments(t.Context(), "", "", 100)
	if e != nil {
		t.Fatal(e)
	}
	u, e := s.AttachmentUsage(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) != 2 || u.Puts != 2 || u.BytesAdded != 2 {
		t.Fatalf("rows=%d usage=%+v", len(rows), u)
	}
}
func TestAttachmentRejectAndDedupeAreAtomic(t *testing.T) {
	s := testAttachmentStore(t)
	x := attachmentFor(1)
	if _, _, e := s.PutAttachment(t.Context(), x, 0, 2); e != ErrAttachmentStorageCap {
		t.Fatalf("storage cap=%v", e)
	}
	rows, e := s.ListAttachments(t.Context(), "", "", 100)
	if e != nil {
		t.Fatal(e)
	}
	u, e := s.AttachmentUsage(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) != 0 || u.Puts != 0 || u.BytesAdded != 0 {
		t.Fatalf("rejection changed state rows=%d usage=%+v", len(rows), u)
	}
	if _, created, e := s.PutAttachment(t.Context(), x, 100, 2); e != nil || !created {
		t.Fatalf("first created=%v err=%v", created, e)
	}
	if _, created, e := s.PutAttachment(t.Context(), x, 100, 2); e != nil || created {
		t.Fatalf("dedupe created=%v err=%v", created, e)
	}
	u, e = s.AttachmentUsage(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if u.Puts != 1 || u.BytesAdded != 1 {
		t.Fatalf("dedupe usage=%+v", u)
	}
}

func TestAttachmentObjectLocationRecorded(t *testing.T) {
	s := testAttachmentStore(t)
	x := attachmentFor(31)
	x.ObjectLocation = "https://namespace.compat.objectstorage.region.oraclecloud.com/files"
	if _, _, err := s.PutAttachment(t.Context(), x, 100, 10); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAttachmentObjects(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ObjectLocation != x.ObjectLocation {
		t.Fatalf("objects = %+v", rows)
	}
	if err = s.SetAttachmentObjectLocation(t.Context(), x.SHA256, "https://target.example/files"); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListAttachmentObjects(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].ObjectLocation != "https://target.example/files" {
		t.Fatalf("updated location = %q", rows[0].ObjectLocation)
	}
}

// A checkpoint ref is no longer written here, but the previous release writes
// attachment_refs rows with ref_kind 'checkpoint' during the blue-green
// overlap, and the List filter must keep reading them. The write path still
// refuses the ref, so the new release adds no checkpoint row of its own.
func TestAttachmentCheckpointRefIsListedButNotStored(t *testing.T) {
	s := testAttachmentStore(t)
	x := attachmentFor(50)
	if _, created, e := s.PutAttachment(t.Context(), x, 100, 10); e != nil || !created {
		t.Fatalf("attachment created=%v err=%v", created, e)
	}
	// The row the previous release leaves behind: its validator accepted any
	// decimal checkpoint id, so this is what the List filter must still find.
	if _, e := s.pool.Exec(t.Context(), `INSERT INTO attachment_refs(sha256,ref_kind,ref_id) VALUES($1,'checkpoint','42')`, x.SHA256); e != nil {
		t.Fatal(e)
	}
	rows, e := s.ListAttachments(t.Context(), "checkpoint", "42", 10)
	if e != nil {
		t.Fatalf("list checkpoint:42: %v", e)
	}
	if len(rows) != 1 || rows[0].SHA256 != x.SHA256 || rows[0].RefKind != "checkpoint" || rows[0].RefID != "42" {
		t.Fatalf("list checkpoint:42 rows=%+v", rows)
	}
	if _, e := s.ListAttachments(t.Context(), "checkpoint", "x42", 10); e == nil {
		t.Fatal("non-decimal checkpoint id accepted by the list filter")
	}
	bad := attachmentFor(51)
	bad.RefKind, bad.RefID = "checkpoint", "42"
	if _, _, e := s.PutAttachment(t.Context(), bad, 100, 10); e == nil {
		t.Fatal("checkpoint ref stored by the write path")
	}
}

// A task ref is stored in attachment_refs, whose CHECK must admit 'task' after
// the attachment-task-ref migration. A non-canonical task id is refused by the
// store's validator before any row is written.
func TestAttachmentTaskRefIsStoredAndListedByRef(t *testing.T) {
	s := testAttachmentStore(t)
	x := attachmentFor(40)
	x.RefKind, x.RefID = "task", "12"
	if _, created, e := s.PutAttachment(t.Context(), x, 100, 10); e != nil || !created {
		t.Fatalf("task ref created=%v err=%v", created, e)
	}
	rows, e := s.ListAttachments(t.Context(), "task", "12", 10)
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) != 1 || rows[0].SHA256 != x.SHA256 || rows[0].RefKind != "task" || rows[0].RefID != "12" {
		t.Fatalf("list task:12 rows=%+v", rows)
	}
	bad := attachmentFor(41)
	bad.RefKind, bad.RefID = "task", "012"
	if _, _, e := s.PutAttachment(t.Context(), bad, 100, 10); e == nil {
		t.Fatal("non-canonical task id stored")
	}
}

// The dots inbox reads a page of task refs in one statement. It must filter to
// the requested set, order by created_at then task id, and bound the rows one
// task contributes so a single large task cannot flood the page.
func TestListTaskAttachmentsByRefsFiltersSortsAndCaps(t *testing.T) {
	s := testAttachmentStore(t)
	base := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	add := func(n int, refID string, at time.Time) Attachment {
		x := attachmentFor(n)
		if _, e := s.pool.Exec(t.Context(), `INSERT INTO attachments(sha256,size_bytes,mime,original_name,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6)`, x.SHA256, x.SizeBytes, x.MIME, x.OriginalName, x.CreatedBy, at); e != nil {
			t.Fatal(e)
		}
		if _, e := s.pool.Exec(t.Context(), `INSERT INTO attachment_refs(sha256,ref_kind,ref_id) VALUES($1,'task',$2)`, x.SHA256, refID); e != nil {
			t.Fatal(e)
		}
		x.RefKind, x.RefID, x.CreatedAt = "task", refID, at
		return x
	}
	late := add(60, "12", base.Add(3*time.Second))
	early := add(61, "12", base)
	other := add(62, "13", base.Add(time.Second))
	add(63, "14", base)

	rows, e := s.ListTaskAttachmentsByRefs(t.Context(), []int64{12, 13}, 10)
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	want := []Attachment{early, other, late}
	for i := range want {
		if rows[i].SHA256 != want[i].SHA256 || rows[i].RefID != want[i].RefID || rows[i].RefKind != "task" || !rows[i].CreatedAt.Equal(want[i].CreatedAt) {
			t.Fatalf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
	capped, e := s.ListTaskAttachmentsByRefs(t.Context(), []int64{12, 13}, 1)
	if e != nil {
		t.Fatal(e)
	}
	if len(capped) != 2 || capped[0].SHA256 != early.SHA256 || capped[1].SHA256 != other.SHA256 {
		t.Fatalf("perRef 1 rows = %+v", capped)
	}
	if rows, e := s.ListTaskAttachmentsByRefs(t.Context(), nil, 10); e != nil || len(rows) != 0 {
		t.Fatalf("no refs rows=%+v err=%v", rows, e)
	}
	if _, e := s.ListTaskAttachmentsByRefs(t.Context(), []int64{0}, 10); e == nil {
		t.Fatal("non-positive task id accepted")
	}
}
