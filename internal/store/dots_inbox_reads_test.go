package store

import (
	"context"
	"testing"
	"time"
)

func TestDotsInboxReadsRecordListAndPrune(t *testing.T) {
	s := schemaStore(t, "dots_inbox_reads")
	ctx := context.Background()

	// A row older than the retention window is removed by the next write.
	old := time.Now().UTC().Add(-DotsInboxReadRetention - time.Hour)
	if _, err := s.pool.Exec(ctx, `INSERT INTO dots_inbox_reads(principal,listed,result,read_at) VALUES('old',1,'ok',$1)`, old); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDotsInboxRead(ctx, "operator:a@example.com", 3, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDotsInboxRead(ctx, "svc-x", 0, "boom"); err != nil {
		t.Fatal(err)
	}

	xs, err := s.ListDotsInboxReads(ctx, time.Time{}, DotsInboxReadListMax)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 2 {
		t.Fatalf("reads = %+v", xs)
	}
	// Newest first.
	if xs[0].Principal != "svc-x" || xs[0].Listed != 0 || xs[0].Result != "boom" || xs[1].Principal != "operator:a@example.com" || xs[1].Listed != 3 || xs[1].Result != "ok" {
		t.Fatalf("reads = %+v", xs)
	}
	for _, x := range xs {
		if x.ID < 1 || x.ReadAt.IsZero() {
			t.Fatalf("read = %+v", x)
		}
	}

	// since keeps reads at or after it.
	second := xs[1].ReadAt
	future := time.Now().UTC().Add(time.Hour)
	if xs, err = s.ListDotsInboxReads(ctx, future, DotsInboxReadListMax); err != nil || len(xs) != 0 {
		t.Fatalf("future since: xs=%+v err=%v", xs, err)
	}
	if xs, err = s.ListDotsInboxReads(ctx, second, 1); err != nil || len(xs) != 1 || xs[0].Principal != "svc-x" {
		t.Fatalf("limit: xs=%+v err=%v", xs, err)
	}
}

func TestDotsInboxReadsRefusesInvalid(t *testing.T) {
	s := schemaStore(t, "dots_inbox_reads_bad")
	ctx := context.Background()
	for _, tc := range []struct {
		principal string
		listed    int
		result    string
	}{
		{"", 1, "ok"},
		{"operator:a@example.com", -1, "ok"},
		{"operator:a@example.com", 1, ""},
	} {
		if err := s.RecordDotsInboxRead(ctx, tc.principal, tc.listed, tc.result); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
	if _, err := s.ListDotsInboxReads(ctx, time.Time{}, 0); err == nil {
		t.Fatal("limit 0 accepted")
	}
}
