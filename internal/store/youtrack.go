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

// YouTrack is the operator's screen; hk is the record agents use. The tables
// in migration_youtrack.go carry the minimum hk needs to keep the mirror
// current and to serve YouTrack-owned reads without a round trip per task.

var (
	ErrYouTrackLinkConflict   = errors.New("youtrack_link_conflict")
	ErrYouTrackActivityInput  = errors.New("youtrack_activity_invalid")
	ErrYouTrackSyncLocked     = errors.New("youtrack_sync_locked")
	ErrYouTrackTaskNotFound   = errors.New("youtrack_task_not_found")
	ErrYouTrackIssueNotLinked = errors.New("youtrack_issue_not_linked")
)

// YouTrackOwnedActor recognises writes that came from YouTrack rather than an
// hk session. It is the value stored in task_events.by for those writes.
const YouTrackOwnedActor = "operator:youtrack"

// YouTrackActivitySource marks task_events rows produced by the activities
// loop. source_ref carries the activity id plus the effect, so one activity
// that changes two fields records two distinct events.
const YouTrackActivitySource = "youtrack.activity"

type YouTrackLink struct {
	TaskID              int64      `json:"task_id"`
	IssueID             string     `json:"issue_id"`
	ReadableID          string     `json:"readable_id"`
	PushedTaskUpdatedAt *time.Time `json:"pushed_task_updated_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
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
	FetchedAt time.Time `json:"fetched_at"`
	Stale     bool      `json:"stale"`
}

// YouTrackNewIssue is one issue the operator filed in YouTrack that hk has
// not seen. The engine resolves the display values; the store owns the task
// row and the link.
type YouTrackNewIssue struct {
	IssueID    string
	ReadableID string
	Title      string
	Lane       string
	Kind       string
	Project    string
	Priority   int
}

// YouTrackActivity is one activitiesPage entry reduced to what hk applies.
// Exactly one of State, Priority, Lane, Comment, NewIssue or Skip is set (a
// custom-field activity may carry both Priority and Lane).
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

// YouTrackApplyOutcome is the closed vocabulary stored for one activity.
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
	// RevertField and RevertValue name the YouTrack-owned field to put back
	// when hk refuses the change (state, priority or lane).
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

const youtrackLinkColumns = `task_id,issue_id,readable_id,pushed_task_updated_at,created_at,updated_at`

func scanYouTrackLink(row interface{ Scan(...any) error }, x *YouTrackLink) error {
	return row.Scan(&x.TaskID, &x.IssueID, &x.ReadableID, &x.PushedTaskUpdatedAt, &x.CreatedAt, &x.UpdatedAt)
}

func (s *Store) GetYouTrackLink(ctx context.Context, taskID int64) (YouTrackLink, bool, error) {
	var x YouTrackLink
	err := scanYouTrackLink(s.pool.QueryRow(ctx, `SELECT `+youtrackLinkColumns+` FROM youtrack_links WHERE task_id=$1`, taskID), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return YouTrackLink{}, false, nil
	}
	return x, err == nil, err
}

func (s *Store) FindYouTrackLinkByReadableID(ctx context.Context, readableID string) (YouTrackLink, bool, error) {
	var x YouTrackLink
	err := scanYouTrackLink(s.pool.QueryRow(ctx, `SELECT `+youtrackLinkColumns+` FROM youtrack_links WHERE readable_id=$1`, readableID), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return YouTrackLink{}, false, nil
	}
	return x, err == nil, err
}

func (s *Store) ListYouTrackLinks(ctx context.Context, taskIDs []int64) (map[int64]YouTrackLink, error) {
	out := map[int64]YouTrackLink{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+youtrackLinkColumns+` FROM youtrack_links WHERE task_id = ANY($1)`, taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x YouTrackLink
		if err := scanYouTrackLink(rows, &x); err != nil {
			return nil, err
		}
		out[x.TaskID] = x
	}
	return out, rows.Err()
}

// LinkYouTrackIssue records (task, issue) both ways. It is idempotent for the
// same pair and refuses a pair that would move an existing link.
func (s *Store) LinkYouTrackIssue(ctx context.Context, taskID int64, issueID, readableID string, at time.Time) (YouTrackLink, error) {
	if taskID < 1 || strings.TrimSpace(issueID) == "" || strings.TrimSpace(readableID) == "" {
		return YouTrackLink{}, ErrYouTrackLinkConflict
	}
	at = at.UTC()
	var x YouTrackLink
	err := scanYouTrackLink(s.pool.QueryRow(ctx, `
		INSERT INTO youtrack_links(task_id,issue_id,readable_id,created_at,updated_at)
		VALUES($1,$2,$3,$4,$4)
		ON CONFLICT(task_id) DO UPDATE SET issue_id=EXCLUDED.issue_id, readable_id=EXCLUDED.readable_id, updated_at=EXCLUDED.updated_at
		WHERE youtrack_links.issue_id=EXCLUDED.issue_id AND youtrack_links.readable_id=EXCLUDED.readable_id
		RETURNING `+youtrackLinkColumns, taskID, issueID, readableID, at), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return YouTrackLink{}, ErrYouTrackLinkConflict
	}
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return YouTrackLink{}, ErrYouTrackLinkConflict
		}
		return YouTrackLink{}, err
	}
	return x, nil
}

// MarkYouTrackTaskPushed records the task revision the last successful push
// covered. A nil-safe no-op for an unlinked task.
func (s *Store) MarkYouTrackTaskPushed(ctx context.Context, taskID int64, updatedAt time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE youtrack_links SET pushed_task_updated_at=$2,updated_at=now() WHERE task_id=$1`, taskID, updatedAt.UTC())
	return err
}

// TouchYouTrackTaskPushed marks the task's current revision as pushed. A
// write-through or an applied YouTrack change has already put the same value
// on both sides, so the next push must not repeat it.
func (s *Store) TouchYouTrackTaskPushed(ctx context.Context, taskID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE youtrack_links SET pushed_task_updated_at=(SELECT updated_at FROM tasks WHERE id=$1),updated_at=now() WHERE task_id=$1`, taskID)
	return err
}

// TaskRouting reads only the lane and priority a write-through needs to
// restore YouTrack when the hk write is refused. The full task read loads
// events and children and is too heavy for a 500-item relane batch.
func (s *Store) TaskRouting(ctx context.Context, id int64) (lane string, priority int, found bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT lane,priority FROM tasks WHERE id=$1`, id).Scan(&lane, &priority)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false, nil
	}
	return lane, priority, err == nil, err
}

// TaskProjectExists reports whether name is in the server's project
// vocabulary. The sync uses it to keep an operator's mistake from failing a
// whole activity.
func (s *Store) TaskProjectExists(ctx context.Context, name string) (bool, error) {
	var known bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_projects WHERE name=$1)`, name).Scan(&known)
	return known, err
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
	rows, err := s.pool.Query(ctx, `SELECT task_id,priority,lane,fetched_at,stale FROM youtrack_field_cache WHERE task_id = ANY($1)`, taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x YouTrackFieldCache
		if err := rows.Scan(&x.TaskID, &x.Priority, &x.Lane, &x.FetchedAt, &x.Stale); err != nil {
			return nil, err
		}
		out[x.TaskID] = x
	}
	return out, rows.Err()
}

func (s *Store) GetYouTrackFieldCache(ctx context.Context, taskID int64) (YouTrackFieldCache, bool, error) {
	var x YouTrackFieldCache
	err := s.pool.QueryRow(ctx, `SELECT task_id,priority,lane,fetched_at,stale FROM youtrack_field_cache WHERE task_id=$1`, taskID).Scan(&x.TaskID, &x.Priority, &x.Lane, &x.FetchedAt, &x.Stale)
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
	_, err := s.pool.Exec(ctx, `
		INSERT INTO youtrack_field_cache(task_id,priority,lane,fetched_at,stale)
		VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(task_id) DO UPDATE SET priority=EXCLUDED.priority,lane=EXCLUDED.lane,fetched_at=EXCLUDED.fetched_at,stale=EXCLUDED.stale`,
		x.TaskID, x.Priority, x.Lane, x.FetchedAt.UTC(), x.Stale)
	return err
}

// SetYouTrackCachedPriority updates only the priority half of the pair. The
// lane stays as cached (or the task's own lane on the first write).
func (s *Store) SetYouTrackCachedPriority(ctx context.Context, taskID int64, priority int, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO youtrack_field_cache(task_id,priority,lane,fetched_at,stale)
		VALUES($1,$2,COALESCE((SELECT lane FROM tasks WHERE id=$1),''),$3,false)
		ON CONFLICT(task_id) DO UPDATE SET priority=EXCLUDED.priority,fetched_at=EXCLUDED.fetched_at,stale=false`,
		taskID, priority, at.UTC())
	return err
}

func (s *Store) SetYouTrackCachedLane(ctx context.Context, taskID int64, lane string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO youtrack_field_cache(task_id,priority,lane,fetched_at,stale)
		VALUES($1,COALESCE((SELECT priority FROM tasks WHERE id=$1),0),$2,$3,false)
		ON CONFLICT(task_id) DO UPDATE SET lane=EXCLUDED.lane,fetched_at=EXCLUDED.fetched_at,stale=false`,
		taskID, lane, at.UTC())
	return err
}

func (s *Store) MarkYouTrackFieldCacheStale(ctx context.Context, taskID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE youtrack_field_cache SET stale=true WHERE task_id=$1`, taskID)
	return err
}

// RefreshYouTrackFields returns the linked tasks whose cache row is missing,
// stale, or older than ttl, oldest first. The sync refreshes these; the read
// path serves the cache so a board poll never fans out per task.
func (s *Store) RefreshYouTrackFields(ctx context.Context, ttl time.Duration, limit int) ([]YouTrackLink, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT l.task_id,l.issue_id,l.readable_id,l.pushed_task_updated_at,l.created_at,l.updated_at
		FROM youtrack_links l
		LEFT JOIN youtrack_field_cache c ON c.task_id=l.task_id
		WHERE c.task_id IS NULL OR c.stale OR c.fetched_at < now() - make_interval(secs => $1::double precision)
		ORDER BY COALESCE(c.fetched_at, to_timestamp(0)), l.task_id
		LIMIT $2`, int(ttl.Seconds()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YouTrackLink{}
	for rows.Next() {
		var x YouTrackLink
		if err := scanYouTrackLink(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// DueYouTrackPushTasks returns the bounded push batch: every unlinked task
// that is not settled, every linked task whose revision moved since its last
// push, and every such task with an hk comment that has not been mirrored.
func (s *Store) DueYouTrackPushTasks(ctx context.Context, limit int) ([]Task, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+taskColumns+`
		FROM tasks
		LEFT JOIN youtrack_links l ON l.task_id=tasks.id
		WHERE (l.task_id IS NULL AND tasks.state NOT IN (`+settledStatesSQL+`))
		   OR (l.task_id IS NOT NULL AND (l.pushed_task_updated_at IS NULL OR tasks.updated_at > l.pushed_task_updated_at))
		   OR ((l.task_id IS NOT NULL OR tasks.state NOT IN (`+settledStatesSQL+`))
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
// same pair and refuses to point one side at two rows.
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
// not been put back yet. A YouTrack outage between the hk transaction and the
// revert leaves a row here; the next run retries it.
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
	// The comment insert lock is taken before the task row for the same
	// reason: CreateTaskComment takes it first and then waits for the FK's
	// KEY SHARE on the task row, so holding the row first would cycle.
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
			if err := touchLinkTx(ctx, tx, x.ID); err != nil {
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
			if err := touchLinkTx(ctx, tx, x.ID); err != nil {
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
		if err := touchLinkTx(ctx, tx, x.ID); err != nil {
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
	if title == "" || !validText(title, MaxBytes) || !validName(in.Lane) || !taskKinds[in.Kind] {
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
		INSERT INTO youtrack_links(task_id,issue_id,readable_id,pushed_task_updated_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$4,$4)
		ON CONFLICT(task_id) DO NOTHING`, x.ID, in.IssueID, in.ReadableID, now); err != nil {
		return 0, err
	}
	return x.ID, nil
}

func touchLinkTx(ctx context.Context, tx pgx.Tx, taskID int64) error {
	_, err := tx.Exec(ctx, `UPDATE youtrack_links SET pushed_task_updated_at=(SELECT updated_at FROM tasks WHERE id=$1),updated_at=now() WHERE task_id=$1`, taskID)
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
