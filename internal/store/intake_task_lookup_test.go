package store

import (
	"fmt"
	"testing"
	"time"
)

// checkTaskBySourceLookup holds the (source, source_ref) lookup to its results:
// a recorded pair is found, an unrecorded ref or source is not, and an empty
// source is answered without error. The MySQL plan test runs it too.
func checkTaskBySourceLookup(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	ref := fmt.Sprintf("lookup-%d", time.Now().UnixNano())
	const source = "lookup-test"
	x, created, err := s.IntakeTask(ctx, IntakeInput{Source: source, SourceRef: ref, Title: "lookup", CreatedBy: "tester"})
	if err != nil || !created {
		t.Fatalf("intake created=%t err=%v", created, err)
	}

	got, found, err := taskBySourceTx(ctx, s.db(), source, ref)
	if err != nil || !found || got.ID != x.ID {
		t.Fatalf("recorded pair: found=%t id=%d err=%v, want task %d", found, got.ID, err, x.ID)
	}
	if _, found, err := taskBySourceTx(ctx, s.db(), source, ref+"-missing"); err != nil || found {
		t.Fatalf("unrecorded ref: found=%t err=%v", found, err)
	}
	if _, found, err := taskBySourceTx(ctx, s.db(), source+"-other", ref); err != nil || found {
		t.Fatalf("other source: found=%t err=%v", found, err)
	}
	if _, found, err := taskBySourceTx(ctx, s.db(), "", ref); err != nil || found {
		t.Fatalf("empty source: found=%t err=%v", found, err)
	}
}

func TestTaskBySourceLookupKeepsResults(t *testing.T) {
	s, _ := searchTestStore(t)
	checkTaskBySourceLookup(t, s)
}
