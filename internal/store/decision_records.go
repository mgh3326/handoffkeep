package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A decision record is the one row decision_requests keeps per question put to
// the operator (docs/linear-internalize.md, "Decision requests"). Six sources
// hold such questions, each with its own open and answered rule; the source's
// record stays the authority and the row mirrors it:
//
//	task_flag     a task's awaiting_decision flag, set without a structured
//	              request (disposition items included). source_ref is
//	              "<task id>/<awaiting event id>": a re-ask is a new row.
//	task_request  refs.decision_request; source_ref is the request id.
//	job_escalate  a job.escalate relay event; source_ref is the event id.
//	lane_event    a lane.event beginning [decision-needed]; source_ref is the
//	              event id.
//	chat          a chat_questions row; source_ref is the question id.
//	linear        a request posted as a Linear issue; source_ref is the
//	              request id. While the issue is the place to answer, the
//	              task_request row of the same request is superseded by it,
//	              so one question has one open row.
//
// Each writer creates its row in its own transaction; each existing answer
// path marks it answered there too, and only when the source itself counts the
// question answered. Nothing applies a default on its own: a passed due_at or
// expires_at changes how a row is listed, never its status or its source.
// The only other close is supersede: a task reaching a completed or canceled
// state, or a job ending (completed or lost, or a relay event the escalation
// rule treats as its end), supersedes its open rows.
//
// superseded_by names what replaced a row: "task_request:<id>" (a new
// revision), "linear:<request id>", "task:<id>:<state>", "job:<id>:<state>",
// or "withdrawn" (the source withdrew the question).
//
// Lock order (lockorder.go): an UPDATE of a row that names a task or a job
// runs only under the task-mutation lock (level 2), so those writers run one
// at a time; chat rows are written only by the chat writers and a snooze
// locks one row and nothing else. An INSERT takes a new key and waits for no
// existing row.

const (
	DecisionSourceTaskFlag    = "task_flag"
	DecisionSourceTaskRequest = "task_request"
	DecisionSourceJobEscalate = "job_escalate"
	DecisionSourceLaneEvent   = "lane_event"
	DecisionSourceChat        = "chat"
	DecisionSourceLinear      = "linear"
)

// DecisionSources lists the sources in a fixed order.
func DecisionSources() []string {
	return []string{DecisionSourceTaskFlag, DecisionSourceTaskRequest, DecisionSourceJobEscalate, DecisionSourceLaneEvent, DecisionSourceChat, DecisionSourceLinear}
}

const (
	DecisionStatusOpen       = "open"
	DecisionStatusAnswered   = "answered"
	DecisionStatusSuperseded = "superseded"
	// DecisionStatusExpired is never stored: it is how an open row past its
	// expires_at is listed.
	DecisionStatusExpired = "expired"
)

var (
	ErrDecisionRecordNotFound = errors.New("decision_record_not_found")
	ErrDecisionRecordNotOpen  = errors.New("decision_record_not_open")
)

// DecisionRecord is one decision_requests row as read. Status is the stored
// status, or expired for an open row past expires_at; Overdue and Snoozed are
// derived at read time.
type DecisionRecord struct {
	ID            int64            `json:"id"`
	Source        string           `json:"source"`
	SourceRef     string           `json:"source_ref"`
	TaskID        *int64           `json:"task_id"`
	JobID         string           `json:"job_id"`
	Lane          string           `json:"lane"`
	Question      string           `json:"question"`
	Options       *DecisionOptions `json:"options,omitempty"`
	Recommended   string           `json:"recommended"`
	DefaultAction string           `json:"default_action"`
	DueAt         *time.Time       `json:"due_at"`
	SnoozedUntil  *time.Time       `json:"snoozed_until"`
	ExpiresAt     *time.Time       `json:"expires_at"`
	Status        string           `json:"status"`
	Overdue       bool             `json:"overdue"`
	Snoozed       bool             `json:"snoozed"`
	Answer        string           `json:"answer"`
	AnsweredBy    string           `json:"answered_by"`
	AnsweredAt    *time.Time       `json:"answered_at"`
	SupersededBy  string           `json:"superseded_by"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

const decisionRecordColumns = `id,source,source_ref,task_id,job_id,lane,question,options,recommended,default_action,due_at,snoozed_until,expires_at,status,answer,answered_by,answered_at,superseded_by,created_at,updated_at`

func scanDecisionRecord(row pgx.Row, x *DecisionRecord, now time.Time) error {
	var options []byte
	if err := row.Scan(&x.ID, &x.Source, &x.SourceRef, &x.TaskID, &x.JobID, &x.Lane, &x.Question, &options, &x.Recommended, &x.DefaultAction,
		&x.DueAt, &x.SnoozedUntil, &x.ExpiresAt, &x.Status, &x.Answer, &x.AnsweredBy, &x.AnsweredAt, &x.SupersededBy, &x.CreatedAt, &x.UpdatedAt); err != nil {
		return err
	}
	x.Options = nil
	if len(options) > 0 && string(options) != "null" {
		var o DecisionOptions
		if err := json.Unmarshal(options, &o); err != nil {
			return err
		}
		x.Options = &o
	}
	if x.Status == DecisionStatusOpen {
		if x.ExpiresAt != nil && !now.Before(*x.ExpiresAt) {
			x.Status = DecisionStatusExpired
		}
		x.Overdue = x.DueAt != nil && !now.Before(*x.DueAt)
		x.Snoozed = x.SnoozedUntil != nil && now.Before(*x.SnoozedUntil)
	}
	return nil
}

// decisionOpen is a new open row.
type decisionOpen struct {
	Source, SourceRef string
	TaskID            int64
	JobID, Lane       string
	Question          string
	Options           *DecisionOptions
	Recommended       string
	DefaultAction     string
	DueAt             *time.Time
	At                time.Time
}

// openDecisionTx creates the row for a new question. A redelivered write
// finds its key taken and changes nothing.
func openDecisionTx(ctx context.Context, tx pgx.Tx, d decisionOpen) error {
	var options any
	if d.Options != nil {
		b, err := json.Marshal(d.Options)
		if err != nil {
			return err
		}
		options = string(b)
	}
	var taskID any
	if d.TaskID > 0 {
		taskID = d.TaskID
	}
	_, err := tx.Exec(ctx, `INSERT INTO decision_requests(source,source_ref,task_id,job_id,lane,question,options,recommended,default_action,due_at,status,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,'open',$11,$11) ON CONFLICT (source,source_ref) DO NOTHING`,
		d.Source, d.SourceRef, taskID, d.JobID, d.Lane, d.Question, options, d.Recommended, d.DefaultAction, d.DueAt, d.At)
	return err
}

// answerDecisionTx marks the open row of one source record answered. A row
// that is already closed, or was never created, is left alone.
func answerDecisionTx(ctx context.Context, tx pgx.Tx, sources []string, ref, answer, by string, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE decision_requests SET status='answered',answer=$3,answered_by=$4,answered_at=$5,updated_at=$5
		WHERE source=ANY($1) AND source_ref=$2 AND status='open'`, sources, ref, answer, by, at)
	return err
}

// supersedeDecisionTx marks the open row of one source record superseded.
func supersedeDecisionTx(ctx context.Context, tx pgx.Tx, sources []string, ref, by string, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE decision_requests SET status='superseded',superseded_by=$3,updated_at=$4
		WHERE source=ANY($1) AND source_ref=$2 AND status='open'`, sources, ref, by, at)
	return err
}

// supersedeTaskDecisionsTx supersedes every open row of a task that reached a
// completed or canceled state. Rows are locked in id order.
func supersedeTaskDecisionsTx(ctx context.Context, tx pgx.Tx, taskID int64, state string, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE decision_requests SET status='superseded',superseded_by=$2,updated_at=$3
		WHERE id IN (SELECT id FROM decision_requests WHERE task_id=$1 AND status='open' ORDER BY id FOR UPDATE)`,
		taskID, fmt.Sprintf("task:%d:%s", taskID, state), at)
	return err
}

// supersedeJobDecisionsTx supersedes every open row of a job that ended.
func supersedeJobDecisionsTx(ctx context.Context, tx pgx.Tx, jobID, state string, at time.Time) error {
	if jobID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE decision_requests SET status='superseded',superseded_by=$2,updated_at=$3
		WHERE id IN (SELECT id FROM decision_requests WHERE job_id=$1 AND status='open' ORDER BY id FOR UPDATE)`,
		jobID, "job:"+jobID+":"+state, at)
	return err
}

// DecisionEndsTask reports whether a task in state supersedes its open
// decision records: a completed or canceled state (icebox included).
func DecisionEndsTask(state string) bool {
	c := TaskCategory(state)
	return c == CategoryCompleted || c == CategoryCanceled
}

// DecisionEndsJob reports whether a job in state supersedes its open decision
// records. An escalated job is waiting on the answer, so it does not.
func DecisionEndsJob(state string) bool { return state == "completed" || state == "lost" }

// relayJobEndKinds are the relay events that close a job's escalations in
// ListOpenEscalations; they supersede the job's open rows.
var relayJobEndKinds = map[string]bool{"job.completed": true, "job.joined": true, "job.lost": true, "job.revoked": true}

// taskDecisionBefore is what a task write compares against: the flag and a
// copy of the structured request as they were under the row lock.
type taskDecisionBefore struct {
	flagged bool
	request *DecisionRequest
}

func decisionBeforeOf(x Task) taskDecisionBefore {
	b := taskDecisionBefore{flagged: x.AwaitingDecision}
	if x.Refs.DecisionRequest != nil {
		r := *x.Refs.DecisionRequest
		b.request = &r
	}
	return b
}

var requestSources = []string{DecisionSourceTaskRequest, DecisionSourceLinear}

// mirrorTaskDecisionsTx keeps a task's rows in step with a write of the task
// row in the same transaction. before is the row as locked, after the row as
// written. note is the question (when the write asks) or the answer (when it
// clears the flag); flagEvent is the awaiting_decision event the write
// inserted when it set the flag, which keys the task_flag row.
func mirrorTaskDecisionsTx(ctx context.Context, tx pgx.Tx, before taskDecisionBefore, after Task, by, note string, flagEvent int64, at time.Time) error {
	if DecisionEndsTask(after.State) {
		return supersedeTaskDecisionsTx(ctx, tx, after.ID, after.State, at)
	}
	prev, cur := before.request, after.Refs.DecisionRequest
	switch {
	case cur != nil && (prev == nil || prev.ID != cur.ID):
		// A new request. It carries the question a flag asked before it, and
		// replaces the revision it names.
		if cur.Status == DecisionRequestOpen {
			d := decisionOpen{Source: DecisionSourceTaskRequest, SourceRef: cur.ID, TaskID: after.ID, Lane: after.Lane, Question: cur.Question,
				Options: after.Refs.DecisionOptions, DefaultAction: cur.DefaultAction, DueAt: cur.DueAt, At: at}
			if after.Refs.DecisionOptions != nil {
				d.Recommended = recommendedOption(*after.Refs.DecisionOptions)
			}
			if err := openDecisionTx(ctx, tx, d); err != nil {
				return err
			}
		}
		by := DecisionSourceTaskRequest + ":" + cur.ID
		if prev != nil {
			if err := supersedeDecisionTx(ctx, tx, requestSources, prev.ID, by, at); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE decision_requests SET status='superseded',superseded_by=$2,updated_at=$3 WHERE task_id=$1 AND source='task_flag' AND status='open'`, after.ID, by, at); err != nil {
			return err
		}
	case cur != nil && prev != nil && prev.Status == DecisionRequestOpen && cur.Status != DecisionRequestOpen:
		if cur.Status == DecisionRequestWithdrawn {
			if err := supersedeDecisionTx(ctx, tx, requestSources, cur.ID, "withdrawn", at); err != nil {
				return err
			}
			break
		}
		answer, who := requestAnswer(*cur)
		if who == "" {
			who = by
		}
		if err := answerDecisionTx(ctx, tx, requestSources, cur.ID, answer, who, at); err != nil {
			return err
		}
	}
	if !before.flagged && after.AwaitingDecision && flagEvent > 0 && (cur == nil || cur.Status != DecisionRequestOpen) {
		d := decisionOpen{Source: DecisionSourceTaskFlag, SourceRef: fmt.Sprintf("%d/%d", after.ID, flagEvent), TaskID: after.ID, Lane: after.Lane, Question: note, Options: after.Refs.DecisionOptions, At: at}
		if after.Refs.DecisionOptions != nil {
			d.Recommended = recommendedOption(*after.Refs.DecisionOptions)
		}
		if err := openDecisionTx(ctx, tx, d); err != nil {
			return err
		}
	}
	if before.flagged && !after.AwaitingDecision {
		if _, err := tx.Exec(ctx, `UPDATE decision_requests SET status='answered',answer=$2,answered_by=$3,answered_at=$4,updated_at=$4
			WHERE id IN (SELECT id FROM decision_requests WHERE task_id=$1 AND source='task_flag' AND status='open' ORDER BY id FOR UPDATE)`, after.ID, note, by, at); err != nil {
			return err
		}
	}
	return nil
}

func recommendedOption(o DecisionOptions) string {
	for _, x := range o.Options {
		if x.Recommended {
			return x.Key
		}
	}
	return ""
}

// requestAnswer is the answer text and answerer a resolved request records.
func requestAnswer(r DecisionRequest) (answer, who string) {
	if r.Resolution == nil {
		return r.Status, ""
	}
	res := r.Resolution
	parts := []string{}
	if r.Status == DecisionRequestDefaultApplied {
		parts = append(parts, "default applied")
	}
	if res.Option != "" {
		parts = append(parts, res.Option)
	}
	if res.Text != "" {
		parts = append(parts, res.Text)
	}
	if res.Receipt != "" {
		parts = append(parts, "receipt "+res.Receipt)
	}
	who = res.By
	if res.Responder != "" {
		who = res.Responder
	}
	return strings.Join(parts, ": "), who
}

// jobTaskTx is the task a job is bound to, or 0.
func jobTaskTx(ctx context.Context, tx pgx.Tx, jobID string) (int64, error) {
	if jobID == "" {
		return 0, nil
	}
	var id int64
	err := tx.QueryRow(ctx, `SELECT task_id FROM jobs WHERE job_id=$1`, jobID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

const laneAnsweredPrefix = "[decision-answered] #"

// laneAnsweredID parses the question id of an exact "[decision-answered]
// #<id>:" reply, the text ListOpenLaneDecisions matches (the id as Postgres
// prints it: no sign, no leading zero).
func laneAnsweredID(text string) (int64, bool) {
	rest, ok := strings.CutPrefix(text, laneAnsweredPrefix)
	if !ok {
		return 0, false
	}
	digits, _, ok := strings.Cut(rest, ":")
	if !ok || digits == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || id < 1 || strconv.FormatInt(id, 10) != digits {
		return 0, false
	}
	return id, true
}

// relayTouchesDecisions reports whether mirroring a relay event updates rows
// that name a task or a job, so its transaction must take the task-mutation
// lock first.
func relayTouchesDecisions(x RelayEvent) bool {
	if relayJobEndKinds[x.Kind] {
		return true
	}
	if x.Kind != "lane.event" {
		return false
	}
	_, answered := laneAnsweredID(x.Text)
	return answered || strings.Contains(x.EventID, "decision-escalation-")
}

// mirrorRelayDecisionsTx runs for a relay event that was just created. The
// matching rules are the ones ListOpenEscalations and ListOpenLaneDecisions
// apply, so a row is answered exactly when its source stops counting it open.
func mirrorRelayDecisionsTx(ctx context.Context, tx pgx.Tx, x RelayEvent) error {
	at := x.ReceivedAt.UTC()
	switch {
	case x.Kind == "job.escalate":
		taskID, err := jobTaskTx(ctx, tx, x.JobID)
		if err != nil {
			return err
		}
		question := x.Question
		if question == "" {
			question = x.Reason
		}
		return openDecisionTx(ctx, tx, decisionOpen{Source: DecisionSourceJobEscalate, SourceRef: strconv.FormatInt(x.ID, 10), TaskID: taskID, JobID: x.JobID, Lane: x.OwnerLane, Question: question, At: at})
	case relayJobEndKinds[x.Kind]:
		return supersedeJobDecisionsTx(ctx, tx, x.JobID, strings.TrimPrefix(x.Kind, "job."), at)
	case x.Kind != "lane.event":
		return nil
	}
	if strings.HasPrefix(x.Text, "[decision-needed]") {
		taskID, err := jobTaskTx(ctx, tx, x.JobID)
		if err != nil {
			return err
		}
		if err := openDecisionTx(ctx, tx, decisionOpen{Source: DecisionSourceLaneEvent, SourceRef: strconv.FormatInt(x.ID, 10), TaskID: taskID, JobID: x.JobID, Lane: x.OwnerLane, Question: x.Text, At: at}); err != nil {
			return err
		}
	}
	by := fmt.Sprintf("lane.event #%d", x.ID)
	if id, ok := laneAnsweredID(x.Text); ok && id < x.ID {
		if _, err := tx.Exec(ctx, `UPDATE decision_requests d SET status='answered',answer=$4,answered_by=$5,answered_at=$6,updated_at=$6
			FROM relay_events e WHERE d.source='lane_event' AND d.source_ref=$1 AND d.status='open'
			AND e.id=$2 AND e.kind='lane.event' AND e.owner_lane=$3`,
			strconv.FormatInt(id, 10), id, x.OwnerLane, x.Text, by, at); err != nil {
			return err
		}
	}
	if strings.Contains(x.EventID, "decision-escalation-") {
		if _, err := tx.Exec(ctx, `UPDATE decision_requests d SET status='answered',answer=$4,answered_by=$5,answered_at=$6,updated_at=$6
			WHERE d.id IN (SELECT d2.id FROM decision_requests d2 JOIN relay_events e ON d2.source_ref=e.id::text
				WHERE d2.source='job_escalate' AND d2.status='open' AND e.kind='job.escalate' AND e.owner_lane=$1 AND e.id<$2
				AND $3 LIKE '%decision-escalation-' || e.id::text || '-%' ORDER BY d2.id FOR UPDATE OF d2)`,
			x.OwnerLane, x.ID, x.EventID, x.Text, by, at); err != nil {
			return err
		}
	}
	return nil
}

// DecisionRecordFilter selects rows for ListDecisionRecords. Status "" means
// open. Snoozed open rows are hidden unless IncludeSnoozed.
type DecisionRecordFilter struct {
	Status         string
	Lane           string
	Source         string
	IncludeSnoozed bool
	Limit          int
}

// ListDecisionRecords returns rows oldest first (created_at, then id). now is
// the clock snooze, expiry and due dates are judged against.
func (s *Store) ListDecisionRecords(ctx context.Context, f DecisionRecordFilter, now time.Time) ([]DecisionRecord, error) {
	if f.Status == "" {
		f.Status = DecisionStatusOpen
	}
	switch f.Status {
	case DecisionStatusOpen, DecisionStatusAnswered, DecisionStatusSuperseded, DecisionStatusExpired:
	default:
		return nil, errors.New("invalid decision request status")
	}
	if f.Source != "" && !validDecisionSource(f.Source) {
		return nil, errors.New("invalid decision request source")
	}
	if f.Lane != "" && !validName(f.Lane) {
		return nil, errors.New("invalid decision request lane")
	}
	if f.Limit < 1 || f.Limit > 1000 {
		f.Limit = 1000
	}
	// Every branch references $1, so its type is always known.
	args := []any{now}
	q := `SELECT ` + decisionRecordColumns + ` FROM decision_requests WHERE `
	switch f.Status {
	case DecisionStatusOpen:
		q += `status='open' AND (expires_at IS NULL OR expires_at>$1::timestamptz)`
	case DecisionStatusExpired:
		q += `status='open' AND expires_at<=$1::timestamptz`
	default:
		args = append(args, f.Status)
		q += fmt.Sprintf(`$1::timestamptz IS NOT NULL AND status=$%d`, len(args))
	}
	if !f.IncludeSnoozed && (f.Status == DecisionStatusOpen || f.Status == DecisionStatusExpired) {
		q += ` AND (snoozed_until IS NULL OR snoozed_until<=$1::timestamptz)`
	}
	if f.Lane != "" {
		args = append(args, f.Lane)
		q += fmt.Sprintf(` AND lane=$%d`, len(args))
	}
	if f.Source != "" {
		args = append(args, f.Source)
		q += fmt.Sprintf(` AND source=$%d`, len(args))
	}
	args = append(args, f.Limit)
	q += fmt.Sprintf(` ORDER BY created_at ASC,id ASC LIMIT $%d`, len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DecisionRecord{}
	for rows.Next() {
		var x DecisionRecord
		if err := scanDecisionRecord(rows, &x, now); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// GetDecisionRecord reads one row.
func (s *Store) GetDecisionRecord(ctx context.Context, id int64, now time.Time) (DecisionRecord, bool, error) {
	var x DecisionRecord
	err := scanDecisionRecord(s.pool.QueryRow(ctx, `SELECT `+decisionRecordColumns+` FROM decision_requests WHERE id=$1`, id), &x, now)
	if errors.Is(err, pgx.ErrNoRows) {
		return DecisionRecord{}, false, nil
	}
	return x, err == nil, err
}

// SnoozeDecisionRecord hides an open row from the default list until until.
// It writes the row only, never its source. A time not after now ends a
// snooze.
func (s *Store) SnoozeDecisionRecord(ctx context.Context, id int64, until, now time.Time) (DecisionRecord, error) {
	if id < 1 || until.IsZero() {
		return DecisionRecord{}, errors.New("invalid decision request snooze")
	}
	until = until.UTC()
	var x DecisionRecord
	err := scanDecisionRecord(s.pool.QueryRow(ctx, `UPDATE decision_requests SET snoozed_until=$2,updated_at=$3 WHERE id=$1 AND status='open' RETURNING `+decisionRecordColumns, id, until, now.UTC()), &x, now)
	if err == nil {
		return x, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DecisionRecord{}, err
	}
	if _, found, err := s.GetDecisionRecord(ctx, id, now); err != nil {
		return DecisionRecord{}, err
	} else if !found {
		return DecisionRecord{}, ErrDecisionRecordNotFound
	}
	return DecisionRecord{}, ErrDecisionRecordNotOpen
}

func validDecisionSource(source string) bool {
	for _, x := range DecisionSources() {
		if x == source {
			return true
		}
	}
	return false
}

// mirrorLinearDecisionTx runs when a request gets its Linear issue. While the
// request is still the task's open one, the issue becomes where it is
// answered: a linear row opens and the task_request row of the same request
// is superseded by it. The answer itself still lands on the task's request
// (AnswerLinearDecision, or any request resolve), which mirrorTaskDecisionsTx
// writes to both rows.
func mirrorLinearDecisionTx(ctx context.Context, tx pgx.Tx, link LinearDecisionLink, at time.Time) error {
	var x Task
	err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1`, link.TaskID), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	r := x.Refs.DecisionRequest
	if r == nil || r.ID != link.RequestID || r.Status != DecisionRequestOpen || DecisionEndsTask(x.State) {
		return nil
	}
	d := decisionOpen{Source: DecisionSourceLinear, SourceRef: r.ID, TaskID: x.ID, Lane: x.Lane, Question: r.Question,
		Options: x.Refs.DecisionOptions, DefaultAction: r.DefaultAction, DueAt: r.DueAt, At: at}
	if x.Refs.DecisionOptions != nil {
		d.Recommended = recommendedOption(*x.Refs.DecisionOptions)
	}
	if err := openDecisionTx(ctx, tx, d); err != nil {
		return err
	}
	return supersedeDecisionTx(ctx, tx, []string{DecisionSourceTaskRequest}, r.ID, DecisionSourceLinear+":"+r.ID, at)
}
