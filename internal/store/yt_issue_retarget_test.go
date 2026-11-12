package store

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// hk 4318 (redo of hk 3909 and 3942): a task's issue op follows the task's
// project until it is sent, and a task in an unmapped project keeps an issue
// intent on a tenant until its project is mapped.

// ytrProject adds hk project name mapped to YouTrack project id.
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

func ytrAfter(task int64, replaced ytwrite.Op) string {
	return ytwrite.IssueKey(task) + ":after-" + strconv.FormatInt(replaced.ID, 10)
}

// 3909: a task moved before its issue op was sent gets the new project's op,
// on the same tenant, in the move's transaction; the stale op is rejected
// project_changed and never sent. A move to an unmapped project leaves an
// intent on the op's tenant. A sent op is left alone.
func TestYTIssueProjectChangeReplacesAnUnsentOp(t *testing.T) {
	s := schemaStore(t, "ytr_move")
	ytbMapped(t, s)
	ytrProject(t, s, "ytr-other", "0-9")
	ytrProject(t, s, "ytr-unmapped", "")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, ytbProject)
	ytrMove(t, s, x.ID, "ytr-other")
	ops := ytiOps(t, s, x.ID)
	if len(ops) != 2 || ops[0].State != ytwrite.StateRejected || ops[0].Reason != ytwrite.ReasonProjectChanged ||
		ops[1].State != ytwrite.StatePrepared || ops[1].YTTargetID != "0-9" || ops[1].Tenant != ytcTenant || ops[1].Key != ytrAfter(x.ID, ops[0]) {
		t.Fatalf("ops after the move %+v", ops)
	}
	var p ytwrite.IssueCreatePayload
	if err := json.Unmarshal(ops[1].Payload, &p); err != nil || p.Project != "ytr-other" {
		t.Fatalf("payload %s (%v)", ops[1].Payload, err)
	}
	// The gate's tenant changing does not move the replacement off the
	// op's tenant.
	s.SetYTIssuesMode(YTIssuesOn, "other.youtrack.cloud")
	ytrMove(t, s, x.ID, "ytr-unmapped")
	ops = ytiOps(t, s, x.ID)
	if len(ops) != 2 || ops[1].State != ytwrite.StateRejected || ops[1].Reason != ytwrite.ReasonProjectChanged {
		t.Fatalf("ops after the move to an unmapped project %+v", ops)
	}
	if got := ytrIntent(t, s, x.ID); got != ytcTenant {
		t.Fatalf("intent tenant %q, want the op's %q", got, ytcTenant)
	}

	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	sent := ytiCreate(t, s, ytbProject)
	op := ytiOps(t, s, sent.ID)[0]
	ytiDispatch(t, s, op)
	ytrMove(t, s, sent.ID, "ytr-other")
	if ops := ytiOps(t, s, sent.ID); len(ops) != 1 || ops[0].State != ytwrite.StateDispatched {
		t.Fatalf("a dispatched op was replaced: %+v", ops)
	}
}

// Two hk projects mapped to one YouTrack project: a move between them keeps
// the YouTrack target but changes the payload's hk project, so the unsent op
// is still replaced.
func TestYTIssueProjectChangeWithinOneYouTrackProject(t *testing.T) {
	s := schemaStore(t, "ytr_twin")
	ytbMapped(t, s)
	ytrProject(t, s, "ytr-twin", ytbProjectID)
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, ytbProject)
	ytrMove(t, s, x.ID, "ytr-twin")
	ops := ytiOps(t, s, x.ID)
	if len(ops) != 2 || ops[0].Reason != ytwrite.ReasonProjectChanged || ops[1].State != ytwrite.StatePrepared || ops[1].YTTargetID != ytbProjectID {
		t.Fatalf("ops after the move %+v", ops)
	}
	var p ytwrite.IssueCreatePayload
	if err := json.Unmarshal(ops[1].Payload, &p); err != nil || p.Project != "ytr-twin" {
		t.Fatalf("payload of the new op %s (%v)", ops[1].Payload, err)
	}
}

// 3909 at the claim: a task moved by a writer that does not sync (a triage
// writes tasks.project itself), or moved while its op was in flight and then
// deferred back to prepared, has its op replaced at the claim, under the lane
// lock, instead of the issue created in the old project.
func TestYTIssueMovedOpIsReplacedAtItsClaim(t *testing.T) {
	s := schemaStore(t, "ytr_claim")
	ytbMapped(t, s)
	ytrProject(t, s, "ytr-other", "0-9")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	ctx := t.Context()

	// A writer that does not sync.
	x := ytiCreate(t, s, ytbProject)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET project='ytr-other' WHERE id=$1`, x.ID); err != nil {
		t.Fatal(err)
	}
	if out := ytiClaim(t, s, ytiOps(t, s, x.ID)[0]); out != ytwrite.ClaimNotPrepared {
		t.Fatalf("claim of the moved op = %s, want not_prepared", out)
	}
	ops := ytiOps(t, s, x.ID)
	if len(ops) != 2 || ops[0].Reason != ytwrite.ReasonProjectChanged || ops[1].State != ytwrite.StatePrepared || ops[1].YTTargetID != "0-9" {
		t.Fatalf("ops after the claim %+v", ops)
	}
	if out := ytiClaim(t, s, ops[1]); out != ytwrite.ClaimClaimed {
		t.Fatalf("claim of the replacement = %s", out)
	}

	// Moved in flight, then deferred back to prepared.
	y := ytiCreate(t, s, ytbProject)
	op := ytiOps(t, s, y.ID)[0]
	if out := ytiClaim(t, s, op); out != ytwrite.ClaimClaimed {
		t.Fatalf("first claim = %s", out)
	}
	ytrMove(t, s, y.ID, "ytr-other")
	if ops := ytiOps(t, s, y.ID); len(ops) != 1 || ops[0].State != ytwrite.StateDispatched {
		t.Fatalf("a dispatched op was replaced: %+v", ops)
	}
	now := time.Now().UTC()
	if _, err := s.YTTenantLedger(ytcTenant).Defer(ctx, now, op.ID, now, ytwrite.ReasonRateLimited, "429"); err != nil {
		t.Fatal(err)
	}
	if out := ytiClaim(t, s, op); out != ytwrite.ClaimNotPrepared {
		t.Fatalf("claim after the move = %s, want not_prepared", out)
	}
	ops = ytiOps(t, s, y.ID)
	if len(ops) != 2 || ops[0].Reason != ytwrite.ReasonProjectChanged || ops[1].State != ytwrite.StatePrepared ||
		ops[1].YTTargetID != "0-9" || ops[1].Key != ytrAfter(y.ID, ops[0]) {
		t.Fatalf("ops after the deferred claim %+v", ops)
	}
	// An op whose task did not move is claimed as before.
	z := ytiCreate(t, s, ytbProject)
	if out := ytiClaim(t, s, ytiOps(t, s, z.ID)[0]); out != ytwrite.ClaimClaimed {
		t.Fatalf("claim of an unmoved op = %s", out)
	}
}

// 3942: a task created in, or moved to, an unmapped project keeps an intent;
// the worker of the intent's tenant prepares the op once the project is
// mapped. A created task gets the first key, a moved one the key after its
// rejected op. Nothing is prepared twice, nor in mode off.
func TestYTIssueUnmappedTaskGetsItsOpOnceMapped(t *testing.T) {
	s := schemaStore(t, "ytr_intent")
	ytbMapped(t, s)
	ytrProject(t, s, "ytr-later", "")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, "ytr-later")
	moved := ytiCreate(t, s, ytbProject)
	ytrMove(t, s, moved.ID, "ytr-later")
	if ytrIntent(t, s, x.ID) != ytcTenant || ytrIntent(t, s, moved.ID) != ytcTenant {
		t.Fatalf("intents %q %q", ytrIntent(t, s, x.ID), ytrIntent(t, s, moved.ID))
	}
	if n, err := s.PrepareMappedYTIssues(t.Context(), 10); err != nil || n != 0 || len(ytiOps(t, s, x.ID)) != 0 {
		t.Fatalf("unmapped pass prepared %d, %v", n, err)
	}
	if err := s.PutYouTrackProject(t.Context(), YouTrackProject{HKProject: "ytr-later", ProjectID: "0-11", ShortName: "HL"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PrepareMappedYTIssues(t.Context(), 10); err != nil || n != 2 {
		t.Fatalf("mapped pass prepared %d, %v; want 2", n, err)
	}
	if ops := ytiOps(t, s, x.ID); len(ops) != 1 || ops[0].State != ytwrite.StatePrepared || ops[0].Key != ytwrite.IssueKey(x.ID) ||
		ops[0].YTTargetID != "0-11" || ops[0].Tenant != ytcTenant {
		t.Fatalf("ops of the created task %+v", ops)
	}
	if ops := ytiOps(t, s, moved.ID); len(ops) != 2 || ops[0].Reason != ytwrite.ReasonProjectChanged || ops[1].State != ytwrite.StatePrepared ||
		ops[1].Key != ytrAfter(moved.ID, ops[0]) || ops[1].Tenant != ytcTenant {
		t.Fatalf("ops of the moved task %+v", ops)
	}
	if ytrIntent(t, s, x.ID) != "" || ytrIntent(t, s, moved.ID) != "" {
		t.Fatal("an intent outlived its op")
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

// A no-project task's op targets the HK bucket, whose id only a process in
// issue mode on knows: a claim in a process that does not know it leaves the
// op as it is rather than treat the task as moved.
func TestYTIssueBucketOpIsKeptWhereTheBucketIsUnknown(t *testing.T) {
	s := schemaStore(t, "ytr_bucket")
	s.SetYTIssuesBucketProject("0-hk")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	task, err := s.CreateDotsRequest(t.Context(), "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	if ops := ytiOps(t, s, task.ID); len(ops) != 1 || ops[0].YTTargetID != "0-hk" {
		t.Fatalf("ops %+v", ops)
	}
	s.SetYTIssuesBucketProject("")
	ytiDispatch(t, s, ytiOps(t, s, task.ID)[0])
	if ops := ytiOps(t, s, task.ID); len(ops) != 1 || ops[0].State != ytwrite.StateDispatched {
		t.Fatalf("ops after a claim without the bucket %+v", ops)
	}
}
