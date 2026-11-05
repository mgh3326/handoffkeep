package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// Status updates with health (docs/linear-internalize.md, "Model additions").
// A person posts a project's health with a short body. The store records the
// project's task counts by category at write time (snapshot) and the change
// from the project's previous update (diff). Staleness is computed at read
// time: a project with no update within StatusUpdateStaleAfter is stale.

const (
	HealthOnTrack  = "on_track"
	HealthAtRisk   = "at_risk"
	HealthOffTrack = "off_track"
)

// StatusUpdateStaleAfter is how long an update stays current.
const StatusUpdateStaleAfter = 7 * 24 * time.Hour

// ErrStatusUpdateInvalid refuses a status update; the text after its code is
// store-authored.
var ErrStatusUpdateInvalid = errors.New("invalid_status_update")

// StatusCounts is a project's tasks by category. In a diff each field is the
// change from the previous update.
type StatusCounts struct {
	Triage    int `json:"triage"`
	Backlog   int `json:"backlog"`
	Unstarted int `json:"unstarted"`
	Started   int `json:"started"`
	Completed int `json:"completed"`
	Canceled  int `json:"canceled"`
}

func (c *StatusCounts) add(category string, n int) {
	switch category {
	case CategoryTriage:
		c.Triage += n
	case CategoryBacklog:
		c.Backlog += n
	case CategoryUnstarted:
		c.Unstarted += n
	case CategoryStarted:
		c.Started += n
	case CategoryCompleted:
		c.Completed += n
	case CategoryCanceled:
		c.Canceled += n
	}
}

func (c StatusCounts) minus(p StatusCounts) StatusCounts {
	return StatusCounts{
		Triage: c.Triage - p.Triage, Backlog: c.Backlog - p.Backlog, Unstarted: c.Unstarted - p.Unstarted,
		Started: c.Started - p.Started, Completed: c.Completed - p.Completed, Canceled: c.Canceled - p.Canceled,
	}
}

type StatusHealthChange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// StatusDiff is an update against the project's previous one. The first
// update of a project has no previous: PreviousID is null and Counts is
// omitted. Health is set only when it changed.
type StatusDiff struct {
	PreviousID *int64              `json:"previous_id"`
	Health     *StatusHealthChange `json:"health,omitempty"`
	Counts     *StatusCounts       `json:"counts,omitempty"`
}

type StatusUpdate struct {
	ID        int64        `json:"id"`
	Project   string       `json:"project"`
	Health    string       `json:"health"`
	Body      string       `json:"body"`
	Snapshot  StatusCounts `json:"snapshot"`
	Diff      StatusDiff   `json:"diff"`
	CreatedBy string       `json:"created_by"`
	CreatedAt time.Time    `json:"created_at"`
}

// StatusUpdateList is a project's updates, newest first, with its staleness
// at the time of the read.
type StatusUpdateList struct {
	Project      string         `json:"project"`
	Stale        bool           `json:"stale"`
	LastUpdateAt *time.Time     `json:"last_update_at"`
	Updates      []StatusUpdate `json:"updates"`
}

// ValidHealth reports whether h is a status update health.
func ValidHealth(h string) bool {
	return h == HealthOnTrack || h == HealthAtRisk || h == HealthOffTrack
}

// StatusUpdateStale reports whether a project whose newest update is at last
// (nil: none) is stale at now.
func StatusUpdateStale(last *time.Time, now time.Time) bool {
	return last == nil || now.Sub(*last) > StatusUpdateStaleAfter
}

// statusDiff compares an update with the project's previous one (nil: none).
func statusDiff(prev *StatusUpdate, health string, snapshot StatusCounts) StatusDiff {
	if prev == nil {
		return StatusDiff{}
	}
	id := prev.ID
	counts := snapshot.minus(prev.Snapshot)
	d := StatusDiff{PreviousID: &id, Counts: &counts}
	if prev.Health != health {
		d.Health = &StatusHealthChange{From: prev.Health, To: health}
	}
	return d
}

func validateStatusUpdate(project, health, body, by string) error {
	switch {
	case !validName(project):
		return fmt.Errorf("%w: project must be a project name", ErrStatusUpdateInvalid)
	case !ValidHealth(health):
		return fmt.Errorf("%w: health must be on_track, at_risk or off_track", ErrStatusUpdateInvalid)
	case strings.TrimSpace(body) == "":
		return fmt.Errorf("%w: body is required", ErrStatusUpdateInvalid)
	case len(body) > MaxBytes:
		return fmt.Errorf("%w: body exceeds %d bytes", ErrStatusUpdateInvalid, MaxBytes)
	case !utf8.ValidString(body) || strings.ContainsRune(body, 0):
		return fmt.Errorf("%w: body must be UTF-8 text", ErrStatusUpdateInvalid)
	case by == "" || !validText(by, 128):
		return fmt.Errorf("%w: author is required", ErrStatusUpdateInvalid)
	}
	return guard.Reject(body)
}

const statusUpdateColumns = `id,project,health,body,snapshot,diff,created_by,created_at`

func scanStatusUpdate(row pgx.Row, x *StatusUpdate) error {
	var snapshot, diff []byte
	if err := row.Scan(&x.ID, &x.Project, &x.Health, &x.Body, &snapshot, &diff, &x.CreatedBy, &x.CreatedAt); err != nil {
		return err
	}
	x.CreatedAt = x.CreatedAt.UTC()
	if _, err := scanJSONB(snapshot, &x.Snapshot); err != nil {
		return err
	}
	_, err := scanJSONB(diff, &x.Diff)
	return err
}

// CreateStatusUpdate records project's health at now. The project must exist
// (ErrProjectNotFound otherwise).
//
// Lock order (lockorder.go): the transaction first locks the project's
// task_projects row FOR NO KEY UPDATE, so two updates of one project diff in
// order; it then only reads tasks and status_updates and inserts one new row.
func (s *Store) CreateStatusUpdate(ctx context.Context, project, health, body, by string, now time.Time) (StatusUpdate, error) {
	if err := validateStatusUpdate(project, health, body, by); err != nil {
		return StatusUpdate{}, err
	}
	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StatusUpdate{}, err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `SELECT name FROM task_projects WHERE name=$1 FOR NO KEY UPDATE`, project).Scan(new(string)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StatusUpdate{}, ErrProjectNotFound
		}
		return StatusUpdate{}, err
	}
	var snapshot StatusCounts
	rows, err := tx.Query(ctx, `SELECT state,count(*) FROM tasks WHERE project=$1 GROUP BY state`, project)
	if err != nil {
		return StatusUpdate{}, err
	}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			rows.Close()
			return StatusUpdate{}, err
		}
		snapshot.add(TaskCategory(state), n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return StatusUpdate{}, err
	}
	var prev *StatusUpdate
	var p StatusUpdate
	err = scanStatusUpdate(tx.QueryRow(ctx, `SELECT `+statusUpdateColumns+` FROM status_updates WHERE project=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, project), &p)
	switch {
	case err == nil:
		prev = &p
	case !errors.Is(err, pgx.ErrNoRows):
		return StatusUpdate{}, err
	}
	diff := statusDiff(prev, health, snapshot)
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return StatusUpdate{}, err
	}
	diffJSON, err := json.Marshal(diff)
	if err != nil {
		return StatusUpdate{}, err
	}
	var x StatusUpdate
	if err := scanStatusUpdate(tx.QueryRow(ctx, `INSERT INTO status_updates(project,health,body,snapshot,diff,created_by,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+statusUpdateColumns, project, health, body, snapshotJSON, diffJSON, by, now), &x); err != nil {
		return StatusUpdate{}, err
	}
	return x, tx.Commit(ctx)
}

// ListStatusUpdates returns project's newest updates first, at most limit
// (1..1000; out of range means 100), and its staleness at now.
func (s *Store) ListStatusUpdates(ctx context.Context, project string, limit int, now time.Time) (StatusUpdateList, error) {
	if !validName(project) {
		return StatusUpdateList{}, fmt.Errorf("%w: project must be a project name", ErrStatusUpdateInvalid)
	}
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	if err := s.pool.QueryRow(ctx, `SELECT name FROM task_projects WHERE name=$1`, project).Scan(new(string)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StatusUpdateList{}, ErrProjectNotFound
		}
		return StatusUpdateList{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+statusUpdateColumns+` FROM status_updates WHERE project=$1 ORDER BY created_at DESC,id DESC LIMIT $2`, project, limit)
	if err != nil {
		return StatusUpdateList{}, err
	}
	defer rows.Close()
	out := StatusUpdateList{Project: project, Updates: []StatusUpdate{}}
	for rows.Next() {
		var x StatusUpdate
		if err := scanStatusUpdate(rows, &x); err != nil {
			return StatusUpdateList{}, err
		}
		out.Updates = append(out.Updates, x)
	}
	if err := rows.Err(); err != nil {
		return StatusUpdateList{}, err
	}
	if len(out.Updates) > 0 {
		at := out.Updates[0].CreatedAt
		out.LastUpdateAt = &at
	}
	out.Stale = StatusUpdateStale(out.LastUpdateAt, now.UTC())
	return out, nil
}

// latestStatusUpdatesTx reads the newest update of each project.
func latestStatusUpdatesTx(ctx context.Context, tx pgx.Tx, projects []string) (map[string]statusLatest, error) {
	out := map[string]statusLatest{}
	if len(projects) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (project) project,health,created_at FROM status_updates
		WHERE project = ANY($1) ORDER BY project,created_at DESC,id DESC`, projects)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var x statusLatest
		if err := rows.Scan(&name, &x.Health, &x.At); err != nil {
			return nil, err
		}
		x.At = x.At.UTC()
		out[name] = x
	}
	return out, rows.Err()
}
