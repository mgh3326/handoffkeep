package store

import (
	"context"
	"sort"
	"testing"
	"time"
)

// The snapshot-consistency cases for the three REPEATABLE READ read-only
// snapshots ported to MySQL in hk 5149 (design §11.1): attention, the console
// work boards and the decision tab. Each case starts its read in a goroutine,
// commits a concurrent write from another connection while the read is parked
// at its seam, and asserts the read still describes the pre-write snapshot: a
// concurrent write between two statements of the read is not seen. Cases use
// only public store methods, so the same list runs on PostgreSQL here and on
// MySQL in snapshot_reads_mysql_test.go.
type snapshotReadCase struct {
	name string
	run  func(t *testing.T, s *Store)
}

var snapshotReadCases []snapshotReadCase

func sortedSnapshotReadCases() []snapshotReadCase {
	out := append([]snapshotReadCase(nil), snapshotReadCases...)
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func TestSnapshotReadsPostgres(t *testing.T) {
	for _, c := range sortedSnapshotReadCases() {
		t.Run(c.name, func(t *testing.T) { c.run(t, schemaStore(t, "snapshot_reads")) })
	}
}

// snapshotBarrier parks the reading goroutine at its seam until the test's
// concurrent write has committed.
type snapshotBarrier struct {
	entered chan struct{}
	release chan struct{}
}

func newSnapshotBarrier() *snapshotBarrier {
	return &snapshotBarrier{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *snapshotBarrier) seam(context.Context) {
	close(b.entered)
	<-b.release
}

func (b *snapshotBarrier) wait(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("snapshot seam not reached")
	}
}

func snapshotProbeTask(t *testing.T, s *Store, lane string) Task {
	t.Helper()
	task, err := s.CreateTask(t.Context(), Task{Lane: lane, Title: "snapshot probe", Kind: "implement", CreatedBy: "snapshot-test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func init() {
	snapshotReadCases = append(snapshotReadCases,
		snapshotReadCase{"attention", snapshotAttentionCase},
		snapshotReadCase{"decision_tab", snapshotDecisionTabCase},
		snapshotReadCase{"work_boards", snapshotWorkBoardsCase},
	)
}

// snapshotAttentionCase parks the attention read after its decision count and
// commits a second open request in the gap. The finished read must still show
// the one pre-write decision.
func snapshotAttentionCase(t *testing.T, s *Store) {
	ctx := t.Context()
	now := time.Now().UTC()
	seedA := snapshotProbeTask(t, s, "snapshot-attn-a")
	if _, err := s.RecordDecisionRequest(ctx, seedA.ID, "snapshot-test", decisionInput()); err != nil {
		t.Fatal(err)
	}
	base, err := s.Attention(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if base.Decisions.Count != 1 || len(base.Decisions.Items) != 1 {
		t.Fatalf("seeded attention decisions=%+v", base.Decisions)
	}

	bar := newSnapshotBarrier()
	attentionDecisionsSeam = bar.seam
	defer func() { attentionDecisionsSeam = nil }()
	type attentionResult struct {
		out Attention
		err error
	}
	done := make(chan attentionResult, 1)
	go func() {
		out, err := s.Attention(ctx, now)
		done <- attentionResult{out, err}
	}()
	bar.wait(t)
	seedB := snapshotProbeTask(t, s, "snapshot-attn-b")
	if _, err := s.RecordDecisionRequest(ctx, seedB.ID, "snapshot-test", decisionInput()); err != nil {
		t.Fatal(err)
	}
	close(bar.release)
	var res attentionResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("attention read did not finish")
	}
	attentionDecisionsSeam = nil
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.out.Decisions.Count != 1 || len(res.out.Decisions.Items) != 1 || res.out.Decisions.Overdue != 0 {
		t.Fatalf("snapshot mixed pre/post-write decisions: %+v", res.out.Decisions)
	}
}

// snapshotDecisionTabCase parks the decision-tab read after its count and
// commits a second open request in the gap. The finished tab must still show
// the one pre-write row.
func snapshotDecisionTabCase(t *testing.T, s *Store) {
	ctx := t.Context()
	now := time.Now().UTC()
	seedA := snapshotProbeTask(t, s, "snapshot-tab-a")
	if _, err := s.RecordDecisionRequest(ctx, seedA.ID, "snapshot-test", decisionInput()); err != nil {
		t.Fatal(err)
	}
	base, err := s.ListDecisionTab(ctx, "open", now, 200)
	if err != nil {
		t.Fatal(err)
	}
	if base.Open != 1 || len(base.Items) != 1 {
		t.Fatalf("seeded decision tab=%+v", base)
	}

	bar := newSnapshotBarrier()
	decisionTabSeam = bar.seam
	defer func() { decisionTabSeam = nil }()
	type tabResult struct {
		out DecisionTab
		err error
	}
	done := make(chan tabResult, 1)
	go func() {
		out, err := s.ListDecisionTab(ctx, "open", now, 200)
		done <- tabResult{out, err}
	}()
	bar.wait(t)
	seedB := snapshotProbeTask(t, s, "snapshot-tab-b")
	if _, err := s.RecordDecisionRequest(ctx, seedB.ID, "snapshot-test", decisionInput()); err != nil {
		t.Fatal(err)
	}
	close(bar.release)
	var res tabResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("decision tab read did not finish")
	}
	decisionTabSeam = nil
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.out.Open != 1 || res.out.Overdue != 0 || len(res.out.Items) != 1 || res.out.Truncated {
		t.Fatalf("snapshot mixed pre/post-write tab: %+v", res.out)
	}
}

// snapshotWorkBoardsCase parks the work-board read after its placements and
// completes the placed job in the gap. The finished board must still show the
// session as it was: placed and running.
func snapshotWorkBoardsCase(t *testing.T, s *Store) {
	ctx := t.Context()
	task := snapshotProbeTask(t, s, "snapshot-board")
	host := "snapshot-host"
	at := time.Now().UTC().Add(-time.Minute)
	if _, _, err := s.RecordPlacement(ctx, PlacementDecision{JobID: "snapshot-job-1", Attempt: 1, TaskID: &task.ID, DecidedAt: at, DecidedOn: "snapshot", Source: "forced", Ranking: "forced", StateSource: "hub", ChosenHost: &host, Outcome: "placed", Family: "openai", Model: "snapshot-model", Role: "worker", Candidates: []PlacementCandidate{}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimJob(ctx, Job{JobID: "snapshot-job-1", TaskID: &task.ID, Host: host, CreatedBy: "snapshot-test"}); err != nil {
		t.Fatal(err)
	}
	spawned := "spawned"
	if _, err := s.UpdateJob(ctx, "snapshot-job-1", JobUpdate{State: &spawned}); err != nil {
		t.Fatal(err)
	}
	base, err := s.ReadTaskWorkBoards(ctx, []Task{task})
	if err != nil {
		t.Fatal(err)
	}
	if len(base[task.ID].Sessions) != 1 || !base[task.ID].Sessions[0].Active {
		t.Fatalf("seeded board=%+v", base[task.ID])
	}

	bar := newSnapshotBarrier()
	workBoardsSeam = bar.seam
	defer func() { workBoardsSeam = nil }()
	type boardsResult struct {
		out map[int64]TaskWorkBoard
		err error
	}
	done := make(chan boardsResult, 1)
	go func() {
		out, err := s.ReadTaskWorkBoards(ctx, []Task{task})
		done <- boardsResult{out, err}
	}()
	bar.wait(t)
	completed := "completed"
	if _, err := s.UpdateJob(ctx, "snapshot-job-1", JobUpdate{State: &completed}); err != nil {
		t.Fatal(err)
	}
	close(bar.release)
	var res boardsResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("work boards read did not finish")
	}
	workBoardsSeam = nil
	if res.err != nil {
		t.Fatal(res.err)
	}
	sessions := res.out[task.ID].Sessions
	if len(sessions) != 1 || !sessions[0].Active || !sessions[0].EndedAt.IsZero() {
		t.Fatalf("snapshot mixed pre/post-write board: %+v", res.out[task.ID])
	}
}
