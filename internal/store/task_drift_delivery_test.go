package store

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// driftTask builds one TaskDriftTask for grouping tests.
func driftTask(id int64, lane, title string, since time.Time) TaskDriftTask {
	return TaskDriftTask{ID: id, Lane: lane, Title: title, Kind: "implement", State: "reviewing", Since: since}
}

// TestGroupDriftForDelivery is the per-lane grouping: rows keep class order
// under their lane, lanes sort, and merged_without_deploy is a count only.
func TestGroupDriftForDelivery(t *testing.T) {
	since := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	drift := TaskDrift{
		SettledOpenDecision: TaskDriftClass{Count: 1, Tasks: []TaskDriftTask{driftTask(1, "lane-b", "settled", since)}},
		MergedPRNotMerged:   TaskDriftClass{Count: 2, Tasks: []TaskDriftTask{driftTask(2, "lane-a", "merged", since), driftTask(3, "lane-a", "merged too", since)}},
		IdleStarted:         TaskDriftClass{Count: 1, Tasks: []TaskDriftTask{driftTask(4, "lane-a", "idle", since)}},
		ExplicitAllSettled:  TaskDriftClass{Count: 1, Tasks: []TaskDriftTask{driftTask(5, "lane-b", "by hand", since)}},
		MergedWithoutDeploy: TaskDriftClass{Count: 3, Tasks: []TaskDriftTask{
			driftTask(6, "lane-a", "old merge", since),
			driftTask(7, "lane-a", "old merge 2", since),
			driftTask(8, "lane-c", "old merge 3", since),
		}},
	}
	got := GroupDriftForDelivery(drift)
	if len(got) != 3 || got[0].Lane != "lane-a" || got[1].Lane != "lane-b" || got[2].Lane != "lane-c" {
		t.Fatalf("lanes = %+v", got)
	}
	a := got[0]
	if len(a.Rows) != 3 || a.Rows[0].Class != "merged_pr_not_merged" || a.Rows[2].Class != "idle_started" {
		t.Fatalf("lane-a rows = %+v", a.Rows)
	}
	if a.MergedWithoutDeploy != 2 || got[2].MergedWithoutDeploy != 1 || len(got[2].Rows) != 0 {
		t.Fatalf("merged_without_deploy counts = %+v", got)
	}
	b := got[1]
	if len(b.Rows) != 2 || b.Rows[0].Class != "settled_open_decision" || b.Rows[1].Class != "explicit_all_settled" {
		t.Fatalf("lane-b rows = %+v", b.Rows)
	}
	text := DriftLaneNoticeText("2026-11-18", a)
	for _, want := range []string{"merged_pr_not_merged #2", "idle_started #4", "merged_without_deploy", "since=2026-11-10"} {
		if !strings.Contains(text, want) {
			t.Errorf("lane text %q lacks %q", text, want)
		}
	}
	if strings.Contains(text, "old merge") {
		t.Errorf("lane text lists a merged_without_deploy row:\n%s", text)
	}
	summary := DriftSummaryText("2026-11-18", got, drift, TaskHygieneReport{})
	for _, want := range []string{"summary: 3 lanes", "merged_pr_not_merged: 2 (lane-a:2)", "merged_without_deploy: 3 (lane-a:2 lane-c:1)", "explicit_all_settled: 1 (lane-b:1)"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q lacks %q", summary, want)
		}
	}
}

// TestDriftDayKST pins the delivery day to KST: the run is due on the first
// tick at or after 09:00 KST, and the marker carries the KST date.
func TestDriftDayKST(t *testing.T) {
	kstAt := func(h, min int) time.Time {
		return time.Date(2026, 11, 18, h, min, 0, 0, time.FixedZone("KST", 9*3600)).UTC()
	}
	if got := DriftDayOf(kstAt(8, 59)); got != "2026-11-18" {
		t.Fatalf("day at 08:59 KST = %q", got)
	}
	if DriftDue(kstAt(8, 59)) {
		t.Fatal("due before 09:00 KST")
	}
	if !DriftDue(kstAt(9, 0)) {
		t.Fatal("not due at 09:00 KST")
	}
	if got := DriftDayOf(kstAt(0, 0)); got != "2026-11-18" {
		t.Fatalf("day at KST midnight = %q", got)
	}
	if DriftNoticeEventID("lane-a", "2026-11-18") != "drift-lane-a-2026-11-18" {
		t.Fatalf("event id = %q", DriftNoticeEventID("lane-a", "2026-11-18"))
	}
	if DriftMetricsDocKey("2026-11-18") != "metrics/waits/2026-11-18" {
		t.Fatalf("metrics key = %q", DriftMetricsDocKey("2026-11-18"))
	}
}

// TestDriftOutboxKindWideningKeepsKinds pins the expand-only widening: the new
// CHECK admits task_drift and every kind the previous release writes.
func TestDriftOutboxKindWideningKeepsKinds(t *testing.T) {
	prior := []string{"decision_answered", "chat_answer", "dots_report", "dots_decision", "dots_blocked", "dots_followup", "yt_write_op", "decision_denied", "disposition_answered", "deploy_result", "deploy_lag", "task_order"}
	pg := strings.Join(driftOutboxKindStatements, "\n")
	for _, kind := range append(prior, OutboxKindTaskDrift) {
		if !strings.Contains(pg, "'"+kind+"'") {
			t.Errorf("PostgreSQL widening lacks %q", kind)
		}
		if !strings.Contains(mysqlOutboxKindsV3, "'"+kind+"'") {
			t.Errorf("MySQL widening lacks %q", kind)
		}
	}
	if OutboxKindTaskDrift != "task_drift" {
		t.Fatalf("kind = %q", OutboxKindTaskDrift)
	}
}

// TestDriftEnqueueIsIdempotentPerLaneAndDay proves the marker: repeating the
// enqueue for one day writes nothing new.
func TestDriftEnqueueIsIdempotentPerLaneAndDay(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	since := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	drift := TaskDrift{
		MergedPRNotMerged: TaskDriftClass{Count: 1, Tasks: []TaskDriftTask{driftTask(41, "lane-a", "merged", since)}},
	}
	if n, err := s.EnqueueDriftNotices(ctx, now, drift, TaskHygieneReport{}); err != nil || n != 2 {
		t.Fatalf("enqueue = %d, %v; want 2 (lane + summary)", n, err)
	}
	if n, err := s.EnqueueDriftNotices(ctx, now.Add(5*time.Minute), drift, TaskHygieneReport{}); err != nil || n != 0 {
		t.Fatalf("repeat enqueue = %d, %v; want 0", n, err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE kind='task_drift'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("task_drift rows = %d, %v; want 2", count, err)
	}
	var event, text string
	if err := s.pool.QueryRow(ctx, `SELECT event_id,text FROM notification_outbox WHERE target_lane='lane-a'`).Scan(&event, &text); err != nil {
		t.Fatal(err)
	}
	if event != "drift-lane-a-2026-11-18" {
		t.Fatalf("event id = %q", event)
	}
	for _, want := range []string{"merged_pr_not_merged #41", "merged"} {
		if !strings.Contains(text, want) {
			t.Errorf("lane text %q lacks %q", text, want)
		}
	}
}

// TestDriftEnqueueWritesSummaryOnEmptyDays: with no lane rows the summary
// still goes out, so the day's delivery is visible.
func TestDriftEnqueueWritesSummaryOnEmptyDays(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	if n, err := s.EnqueueDriftNotices(ctx, now, TaskDrift{}, TaskHygieneReport{}); err != nil || n != 1 {
		t.Fatalf("empty enqueue = %d, %v; want 1 (summary)", n, err)
	}
	var event, lane string
	if err := s.pool.QueryRow(ctx, `SELECT event_id,target_lane FROM notification_outbox WHERE kind='task_drift'`).Scan(&event, &lane); err != nil {
		t.Fatal(err)
	}
	if event != "drift-operator-desk-2026-11-18" || lane != "operator-desk" {
		t.Fatalf("summary row = %q to %q", event, lane)
	}
}

// TestDriftDeliveryRunsOncePerDayAcrossRedeploys proves one day runs once: the
// metrics document is the marker, so a second run — a redeploy's first tick —
// writes nothing.
func TestDriftDeliveryRunsOncePerDayAcrossRedeploys(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := t.Context()
	at := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	ran, err := s.RunDriftDelivery(ctx, at)
	if err != nil || !ran {
		t.Fatalf("first run = %v, %v; want true", ran, err)
	}
	if _, found, err := s.GetDocument(ctx, DriftMetricsDocKey("2026-11-18")); err != nil || !found {
		t.Fatalf("metrics doc found=%v err=%v", found, err)
	}
	var outbox int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE kind='task_drift'`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	ran, err = s.RunDriftDelivery(ctx, at.Add(5*time.Minute))
	if err != nil || ran {
		t.Fatalf("second run = %v, %v; want false", ran, err)
	}
	var again int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE kind='task_drift'`).Scan(&again); err != nil || again != outbox {
		t.Fatalf("outbox %d -> %d, %v; a repeat must write nothing", outbox, again, err)
	}
	ran, err = s.RunDriftDelivery(ctx, at.Add(24*time.Hour))
	if err != nil || !ran {
		t.Fatalf("next day run = %v, %v; want true", ran, err)
	}
}

// TestDriftMetricsLeavesWaitsNull pins §6's shape until hk 5128: wait fields
// are null, the §6 channels are present, and no task moves.
func TestDriftMetricsLeavesWaitsNull(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := t.Context()
	m, err := s.BuildDriftMetrics(ctx, "2026-11-18", TaskDrift{}, TaskHygieneReport{})
	if err != nil {
		t.Fatal(err)
	}
	if m.OpenOperatorWaits != nil || m.OpenOperatorWaitP50H != nil || m.OperatorWaitsOpened != nil || m.OperatorWaitsCleared != nil {
		t.Fatalf("waits must be null until hk 5128: %+v", m)
	}
	for _, ch := range []string{DecisionChannelConsole, DecisionChannelDots, DecisionChannelSession, DecisionChannelCLI} {
		if _, ok := m.DecisionAnswers[ch]; !ok {
			t.Fatalf("decision answers lack channel %q: %v", ch, m.DecisionAnswers)
		}
	}
	if m.OrdersCreated != 0 || m.OrdersAcked != 0 || m.OrderAckP50S != nil {
		t.Fatalf("empty day orders = %+v", m)
	}
}

// TestDriftDeliverySkipsExplicitTasks pins the explicit-task skip (hk 5112
// track B-2) inside the delivery: an explicit task with a merged change is a
// reminder for its owner (explicit_all_settled), never a mechanical row, and
// hygiene proposes no close for it.
func TestDriftDeliverySkipsExplicitTasks(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	explicit := flowTask(t, s, pool, "implement", StateDeveloping, true)
	auto := flowTask(t, s, pool, "implement", StateDeveloping, true)
	if _, _, err := s.SetTaskCloseRule(ctx, explicit.ID, CloseRuleExplicit, "tester", "programme task"); err != nil {
		t.Fatal(err)
	}
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: testPR(5112301), TaskID: explicit.ID})
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: testPR(5112302), TaskID: auto.ID})
	if _, err := pool.Exec(ctx, `UPDATE changes SET state='merged' WHERE repo='example/app' AND pr_number IN (5112301,5112302)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	drift := driftAt(t, s, now, DriftDefaultMergedDays)
	if ids := driftIDs(drift.MergedPRNotMerged); ids[explicit.ID] || !ids[auto.ID] {
		t.Fatalf("merged_pr_not_merged ids %v, want the auto task only", ids)
	}
	if ids := driftIDs(drift.ExplicitAllSettled); !ids[explicit.ID] {
		t.Fatalf("explicit_all_settled ids %v, want the explicit task", ids)
	}
	rep, err := s.TaskHygiene(ctx, now, "", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.MechanicalCloses {
		if c.TaskID == explicit.ID {
			t.Fatalf("hygiene proposes a close for explicit task %d: %+v", explicit.ID, c)
		}
	}
	ds := GroupDriftForDelivery(drift)
	var lane *DriftLaneDelivery
	for i := range ds {
		for _, r := range ds[i].Rows {
			if r.Task.ID == explicit.ID {
				if r.Class != "explicit_all_settled" {
					t.Fatalf("explicit task listed as %q, want explicit_all_settled", r.Class)
				}
				lane = &ds[i]
			}
		}
	}
	if lane == nil {
		t.Fatalf("delivery has no reminder row for explicit task %d", explicit.ID)
	}
	text := DriftLaneNoticeText(DriftDayOf(now), *lane)
	want := "explicit_all_settled #" + strconv.FormatInt(explicit.ID, 10)
	if !strings.Contains(text, want) {
		t.Fatalf("lane text lacks %q:\n%s", want, text)
	}
	if strings.Contains(text, "merged_pr_not_merged #"+strconv.FormatInt(explicit.ID, 10)) {
		t.Fatalf("lane text lists the explicit task as mechanical:\n%s", text)
	}
}

// TestMetricsPrefixExcludedFromMirror pins the metrics/ exclusion from the
// YouTrack article mirror (hk 5112 track B-3, design §6): counted, not
// mirrored.
func TestMetricsPrefixExcludedFromMirror(t *testing.T) {
	if !YTArticleExcluded("metrics/waits/2026-11-18", 0) {
		t.Fatal("metrics/waits/2026-11-18 should be excluded")
	}
	if YTArticleExcluded("design/2026-11-18/hk5112-track-b", 0) {
		t.Fatal("design docs must still mirror")
	}
}
