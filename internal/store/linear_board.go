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
)

const LinearOpWorkBoard = "work_board"

type LinearBoardSession struct {
	JobID   string    `json:"job_id"`
	Host    string    `json:"host"`
	Pane    string    `json:"pane"`
	Family  string    `json:"family"`
	Model   string    `json:"model"`
	Profile string    `json:"profile"`
	Role    string    `json:"role"`
	Repo    string    `json:"repo"`
	Verdict string    `json:"verdict,omitempty"`
	Active  bool      `json:"active"`
	EndedAt time.Time `json:"ended_at,omitempty"`
	At      time.Time `json:"at"`
}

type LinearBoardEvent struct {
	ID    string    `json:"id"`
	JobID string    `json:"job_id,omitempty"`
	PR    string    `json:"pr,omitempty"`
	Head  string    `json:"head,omitempty"`
	Kind  string    `json:"kind"`
	Text  string    `json:"text"`
	At    time.Time `json:"at"`
}

// LinearBoard is the durable projection. Only event writers update it; the
// hourly reconciler resends it without inventing events or comments.
type LinearBoard struct {
	Version  int64                `json:"version"`
	Task     Task                 `json:"task"`
	State    string               `json:"state"`
	Sessions []LinearBoardSession `json:"sessions"`
	Last     LinearBoardEvent     `json:"last"`
}

func (s *Store) EnableLinearBoard()       { s.linearBoard.Store(true) }
func (s *Store) LinearBoardEnabled() bool { return s.linearBoard.Load() }

func (b *LinearBoard) ApplyTask(task Task) {
	previous := b.Task
	task.Events = nil
	newJob := previous.Refs.JobID != "" && previous.Refs.JobID != task.Refs.JobID
	// A reopen is a task leaving a settled or parked state for unstarted; it
	// replaces the old reopened state as the signal that the previous PR and
	// head no longer describe the work.
	reopened := task.State == StateUnstarted && (IsSettledState(previous.State) || previous.State == StateIcebox)
	if !reopened && !newJob {
		if task.Refs.PR == "" {
			task.Refs.PR = previous.Refs.PR
		}
		if task.Refs.HeadSHA == "" {
			task.Refs.HeadSHA = previous.Refs.HeadSHA
		}
	}
	b.Task = task
	if (b.State != "Done" && b.State != "Canceled") || LinearTaskStateMapping[task.State].Terminal || reopened || newJob {
		if b.State == "" || previous.State != task.State || previous.Refs.PR != task.Refs.PR || newJob {
			if mapping := LinearTaskStateMapping[task.State]; mapping.Mutate && !task.AwaitingDecision {
				b.State = mapping.Name
			}
			if task.Refs.PR != "" && (task.State == StateResearching || task.State == StateDeveloping) {
				b.State = "In Review"
			}
		}
	}
	if b.State == "" || (b.State == "Backlog" && task.Refs.JobID != "") {
		b.State = "In Progress"
	}
	if previous.Refs.JobID != "" && previous.Refs.JobID != task.Refs.JobID {
		for i := range b.Sessions {
			b.Sessions[i].Active = false
			if task.UpdatedAt.After(b.Sessions[i].EndedAt) {
				b.Sessions[i].EndedAt = task.UpdatedAt
			}
		}
	}
}

func (b *LinearBoard) ApplyPlacement(d PlacementDecision) {
	session := LinearBoardSession{JobID: d.JobID, Host: *d.ChosenHost, Pane: d.PaneID, Family: d.Family, Model: d.Model, Profile: d.Profile, Role: d.Role, Repo: d.Repo, Active: true, At: d.DecidedAt}
	for i, old := range b.Sessions {
		if old.JobID != d.JobID {
			continue
		}
		if old.At.After(d.DecidedAt) {
			return
		}
		if session.Pane == "" {
			session.Pane = old.Pane
		}
		if !old.EndedAt.Before(d.DecidedAt) {
			session.Active, session.EndedAt, session.Verdict = false, old.EndedAt, old.Verdict
		}
		b.Sessions[i] = session
		return
	}
	b.Sessions = append(b.Sessions, session)
}

func (b *LinearBoard) ApplyEvent(event LinearBoardEvent, jobID, pr, head string) LinearBoardEvent {
	event.JobID = jobID
	terminal := event.Kind == "READY" || event.Kind == "FINDINGS" || event.Kind == "BLOCKED" || event.Kind == "MERGED"
	if event.Kind == "FINDINGS" && jobID == "mergewake" {
		for i := range b.Sessions {
			session := &b.Sessions[i]
			if session.Active && session.Role == "review" && !event.At.Before(session.At) && !event.At.Before(session.EndedAt) {
				session.Active, session.EndedAt, session.Verdict = false, event.At, event.Text
			}
		}
	}
	if event.Kind == "MERGED" {
		for i := range b.Sessions {
			if !event.At.Before(b.Sessions[i].At) {
				b.Sessions[i].Active = false
				b.Sessions[i].EndedAt = event.At
			}
		}
	}
	// mergewake is a lane sender; only real job ids own sessions.
	if terminal && jobID != "" && jobID != "mergewake" {
		i := b.sessionIndex(jobID)
		session := &b.Sessions[i]
		if !event.At.Before(session.At) && !event.At.Before(session.EndedAt) {
			session.Active, session.EndedAt = false, event.At
			if event.Text != "" {
				session.Verdict = event.Text
			}
		}
		if session.Role == "review" && event.Kind == "READY" {
			event.Kind = "FINDINGS"
		}
	}
	if !event.At.Before(b.Last.At) {
		b.Last = event
		if pr != "" && (b.Task.Refs.PR == "" || BoardPRRef(b.Task.Refs.PR) != BoardPRRef(pr)) {
			b.Task.Refs.PR = pr
		}
		if head != "" {
			b.Task.Refs.HeadSHA = head
		}
		if !IsSettledState(b.Task.State) {
			switch event.Kind {
			case "READY":
				if b.State != "Done" && b.State != "Canceled" {
					b.State = "In Review"
				}
			case "MERGED":
				b.State = "Done"
			}
		}
	}
	return event
}

func (b *LinearBoard) sessionIndex(jobID string) int {
	for i := range b.Sessions {
		if b.Sessions[i].JobID == jobID {
			return i
		}
	}
	b.Sessions = append(b.Sessions, LinearBoardSession{JobID: jobID})
	return len(b.Sessions) - 1
}

func (b *LinearBoard) ApplyJob(job Job) {
	i := b.sessionIndex(job.JobID)
	session := &b.Sessions[i]
	if job.PaneID != "" {
		session.Pane = job.PaneID
	}
	if jobTerminal[job.State] && !job.UpdatedAt.Before(session.At) && !job.UpdatedAt.Before(session.EndedAt) {
		session.Active, session.EndedAt = false, job.UpdatedAt
		if session.Verdict == "" {
			session.Verdict = job.State
		}
	}
}

// PrimaryJob retains the lead's labels after completion and also covers a
// placement arriving before the task claim establishes refs.job_id.
func (b LinearBoard) PrimaryJob() string {
	if b.Task.Refs.JobID != "" {
		return b.Task.Refs.JobID
	}
	var latest LinearBoardSession
	for _, s := range b.Sessions {
		if (s.Role == "lead" || s.Role == "worker") && (latest.JobID == "" || s.At.After(latest.At)) {
			latest = s
		}
	}
	return latest.JobID
}

func loadLinearBoardTx(ctx context.Context, tx pgx.Tx, task Task) (LinearBoard, error) {
	var b LinearBoard
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT status FROM linear_board_tasks WHERE task_id=$1 FOR UPDATE`, task.ID).Scan(&raw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return b, err
	}
	if _, err = scanJSONB(raw, &b); err != nil {
		return b, err
	}
	b.ApplyTask(task)
	return b, nil
}

func saveLinearBoardTx(ctx context.Context, tx pgx.Tx, b LinearBoard, event LinearBoardEvent) error {
	b.Version++
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO linear_board_tasks(task_id,status) VALUES($1,$2::jsonb) ON CONFLICT(task_id) DO UPDATE SET status=EXCLUDED.status`, b.Task.ID, string(raw)); err != nil {
		return err
	}
	if IsHKDotsRequest(b.Task) {
		return nil
	}
	// Always persist the console projection; eligibility gates only delivery.
	if LinearMirrorReason(b.Task) == "" {
		var linked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM linear_issues WHERE task_id=$1)`, b.Task.ID).Scan(&linked); err != nil {
			return err
		}
		if !linked {
			return nil
		}
	}
	return enqueueLinearOutboxTx(ctx, tx, b.Task.ID, LinearOpWorkBoard, LinearOutboxPayload{Board: &b, Event: &event})
}

func (s *Store) recordLinearBoardTaskTx(ctx context.Context, tx pgx.Tx, task Task, note string) error {
	if !s.LinearBoardEnabled() {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM linear_board_tasks WHERE task_id=$1)`, task.ID).Scan(&exists); err != nil {
		return err
	}
	if !exists && !IsStartedState(task.State) && task.Refs.JobID == "" {
		return nil
	}
	b, err := loadLinearBoardTx(ctx, tx, task)
	if err != nil {
		return err
	}
	e := LinearBoardEvent{ID: fmt.Sprintf("task:%d:%s", task.ID, task.UpdatedAt.Format(time.RFC3339Nano)), Kind: task.State, Text: note, At: task.UpdatedAt}
	if !e.At.Before(b.Last.At) {
		b.Last = e
	}
	return saveLinearBoardTx(ctx, tx, b, e)
}

func (s *Store) recordLinearBoardPlacementTx(ctx context.Context, tx pgx.Tx, d PlacementDecision) error {
	if !s.LinearBoardEnabled() {
		return nil
	}
	var task Task
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, *d.TaskID), &task); err != nil {
		return err
	}
	b, err := loadLinearBoardTx(ctx, tx, task)
	if err != nil {
		return err
	}
	b.ApplyPlacement(d)
	job, jobErr := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_id=$1`, d.JobID))
	if jobErr == nil {
		b.ApplyJob(job)
	} else if !errors.Is(jobErr, pgx.ErrNoRows) {
		return jobErr
	}
	if b.State == "Backlog" {
		b.State = "In Progress"
	}
	e := LinearBoardEvent{ID: fmt.Sprintf("placement:%d", d.ID), Kind: "spawn", Text: d.JobID, At: d.DecidedAt}
	if !e.At.Before(b.Last.At) {
		b.Last = e
	}
	return saveLinearBoardTx(ctx, tx, b, e)
}

// BoardRelayEvent accepts the existing job and mergewake lane protocols.
var boardLaneLine = regexp.MustCompile(`^\[([^\]]+)\]\s+(READY|ESC|BLOCKED|FINDINGS|MERGED)\b`)
var boardPRShort = regexp.MustCompile(`\b(?:[A-Za-z0-9_.-]+/)?([A-Za-z0-9_.-]+)#[1-9][0-9]*\b`)
var boardPRURL = regexp.MustCompile(`https://github[.]com/[^/\s]+/([^/\s]+)/pull/([1-9][0-9]*)`)
var boardHead = regexp.MustCompile(`\bhead=([a-fA-F0-9]{7,64})\b`)

func BoardPRRef(text string) string {
	if m := boardPRURL.FindStringSubmatch(text); m != nil {
		return strings.ToLower(m[1] + "#" + m[2])
	}
	if m := boardPRShort.FindString(text); m != "" {
		return strings.ToLower(m[strings.LastIndex(m, "/")+1:])
	}
	return ""
}

func BoardRelayEvent(x RelayEvent) (LinearBoardEvent, string, bool) {
	e := LinearBoardEvent{ID: fmt.Sprintf("relay:%d", x.ID), At: x.ReceivedAt, Text: x.Text, PR: x.PR, Head: x.Head}
	if x.EventTime != nil {
		e.At = *x.EventTime
	}
	job := x.JobID
	if x.Kind == "lane.event" {
		m := boardLaneLine.FindStringSubmatch(x.Text)
		if m == nil {
			return e, job, false
		}
		if job == "" {
			job = m[1]
		}
		e.Kind = m[2]
		if e.PR == "" {
			e.PR = BoardPRRef(x.Text)
		}
		if h := boardHead.FindStringSubmatch(x.Text); h != nil && e.Head == "" {
			e.Head = h[1]
		}
	} else {
		switch x.Kind {
		case "job.completed", "job.joined":
			e.Kind = "READY"
		case "job.escalate":
			e.Kind = "ESC"
		case "job.lost", "job.revoked":
			e.Kind = "BLOCKED"
		default:
			return e, job, false
		}
		e.Text = strings.Join(nonempty(x.Question, x.Reason, x.ReportLastLine), " | ")
	}
	e.JobID = job
	return e, job, true
}

func nonempty(values ...string) []string {
	out := []string{}
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (s *Store) recordLinearBoardRelayTx(ctx context.Context, tx pgx.Tx, x RelayEvent) error {
	if !s.LinearBoardEnabled() {
		return nil
	}
	e, job, ok := BoardRelayEvent(x)
	if !ok {
		return nil
	}
	rows, err := tx.Query(ctx, `WITH placement_owners AS (
 SELECT task_id FROM placement_decisions WHERE job_id=$1 AND task_id IS NOT NULL AND outcome='placed'
 ), job_tasks AS (
 SELECT task_id FROM placement_owners UNION SELECT id FROM tasks
 WHERE NOT EXISTS(SELECT 1 FROM placement_owners) AND (refs->>'job_id'=$1 OR id IN (SELECT task_id FROM jobs WHERE job_id=$1))
 ) SELECT `+taskColumns+` FROM tasks WHERE id IN (SELECT task_id FROM job_tasks)
 OR (NOT EXISTS(SELECT 1 FROM job_tasks) AND (
 ($2<>'' AND refs->>'pr'=$2)
 OR ($3<>'' AND lower(regexp_replace(regexp_replace(refs->>'pr','^https://github[.]com/[^/]+/([^/]+)/pull/([0-9]+).*$', '\1#\2'),'^[^/]+/([^/]+#[0-9]+)$','\1'))=$3)))
 ORDER BY id FOR UPDATE`, job, e.PR, BoardPRRef(e.PR))
	if err != nil {
		return err
	}
	tasks := []Task{}
	for rows.Next() {
		var task Task
		if err := scanTask(rows, &task); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, task)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, task := range tasks {
		b, err := loadLinearBoardTx(ctx, tx, task)
		if err != nil {
			return err
		}
		comment := b.ApplyEvent(e, job, e.PR, e.Head)
		if err = saveLinearBoardTx(ctx, tx, b, comment); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) recordLinearBoardJobTx(ctx context.Context, tx pgx.Tx, job Job) error {
	if job.TaskID == nil || !s.LinearBoardEnabled() {
		return nil
	}
	var task Task
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=COALESCE((SELECT task_id FROM placement_decisions WHERE job_id=$1 AND outcome='placed' AND task_id IS NOT NULL ORDER BY decided_at DESC,id DESC LIMIT 1),$2) FOR UPDATE`, job.JobID, job.TaskID), &task); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	b, err := loadLinearBoardTx(ctx, tx, task)
	if err != nil {
		return err
	}
	b.ApplyJob(job)
	return saveLinearBoardTx(ctx, tx, b, LinearBoardEvent{})
}

// Terminal tasks need one repair when their projection is missing or stale, then
// stop generating hourly outbox rows. Recheck under the task lock as well.
const linearBoardReconcileFilter = `(id IN (SELECT task_id FROM linear_board_tasks)
 OR COALESCE(refs->>'job_id','')<>''
 OR id IN (SELECT task_id FROM placement_decisions WHERE outcome='placed' AND task_id IS NOT NULL))
 AND (state NOT IN (` + settledStatesSQL + `)
 OR NOT EXISTS (SELECT 1 FROM linear_board_tasks b WHERE b.task_id=tasks.id)
 OR EXISTS (
 SELECT 1 FROM linear_board_tasks b WHERE b.task_id=tasks.id
 AND (b.status->'task'->>'state' IS DISTINCT FROM tasks.state
 OR (b.status->'task'->>'updated_at')::timestamptz IS DISTINCT FROM tasks.updated_at)))`

func (s *Store) ReconcileLinearBoard(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT id FROM tasks WHERE `+linearBoardReconcileFilter+` ORDER BY id`)
	if err != nil {
		return err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var reconcileErrors []error
	for _, id := range ids {
		if err := s.reconcileLinearBoardTask(ctx, id); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("linear board task %d: %w", id, err))
		}
	}
	return errors.Join(reconcileErrors...)
}

func (s *Store) reconcileLinearBoardTask(ctx context.Context, id int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var task Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 AND (`+linearBoardReconcileFilter+`) FOR UPDATE`, id), &task); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	b, err := loadLinearBoardTx(ctx, tx, task)
	if err != nil {
		return err
	}
	placements, err := tx.Query(ctx, `SELECT `+placementColumns+` FROM placement_decisions WHERE task_id=$1 AND outcome='placed' ORDER BY decided_at,id`, id)
	if err != nil {
		return err
	}
	for placements.Next() {
		decision, err := scanPlacement(placements)
		if err != nil {
			placements.Close()
			return err
		}
		b.ApplyPlacement(decision)
	}
	if b.State == "Backlog" && len(b.Sessions) > 0 && task.State == "backlog" {
		b.State = "In Progress"
	}
	err = placements.Err()
	placements.Close()
	if err != nil {
		return err
	}
	jobs, err := tx.Query(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_id IN (SELECT job_id FROM placement_decisions WHERE task_id=$1 AND outcome='placed') ORDER BY job_id`, id)
	if err != nil {
		return err
	}
	for jobs.Next() {
		job, err := scanJob(jobs)
		if err != nil {
			jobs.Close()
			return err
		}
		b.ApplyJob(job)
	}
	err = jobs.Err()
	jobs.Close()
	if err != nil {
		return err
	}
	if b.Last.ID == "" {
		b.Last = LinearBoardEvent{ID: fmt.Sprintf("task:%d", id), Kind: task.State, At: task.UpdatedAt}
	}
	if err = saveLinearBoardTx(ctx, tx, b, LinearBoardEvent{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
