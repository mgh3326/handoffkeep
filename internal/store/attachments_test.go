package store

import (
	"fmt"
	"sync"
	"testing"
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
