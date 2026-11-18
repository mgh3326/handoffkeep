package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The daily drift and hygiene delivery (hk 5112 track B-3) notifies each lane
// with drift rows once a day and records the metrics/waits/<date> document.
// It never changes a task: TaskDrift only reads, and TaskHygiene runs as a
// dry run.

// driftKST is the delivery day's clock: the worker runs once on the first
// tick at or after 09:00 KST.
var driftKST = time.FixedZone("KST", 9*3600)

// DriftRunHourKST is the KST hour the daily delivery runs at or after.
const DriftRunHourKST = 9

// DriftDayOf returns the KST calendar day of now as YYYY-MM-DD. The outbox
// event id drift-<lane>-<date> and the metrics doc key carry this day, so a
// redeploy neither skips nor repeats a date.
func DriftDayOf(now time.Time) string {
	return now.In(driftKST).Format("2006-01-02")
}

// DriftDue reports whether now is at or after 09:00 on its KST day.
func DriftDue(now time.Time) bool {
	return now.In(driftKST).Hour() >= DriftRunHourKST
}

// DriftNoticeEventID is the idempotent outbox id of one lane's delivery for
// one day.
func DriftNoticeEventID(lane, day string) string {
	return "drift-" + lane + "-" + day
}

// DriftSummaryEventID is the idempotent outbox id of the operator-desk
// summary for one day. It carries a -summary suffix (the asker-notice
// precedent, decisionAskerAnswerNoticeEventID) so it never equals a lane
// notice id: lane operator-desk's own notice is drift-operator-desk-<day>.
func DriftSummaryEventID(day string) string {
	return "drift-operator-desk-" + day + "-summary"
}

// DriftMetricsDocKey is the metrics document of one day.
func DriftMetricsDocKey(day string) string {
	return "metrics/waits/" + day
}

// driftDeliveryClasses are the drift classes a lane notice lists row by row,
// in notice order. merged_without_deploy is a count only (200+ rows), and
// explicit_all_settled lists explicit tasks as reminders for their owner: the
// mechanical classes already skip explicit tasks (hk 5112 track B-2).
var driftDeliveryClasses = []struct {
	name  string
	class func(*TaskDrift) *TaskDriftClass
}{
	{"settled_open_decision", func(d *TaskDrift) *TaskDriftClass { return &d.SettledOpenDecision }},
	{"merged_pr_not_merged", func(d *TaskDrift) *TaskDriftClass { return &d.MergedPRNotMerged }},
	{"auto_close_held", func(d *TaskDrift) *TaskDriftClass { return &d.AutoCloseHeld }},
	{"idle_started", func(d *TaskDrift) *TaskDriftClass { return &d.IdleStarted }},
	{"explicit_all_settled", func(d *TaskDrift) *TaskDriftClass { return &d.ExplicitAllSettled }},
}

// DriftDeliveryRow is one listed drift row: its class and the task.
type DriftDeliveryRow struct {
	Class string        `json:"class"`
	Task  TaskDriftTask `json:"task"`
}

// DriftLaneDelivery is one lane's share of a drift report: the rows that name
// it, and its merged_without_deploy count (a count only, never listed).
type DriftLaneDelivery struct {
	Lane                string             `json:"lane"`
	Rows                []DriftDeliveryRow `json:"rows"`
	MergedWithoutDeploy int                `json:"merged_without_deploy"`
}

// GroupDriftForDelivery groups a drift report by owning lane, in lane order.
// Rows keep class order, then task id. A lane with no named rows but a
// merged_without_deploy share still gets a delivery (the count).
func GroupDriftForDelivery(drift TaskDrift) []DriftLaneDelivery {
	byLane := map[string]*DriftLaneDelivery{}
	at := func(lane string) *DriftLaneDelivery {
		d, ok := byLane[lane]
		if !ok {
			d = &DriftLaneDelivery{Lane: lane}
			byLane[lane] = d
		}
		return d
	}
	for _, c := range driftDeliveryClasses {
		for _, x := range c.class(&drift).Tasks {
			d := at(x.Lane)
			d.Rows = append(d.Rows, DriftDeliveryRow{Class: c.name, Task: x})
		}
	}
	for _, x := range drift.MergedWithoutDeploy.Tasks {
		at(x.Lane).MergedWithoutDeploy++
	}
	// Truncated classes name more candidates than listed: the unlisted rows'
	// per-lane split is unknown, so the count covers only listed rows.
	out := make([]DriftLaneDelivery, 0, len(byLane))
	for _, d := range byLane {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Lane < out[j].Lane })
	return out
}

// DriftLaneNoticeText renders one lane's delivery: one line per row with
// class, task id, title and since, and the merged_without_deploy count.
func DriftLaneNoticeText(day string, d DriftLaneDelivery) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[hk drift %s] %s: %d rows", day, d.Lane, len(d.Rows))
	if d.MergedWithoutDeploy > 0 {
		fmt.Fprintf(&b, " (+%d merged_without_deploy, see console)", d.MergedWithoutDeploy)
	}
	for _, r := range d.Rows {
		fmt.Fprintf(&b, "\n%s #%d %s %s since=%s",
			r.Class, r.Task.ID, r.Task.Kind, r.Task.Title, r.Task.Since.Format(time.RFC3339))
		if r.Task.PR != "" {
			fmt.Fprintf(&b, " pr=%s", r.Task.PR)
		}
	}
	return b.String()
}

// DriftSummaryText renders the operator-desk summary: counts per class and
// lane, plus the hygiene dry-run totals.
func DriftSummaryText(day string, ds []DriftLaneDelivery, drift TaskDrift, hygiene TaskHygieneReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[hk drift %s] summary: %d lanes", day, len(ds))
	lanes := func(counts map[string]int) string {
		names := make([]string, 0, len(counts))
		for lane, n := range counts {
			if n > 0 {
				names = append(names, fmt.Sprintf("%s:%d", lane, n))
			}
		}
		sort.Strings(names)
		if len(names) == 0 {
			return ""
		}
		return " (" + strings.Join(names, " ") + ")"
	}
	class := func(name string, c TaskDriftClass) {
		per := map[string]int{}
		for _, d := range ds {
			switch name {
			case "merged_without_deploy":
				per[d.Lane] += d.MergedWithoutDeploy
			default:
				for _, r := range d.Rows {
					if r.Class == name {
						per[d.Lane]++
					}
				}
			}
		}
		fmt.Fprintf(&b, "\n%s: %d%s", name, c.Count, lanes(per))
		if c.Truncated {
			fmt.Fprintf(&b, " (truncated)")
		}
	}
	class("settled_open_decision", drift.SettledOpenDecision)
	class("merged_pr_not_merged", drift.MergedPRNotMerged)
	class("auto_close_held", drift.AutoCloseHeld)
	class("merged_without_deploy", drift.MergedWithoutDeploy)
	class("idle_started", drift.IdleStarted)
	class("explicit_all_settled", drift.ExplicitAllSettled)
	fmt.Fprintf(&b, "\nhygiene dry-run: %d prospective closes; findings merged_pr=%d auto_close=%d settled=%d live_lease=%d lagging_decisions=%d merged_without_deploy=%d",
		len(hygiene.MechanicalCloses),
		len(hygiene.Findings.MergedPROpenTasks), len(hygiene.Findings.AutoCloseHeldParents),
		len(hygiene.Findings.SettledOpenDecisions), len(hygiene.Findings.LiveAttemptsWithoutLease),
		len(hygiene.Findings.AnsweredDecisionsNotDelivered), len(hygiene.Findings.MergedWithoutDeploy))
	return b.String()
}

// EnqueueDriftNotices writes one task_drift outbox row per lane with rows and
// one operator-desk summary row, all for the KST day of now. The summary goes
// out every day, even when no lane has rows, so the day's delivery is
// visible. Event ids are deterministic per lane and day, so a repeated run
// writes nothing new. It returns how many rows this call wrote.
func (s *Store) EnqueueDriftNotices(ctx context.Context, now time.Time, drift TaskDrift, hygiene TaskHygieneReport) (int, error) {
	var wrote int
	err := s.retryTx(ctx, func() error {
		var err error
		wrote, err = s.enqueueDriftNoticesOnce(ctx, now, drift, hygiene)
		return err
	})
	return wrote, err
}

// enqueueDriftNoticesOnce is one whole EnqueueDriftNotices transaction (retried
// on MySQL after a deadlock or lock-wait timeout). Its writes are outbox rows
// keyed by deterministic event ids and no send happens inside it, so a retry
// that repeats them writes nothing new.
func (s *Store) enqueueDriftNoticesOnce(ctx context.Context, now time.Time, drift TaskDrift, hygiene TaskHygieneReport) (int, error) {
	now = now.UTC()
	day := DriftDayOf(now)
	ds := GroupDriftForDelivery(drift)
	tx, release, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if s.dialect == mysqlDialect {
		if _, err = mysqlTransactionLock(ctx, tx, mysqlLockKey("text", "drift_notices"), false); err != nil {
			return 0, err
		}
	} else if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('drift_notices', 0))`); err != nil {
		return 0, err
	}
	wrote := 0
	for _, d := range ds {
		ok, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindTaskDrift, d.Lane, DriftNoticeEventID(d.Lane, day), DriftLaneNoticeText(day, d), now)
		if err != nil {
			return 0, err
		}
		if ok {
			wrote++
		}
	}
	ok, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindTaskDrift, "operator-desk", DriftSummaryEventID(day), DriftSummaryText(day, ds, drift, hygiene), now)
	if err != nil {
		return 0, err
	}
	if ok {
		wrote++
	}
	return wrote, tx.Commit(ctx)
}
