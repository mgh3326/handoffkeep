package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// ledgerStore runs each ledger test in a throwaway schema: the ledger reads
// every task, decision and feed row, so its sections are exact there.
func ledgerStore(t *testing.T) *Store {
	t.Helper()
	pool, _ := testdb.SchemaPool(t, "ledger")
	s := &Store{pool: pool}
	if err := s.migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

func ledgerIDs(xs []LedgerStartedTask) []int64 {
	out := []int64{}
	for _, x := range xs {
		out = append(out, x.ID)
	}
	return out
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sectionsOf maps each task id to the first-three sections it appears in.
func sectionsOf(in LedgerInFlight, w LedgerWaiting, st LedgerStalled) map[int64][]string {
	out := map[int64][]string{}
	for _, x := range in.Tasks {
		out[x.ID] = append(out[x.ID], "in_flight")
	}
	for _, g := range w.Groups {
		if g.TaskID != nil {
			out[*g.TaskID] = append(out[*g.TaskID], "waiting")
		}
	}
	seen := map[int64]bool{}
	for _, x := range st.Tasks {
		out[x.ID] = append(out[x.ID], "stalled")
		seen[x.ID] = true
	}
	for _, e := range st.Events {
		if !seen[e.Task.ID] {
			out[e.Task.ID] = append(out[e.Task.ID], "stalled")
			seen[e.Task.ID] = true
		}
	}
	return out
}

func TestLedgerNextTrigger(t *testing.T) {
	pr := "https://github.com/example/app/pull/1"
	for _, tc := range []struct {
		state, pr, trigger, source string
	}{
		{StateResearching, "", "review", SourceMergewakePR},
		{StateDeveloping, "", "review", SourceMergewakePR},
		{StateDeveloping, pr, "review", SourceMergewakePR},
		{StateReviewing, pr, "merge", SourceMergewakeMerge},
		{StateReviewing, "", "merge", "manual"},
		{StateMerged, pr, "deploy", SourceDeploy},
		{StateMerged, "", "", ""},
		{StateBacklog, "", "", ""},
		{StateDropped, pr, "", ""},
	} {
		trigger, source := ledgerNextTrigger(tc.state, tc.pr)
		if trigger != tc.trigger || source != tc.source {
			t.Errorf("%s pr=%q: got %q %q, want %q %q", tc.state, tc.pr, trigger, source, tc.trigger, tc.source)
		}
	}
}

// TestLedgerSectionsPlaceEachTaskOnce drives the placement rule without a
// database: fresh work is in flight, idle work is stalled, and a task with
// the flag or any open request is only under Waiting.
func TestLedgerSectionsPlaceEachTaskOnce(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	after := 24 * time.Hour
	project := "experiment"
	row := func(id int64, state string, activity time.Time, flagged bool) ledgerStartedRow {
		return ledgerStartedRow{LedgerStartedTask: LedgerStartedTask{LedgerTask: LedgerTask{ID: id, State: state, Project: &project}, ActivityAt: activity}, Flagged: flagged}
	}
	fresh := row(1, StateDeveloping, now.Add(-time.Hour), false)
	stalledOld := row(2, StateReviewing, now.Add(-72*time.Hour), false)
	stalledNewer := row(3, StateResearching, now.Add(-30*time.Hour), false)
	flaggedOld := row(4, StateDeveloping, now.Add(-72*time.Hour), true)
	listedUnflagged := row(5, StateDeveloping, now.Add(-time.Hour), false) // a listed request without the flag
	snoozedOnly := row(6, StateDeveloping, now.Add(-72*time.Hour), true)   // open request, snoozed: not listed
	task4, task5, missing := int64(4), int64(5), int64(99)
	records := []DecisionRecord{
		{ID: 10, Source: DecisionSourceChat, Question: "no task", CreatedAt: now.Add(-5 * time.Hour)},
		{ID: 11, Source: DecisionSourceTaskFlag, TaskID: &task4, CreatedAt: now.Add(-4 * time.Hour)},
		{ID: 12, Source: DecisionSourceJobEscalate, TaskID: &task5, CreatedAt: now.Add(-3 * time.Hour)},
		{ID: 13, Source: DecisionSourceTaskRequest, TaskID: &task4, CreatedAt: now.Add(-2 * time.Hour)},
		{ID: 14, Source: DecisionSourceLaneEvent, TaskID: &missing, CreatedAt: now.Add(-time.Hour)},
	}
	tasks := map[int64]LedgerTask{4: {ID: 4, State: StateDeveloping}, 5: {ID: 5, State: StateDeveloping}}
	events := []LedgerFlaggedEvent{
		{EventID: 20, Task: LedgerTask{ID: 7, State: StateDropped}, Source: SourceMergewakeMerge, At: now.Add(-time.Hour)},
		{EventID: 21, Task: LedgerTask{ID: 5, State: StateDropped}, Source: SourceMergewakeMerge, At: now.Add(-time.Hour)},
	}
	started := []ledgerStartedRow{fresh, stalledOld, stalledNewer, flaggedOld, listedUnflagged, snoozedOnly}
	in, w, st := ledgerSections(started, records, tasks, events, now, after)

	if got := ledgerIDs(in.Tasks); !sameIDs(got, []int64{1}) {
		t.Fatalf("in flight %v", got)
	}
	if in.Tasks[0].NextTrigger != "review" || in.Tasks[0].NextSource != SourceMergewakePR {
		t.Fatalf("next trigger %+v", in.Tasks[0])
	}
	if got := ledgerIDs(st.Tasks); !sameIDs(got, []int64{2, 3}) {
		t.Fatalf("stalled %v, want oldest activity first", got)
	}
	if len(st.Events) != 1 || st.Events[0].EventID != 20 {
		t.Fatalf("stalled events %+v: an event of a waiting task must not repeat it", st.Events)
	}
	if w.Count != len(records) || len(w.Groups) != 4 {
		t.Fatalf("waiting %+v", w)
	}
	// Groups follow their oldest request; the no-task group is last.
	if g := w.Groups[0]; g.TaskID == nil || *g.TaskID != 4 || g.Task == nil || len(g.Requests) != 2 || g.Requests[0].ID != 11 || g.Requests[1].ID != 13 {
		t.Fatalf("group 0 %+v", g)
	}
	if g := w.Groups[1]; g.TaskID == nil || *g.TaskID != 5 || len(g.Requests) != 1 {
		t.Fatalf("group 1 %+v", g)
	}
	if g := w.Groups[2]; g.TaskID == nil || *g.TaskID != 99 || g.Task != nil {
		t.Fatalf("group 2 %+v: a task that does not exist keeps its id and no task", g)
	}
	if g := w.Groups[3]; g.TaskID != nil || len(g.Requests) != 1 || g.Requests[0].ID != 10 {
		t.Fatalf("group 3 %+v", g)
	}
	for id, where := range sectionsOf(in, w, st) {
		if len(where) > 1 {
			t.Errorf("task %d is in %v", id, where)
		}
	}
	if where := sectionsOf(in, w, st)[6]; len(where) != 0 {
		t.Errorf("a task whose only request is snoozed is in %v", where)
	}

	// Empty input still gives empty lists, never null.
	in, w, st = ledgerSections(nil, nil, nil, nil, now, after)
	raw, err := json.Marshal(struct {
		A LedgerInFlight
		B LedgerWaiting
		C LedgerStalled
	}{in, w, st})
	if err != nil || bytes.Contains(raw, []byte("null")) {
		t.Fatalf("empty sections %s err=%v", raw, err)
	}
}

func TestLedgerProjectsStalenessAtFixedTimes(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	a, b, c := "alpha", "beta", "gamma"
	row := func(p *string) ledgerStartedRow {
		return ledgerStartedRow{LedgerStartedTask: LedgerStartedTask{LedgerTask: LedgerTask{Project: p}}}
	}
	started := []ledgerStartedRow{row(&b), row(&a), row(&b), row(&c), row(nil)}
	latest := map[string]statusLatest{
		a: {Health: HealthOnTrack, At: now.Add(-StatusUpdateStaleAfter)},
		b: {Health: HealthAtRisk, At: now.Add(-StatusUpdateStaleAfter - time.Second)},
	}
	got := ledgerProjects(started, latest, now)
	if len(got) != 3 || got[0].Project != a || got[1].Project != b || got[2].Project != c {
		t.Fatalf("projects %+v", got)
	}
	if got[0].Started != 1 || got[0].Stale || got[0].Health != HealthOnTrack {
		t.Fatalf("alpha at exactly 7 days %+v", got[0])
	}
	if got[1].Started != 2 || !got[1].Stale || got[1].Health != HealthAtRisk {
		t.Fatalf("beta past 7 days %+v", got[1])
	}
	if got[2].LastUpdateAt != nil || !got[2].Stale {
		t.Fatalf("gamma with no update %+v", got[2])
	}
}

func TestStatusUpdateStaleAtFixedTimes(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		now   time.Time
		stale bool
	}{
		{at, false},
		{at.Add(StatusUpdateStaleAfter), false},
		{at.Add(StatusUpdateStaleAfter + time.Second), true},
	} {
		if got := StatusUpdateStale(&at, tc.now); got != tc.stale {
			t.Errorf("now=%s stale=%t", tc.now, got)
		}
	}
	if !StatusUpdateStale(nil, at) {
		t.Error("a project with no update is not stale")
	}
}

func TestStatusDiff(t *testing.T) {
	snap := StatusCounts{Triage: 1, Backlog: 2, Started: 3}
	if d := statusDiff(nil, HealthOnTrack, snap); d.PreviousID != nil || d.Counts != nil || d.Health != nil {
		t.Fatalf("first diff %+v", d)
	}
	prev := &StatusUpdate{ID: 7, Health: HealthOnTrack, Snapshot: StatusCounts{Backlog: 3, Started: 1, Completed: 2}}
	d := statusDiff(prev, HealthAtRisk, snap)
	want := StatusCounts{Triage: 1, Backlog: -1, Started: 2, Completed: -2}
	if d.PreviousID == nil || *d.PreviousID != 7 || d.Counts == nil || *d.Counts != want || d.Health == nil || *d.Health != (StatusHealthChange{From: HealthOnTrack, To: HealthAtRisk}) {
		t.Fatalf("diff %+v counts=%+v health=%+v", d, d.Counts, d.Health)
	}
	if d := statusDiff(prev, HealthOnTrack, prev.Snapshot); d.Health != nil || *d.Counts != (StatusCounts{}) {
		t.Fatalf("unchanged diff %+v", d)
	}
}

func TestValidateStatusUpdate(t *testing.T) {
	if err := validateStatusUpdate("experiment", HealthOffTrack, "slipping", "tester"); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][4]string{
		"health":       {"experiment", "green", "body", "tester"},
		"empty health": {"experiment", "", "body", "tester"},
		"body":         {"experiment", HealthAtRisk, "  ", "tester"},
		"project":      {"bad name", HealthAtRisk, "body", "tester"},
		"author":       {"experiment", HealthAtRisk, "body", ""},
		"nul":          {"experiment", HealthAtRisk, "a\x00b", "tester"},
	} {
		if err := validateStatusUpdate(args[0], args[1], args[2], args[3]); !errors.Is(err, ErrStatusUpdateInvalid) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
}

// TestLedgerSeedOneTaskPerCase seeds one task per case and checks each lands
// in exactly the right section, and that two reads at one now are
// byte-identical.
func TestLedgerSeedOneTaskPerCase(t *testing.T) {
	s := ledgerStore(t)
	ctx := t.Context()
	s.SetStartedStalledHours(24)
	pool := s.pool

	fresh := flowTask(t, s, pool, "implement", StateDeveloping, true)
	stalled := flowTask(t, s, pool, "implement", StateDeveloping, true)
	ageTask(t, s, stalled.ID, time.Now().UTC().Add(-72*time.Hour))
	flagged := flowTask(t, s, pool, "implement", StateReviewing, true)
	if _, err := s.TransitionTask(ctx, flagged.ID, "needs_decision", "asker", "ship it?", nil, ""); err != nil {
		t.Fatal(err)
	}
	triage := flowTask(t, s, pool, "implement", StateTriage, false)
	settled := flowTask(t, s, pool, "implement", StateMerged, true)
	canceled := flowTask(t, s, pool, "implement", StateDropped, true)
	if res := send(t, s, mergeEvent(canceled.ID, testPR(4101))); res.Applied {
		t.Fatalf("merge on a dropped task applied: %+v", res)
	}
	oldCanceled := flowTask(t, s, pool, "implement", StateDropped, true)
	old := time.Now().UTC().Add(-LedgerEventWindow - time.Hour)
	ev := mergeEvent(oldCanceled.ID, testPR(4102))
	ev.At = &old
	send(t, s, ev)

	now := time.Now().UTC()
	l, err := s.Ledger(ctx, "desk-ledger-test", now)
	if err != nil {
		t.Fatal(err)
	}
	if got := ledgerIDs(l.InFlight.Tasks); !sameIDs(got, []int64{fresh.ID}) {
		t.Fatalf("in flight %v, want [%d]", got, fresh.ID)
	}
	if x := l.InFlight.Tasks[0]; x.NextTrigger != "review" || x.State != StateDeveloping {
		t.Fatalf("in flight row %+v", x)
	}
	if got := ledgerIDs(l.Stalled.Tasks); !sameIDs(got, []int64{stalled.ID}) {
		t.Fatalf("stalled %v, want [%d]", got, stalled.ID)
	}
	if len(l.Stalled.Events) != 1 || l.Stalled.Events[0].Task.ID != canceled.ID || l.Stalled.Events[0].SourceRef != testPR(4101) {
		t.Fatalf("stalled events %+v, want the recent merge on %d only", l.Stalled.Events, canceled.ID)
	}
	w := l.WaitingOnOperator
	if w.Count != 1 || len(w.Groups) != 1 || w.Groups[0].TaskID == nil || *w.Groups[0].TaskID != flagged.ID || w.Groups[0].Task == nil || w.Groups[0].Requests[0].Question != "ship it?" {
		t.Fatalf("waiting %+v", w)
	}
	if l.Triage.Count != 1 || len(l.Triage.Tasks) != 1 || l.Triage.Tasks[0].ID != triage.ID || l.Triage.OldestAt == nil {
		t.Fatalf("triage %+v", l.Triage)
	}
	where := sectionsOf(l.InFlight, l.WaitingOnOperator, l.Stalled)
	for id, sections := range where {
		if len(sections) != 1 {
			t.Errorf("task %d in %v", id, sections)
		}
	}
	for _, id := range []int64{settled.ID, oldCanceled.ID} {
		if len(where[id]) != 0 {
			t.Errorf("task %d listed in %v", id, where[id])
		}
	}
	if len(l.InFlight.Projects) != 1 || l.InFlight.Projects[0].Project != testProjectName || l.InFlight.Projects[0].Started != 3 || !l.InFlight.Projects[0].Stale {
		t.Fatalf("projects %+v", l.InFlight.Projects)
	}

	again, err := s.Ledger(ctx, "desk-ledger-test", now)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(l)
	b, _ := json.Marshal(again)
	if !bytes.Equal(a, b) {
		t.Fatalf("two reads differ:\n%s\n%s", a, b)
	}

	if _, err := s.CreateStatusUpdate(ctx, testProjectName, HealthOnTrack, "moving", "tester", now); err != nil {
		t.Fatal(err)
	}
	l, err = s.Ledger(ctx, "desk-ledger-test", now)
	if err != nil {
		t.Fatal(err)
	}
	if p := l.InFlight.Projects[0]; p.Stale || p.Health != HealthOnTrack || p.LastUpdateAt == nil {
		t.Fatalf("project after update %+v", p)
	}
}

func cursorOf(t *testing.T, s *Store, consumer string) (int64, bool) {
	t.Helper()
	var id int64
	err := s.pool.QueryRow(t.Context(), `SELECT acked_sync_id FROM ledger_cursors WHERE consumer=$1`, consumer).Scan(&id)
	if err != nil {
		return 0, false
	}
	return id, true
}

func maxSyncID(t *testing.T, s *Store) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(t.Context(), `SELECT max_sync_id FROM sync_state WHERE singleton`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLedgerCursor(t *testing.T) {
	s := ledgerStore(t)
	ctx := t.Context()
	const consumer = "desk-cursor-test"
	flowTask(t, s, s.pool, "implement", StateBacklog, false)

	// Before any ack: no cursor, empty delta.
	l, err := s.Ledger(ctx, consumer, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	ch := l.ChangedSinceAck
	if !ch.NoCursor || ch.Cursor != nil || len(ch.Rows) != 0 || len(ch.Summary) != 0 || ch.MaxSyncID != maxSyncID(t, s) || ch.MaxSyncID < 1 {
		t.Fatalf("no-cursor section %+v", ch)
	}
	if _, found := cursorOf(t, s, consumer); found {
		t.Fatal("a read created a cursor")
	}

	// The first ack of an unknown consumer starts at the current watermark.
	start := maxSyncID(t, s)
	c, err := s.AckLedger(ctx, consumer, 0, "tester", time.Now().UTC())
	if err != nil || c.AckedSyncID != start || c.Consumer != consumer || c.UpdatedBy != "tester" {
		t.Fatalf("first ack %+v err=%v, want cursor %d", c, err, start)
	}

	// Reads do not move it, and the delta has only later rows.
	x := flowTask(t, s, s.pool, "implement", StateBacklog, false)
	for i := 0; i < 2; i++ {
		l, err = s.Ledger(ctx, consumer, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := cursorOf(t, s, consumer); got != start {
		t.Fatalf("cursor moved on read: %d, want %d", got, start)
	}
	ch = l.ChangedSinceAck
	if ch.NoCursor || ch.Cursor == nil || ch.Cursor.AckedSyncID != start || len(ch.Rows) == 0 || ch.Total != int64(len(ch.Rows)) || ch.Truncated {
		t.Fatalf("delta %+v", ch)
	}
	sawTask := false
	for _, r := range ch.Rows {
		if r.SyncID <= start {
			t.Fatalf("row %+v at or before the cursor %d", r, start)
		}
		sawTask = sawTask || (r.Model == "task" && r.ModelID == itoa(x.ID))
	}
	if !sawTask {
		t.Fatalf("delta %+v lacks task %d", ch.Rows, x.ID)
	}
	var summed int64
	for _, sc := range ch.Summary {
		summed += sc.Count
	}
	if summed != ch.Total || ch.LastSyncID != ch.Rows[len(ch.Rows)-1].SyncID {
		t.Fatalf("summary %+v total %d last %d", ch.Summary, ch.Total, ch.LastSyncID)
	}

	// Ack forward moves it; the next delta starts after it.
	mid := ch.LastSyncID
	if c, err = s.AckLedger(ctx, consumer, mid, "tester", time.Now().UTC()); err != nil || c.AckedSyncID != mid {
		t.Fatalf("forward ack %+v err=%v", c, err)
	}
	y := flowTask(t, s, s.pool, "implement", StateBacklog, false)
	l, err = s.Ledger(ctx, consumer, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	sawY := false
	for _, r := range l.ChangedSinceAck.Rows {
		if r.SyncID <= mid {
			t.Fatalf("row %+v at or before the acked %d", r, mid)
		}
		if r.Model == "task" && r.ModelID == itoa(x.ID) {
			t.Fatalf("acked change of task %d listed again", x.ID)
		}
		sawY = sawY || (r.Model == "task" && r.ModelID == itoa(y.ID))
	}
	if !sawY {
		t.Fatalf("delta after ack lacks task %d: %+v", y.ID, l.ChangedSinceAck.Rows)
	}

	// Backward is refused and changes nothing; the same id is accepted.
	if _, err = s.AckLedger(ctx, consumer, mid-1, "tester", time.Now().UTC()); !errors.Is(err, ErrLedgerAckBackward) {
		t.Fatalf("backward ack err=%v", err)
	}
	if got, _ := cursorOf(t, s, consumer); got != mid {
		t.Fatalf("cursor %d after a refused ack, want %d", got, mid)
	}
	if _, err = s.AckLedger(ctx, consumer, mid, "tester", time.Now().UTC()); err != nil {
		t.Fatalf("same-id ack err=%v", err)
	}
	if _, err = s.AckLedger(ctx, consumer, maxSyncID(t, s)+1, "tester", time.Now().UTC()); !errors.Is(err, ErrInvalidSync) {
		t.Fatalf("ack past the watermark err=%v", err)
	}

	// A cursor older than the reset boundary answers like the feed: reset
	// with the oldest retained id.
	top := maxSyncID(t, s)
	if _, err = s.pool.Exec(ctx, `DELETE FROM sync_log WHERE sync_id<=$1`, mid+1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE sync_state SET pruned_through=$1 WHERE singleton`, mid+1); err != nil {
		t.Fatal(err)
	}
	var wantMin int64
	if err = s.pool.QueryRow(ctx, `SELECT COALESCE(min(sync_id),$1) FROM sync_log`, top+1).Scan(&wantMin); err != nil {
		t.Fatal(err)
	}
	_, err = s.Ledger(ctx, consumer, time.Now().UTC())
	var reset *SyncReset
	if !errors.As(err, &reset) || reset.MinSyncID != wantMin {
		t.Fatalf("stale cursor err=%v, want reset with min_sync_id %d", err, wantMin)
	}
	// The feed itself agrees.
	if _, _, err = s.SyncDelta(ctx, mid, 10); !errors.As(err, &reset) || reset.MinSyncID != wantMin {
		t.Fatalf("feed err=%v", err)
	}
	// An ack below the boundary is a reset too; an ack at the watermark
	// recovers.
	if _, err = s.AckLedger(ctx, consumer, mid, "tester", time.Now().UTC()); !errors.As(err, &reset) {
		t.Fatalf("ack below the boundary err=%v", err)
	}
	if c, err = s.AckLedger(ctx, consumer, top, "tester", time.Now().UTC()); err != nil || c.AckedSyncID != top {
		t.Fatalf("recovery ack %+v err=%v", c, err)
	}
	if l, err = s.Ledger(ctx, consumer, time.Now().UTC()); err != nil || len(l.ChangedSinceAck.Rows) != 0 || l.ChangedSinceAck.LastSyncID != top {
		t.Fatalf("after recovery %+v err=%v", l.ChangedSinceAck, err)
	}

	if _, err = s.Ledger(ctx, "bad consumer", time.Now().UTC()); err == nil {
		t.Fatal("invalid consumer accepted")
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func TestStatusUpdates(t *testing.T) {
	s := ledgerStore(t)
	ctx := t.Context()
	const project = "brewdial"
	seed := func(state string) {
		x, err := s.CreateTask(ctx, Task{Lane: relaneLane(t, "status"), Title: "status " + state, Kind: "implement", CreatedBy: "tester", Project: projectPtr(project)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(ctx, `UPDATE tasks SET state=$2 WHERE id=$1`, x.ID, state); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []string{StateBacklog, StateBacklog, StateDeveloping, StateMerged, StateIcebox} {
		seed(state)
	}
	// A task of another project does not count.
	flowTask(t, s, s.pool, "implement", StateDeveloping, true)

	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.CreateStatusUpdate(ctx, project, "green", "body", "tester", t0); !errors.Is(err, ErrStatusUpdateInvalid) {
		t.Fatalf("invalid health err=%v", err)
	}
	if _, err := s.CreateStatusUpdate(ctx, "no-such-project", HealthOnTrack, "body", "tester", t0); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("unknown project err=%v", err)
	}
	if list, err := s.ListStatusUpdates(ctx, project, 0, t0); err != nil || len(list.Updates) != 0 || !list.Stale || list.LastUpdateAt != nil {
		t.Fatalf("empty list %+v err=%v", list, err)
	}

	first, err := s.CreateStatusUpdate(ctx, project, HealthOnTrack, "on plan", "tester", t0)
	if err != nil {
		t.Fatal(err)
	}
	if want := (StatusCounts{Backlog: 2, Started: 1, Completed: 1, Canceled: 1}); first.Snapshot != want {
		t.Fatalf("snapshot %+v, want %+v", first.Snapshot, want)
	}
	if first.Diff.PreviousID != nil || first.Diff.Counts != nil || first.Diff.Health != nil || first.CreatedBy != "tester" || !first.CreatedAt.Equal(t0) {
		t.Fatalf("first update %+v", first)
	}

	seed(StateTriage)
	seed(StateDeveloping)
	t1 := t0.Add(time.Hour)
	second, err := s.CreateStatusUpdate(ctx, project, HealthAtRisk, "slipping", "tester", t1)
	if err != nil {
		t.Fatal(err)
	}
	if second.Diff.PreviousID == nil || *second.Diff.PreviousID != first.ID || second.Diff.Counts == nil ||
		*second.Diff.Counts != (StatusCounts{Triage: 1, Started: 1}) || second.Diff.Health == nil ||
		*second.Diff.Health != (StatusHealthChange{From: HealthOnTrack, To: HealthAtRisk}) {
		t.Fatalf("second diff %+v", second.Diff)
	}

	for _, tc := range []struct {
		now   time.Time
		stale bool
	}{
		{t1, false},
		{t1.Add(StatusUpdateStaleAfter), false},
		{t1.Add(StatusUpdateStaleAfter + time.Second), true},
	} {
		list, err := s.ListStatusUpdates(ctx, project, 0, tc.now)
		if err != nil {
			t.Fatal(err)
		}
		if list.Stale != tc.stale || len(list.Updates) != 2 || list.Updates[0].ID != second.ID || list.Updates[1].ID != first.ID || list.LastUpdateAt == nil || !list.LastUpdateAt.Equal(t1) {
			t.Fatalf("list at %s: %+v", tc.now, list)
		}
	}
	if list, err := s.ListStatusUpdates(ctx, project, 1, t1); err != nil || len(list.Updates) != 1 || list.Updates[0].ID != second.ID {
		t.Fatalf("limited list %+v err=%v", list, err)
	}
	if _, err := s.ListStatusUpdates(ctx, "no-such-project", 0, t1); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("unknown project list err=%v", err)
	}
}
