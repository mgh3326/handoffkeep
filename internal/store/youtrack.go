package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// YouTrack mirror rows, the project mapping, and the link rows the write
// protocol and the audit read. The tables the removed sync wrote
// (youtrack_outbox, youtrack_field_cache, youtrack_cursor,
// youtrack_attempts, youtrack_processed_activities) are left in place,
// unused; a later migration drops them, and MySQL never creates them
// (hk 5077).

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
	err := scanYouTrackMirror(s.db().QueryRow(ctx, sqlFor(s,
		`SELECT `+youtrackMirrorColumns+` FROM task_origins WHERE provider='youtrack' AND task_id=$1`,
		`SELECT `+youtrackMirrorColumns+` FROM task_origins WHERE provider='youtrack' AND task_id=?`), taskID), &x)
	if errors.Is(err, ErrNoRows) {
		return YouTrackMirror{}, false, nil
	}
	return x, err == nil, err
}

// FindYouTrackMirrorByIssueID finds a task by the issue's stable internal id.
// The display number is deliberately not a key.
func (s *Store) FindYouTrackMirrorByIssueID(ctx context.Context, issueID string) (YouTrackMirror, bool, error) {
	var x YouTrackMirror
	err := scanYouTrackMirror(s.db().QueryRow(ctx, sqlFor(s,
		`SELECT `+youtrackMirrorColumns+` FROM task_origins WHERE provider='youtrack' AND external_id=$1`,
		`SELECT `+youtrackMirrorColumns+` FROM task_origins WHERE provider='youtrack' AND external_id=?`), issueID), &x)
	if errors.Is(err, ErrNoRows) {
		return YouTrackMirror{}, false, nil
	}
	return x, err == nil, err
}

func (s *Store) ListYouTrackMirrors(ctx context.Context, taskIDs []int64) (map[int64]YouTrackMirror, error) {
	out := map[int64]YouTrackMirror{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	var query string
	var args []any
	if s.dialect == mysqlDialect {
		query = `SELECT ` + youtrackMirrorColumns + ` FROM task_origins WHERE provider='youtrack' AND task_id IN (` + placeholders(len(taskIDs)) + `)`
		args = make([]any, len(taskIDs))
		for i, id := range taskIDs {
			args[i] = id
		}
	} else {
		query = `SELECT ` + youtrackMirrorColumns + ` FROM task_origins WHERE provider='youtrack' AND task_id = ANY($1)`
		args = []any{taskIDs}
	}
	rows, err := s.db().Query(ctx, query, args...)
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
	if s.dialect == mysqlDialect {
		return s.upsertYouTrackMirrorMySQL(ctx, in, at, syncedAt)
	}
	var x YouTrackMirror
	err := scanYouTrackMirror(s.db().QueryRow(ctx, `
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

// upsertYouTrackMirrorMySQL is UpsertYouTrackMirror on MySQL, which has no
// RETURNING and no conflict target. ON DUPLICATE KEY UPDATE would also match
// the issue's own unique key and then rewrite that row — the other task's —
// so the write targets the task's row only: an UPDATE, an INSERT when the task
// has none, and a re-read of the row it wrote. A unique violation means
// another task already holds the issue (ErrYouTrackMirrorConflict), exactly as
// PostgreSQL's conflict on the issue's key does.
func (s *Store) upsertYouTrackMirrorMySQL(ctx context.Context, in YouTrackMirrorInput, at time.Time, syncedAt any) (YouTrackMirror, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return YouTrackMirror{}, err
	}
	defer release()
	defer tx.Rollback(ctx)
	displayRef, url := nullIfEmpty(in.DisplayRef), nullIfEmpty(in.URL)
	// apply writes this call's fields on the task's own row. A unique
	// violation here is always the issue's key held by another task.
	apply := func() (int64, error) {
		n, err := tx.Exec(ctx, `UPDATE task_origins SET namespace=?,external_id=?,display_ref=?,url=?,tenant=?,role=?,
			synced_at=COALESCE(?,synced_at)
		WHERE provider='youtrack' AND task_id=?`,
			in.Tenant, in.IssueID, displayRef, url, in.Tenant, in.Role, syncedAt, in.TaskID)
		if errors.Is(err, errUniqueViolation) {
			return 0, ErrYouTrackMirrorConflict
		}
		return n, err
	}
	n, err := apply()
	if err != nil {
		return YouTrackMirror{}, err
	}
	if n == 0 {
		// No affected row: the task has no youtrack row (CLIENT_FOUND_ROWS
		// makes an unchanged match count as affected, so zero means absent).
		// The locking read holds the key until this transaction ends, so the
		// insert cannot race a second writer of the same task.
		var id int64
		err = tx.QueryRow(ctx, `SELECT id FROM task_origins WHERE provider='youtrack' AND task_id=? FOR UPDATE`,
			in.TaskID).Scan(&id)
		switch {
		case errors.Is(err, ErrNoRows):
			_, err = tx.Exec(ctx, `
				INSERT INTO task_origins(task_id,provider,namespace,external_id,display_ref,url,title_snapshot,snapshot_at,created_by,created_at,tenant,role,synced_at)
				VALUES(?, 'youtrack', ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)`,
				in.TaskID, in.Tenant, in.IssueID, displayRef, url, at, YouTrackSyncActor, at, in.Tenant, in.Role, syncedAt)
			if errors.Is(err, errUniqueViolation) {
				// The task had no row, so the duplicate is the issue's own
				// key: another task holds it.
				return YouTrackMirror{}, ErrYouTrackMirrorConflict
			}
		case err == nil:
			// A same-task writer committed its row between our UPDATE and the
			// locking read (READ COMMITTED takes no gap lock on an absent
			// key). Its row is here now; this call's fields are applied to it
			// rather than dropped.
			_, err = apply()
		}
		if err != nil {
			return YouTrackMirror{}, err
		}
	}
	var x YouTrackMirror
	if err := scanYouTrackMirror(tx.QueryRow(ctx,
		`SELECT `+youtrackMirrorColumns+` FROM task_origins WHERE provider='youtrack' AND task_id=?`, in.TaskID), &x); err != nil {
		return YouTrackMirror{}, err
	}
	return x, tx.Commit(ctx)
}

// ValidTaskLane reports whether name is a lane/task name. Other packages use
// it instead of copying the lane alphabet.
func ValidTaskLane(name string) bool { return validName(name) }

// TaskProjectExists reports whether name is in the server's project
// vocabulary.
func (s *Store) TaskProjectExists(ctx context.Context, name string) (bool, error) {
	var known bool
	err := s.db().QueryRow(ctx, sqlFor(s,
		`SELECT EXISTS(SELECT 1 FROM task_projects WHERE name=$1)`,
		`SELECT EXISTS(SELECT 1 FROM task_projects WHERE name=?)`), name).Scan(&known)
	return known, err
}

func (s *Store) ListYouTrackProjects(ctx context.Context) ([]YouTrackProject, error) {
	rows, err := s.db().Query(ctx, `SELECT hk_project,yt_project_id,short_name FROM youtrack_projects ORDER BY hk_project`)
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
	err := s.db().QueryRow(ctx, sqlFor(s,
		`SELECT hk_project,yt_project_id,short_name FROM youtrack_projects WHERE hk_project=$1`,
		`SELECT hk_project,yt_project_id,short_name FROM youtrack_projects WHERE hk_project=?`), hkProject).Scan(&x.HKProject, &x.ProjectID, &x.ShortName)
	if errors.Is(err, ErrNoRows) {
		err = s.db().QueryRow(ctx, `SELECT hk_project,yt_project_id,short_name FROM youtrack_projects WHERE hk_project='other'`).Scan(&x.HKProject, &x.ProjectID, &x.ShortName)
		if errors.Is(err, ErrNoRows) {
			return YouTrackProject{}, false, nil
		}
	}
	return x, err == nil, err
}

// YouTrackProjectByShortName maps an issue's project back to an hk project.
func (s *Store) YouTrackProjectByShortName(ctx context.Context, shortName string) (YouTrackProject, bool, error) {
	var x YouTrackProject
	err := s.db().QueryRow(ctx, sqlFor(s,
		`SELECT hk_project,yt_project_id,short_name FROM youtrack_projects WHERE lower(short_name)=lower($1)`,
		`SELECT hk_project,yt_project_id,short_name FROM youtrack_projects WHERE lower(short_name)=lower(?)`), shortName).Scan(&x.HKProject, &x.ProjectID, &x.ShortName)
	if errors.Is(err, ErrNoRows) {
		return YouTrackProject{}, false, nil
	}
	return x, err == nil, err
}

// PutYouTrackProject writes one mapping.
func (s *Store) PutYouTrackProject(ctx context.Context, p YouTrackProject) error {
	if strings.TrimSpace(p.HKProject) == "" || strings.TrimSpace(p.ShortName) == "" {
		return ErrYouTrackProjectUnknown
	}
	now := time.Now().UTC()
	args := []any{p.HKProject, p.ProjectID, p.ShortName, now}
	if s.dialect == mysqlDialect {
		// One placeholder per column on MySQL; PostgreSQL repeats $4.
		args = append(args, now)
	}
	_, err := s.db().Exec(ctx, sqlFor(s, `
		INSERT INTO youtrack_projects(hk_project,yt_project_id,short_name,created_at,updated_at)
		VALUES($1,$2,$3,$4,$4)
		ON CONFLICT(hk_project) DO UPDATE SET yt_project_id=EXCLUDED.yt_project_id,short_name=EXCLUDED.short_name,updated_at=EXCLUDED.updated_at`,
		`
		INSERT INTO youtrack_projects(hk_project,yt_project_id,short_name,created_at,updated_at)
		VALUES(?,?,?,?,?)
		ON DUPLICATE KEY UPDATE yt_project_id=VALUES(yt_project_id),short_name=VALUES(short_name),updated_at=VALUES(updated_at)`),
		args...)
	return err
}

// SetYouTrackProjectID records the YouTrack id of a mapped project. It is not a
// mapping write: the id is resolved on first use, and clearing skips here would
// let unrelated skipped items back into every batch. Writing the mapping
// (PutYouTrackProject) is what clears skips.
func (s *Store) SetYouTrackProjectID(ctx context.Context, hkProject, projectID string) error {
	now := time.Now().UTC()
	args := []any{hkProject, projectID, now}
	if s.dialect == mysqlDialect {
		// MySQL binds in statement order: the SET clause comes first.
		args = []any{projectID, now, hkProject}
	}
	_, err := s.db().Exec(ctx, sqlFor(s,
		`UPDATE youtrack_projects SET yt_project_id=$2,updated_at=$3 WHERE hk_project=$1`,
		`UPDATE youtrack_projects SET yt_project_id=?,updated_at=? WHERE hk_project=?`), args...)
	return err
}

// LinkYouTrackComment records a mirrored comment. It is idempotent for the
// same pair.
func (s *Store) LinkYouTrackComment(ctx context.Context, taskID, hkCommentID int64, youtrackCommentID string, at time.Time) error {
	if taskID < 1 || hkCommentID < 1 || strings.TrimSpace(youtrackCommentID) == "" {
		return ErrYouTrackActivityInput
	}
	_, err := s.db().Exec(ctx, sqlFor(s, `
		INSERT INTO youtrack_comment_links(hk_comment_id,task_id,youtrack_comment_id,created_at)
		VALUES($1,$2,$3,$4)
		ON CONFLICT(hk_comment_id) DO NOTHING`, `
		INSERT INTO youtrack_comment_links(hk_comment_id,task_id,youtrack_comment_id,created_at)
		VALUES(?,?,?,?)`), hkCommentID, taskID, youtrackCommentID, at.UTC())
	// PostgreSQL's ON CONFLICT(hk_comment_id) DO NOTHING ignores a repeat of
	// hk_comment_id and raises on a repeat of youtrack_comment_id. MySQL
	// reports both as a duplicate, so on a duplicate check which one it was:
	// a row already holding hk_comment_id is the no-op; otherwise the YouTrack
	// id belongs to another comment and the error must surface, not be
	// dropped.
	if s.dialect == mysqlDialect && errors.Is(err, errUniqueViolation) {
		var held int64
		if qerr := s.db().QueryRow(ctx, `SELECT COUNT(*) FROM youtrack_comment_links WHERE hk_comment_id=?`, hkCommentID).Scan(&held); qerr != nil {
			return qerr
		}
		if held > 0 {
			return nil
		}
	}
	return err
}

// YouTrackSyncLock is the single-run guard. Both forms are session scoped
// (pg_try_advisory_lock, GET_LOCK), so the caller must hold its connection
// until Release.
type YouTrackSyncLock struct {
	conn      *pgxpool.Conn
	mysqlConn *sql.Conn
}

func (s *Store) TryYouTrackSyncLock(ctx context.Context) (*YouTrackSyncLock, bool, error) {
	if s.dialect == mysqlDialect {
		conn, held, err := mysqlTrySessionLock(ctx, s.mysqlPool, mysqlYTSyncLock)
		if err != nil || !held {
			return nil, held, err
		}
		return &YouTrackSyncLock{mysqlConn: conn}, true, nil
	}
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
	if l == nil {
		return nil
	}
	if l.mysqlConn != nil {
		err := mysqlReleaseSessionLock(l.mysqlConn, mysqlYTSyncLock)
		l.mysqlConn = nil
		return err
	}
	if l.conn == nil {
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
	rows, err := s.db().Query(ctx, `SELECT DISTINCT COALESCE(project,''),lane FROM tasks WHERE lane <> '' ORDER BY 1,2`)
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
