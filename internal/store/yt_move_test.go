package store

import (
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// Project moves (hk 4055): a bound task whose hk project moves to another
// mapped YouTrack project gets an issue_move op, then its snapshot in that
// project, on its lane. Tests run on the fields-mode setup of yt_fields_test.go.

const (
	ytmProject   = "ytm-target"
	ytmProjectID = "0-3"
)

func ytmMapped(t *testing.T, s *Store) {
	t.Helper()
	if err := s.PutYouTrackProject(t.Context(), YouTrackProject{HKProject: ytmProject, ProjectID: ytmProjectID, ShortName: "OT"}); err != nil {
		t.Fatal(err)
	}
}

// ytmSetup is ytfSetup with the moves mode on (the move tests' setup).
func ytmSetup(t *testing.T, prefix string) (s *Store, bound, unbound int64) {
	t.Helper()
	s, bound, unbound = ytfSetup(t, prefix)
	s.SetYTMovesMode(YTMovesOn)
	return s, bound, unbound
}

// ytmOps is the issue_move ops of a task, oldest first.
func ytmOps(t *testing.T, s *Store, task int64) []ytwrite.Op {
	t.Helper()
	ops, err := s.YTWriteLedger().list(t.Context(), `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE kind=$1 AND entity_kind='task' AND entity_id=$2 ORDER BY id`,
		ytwrite.KindIssueMove, strconv.FormatInt(task, 10))
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// ytmSettle claims op on the tenant's worker and applies it with issue as
// the answer, as the worker does when YouTrack confirms the write. The op must
// be due and first on its lane.
func ytmSettle(t *testing.T, s *Store, op ytwrite.Op, issue string) {
	t.Helper()
	if out := ytiClaim(t, s, op); out != ytwrite.ClaimClaimed {
		t.Fatalf("claim of op %d (%s): %s", op.ID, op.Kind, out)
	}
	if _, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), op.ID, issue, nil); err != nil {
		t.Fatal(err)
	}
}

// ytmBinding is the task's binding project and readable id.
func ytmBinding(t *testing.T, s *Store, task int64) (project, readable string) {
	t.Helper()
	if err := s.pool.QueryRow(t.Context(), `SELECT project_id, issue_readable FROM yt_task_bindings WHERE task_id=$1`, task).Scan(&project, &readable); err != nil {
		t.Fatal(err)
	}
	return project, readable
}

// A project change makes one move, then the snapshot in the target project.
// The snapshot waits for the move on the lane; the move's apply moves the
// binding's project in the same transaction.
func TestYTMoveProjectChangeMakesMoveThenFields(t *testing.T) {
	s, bound, _ := ytmSetup(t, "ytm_move")
	ytmMapped(t, s)
	ytFeedExec(t, s, `UPDATE tasks SET project=$2, priority=5 WHERE id=$1`, bound, ytmProject)
	ytfPass(t, s)
	moves := ytmOps(t, s, bound)
	if len(moves) != 1 {
		t.Fatalf("move ops %+v, want one", moves)
	}
	mv := moves[0]
	if mv.State != ytwrite.StatePrepared || mv.Principal != YTFieldsPrincipal || mv.Tenant != ytcTenant || mv.YTTargetID != ytfIssue(bound) ||
		!strings.HasPrefix(mv.Key, ytwrite.KindIssueMove+":"+strconv.FormatInt(bound, 10)+":1:") {
		t.Fatalf("move op %+v", mv)
	}
	if project, err := ytwrite.IssueMoveProject(mv); err != nil || project != ytmProjectID {
		t.Fatalf("move target %q %v", project, err)
	}
	fields := ytfOps(t, s, bound)
	if len(fields) != 1 || fields[0].ID <= mv.ID {
		t.Fatalf("snapshot ops %+v after move %d", fields, mv.ID)
	}
	if p := ytfPayload(t, fields[0]); p.ProjectID != ytmProjectID || ytfField(p, "hk 프로젝트") != ytmProject {
		t.Fatalf("snapshot payload %+v", p)
	}
	if out := ytiClaim(t, s, fields[0]); out != ytwrite.ClaimBlocked {
		t.Fatalf("snapshot claimed before its move: %s", out)
	}
	ytmSettle(t, s, mv, ytfIssue(bound))
	if project, readable := ytmBinding(t, s, bound); project != ytmProjectID || readable != "" {
		t.Fatalf("binding after the move: project %q readable %q", project, readable)
	}
	if out := ytiClaim(t, s, fields[0]); out != ytwrite.ClaimClaimed {
		t.Fatalf("snapshot after its move: %s", out)
	}
}

// A target project no youtrack_projects row maps prepares nothing: no move,
// no snapshot.
func TestYTMoveUnmappedTargetPreparesNothing(t *testing.T) {
	s, bound, _ := ytmSetup(t, "ytm_unmapped")
	ytFeedExec(t, s, `UPDATE tasks SET project='ytm-nowhere', priority=5 WHERE id=$1`, bound)
	ytfPass(t, s)
	if ops := ytmOps(t, s, bound); len(ops) != 0 {
		t.Fatalf("move ops to an unmapped project %+v", ops)
	}
	if ops := ytfOps(t, s, bound); len(ops) != 0 {
		t.Fatalf("snapshot ops for an unmapped project %+v", ops)
	}
	if project, _ := ytmBinding(t, s, bound); project != ytbProjectID {
		t.Fatalf("binding moved to %q", project)
	}
}

// Moved back after the first move applied, the task gets another move.
func TestYTMoveBackAgainMakesAnotherMove(t *testing.T) {
	s, bound, _ := ytmSetup(t, "ytm_back")
	ytmMapped(t, s)
	ytFeedExec(t, s, `UPDATE tasks SET project=$2 WHERE id=$1`, bound, ytmProject)
	ytfPass(t, s)
	first := ytmOps(t, s, bound)
	if len(first) != 1 {
		t.Fatalf("move ops %+v", first)
	}
	ytmSettle(t, s, first[0], ytfIssue(bound))
	ytFeedExec(t, s, `UPDATE tasks SET project=$2 WHERE id=$1`, bound, ytbProject)
	ytfPass(t, s)
	moves := ytmOps(t, s, bound)
	if len(moves) != 2 || moves[1].State != ytwrite.StatePrepared || !strings.HasPrefix(moves[1].Key, ytwrite.KindIssueMove+":"+strconv.FormatInt(bound, 10)+":2:") {
		t.Fatalf("move ops %+v, want a second one", moves)
	}
	if p, err := ytwrite.IssueMoveProject(moves[1]); err != nil || p != ytbProjectID {
		t.Fatalf("second move target %q %v", p, err)
	}
}

// Moved away and back before the first move applied: the issue must end where
// the task is. The second move is prepared behind the first, and the snapshots
// follow their moves on the lane.
func TestYTMoveBackBeforeTheMoveAppliesMovesBack(t *testing.T) {
	s, bound, _ := ytmSetup(t, "ytm_pending")
	ytmMapped(t, s)
	ytFeedExec(t, s, `UPDATE tasks SET project=$2 WHERE id=$1`, bound, ytmProject)
	ytfPass(t, s)
	ytFeedExec(t, s, `UPDATE tasks SET project=$2 WHERE id=$1`, bound, ytbProject)
	ytfPass(t, s)
	moves := ytmOps(t, s, bound)
	if len(moves) != 2 {
		t.Fatalf("move ops %+v, want two", moves)
	}
	if p, _ := ytwrite.IssueMoveProject(moves[0]); p != ytmProjectID {
		t.Fatalf("first move to %q", p)
	}
	if p, _ := ytwrite.IssueMoveProject(moves[1]); p != ytbProjectID {
		t.Fatalf("second move to %q", p)
	}
	fields := ytfOps(t, s, bound)
	if len(fields) != 2 || ytfPayload(t, fields[1]).ProjectID != ytbProjectID {
		t.Fatalf("snapshot ops %+v", fields)
	}
	ytmSettle(t, s, moves[0], ytfIssue(bound))
	ytmSettle(t, s, fields[0], ytfIssue(bound))
	ytmSettle(t, s, moves[1], ytfIssue(bound))
	ytmSettle(t, s, fields[1], ytfIssue(bound))
	if project, _ := ytmBinding(t, s, bound); project != ytbProjectID {
		t.Fatalf("binding ends in %q, want the task's project %q", project, ytbProjectID)
	}
}

// A lost reply to a move is settled by the issue's project, read back from
// YouTrack: the move applies, and the binding takes the target project. The
// move is sent once.
func TestYTMoveLostReplyIsSettledByProject(t *testing.T) {
	s, bound, _ := ytmSetup(t, "ytm_evidence")
	ytmMapped(t, s)
	yt := ytwritetest.NewFake()
	issue := yt.InjectIssue(ytbProjectID, bound, "#"+strconv.FormatInt(bound, 10)+" move", "d")
	ytFeedExec(t, s, `UPDATE yt_task_bindings SET issue_id=$2 WHERE task_id=$1`, bound, issue)
	ytFeedExec(t, s, `UPDATE tasks SET project=$2 WHERE id=$1`, bound, ytmProject)
	ytfPass(t, s)
	moves := ytmOps(t, s, bound)
	if len(moves) != 1 || moves[0].YTTargetID != issue {
		t.Fatalf("move ops %+v", moves)
	}
	yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
	var offset atomic.Int64
	p := ytcProtocolOn(t, s, yt, ytcTenant, &offset)
	if _, err := p.Drive(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	if op, err := s.YTWriteLedger().ByID(t.Context(), moves[0].ID); err != nil || op.State != ytwrite.StateRemoteUnknown {
		t.Fatalf("move after a lost reply: %+v %v", op, err)
	}
	if project, _ := yt.ProjectOf(issue); project != ytmProjectID {
		t.Fatalf("the move did not reach YouTrack: issue in %q", project)
	}
	offset.Store(int64(time.Hour))
	if _, err := p.Drive(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	if op, err := s.YTWriteLedger().ByID(t.Context(), moves[0].ID); err != nil || op.State != ytwrite.StateApplied || op.YTResultID != issue {
		t.Fatalf("move after evidence: %+v %v", op, err)
	}
	if n := len(yt.Moves()); n != 1 {
		t.Fatalf("YouTrack received %d moves, want 1", n)
	}
	if project, _ := ytmBinding(t, s, bound); project != ytmProjectID {
		t.Fatalf("binding project %q after the applied move", project)
	}
}

// Moves mode off (the default): a moved task is skipped as before moves
// existed, with no move and no snapshot. Turning moves off holds a move that
// was already prepared: the worker admits nothing while it is off.
func TestYTMoveModeOffPreparesNothing(t *testing.T) {
	s, bound, _ := ytfSetup(t, "ytm_off")
	ytmMapped(t, s)
	ytFeedExec(t, s, `UPDATE tasks SET project=$2 WHERE id=$1`, bound, ytmProject)
	ytfPass(t, s)
	if ops := ytmOps(t, s, bound); len(ops) != 0 {
		t.Fatalf("moves off prepared moves %+v", ops)
	}
	if ops := ytfOps(t, s, bound); len(ops) != 0 {
		t.Fatalf("moves off prepared snapshots %+v", ops)
	}
	// On, the same change is a move; off again, the move is held.
	s.SetYTMovesMode(YTMovesOn)
	ytFeedExec(t, s, `UPDATE tasks SET priority=6 WHERE id=$1`, bound)
	ytfPass(t, s)
	moves := ytmOps(t, s, bound)
	if len(moves) != 1 {
		t.Fatalf("moves on prepared %+v, want one", moves)
	}
	s.SetYTMovesMode(YTMovesOff)
	if out := ytiClaim(t, s, moves[0]); out != ytwrite.ClaimHeld {
		t.Fatalf("claim of a move with moves off: %s", out)
	}
}
