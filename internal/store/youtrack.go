package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

// YouTrack is the operator's screen; hk is the record agents use. PG columns
// are the execution values. A priority or lane write lands in PG first and
// leaves for YouTrack through youtrack_outbox, so a YouTrack outage never
// blocks an hk write. The reverse direction reads activities and applies the
// operator's changes to PG.

const (
	// YouTrackProvider is the task_origins provider for a mirrored issue.
	YouTrackProvider = "youtrack"
	// YouTrackOwnedActor is the author of an hk comment or event that came
	// from YouTrack.
	YouTrackOwnedActor = "operator:youtrack"
	// YouTrackSyncActor writes mirror rows; it is not an operator identity.
	YouTrackSyncActor = "youtrack-sync"
	// YouTrackActivitySource marks task_events rows produced by the pull loop.
	YouTrackActivitySource = "youtrack.activity"

	YouTrackSourceHK       = "hk"
	YouTrackSourceYouTrack = "youtrack"

	YouTrackOpPriority = "priority"
	YouTrackOpLane     = "lane"
)

var (
	ErrYouTrackMirrorConflict = errors.New("youtrack_mirror_conflict")
	ErrYouTrackActivityInput  = errors.New("youtrack_activity_invalid")
	ErrYouTrackTaskNotFound   = errors.New("youtrack_task_not_found")
	ErrYouTrackProjectUnknown = errors.New("youtrack_project_unknown")
	ErrYouTrackOutboxInvalid  = errors.New("youtrack_outbox_invalid")
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
// origin (the issue created the hk task).
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

type YouTrackCursor struct {
	Cursor         string    `json:"cursor"`
	LastActivityID string    `json:"last_activity_id"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type YouTrackCommentLink struct {
	HKCommentID       int64     `json:"hk_comment_id"`
	TaskID            int64     `json:"task_id"`
	YouTrackCommentID string    `json:"youtrack_comment_id"`
	CreatedAt         time.Time `json:"created_at"`
}

type YouTrackFieldCache struct {
	TaskID    int64     `json:"task_id"`
	Priority  int       `json:"priority"`
	Lane      string    `json:"lane"`
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
	Stale     bool      `json:"stale"`
}

type YouTrackOutboxItem struct {
	ID       int64  `json:"id"`
	TaskID   int64  `json:"task_id"`
	Op       string `json:"op"`
	Value    string `json:"value"`
	Attempts int    `json:"attempts"`
}

// YouTrackNewIssue is one issue the operator filed that hk has not seen.
type YouTrackNewIssue struct {
	IssueID    string
	ReadableID string
	Title      string
	Lane       string
	Kind       string
	Project    string
	Tenant     string
	Priority   int
}

// YouTrackActivity is one activitiesPage entry reduced to what hk applies.
type YouTrackActivity struct {
	ID         string
	TaskID     int64
	IssueID    string
	ReadableID string
	State      string
	Priority   *int
	Lane       string
	Comment    string
	CommentID  string
	NewIssue   *YouTrackNewIssue
	// Skip records why this activity was deliberately not applied. It still
	// consumes the activity so a later run does not log it again.
	Skip string
}

const (
	YouTrackOutcomeApplied   = "applied"
	YouTrackOutcomeSkipped   = "skipped"
	YouTrackOutcomeRejected  = "rejected"
	YouTrackOutcomeDuplicate = "duplicate"
)

type YouTrackApplyResult struct {
	Applied bool   `json:"applied"`
	Outcome string `json:"outcome"`
	// Refusal is a short Korean reason when Outcome is rejected.
	Refusal string `json:"refusal,omitempty"`
	// RevertField and RevertValue name the YouTrack field to put back when hk
	// refuses the change (state, priority or lane).
	RevertField string `json:"revert_field,omitempty"`
	RevertValue string `json:"revert_value,omitempty"`
}

type YouTrackPendingRevert struct {
	ActivityID  string
	IssueID     string
	RevertField string
	RevertValue string
	Refusal     string
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
	var x YouTrackMirror
	err := scanYouTrackMirror(s.pool.QueryRow(ctx, `
		INSERT INTO task_origins(task_id,provider,namespace,external_id,display_ref,url,title_snapshot,snapshot_at,created_by,created_at,tenant,role,synced_at)
		VALUES($1,'youtrack',$2,$3,$4,$5,NULL,$6,$7,$6,$2,$8,$6)
		ON CONFLICT (task_id) WHERE provider='youtrack' DO UPDATE SET
			namespace=EXCLUDED.namespace,
			external_id=EXCLUDED.external_id,
			display_ref=EXCLUDED.display_ref,
			url=EXCLUDED.url,
			tenant=EXCLUDED.tenant,
			role=EXCLUDED.role,
			synced_at=EXCLUDED.synced_at
		RETURNING `+youtrackMirrorColumns,
		in.TaskID, in.Tenant, in.IssueID, nullIfEmpty(in.DisplayRef), nullIfEmpty(in.URL), at, YouTrackSyncActor, in.Role), &x)
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return YouTrackMirror{}, ErrYouTrackMirrorConflict
		}
		return YouTrackMirror{}, err
	}
	return x, nil
}

// MarkYouTrackTaskSynced records the task revision the last successful push
// covered.
func (s *Store) MarkYouTrackTaskSynced(ctx context.Context, taskID int64, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE task_origins SET synced_at=$2 WHERE provider='youtrack' AND task_id=$1`, taskID, at.UTC())
	return err
}

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

func (s *Store) SetYouTrackProjectID(ctx context.Context, hkProject, projectID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE youtrack_projects SET yt_project_id=$2,updated_at=now() WHERE hk_project=$1`, hkProject, projectID)
	return err
}

func (s *Store) GetYouTrackCursor(ctx context.Context) (YouTrackCursor, error) {
	var x YouTrackCursor
	err := s.pool.QueryRow(ctx, `SELECT cursor,last_activity_id,updated_at FROM youtrack_cursor WHERE singleton`).Scan(&x.Cursor, &x.LastActivityID, &x.UpdatedAt)
	return x, err
}

func (s *Store) AdvanceYouTrackCursor(ctx context.Context, cursor string) error {
	_, err := s.pool.Exec(ctx, `UPDATE youtrack_cursor SET cursor=$1,updated_at=now() WHERE singleton`, cursor)
	return err
}

func (s *Store) GetYouTrackFieldCaches(ctx context.Context, taskIDs []int64) (map[int64]YouTrackFieldCache, error) {
	out := map[int64]YouTrackFieldCache{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT task_id,priority,lane,source,fetched_at,stale FROM youtrack_field_cache WHERE task_id = ANY($1)`, taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x YouTrackFieldCache
		if err := rows.Scan(&x.TaskID, &x.Priority, &x.Lane, &x.Source, &x.FetchedAt, &x.Stale); err != nil {
			return nil, err
		}
		out[x.TaskID] = x
	}
	return out, rows.Err()
}

func (s *Store) GetYouTrackFieldCache(ctx context.Context, taskID int64) (YouTrackFieldCache, bool, error) {
	var x YouTrackFieldCache
	err := s.pool.QueryRow(ctx, `SELECT task_id,priority,lane,source,fetched_at,stale FROM youtrack_field_cache WHERE task_id=$1`, taskID).Scan(&x.TaskID, &x.Priority, &x.Lane, &x.Source, &x.FetchedAt, &x.Stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return YouTrackFieldCache{}, false, nil
	}
	return x, err == nil, err
}

// PutYouTrackFieldCache stores the full pair read from YouTrack.
func (s *Store) PutYouTrackFieldCache(ctx context.Context, x YouTrackFieldCache) error {
	if x.TaskID < 1 || !validName(x.Lane) {
		return ErrYouTrackActivityInput
	}
	if x.Source == "" {
		x.Source = YouTrackSourceYouTrack
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO youtrack_field_cache(task_id,priority,lane,source,fetched_at,stale)
		VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT(task_id) DO UPDATE SET priority=EXCLUDED.priority,lane=EXCLUDED.lane,source=EXCLUDED.source,fetched_at=EXCLUDED.fetched_at,stale=EXCLUDED.stale`,
		x.TaskID, x.Priority, x.Lane, x.Source, x.FetchedAt.UTC(), x.Stale)
	return err
}

// SetYouTrackCachedPriority records an hk-originated priority write.
func (s *Store) SetYouTrackCachedPriority(ctx context.Context, taskID int64, priority int, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO youtrack_field_cache(task_id,priority,lane,source,fetched_at,stale)
		VALUES($1,$2,COALESCE((SELECT lane FROM tasks WHERE id=$1),''),$3,$4,false)
		ON CONFLICT(task_id) DO UPDATE SET priority=EXCLUDED.priority,source=EXCLUDED.source,fetched_at=EXCLUDED.fetched_at,stale=false`,
		taskID, priority, YouTrackSourceHK, at.UTC())
	return err
}

func (s *Store) SetYouTrackCachedLane(ctx context.Context, taskID int64, lane string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO youtrack_field_cache(task_id,priority,lane,source,fetched_at,stale)
		VALUES($1,COALESCE((SELECT priority FROM tasks WHERE id=$1),0),$2,$3,$4,false)
		ON CONFLICT(task_id) DO UPDATE SET lane=EXCLUDED.lane,source=EXCLUDED.source,fetched_at=EXCLUDED.fetched_at,stale=false`,
		taskID, lane, YouTrackSourceHK, at.UTC())
	return err
}

func (s *Store) MarkYouTrackFieldCacheStale(ctx context.Context, taskID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE youtrack_field_cache SET stale=true WHERE task_id=$1`, taskID)
	return err
}

// RefreshYouTrackFields returns mirrored tasks ordered by cache age. The sync
// refreshes them; the read path serves the cache so a board poll never fans
// out per task. There is no TTL expiry: a value is refreshed by the sync.
func (s *Store) RefreshYouTrackFields(ctx context.Context, limit int) ([]YouTrackMirror, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT o.task_id,o.external_id,COALESCE(o.display_ref,''),COALESCE(o.url,''),o.tenant,o.role,o.synced_at,o.created_at
		FROM task_origins o
		LEFT JOIN youtrack_field_cache c ON c.task_id=o.task_id
		WHERE o.provider='youtrack'
		ORDER BY COALESCE(c.fetched_at, to_timestamp(0)), o.task_id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YouTrackMirror{}
	for rows.Next() {
		var x YouTrackMirror
		if err := scanYouTrackMirror(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// EnqueueYouTrack adds or replaces the pending outbox entry for one field.
// The PG write already committed, so this never blocks on YouTrack.
func (s *Store) EnqueueYouTrack(ctx context.Context, taskID int64, op, value string) error {
	if taskID < 1 || (op != YouTrackOpPriority && op != YouTrackOpLane) || value == "" {
		return ErrYouTrackOutboxInvalid
	}
	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO youtrack_outbox(task_id,op,value,created_at,next_attempt_at)
		VALUES($1,$2,$3,$4,$4)
		ON CONFLICT (task_id,op) WHERE done_at IS NULL DO UPDATE SET
			value=EXCLUDED.value,next_attempt_at=EXCLUDED.next_attempt_at,attempts=0,last_error=''`,
		taskID, op, value, now)
	return err
}

func (s *Store) DueYouTrackOutbox(ctx context.Context, limit int) ([]YouTrackOutboxItem, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id,task_id,op,value,attempts FROM youtrack_outbox WHERE done_at IS NULL AND next_attempt_at<=now() ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YouTrackOutboxItem{}
	for rows.Next() {
		var x YouTrackOutboxItem
		if err := rows.Scan(&x.ID, &x.TaskID, &x.Op, &x.Value, &x.Attempts); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) MarkYouTrackOutboxDone(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE youtrack_outbox SET done_at=now(),last_error='' WHERE id=$1`, id)
	return err
}

func (s *Store) RescheduleYouTrackOutbox(ctx context.Context, id int64, attempts int, next time.Time, lastError string) error {
	_, err := s.pool.Exec(ctx, `UPDATE youtrack_outbox SET attempts=$2,next_attempt_at=$3,last_error=$4 WHERE id=$1`, id, attempts, next.UTC(), truncateText(lastError, 512))
	return err
}

func truncateText(v string, max int) string {
	if len(v) <= max {
		return v
	}
	return v[:max]
}

// DueYouTrackPushTasks returns the bounded push batch: every unmirrored task
// that is not settled, every mirrored task whose revision moved since its
// last sync, and every such task with an hk comment that has not been
// mirrored.
func (s *Store) DueYouTrackPushTasks(ctx context.Context, limit int) ([]Task, error) {
	if limit < 1 {
		return nil, nil
	}
	// taskColumns carries unqualified names, so project the tasks first and
	// join the mirror; task_origins.created_at would otherwise be ambiguous.
	rows, err := s.pool.Query(ctx, `
		SELECT tasks.*
		FROM (SELECT `+taskColumns+` FROM tasks) tasks
		LEFT JOIN task_origins o ON o.task_id=tasks.id AND o.provider='youtrack'
		WHERE (o.task_id IS NULL AND tasks.state NOT IN (`+settledStatesSQL+`))
		   OR (o.task_id IS NOT NULL AND (o.synced_at IS NULL OR tasks.updated_at > o.synced_at))
		   OR ((o.task_id IS NOT NULL OR tasks.state NOT IN (`+settledStatesSQL+`))
		       AND EXISTS (SELECT 1 FROM task_comments c
		                   WHERE c.task_id=tasks.id AND c.author<>$2
		                     AND NOT EXISTS (SELECT 1 FROM youtrack_comment_links cl WHERE cl.hk_comment_id=c.id)))
		ORDER BY tasks.updated_at ASC, tasks.id ASC
		LIMIT $1`, limit, YouTrackOwnedActor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var x Task
		if err := scanTask(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// UnpushedYouTrackComments lists hk comments with no mirror link, in id order.
func (s *Store) UnpushedYouTrackComments(ctx context.Context, taskID, afterID int64, limit int) ([]TaskComment, error) {
	if taskID < 1 || limit < 1 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.id,c.task_id,c.body,c.author,c.created_at
		FROM task_comments c
		WHERE c.task_id=$1 AND c.id>$2 AND c.author<>$3
		  AND NOT EXISTS (SELECT 1 FROM youtrack_comment_links l WHERE l.hk_comment_id=c.id)
		ORDER BY c.id ASC LIMIT $4`, taskID, afterID, YouTrackOwnedActor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskComment{}
	for rows.Next() {
		var x TaskComment
		if err := scanTaskComment(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
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

func (s *Store) FindYouTrackCommentLinkByYouTrackID(ctx context.Context, youtrackCommentID string) (YouTrackCommentLink, bool, error) {
	var x YouTrackCommentLink
	err := s.pool.QueryRow(ctx, `SELECT hk_comment_id,task_id,youtrack_comment_id,created_at FROM youtrack_comment_links WHERE youtrack_comment_id=$1`, youtrackCommentID).Scan(&x.HKCommentID, &x.TaskID, &x.YouTrackCommentID, &x.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return YouTrackCommentLink{}, false, nil
	}
	return x, err == nil, err
}

// PendingYouTrackReverts lists refused activities whose YouTrack field has
// not been put back yet.
func (s *Store) PendingYouTrackReverts(ctx context.Context, limit int) ([]YouTrackPendingRevert, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT activity_id,issue_id,revert_field,revert_value,refusal FROM youtrack_processed_activities WHERE NOT revert_done ORDER BY processed_at,activity_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YouTrackPendingRevert{}
	for rows.Next() {
		var x YouTrackPendingRevert
		if err := rows.Scan(&x.ActivityID, &x.IssueID, &x.RevertField, &x.RevertValue, &x.Refusal); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) MarkYouTrackRevertDone(ctx context.Context, activityID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE youtrack_processed_activities SET revert_done=true WHERE activity_id=$1`, activityID)
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

// ApplyYouTrackActivity applies one activity exactly once. The processed row,
// the effect and the cursor's last activity id commit together; a refused
// transition rolls its own savepoint back, records the Korean reason, and is
// reverted in YouTrack by the caller.
func (s *Store) ApplyYouTrackActivity(ctx context.Context, in YouTrackActivity) (YouTrackApplyResult, error) {
	if strings.TrimSpace(in.ID) == "" || (in.TaskID < 1 && in.NewIssue == nil && in.Skip == "") {
		return YouTrackApplyResult{}, ErrYouTrackActivityInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return YouTrackApplyResult{}, err
	}
	defer tx.Rollback(ctx)
	// Level 2 first: the transition and relane helpers below take it, and
	// taking it after a task row lock would invert the package's lock order.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return YouTrackApplyResult{}, err
	}
	// The comment insert lock is taken before the task row: CreateTaskComment
	// takes it first and then waits for the FK's KEY SHARE on the task row, so
	// holding the row first would cycle.
	if in.Comment != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, taskCommentInsertLock); err != nil {
			return YouTrackApplyResult{}, err
		}
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO youtrack_processed_activities(activity_id,outcome,task_id,issue_id,revert_done,processed_at)
		VALUES($1,$2,$3,$4,true,$5)
		ON CONFLICT(activity_id) DO NOTHING`, in.ID, YouTrackOutcomeDuplicate, nullInt64(in.TaskID), in.IssueID, time.Now().UTC())
	if err != nil {
		return YouTrackApplyResult{}, err
	}
	if tag.RowsAffected() == 0 {
		return YouTrackApplyResult{Applied: false, Outcome: YouTrackOutcomeDuplicate}, tx.Commit(ctx)
	}

	if in.Skip != "" {
		if _, err := tx.Exec(ctx, `UPDATE youtrack_processed_activities SET outcome=$2,refusal=$3 WHERE activity_id=$1`, in.ID, YouTrackOutcomeSkipped, in.Skip); err != nil {
			return YouTrackApplyResult{}, err
		}
		if err := noteActivityProcessed(ctx, tx, in.ID); err != nil {
			return YouTrackApplyResult{}, err
		}
		return YouTrackApplyResult{Applied: true, Outcome: YouTrackOutcomeSkipped}, tx.Commit(ctx)
	}

	out := YouTrackApplyResult{Applied: true, Outcome: YouTrackOutcomeApplied}
	if in.NewIssue != nil {
		taskID, err := s.createYouTrackIssueTx(ctx, tx, *in.NewIssue)
		if err != nil {
			return YouTrackApplyResult{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE youtrack_processed_activities SET outcome=$2,task_id=$3,issue_id=$4 WHERE activity_id=$1`, in.ID, out.Outcome, taskID, in.IssueID); err != nil {
			return YouTrackApplyResult{}, err
		}
		if err := noteActivityProcessed(ctx, tx, in.ID); err != nil {
			return YouTrackApplyResult{}, err
		}
		return out, tx.Commit(ctx)
	}

	var x Task
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, in.TaskID), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return YouTrackApplyResult{}, ErrYouTrackTaskNotFound
		}
		return YouTrackApplyResult{}, err
	}
	if in.State != "" && in.State != x.State {
		moved, refusal, terr := s.applyYouTrackStateTx(ctx, tx, x, in)
		if terr != nil {
			return YouTrackApplyResult{}, terr
		}
		if refusal != "" {
			out.Outcome, out.Refusal = YouTrackOutcomeRejected, refusal
			out.RevertField, out.RevertValue = "state", x.State
		} else {
			x = moved
		}
	}

	if in.Priority != nil && *in.Priority != x.Priority {
		if *in.Priority < 0 {
			if out.Outcome != YouTrackOutcomeRejected {
				out.Outcome, out.Refusal = YouTrackOutcomeRejected, "우선순위는 0 이상이어야 합니다"
				out.RevertField, out.RevertValue = "priority", fmt.Sprint(x.Priority)
			}
		} else {
			now := time.Now().UTC()
			if _, err := tx.Exec(ctx, `UPDATE tasks SET priority=$2,updated_at=$3 WHERE id=$1`, x.ID, *in.Priority, now); err != nil {
				return YouTrackApplyResult{}, err
			}
			if _, _, err := insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: "metadata", From: x.State, To: x.State, By: YouTrackOwnedActor, Note: "YouTrack에서 우선순위 변경", Refs: x.Refs, At: now, Source: YouTrackActivitySource, SourceRef: in.ID + "/priority", Field: "priority", FromValue: fmt.Sprint(x.Priority), ToValue: fmt.Sprint(*in.Priority)}); err != nil {
				return YouTrackApplyResult{}, err
			}
			if err := touchMirrorTx(ctx, tx, x.ID); err != nil {
				return YouTrackApplyResult{}, err
			}
			x.Priority = *in.Priority
		}
	}

	if in.Lane != "" && in.Lane != x.Lane {
		moved, changed, lerr := s.relaneTaskTx(ctx, tx, x.ID, in.Lane, YouTrackOwnedActor, "YouTrack에서 레인 변경", true, YouTrackActivitySource, in.ID+"/lane")
		if lerr != nil {
			reason, refused := youtrackLaneRefusal(lerr)
			if !refused {
				return YouTrackApplyResult{}, lerr
			}
			if out.Outcome != YouTrackOutcomeRejected {
				out.Outcome, out.Refusal = YouTrackOutcomeRejected, reason
				out.RevertField, out.RevertValue = "lane", x.Lane
			}
		} else if changed {
			if err := touchMirrorTx(ctx, tx, x.ID); err != nil {
				return YouTrackApplyResult{}, err
			}
			x = moved
		}
	}

	if in.Comment != "" {
		reason, cerr := s.applyYouTrackCommentTx(ctx, tx, x.ID, in.Comment, in.CommentID)
		if cerr != nil {
			return YouTrackApplyResult{}, cerr
		}
		if reason != "" && out.Outcome != YouTrackOutcomeRejected {
			out.Outcome, out.Refusal = YouTrackOutcomeRejected, reason
		}
	}

	if out.Outcome == YouTrackOutcomeRejected {
		if _, err := tx.Exec(ctx, `UPDATE youtrack_processed_activities SET outcome=$2,task_id=$3,issue_id=$4,revert_field=$5,revert_value=$6,refusal=$7,revert_done=false WHERE activity_id=$1`, in.ID, out.Outcome, x.ID, in.IssueID, out.RevertField, out.RevertValue, out.Refusal); err != nil {
			return YouTrackApplyResult{}, err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE youtrack_processed_activities SET outcome=$2,task_id=$3 WHERE activity_id=$1`, in.ID, out.Outcome, x.ID); err != nil {
		return YouTrackApplyResult{}, err
	}
	if err := noteActivityProcessed(ctx, tx, in.ID); err != nil {
		return YouTrackApplyResult{}, err
	}
	return out, tx.Commit(ctx)
}

// applyYouTrackStateTx runs one hk state move behind a savepoint. A refusal
// rolls the savepoint back so nothing of the move lands and the activity can
// still record its other effects.
func (s *Store) applyYouTrackStateTx(ctx context.Context, tx pgx.Tx, x Task, in YouTrackActivity) (Task, string, error) {
	reopen := false
	if IsSettledState(x.State) {
		if in.State != StateUnstarted {
			return Task{}, "정리된 작업은 착수 전으로만 되돌릴 수 있습니다", nil
		}
		reopen = true
	}
	tin := TransitionInput{
		To:        in.State,
		By:        YouTrackOwnedActor,
		Note:      "YouTrack에서 상태 변경",
		Evidence:  x.Refs.PR,
		Source:    YouTrackActivitySource,
		SourceRef: in.ID + "/state",
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT youtrack_state`); err != nil {
		return Task{}, "", err
	}
	moved, err := s.transitionTaskTx(ctx, tx, x.ID, tin, reopen)
	if err == nil {
		if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT youtrack_state`); err != nil {
			return Task{}, "", err
		}
		// The state is now on both sides; do not echo it back on the next push.
		if err := touchMirrorTx(ctx, tx, x.ID); err != nil {
			return Task{}, "", err
		}
		return moved, "", nil
	}
	if reason, ok := youtrackRefusal(err); ok {
		if _, rerr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT youtrack_state`); rerr != nil {
			return Task{}, "", rerr
		}
		return Task{}, reason, nil
	}
	return Task{}, "", err
}

func (s *Store) applyYouTrackCommentTx(ctx context.Context, tx pgx.Tx, taskID int64, body, youtrackCommentID string) (string, error) {
	if err := CheckTaskCommentBody(body); err != nil {
		switch {
		case errors.Is(err, ErrTaskCommentTooLong):
			return "코멘트가 너무 길어 hk에 저장하지 않았습니다", nil
		case errors.Is(err, ErrTaskCommentEmpty):
			return "빈 코멘트는 hk에 저장하지 않았습니다", nil
		default:
			return "코멘트가 비밀값으로 보여 hk에 저장하지 않았습니다", nil
		}
	}
	var comment TaskComment
	now := time.Now().UTC()
	err := scanTaskComment(tx.QueryRow(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,$2,$3,$4) RETURNING `+taskCommentColumns, taskID, body, YouTrackOwnedActor, now), &comment)
	if err != nil {
		return "", err
	}
	if youtrackCommentID != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO youtrack_comment_links(hk_comment_id,task_id,youtrack_comment_id,created_at) VALUES($1,$2,$3,$4) ON CONFLICT(hk_comment_id) DO NOTHING`, comment.ID, taskID, youtrackCommentID, now); err != nil {
			return "", err
		}
	}
	return "", nil
}

func (s *Store) createYouTrackIssueTx(ctx context.Context, tx pgx.Tx, in YouTrackNewIssue) (int64, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" || !validText(title, MaxBytes) || !validName(in.Lane) || !taskKinds[in.Kind] || strings.TrimSpace(in.Tenant) == "" {
		return 0, ErrYouTrackActivityInput
	}
	if err := requireTaskProject(ctx, tx, &in.Project); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var x Task
	err := scanTask(tx.QueryRow(ctx, `
		INSERT INTO tasks(lane,parent_lane,title,kind,state,priority,refs,claimed_by,created_by,created_at,updated_at,last_activity_at,body_doc,project,area,expected,parent_id)
		VALUES($1,'',$2,$3,$4,$5,'{}'::jsonb,'',$6,$7,$7,$7,'',$8,NULL,NULL,NULL)
		RETURNING `+taskColumns, in.Lane, title, in.Kind, StateTriage, in.Priority, YouTrackOwnedActor, now, in.Project), &x)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_origins(task_id,provider,namespace,external_id,display_ref,url,title_snapshot,snapshot_at,created_by,created_at,tenant,role,synced_at)
		VALUES($1,'youtrack',$2,$3,$4,'',NULL,$5,$6,$5,$2,'origin',$5)
		ON CONFLICT (task_id) WHERE provider='youtrack' DO NOTHING`, x.ID, in.Tenant, in.IssueID, in.ReadableID, now, YouTrackSyncActor); err != nil {
		return 0, err
	}
	return x.ID, nil
}

func touchMirrorTx(ctx context.Context, tx pgx.Tx, taskID int64) error {
	_, err := tx.Exec(ctx, `UPDATE task_origins SET synced_at=(SELECT updated_at FROM tasks WHERE id=$1) WHERE provider='youtrack' AND task_id=$1`, taskID)
	return err
}

func noteActivityProcessed(ctx context.Context, tx pgx.Tx, activityID string) error {
	_, err := tx.Exec(ctx, `UPDATE youtrack_cursor SET last_activity_id=$1,updated_at=now() WHERE singleton`, activityID)
	return err
}

func nullInt64(v int64) any {
	if v < 1 {
		return nil
	}
	return v
}

// youtrackRefusal turns the store's validation sentinels into a short Korean
// reason. Anything else is a real failure, not a refusal.
func youtrackRefusal(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrClosureInvalid):
		return "종료 근거가 없어 hk가 상태를 바꾸지 않았습니다", true
	case errors.Is(err, ErrTaskJobRequired):
		return "작업을 시작하려면 claim과 job 연결이 필요합니다", true
	case errors.Is(err, ErrDuplicateOfRequired):
		return "중복 작업은 대상 작업이 필요합니다", true
	case errors.Is(err, ErrDecisionRequestOpen):
		return "열린 결정 요청이 있어 상태를 바꿀 수 없습니다", true
	case errors.Is(err, ErrDispositionOperatorOnly):
		return "운영자 답변으로만 처리할 수 있는 작업입니다", true
	case errors.Is(err, ErrTaskConflict):
		return "hk의 전이 규칙이 이 상태 변경을 허용하지 않습니다", true
	case errors.Is(err, ErrTaskTerminal):
		return "이미 종료된 작업입니다", true
	}
	var denied wrkrecords.Error
	if errors.As(err, &denied) {
		return "hk 기록 잠금이 이 상태 변경을 거부했습니다", true
	}
	return "", false
}

func youtrackLaneRefusal(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrTaskTerminal):
		return "이미 종료된 작업의 레인은 바꿀 수 없습니다", true
	case errors.Is(err, ErrTaskNotFound):
		return "hk에서 작업을 찾지 못했습니다", true
	case errors.Is(err, ErrTaskLaneUnknown):
		return "hk에 없는 레인입니다", true
	case errors.Is(err, ErrTaskConflict):
		return "hk가 레인 변경을 거부했습니다", true
	}
	var denied wrkrecords.Error
	if errors.As(err, &denied) {
		return "hk 기록 잠금이 레인 변경을 거부했습니다", true
	}
	return "", false
}
