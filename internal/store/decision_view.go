package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidDecisionTab = errors.New("invalid decision tab")

type DecisionTabItem struct {
	DecisionRecord
	RequestState       string `json:"request_state"`
	RequestID          string `json:"request_id"`
	Revision           int    `json:"revision"`
	SupersededRecordID *int64 `json:"superseded_record_id"`
}

type DecisionTab struct {
	Open      int
	Overdue   int
	Truncated bool
	Items     []DecisionTabItem
}

func (s *Store) ListDecisionTab(ctx context.Context, tab string, now time.Time, limit int) (DecisionTab, error) {
	var out DecisionTab
	var predicate, order string
	switch tab {
	case "open":
		predicate = decisionOpenSQL
		order = `(due_at IS NOT NULL AND due_at<=$1::timestamptz) DESC,due_at ASC NULLS LAST,created_at,id`
	case "overdue":
		predicate = decisionOpenSQL + ` AND due_at<=$1::timestamptz`
		order = `(due_at IS NOT NULL AND due_at<=$1::timestamptz) DESC,due_at ASC NULLS LAST,created_at,id`
	case "answered":
		predicate = `$1::timestamptz IS NOT NULL AND (status='answered' OR (status='superseded' AND superseded_by='withdrawn'))`
		order = `answered_at DESC NULLS LAST,id DESC`
	case "cleanup":
		predicate = decisionOpenSQL + ` AND EXISTS (SELECT 1 FROM tasks WHERE tasks.id=decision_requests.task_id AND tasks.state IN (` + settledStatesSQL + `))`
		order = `created_at,id`
	default:
		return out, ErrInvalidDecisionTab
	}
	if limit < 1 || limit > 200 {
		limit = 200
	}
	now = now.UTC()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `SELECT COUNT(*),COUNT(*) FILTER (WHERE due_at<=$1::timestamptz) FROM decision_requests WHERE `+decisionOpenSQL, now).Scan(&out.Open, &out.Overdue)
	if err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT `+decisionRecordColumns+` FROM decision_requests WHERE `+predicate+` ORDER BY `+order+` LIMIT $2`, now, limit+1)
	if err != nil {
		return out, err
	}
	out.Items = []DecisionTabItem{}
	for rows.Next() {
		var x DecisionTabItem
		if err = scanDecisionRecord(rows, &x.DecisionRecord, now); err != nil {
			rows.Close()
			return out, err
		}
		out.Items = append(out.Items, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Items) > limit {
		out.Truncated = true
		out.Items = out.Items[:limit]
	}
	for i := range out.Items {
		x := &out.Items[i]
		x.SupersededRecordID, err = decisionReplacement(ctx, tx, x.SupersededBy)
		if err != nil {
			return out, err
		}
		if x.Source != DecisionSourceTaskRequest || x.TaskID == nil {
			continue
		}
		task, found, err := decisionViewTask(ctx, tx, *x.TaskID)
		if err != nil {
			return out, err
		}
		if !found {
			continue
		}
		for _, entry := range DecisionRequestHistory(task) {
			if entry.Request.ID == x.SourceRef {
				x.RequestID = entry.Request.ID
				x.Revision = entry.Request.Revision
				x.RequestState = DecisionRequestState(entry.Request, task.State, entry.SupersededBy != "", now)
				switch entry.Request.Status {
				case DecisionRequestOpen, DecisionRequestAnswered, DecisionRequestDefaultApplied, DecisionRequestWithdrawn:
				default:
					x.RequestState = entry.Request.Status
				}
				if entry.Request.Status == DecisionRequestDefaultApplied && (entry.Request.Resolution == nil || entry.Request.Resolution.Receipt == "") {
					x.RequestState = "default_applied_without_receipt"
				}
				break
			}
		}
	}
	return out, tx.Commit(ctx)
}

func decisionReplacement(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, ref string) (*int64, error) {
	source, key, ok := strings.Cut(ref, ":")
	if !ok || source != DecisionSourceTaskRequest {
		return nil, nil
	}
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM decision_requests WHERE source=$1 AND source_ref=$2`, source, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func (s *Store) DecisionReplacement(ctx context.Context, ref string) (*int64, error) {
	return decisionReplacement(ctx, s.pool, ref)
}

func decisionViewTask(ctx context.Context, tx pgx.Tx, id int64) (Task, bool, error) {
	var task Task
	err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1`, id), &task)
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	rows, err := tx.Query(ctx, `SELECT refs FROM task_events WHERE task_id=$1 ORDER BY at,id`, id)
	if err != nil {
		return Task{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return Task{}, false, err
		}
		if len(raw) > 0 {
			var refs TaskRefs
			if _, err = scanJSONB(raw, &refs); err != nil {
				return Task{}, false, fmt.Errorf("decision task event: %w", err)
			}
			task.Events = append(task.Events, TaskEvent{Refs: &refs})
		}
	}
	return task, true, rows.Err()
}

type DecisionAttempt struct {
	ID                int64     `json:"id"`
	Channel           string    `json:"channel"`
	Provenance        string    `json:"provenance"`
	Actor             string    `json:"actor"`
	Result            string    `json:"result"`
	RevisionRequestID string    `json:"revision_request_id"`
	QuestionRevision  int       `json:"question_revision"`
	ReceivedAt        time.Time `json:"received_at"`
}

func (s *Store) ListDecisionAttempts(ctx context.Context, taskID int64, limit int) ([]DecisionAttempt, error) {
	if limit < 1 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT id,channel,COALESCE(provenance,''),actor,result,revision_request_id,COALESCE(question_revision,0),received_at FROM decision_answer_attempts WHERE task_id=$1 ORDER BY id DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DecisionAttempt{}
	for rows.Next() {
		var x DecisionAttempt
		if err = rows.Scan(&x.ID, &x.Channel, &x.Provenance, &x.Actor, &x.Result, &x.RevisionRequestID, &x.QuestionRevision, &x.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
