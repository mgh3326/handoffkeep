package store

import (
	"context"
	"sort"
	"strconv"
	"time"
)

// The decision backfill is a dry run only. ListOpenDecisionSources reads every
// question each source currently counts as open, by that source's own rule,
// with the task and job it names; ClassifyOpenDecisions sorts them into
// classes for the operator. Nothing here writes: closing any item needs the
// operator's explicit OK, which is outside this code.

// OpenDecisionItem is one question a source counts as open. TaskState and
// JobState are empty when the item names no task or job, or names one hk has
// no record of.
type OpenDecisionItem struct {
	Source    string    `json:"source"`
	SourceRef string    `json:"source_ref"`
	Lane      string    `json:"lane"`
	Question  string    `json:"question"`
	CreatedAt time.Time `json:"created_at"`
	TaskID    *int64    `json:"task_id,omitempty"`
	TaskState string    `json:"task_state,omitempty"`
	JobID     string    `json:"job_id,omitempty"`
	JobState  string    `json:"job_state,omitempty"`
}

// OpenDecisionSources is the read the dry run classifies. Truncated names a
// source whose read hit its limit, so its count is a lower bound.
type OpenDecisionSources struct {
	ServerTime time.Time          `json:"server_time"`
	Items      []OpenDecisionItem `json:"items"`
	Truncated  []string           `json:"truncated"`
}

// openDecisionSourceLimit caps each source's read; the relay sources use
// their list functions' own cap, which is the same.
const openDecisionSourceLimit = 1000

const decisionQuestionPreview = 200

// ListOpenDecisionSources reads the open questions of every source. It runs
// SELECTs only.
func (s *Store) ListOpenDecisionSources(ctx context.Context) (OpenDecisionSources, error) {
	out := OpenDecisionSources{Items: []OpenDecisionItem{}, Truncated: []string{}}
	if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&out.ServerTime); err != nil {
		return out, err
	}
	add := func(source string, n int, items []OpenDecisionItem) {
		if n >= openDecisionSourceLimit {
			out.Truncated = append(out.Truncated, source)
		}
		out.Items = append(out.Items, items...)
	}

	// task_flag: the awaiting_decision flag without an open structured request
	// (ListOpenTaskDecisions), disposition items included
	// (ListOpenDispositions; they never carry a request). The ref is the
	// latest awaiting event, the one the flag's mirror row is keyed by.
	rows, err := s.pool.Query(ctx, `SELECT t.id,t.lane,t.state,COALESCE(ev.id,0),COALESCE(ev.at,t.updated_at),COALESCE(ev.note,'')
		FROM tasks t LEFT JOIN LATERAL (SELECT id,at,note FROM task_events WHERE task_id=t.id AND `+awaitingEventSQL+` ORDER BY at DESC,id DESC LIMIT 1) ev ON true
		WHERE t.awaiting_decision AND COALESCE(t.refs->'decision_request'->>'status','')<>'open'
		ORDER BY t.id LIMIT $1`, openDecisionSourceLimit)
	if err != nil {
		return out, err
	}
	items := []OpenDecisionItem{}
	for rows.Next() {
		var id, ev int64
		var x OpenDecisionItem
		if err := rows.Scan(&id, &x.Lane, &x.TaskState, &ev, &x.CreatedAt, &x.Question); err != nil {
			rows.Close()
			return out, err
		}
		x.Source, x.SourceRef, x.TaskID = DecisionSourceTaskFlag, strconv.FormatInt(id, 10)+"/"+strconv.FormatInt(ev, 10), &id
		items = append(items, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	add(DecisionSourceTaskFlag, len(items), items)

	// task_request: the task's current request is open, whatever the task
	// state (ListOpenDecisionRequests).
	rows, err = s.pool.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE refs->'decision_request'->>'status'='open' ORDER BY id LIMIT $1`, openDecisionSourceLimit)
	if err != nil {
		return out, err
	}
	items = []OpenDecisionItem{}
	for rows.Next() {
		var t Task
		if err := scanTask(rows, &t); err != nil {
			rows.Close()
			return out, err
		}
		if t.Refs.DecisionRequest == nil {
			continue
		}
		id := t.ID
		r := t.Refs.DecisionRequest
		items = append(items, OpenDecisionItem{Source: DecisionSourceTaskRequest, SourceRef: r.ID, Lane: t.Lane, Question: r.Question, CreatedAt: r.RequestedAt, TaskID: &id, TaskState: t.State})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	add(DecisionSourceTaskRequest, len(items), items)

	// job_escalate and lane_event: the decision inbox's own lists.
	escalations, err := s.ListOpenEscalations(ctx, openDecisionSourceLimit)
	if err != nil {
		return out, err
	}
	items = []OpenDecisionItem{}
	for _, e := range escalations {
		q := e.Question
		if q == "" {
			q = e.Reason
		}
		items = append(items, OpenDecisionItem{Source: DecisionSourceJobEscalate, SourceRef: strconv.FormatInt(e.ID, 10), Lane: e.OwnerLane, Question: q, CreatedAt: e.ReceivedAt, JobID: e.JobID})
	}
	add(DecisionSourceJobEscalate, len(escalations), items)
	lanes, err := s.ListOpenLaneDecisions(ctx, openDecisionSourceLimit)
	if err != nil {
		return out, err
	}
	items = []OpenDecisionItem{}
	for _, e := range lanes {
		items = append(items, OpenDecisionItem{Source: DecisionSourceLaneEvent, SourceRef: strconv.FormatInt(e.ID, 10), Lane: e.OwnerLane, Question: e.Text, CreatedAt: e.ReceivedAt, JobID: e.JobID})
	}
	add(DecisionSourceLaneEvent, len(lanes), items)

	// chat: pending questions.
	rows, err = s.pool.Query(ctx, `SELECT id,lane,body,created_at FROM chat_questions WHERE state='pending' ORDER BY id LIMIT $1`, openDecisionSourceLimit)
	if err != nil {
		return out, err
	}
	items = []OpenDecisionItem{}
	for rows.Next() {
		x := OpenDecisionItem{Source: DecisionSourceChat}
		if err := rows.Scan(&x.SourceRef, &x.Lane, &x.Question, &x.CreatedAt); err != nil {
			rows.Close()
			return out, err
		}
		items = append(items, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	add(DecisionSourceChat, len(items), items)

	// linear: an issue not superseded whose request is still the task's open
	// one (AnswerLinearDecision answers nothing else).
	rows, err = s.pool.Query(ctx, `SELECT l.request_id,l.created_at,t.id,t.lane,t.state,COALESCE(t.refs->'decision_request'->>'question','')
		FROM linear_decision_issues l JOIN tasks t ON t.id=l.task_id
		WHERE l.superseded_by='' AND t.refs->'decision_request'->>'id'=l.request_id AND t.refs->'decision_request'->>'status'='open'
		ORDER BY l.request_id LIMIT $1`, openDecisionSourceLimit)
	if err != nil {
		return out, err
	}
	items = []OpenDecisionItem{}
	for rows.Next() {
		var id int64
		x := OpenDecisionItem{Source: DecisionSourceLinear}
		if err := rows.Scan(&x.SourceRef, &x.CreatedAt, &id, &x.Lane, &x.TaskState, &x.Question); err != nil {
			rows.Close()
			return out, err
		}
		x.TaskID = &id
		items = append(items, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	add(DecisionSourceLinear, len(items), items)

	if err := s.linkOpenDecisionItems(ctx, out.Items); err != nil {
		return out, err
	}
	for i := range out.Items {
		out.Items[i].Question = firstBytes(out.Items[i].Question, decisionQuestionPreview)
		out.Items[i].CreatedAt = out.Items[i].CreatedAt.UTC()
	}
	return out, nil
}

// linkOpenDecisionItems fills each item's job state, the task its job is
// bound to, and that task's state.
func (s *Store) linkOpenDecisionItems(ctx context.Context, items []OpenDecisionItem) error {
	jobIDs := []string{}
	for _, x := range items {
		if x.JobID != "" {
			jobIDs = append(jobIDs, x.JobID)
		}
	}
	type jobLink struct {
		task  int64
		state string
	}
	jobs := map[string]jobLink{}
	if len(jobIDs) > 0 {
		rows, err := s.pool.Query(ctx, `SELECT job_id,COALESCE(task_id,0),state FROM jobs WHERE job_id=ANY($1)`, jobIDs)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var j jobLink
			if err := rows.Scan(&id, &j.task, &j.state); err != nil {
				rows.Close()
				return err
			}
			jobs[id] = j
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	taskIDs := []int64{}
	for i := range items {
		x := &items[i]
		if j, ok := jobs[x.JobID]; ok && x.JobID != "" {
			x.JobState = j.state
			if x.TaskID == nil && j.task > 0 {
				id := j.task
				x.TaskID = &id
			}
		}
		if x.TaskID != nil && x.TaskState == "" {
			taskIDs = append(taskIDs, *x.TaskID)
		}
	}
	if len(taskIDs) == 0 {
		return nil
	}
	states := map[int64]string{}
	rows, err := s.pool.Query(ctx, `SELECT id,state FROM tasks WHERE id=ANY($1)`, taskIDs)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			rows.Close()
			return err
		}
		states[id] = state
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range items {
		if x := &items[i]; x.TaskID != nil && x.TaskState == "" {
			x.TaskState = states[*x.TaskID]
		}
	}
	return nil
}

// Dry-run classes.
const (
	DecisionClassLive                = "live"
	DecisionClassSupersededCandidate = "superseded_candidate"
	DecisionClassStaleCandidate      = "stale_candidate"
	DecisionClassOrphan              = "orphan"
)

// DecisionClasses lists the classes in report order.
func DecisionClasses() []string {
	return []string{DecisionClassLive, DecisionClassSupersededCandidate, DecisionClassStaleCandidate, DecisionClassOrphan}
}

// DecisionStaleAge is the age past which an item with no live task or job is
// a stale candidate.
const DecisionStaleAge = 14 * 24 * time.Hour

// ClassifyOpenDecision sorts one open item. The first rule that matches wins:
//
//  1. orphan: it names no task and no job that hk has a record of;
//  2. superseded_candidate: its task is completed or canceled (icebox
//     included), or its job is completed or lost;
//  3. live: its task is started, or its job is claimed or spawned;
//  4. stale_candidate: it is older than DecisionStaleAge;
//  5. live: a younger item waiting on a task not started yet or on an
//     escalated job.
func ClassifyOpenDecision(x OpenDecisionItem, now time.Time) string {
	hasTask := x.TaskID != nil && x.TaskState != ""
	hasJob := x.JobID != "" && x.JobState != ""
	switch {
	case !hasTask && !hasJob:
		return DecisionClassOrphan
	case (hasTask && DecisionEndsTask(x.TaskState)) || (hasJob && DecisionEndsJob(x.JobState)):
		return DecisionClassSupersededCandidate
	case (hasTask && IsStartedState(x.TaskState)) || (hasJob && (x.JobState == "claimed" || x.JobState == "spawned")):
		return DecisionClassLive
	case now.Sub(x.CreatedAt) > DecisionStaleAge:
		return DecisionClassStaleCandidate
	}
	return DecisionClassLive
}

// DecisionBackfillRow is one classified item. Age is whole hours at the
// server's clock.
type DecisionBackfillRow struct {
	OpenDecisionItem
	AgeHours int64  `json:"age_hours"`
	Class    string `json:"class"`
}

// DecisionBackfillReport is the dry run's output: every row, totals per class
// and per source (every class and source present, zero included).
type DecisionBackfillReport struct {
	DryRun     bool                  `json:"dry_run"`
	ServerTime time.Time             `json:"server_time"`
	Rows       []DecisionBackfillRow `json:"rows"`
	ByClass    map[string]int        `json:"by_class"`
	BySource   map[string]int        `json:"by_source"`
	Total      int                   `json:"total"`
	Truncated  []string              `json:"truncated"`
}

// ClassifyOpenDecisions classifies a read at its server time. Rows are in
// source order, then by created_at and source_ref.
func ClassifyOpenDecisions(src OpenDecisionSources) DecisionBackfillReport {
	out := DecisionBackfillReport{DryRun: true, ServerTime: src.ServerTime, Rows: []DecisionBackfillRow{}, ByClass: map[string]int{}, BySource: map[string]int{}, Truncated: src.Truncated}
	if out.Truncated == nil {
		out.Truncated = []string{}
	}
	for _, c := range DecisionClasses() {
		out.ByClass[c] = 0
	}
	order := map[string]int{}
	for i, x := range DecisionSources() {
		out.BySource[x] = 0
		order[x] = i
	}
	for _, x := range src.Items {
		class := ClassifyOpenDecision(x, src.ServerTime)
		age := int64(src.ServerTime.Sub(x.CreatedAt) / time.Hour)
		if age < 0 {
			age = 0
		}
		out.Rows = append(out.Rows, DecisionBackfillRow{OpenDecisionItem: x, AgeHours: age, Class: class})
		out.ByClass[class]++
		out.BySource[x.Source]++
		out.Total++
	}
	sort.SliceStable(out.Rows, func(i, j int) bool {
		a, b := out.Rows[i], out.Rows[j]
		if order[a.Source] != order[b.Source] {
			return order[a.Source] < order[b.Source]
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.SourceRef < b.SourceRef
	})
	return out
}
