package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// The daily metrics document (hk 5112 track B-3, design §6) counts, per day,
// the operator waits and their ages, the waits opened and cleared, the orders
// created and acknowledged with the time to acknowledgement, the decision
// answers by channel (console, dots, session, cli), and the per-lane wake
// check's notices, looks and time from notice to look (operator decision 418,
// hk 5201, design §10).
//
// The waits come from hk 5128: until it lands this worker writes what exists
// today and leaves the wait fields null.

// DriftMetrics is the body of metrics/waits/<YYYY-MM-DD>. Pointer fields are
// null until the data exists (hk 5128).
type DriftMetrics struct {
	Date string `json:"date"`
	// Drift holds each drift class's total count.
	Drift map[string]int `json:"drift"`
	// HygieneDryRun counts the hygiene dry-run's prospective closes and
	// findings.
	HygieneDryRun map[string]int `json:"hygiene_dry_run"`
	// OpenOperatorWaits is null until hk 5128 derives the waits.
	OpenOperatorWaits    *int           `json:"open_operator_waits"`
	OpenOperatorWaitP50H *float64       `json:"open_operator_wait_p50h"`
	OpenOperatorWaitP90H *float64       `json:"open_operator_wait_p90h"`
	OpenOperatorWaitMaxH *float64       `json:"open_operator_wait_maxh"`
	OperatorWaitsOpened  *int           `json:"operator_waits_opened"`
	OperatorWaitsCleared *int           `json:"operator_waits_cleared"`
	OrdersCreated        int            `json:"orders_created"`
	OrdersAcked          int            `json:"orders_acknowledged"`
	OrderAckP50S         *int64         `json:"order_ack_p50s"`
	OrderAckP90S         *int64         `json:"order_ack_p90s"`
	OrderAckMaxS         *int64         `json:"order_ack_maxs"`
	DecisionAnswers      map[string]int `json:"decision_answers"`
	// LaneCheckNotices is the lane_check notices queued on the day, LaneLooks
	// the looks recorded on it, and the three look fields the time from the
	// notice to its look (operator decision 418, hk 5201).
	LaneCheckNotices int    `json:"lane_check_notices"`
	LaneLooks        int    `json:"lane_looks"`
	LaneLookP50S     *int64 `json:"lane_look_p50s"`
	LaneLookP90S     *int64 `json:"lane_look_p90s"`
	LaneLookMaxS     *int64 `json:"lane_look_maxs"`
}

// driftMetricsDayRange returns the UTC instants of the KST calendar day's
// start and end.
func driftMetricsDayRange(day string) (time.Time, time.Time, error) {
	start, err := time.ParseInLocation("2006-01-02", day, driftKST)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return start.UTC(), start.Add(24 * time.Hour).UTC(), nil
}

// driftOrderStats counts the orders created on the KST day and the
// acknowledgement delays, in seconds, of the orders acknowledged that day.
func (s *Store) driftOrderStats(ctx context.Context, start, end time.Time) (created int, ackDelays []int64, err error) {
	q := s.q(`SELECT count(*) FROM task_orders WHERE created_at>=$1 AND created_at<$2`,
		`SELECT count(*) FROM task_orders WHERE created_at>=? AND created_at<?`)
	if err = s.db().QueryRow(ctx, q, start, end).Scan(&created); err != nil {
		return 0, nil, err
	}
	q = s.q(`SELECT created_at,acked_at FROM task_orders WHERE acked_at>=$1 AND acked_at<$2`,
		`SELECT created_at,acked_at FROM task_orders WHERE acked_at>=? AND acked_at<?`)
	rows, err := s.db().Query(ctx, q, start, end)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var createdAt, ackedAt time.Time
		if err = rows.Scan(&createdAt, &ackedAt); err != nil {
			return 0, nil, err
		}
		ackDelays = append(ackDelays, int64(ackedAt.Sub(createdAt)/time.Second))
	}
	if err = rows.Err(); err != nil {
		return 0, nil, err
	}
	return created, ackDelays, nil
}

// driftDecisionAnswers counts the decision resolutions committed on the KST
// day by channel. The §6 channels are always present, even at zero.
func (s *Store) driftDecisionAnswers(ctx context.Context, start, end time.Time) (map[string]int, error) {
	out := map[string]int{
		DecisionChannelConsole: 0, DecisionChannelDots: 0,
		DecisionChannelSession: 0, DecisionChannelCLI: 0,
	}
	q := s.q(`SELECT channel,count(*) FROM decision_resolutions WHERE committed_at>=$1 AND committed_at<$2 GROUP BY channel`,
		`SELECT channel,count(*) FROM decision_resolutions WHERE committed_at>=? AND committed_at<? GROUP BY channel`)
	rows, err := s.db().Query(ctx, q, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var channel string
		var n int
		if err = rows.Scan(&channel, &n); err != nil {
			return nil, err
		}
		out[channel] += n
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// driftLaneCheckStats counts the lane_check notices queued on the day and the
// looks recorded on it, with the delay from each notice to its look. A look
// whose notice row is missing still counts, but contributes no delay.
func (s *Store) driftLaneCheckStats(ctx context.Context, start, end time.Time) (notices int, looks int, delays []int64, err error) {
	q := s.q(`SELECT count(*) FROM notification_outbox WHERE kind='lane_check' AND created_at>=$1 AND created_at<$2`,
		`SELECT count(*) FROM notification_outbox WHERE kind='lane_check' AND created_at>=? AND created_at<?`)
	if err = s.db().QueryRow(ctx, q, start, end).Scan(&notices); err != nil {
		return 0, 0, nil, err
	}
	// The clearing row the check writes for an empty lane is the store's
	// record, not a lane's look, so it is not counted as one.
	q = s.q(`SELECT count(*) FROM lane_looks WHERE created_at>=$1 AND created_at<$2 AND actor<>$3`,
		`SELECT count(*) FROM lane_looks WHERE created_at>=? AND created_at<? AND actor<>?`)
	if err = s.db().QueryRow(ctx, q, start, end, laneLookStoreActor).Scan(&looks); err != nil {
		return 0, 0, nil, err
	}
	q = s.q(`SELECT l.created_at,n.created_at FROM lane_looks l JOIN notification_outbox n ON n.event_id=l.notice_event_id
		WHERE l.created_at>=$1 AND l.created_at<$2 AND l.actor<>$3 AND n.kind='lane_check'`,
		`SELECT l.created_at,n.created_at FROM lane_looks l JOIN notification_outbox n ON n.event_id=l.notice_event_id
		WHERE l.created_at>=? AND l.created_at<? AND l.actor<>? AND n.kind='lane_check'`)
	rows, err := s.db().Query(ctx, q, start, end, laneLookStoreActor)
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var lookAt, noticeAt time.Time
		if err = rows.Scan(&lookAt, &noticeAt); err != nil {
			return 0, 0, nil, err
		}
		if d := int64(lookAt.Sub(noticeAt) / time.Second); d >= 0 {
			delays = append(delays, d)
		}
	}
	if err = rows.Err(); err != nil {
		return 0, 0, nil, err
	}
	return notices, looks, delays, nil
}

// percentile picks the nearest-rank value at p (0..1) of sorted values, or
// nil when empty.
func percentile(sorted []int64, p float64) *int64 {
	if len(sorted) == 0 {
		return nil
	}
	i := int(float64(len(sorted))*p + 0.5)
	if i < 1 {
		i = 1
	}
	if i > len(sorted) {
		i = len(sorted)
	}
	v := sorted[i-1]
	return &v
}

// BuildDriftMetrics reads what the metrics document records for the KST day:
// the drift and hygiene counts it is given, the order stats and decision
// answers by channel. Operator-wait fields stay null until hk 5128.
func (s *Store) BuildDriftMetrics(ctx context.Context, day string, drift TaskDrift, hygiene TaskHygieneReport) (DriftMetrics, error) {
	start, end, err := driftMetricsDayRange(day)
	if err != nil {
		return DriftMetrics{}, err
	}
	created, ackDelays, err := s.driftOrderStats(ctx, start, end)
	if err != nil {
		return DriftMetrics{}, fmt.Errorf("drift metrics orders: %w", err)
	}
	answers, err := s.driftDecisionAnswers(ctx, start, end)
	if err != nil {
		return DriftMetrics{}, fmt.Errorf("drift metrics answers: %w", err)
	}
	laneNotices, laneLooks, lookDelays, err := s.driftLaneCheckStats(ctx, start, end)
	if err != nil {
		return DriftMetrics{}, fmt.Errorf("drift metrics lane check: %w", err)
	}
	sort.Slice(ackDelays, func(i, j int) bool { return ackDelays[i] < ackDelays[j] })
	sort.Slice(lookDelays, func(i, j int) bool { return lookDelays[i] < lookDelays[j] })
	return DriftMetrics{
		Date: day,
		Drift: map[string]int{
			"settled_open_decision": drift.SettledOpenDecision.Count,
			"merged_pr_not_merged":  drift.MergedPRNotMerged.Count,
			"auto_close_held":       drift.AutoCloseHeld.Count,
			"merged_without_deploy": drift.MergedWithoutDeploy.Count,
			"idle_started":          drift.IdleStarted.Count,
			"explicit_all_settled":  drift.ExplicitAllSettled.Count,
		},
		HygieneDryRun: map[string]int{
			"mechanical_closes":           len(hygiene.MechanicalCloses),
			"merged_pr_open_tasks":        len(hygiene.Findings.MergedPROpenTasks),
			"auto_close_held_parents":     len(hygiene.Findings.AutoCloseHeldParents),
			"settled_open_decisions":      len(hygiene.Findings.SettledOpenDecisions),
			"live_attempts_without_lease": len(hygiene.Findings.LiveAttemptsWithoutLease),
			"answered_not_delivered":      len(hygiene.Findings.AnsweredDecisionsNotDelivered),
			"merged_without_deploy":       len(hygiene.Findings.MergedWithoutDeploy),
		},
		OrdersCreated:    created,
		OrdersAcked:      len(ackDelays),
		OrderAckP50S:     percentile(ackDelays, 0.5),
		OrderAckP90S:     percentile(ackDelays, 0.9),
		OrderAckMaxS:     percentile(ackDelays, 1),
		DecisionAnswers:  answers,
		LaneCheckNotices: laneNotices,
		LaneLooks:        laneLooks,
		LaneLookP50S:     percentile(lookDelays, 0.5),
		LaneLookP90S:     percentile(lookDelays, 0.9),
		LaneLookMaxS:     percentile(lookDelays, 1),
	}, nil
}

// WriteDriftMetrics records the metrics/waits/<day> document. The key is the
// run's marker: a day whose document exists never runs again.
func (s *Store) WriteDriftMetrics(ctx context.Context, day string, m DriftMetrics) (Document, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return Document{}, err
	}
	doc, changed, err := s.PutDocument(ctx, Document{
		Key: DriftMetricsDocKey(day), Kind: "report", Body: string(body), CreatedBy: "drift-worker",
	})
	if err != nil {
		return Document{}, err
	}
	_ = changed
	return doc, nil
}

// RunDriftDelivery runs one daily delivery for the KST day of now: the drift
// report (merged_days 2) and the hygiene dry run, one task_drift outbox row
// per lane with rows plus the operator-desk summary, and the metrics
// document. A day whose metrics document exists is already delivered: it
// reports ran=false and writes nothing, so a redeploy neither skips nor
// repeats a day. It never changes a task.
func (s *Store) RunDriftDelivery(ctx context.Context, now time.Time) (bool, error) {
	now = now.UTC()
	day := DriftDayOf(now)
	var ran bool
	err := s.retryTx(ctx, func() error {
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		if err = s.lockWorkerOperationTx(ctx, tx, "drift_delivery"); err != nil {
			return err
		}
		var found bool
		err = tx.QueryRow(ctx, s.q(`SELECT EXISTS(SELECT 1 FROM documents WHERE key=$1)`, "SELECT EXISTS(SELECT 1 FROM documents WHERE `key`=?)"), DriftMetricsDocKey(day)).Scan(&found)
		if err != nil {
			return err
		}
		if found {
			return tx.Commit(ctx)
		}
		drift, err := s.TaskDrift(ctx, now, DriftDefaultMergedDays)
		if err != nil {
			return fmt.Errorf("drift delivery drift: %w", err)
		}
		hygiene, err := s.TaskHygiene(ctx, now, "", true)
		if err != nil {
			return fmt.Errorf("drift delivery hygiene: %w", err)
		}
		if _, err := s.EnqueueDriftNotices(ctx, now, drift, hygiene); err != nil {
			return fmt.Errorf("drift delivery notices: %w", err)
		}
		m, err := s.BuildDriftMetrics(ctx, day, drift, hygiene)
		if err != nil {
			return err
		}
		if _, err := s.WriteDriftMetrics(ctx, day, m); err != nil {
			return fmt.Errorf("drift delivery metrics: %w", err)
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		ran = true
		return nil
	})
	return ran, err
}
