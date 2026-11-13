package store

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// hk 4318 (redo of hk 3909 and 3942): an issue_create op is dispatched to the
// YouTrack project its task is in when the op is claimed, keeping its id and
// key, and a task created in an unmapped project keeps an issue intent on a
// tenant until its project is mapped.

// ytrProject adds hk project name mapped to YouTrack project id ("" leaves it
// unmapped).
func ytrProject(t *testing.T, s *Store, name, id string) {
	t.Helper()
	if _, err := s.AddTaskProject(t.Context(), name, "test"); err != nil {
		t.Fatal(err)
	}
	if id == "" {
		return
	}
	if err := s.PutYouTrackProject(t.Context(), YouTrackProject{HKProject: name, ProjectID: id, ShortName: "HO"}); err != nil {
		t.Fatal(err)
	}
}

// ytrIntent is the task's yt_issue_intents tenant, "" when it has none.
func ytrIntent(t *testing.T, s *Store, task int64) string {
	t.Helper()
	var tenant string
	if err := s.pool.QueryRow(t.Context(), `SELECT coalesce((SELECT tenant FROM yt_issue_intents WHERE task_id=$1),'')`, task).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	return tenant
}

func ytrMove(t *testing.T, s *Store, task int64, project string) {
	t.Helper()
	if _, changed, err := s.SetTaskProject(t.Context(), task, project, "test", "moved"); err != nil || !changed {
		t.Fatalf("SetTaskProject %d -> %s: changed=%t err=%v", task, project, changed, err)
	}
}

// ytrOp re-reads op.
func ytrOp(t *testing.T, s *Store, id int64) ytwrite.Op {
	t.Helper()
	op, err := s.YTWriteLedger().ByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// ytrProjectOf is the hk project an issue_create op's payload names.
func ytrProjectOf(t *testing.T, op ytwrite.Op) string {
	t.Helper()
	var p ytwrite.IssueCreatePayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		t.Fatalf("payload %s: %v", op.Payload, err)
	}
	return p.Project
}

// 3909: a move does not touch the prepared op; its claim dispatches the same
// op (same id, same key, so a release that does not retarget reads it too)
// to the YouTrack project the task is in now, with the payload and payload
// hash IssueOp gives that project. An op whose task did not move is
// dispatched as it was prepared.
func TestYTIssueClaimTargetsTheTasksCurrentProject(t *testing.T) {
	s := schemaStore(t, "ytr_claim")
	ytbMapped(t, s)
	ytrProject(t, s, "ytr-other", "0-9")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, ytbProject)
	prepared := ytiOps(t, s, x.ID)[0]
	ytrMove(t, s, x.ID, "ytr-other")
	if ops := ytiOps(t, s, x.ID); len(ops) != 1 || ops[0].State != ytwrite.StatePrepared || ops[0].YTTargetID != ytbProjectID ||
		ops[0].PayloadSHA != prepared.PayloadSHA || ytrIntent(t, s, x.ID) != "" {
		t.Fatalf("the move changed the op: %+v, intent %q", ops, ytrIntent(t, s, x.ID))
	}
	if out := ytiClaim(t, s, prepared); out != ytwrite.ClaimClaimed {
		t.Fatalf("claim = %s", out)
	}
	got := ytrOp(t, s, prepared.ID)
	want, err := ytwrite.IssueOp(ytwrite.IssueRequest{Principal: YTIssuePrincipal, Actor: prepared.Actor, TaskID: x.ID, Title: x.Title,
		Project: "ytr-other", YTProjectID: "0-9", YTProjectShort: "HO", Tenant: ytcTenant})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ytwrite.StateDispatched || got.Key != ytwrite.IssueKey(x.ID) || got.YTTargetID != "0-9" ||
		got.PayloadSHA != want.PayloadSHA || ytrProjectOf(t, got) != "ytr-other" || got.Tenant != ytcTenant {
		t.Fatalf("dispatched op %+v, want target 0-9 sha %s", got, want.PayloadSHA)
	}
	if ops := ytiOps(t, s, x.ID); len(ops) != 1 {
		t.Fatalf("ops %+v", ops)
	}

	y := ytiCreate(t, s, ytbProject)
	op := ytiOps(t, s, y.ID)[0]
	if out := ytiClaim(t, s, op); out != ytwrite.ClaimClaimed {
		t.Fatalf("claim of an unmoved op = %s", out)
	}
	if got := ytrOp(t, s, op.ID); got.YTTargetID != op.YTTargetID || got.PayloadSHA != op.PayloadSHA || string(got.Payload) != string(op.Payload) {
		t.Fatalf("an unmoved op changed: %+v, was %+v", got, op)
	}
}

// Two hk projects mapped to one YouTrack project: a move between them keeps
// the target and changes the payload's hk project.
func TestYTIssueClaimRetargetsWithinOneYouTrackProject(t *testing.T) {
	s := schemaStore(t, "ytr_twin")
	ytbMapped(t, s)
	ytrProject(t, s, "ytr-twin", ytbProjectID)
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, ytbProject)
	op := ytiOps(t, s, x.ID)[0]
	ytrMove(t, s, x.ID, "ytr-twin")
	ytiDispatch(t, s, op)
	if got := ytrOp(t, s, op.ID); got.YTTargetID != ytbProjectID || ytrProjectOf(t, got) != "ytr-twin" || got.PayloadSHA == op.PayloadSHA {
		t.Fatalf("dispatched op %+v", got)
	}
}

// A move to an unmapped project, or to none where this process does not know
// the HK bucket, keeps the op as it was prepared (main's behaviour) and
// records no intent: an intent is made only at create time.
func TestYTIssueClaimKeepsTheOpWithoutAMappedProject(t *testing.T) {
	s := schemaStore(t, "ytr_keep")
	ytbMapped(t, s)
	ytrProject(t, s, "ytr-unmapped", "")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, ytbProject)
	op := ytiOps(t, s, x.ID)[0]
	ytrMove(t, s, x.ID, "ytr-unmapped")
	if ytrIntent(t, s, x.ID) != "" {
		t.Fatal("a move recorded an intent")
	}
	ytiDispatch(t, s, op)
	if got := ytrOp(t, s, op.ID); got.YTTargetID != ytbProjectID || got.PayloadSHA != op.PayloadSHA {
		t.Fatalf("op %+v", got)
	}

	// A task with neither op nor intent (created in mode off) moved to an
	// unmapped project gets none.
	s.SetYTIssuesMode(YTIssuesOff, "")
	y := ytiCreate(t, s, ytbProject)
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	ytrMove(t, s, y.ID, "ytr-unmapped")
	if len(ytiOps(t, s, y.ID)) != 0 || ytrIntent(t, s, y.ID) != "" {
		t.Fatalf("ops %+v, intent %q", ytiOps(t, s, y.ID), ytrIntent(t, s, y.ID))
	}

	// A no-project task's op targets the HK bucket; a claim in a process that
	// does not know the bucket id dispatches it as it is.
	s.SetYTIssuesBucketProject("0-hk")
	task, err := s.CreateDotsRequest(t.Context(), "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	bucketOp := ytiOps(t, s, task.ID)[0]
	s.SetYTIssuesBucketProject("")
	ytiDispatch(t, s, bucketOp)
	if got := ytrOp(t, s, bucketOp.ID); got.YTTargetID != "0-hk" || got.PayloadSHA != bucketOp.PayloadSHA {
		t.Fatalf("bucket op %+v", got)
	}
}

// An op deferred back to prepared (nothing was created) takes the task's
// project again at its next claim.
func TestYTIssueDeferredOpIsRetargetedAtItsNextClaim(t *testing.T) {
	s := schemaStore(t, "ytr_defer")
	ytbMapped(t, s)
	ytrProject(t, s, "ytr-other", "0-9")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	ctx := t.Context()
	x := ytiCreate(t, s, ytbProject)
	op := ytiOps(t, s, x.ID)[0]
	if out := ytiClaim(t, s, op); out != ytwrite.ClaimClaimed {
		t.Fatalf("first claim = %s", out)
	}
	ytrMove(t, s, x.ID, "ytr-other")
	if got := ytrOp(t, s, op.ID); got.YTTargetID != ytbProjectID {
		t.Fatalf("a dispatched op was retargeted: %+v", got)
	}
	now := time.Now().UTC()
	if _, err := s.YTTenantLedger(ytcTenant).Defer(ctx, now, op.ID, now, ytwrite.ReasonRateLimited, "429"); err != nil {
		t.Fatal(err)
	}
	if out := ytiClaim(t, s, op); out != ytwrite.ClaimClaimed {
		t.Fatalf("claim after the defer = %s", out)
	}
	if got := ytrOp(t, s, op.ID); got.YTTargetID != "0-9" || ytrProjectOf(t, got) != "ytr-other" {
		t.Fatalf("op after its second claim %+v", got)
	}
}

// The guard (migration 20261113-yt-issue-claim-target) lets an issue_create
// op's target and payload change only in the update that dispatches it.
func TestYTIssueTargetChangesOnlyAtItsClaim(t *testing.T) {
	s := schemaStore(t, "ytr_guard")
	ytbMapped(t, s)
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	ctx := t.Context()
	x := ytiCreate(t, s, ytbProject)
	op := ytiOps(t, s, x.ID)[0]
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET yt_target_id='0-9', payload_sha='x' WHERE id=$1`, op.ID); err == nil {
		t.Fatal("a prepared op was retargeted outside its claim")
	}
	ytiDispatch(t, s, op)
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET yt_target_id='0-9', payload_sha='x' WHERE id=$1`, op.ID); err == nil {
		t.Fatal("a dispatched op was retargeted")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET state='prepared', payload='{}'::jsonb WHERE id=$1`, op.ID); err == nil {
		t.Fatal("a dispatched op's payload changed on its way back to prepared")
	}
}

// 3942: a task created in an unmapped project keeps an intent; the worker of
// the intent's tenant prepares the op once the project is mapped. Nothing is
// prepared twice, nor in mode off.
func TestYTIssueUnmappedTaskGetsItsOpOnceMapped(t *testing.T) {
	s := schemaStore(t, "ytr_intent")
	ytrProject(t, s, "ytr-later", "")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, "ytr-later")
	if ytrIntent(t, s, x.ID) != ytcTenant {
		t.Fatalf("intent %q", ytrIntent(t, s, x.ID))
	}
	if n, err := s.PrepareMappedYTIssues(t.Context(), 10); err != nil || n != 0 || len(ytiOps(t, s, x.ID)) != 0 {
		t.Fatalf("unmapped pass prepared %d, %v", n, err)
	}
	if err := s.PutYouTrackProject(t.Context(), YouTrackProject{HKProject: "ytr-later", ProjectID: "0-11", ShortName: "HL"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PrepareMappedYTIssues(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("mapped pass prepared %d, %v; want 1", n, err)
	}
	if ops := ytiOps(t, s, x.ID); len(ops) != 1 || ops[0].State != ytwrite.StatePrepared || ops[0].Key != ytwrite.IssueKey(x.ID) ||
		ops[0].YTTargetID != "0-11" || ops[0].Tenant != ytcTenant || ytrIntent(t, s, x.ID) != "" {
		t.Fatalf("ops %+v, intent %q", ops, ytrIntent(t, s, x.ID))
	}
	if n, err := s.PrepareMappedYTIssues(t.Context(), 10); err != nil || n != 0 {
		t.Fatalf("second pass prepared %d, %v", n, err)
	}
	ytrProject(t, s, "ytr-off", "")
	off := ytiCreate(t, s, "ytr-off")
	s.SetYTIssuesMode(YTIssuesOff, "")
	if err := s.PutYouTrackProject(t.Context(), YouTrackProject{HKProject: "ytr-off", ProjectID: "0-12", ShortName: "HF"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PrepareMappedYTIssues(t.Context(), 10); err != nil || n != 0 || len(ytiOps(t, s, off.ID)) != 0 {
		t.Fatalf("mode off prepared %d, %v", n, err)
	}
}

// 3942: a task waiting on an intent that moves to a mapped project gets its op
// in the move, on the intent's tenant (not the gate's), and loses the intent;
// one that moves to another unmapped project keeps it.
func TestYTIssueIntentTaskMovedToAMappedProject(t *testing.T) {
	s := schemaStore(t, "ytr_intent_move")
	ytbMapped(t, s)
	ytrProject(t, s, "ytr-later", "")
	ytrProject(t, s, "ytr-never", "")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, "ytr-later")
	ytrMove(t, s, x.ID, "ytr-never")
	if ytrIntent(t, s, x.ID) != ytcTenant || len(ytiOps(t, s, x.ID)) != 0 {
		t.Fatalf("after an unmapped move: intent %q ops %+v", ytrIntent(t, s, x.ID), ytiOps(t, s, x.ID))
	}
	s.SetYTIssuesMode(YTIssuesOn, "other.youtrack.cloud")
	ytrMove(t, s, x.ID, ytbProject)
	if ops := ytiOps(t, s, x.ID); len(ops) != 1 || ops[0].Tenant != ytcTenant || ops[0].YTTargetID != ytbProjectID || ytrIntent(t, s, x.ID) != "" {
		t.Fatalf("ops %+v, intent %q", ops, ytrIntent(t, s, x.ID))
	}
}

// 3942: an intent carries its tenant and is consumed only by that tenant's
// worker; the op it becomes is bound to that tenant.
func TestYTIssueIntentIsConsumedOnlyOnItsTenant(t *testing.T) {
	s := schemaStore(t, "ytr_tenant")
	ytrProject(t, s, "ytr-later", "")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, "ytr-later")
	if err := s.PutYouTrackProject(t.Context(), YouTrackProject{HKProject: "ytr-later", ProjectID: "0-11", ShortName: "HL"}); err != nil {
		t.Fatal(err)
	}
	const other = "other.youtrack.cloud"
	s.SetYTIssuesMode(YTIssuesOn, other)
	if n, err := s.PrepareMappedYTIssues(t.Context(), 10); err != nil || n != 0 || len(ytiOps(t, s, x.ID)) != 0 || ytrIntent(t, s, x.ID) != ytcTenant {
		t.Fatalf("another tenant's worker prepared %d (%v), ops %+v", n, err, ytiOps(t, s, x.ID))
	}
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	if n, err := s.PrepareMappedYTIssues(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("own tenant prepared %d, %v", n, err)
	}
	if ops := ytiOps(t, s, x.ID); len(ops) != 1 || ops[0].Tenant != ytcTenant || ops[0].YTTargetID != "0-11" {
		t.Fatalf("ops %+v", ops)
	}
}

// issues-backfill honours an intent's tenant (PR #357 review): a backfill on
// another tenant leaves the task to the intent's worker; one on the intent's
// tenant prepares the op and drops the intent.
func TestYTIssueBackfillHonoursTheIntentTenant(t *testing.T) {
	s := schemaStore(t, "ytr_backfill")
	ytrProject(t, s, "ytr-later", "")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, "ytr-later")
	if err := s.PutYouTrackProject(t.Context(), YouTrackProject{HKProject: "ytr-later", ProjectID: "0-11", ShortName: "HL"}); err != nil {
		t.Fatal(err)
	}
	r, err := s.BackfillYTIssueOps(t.Context(), "other.youtrack.cloud", "ytr-later", false)
	if err != nil || len(r.Projects) != 1 || r.Projects[0].OtherTenant != 1 || r.Projects[0].Prepared != 0 || len(ytiOps(t, s, x.ID)) != 0 {
		t.Fatalf("other-tenant backfill %+v %v, ops %+v", r, err, ytiOps(t, s, x.ID))
	}
	r, err = s.BackfillYTIssueOps(t.Context(), ytcTenant, "ytr-later", false)
	if err != nil || len(r.Projects) != 1 || r.Projects[0].Prepared != 1 {
		t.Fatalf("own-tenant backfill %+v %v", r, err)
	}
	if ops := ytiOps(t, s, x.ID); len(ops) != 1 || ops[0].Tenant != ytcTenant || ytrIntent(t, s, x.ID) != "" {
		t.Fatalf("ops %+v, intent %q", ops, ytrIntent(t, s, x.ID))
	}
}

// issues-backfill reads the task under its lane lock (PR #357 review, round
// 2): a move that holds the lane when the backfill starts is what the backfill
// sees, so it neither prepares the old project's op nor drops the new intent.
func TestYTIssueBackfillReadsTheTaskUnderTheLaneLock(t *testing.T) {
	s := schemaStore(t, "ytr_backfill_race")
	ytrProject(t, s, "ytr-mapped", "")
	ytrProject(t, s, "ytr-unmapped", "")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, "ytr-mapped")
	if err := s.PutYouTrackProject(t.Context(), YouTrackProject{HKProject: "ytr-mapped", ProjectID: "0-11", ShortName: "HL"}); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// The move, as SetTaskProject runs it: task row, then the lane lock.
	if _, err := tx.Exec(ctx, `UPDATE tasks SET project='ytr-unmapped' WHERE id=$1`, x.ID); err != nil {
		t.Fatal(err)
	}
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(x.ID, 10)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var r YTIssueBackfillReport
	go func() {
		var err error
		r, err = s.BackfillYTIssueOps(context.Background(), ytcTenant, "", false)
		done <- err
	}()
	// The backfill lists the task, then waits on the lane lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND objsubid=1
			AND ((classid::bigint << 32) | objid::bigint) = hashtextextended('yt_write_entity:task:' || $1, 0))`, strconv.FormatInt(x.ID, 10)).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backfill never waited on the lane lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ops := ytiOps(t, s, x.ID); len(ops) != 0 || ytrIntent(t, s, x.ID) != ytcTenant {
		t.Fatalf("backfill used the stale project: report %+v ops %+v intent %q", r, ops, ytrIntent(t, s, x.ID))
	}
}
