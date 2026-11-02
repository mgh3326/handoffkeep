package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func validPlacementFixture() PlacementDecision {
	task := int64(1)
	host := "host-a"
	return PlacementDecision{JobID: "board-job", Attempt: 1, TaskID: &task, DecidedAt: time.Now().UTC().Truncate(time.Microsecond), DecidedOn: "host-a", Source: "auto", Ranking: "score", StateSource: "hub", ChosenHost: &host, Outcome: "placed", Family: "openai", Model: "model-a", Profile: "profile-a", Role: "lead", Repo: "handoffkeep", Candidates: []PlacementCandidate{}}
}

func TestPlacementValidation(t *testing.T) {
	if err := validatePlacement(validPlacementFixture()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*PlacementDecision)
	}{
		{"identity", func(d *PlacementDecision) { d.JobID = "../bad" }},
		{"time", func(d *PlacementDecision) { d.DecidedAt = time.Now().Add(time.Hour) }},
		{"retention", func(d *PlacementDecision) { d.DecidedAt = time.Now().Add(-181 * 24 * time.Hour) }},
		{"role", func(d *PlacementDecision) { d.Role = "builder" }},
		{"host", func(d *PlacementDecision) { d.ChosenHost = nil }},
		{"family", func(d *PlacementDecision) { d.Family = "" }},
		{"account", func(d *PlacementDecision) { d.AccountKey = "private/account" }},
		{"candidates", func(d *PlacementDecision) { d.Candidates = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := validPlacementFixture()
			tc.change(&d)
			if err := validatePlacement(d); err == nil {
				t.Fatal("accepted invalid placement")
			}
		})
	}
}

func TestWorkBoardEventOrderingAndPlacement(t *testing.T) {
	at := time.Now().UTC()
	var b LinearBoard
	task := Task{ID: 1, State: "claimed", Refs: TaskRefs{JobID: "board-job"}}
	b.ApplyTask(task)
	d := validPlacementFixture()
	b.ApplyPlacement(d)
	b.ApplyEvent(LinearBoardEvent{ID: "ready", Kind: "READY", At: at}, "board-job", "https://github.com/example/repo/pull/1", "abc123")
	b.ApplyTask(task)
	if b.State != "In Review" || b.Task.Refs.PR == "" || b.Task.Refs.HeadSHA != "abc123" {
		t.Fatal(b)
	}
	b.ApplyEvent(LinearBoardEvent{ID: "merge", Kind: "MERGED", At: at.Add(time.Minute)}, "board-job", "", "")
	b.ApplyEvent(LinearBoardEvent{ID: "late", Kind: "READY", At: at.Add(-time.Minute)}, "board-job", "", "")
	if b.State != "Done" || b.Last.ID != "merge" {
		t.Fatal(b)
	}
	for _, kind := range []string{"READY", "ESC", "BLOCKED", "FINDINGS", "MERGED"} {
		e, job, ok := BoardRelayEvent(RelayEvent{ID: 1, Kind: "lane.event", Text: "[board-job] " + kind + " | details", ReceivedAt: at})
		if !ok || job != "board-job" || e.Kind != kind {
			t.Fatal(e, job, ok)
		}
	}
	if _, _, ok := BoardRelayEvent(RelayEvent{Kind: "lane.event", Text: "ordinary chat"}); ok {
		t.Fatal("chat interpreted as board event")
	}
}

func TestWorkBoardCommittedEventsAndPlacementRetention(t *testing.T) {
	_, url := testdb.Schema(t, "work_board")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	st.EnableLinearBoard()
	if err = st.migrate(t.Context()); err != nil {
		t.Fatal("second migration", err)
	}
	task := linearTask(t, st, TaskRefs{})
	rows, err := st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	task, err = st.ClaimTask(t.Context(), task.ID, "lead", "board-job", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ClaimTask(t.Context(), task.ID, "lead", "board-job", ""); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	d := validPlacementFixture()
	d.TaskID = &task.ID
	got, created, err := st.RecordPlacement(t.Context(), d)
	if err != nil || !created {
		t.Fatal(got, created, err)
	}
	if _, created, err = st.RecordPlacement(t.Context(), d); err != nil || created {
		t.Fatal(created, err)
	}
	changed := d
	changed.Model = "other"
	if _, _, err = st.RecordPlacement(t.Context(), changed); !errors.Is(err, ErrPlacementConflict) {
		t.Fatal(err)
	}
	event := RelayEvent{Kind: "lane.event", OwnerLane: task.Lane, EventID: "board-ready", Text: "[board-job] READY | test"}
	if _, created, err = st.AppendRelayEvent(t.Context(), event); err != nil || !created {
		t.Fatal(created, err)
	}
	if _, created, err = st.AppendRelayEvent(t.Context(), event); err != nil || created {
		t.Fatal(created, err)
	}
	rows, err = st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	last := rows[len(rows)-1]
	if last.Payload.Board.State != "In Review" || last.Payload.Event.Kind != "READY" {
		t.Fatal(last)
	}
	if _, err = st.pool.Exec(t.Context(), `CREATE FUNCTION fail_board_queue() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'board queue failure'; END; $$`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(t.Context(), `CREATE TRIGGER fail_board_queue BEFORE INSERT ON linear_outbox FOR EACH ROW EXECUTE FUNCTION fail_board_queue()`); err != nil {
		t.Fatal(err)
	}
	failed := d
	failed.JobID = "rolled-back"
	failed.Attempt = 2
	if _, _, err = st.RecordPlacement(t.Context(), failed); err == nil || !strings.Contains(err.Error(), "board queue failure") {
		t.Fatal(err)
	}
	placements, err := st.ListPlacements(t.Context(), "rolled-back", "", time.Time{}, 10)
	if err != nil || len(placements) != 0 {
		t.Fatal(placements, err)
	}
	if _, err = st.pool.Exec(t.Context(), `DROP TRIGGER fail_board_queue ON linear_outbox`); err != nil {
		t.Fatal(err)
	}
	// Retention deletes only the expired row, in this isolated schema.
	if _, err = st.pool.Exec(t.Context(), `UPDATE placement_decisions SET decided_at=$1 WHERE id=$2`, time.Now().Add(-181*24*time.Hour), got.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := st.PrunePlacements(t.Context()); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if err = st.ReconcileLinearBoard(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 4 || rows[3].Payload.Event.Kind != "" {
		t.Fatal(rows, err)
	}
	// A linked issue is also recorded in the task refs by the same outbox tx.
	for range 4 {
		_, err = st.ProcessNextLinearBoardOutbox(t.Context(), func(_ context.Context, op LinearOutbox) (LinearOutboxResult, error) {
			return LinearOutboxResult{State: "sent", RemoteID: "remote", Identifier: "ROB-1234"}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	linked, found, err := st.GetTask(t.Context(), task.ID)
	if err != nil || !found || linked.Refs.Linear.IssueID != "remote" {
		t.Fatal(linked, found, err)
	}
}

func TestWorkBoardTerminalBeforePlacementAndLateJob(t *testing.T) {
	at := time.Now().UTC()
	var b LinearBoard
	b.ApplyTask(Task{ID: 1, State: "claimed", UpdatedAt: at})
	b.ApplyEvent(LinearBoardEvent{Kind: "FINDINGS", Text: "2 findings: changes requested", At: at.Add(time.Minute)}, "review-job", "", "")
	d := validPlacementFixture()
	d.JobID = "review-job"
	d.Role = "review"
	d.DecidedAt = at
	b.ApplyPlacement(d)
	session := b.Sessions[b.sessionIndex("review-job")]
	if session.Active || session.Verdict != "2 findings: changes requested" || session.Host != "host-a" {
		t.Fatal(session)
	}
	b.ApplyJob(Job{JobID: "review-job", State: "completed", PaneID: "review-pane", UpdatedAt: at.Add(2 * time.Minute)})
	session = b.Sessions[b.sessionIndex("review-job")]
	if session.Active || session.Verdict != "2 findings: changes requested" || session.Pane != "review-pane" {
		t.Fatal(session)
	}
	b.ApplyEvent(LinearBoardEvent{Kind: "FINDINGS", Text: "old verdict", At: at.Add(30 * time.Second)}, "review-job", "", "")
	if b.Sessions[b.sessionIndex("review-job")].Verdict != "2 findings: changes requested" {
		t.Fatal(b.Sessions)
	}
}

func TestWorkBoardMergewakeProtocolAndPRReferences(t *testing.T) {
	for _, text := range []string{"handoffkeep#12", "owner/handoffkeep#12", "https://github.com/owner/handoffkeep/pull/12"} {
		if got := BoardPRRef(text); got != "handoffkeep#12" {
			t.Fatal(text, got)
		}
	}
	event, job, ok := BoardRelayEvent(RelayEvent{Kind: "lane.event", Text: "[mergewake] FINDINGS handoffkeep#12 head=abc1234 | 2 finding(s) for lane work", ReceivedAt: time.Now().UTC()})
	if !ok || event.Kind != "FINDINGS" || event.PR != "handoffkeep#12" || event.Head != "abc1234" || job != "mergewake" {
		t.Fatal(event, job, ok)
	}
	var b LinearBoard
	b.ApplyTask(Task{ID: 1, State: "claimed", Refs: TaskRefs{PR: "https://github.com/owner/handoffkeep/pull/12"}})
	d := validPlacementFixture()
	d.Role = "review"
	b.ApplyPlacement(d)
	event.At = d.DecidedAt.Add(time.Second)
	b.ApplyEvent(event, job, event.PR, event.Head)
	if b.Sessions[0].Active || b.Sessions[0].Verdict == "" || !strings.HasPrefix(b.Task.Refs.PR, "https://") {
		t.Fatal(b)
	}
	b.ApplyEvent(LinearBoardEvent{Kind: "MERGED", At: event.At.Add(time.Minute)}, job, event.PR, "")
	b.ApplyTask(Task{ID: 1, State: "in_progress", Refs: b.Task.Refs})
	if b.State != "Done" {
		t.Fatal("task metadata reopened merged issue", b)
	}
}

func TestWorkBoardReconcileRecoversPlacementAndReviewOwner(t *testing.T) {
	_, url := testdb.Schema(t, "board_recovery")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	owner := linearTask(t, st, TaskRefs{})
	reviewer := linearTask(t, st, TaskRefs{})
	job, _, err := st.ClaimJob(t.Context(), Job{JobID: "review-owned", TaskID: reviewer.ID, Host: "host-a", CreatedBy: "node"})
	if err != nil {
		t.Fatal(err)
	}
	d := validPlacementFixture()
	d.JobID = job.JobID
	d.TaskID = &owner.ID
	d.Role = "review"
	if _, _, err = st.RecordPlacement(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	// A write made while the board was disabled is recovered at the hourly reconciliation.
	st.EnableLinearBoard()
	if err = st.ReconcileLinearBoard(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListLinearOutbox(t.Context(), owner.ID)
	if err != nil || len(rows) != 1 || len(rows[0].Payload.Board.Sessions) != 1 {
		t.Fatal(rows, err)
	}
	state, pane := "spawned", "review-pane"
	if _, err = st.UpdateJob(t.Context(), job.JobID, JobUpdate{State: &state, PaneID: &pane}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.AppendRelayEvent(t.Context(), RelayEvent{Kind: "job.completed", JobID: job.JobID, OwnerLane: owner.Lane, ReportLastLine: "0 findings; approved"}); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListLinearOutbox(t.Context(), owner.ID)
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	last := rows[2].Payload.Board
	if last.Sessions[0].Pane != pane || last.Sessions[0].Verdict != "0 findings; approved" || last.Sessions[0].Active {
		t.Fatal(last)
	}
	other, err := st.ListLinearOutbox(t.Context(), reviewer.ID)
	if err != nil || len(other) != 0 {
		t.Fatal("review job updated its separate task", other, err)
	}
}

func TestWorkBoardMigrationAfterExistingVersion28(t *testing.T) {
	_, url := testdb.Schema(t, "board_v29")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	for _, query := range []string{`DROP TABLE linear_board_tasks, placement_decisions`, `ALTER TABLE linear_outbox DROP CONSTRAINT linear_outbox_op_check`, `ALTER TABLE linear_outbox ADD CONSTRAINT linear_outbox_op_check CHECK(op IN ('issue_create','issue_state','terminal_comment','terminal_archive'))`, `DELETE FROM schema_version WHERE version=29`} {
		if _, err = st.pool.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = st.migrate(t.Context()); err != nil {
		t.Fatal("repeated migration", err)
	}
	var versions int
	if err = st.pool.QueryRow(t.Context(), `SELECT count(*) FROM schema_version WHERE version IN (28,29)`).Scan(&versions); err != nil || versions != 2 || SchemaVersion != 29 {
		t.Fatal(versions, SchemaVersion, err)
	}
	if _, err = st.pool.Exec(t.Context(), `SELECT job_id FROM placement_decisions LIMIT 1`); err != nil {
		t.Fatal(err)
	}
}
