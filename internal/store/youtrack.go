package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// YouTrack mirror rows, the project mapping, and the link rows the write
// protocol and the audit read. The tables the removed sync wrote
// (youtrack_outbox, youtrack_field_cache, youtrack_cursor,
// youtrack_attempts, youtrack_processed_activities) are left in place,
// unused; a later migration drops them.

const (
	// YouTrackProvider is the task_origins provider for a mirrored issue.
	YouTrackProvider = "youtrack"
	// YouTrackOwnedActor is the author of an hk comment or event that came
	// from YouTrack.
	YouTrackOwnedActor = "operator:youtrack"
	// YouTrackSyncActor writes mirror rows; it is not an operator identity.
	YouTrackSyncActor = "youtrack-sync"
)

var (
	ErrYouTrackMirrorConflict = errors.New("youtrack_mirror_conflict")
	ErrYouTrackTaskNotFound   = errors.New("youtrack_task_not_found")
	ErrYouTrackProjectUnknown = errors.New("youtrack_project_unknown")
	ErrYouTrackActivityInput  = errors.New("youtrack_activity_invalid")
)

// YouTrackMirror is one task's YouTrack issue, stored in task_origins.
type YouTrackMirror struct {
	TaskID     int64      `json:"task_id"`
	IssueID    string     `json:"issue_id"`
	DisplayRef string     `json:"display_ref,omitempty"`
	URL        string     `json:"url,omitempty"`
	Tenant     string     `json:"tenant,omitempty"`
	Role       string     `json:"role"`
	SyncedAt   *time.Time `json:"synced_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// YouTrackMirrorInput is one upsert. Role is mirror (hk created the issue) or
// origin (the issue created the hk task). The zero SyncedAt stores NULL: a
// link adopted before its field push has no watermark yet.
type YouTrackMirrorInput struct {
	TaskID     int64
	IssueID    string
	DisplayRef string
	URL        string
	Tenant     string
	Role       string
	SyncedAt   time.Time
}

// YouTrackProject maps one hk project to one YouTrack project.
type YouTrackProject struct {
	HKProject string `json:"hk_project"`
	ProjectID string `json:"yt_project_id,omitempty"`
	ShortName string `json:"short_name"`
}

type YouTrackCommentLink struct {
	HKCommentID       int64     `json:"hk_comment_id"`
	TaskID            int64     `json:"task_id"`
	YouTrackCommentID string    `json:"youtrack_comment_id"`
	CreatedAt         time.Time `json:"created_at"`
}

const youtrackMirrorColumns = `task_id,external_id,COALESCE(display_ref,''),COALESCE(url,''),tenant,role,synced_at,created_at`

func scanYouTrackMirror(row interface{ Scan(...any) error }, x *YouTrackMirror) error {
	return row.Scan(&x.TaskID, &x.IssueID, &x.DisplayRef, &x.URL, &x.Tenant, &x.Role, &x.SyncedAt, &x.CreatedAt)
}

func (s *Store) GetYouTrackMirror(ctx context.Context, taskID int64) (YouTrackMirror, bool, error) {
	var x YouTrackMirror
	err := scanYouTrackMirror(s.pool.QueryRow(ctx, `SELECT `+youtrackMirrorColumns+` FROM task_origins WHERE provider='youtrack' AND task_id=$1`, taskID), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return YouTrackMirror{}, false, nil
	}
	return x, err == nil, err
}

// FindYouTrackMirrorByIssueID finds a task by the issue's stable internal id.
// The display number is deliberately not a key.
func (s *Store) FindYouTrackMirrorByIssueID(ctx context.Context, issueID string) (YouTrackMirror, bool, error) {
	var x YouTrackMirror
	err := scanYouTrackMirror(s.pool.QueryRow(ctx, `SELECT `+youtrackMirrorColumns+` FROM task_origins WHERE provider='youtrack' AND external_id=$1`, issueID), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return YouTrackMirror{}, false, nil
	}
	return x, err == nil, err
}

func (s *Store) ListYouTrackMirrors(ctx context.Context, taskIDs []int64) (map[int64]YouTrackMirror, error) {
	out := map[int64]YouTrackMirror{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+youtrackMirrorColumns+` FROM task_origins WHERE provider='youtrack' AND task_id = ANY($1)`, taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x YouTrackMirror
		if err := scanYouTrackMirror(rows, &x); err != nil {
			return nil, err
		}
		out[x.TaskID] = x
	}
	return out, rows.Err()
}

// UpsertYouTrackMirror records the one YouTrack issue a task mirrors. An
// existing row is updated in place; an issue already linked to a different
// task is a conflict.
func (s *Store) UpsertYouTrackMirror(ctx context.Context, in YouTrackMirrorInput) (YouTrackMirror, error) {
	if in.TaskID < 1 || strings.TrimSpace(in.IssueID) == "" || strings.TrimSpace(in.Tenant) == "" {
		return YouTrackMirror{}, ErrYouTrackMirrorConflict
	}
	if in.Role != "mirror" && in.Role != "origin" {
		return YouTrackMirror{}, ErrYouTrackMirrorConflict
	}
	at := in.SyncedAt.UTC()
	var syncedAt any
	if !in.SyncedAt.IsZero() {
		syncedAt = at
	} else {
		// No push happened yet; only created_at/snapshot_at need a time.
		at = time.Now().UTC()
	}
	var x YouTrackMirror
	err := scanYouTrackMirror(s.pool.QueryRow(ctx, `
		INSERT INTO task_origins(task_id,provider,namespace,external_id,display_ref,url,title_snapshot,snapshot_at,created_by,created_at,tenant,role,synced_at)
		VALUES($1,'youtrack',$2,$3,$4,$5,NULL,$6,$7,$6,$2,$8,$9)
		ON CONFLICT (task_id) WHERE provider='youtrack' DO UPDATE SET
			namespace=EXCLUDED.namespace,
			external_id=EXCLUDED.external_id,
			display_ref=EXCLUDED.display_ref,
			url=EXCLUDED.url,
			tenant=EXCLUDED.tenant,
			role=EXCLUDED.role,
			synced_at=COALESCE(EXCLUDED.synced_at,task_origins.synced_at)
		RETURNING `+youtrackMirrorColumns,
		in.TaskID, in.Tenant, in.IssueID, nullIfEmpty(in.DisplayRef), nullIfEmpty(in.URL), at, YouTrackSyncActor, in.Role, syncedAt), &x)
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return YouTrackMirror{}, ErrYouTrackMirrorConflict
		}
		return YouTrackMirror{}, err
	}
	return x, nil
}

// ValidTaskLane reports whether name is a lane/task name. Other packages use
// it instead of copying the lane alphabet.
func ValidTaskLane(name string) bool { return validName(name) }

// TaskProjectExists reports whether name is in the server's project
// vocabulary.
func (s *Store) TaskProjectExists(ctx context.Context, name string) (bool, error) {
	var known bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_projects WHERE name=$1)`, name).Scan(&known)
	return known, err
}

func (s *Store) ListYouTrackProjects(ctx context.Context) ([]YouTrackProject, error) {
	rows, err := s.pool.Query(ctx, `SELECT hk_project,yt_project_id,short_name FROM youtrack_projects ORDER BY hk_project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YouTrackProject{}
	for rows.Next() {
		var x YouTrackProject
		if err := rows.Scan(&x.HKProject, &x.ProjectID, &x.ShortName); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// YouTrackProjectFor returns the mapping for an hk project, falling back to
// the OTHER row. found is false only when even OTHER is missing.
func (s *Store) YouTrackProjectFor(ctx context.Context, hkProject string) (YouTrackProject, bool, error) {
	var x YouTrackProject
	err := s.pool.QueryRow(ctx, `SELECT hk_project,yt_project_id,short_name FROM youtrack_projects WHERE hk_project=$1`, hkProject).Scan(&x.HKProject, &x.ProjectID, &x.ShortName)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.pool.QueryRow(ctx, `SELECT hk_project,yt_project_id,short_name FROM youtrack_projects WHERE hk_project='other'`).Scan(&x.HKProject, &x.ProjectID, &x.ShortName)
		if errors.Is(err, pgx.ErrNoRows) {
			return YouTrackProject{}, false, nil
		}
	}
	return x, err == nil, err
}

// YouTrackProjectByShortName maps an issue's project back to an hk project.
func (s *Store) YouTrackProjectByShortName(ctx context.Context, shortName string) (YouTrackProject, bool, error) {
	var x YouTrackProject
	err := s.pool.QueryRow(ctx, `SELECT hk_project,yt_project_id,short_name FROM youtrack_projects WHERE lower(short_name)=lower($1)`, shortName).Scan(&x.HKProject, &x.ProjectID, &x.ShortName)
	if errors.Is(err, pgx.ErrNoRows) {
		return YouTrackProject{}, false, nil
	}
	return x, err == nil, err
}

// PutYouTrackProject writes one mapping.
func (s *Store) PutYouTrackProject(ctx context.Context, p YouTrackProject) error {
	if strings.TrimSpace(p.HKProject) == "" || strings.TrimSpace(p.ShortName) == "" {
		return ErrYouTrackProjectUnknown
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO youtrack_projects(hk_project,yt_project_id,short_name,created_at,updated_at)
		VALUES($1,$2,$3,$4,$4)
		ON CONFLICT(hk_project) DO UPDATE SET yt_project_id=EXCLUDED.yt_project_id,short_name=EXCLUDED.short_name,updated_at=EXCLUDED.updated_at`,
		p.HKProject, p.ProjectID, p.ShortName, time.Now().UTC())
	return err
}

// SetYouTrackProjectID records the YouTrack id of a mapped project. It is not a
// mapping write: the id is resolved on first use, and clearing skips here would
// let unrelated skipped items back into every batch. Writing the mapping
// (PutYouTrackProject) is what clears skips.
func (s *Store) SetYouTrackProjectID(ctx context.Context, hkProject, projectID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE youtrack_projects SET yt_project_id=$2,updated_at=$3 WHERE hk_project=$1`, hkProject, projectID, time.Now().UTC())
	return err
}

// LinkYouTrackComment records a mirrored comment. It is idempotent for the
// same pair.
func (s *Store) LinkYouTrackComment(ctx context.Context, taskID, hkCommentID int64, youtrackCommentID string, at time.Time) error {
	if taskID < 1 || hkCommentID < 1 || strings.TrimSpace(youtrackCommentID) == "" {
		return ErrYouTrackActivityInput
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO youtrack_comment_links(hk_comment_id,task_id,youtrack_comment_id,created_at)
		VALUES($1,$2,$3,$4)
		ON CONFLICT(hk_comment_id) DO NOTHING`, hkCommentID, taskID, youtrackCommentID, at.UTC())
	return err
}

// YouTrackSyncLock is the single-run guard. pg_try_advisory_lock is session
// scoped, so the caller must hold its connection until Release.
type YouTrackSyncLock struct {
	conn *pgxpool.Conn
}

func (s *Store) TryYouTrackSyncLock(ctx context.Context) (*YouTrackSyncLock, bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(8241873404)`).Scan(&held); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !held {
		conn.Release()
		return nil, false, nil
	}
	return &YouTrackSyncLock{conn: conn}, true, nil
}

func (l *YouTrackSyncLock) Release(ctx context.Context) error {
	if l == nil || l.conn == nil {
		return nil
	}
	_, err := l.conn.Exec(ctx, `SELECT pg_advisory_unlock(8241873404)`)
	l.conn.Release()
	l.conn = nil
	return err
}

// ListTaskLanes lists the distinct lanes each project's tasks use, in name
// order, keyed by project; tasks with no project are under "". The YouTrack
// schema run adds each project's lanes to that project's 레인 enum, so a
// project's list holds only the lanes its own tasks used.
func (s *Store) ListTaskLanes(ctx context.Context) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT COALESCE(project,''),lane FROM tasks WHERE lane <> '' ORDER BY 1,2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lanes := map[string][]string{}
	for rows.Next() {
		var project, lane string
		if err := rows.Scan(&project, &lane); err != nil {
			return nil, err
		}
		lanes[project] = append(lanes[project], lane)
	}
	return lanes, rows.Err()
}
