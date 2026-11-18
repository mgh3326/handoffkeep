package store

import (
	"context"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The feed pass and the family lane run on both dialects: the cursor, the
// sync_id order and the transaction-scoped feed lock are the area's
// cross-dialect contract (design §4).
func init() {
	registerYTAreaCase("feed/cursor-and-sync-id-order", func(t *testing.T, s *Store) {
		ctx := t.Context()
		task, _, err := s.IntakeTask(ctx, IntakeInput{Source: "yt-feed-case", SourceRef: "one", Title: "feed task", Project: projectPtr("handoffkeep"), CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		var batches [][]FeedRow
		c, err := newYTFeedConsumer("case_feed", []string{"task"}, func(_ context.Context, _ transaction, rows []FeedRow) error {
			batches = append(batches, append([]FeedRow(nil), rows...))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// The task was written before this consumer's cursor existed, so the
		// first pass creates the cursor at the feed's max_sync_id and reads
		// nothing: history before a new cursor is the backfill's.
		now := time.Now().UTC()
		first, err := s.runYTFeedConsumer(ctx, c, 100, now)
		if err != nil || !first.Created || first.Busy || first.Read != 0 || first.Handled != 0 {
			t.Fatalf("first pass: %+v err=%v", first, err)
		}
		if len(batches) != 0 {
			t.Fatalf("first pass batches: %+v", batches)
		}
		// A task change commits a new sync_log row the next pass reads.
		priority := 5
		if _, err := s.TransitionTaskWith(ctx, task.ID, TransitionInput{To: StateBacklog, By: "tester",
			Priority: &priority, Kind: "implement", Project: projectPtr("handoffkeep")}); err != nil {
			t.Fatal(err)
		}
		second, err := s.runYTFeedConsumer(ctx, c, 100, time.Now().UTC())
		if err != nil || second.Created || second.Read == 0 || second.Cursor <= first.Cursor {
			t.Fatalf("second pass: %+v err=%v", second, err)
		}
		if len(batches) != 1 || len(batches[0]) == 0 {
			t.Fatalf("second pass batches: %+v", batches)
		}
		for i := 1; i < len(batches[0]); i++ {
			if batches[0][i].SyncID <= batches[0][i-1].SyncID {
				t.Fatalf("rows not in sync_id order: %+v", batches[0])
			}
		}
	})

	registerYTAreaCase("family/lane-and-root-recorded-once", func(t *testing.T, s *Store) {
		ctx := t.Context()
		l := s.YTWriteLedger()
		op, _, err := l.Prepare(ctx, time.Now().UTC(), ytwrite.NewOp{
			Principal: "p", Kind: ytwrite.KindArticleCreate, Key: "case-family", PayloadSHA: "sha-case-family",
			Payload: []byte(`{}`), EntityKind: ytwrite.ArticleEntity, EntityID: "1",
			YTTargetID: "3-90", Tenant: "yt.example", Actor: "tester",
		})
		if err != nil {
			t.Fatal(err)
		}
		unlock, ok, err := l.LockFamily(ctx, "yt.example", "0-30", "briefs")
		if err != nil || !ok {
			t.Fatalf("lock family: ok=%v err=%v", ok, err)
		}
		if _, ok, err := l.LockFamily(ctx, "yt.example", "0-30", "briefs"); err != nil || ok {
			t.Fatalf("second lane holder: ok=%v err=%v", ok, err)
		}
		unlock()
		unlock2, ok, err := l.LockFamily(ctx, "yt.example", "0-30", "briefs")
		if err != nil || !ok {
			t.Fatalf("relock family: ok=%v err=%v", ok, err)
		}
		unlock2()

		if _, found, err := l.FamilyRoot(ctx, "yt.example", "0-30", "briefs"); err != nil || found {
			t.Fatalf("empty root: found=%v err=%v", found, err)
		}
		if err := l.RecordFamilyRoot(ctx, "yt.example", "0-30", "briefs", "3-1", op.ID); err != nil {
			t.Fatal(err)
		}
		if err := l.RecordFamilyRoot(ctx, "yt.example", "0-30", "briefs", "3-2", op.ID); err != nil {
			t.Fatal(err)
		}
		got, found, err := l.FamilyRoot(ctx, "yt.example", "0-30", "briefs")
		if err != nil || !found || got != "3-1" {
			t.Fatalf("root %q found=%v err=%v, want 3-1", got, found, err)
		}
	})
}
