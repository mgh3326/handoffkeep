package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
)

func testAttachmentStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL attachment tests")
	}
	s, e := Open(t.Context(), url)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.pool.Exec(t.Context(), `TRUNCATE attachment_refs, attachments, attachment_usage`); e != nil {
		s.Close()
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `TRUNCATE attachment_refs, attachments, attachment_usage`)
		s.Close()
	})
	return s
}
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
