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

// The comment_append gate admits an hk comment's copy by the comments mode
// and never a document copy (principal hk:doc-comments), which the document
// mode admits alone. The issue_create gate admits nothing while the issues
// mode is off.
func TestYTAdmitSQLPerKind(t *testing.T) {
	s := &Store{}
	// issue_fields and issue_move are FALSE until their modes are on: moves
	// have their own mode (hk 4111), off by default.
	const noFields = `(o.kind = 'issue_fields' AND (FALSE))`
	const noMove = `(o.kind = 'issue_move' AND (FALSE))`
	// issue_links admits nothing until its consumer and mode land (hk 4638 S3).
	const noLinks = `(o.kind = 'issue_links' AND (FALSE))`
	// The document kinds (hk 4206) are FALSE until the docs mode is on.
	const noArticles = `(o.kind = 'article_create' AND (FALSE)) OR (o.kind = 'article_update' AND (FALSE))`
	const noDecisions = `(o.kind = 'decision_create' AND (FALSE)) OR (o.kind = 'decision_fields' AND (FALSE))`
	for _, tc := range []struct {
		mode YTCommentsMode
		gate string
	}{
		{YTCommentsOff, `FALSE`},
		{YTCommentsCanary, `o.principal <> 'hk:doc-comments' AND EXISTS (SELECT 1 FROM yt_comment_canary c
		WHERE o.entity_kind = 'task' AND c.task_id::text = o.entity_id AND c.issue_id = o.yt_target_id AND c.tenant = o.tenant)`},
		// Mode on also admits an hk comment queued with no target behind
		// its task's issue_create (hk 4263); its claim resolves the target.
		{YTCommentsOn, `o.principal <> 'hk:doc-comments' AND o.entity_kind = 'task' AND (EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant)
		OR (o.principal = 'hk:task-comments' AND o.yt_target_id = ''))`},
	} {
		s.SetYTCommentsMode(tc.mode)
		want := `(o.tenant = $2 AND (` + noArticles + ` OR (o.kind = 'comment_append' AND ((` + tc.gate + `) OR (FALSE))) OR ` + noDecisions + ` OR (o.kind = 'issue_create' AND (FALSE)) OR ` + noFields + ` OR ` + noLinks + ` OR ` + noMove + `))`
		if got := s.ytAdmitSQL("$2"); got != want {
			t.Errorf("mode %s:\n got %s\nwant %s", tc.mode, got, want)
		}
	}
	// The document mode (hk 4xxx) admits only document copies bound to their
	// task's issue; off admits nothing.
	s.SetYTCommentsMode(YTCommentsOff)
	s.SetYTDocCommentsMode(YTDocCommentsOff)
	if got := ytDocCommentGateSQL(s); got != `FALSE` {
		t.Errorf("doc comments off: %s", got)
	}
	s.SetYTDocCommentsMode(YTDocCommentsOn)
	wantDoc := `(o.tenant = $2 AND (` + noArticles + ` OR (o.kind = 'comment_append' AND ((FALSE) OR (o.principal = 'hk:doc-comments' AND o.entity_kind = 'task' AND EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant)))) OR ` + noDecisions + ` OR (o.kind = 'issue_create' AND (FALSE)) OR ` + noFields + ` OR ` + noLinks + ` OR ` + noMove + `))`
	if got := s.ytAdmitSQL("$2"); got != wantDoc {
		t.Errorf("doc comments on:\n got %s\nwant %s", got, wantDoc)
	}
	s.SetYTDocCommentsMode(YTDocCommentsOff)
	// The fields mode (hk 4003) admits an op whose target is the task's
	// bound issue on the op's tenant; on without a tenant is off.
	s.SetYTCommentsMode(YTCommentsOff)
	s.SetYTFieldsMode(YTFieldsOn, "")
	if got := ytFieldsGateSQL(s); got != `FALSE` {
		t.Errorf("fields on without a tenant: %s", got)
	}
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	fieldsGate := `o.entity_kind = 'task' AND EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant)`
	want := `(o.tenant = $2 AND (` + noArticles + ` OR (o.kind = 'comment_append' AND ((FALSE) OR (FALSE))) OR ` + noDecisions + ` OR (o.kind = 'issue_create' AND (FALSE)) OR (o.kind = 'issue_fields' AND (` + fieldsGate + `)) OR ` + noLinks + ` OR ` + noMove + `))`
	if got := s.ytAdmitSQL("$2"); got != want {
		t.Errorf("fields on, moves off:\n got %s\nwant %s", got, want)
	}
	// The moves mode (hk 4111) admits an issue_move op whose target is the
	// task's bound issue on the op's tenant; off is FALSE, with fields on or not.
	s.SetYTMovesMode(YTMovesOn)
	wantMoves := `(o.tenant = $2 AND (` + noArticles + ` OR (o.kind = 'comment_append' AND ((FALSE) OR (FALSE))) OR ` + noDecisions + ` OR (o.kind = 'issue_create' AND (FALSE)) OR (o.kind = 'issue_fields' AND (` + fieldsGate + `)) OR ` + noLinks + ` OR (o.kind = 'issue_move' AND (` + fieldsGate + `))))`
	if got := s.ytAdmitSQL("$2"); got != wantMoves {
		t.Errorf("moves on:\n got %s\nwant %s", got, wantMoves)
	}
	s.SetYTMovesMode(YTMovesOff)
	if got := ytMoveGateSQL(s); got != `FALSE` {
		t.Errorf("moves off: %s", got)
	}
	// The decisions mode (hk 4054) admits a create while the decision is
	// unbound (or bound by this op) and a snapshot whose target is the
	// bound issue; on without a tenant or a DEC project is off.
	s.SetYTFieldsMode(YTFieldsOff, "")
	s.SetYTDecisionsMode(YTDecisionsOn, ytcTenant, "", "DEC")
	if got := ytDecisionCreateGateSQL(s) + ytDecisionFieldsGateSQL(s); got != `FALSEFALSE` {
		t.Errorf("decisions on without a project: %s", got)
	}
	s.SetYTDecisionsMode(YTDecisionsOn, ytcTenant, "0-12", "DEC")
	want = `(o.tenant = $2 AND (` + noArticles + ` OR (o.kind = 'comment_append' AND ((FALSE) OR (FALSE))) OR (o.kind = 'decision_create' AND (o.entity_kind = 'decision' AND NOT EXISTS (SELECT 1 FROM yt_decision_bindings b
		WHERE b.decision_id = o.entity_id AND b.op_id IS DISTINCT FROM o.id))) OR (o.kind = 'decision_fields' AND (o.entity_kind = 'decision' AND EXISTS (SELECT 1 FROM yt_decision_bindings b
		WHERE b.decision_id = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant))) OR (o.kind = 'issue_create' AND (FALSE)) OR ` + noFields + ` OR ` + noLinks + ` OR ` + noMove + `))`
	if got := s.ytAdmitSQL("$2"); got != want {
		t.Errorf("decisions on:\n got %s\nwant %s", got, want)
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
