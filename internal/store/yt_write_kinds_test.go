package store

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Every admission gate names a kind the protocol can send: a gate for an
// unregistered kind would admit ops that send only rejects.
func TestYTKindGatesNameRegisteredKinds(t *testing.T) {
	registered := ytwrite.RegisteredKinds()
	for k := range ytKindGates {
		if !slices.Contains(registered, k) {
			t.Errorf("gate for unregistered kind %q (registered %v)", k, registered)
		}
	}
	if _, ok := ytKindGates[ytwrite.KindCommentAppend]; !ok {
		t.Fatal("comment_append has no gate")
	}
}

// The per-kind admission keeps the comment gate exactly as it was, and the
// issue_create gate admits nothing while the issues mode is off.
func TestYTAdmitSQLPerKind(t *testing.T) {
	s := &Store{}
	for _, tc := range []struct {
		mode YTCommentsMode
		gate string
	}{
		{YTCommentsOff, `FALSE`},
		{YTCommentsCanary, `EXISTS (SELECT 1 FROM yt_comment_canary c
		WHERE o.entity_kind = 'task' AND c.task_id::text = o.entity_id AND c.issue_id = o.yt_target_id AND c.tenant = o.tenant)`},
		{YTCommentsOn, `o.entity_kind = 'task' AND EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant)`},
	} {
		s.SetYTCommentsMode(tc.mode)
		want := `(o.tenant = $2 AND ((o.kind = 'comment_append' AND (` + tc.gate + `)) OR (o.kind = 'issue_create' AND (FALSE))))`
		if got := s.ytAdmitSQL("$2"); got != want {
			t.Errorf("mode %s:\n got %s\nwant %s", tc.mode, got, want)
		}
	}
}

// An op of a kind with no gate is never admitted by the tenant's worker, even
// on a task whose binding (mode on) or canary row (mode canary) admits the
// comment op beside it.
func TestYTUnregisteredKindNeverAdmitted(t *testing.T) {
	for _, mode := range []YTCommentsMode{YTCommentsOn, YTCommentsCanary} {
		t.Run(string(mode), func(t *testing.T) {
			s := schemaStore(t, "ytk_"+string(mode))
			ctx := t.Context()
			ytbMapped(t, s)
			s.SetYTCommentsMode(mode)
			admit := func(task int64) {
				t.Helper()
				if mode == YTCommentsOn {
					ytbBind(t, s, ytbBinding(task))
				} else {
					ytcEnroll(t, s, task)
				}
			}
			// Separate tasks, so neither op queues behind the other's lane.
			otherTask, commentTask := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
			admit(otherTask)
			admit(commentTask)
			id := strconv.FormatInt(otherTask, 10)
			other, _, err := s.YTWriteLedger().Prepare(ctx, time.Now().UTC(), ytwrite.NewOp{
				Principal: YTCommentPrincipal, Kind: "issue_create_test", Key: "k-" + id, PayloadSHA: "sha", Payload: []byte(`{}`),
				EntityKind: "task", EntityID: id, YTTargetID: "2-" + id, Tenant: ytcTenant, Actor: "worker",
			})
			if err != nil {
				t.Fatal(err)
			}
			c := ytbComment(t, s, commentTask, "admitted "+string(mode))
			comment, ok := ytcOpFor(t, s, c.ID)
			if !ok || comment.State != ytwrite.StatePrepared || comment.Tenant != ytcTenant {
				t.Fatalf("comment op %+v %v", comment, ok)
			}

			ledger := s.YTTenantLedger(ytcTenant)
			now := time.Now().UTC().Add(time.Minute)
			due, err := ledger.Due(ctx, now, 50)
			if err != nil {
				t.Fatal(err)
			}
			listed := func(id int64) bool { return slices.ContainsFunc(due, func(o ytwrite.Op) bool { return o.ID == id }) }
			if listed(other.ID) || !listed(comment.ID) {
				t.Fatalf("Due %v: other kind listed %v, comment listed %v", opIDs(due), listed(other.ID), listed(comment.ID))
			}
			if got, out, err := ledger.Claim(ctx, now, other.ID); err != nil || out != ytwrite.ClaimHeld || got.State != ytwrite.StatePrepared {
				t.Fatalf("claim of the other kind: %s %+v %v, want held", out, got, err)
			}
			if got, out, err := ledger.Claim(ctx, now, comment.ID); err != nil || out != ytwrite.ClaimClaimed || got.State != ytwrite.StateDispatched {
				t.Fatalf("claim of the comment op: %s %+v %v, want claimed", out, got, err)
			}
		})
	}
}

func opIDs(ops []ytwrite.Op) string {
	ids := make([]string, len(ops))
	for i, o := range ops {
		ids[i] = strconv.FormatInt(o.ID, 10)
	}
	return strings.Join(ids, ",")
}
