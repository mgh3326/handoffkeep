package ytwritetest

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

const (
	testIssue = "2-3697"
	testTask  = 3697
)

// NewLedger returns a fresh, empty Ledger for one subtest.
type NewLedger func(t *testing.T) ytwrite.Ledger

type env struct {
	t      *testing.T
	clock  *Clock
	yt     *FakeYouTrack
	ledger ytwrite.Ledger
	p      *ytwrite.Protocol
	page   int
}

func newEnv(t *testing.T, nl NewLedger) *env {
	t.Helper()
	e := &env{t: t, clock: NewClock(), yt: NewFake(), ledger: nl(t)}
	e.restart(e.ledger)
	return e
}

// restart builds a new Protocol over l and the same remote: a process restart.
func (e *env) restart(l ytwrite.Ledger) {
	e.t.Helper()
	p, err := ytwrite.New(ytwrite.Config{Ledger: l, Remote: e.yt, Now: e.clock.Now, PageSize: e.page})
	if err != nil {
		e.t.Fatal(err)
	}
	e.p = p
}

func (e *env) req(key, body string) ytwrite.CommentRequest {
	return ytwrite.CommentRequest{Principal: "agent-a", Key: key, Actor: "worker", TaskID: testTask, YTIssueID: testIssue, Body: body}
}

func (e *env) append(key, body string) ytwrite.Result {
	e.t.Helper()
	r, err := e.p.AppendComment(context.Background(), e.req(key, body))
	if err != nil {
		e.t.Fatalf("AppendComment(%s): %v", key, err)
	}
	return r
}

func (e *env) drive() ytwrite.DriveStats {
	e.t.Helper()
	st, err := e.p.Drive(context.Background(), 100)
	if err != nil {
		e.t.Fatalf("Drive: %v", err)
	}
	return st
}

func (e *env) lookup(r ytwrite.Result) ytwrite.Result {
	e.t.Helper()
	got, err := e.p.Lookup(context.Background(), "agent-a", r.OpID)
	if err != nil {
		e.t.Fatalf("Lookup(%d): %v", r.OpID, err)
	}
	return got
}

func (e *env) wantStatus(r ytwrite.Result, status ytwrite.Status, state ytwrite.State, reason string) {
	e.t.Helper()
	if r.Status != status || r.State != state || r.Reason != reason {
		e.t.Fatalf("result = %s/%s/%q, want %s/%s/%q (op %d)", r.Status, r.State, r.Reason, status, state, reason, r.OpID)
	}
}

func (e *env) wantRemote(calls, commits int) {
	e.t.Helper()
	if e.yt.Calls() != calls || e.yt.Commits() != commits {
		e.t.Fatalf("remote calls=%d commits=%d, want %d and %d", e.yt.Calls(), e.yt.Commits(), calls, commits)
	}
}

// RunProtocol runs the protocol's public-contract fault suite over a Ledger:
// no false done, no duplicate create, key reuse, concurrent callers, crashes at
// each ledger/remote boundary, ordered writes to one entity.
func RunProtocol(t *testing.T, nl NewLedger) {
	t.Run("applied_end_to_end", func(t *testing.T) {
		e := newEnv(t, nl)
		r := e.append("k1", "첫 코멘트")
		e.wantStatus(r, ytwrite.StatusApplied, ytwrite.StateApplied, "")
		if r.YTID != "c-1" || r.Replayed || r.RetryAfter != 0 {
			t.Fatalf("result = %+v", r)
		}
		cs := e.yt.Comments(testIssue)
		if len(cs) != 1 {
			t.Fatalf("remote comments = %d", len(cs))
		}
		lines := strings.Split(cs[0].Text, "\n")
		actor, at, id, ok := ytwrite.ParseProvenance(lines[0])
		if !ok || actor != "worker" || id != r.OpID || !at.Equal(e.clock.Now()) {
			t.Fatalf("provenance line %q parsed as %q %v %d %v", lines[0], actor, at, id, ok)
		}
		if lines[len(lines)-1] != ytwrite.OpMarker(r.OpID) || !strings.Contains(cs[0].Text, "\n\n첫 코멘트\n\n") {
			t.Fatalf("comment text = %q", cs[0].Text)
		}
		m, ok, err := e.ledger.MapGet(context.Background(), "comment", "op:"+itoa(r.OpID))
		if err != nil || !ok || m.YTID != "c-1" || m.YTParentID != testIssue || m.OpID != r.OpID {
			t.Fatalf("map row = %+v %v %v", m, ok, err)
		}
		again := e.append("k1", "첫 코멘트")
		if !again.Replayed || again.OpID != r.OpID || again.Status != ytwrite.StatusApplied || again.YTID != "c-1" {
			t.Fatalf("replay = %+v", again)
		}
		e.wantRemote(1, 1)
	})

	t.Run("lost_reply_stays_pending_until_evidence", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: CommitDropReply})
		r := e.append("k1", "hello")
		e.wantStatus(r, ytwrite.StatusPending, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		if r.RetryAfter <= 0 || r.YTID != "" {
			t.Fatalf("pending result = %+v", r)
		}
		e.wantRemote(1, 1)
		// The same key while unknown is a replay: pending, nothing sent.
		again := e.append("k1", "hello")
		e.wantStatus(again, ytwrite.StatusPending, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		if !again.Replayed || again.OpID != r.OpID {
			t.Fatalf("replay = %+v", again)
		}
		if st := e.drive(); st.Reconciled != 0 {
			t.Fatalf("reconciled before due: %+v", st)
		}
		e.clock.Advance(time.Minute)
		if st := e.drive(); st.Reconciled != 1 || st.Applied != 1 {
			t.Fatalf("drive = %+v", st)
		}
		got := e.lookup(r)
		e.wantStatus(got, ytwrite.StatusApplied, ytwrite.StateApplied, "")
		if got.YTID != e.yt.Comments(testIssue)[0].ID {
			t.Fatalf("adopted %q, remote has %q", got.YTID, e.yt.Comments(testIssue)[0].ID)
		}
		e.wantRemote(1, 1)
	})

	t.Run("delayed_commit_beyond_reconciliation", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: DelayCommit})
		r := e.append("k1", "late")
		e.wantStatus(r, ytwrite.StatusPending, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		for i := 0; i < 3; i++ {
			e.clock.Advance(10 * time.Minute)
			e.drive()
			e.wantStatus(e.lookup(r), ytwrite.StatusPending, ytwrite.StateRemoteUnknown, ytwrite.ReasonNoEvidence)
		}
		e.wantRemote(1, 0) // absence kept it unknown; nothing was re-created
		unknown, err := e.ledger.ListState(context.Background(), ytwrite.StateRemoteUnknown, 10)
		if err != nil || len(unknown) != 1 || unknown[0].Reconciles < 3 {
			t.Fatalf("unresolved ops = %+v %v", unknown, err)
		}
		e.yt.ReleaseDelayed()
		e.clock.Advance(10 * time.Minute)
		e.drive()
		e.wantStatus(e.lookup(r), ytwrite.StatusApplied, ytwrite.StateApplied, "")
		e.wantRemote(1, 1)
	})

	t.Run("reply_without_id_is_uncertain", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: NoID})
		r := e.append("k1", "no id")
		e.wantStatus(r, ytwrite.StatusPending, ytwrite.StateRemoteUnknown, ytwrite.ReasonNoID)
		e.clock.Advance(time.Minute)
		e.drive()
		e.wantStatus(e.lookup(r), ytwrite.StatusApplied, ytwrite.StateApplied, "")
		e.wantRemote(1, 1)
	})

	t.Run("rate_limit_honours_retry_after", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: RateLimit, RetryAfter: 30 * time.Second})
		r := e.append("k1", "slow down")
		e.wantStatus(r, ytwrite.StatusPending, ytwrite.StatePrepared, ytwrite.ReasonRateLimited)
		if r.RetryAfter != 30*time.Second {
			t.Fatalf("retry hint = %v, want the server's 30s (not capped to 5s)", r.RetryAfter)
		}
		e.wantRemote(1, 0)
		e.clock.Advance(29 * time.Second)
		if st := e.drive(); st.Visited != 0 {
			t.Fatalf("dispatched before Retry-After: %+v", st)
		}
		e.wantRemote(1, 0)
		e.clock.Advance(2 * time.Second)
		e.drive()
		e.wantStatus(e.lookup(r), ytwrite.StatusApplied, ytwrite.StateApplied, "")
		e.wantRemote(2, 1) // the 429 was not processed, so the second call is the only commit
	})

	t.Run("unauthorized_waits_for_repair", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: Refuse, Status: http.StatusUnauthorized})
		r := e.append("k1", "token expired")
		e.wantStatus(r, ytwrite.StatusPending, ytwrite.StatePrepared, ytwrite.ReasonUnauthorized)
		e.clock.Advance(6 * time.Minute)
		e.drive()
		e.wantStatus(e.lookup(r), ytwrite.StatusApplied, ytwrite.StateApplied, "")
	})

	t.Run("server_error_is_never_recreated", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: ServerError, Status: http.StatusServiceUnavailable})
		r := e.append("k1", "maybe")
		e.wantStatus(r, ytwrite.StatusPending, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
		for i := 0; i < 5; i++ {
			e.clock.Advance(10 * time.Minute)
			e.drive()
		}
		e.wantStatus(e.lookup(r), ytwrite.StatusPending, ytwrite.StateRemoteUnknown, ytwrite.ReasonNoEvidence)
		e.wantRemote(1, 0)
		// A later write to the same task waits behind it.
		next := e.append("k2", "behind")
		e.wantStatus(next, ytwrite.StatusPending, ytwrite.StatePrepared, ytwrite.ReasonWaitingForEarlier)
		// The operator looked and says it never happened.
		if _, err := e.p.Resolve(context.Background(), next.OpID, ytwrite.Resolution{By: "operator", Note: "x"}); !errors.Is(err, ytwrite.ErrState) {
			t.Fatalf("resolve of a prepared op = %v, want ErrState", err)
		}
		res, err := e.p.Resolve(context.Background(), r.OpID, ytwrite.Resolution{By: "operator", Note: "checked the issue"})
		if err != nil {
			t.Fatal(err)
		}
		e.wantStatus(res, ytwrite.StatusRejected, ytwrite.StateRejected, ytwrite.ReasonOperatorNotDone)
		e.clock.Advance(time.Minute)
		e.drive()
		e.wantStatus(e.lookup(next), ytwrite.StatusApplied, ytwrite.StateApplied, "")
		e.wantRemote(2, 1)
	})

	t.Run("operator_can_adopt_an_unknown_op", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: ServerError, Commit: true})
		r := e.append("k1", "it did land")
		id := e.yt.Comments(testIssue)[0].ID
		res, err := e.p.Resolve(context.Background(), r.OpID, ytwrite.Resolution{Applied: true, YTID: id, By: "operator"})
		if err != nil {
			t.Fatal(err)
		}
		e.wantStatus(res, ytwrite.StatusApplied, ytwrite.StateApplied, "")
		if res.YTID != id {
			t.Fatalf("ytid = %q", res.YTID)
		}
		if _, err := e.p.Resolve(context.Background(), r.OpID, ytwrite.Resolution{By: "operator"}); !errors.Is(err, ytwrite.ErrState) {
			t.Fatalf("second resolve = %v, want ErrState", err)
		}
	})

	t.Run("final_refusal_is_rejected_and_replayed_without_sending", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: Refuse, Status: http.StatusNotFound})
		r := e.append("k1", "gone")
		e.wantStatus(r, ytwrite.StatusRejected, ytwrite.StateRejected, "http_404")
		again := e.append("k1", "gone")
		e.wantStatus(again, ytwrite.StatusRejected, ytwrite.StateRejected, "http_404")
		e.wantRemote(1, 0)
	})

	t.Run("key_reuse_and_principal_scope", func(t *testing.T) {
		e := newEnv(t, nl)
		r := e.append("k1", "A")
		_, err := e.p.AppendComment(context.Background(), e.req("k1", "B"))
		if !errors.Is(err, ytwrite.ErrKeyConflict) {
			t.Fatalf("same key, other body = %v, want ErrKeyConflict", err)
		}
		actor := e.req("k1", "A")
		actor.Actor = "another-worker"
		if _, err := e.p.AppendComment(context.Background(), actor); !errors.Is(err, ytwrite.ErrKeyConflict) {
			t.Fatalf("same key, other actor = %v, want ErrKeyConflict", err)
		}
		e.wantRemote(1, 1)
		other := e.req("k1", "B")
		other.Principal = "agent-b"
		ro, err := e.p.AppendComment(context.Background(), other)
		if err != nil || ro.OpID == r.OpID || ro.Status != ytwrite.StatusApplied {
			t.Fatalf("other principal, same key = %+v %v", ro, err)
		}
		if _, err := e.p.Lookup(context.Background(), "agent-b", r.OpID); !errors.Is(err, ytwrite.ErrNotFound) {
			t.Fatalf("lookup of another principal's op = %v, want ErrNotFound", err)
		}
		// Two intentional identical comments are two operations, told apart by key.
		e.append("k2", "A")
		e.wantRemote(3, 3)
	})

	t.Run("empty_key_returns_a_handle_and_does_not_deduplicate", func(t *testing.T) {
		e := newEnv(t, nl)
		a, b := e.append("", "same"), e.append("", "same")
		if a.OpID == b.OpID || a.Status != ytwrite.StatusApplied || b.Status != ytwrite.StatusApplied {
			t.Fatalf("a=%+v b=%+v", a, b)
		}
	})

	t.Run("concurrent_same_key_callers_send_once", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.SetLatency(5 * time.Millisecond)
		const n = 12
		results := make([]ytwrite.Result, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = e.p.AppendComment(context.Background(), e.req("same", "once"))
			}()
		}
		wg.Wait()
		for i := range results {
			if errs[i] != nil || results[i].OpID != results[0].OpID {
				t.Fatalf("caller %d: %+v %v (want op %d)", i, results[i], errs[i], results[0].OpID)
			}
			if results[i].Status == ytwrite.StatusApplied && results[i].YTID == "" {
				t.Fatalf("caller %d told applied without an id: %+v", i, results[i])
			}
		}
		e.wantStatus(e.lookup(results[0]), ytwrite.StatusApplied, ytwrite.StateApplied, "")
		e.wantRemote(1, 1)
	})

	t.Run("two_writes_to_one_entity_apply_in_order", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: CommitDropReply})
		a := e.append("ka", "first")
		b := e.append("kb", "second")
		e.wantStatus(a, ytwrite.StatusPending, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		e.wantStatus(b, ytwrite.StatusPending, ytwrite.StatePrepared, ytwrite.ReasonWaitingForEarlier)
		e.wantRemote(1, 1) // the second was not sent past the unresolved first
		// Another task is another lane and is not held.
		other := e.req("kc", "elsewhere")
		other.TaskID, other.YTIssueID = 9, "2-9"
		if ro, err := e.p.AppendComment(context.Background(), other); err != nil || ro.Status != ytwrite.StatusApplied {
			t.Fatalf("other lane = %+v %v", ro, err)
		}
		e.clock.Advance(time.Minute)
		e.drive()
		e.wantStatus(e.lookup(a), ytwrite.StatusApplied, ytwrite.StateApplied, "")
		e.wantStatus(e.lookup(b), ytwrite.StatusApplied, ytwrite.StateApplied, "")
		cs := e.yt.Comments(testIssue)
		if len(cs) != 2 || !strings.Contains(cs[0].Text, "first") || !strings.Contains(cs[1].Text, "second") {
			t.Fatalf("remote order = %+v", cs)
		}
	})

	t.Run("a_rate_limited_write_holds_later_writes", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: RateLimit, RetryAfter: time.Minute})
		a := e.append("ka", "first")
		b := e.append("kb", "second")
		e.wantStatus(a, ytwrite.StatusPending, ytwrite.StatePrepared, ytwrite.ReasonRateLimited)
		e.wantStatus(b, ytwrite.StatusPending, ytwrite.StatePrepared, ytwrite.ReasonWaitingForEarlier)
		e.wantRemote(1, 0)
		e.clock.Advance(2 * time.Minute)
		e.drive()
		cs := e.yt.Comments(testIssue)
		if len(cs) != 2 || !strings.Contains(cs[0].Text, "first") || !strings.Contains(cs[1].Text, "second") {
			t.Fatalf("remote order = %+v", cs)
		}
	})

	t.Run("evidence_search_pages_and_demands_the_exact_body", func(t *testing.T) {
		e := newEnv(t, nl)
		e.page = 2
		e.restart(e.ledger)
		e.yt.SetPageCap(2)
		for i := 0; i < 3; i++ {
			e.yt.Inject(testIssue, "someone else "+itoa(int64(i)))
		}
		e.yt.Inject(testIssue, "lookalike\n\n"+ytwrite.OpMarker(1))
		e.yt.Queue(Fault{Kind: CommitDropReply})
		r := e.append("k1", "real")
		if r.OpID != 1 {
			t.Fatalf("op id = %d, the lookalike targets op 1", r.OpID)
		}
		for i := 0; i < 3; i++ {
			e.yt.Inject(testIssue, "after "+itoa(int64(i)))
		}
		e.clock.Advance(time.Minute)
		e.drive()
		got := e.lookup(r)
		e.wantStatus(got, ytwrite.StatusPending, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)
		real := ""
		for _, c := range e.yt.Comments(testIssue) {
			if strings.Contains(c.Text, "\n\nreal\n\n") {
				real = c.ID
			}
		}
		if real == "" || got.YTID != "" {
			t.Fatalf("adopted %q, the real comment is %q", got.YTID, real)
		}
	})

	t.Run("evidence_search_at_old_page_limit", func(t *testing.T) {
		e := newEnv(t, nl)
		e.page = 1
		e.restart(e.ledger)
		for i := 0; i < 1999; i++ {
			e.yt.Inject(testIssue, "unrelated")
		}
		e.yt.Queue(Fault{Kind: CommitDropReply})
		r := e.append("limit", "present")
		e.clock.Advance(time.Minute)
		e.drive()
		e.wantStatus(e.lookup(r), ytwrite.StatusApplied, ytwrite.StateApplied, "")
	})

	for _, tc := range []struct {
		name   string
		mutate func(y *FakeYouTrack, issue, id string)
		reason string
	}{
		{"two_identical_matches", func(y *FakeYouTrack, issue, id string) { y.Duplicate(issue, id) }, ytwrite.ReasonMultipleMatches},
		{"live_and_deleted", func(y *FakeYouTrack, issue, id string) { y.Delete(issue, y.Duplicate(issue, id)) }, ytwrite.ReasonMarkerDeleted},
		{"live_and_modified", func(y *FakeYouTrack, issue, id string) { y.Inject(issue, "changed "+ytwrite.OpMarker(1)) }, ytwrite.ReasonMarkerMismatch},
		{"match_was_deleted", func(y *FakeYouTrack, issue, id string) { y.Delete(issue, id) }, ytwrite.ReasonMarkerDeleted},
	} {
		t.Run("ambiguous_evidence_stays_unknown/"+tc.name, func(t *testing.T) {
			e := newEnv(t, nl)
			e.yt.Queue(Fault{Kind: CommitDropReply})
			r := e.append("k1", "x")
			tc.mutate(e.yt, testIssue, e.yt.Comments(testIssue)[0].ID)
			e.clock.Advance(time.Minute)
			e.drive()
			e.wantStatus(e.lookup(r), ytwrite.StatusPending, ytwrite.StateRemoteUnknown, tc.reason)
		})
	}

	t.Run("offset_shift_stays_unknown", func(t *testing.T) {
		e := newEnv(t, nl)
		e.page = 1
		e.restart(e.ledger)
		e.yt.Inject(testIssue, "unrelated")
		e.yt.Queue(Fault{Kind: CommitDropReply})
		r := e.append("shift", "present")
		e.yt.ShiftPages()
		e.clock.Advance(time.Minute)
		e.drive()
		e.wantStatus(e.lookup(r), ytwrite.StatusPending, ytwrite.StateRemoteUnknown, ytwrite.ReasonReconcileFailed)
		e.wantRemote(1, 1)
	})

	t.Run("evidence_search_failure_stays_unknown", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: CommitDropReply})
		r := e.append("k1", "x")
		e.yt.FailLists(1)
		e.clock.Advance(time.Minute)
		e.drive()
		e.wantStatus(e.lookup(r), ytwrite.StatusPending, ytwrite.StateRemoteUnknown, ytwrite.ReasonReconcileFailed)
		e.clock.Advance(10 * time.Minute)
		e.drive()
		e.wantStatus(e.lookup(r), ytwrite.StatusApplied, ytwrite.StateApplied, "")
	})

	t.Run("invalid_requests_store_nothing", func(t *testing.T) {
		e := newEnv(t, nl)
		for name, mut := range map[string]func(*ytwrite.CommentRequest){
			"marker in body":  func(r *ytwrite.CommentRequest) { r.Body = "spoof <!-- hk-op:1 -->" },
			"nul in body":     func(r *ytwrite.CommentRequest) { r.Body = "a\x00b" },
			"bad utf8 body":   func(r *ytwrite.CommentRequest) { r.Body = "a\xffb" },
			"empty body":      func(r *ytwrite.CommentRequest) { r.Body = "  \n" },
			"huge body":       func(r *ytwrite.CommentRequest) { r.Body = strings.Repeat("x", ytwrite.MaxBodyBytes+1) },
			"separator actor": func(r *ytwrite.CommentRequest) { r.Actor = "a · b" },
			"newline actor":   func(r *ytwrite.CommentRequest) { r.Actor = "a\nb" },
			"no principal":    func(r *ytwrite.CommentRequest) { r.Principal = "" },
			"zero task":       func(r *ytwrite.CommentRequest) { r.TaskID = 0 },
			"bad issue id":    func(r *ytwrite.CommentRequest) { r.YTIssueID = "../x" },
			"bad key":         func(r *ytwrite.CommentRequest) { r.Key = "has space" },
		} {
			req := e.req("kx", "ok")
			mut(&req)
			if _, err := e.p.AppendComment(context.Background(), req); !errors.Is(err, ytwrite.ErrInvalid) {
				t.Errorf("%s: err = %v, want ErrInvalid", name, err)
			}
		}
		if due, _ := e.ledger.Due(context.Background(), e.clock.Now().Add(time.Hour), 10); len(due) != 0 {
			t.Fatalf("invalid requests left %d ops", len(due))
		}
		e.wantRemote(0, 0)
	})

	t.Run("caller_cancellation_does_not_cancel_the_write", func(t *testing.T) {
		e := newEnv(t, nl)
		ctx, cancel := context.WithCancel(context.Background())
		e.yt.BeforeCreate = func(context.Context) { cancel() }
		r, err := e.p.AppendComment(ctx, e.req("k1", "walked away"))
		if err != nil {
			t.Fatal(err)
		}
		e.wantStatus(r, ytwrite.StatusApplied, ytwrite.StateApplied, "")
		e.wantRemote(1, 1)
	})

	t.Run("crash_at_each_boundary", func(t *testing.T) {
		type want struct {
			state   ytwrite.State // after restart and enough Drive passes
			commits int
			calls   int
		}
		for _, tc := range []struct {
			name   string
			method string
			after  bool
			fault  *Fault
			want   want
		}{
			{"before_prepare_commit", "Prepare", false, nil, want{ytwrite.StateApplied, 1, 1}},
			{"after_prepare_commit", "Prepare", true, nil, want{ytwrite.StateApplied, 1, 1}},
			{"before_claim_commit", "Claim", false, nil, want{ytwrite.StateApplied, 1, 1}},
			// Dispatch ownership committed, then the process died before it
			// sent. It cannot tell that from "sent", so it stays unknown: no
			// resend, no false done.
			{"after_claim_commit_before_send", "Claim", true, nil, want{ytwrite.StateRemoteUnknown, 0, 0}},
			{"remote_commit_before_apply_commit", "Apply", false, nil, want{ytwrite.StateApplied, 1, 1}},
			{"after_apply_commit", "Apply", true, nil, want{ytwrite.StateApplied, 1, 1}},
			{"lost_reply_before_unknown_recorded", "MarkUnknown", false, &Fault{Kind: CommitDropReply}, want{ytwrite.StateApplied, 1, 1}},
			{"lost_reply_after_unknown_recorded", "MarkUnknown", true, &Fault{Kind: CommitDropReply}, want{ytwrite.StateApplied, 1, 1}},
			{"before_defer_commit", "Defer", false, &Fault{Kind: RateLimit, RetryAfter: time.Minute}, want{ytwrite.StateRemoteUnknown, 0, 1}},
			{"after_defer_commit", "Defer", true, &Fault{Kind: RateLimit, RetryAfter: time.Minute}, want{ytwrite.StateApplied, 1, 2}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := newEnv(t, nl)
				cl := &CrashLedger{Ledger: e.ledger, Method: tc.method, After: tc.after}
				e.restart(cl)
				if tc.fault != nil {
					e.yt.Queue(*tc.fault)
				}
				first, err := e.p.AppendComment(context.Background(), e.req("k1", "crashy"))
				if !cl.Dead() {
					t.Fatalf("the crash at %s never fired", tc.method)
				}
				if err == nil && first.Status == ytwrite.StatusApplied {
					if op, gerr := e.ledger.ByID(context.Background(), first.OpID); gerr != nil || op.State != ytwrite.StateApplied {
						t.Fatalf("caller was told applied but the ledger says %+v %v", op, gerr)
					}
				}
				if tc.name == "after_defer_commit" {
					op, err := e.ledger.ByID(context.Background(), 1)
					if err != nil || op.State != ytwrite.StatePrepared {
						t.Fatalf("after defer: %+v %v", op, err)
					}
				}
				// A new process: retry the caller's key and run the worker until
				// everything that can resolve has.
				e.restart(e.ledger)
				if tc.name == "after_defer_commit" {
					e.drive()
					e.wantRemote(1, 0)
				}
				if _, err := e.p.AppendComment(context.Background(), e.req("k1", "crashy")); err != nil {
					t.Fatalf("retry after the crash: %v", err)
				}
				for i := 0; i < 4; i++ {
					e.clock.Advance(10 * time.Minute)
					e.drive()
				}
				res, err := e.p.Lookup(context.Background(), "agent-a", 1)
				if err != nil {
					t.Fatal(err)
				}
				if res.State != tc.want.state {
					t.Fatalf("final state = %s (%s), want %s", res.State, res.Reason, tc.want.state)
				}
				if tc.want.state == ytwrite.StateApplied && res.YTID == "" {
					t.Fatalf("applied without an id: %+v", res)
				}
				e.wantRemote(tc.want.calls, tc.want.commits)
			})
		}
	})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
