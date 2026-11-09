package ytwritetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

func newOp(key, entityID string) ytwrite.NewOp {
	return ytwrite.NewOp{
		Principal: "p", Kind: ytwrite.KindCommentAppend, Key: key, PayloadSHA: "sha-" + key,
		Payload: []byte(`{"body":"b"}`), EntityKind: "task", EntityID: entityID, YTTargetID: "2-" + entityID, Actor: "a",
	}
}

// RunLedger runs the Ledger contract: the compare-and-set rules every
// implementation must give the protocol. The Postgres ledger runs it against a
// real database, where the races are real.
func RunLedger(t *testing.T, nl NewLedger) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

	prepare := func(t *testing.T, l ytwrite.Ledger, key, entity string) ytwrite.Op {
		t.Helper()
		op, created, err := l.Prepare(ctx, t0, newOp(key, entity))
		if err != nil || !created {
			t.Fatalf("Prepare(%s) = %+v %v %v", key, op, created, err)
		}
		return op
	}
	claim := func(t *testing.T, l ytwrite.Ledger, id int64) ytwrite.ClaimOutcome {
		t.Helper()
		_, out, err := l.Claim(ctx, t0, id)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	t.Run("prepare_is_keyed_by_principal_kind_key", func(t *testing.T) {
		l := nl(t)
		a := prepare(t, l, "k", "1")
		if a.State != ytwrite.StatePrepared || a.ID <= 0 || !a.NextAttemptAt.Equal(t0) {
			t.Fatalf("op = %+v", a)
		}
		again, created, err := l.Prepare(ctx, t0.Add(time.Hour), newOp("k", "1"))
		if err != nil || created || again.ID != a.ID || !again.CreatedAt.Equal(t0) {
			t.Fatalf("replay = %+v %v %v", again, created, err)
		}
		clash := newOp("k", "1")
		clash.PayloadSHA = "other"
		if _, _, err := l.Prepare(ctx, t0, clash); !errors.Is(err, ytwrite.ErrKeyConflict) {
			t.Fatalf("conflicting payload = %v", err)
		}
		other := newOp("k", "1")
		other.Principal = "q"
		if o, created, err := l.Prepare(ctx, t0, other); err != nil || !created || o.ID == a.ID {
			t.Fatalf("other principal = %+v %v %v", o, created, err)
		}
		if _, err := l.Get(ctx, "q", a.ID); !errors.Is(err, ytwrite.ErrNotFound) {
			t.Fatalf("Get across principals = %v", err)
		}
		if _, err := l.ByID(ctx, 99999); !errors.Is(err, ytwrite.ErrNotFound) {
			t.Fatalf("ByID of a missing op = %v", err)
		}
	})

	t.Run("claim_outcomes", func(t *testing.T) {
		l := nl(t)
		a := prepare(t, l, "a", "1")
		if out := claim(t, l, a.ID); out != ytwrite.ClaimClaimed {
			t.Fatalf("first claim = %s", out)
		}
		got, _ := l.ByID(ctx, a.ID)
		if got.State != ytwrite.StateDispatched || got.Attempts != 1 || !got.DispatchedAt.Equal(t0) {
			t.Fatalf("claimed op = %+v", got)
		}
		if out := claim(t, l, a.ID); out != ytwrite.ClaimNotPrepared {
			t.Fatalf("second claim = %s", out)
		}
		// A deferred op is not claimable before its time.
		if _, err := l.Defer(ctx, t0, a.ID, t0.Add(time.Minute), ytwrite.ReasonRateLimited, "429"); err != nil {
			t.Fatal(err)
		}
		if _, out, _ := l.Claim(ctx, t0.Add(30*time.Second), a.ID); out != ytwrite.ClaimNotDue {
			t.Fatalf("early claim = %s", out)
		}
		if _, out, _ := l.Claim(ctx, t0.Add(time.Minute), a.ID); out != ytwrite.ClaimClaimed {
			t.Fatalf("due claim = %s", out)
		}
		if got, _ := l.ByID(ctx, a.ID); got.Attempts != 2 {
			t.Fatalf("attempts = %d", got.Attempts)
		}
		if _, _, err := l.Claim(ctx, t0, 99999); !errors.Is(err, ytwrite.ErrNotFound) {
			t.Fatalf("claim of a missing op = %v", err)
		}
	})

	t.Run("stale_recovery_checks_attempt_and_cutoff", func(t *testing.T) {
		l := nl(t)
		a := prepare(t, l, "stale", "1")
		claim(t, l, a.ID)
		snapshot, err := l.Stale(ctx, t0, 10)
		if err != nil || len(snapshot) != 1 {
			t.Fatalf("stale snapshot = %+v %v", snapshot, err)
		}
		if _, err := l.Defer(ctx, t0, a.ID, t0, "retry", ""); err != nil {
			t.Fatal(err)
		}
		if _, out, err := l.Claim(ctx, t0, a.ID); err != nil || out != ytwrite.ClaimClaimed {
			t.Fatalf("reclaim = %s %v", out, err)
		}
		if changed, err := l.RecoverStale(ctx, a.ID, snapshot[0].Attempts, t0, t0, "old snapshot"); err != nil || changed {
			t.Fatalf("old attempt recovered = %v %v", changed, err)
		}
		current, err := l.ByID(ctx, a.ID)
		if err != nil || current.State != ytwrite.StateDispatched {
			t.Fatalf("current = %+v %v", current, err)
		}
		if changed, err := l.RecoverStale(ctx, a.ID, current.Attempts, t0.Add(-time.Second), t0, "too early"); err != nil || changed {
			t.Fatalf("before cutoff = %v %v", changed, err)
		}
		if changed, err := l.RecoverStale(ctx, a.ID, current.Attempts, t0, t0, "stale"); err != nil || !changed {
			t.Fatalf("recovery = %v %v", changed, err)
		}
	})

	t.Run("one_unresolved_op_per_entity_blocks_later_ones", func(t *testing.T) {
		l := nl(t)
		a, b, c := prepare(t, l, "a", "1"), prepare(t, l, "b", "1"), prepare(t, l, "c", "2")
		if out := claim(t, l, b.ID); out != ytwrite.ClaimBlocked {
			t.Fatalf("claim behind a prepared op = %s", out)
		}
		if out := claim(t, l, c.ID); out != ytwrite.ClaimClaimed {
			t.Fatalf("other entity = %s", out)
		}
		claim(t, l, a.ID)
		for _, step := range []struct {
			name string
			do   func() error
		}{
			{"dispatched", func() error { return nil }},
			{"remote_unknown", func() error {
				_, err := l.MarkUnknown(ctx, t0, a.ID, t0, ytwrite.ReasonTransport, "")
				return err
			}},
		} {
			if err := step.do(); err != nil {
				t.Fatal(err)
			}
			if out := claim(t, l, b.ID); out != ytwrite.ClaimBlocked {
				t.Fatalf("claim behind a %s op = %s", step.name, out)
			}
		}
		if _, err := l.Apply(ctx, t0, a.ID, "c-1", nil); err != nil {
			t.Fatal(err)
		}
		if out := claim(t, l, b.ID); out != ytwrite.ClaimClaimed {
			t.Fatalf("claim after the earlier op resolved = %s", out)
		}
	})

	t.Run("due_skips_ops_queued_behind_an_unresolved_one", func(t *testing.T) {
		l := nl(t)
		a, b, c := prepare(t, l, "a", "1"), prepare(t, l, "b", "1"), prepare(t, l, "c", "2")
		due, _ := l.Due(ctx, t0, 10)
		if len(due) != 2 || due[0].ID != a.ID || due[1].ID != c.ID {
			t.Fatalf("due = %+v, want a and c (b is behind a)", due)
		}
		if _, err := l.Reject(ctx, t0, a.ID, "x", ""); err != nil {
			t.Fatal(err)
		}
		if due, _ = l.Due(ctx, t0, 10); len(due) != 2 || due[0].ID != b.ID {
			t.Fatalf("due after a resolved = %+v, want b and c", due)
		}
	})

	t.Run("a_rejected_op_releases_its_entity", func(t *testing.T) {
		l := nl(t)
		a, b := prepare(t, l, "a", "1"), prepare(t, l, "b", "1")
		if _, err := l.Reject(ctx, t0, a.ID, "http_404", "gone"); err != nil {
			t.Fatal(err)
		}
		if out := claim(t, l, b.ID); out != ytwrite.ClaimClaimed {
			t.Fatalf("claim = %s", out)
		}
	})

	t.Run("concurrent_claims_have_one_winner", func(t *testing.T) {
		l := nl(t)
		a := prepare(t, l, "a", "1")
		var wins, errs int
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, out, err := l.Claim(ctx, t0, a.ID)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs++
				} else if out == ytwrite.ClaimClaimed {
					wins++
				}
			}()
		}
		wg.Wait()
		if wins != 1 || errs != 0 {
			t.Fatalf("winners = %d errors = %d, want 1 and 0", wins, errs)
		}
	})

	t.Run("concurrent_prepare_and_claim_on_one_entity_dispatch_the_oldest_first", func(t *testing.T) {
		l := nl(t)
		const n = 16
		claimed := make(chan int64, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				op, _, err := l.Prepare(ctx, t0, newOp("k"+itoa(int64(i)), "7"))
				if err != nil {
					t.Error(err)
					return
				}
				if _, out, err := l.Claim(ctx, t0, op.ID); err != nil {
					t.Error(err)
				} else if out == ytwrite.ClaimClaimed {
					claimed <- op.ID
				}
			}()
		}
		wg.Wait()
		close(claimed)
		var got []int64
		for id := range claimed {
			got = append(got, id)
		}
		// Nothing resolves, so at most one op may have been dispatched, and no
		// op may have jumped an earlier one that was already being prepared.
		if len(got) > 1 {
			t.Fatalf("%d ops dispatched on one entity: %v", len(got), got)
		}
		if len(got) == 1 {
			dispatched, _ := l.ListState(ctx, ytwrite.StateDispatched, 10)
			prepared, _ := l.ListState(ctx, ytwrite.StatePrepared, 100)
			for _, p := range prepared {
				if p.ID < got[0] {
					t.Fatalf("op %d was dispatched past the earlier op %d (dispatched %v)", got[0], p.ID, dispatched)
				}
			}
		}
	})

	t.Run("apply_is_idempotent_and_conflicts_are_loud", func(t *testing.T) {
		l := nl(t)
		a, b := prepare(t, l, "a", "1"), prepare(t, l, "b", "2")
		if _, err := l.Apply(ctx, t0, a.ID, "c-1", nil); !errors.Is(err, ytwrite.ErrState) {
			t.Fatalf("apply from prepared = %v", err)
		}
		claim(t, l, a.ID)
		maps := []ytwrite.MapRow{{EntityKind: "comment", HKKey: "op:1", YTID: "c-1", YTParentID: "2-1"}}
		got, err := l.Apply(ctx, t0.Add(time.Second), a.ID, "c-1", maps)
		if err != nil || got.State != ytwrite.StateApplied || got.YTResultID != "c-1" || !got.ResolvedAt.Equal(t0.Add(time.Second)) {
			t.Fatalf("apply = %+v %v", got, err)
		}
		if _, err := l.Apply(ctx, t0, a.ID, "c-1", maps); err != nil {
			t.Fatalf("second apply with the same id = %v", err)
		}
		if _, err := l.Apply(ctx, t0, a.ID, "c-2", nil); !errors.Is(err, ytwrite.ErrConflictingResult) {
			t.Fatalf("apply with another id = %v", err)
		}
		m, ok, err := l.MapGet(ctx, "comment", "op:1")
		if err != nil || !ok || m.YTID != "c-1" || m.OpID != a.ID || m.YTParentID != "2-1" {
			t.Fatalf("map row = %+v %v %v", m, ok, err)
		}
		// An op whose map row collides with another op's applies nothing.
		claim(t, l, b.ID)
		clash := []ytwrite.MapRow{{EntityKind: "comment", HKKey: "op:2", YTID: "c-1"}}
		if _, err := l.Apply(ctx, t0, b.ID, "c-1", clash); !errors.Is(err, ytwrite.ErrConflictingResult) {
			t.Fatalf("apply with a colliding yt id = %v", err)
		}
		if bb, _ := l.ByID(ctx, b.ID); bb.State != ytwrite.StateDispatched {
			t.Fatalf("a refused apply moved the op to %s", bb.State)
		}
		if _, ok, _ := l.MapGet(ctx, "comment", "op:2"); ok {
			t.Fatal("a refused apply left a map row")
		}
		if _, err := l.Reject(ctx, t0, a.ID, "x", ""); !errors.Is(err, ytwrite.ErrState) {
			t.Fatalf("reject after applied = %v", err)
		}
	})

	t.Run("unknown_lifecycle_and_listings", func(t *testing.T) {
		l := nl(t)
		a, b, c := prepare(t, l, "a", "1"), prepare(t, l, "b", "2"), prepare(t, l, "c", "3")
		for _, op := range []ytwrite.Op{a, b} {
			claim(t, l, op.ID)
		}
		if _, err := l.MarkUnknown(ctx, t0, c.ID, t0, "x", ""); !errors.Is(err, ytwrite.ErrState) {
			t.Fatalf("mark unknown from prepared = %v", err)
		}
		if _, err := l.TouchUnknown(ctx, t0, a.ID, t0, "x", ""); !errors.Is(err, ytwrite.ErrState) {
			t.Fatalf("touch from dispatched = %v", err)
		}
		next := t0.Add(time.Minute)
		if got, err := l.MarkUnknown(ctx, t0, a.ID, next, ytwrite.ReasonTransport, "reset"); err != nil || got.State != ytwrite.StateRemoteUnknown || !got.NextAttemptAt.Equal(next) || got.LastError != "reset" {
			t.Fatalf("mark unknown = %+v %v", got, err)
		}
		if got, err := l.MarkUnknown(ctx, t0, a.ID, next.Add(time.Hour), "other", ""); err != nil || !got.NextAttemptAt.Equal(next) || got.Reason != ytwrite.ReasonTransport {
			t.Fatalf("repeated mark unknown changed the op: %+v %v", got, err)
		}
		if got, err := l.TouchUnknown(ctx, t0, a.ID, next.Add(time.Minute), ytwrite.ReasonNoEvidence, ""); err != nil || got.Reconciles != 1 || got.Reason != ytwrite.ReasonNoEvidence {
			t.Fatalf("touch = %+v %v", got, err)
		}
		if due, _ := l.Due(ctx, t0, 10); len(due) != 1 || due[0].ID != c.ID {
			t.Fatalf("due at t0 = %+v (c is prepared and due; a is unknown until later)", due)
		}
		if due, _ := l.Due(ctx, t0.Add(time.Hour), 10); len(due) != 2 || due[0].ID != a.ID || due[1].ID != c.ID {
			t.Fatalf("due later = %+v", due)
		}
		if due, _ := l.Due(ctx, t0.Add(time.Hour), 1); len(due) != 1 || due[0].ID != a.ID {
			t.Fatalf("due with limit 1 = %+v", due)
		}
		if st, _ := l.Stale(ctx, t0.Add(-time.Second), 10); len(st) != 0 {
			t.Fatalf("stale before dispatch = %+v", st)
		}
		if st, _ := l.Stale(ctx, t0, 10); len(st) != 1 || st[0].ID != b.ID {
			t.Fatalf("stale = %+v", st)
		}
		if un, _ := l.ListState(ctx, ytwrite.StateRemoteUnknown, 10); len(un) != 1 || un[0].ID != a.ID {
			t.Fatalf("unknown = %+v", un)
		}
		if _, err := l.Defer(ctx, t0, a.ID, t0, "x", ""); !errors.Is(err, ytwrite.ErrState) {
			t.Fatalf("defer from unknown = %v", err)
		}
		if got, err := l.Reject(ctx, t0, a.ID, ytwrite.ReasonOperatorNotDone, "by op"); err != nil || got.State != ytwrite.StateRejected {
			t.Fatalf("reject from unknown = %+v %v", got, err)
		}
	})
}
