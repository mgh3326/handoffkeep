package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const LinearDrainAdvisoryLock int64 = 824180174

const (
	LinearOpIssueCreate     = "issue_create"
	LinearOpIssueState      = "issue_state"
	LinearOpTerminalComment = "terminal_comment"
	LinearOpTerminalArchive = "terminal_archive"
)

type LinearStateMapping struct {
	Name     string
	Mutate   bool
	Terminal bool
}

// LinearTaskStateMapping is the sole hk-to-Linear state mapping table.
var LinearTaskStateMapping = map[string]LinearStateMapping{
	"backlog":        {Name: "Backlog", Mutate: true},
	"claimed":        {Name: "In Progress", Mutate: true},
	"in_progress":    {Name: "In Progress", Mutate: true},
	"verifying":      {Name: "In Review", Mutate: true},
	"join":           {Name: "In Review", Mutate: true},
	"merged":         {Name: "Done", Mutate: true, Terminal: true},
	"dropped":        {Name: "Canceled", Mutate: true, Terminal: true},
	"hold":           {Name: "Backlog", Mutate: true},
	"needs_decision": {Mutate: false},
}

func TaskStateNames() []string {
	names := make([]string, 0, len(taskStates))
	for name := range taskStates {
		names = append(names, name)
	}
	return names
}

type LinearOutboxPayload struct {
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	State       string   `json:"state,omitempty"`
	Comment     string   `json:"comment,omitempty"`
}

type LinearOutbox struct {
	ID            int64               `json:"id"`
	TaskID        int64               `json:"task_id"`
	Seq           int                 `json:"seq"`
	Op            string              `json:"op"`
	Payload       LinearOutboxPayload `json:"payload"`
	State         string              `json:"state"`
	Attempts      int                 `json:"attempts"`
	NextAttemptAt time.Time           `json:"next_attempt_at"`
	LastError     string              `json:"last_error"`
	RemoteID      string              `json:"remote_id"`
	CreatedAt     time.Time           `json:"created_at"`
	UpdatedAt     time.Time           `json:"updated_at"`
}

type LinearIssue struct {
	TaskID     int64     `json:"task_id"`
	IssueID    string    `json:"issue_id"`
	Identifier string    `json:"identifier"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type LinearOutboxResult struct {
	State      string
	RetryAt    time.Time
	LastError  string
	RemoteID   string
	Identifier string
}

type LinearOutboxStatus struct {
	Pending   int    `json:"pending"`
	Failed    int    `json:"failed"`
	LastError string `json:"last_error,omitempty"`
}

func (s *Store) EnableLinearSync() { s.linearSync.Store(true) }

func (s *Store) LinearSyncEnabled() bool { return s.linearSync.Load() }

func validLinearOp(op string) bool {
	switch op {
	case LinearOpIssueCreate, LinearOpIssueState, LinearOpTerminalComment, LinearOpTerminalArchive:
		return true
	default:
		return false
	}
}

func enqueueLinearOutboxTx(ctx context.Context, tx pgx.Tx, taskID int64, op string, payload LinearOutboxPayload) error {
	if !validLinearOp(op) {
		return errors.New("invalid linear outbox operation")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `INSERT INTO linear_outbox(task_id,seq,op,payload,state,attempts,next_attempt_at,last_error,remote_id,created_at,updated_at)
		SELECT $1,COALESCE(MAX(seq),0)+1,$2,$3::jsonb,'pending',0,$4,'','',$4,$4 FROM linear_outbox WHERE task_id=$1`, taskID, op, string(raw), now)
	return err
}

func uniqueLinearLabels(task Task) []string {
	values := append([]string{}, task.Refs.Linear.Labels...)
	values = append(values, task.Lane)
	if task.Refs.Linear.Tier != "" {
		values = append(values, task.Refs.Linear.Tier)
	}
	if task.Refs.Linear.Grade != "" {
		values = append(values, task.Refs.Linear.Grade)
	}
	seen := map[string]bool{}
	labels := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			labels = append(labels, value)
		}
	}
	return labels
}

func recorded(value, prefix string) string {
	if value == "" || (prefix != "" && !strings.HasPrefix(value, prefix)) {
		return "미기록"
	}
	return value
}

func enqueueLinearTaskCreate(ctx context.Context, tx pgx.Tx, task Task) error {
	mapping := LinearTaskStateMapping["backlog"]
	metadata := task.Refs.Linear
	description := strings.Join([]string{
		"Purpose: " + task.Title,
		"Brief: " + recorded(metadata.Brief, ""),
		"Lane: " + task.Lane,
		"Tier: " + recorded(metadata.Tier, ""),
		"Grade: " + recorded(metadata.Grade, ""),
		fmt.Sprintf("hk-task:%d", task.ID),
	}, "\n")
	return enqueueLinearOutboxTx(ctx, tx, task.ID, LinearOpIssueCreate, LinearOutboxPayload{
		Title:       task.Title,
		Description: description,
		Labels:      uniqueLinearLabels(task),
		State:       mapping.Name,
	})
}

var deploySHA = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

func linearTerminalComment(task Task, note string) string {
	metadata := task.Refs.Linear
	summary := strings.TrimSpace(note)
	if summary == "" {
		summary = task.Title + " → " + task.State
	}
	deploy := metadata.DeploySHA
	if !deploySHA.MatchString(deploy) {
		deploy = "미기록"
	}
	return strings.Join([]string{
		"Summary: " + summary,
		"Report: " + recorded(metadata.Report, "report/"),
		"Verify (independent): " + recorded(metadata.Verify, "report/"),
		"Decision: " + recorded(metadata.Decision, "decision/"),
		"Deploy SHA: " + deploy,
	}, "\n")
}

func enqueueLinearTaskTransition(ctx context.Context, tx pgx.Tx, task Task, note string) error {
	mapping, known := LinearTaskStateMapping[task.State]
	if !known {
		return fmt.Errorf("missing Linear mapping for task state %q", task.State)
	}
	if !mapping.Mutate {
		return nil
	}
	if err := enqueueLinearOutboxTx(ctx, tx, task.ID, LinearOpIssueState, LinearOutboxPayload{State: mapping.Name}); err != nil {
		return err
	}
	if !mapping.Terminal {
		return nil
	}
	if err := enqueueLinearOutboxTx(ctx, tx, task.ID, LinearOpTerminalComment, LinearOutboxPayload{Comment: linearTerminalComment(task, note)}); err != nil {
		return err
	}
	return enqueueLinearOutboxTx(ctx, tx, task.ID, LinearOpTerminalArchive, LinearOutboxPayload{})
}

func scanLinearOutbox(row interface{ Scan(...any) error }, item *LinearOutbox) error {
	var raw []byte
	if err := row.Scan(&item.ID, &item.TaskID, &item.Seq, &item.Op, &raw, &item.State, &item.Attempts, &item.NextAttemptAt, &item.LastError, &item.RemoteID, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return err
	}
	return json.Unmarshal(raw, &item.Payload)
}

const linearOutboxColumns = `id,task_id,seq,op,payload,state,attempts,next_attempt_at,last_error,remote_id,created_at,updated_at`

// ProcessNextLinearOutbox holds the selected row lock through processing. A
// single advisory-lock owner normally calls it, while SKIP LOCKED keeps the
// database boundary safe if a second caller is accidentally introduced.
func (s *Store) ProcessNextLinearOutbox(ctx context.Context, process func(context.Context, LinearOutbox) (LinearOutboxResult, error)) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var item LinearOutbox
	err = scanLinearOutbox(tx.QueryRow(ctx, `SELECT `+linearOutboxColumns+` FROM linear_outbox current
		WHERE state='pending' AND next_attempt_at<=now()
		AND NOT EXISTS (
			SELECT 1 FROM linear_outbox earlier
			WHERE earlier.task_id=current.task_id AND earlier.seq<current.seq
			AND earlier.state NOT IN ('sent','skipped')
		)
		ORDER BY task_id ASC,seq ASC FOR UPDATE SKIP LOCKED LIMIT 1`), &item)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, err := process(ctx, item)
	if err != nil {
		return false, err
	}
	if result.State != "pending" && result.State != "sent" && result.State != "failed" && result.State != "skipped" {
		return false, fmt.Errorf("invalid linear outbox result state %q", result.State)
	}
	if result.State == "pending" && result.RetryAt.IsZero() {
		return false, errors.New("pending linear outbox result requires retry time")
	}
	if result.State != "pending" {
		result.RetryAt = item.NextAttemptAt
	}
	now := time.Now().UTC()
	if result.RemoteID != "" {
		created := now
		if _, err = tx.Exec(ctx, `INSERT INTO linear_issues(task_id,issue_id,identifier,created_at,updated_at) VALUES($1,$2,$3,$4,$4)
			ON CONFLICT(task_id) DO UPDATE SET issue_id=EXCLUDED.issue_id,identifier=EXCLUDED.identifier,updated_at=EXCLUDED.updated_at`, item.TaskID, result.RemoteID, result.Identifier, created); err != nil {
			return false, err
		}
	}
	remoteID := result.RemoteID
	if remoteID == "" {
		remoteID = item.RemoteID
	}
	if _, err = tx.Exec(ctx, `UPDATE linear_outbox SET state=$2,attempts=attempts+1,next_attempt_at=$3,last_error=$4,remote_id=$5,updated_at=$6 WHERE id=$1`, item.ID, result.State, result.RetryAt, result.LastError, remoteID, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Store) GetLinearIssue(ctx context.Context, taskID int64) (LinearIssue, bool, error) {
	var issue LinearIssue
	err := s.pool.QueryRow(ctx, `SELECT task_id,issue_id,identifier,created_at,updated_at FROM linear_issues WHERE task_id=$1`, taskID).Scan(&issue.TaskID, &issue.IssueID, &issue.Identifier, &issue.CreatedAt, &issue.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return issue, false, nil
	}
	return issue, err == nil, err
}

func (s *Store) ListLinearOutbox(ctx context.Context, taskID int64) ([]LinearOutbox, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+linearOutboxColumns+` FROM linear_outbox WHERE ($1=0 OR task_id=$1) ORDER BY task_id,seq`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []LinearOutbox{}
	for rows.Next() {
		var item LinearOutbox
		if err := scanLinearOutbox(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetLinearOutboxStatus(ctx context.Context) (LinearOutboxStatus, error) {
	var status LinearOutboxStatus
	err := s.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE state='pending'),
		count(*) FILTER (WHERE state='failed'),
		COALESCE((SELECT last_error FROM linear_outbox WHERE last_error<>'' ORDER BY updated_at DESC,id DESC LIMIT 1),'')
		FROM linear_outbox`).Scan(&status.Pending, &status.Failed, &status.LastError)
	return status, err
}

type LinearDrainLease struct {
	conn *pgxpool.Conn
}

func (s *Store) TryLinearDrainLease(ctx context.Context) (*LinearDrainLease, bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, LinearDrainAdvisoryLock).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return &LinearDrainLease{conn: conn}, true, nil
}

func (lease *LinearDrainLease) Release(ctx context.Context) error {
	if lease == nil || lease.conn == nil {
		return nil
	}
	conn := lease.conn
	lease.conn = nil
	defer conn.Release()
	var released bool
	if err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, LinearDrainAdvisoryLock).Scan(&released); err != nil {
		return err
	}
	if !released {
		return errors.New("linear drain advisory lock was not held")
	}
	return nil
}
