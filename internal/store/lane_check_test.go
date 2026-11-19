package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// laneCheckFixture seeds one project whose design lane is lane, so the check
// finds the lane the way it finds a real one.
func laneCheckFixture(t *testing.T, s *Store, project, lane string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.AddTaskProject(ctx, project, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProjectDesignLane(ctx, project, lane, "tester", "lane check test"); err != nil {
		t.Fatal(err)
	}
}

// laneCheckTask creates one backlog task in lane with a title unique enough
// not to trip the duplicate check.
func laneCheckTask(t *testing.T, s *Store, project, lane, title string, priority int) Task {
	t.Helper()
	p := project
	x, err := s.CreateTask(context.Background(), Task{
		Lane: lane, Title: title, Kind: "ops", Project: &p, CreatedBy: "tester", Priority: priority,
	})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// laneCheckFindings reads the lane's current findings.
func laneCheckFindings(t *testing.T, s *Store, lane string, now time.Time) LaneCheck {
	t.Helper()
	checks, err := s.LaneChecks(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if c.Lane == lane {
			return c
		}
	}
	return LaneCheck{Lane: lane}
}

func laneCheckFinding(c LaneCheck, name string) LaneCheckFinding {
	for _, f := range c.Findings {
		if f.Name == name {
			return f
		}
	}
	return LaneCheckFinding{Name: name}
}

func laneCheckOutbox(t *testing.T, s *Store, lane string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM notification_outbox WHERE kind='lane_check' AND target_lane=$1`, lane).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestLaneCheckFindsOrderUnacked: an open order to the lane is a finding, and
// its acknowledgement clears it.
func TestLaneCheckFindsOrderUnacked(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "lane_order")
	laneCheckFixture(t, s, "order-project", "order-lane")
	task := laneCheckTask(t, s, "order-project", "order-lane", "ordered work", 1)
	order, err := s.CreateTaskOrder(ctx, CreateTaskOrderInput{TaskID: task.ID, Words: "start it", Actor: "operator", Channel: TaskOrderChannelCLI, Verified: true, Key: "order-key-1"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	got := laneCheckFinding(laneCheckFindings(t, s, "order-lane", now), LaneFindingOrderUnacked)
	if len(got.TaskIDs) != 1 || got.TaskIDs[0] != task.ID {
		t.Fatalf("order_unacked ids = %v, want [%d]", got.TaskIDs, task.ID)
	}
	if _, err := s.AckTaskOrder(ctx, order.Order.ID, "order-lane", "starting"); err != nil {
		t.Fatal(err)
	}
	got = laneCheckFinding(laneCheckFindings(t, s, "order-lane", now), LaneFindingOrderUnacked)
	if len(got.TaskIDs) != 0 {
		t.Fatalf("acknowledged order still listed: %v", got.TaskIDs)
	}
}

// TestLaneCheckFindsBlockerLanded: a backlog task whose only blocker settled
// after the task last moved is a finding, as ids only.
func TestLaneCheckFindsBlockerLanded(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "lane_blocker")
	laneCheckFixture(t, s, "blocker-project", "blocker-lane")
	blocker := laneCheckTask(t, s, "blocker-project", "blocker-lane", "the blocker", 1)
	blocked := laneCheckTask(t, s, "blocker-project", "blocker-lane", "blocked work", 1)
	if _, err := s.AddTaskRelation(ctx, blocker.ID, RelationBlocks, blocked.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if got := laneCheckFinding(laneCheckFindings(t, s, "blocker-lane", now), LaneFindingBlockerLanded); len(got.TaskIDs) != 0 {
		t.Fatalf("blocker_landed before the blocker settled: %v", got.TaskIDs)
	}
	if _, err := s.TransitionTaskWith(ctx, blocker.ID, TransitionInput{To: StateCompleted, By: "tester", Note: "blocker done", Evidence: "finished"}); err != nil {
		t.Fatal(err)
	}
	got := laneCheckFinding(laneCheckFindings(t, s, "blocker-lane", now), LaneFindingBlockerLanded)
	if len(got.TaskIDs) != 1 || got.TaskIDs[0] != blocked.ID {
		t.Fatalf("blocker_landed ids = %v, want [%d]", got.TaskIDs, blocked.ID)
	}
}

// TestLaneCheckBlockerLandedSkipsOpenWait: a backlog task whose blocker settled
// is not blocker_landed and gives no room_to_run while it carries an open
// operator wait; clearing the wait brings it back, and wait_cleared reports it.
func TestLaneCheckBlockerLandedSkipsOpenWait(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "lane_blocker_wait")
	laneCheckFixture(t, s, "blocker-wait-project", "blocker-wait-lane")
	blocker := laneCheckTask(t, s, "blocker-wait-project", "blocker-wait-lane", "the blocker", 1)
	blocked := laneCheckTask(t, s, "blocker-wait-project", "blocker-wait-lane", "blocked work", 1)
	if _, err := s.AddTaskRelation(ctx, blocker.ID, RelationBlocks, blocked.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	wait, err := s.AddTaskWait(ctx, blocked.ID, TaskWaitInput{OnKind: WaitOnExternal, OnRef: "ci-build", Reason: "waiting on CI"}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTaskWith(ctx, blocker.ID, TransitionInput{To: StateCompleted, By: "tester", Note: "blocker done", Evidence: "finished"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	quiet := laneCheckFindings(t, s, "blocker-wait-lane", now)
	if got := laneCheckFinding(quiet, LaneFindingBlockerLanded); len(got.TaskIDs) != 0 {
		t.Fatalf("blocker_landed with an open wait: %v", got.TaskIDs)
	}
	if got := laneCheckFinding(quiet, LaneFindingRoomToRun); len(got.TaskIDs) != 0 {
		t.Fatalf("room_to_run with an open wait: %v", got.TaskIDs)
	}
	// The lane looks at that quiet state, so the later clearing is the only news.
	if _, _, err := s.RecordLaneLook(ctx, "blocker-wait-lane", quiet.NoticeEventID, "quiet", "blocker-wait-lane", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClearTaskWait(ctx, blocked.ID, wait.ID, "tester", "CI passed"); err != nil {
		t.Fatal(err)
	}
	c := laneCheckFindings(t, s, "blocker-wait-lane", now.Add(time.Minute))
	if got := laneCheckFinding(c, LaneFindingBlockerLanded); len(got.TaskIDs) != 1 || got.TaskIDs[0] != blocked.ID {
		t.Fatalf("blocker_landed after the wait cleared = %v, want [%d]", got.TaskIDs, blocked.ID)
	}
	if got := laneCheckFinding(c, LaneFindingRoomToRun); len(got.TaskIDs) != 1 || got.TaskIDs[0] != blocked.ID {
		t.Fatalf("room_to_run after the wait cleared = %v, want [%d]", got.TaskIDs, blocked.ID)
	}
	if got := laneCheckFinding(c, LaneFindingWaitCleared); len(got.TaskIDs) != 1 || got.TaskIDs[0] != blocked.ID {
		t.Fatalf("wait_cleared after the wait cleared = %v, want [%d]", got.TaskIDs, blocked.ID)
	}
}

// TestLaneCheckFindsKeptBacklogAndRoomToRun: an old priority 1 backlog task
// with no wait and no claim is kept_backlog, and a lane with no started job
// has room_to_run naming it.
func TestLaneCheckFindsKeptBacklogAndRoomToRun(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "lane_kept")
	laneCheckFixture(t, s, "kept-project", "kept-lane")
	task := laneCheckTask(t, s, "kept-project", "kept-lane", "kept work", 2)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, task.ID, time.Now().UTC().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	c := laneCheckFindings(t, s, "kept-lane", now)
	kept := laneCheckFinding(c, LaneFindingKeptBacklog)
	if len(kept.TaskIDs) != 1 || kept.TaskIDs[0] != task.ID {
		t.Fatalf("kept_backlog ids = %v, want [%d]", kept.TaskIDs, task.ID)
	}
	room := laneCheckFinding(c, LaneFindingRoomToRun)
	if len(room.TaskIDs) != 1 || room.TaskIDs[0] != task.ID {
		t.Fatalf("room_to_run ids = %v, want [%d]", room.TaskIDs, task.ID)
	}
	// An open hand wait takes it out of kept_backlog, and the room with it.
	if _, err := s.AddTaskWait(ctx, task.ID, TaskWaitInput{OnKind: WaitOnExternal, OnRef: "ci-build", Reason: "waiting on CI"}, "tester"); err != nil {
		t.Fatal(err)
	}
	c = laneCheckFindings(t, s, "kept-lane", now)
	if got := laneCheckFinding(c, LaneFindingKeptBacklog); len(got.TaskIDs) != 0 {
		t.Fatalf("kept_backlog with an open wait: %v", got.TaskIDs)
	}
	if got := laneCheckFinding(c, LaneFindingRoomToRun); len(got.TaskIDs) != 0 {
		t.Fatalf("room_to_run with nothing to run: %v", got.TaskIDs)
	}
}

// TestLaneCheckKeptBacklogSkipsOpenDecision: a task waiting on an operator
// decision is listed as needs_operator, so it is not kept_backlog until the
// request is resolved.
func TestLaneCheckKeptBacklogSkipsOpenDecision(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "lane_kept_decision")
	laneCheckFixture(t, s, "decision-project", "decision-lane")
	task := laneCheckTask(t, s, "decision-project", "decision-lane", "decided work", 2)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, task.ID, time.Now().UTC().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	request, err := s.RecordDecisionRequest(ctx, task.ID, "tester", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	c := laneCheckFindings(t, s, "decision-lane", now)
	if got := laneCheckFinding(c, LaneFindingKeptBacklog); len(got.TaskIDs) != 0 {
		t.Fatalf("kept_backlog with an open decision request: %v", got.TaskIDs)
	}
	if _, err := s.ResolveDecisionRequest(ctx, task.ID, "tester", DecisionResolveInput{RequestID: request.Request.ID, Kind: DecisionRequestAnswered, Option: "A", Responder: "operator"}); err != nil {
		t.Fatal(err)
	}
	c = laneCheckFindings(t, s, "decision-lane", now)
	if got := laneCheckFinding(c, LaneFindingKeptBacklog); len(got.TaskIDs) != 1 || got.TaskIDs[0] != task.ID {
		t.Fatalf("kept_backlog after the decision resolved = %v, want [%d]", got.TaskIDs, task.ID)
	}
}

// TestLaneCheckFindsWaitCleared: a wait that cleared after the lane's last
// look is a finding, and the lane's baseline is the look it recorded.
func TestLaneCheckFindsWaitCleared(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "lane_wait")
	laneCheckFixture(t, s, "wait-project", "wait-lane")
	kept := laneCheckTask(t, s, "wait-project", "wait-lane", "kept work for a baseline", 2)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, kept.ID, time.Now().UTC().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	waited := laneCheckTask(t, s, "wait-project", "wait-lane", "waited work", 3)
	wait, err := s.AddTaskWait(ctx, waited.ID, TaskWaitInput{OnKind: WaitOnExternal, OnRef: "ci-build", Reason: "waiting on CI"}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// The lane looks at the first notice: that look is its baseline.
	first := laneCheckFindings(t, s, "wait-lane", now)
	if first.FindingsHash == "" {
		t.Fatal("no first notice to look at")
	}
	if _, _, err := s.RecordLaneLook(ctx, "wait-lane", first.NoticeEventID, "first look", "wait-lane", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClearTaskWait(ctx, waited.ID, wait.ID, "tester", "CI passed"); err != nil {
		t.Fatal(err)
	}
	got := laneCheckFinding(laneCheckFindings(t, s, "wait-lane", now.Add(time.Minute)), LaneFindingWaitCleared)
	if len(got.TaskIDs) != 1 || got.TaskIDs[0] != waited.ID {
		t.Fatalf("wait_cleared ids = %v, want [%d]", got.TaskIDs, waited.ID)
	}
}

// TestLaneCheckDedupeBucketLookAndStateChange pins the notice identity: one
// notice per bucket, a second after the bucket, a look stopping it, and a
// state change re-noticing after the look.
func TestLaneCheckDedupeBucketLookAndStateChange(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "lane_dedupe")
	laneCheckFixture(t, s, "dedupe-project", "dedupe-lane")
	task := laneCheckTask(t, s, "dedupe-project", "dedupe-lane", "dedupe work", 2)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, task.ID, time.Now().UTC().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for range 2 {
		if _, err := s.RunLaneCheck(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	if n := laneCheckOutbox(t, s, "dedupe-lane"); n != 1 {
		t.Fatalf("rows after two runs in one bucket = %d, want 1", n)
	}
	c := laneCheckFindings(t, s, "dedupe-lane", now)
	if c.FindingsHash == "" || c.NoticeEventID == "" {
		t.Fatalf("check identity = %+v", c)
	}
	if _, err := s.RunLaneCheck(ctx, now.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := laneCheckOutbox(t, s, "dedupe-lane"); n != 2 {
		t.Fatalf("rows after the next bucket = %d, want 2", n)
	}
	// A look at the newest notice stops the repeat across buckets.
	next := laneCheckFindings(t, s, "dedupe-lane", now.Add(6*time.Hour))
	if next.FindingsHash != c.FindingsHash {
		t.Fatalf("hash changed with no state change: %s -> %s", c.FindingsHash, next.FindingsHash)
	}
	if _, changed, err := s.RecordLaneLook(ctx, "dedupe-lane", next.NoticeEventID, "looked", "dedupe-lane", now.Add(6*time.Hour)); err != nil || !changed {
		t.Fatalf("RecordLaneLook: changed=%t err=%v", changed, err)
	}
	if _, err := s.RunLaneCheck(ctx, now.Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := laneCheckOutbox(t, s, "dedupe-lane"); n != 2 {
		t.Fatalf("look did not stop the notice: %d rows", n)
	}
	// A new kept_backlog task changes the state and re-notices.
	late := laneCheckTask(t, s, "dedupe-project", "dedupe-lane", "new kept work", 2)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, late.ID, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunLaneCheck(ctx, now.Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := laneCheckOutbox(t, s, "dedupe-lane"); n != 3 {
		t.Fatalf("state change did not re-notice: %d rows", n)
	}
}

// TestLaneCheckLookDoesNotRenotifyItsOwnWaitCleared pins the review finding on
// #504: a look at a notice that carried wait_cleared must not make the next
// check notice the same lane again, because the look itself advances the
// baseline that removed the finding.
func TestLaneCheckLookDoesNotRenotifyItsOwnWaitCleared(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "lane_waitlook")
	laneCheckFixture(t, s, "waitlook-project", "waitlook-lane")
	kept := laneCheckTask(t, s, "waitlook-project", "waitlook-lane", "standing kept work", 2)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, kept.ID, time.Now().UTC().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	waited := laneCheckTask(t, s, "waitlook-project", "waitlook-lane", "task with a wait", 3)
	now := time.Now().UTC()
	if wrote, err := s.RunLaneCheck(ctx, now); err != nil || wrote != 1 {
		t.Fatalf("first RunLaneCheck = %d, %v", wrote, err)
	}
	first := laneCheckFindings(t, s, "waitlook-lane", now)
	if _, _, err := s.RecordLaneLook(ctx, "waitlook-lane", first.NoticeEventID, "first look", "waitlook-lane", now); err != nil {
		t.Fatal(err)
	}
	wait, err := s.AddTaskWait(ctx, waited.ID, TaskWaitInput{OnKind: WaitOnExternal, OnRef: "ci-build", Reason: "waiting on CI"}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClearTaskWait(ctx, waited.ID, wait.ID, "tester", "CI passed"); err != nil {
		t.Fatal(err)
	}
	// The cleared wait is new state: one notice.
	if wrote, err := s.RunLaneCheck(ctx, now.Add(time.Minute)); err != nil || wrote != 1 {
		t.Fatalf("wait-cleared RunLaneCheck = %d, %v", wrote, err)
	}
	second := laneCheckFindings(t, s, "waitlook-lane", now.Add(time.Minute))
	if got := laneCheckFinding(second, LaneFindingWaitCleared); len(got.TaskIDs) != 1 {
		t.Fatalf("wait_cleared = %v, want the waited task", got.TaskIDs)
	}
	if _, _, err := s.RecordLaneLook(ctx, "waitlook-lane", second.NoticeEventID, "looked", "waitlook-lane", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// After the look the same standing state must not be noticed again, even
	// in the next bucket: the old full-hash comparison did exactly that.
	if wrote, err := s.RunLaneCheck(ctx, now.Add(7*time.Hour)); err != nil || wrote != 0 {
		t.Fatalf("RunLaneCheck after the look = %d, %v; want 0", wrote, err)
	}
	if got := laneCheckOutbox(t, s, "waitlook-lane"); got != 2 {
		t.Fatalf("lane_check rows = %d, want 2", got)
	}
}

// TestLaneCheckNeverNoticesOperatorDesk: operator-desk is not a design lane,
// so it gets no notice however much its backlog holds.
func TestLaneCheckNeverNoticesOperatorDesk(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "lane_desk")
	laneCheckFixture(t, s, "desk-project", "operator-desk")
	task := laneCheckTask(t, s, "desk-project", "operator-desk", "desk work", 2)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, task.ID, time.Now().UTC().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunLaneCheck(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if n := laneCheckOutbox(t, s, "operator-desk"); n != 0 {
		t.Fatalf("operator-desk got %d lane_check rows", n)
	}
}

// TestRecordLaneLookBindsNoticeToLane pins the look's input: a malformed or
// another lane's notice is refused, a notice that does not name the current
// findings is refused, and a repeat changes nothing.
func TestRecordLaneLookBindsNoticeToLane(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "lane_look")
	laneCheckFixture(t, s, "look-project", "look-lane")
	now := time.Now().UTC()
	cur := laneCheckFindings(t, s, "look-lane", now)
	notice := cur.NoticeEventID
	if _, _, err := s.RecordLaneLook(ctx, "look-lane", LaneCheckEventID("other-lane", cur.FindingsHash, cur.LookEpoch, now), "", "look-lane", now); err == nil {
		t.Fatal("a look with another lane's notice was accepted")
	}
	if _, _, err := s.RecordLaneLook(ctx, "look-lane", "lanecheck-look-lane-nothash-1", "", "look-lane", now); err == nil {
		t.Fatal("a malformed notice was accepted")
	}
	if _, _, err := s.RecordLaneLook(ctx, "look-lane", LaneCheckEventID("look-lane", "deadbeef", cur.LookEpoch, now), "", "look-lane", now); err == nil {
		t.Fatal("a notice no check ever carried was accepted")
	}
	// The look stores the state it leaves (here no findings), not the notice
	// id's hash: the two differ whenever wait_cleared is present.
	if look, changed, err := s.RecordLaneLook(ctx, "look-lane", notice, "noted", "look-lane", now); err != nil || !changed || look.FindingsHash != cur.LookHash || look.FindingsHash != laneCheckHash(nil) {
		t.Fatalf("first look = %+v changed=%t err=%v", look, changed, err)
	}
	if _, changed, err := s.RecordLaneLook(ctx, "look-lane", notice, "again", "look-lane", now); err != nil || changed {
		t.Fatalf("repeat look changed=%t err=%v", changed, err)
	}
}

// TestRecordLaneLookRefusesStateItDidNotSee pins the review finding on #504:
// a look acknowledges exactly the state its notice named. A finding that
// appeared between the notice and the look must not be silenced by it.
func TestRecordLaneLookRefusesStateItDidNotSee(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "lane_lookstale")
	laneCheckFixture(t, s, "lookstale-project", "lookstale-lane")
	first := laneCheckTask(t, s, "lookstale-project", "lookstale-lane", "first kept work", 2)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, first.ID, time.Now().UTC().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	before := laneCheckFindings(t, s, "lookstale-lane", now)
	if len(laneCheckFinding(before, LaneFindingKeptBacklog).TaskIDs) != 1 {
		t.Fatal("no kept_backlog finding to name")
	}
	late := laneCheckTask(t, s, "lookstale-project", "lookstale-lane", "second kept work", 2)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, late.ID, time.Now().UTC().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordLaneLook(ctx, "lookstale-lane", before.NoticeEventID, "looked at the old notice", "lookstale-lane", now); err == nil {
		t.Fatal("a stale notice was accepted and would silence the newer finding")
	}
	after := laneCheckFindings(t, s, "lookstale-lane", now)
	if _, changed, err := s.RecordLaneLook(ctx, "lookstale-lane", after.NoticeEventID, "looked", "lookstale-lane", now); err != nil || !changed {
		t.Fatalf("current notice: changed=%t err=%v", changed, err)
	}
}

// TestLaneCheckReclearsAfterAQuietPeriod pins the review finding on #504: a
// lane whose findings go empty has that empty state recorded, so a finding
// that returns with the same ids is new state and is noticed again. Two
// clearings fall inside one notice bucket, where reusing the empty state's
// deterministic notice id would drop the second row.
func TestLaneCheckReclearsAfterAQuietPeriod(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "lane_reclear")
	laneCheckFixture(t, s, "reclear-project", "reclear-lane")
	now := time.Unix(time.Now().UTC().Unix()/laneCheckBucketSeconds*laneCheckBucketSeconds, 0).UTC()
	// The lane starts empty: the check records that as its last known state.
	if wrote, err := s.RunLaneCheck(ctx, now); err != nil || wrote != 0 {
		t.Fatalf("empty RunLaneCheck = %d, %v", wrote, err)
	}
	task := laneCheckTask(t, s, "reclear-project", "reclear-lane", "recurring kept work", 2)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, task.ID, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if wrote, err := s.RunLaneCheck(ctx, now.Add(time.Minute)); err != nil || wrote != 1 {
		t.Fatalf("first finding RunLaneCheck = %d, %v", wrote, err)
	}
	cur := laneCheckFindings(t, s, "reclear-lane", now.Add(time.Minute))
	if _, _, err := s.RecordLaneLook(ctx, "reclear-lane", cur.NoticeEventID, "looked", "reclear-lane", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The finding disappears (the task is parked; no wait_cleared, so only the
	// hash decides), and the check must record that empty state again even
	// though the first clearing shared this bucket.
	if _, err := s.TransitionTaskWith(ctx, task.ID, TransitionInput{To: StateUnstarted, By: "tester", Note: "parked"}); err != nil {
		t.Fatal(err)
	}
	if wrote, err := s.RunLaneCheck(ctx, now.Add(2*time.Minute)); err != nil || wrote != 0 {
		t.Fatalf("quiet RunLaneCheck = %d, %v", wrote, err)
	}
	var cleared int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM lane_looks WHERE lane=$1 AND actor=$2`, "reclear-lane", laneLookStoreActor).Scan(&cleared); err != nil {
		t.Fatal(err)
	}
	if cleared != 2 {
		t.Fatalf("clearing rows = %d, want 2", cleared)
	}
	if _, err := s.TransitionTaskWith(ctx, task.ID, TransitionInput{To: StateBacklog, By: "tester", Note: "back"}); err != nil {
		t.Fatal(err)
	}
	if wrote, err := s.RunLaneCheck(ctx, now.Add(3*time.Minute)); err != nil || wrote != 1 {
		t.Fatalf("returning finding RunLaneCheck = %d, %v; want 1", wrote, err)
	}
	if got := laneCheckOutbox(t, s, "reclear-lane"); got != 2 {
		t.Fatalf("lane_check rows = %d, want 2", got)
	}
}

// TestLaneCheckIdentity pins the notice id, the look hash and the text without
// a database: a stable hash per state, one id per 6-hour bucket, a text that
// lists at most ten ids and then the count, and a look hash that does not
// depend on wait_cleared.
func TestLaneCheckIdentity(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	a := []LaneCheckFinding{{Name: LaneFindingKeptBacklog, TaskIDs: []int64{7, 3}}, {Name: LaneFindingOrderUnacked, TaskIDs: []int64{9}}}
	b := []LaneCheckFinding{{Name: LaneFindingOrderUnacked, TaskIDs: []int64{9}}, {Name: LaneFindingKeptBacklog, TaskIDs: []int64{3, 7}}}
	if laneCheckHash(a) != laneCheckHash(b) {
		t.Fatal("hash depends on finding order")
	}
	c := append(append([]LaneCheckFinding{}, a...), LaneCheckFinding{Name: LaneFindingBlockerLanded, TaskIDs: []int64{11}})
	if laneCheckHash(a) == laneCheckHash(c) {
		t.Fatal("hash did not change with a new finding")
	}
	hash := laneCheckHash(a)
	if got := LaneCheckEventID("lane-a", hash, 0, now); got != LaneCheckEventID("lane-a", hash, 0, now.Add(5*time.Hour)) {
		t.Fatalf("ids differ inside one bucket: %s / %s", got, LaneCheckEventID("lane-a", hash, 0, now.Add(5*time.Hour)))
	}
	next := LaneCheckEventID("lane-a", hash, 0, now.Add(7*time.Hour))
	if next == LaneCheckEventID("lane-a", hash, 0, now) {
		t.Fatal("the next bucket reused the id")
	}
	lane, parsed, ok := ParseLaneCheckEventID(next)
	if !ok || lane != "lane-a" || parsed != hash {
		t.Fatalf("parse = %q %q %t", lane, parsed, ok)
	}
	// A look epoch makes the same findings in the same bucket a new id, so a
	// state that returns after a look or clearing is noticed (review of #504).
	if LaneCheckEventID("lane-a", hash, 2, now) == LaneCheckEventID("lane-a", hash, 1, now) {
		t.Fatal("the look epoch does not change the notice id")
	}
	if lane, parsed, ok := ParseLaneCheckEventID(LaneCheckEventID("lane-a", hash, 12345, now)); !ok || lane != "lane-a" || parsed != hash {
		t.Fatalf("epoch parse = %q %q %t", lane, parsed, ok)
	}
	ids := make([]int64, 12)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	text := LaneCheckNoticeText(LaneCheck{Lane: "lane-a", Findings: []LaneCheckFinding{{Name: LaneFindingKeptBacklog, TaskIDs: ids}}, NoticeEventID: next})
	if !strings.Contains(text, "1, 2, 3, 4, 5, 6, 7, 8, 9, 10 (12)") || strings.Contains(text, "11, 12") {
		t.Fatalf("text = %q", text)
	}
	if !strings.Contains(text, "handoffkeep lanes look lane-a --notice "+next) {
		t.Fatalf("text lacks the look command: %q", text)
	}
	// A look records the state it leaves: wait_cleared is not part of it, so
	// the look cannot be undone by its own baseline advance (review of #504).
	stable := []LaneCheckFinding{{Name: LaneFindingKeptBacklog, TaskIDs: []int64{7}}}
	withWait := append(append([]LaneCheckFinding{}, stable...), LaneCheckFinding{Name: LaneFindingWaitCleared, TaskIDs: []int64{9}})
	if laneCheckLookHash(withWait) != laneCheckLookHash(stable) {
		t.Fatal("look hash depends on wait_cleared")
	}
	if laneCheckHash(withWait) == laneCheckLookHash(withWait) {
		t.Fatal("the notice hash must include wait_cleared")
	}
}

// TestLaneCheckClearingEventIDsAreUnique pins the round-4 review fix: two
// clearings inside one bucket must not share an event id, or the second row is
// silently dropped by the (lane, notice_event_id) key.
func TestLaneCheckClearingEventIDsAreUnique(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	empty := laneCheckLookHash(nil)
	first := laneCheckClearingEventID("lane-a", empty, now)
	second := laneCheckClearingEventID("lane-a", empty, now.Add(time.Hour))
	if first == second {
		t.Fatalf("two clearings in one bucket share %q", first)
	}
	if !strings.HasPrefix(first, "lanecheck-clear-lane-a-"+empty+"-") {
		t.Fatalf("clearing id = %q", first)
	}
}

// TestLaneCheckOutboxKindWideningKeepsKinds pins the expand-only widening: the
// new CHECK admits lane_check and every kind the previous release writes.
func TestLaneCheckOutboxKindWideningKeepsKinds(t *testing.T) {
	prior := []string{"decision_answered", "chat_answer", "dots_report", "dots_decision", "dots_blocked", "dots_followup", "yt_write_op", "decision_denied", "disposition_answered", "deploy_result", "deploy_lag", "task_order", "task_drift"}
	pg := strings.Join(laneCheckStatements, "\n")
	for _, kind := range append(prior, OutboxKindLaneCheck) {
		if !strings.Contains(pg, "'"+kind+"'") {
			t.Errorf("PostgreSQL widening lacks %q", kind)
		}
		if !strings.Contains(mysqlOutboxKindsV4, "'"+kind+"'") {
			t.Errorf("MySQL widening lacks %q", kind)
		}
	}
	if OutboxKindLaneCheck != "lane_check" {
		t.Fatalf("kind = %q", OutboxKindLaneCheck)
	}
	if !strings.Contains(strings.Join(laneCheckStatements, "\n"), "CREATE TABLE lane_looks") {
		t.Error("PostgreSQL migration does not create lane_looks")
	}
	if !strings.Contains(strings.Join(mysqlLaneCheckSteps, "\n"), "CREATE TABLE IF NOT EXISTS lane_looks") {
		t.Error("MySQL migration does not create lane_looks")
	}
}
