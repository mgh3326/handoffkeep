package ytwritetest

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The issue_create fixture of the shared fault suite (hk 3910): the op is
// prepared as the store prepares it (one per task, key IssueKey) and the
// worker's Drive sends it; the fake fails CreateIssue the way RunProtocol's
// fake fails CreateComment.
const (
	issueSuiteProject = "0-7"
	issueSuiteTenant  = "tenant.youtrack.cloud"
)

func issueSuiteReq() ytwrite.IssueRequest {
	return ytwrite.IssueRequest{Principal: "hk:task-issues", Actor: "worker", TaskID: testTask, Title: "issue suite",
		Project: "handoffkeep", YTProjectID: issueSuiteProject, YTProjectShort: "HK", Tenant: issueSuiteTenant}
}

// prepareIssue stores the task's issue_create op through l.
func (e *env) prepareIssue(l ytwrite.Ledger) (ytwrite.Op, bool, error) {
	n, err := ytwrite.IssueOp(issueSuiteReq())
	if err != nil {
		e.t.Fatal(err)
	}
	return l.Prepare(context.Background(), e.clock.Now(), n)
}

func (e *env) mustPrepareIssue() ytwrite.Op {
	e.t.Helper()
	op, _, err := e.prepareIssue(e.ledger)
	if err != nil {
		e.t.Fatal(err)
	}
	return op
}

func (e *env) wantOp(id int64, state ytwrite.State, reason string) ytwrite.Op {
	e.t.Helper()
	op, err := e.ledger.ByID(context.Background(), id)
	if err != nil || op.State != state || op.Reason != reason {
		e.t.Fatalf("op %d = %s/%q %v, want %s/%q", id, op.State, op.Reason, err, state, reason)
	}
	return op
}

// wantIssue checks that the applied op names the one issue the project has
// and that its task map row points at it.
func (e *env) wantIssue(op ytwrite.Op) {
	e.t.Helper()
	issues := e.yt.Issues(issueSuiteProject)
	if len(issues) != 1 || op.YTResultID != issues[0].ID {
		e.t.Fatalf("op %d applied with %q, project has %+v", op.ID, op.YTResultID, issues)
	}
	m, ok, err := e.ledger.MapGet(context.Background(), "task", itoa(testTask))
	if err != nil || !ok || m.YTID != op.YTResultID || m.YTParentID != issueSuiteProject || m.OpID != op.ID {
		e.t.Fatalf("task map row %+v %v %v", m, ok, err)
	}
}

// RunIssueProtocol runs RunProtocol's fault cases for the issue_create kind:
// no false done, no duplicate create (a lost or delayed reply is never
// re-created), retry hints, operator resolution, concurrent workers and a
// crash at each ledger/remote boundary.
func RunIssueProtocol(t *testing.T, nl NewLedger) {
	t.Run("applied_end_to_end", func(t *testing.T) {
		e := newEnv(t, nl)
		op := e.mustPrepareIssue()
		e.drive()
		got := e.wantOp(op.ID, ytwrite.StateApplied, "")
		e.wantIssue(got)
		again, created, err := e.prepareIssue(e.ledger)
		if err != nil || created || again.ID != op.ID || again.State != ytwrite.StateApplied {
			t.Fatalf("replay = %+v %v %v", again, created, err)
		}
		e.drive()
		e.wantRemote(1, 1)
	})

	t.Run("lost_reply_stays_pending_until_evidence", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: CommitDropReply})
		op := e.mustPrepareIssue()
		e.drive()
		e.wantOp(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		if st := e.drive(); st.Reconciled != 0 {
			t.Fatalf("reconciled before due: %+v", st)
		}
		e.clock.Advance(time.Minute)
		if st := e.drive(); st.Reconciled != 1 || st.Applied != 1 {
			t.Fatalf("drive = %+v", st)
		}
		e.wantIssue(e.wantOp(op.ID, ytwrite.StateApplied, ""))
		e.wantRemote(1, 1)
	})

	t.Run("delayed_commit_beyond_reconciliation", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: DelayCommit})
		op := e.mustPrepareIssue()
		e.drive()
		e.wantOp(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		for i := 0; i < 3; i++ {
			e.clock.Advance(10 * time.Minute)
			e.drive()
			e.wantOp(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonNoEvidence)
		}
		e.wantRemote(1, 0) // absence kept it unknown; nothing was re-created
		e.yt.ReleaseDelayed()
		e.clock.Advance(10 * time.Minute)
		e.drive()
		e.wantIssue(e.wantOp(op.ID, ytwrite.StateApplied, ""))
		e.wantRemote(1, 1)
	})

	t.Run("reply_without_id_is_uncertain", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: NoID})
		op := e.mustPrepareIssue()
		e.drive()
		e.wantOp(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonNoID)
		e.clock.Advance(time.Minute)
		e.drive()
		e.wantIssue(e.wantOp(op.ID, ytwrite.StateApplied, ""))
		e.wantRemote(1, 1)
	})

	t.Run("rate_limit_honours_retry_after", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: RateLimit, RetryAfter: 30 * time.Second})
		op := e.mustPrepareIssue()
		e.drive()
		got := e.wantOp(op.ID, ytwrite.StatePrepared, ytwrite.ReasonRateLimited)
		if want := e.clock.Now().Add(30 * time.Second); !got.NextAttemptAt.Equal(want) {
			t.Fatalf("next attempt %v, want the server's 30s (%v)", got.NextAttemptAt, want)
		}
		e.clock.Advance(29 * time.Second)
		if st := e.drive(); st.Visited != 0 {
			t.Fatalf("dispatched before Retry-After: %+v", st)
		}
		e.clock.Advance(2 * time.Second)
		e.drive()
		e.wantIssue(e.wantOp(op.ID, ytwrite.StateApplied, ""))
		e.wantRemote(2, 1)
	})

	t.Run("unauthorized_waits_for_repair", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: Refuse, Status: http.StatusUnauthorized})
		op := e.mustPrepareIssue()
		e.drive()
		e.wantOp(op.ID, ytwrite.StatePrepared, ytwrite.ReasonUnauthorized)
		e.clock.Advance(6 * time.Minute)
		e.drive()
		e.wantIssue(e.wantOp(op.ID, ytwrite.StateApplied, ""))
	})

	t.Run("server_error_is_never_recreated", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: ServerError, Status: http.StatusServiceUnavailable})
		op := e.mustPrepareIssue()
		e.drive()
		e.wantOp(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
		for i := 0; i < 5; i++ {
			e.clock.Advance(10 * time.Minute)
			e.drive()
		}
		e.wantOp(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonNoEvidence)
		e.wantRemote(1, 0)
		res, err := e.p.Resolve(context.Background(), op.ID, ytwrite.Resolution{By: "operator", Note: "checked the project"})
		if err != nil {
			t.Fatal(err)
		}
		e.wantStatus(res, ytwrite.StatusRejected, ytwrite.StateRejected, ytwrite.ReasonOperatorNotDone)
		e.clock.Advance(time.Hour)
		e.drive()
		e.wantRemote(1, 0)
	})

	t.Run("operator_can_apply_an_unknown_op", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: ServerError, Commit: true})
		op := e.mustPrepareIssue()
		e.drive()
		created := false
		res, err := e.p.Resolve(context.Background(), op.ID, ytwrite.Resolution{Applied: true, YTID: e.yt.Issues(issueSuiteProject)[0].ID, Adopted: &created, By: "operator"})
		if err != nil {
			t.Fatal(err)
		}
		e.wantStatus(res, ytwrite.StatusApplied, ytwrite.StateApplied, "")
		e.wantIssue(e.wantOp(op.ID, ytwrite.StateApplied, ""))
		e.wantRemote(1, 1)
	})

	t.Run("final_refusal_is_rejected_and_never_resent", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.Queue(Fault{Kind: Refuse, Status: http.StatusNotFound})
		op := e.mustPrepareIssue()
		e.drive()
		e.wantOp(op.ID, ytwrite.StateRejected, "http_404")
		if again, created, err := e.prepareIssue(e.ledger); err != nil || created || again.State != ytwrite.StateRejected {
			t.Fatalf("replay = %+v %v %v", again, created, err)
		}
		e.clock.Advance(time.Hour)
		e.drive()
		e.wantRemote(1, 0)
	})

	t.Run("concurrent_workers_send_once", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.SetLatency(5 * time.Millisecond)
		op := e.mustPrepareIssue()
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := e.p.Drive(context.Background(), 10); err != nil {
					t.Errorf("Drive: %v", err)
				}
			}()
		}
		wg.Wait()
		e.wantIssue(e.wantOp(op.ID, ytwrite.StateApplied, ""))
		e.wantRemote(1, 1)
	})

	// Send adopted the task's existing issue and the process died before the
	// apply committed. The adopted issue carries no marker, and nothing durable
	// says this dispatch adopted rather than POSTed (its hk ID may have been
	// set after the search, with a POST still in flight), so the op stays
	// remote_unknown for the operator and nothing is re-created (PR #335
	// review; hk 3911 needs durable adoption evidence first).
	t.Run("lost_adoption_settlement_stays_unknown", func(t *testing.T) {
		e := newEnv(t, nl)
		e.yt.InjectIssue(issueSuiteProject, testTask, "#"+itoa(testTask)+" imported", "legacy body")
		e.clock.Advance(time.Hour)
		cl := &CrashLedger{Ledger: e.ledger, Method: "Apply"}
		e.restart(cl)
		if _, _, err := e.prepareIssue(cl); err != nil {
			t.Fatal(err)
		}
		_, _ = e.p.Drive(context.Background(), 10)
		if !cl.Dead() {
			t.Fatal("the crash at Apply never fired")
		}
		e.restart(e.ledger)
		e.clock.Advance(10 * time.Minute)
		e.drive()
		e.wantOp(1, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)
		e.wantRemote(0, 0)
	})

	t.Run("crash_at_each_boundary", func(t *testing.T) {
		for _, tc := range []struct {
			name           string
			method         string
			after          bool
			fault          *Fault
			state          ytwrite.State
			calls, commits int
		}{
			{"before_prepare_commit", "Prepare", false, nil, ytwrite.StateApplied, 1, 1},
			{"after_prepare_commit", "Prepare", true, nil, ytwrite.StateApplied, 1, 1},
			{"before_claim_commit", "Claim", false, nil, ytwrite.StateApplied, 1, 1},
			// Dispatch ownership committed, then the process died before it
			// sent: it cannot tell that from "sent", so it stays unknown.
			{"after_claim_commit_before_send", "Claim", true, nil, ytwrite.StateRemoteUnknown, 0, 0},
			{"remote_commit_before_apply_commit", "Apply", false, nil, ytwrite.StateApplied, 1, 1},
			{"after_apply_commit", "Apply", true, nil, ytwrite.StateApplied, 1, 1},
			{"lost_reply_before_unknown_recorded", "MarkUnknown", false, &Fault{Kind: CommitDropReply}, ytwrite.StateApplied, 1, 1},
			{"lost_reply_after_unknown_recorded", "MarkUnknown", true, &Fault{Kind: CommitDropReply}, ytwrite.StateApplied, 1, 1},
			{"before_defer_commit", "Defer", false, &Fault{Kind: RateLimit, RetryAfter: time.Minute}, ytwrite.StateRemoteUnknown, 1, 0},
			{"after_defer_commit", "Defer", true, &Fault{Kind: RateLimit, RetryAfter: time.Minute}, ytwrite.StateApplied, 2, 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := newEnv(t, nl)
				cl := &CrashLedger{Ledger: e.ledger, Method: tc.method, After: tc.after}
				e.restart(cl)
				if tc.fault != nil {
					e.yt.Queue(*tc.fault)
				}
				if _, _, err := e.prepareIssue(cl); err == nil {
					// The dead process's pass ends in errors; what it
					// committed is what the restart finds.
					_, _ = e.p.Drive(context.Background(), 10)
				}
				if !cl.Dead() {
					t.Fatalf("the crash at %s never fired", tc.method)
				}
				e.restart(e.ledger)
				if _, _, err := e.prepareIssue(e.ledger); err != nil {
					t.Fatalf("prepare after the crash: %v", err)
				}
				for i := 0; i < 4; i++ {
					e.clock.Advance(10 * time.Minute)
					e.drive()
				}
				op, err := e.ledger.ByID(context.Background(), 1)
				if err != nil || op.State != tc.state {
					t.Fatalf("final op = %+v %v, want %s", op, err, tc.state)
				}
				if tc.state == ytwrite.StateApplied {
					e.wantIssue(op)
				}
				e.wantRemote(tc.calls, tc.commits)
			})
		}
	})
}
