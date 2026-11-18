package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// A task's spawn record says where its panewire spawn request stands (hk
// 4377, operator decisions 185 and 188). panewire owns the request; hk keeps
// one row per task, the latest by updated_at, and shows it on the task.
//
// Idempotency: a write whose updated_at is not newer than the stored one is
// ignored and answers the stored record, so a replayed or reordered write
// changes nothing.

// Spawn states, in the order panewire moves a request through them.
const (
	SpawnWaiting     = "waiting"
	SpawnPending     = "pending"
	SpawnParked      = "parked"
	SpawnBound       = "bound"
	SpawnLaunching   = "launching"
	SpawnReconciling = "reconciling"
	SpawnLanded      = "landed"
	SpawnRefused     = "refused"
	SpawnGivenUp     = "given_up"
	SpawnCancelled   = "cancelled"
)

// spawnStateList is the wire order of the states, quoted in the refusal
// reason for an unknown state.
var spawnStateList = []string{SpawnWaiting, SpawnPending, SpawnParked, SpawnBound, SpawnLaunching, SpawnReconciling, SpawnLanded, SpawnRefused, SpawnGivenUp, SpawnCancelled}

// TaskStatesMax bounds one GET /v1/tasks/states query, and one
// GET /v1/tasks/states/snapshot query.
const TaskStatesMax = 200

// spawnMaxClockSkew bounds how far ahead of this server's clock a record's
// updated_at may be. A record stamped further ahead would win the
// updated_at comparison and block every later write until that time, so the
// bound caps the damage a bad sender can do to an hour.
const spawnMaxClockSkew = time.Hour

// ErrSpawnInvalid refuses a spawn record or a states query the store cannot
// accept. Its text is the wire code (TestErrorRuleCodesMatchSentinels); the
// reason after "invalid_context: " is store-authored and is returned to the
// caller.
var ErrSpawnInvalid = errors.New("invalid_context")

// spawnTerminal reports whether state ends a spawn request: a terminal
// transition writes the task's comment once.
func spawnTerminal(state string) bool {
	switch state {
	case SpawnLanded, SpawnRefused, SpawnGivenUp, SpawnCancelled:
		return true
	}
	return false
}

// spawnConditionMet reports whether a task in state has reached the level a
// start condition waits for: merged is satisfied by merged or deployed,
// deployed only by deployed.
func spawnConditionMet(until, state string) bool {
	switch until {
	case "merged":
		return state == StateMerged || state == StateDeployed
	case "deployed":
		return state == StateDeployed
	}
	return false
}

func spawnInvalid(reason string) error { return fmt.Errorf("%w: %s", ErrSpawnInvalid, reason) }

// TaskSpawn is a task's spawn request record as the server shows it.
type TaskSpawn struct {
	RequestID      string          `json:"request_id"`
	Job            string          `json:"job"`
	Role           string          `json:"role"`
	State          string          `json:"state"`
	Reason         string          `json:"reason"`
	Attempt        int             `json:"attempt"`
	Host           string          `json:"host"`
	NextCheckAt    *time.Time      `json:"next_check_at"`
	ExpiresAt      *time.Time      `json:"expires_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	StartCondition *TaskSpawnStart `json:"start_condition"`
}

// TaskSpawnStart is the condition a queued request waits for: the blocking
// task reaching Until. TaskState is that task's current state and Met says
// whether the condition holds now, both read when the record is shown.
type TaskSpawnStart struct {
	TaskID    int64  `json:"task_id"`
	Until     string `json:"until"`
	TaskState string `json:"task_state"`
	Met       bool   `json:"met"`
}

// TaskSpawnStartInput is the start condition a PUT names.
type TaskSpawnStartInput struct {
	TaskID int64  `json:"task_id"`
	Until  string `json:"until"`
}

// TaskSpawnInput is the body of PUT /v1/tasks/{id}/spawn.
type TaskSpawnInput struct {
	RequestID      string               `json:"request_id"`
	Job            string               `json:"job"`
	Role           string               `json:"role"`
	State          string               `json:"state"`
	Reason         string               `json:"reason"`
	Attempt        int                  `json:"attempt"`
	Host           string               `json:"host"`
	NextCheckAt    *time.Time           `json:"next_check_at"`
	ExpiresAt      *time.Time           `json:"expires_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
	StartCondition *TaskSpawnStartInput `json:"start_condition,omitempty"`
}

// checkTaskSpawnInput refuses a record the table cannot hold. Every refusal
// is ErrSpawnInvalid except a secret-like reason, which guard.Reject answers.
func checkTaskSpawnInput(in TaskSpawnInput) error {
	switch {
	case in.RequestID == "" || !validText(in.RequestID, 128):
		return spawnInvalid("request_id is required, at most 128 bytes")
	case in.Job == "" || !validText(in.Job, 128):
		return spawnInvalid("job is required, at most 128 bytes")
	case !validText(in.Role, 64):
		return spawnInvalid("role is at most 64 bytes")
	case !validText(in.Host, 128):
		return spawnInvalid("host is at most 128 bytes")
	case in.Attempt < 1 || in.Attempt > math.MaxInt32:
		return spawnInvalid("attempt must be 1 to 2147483647")
	case in.UpdatedAt.IsZero():
		return spawnInvalid("updated_at is required")
	case in.UpdatedAt.After(time.Now().Add(spawnMaxClockSkew)):
		return spawnInvalid("updated_at is more than an hour ahead of the server clock")
	case !spawnKnown(in.State):
		return spawnInvalid("state is not a spawn state; want one of " + strings.Join(spawnStateList, ", "))
	case !validText(in.Reason, 1024):
		return spawnInvalid("reason is at most 1024 bytes")
	}
	if in.StartCondition != nil {
		if in.StartCondition.TaskID < 1 {
			return spawnInvalid("start_condition.task_id must name a task")
		}
		if in.StartCondition.Until != "merged" && in.StartCondition.Until != "deployed" {
			return spawnInvalid("start_condition.until must be merged or deployed")
		}
	}
	// A terminal write's notice comment carries request_id and job, so they
	// are guarded with the reason.
	for _, text := range []string{in.RequestID, in.Job, in.Reason} {
		if err := guard.Reject(text); err != nil {
			return err
		}
	}
	return nil
}

func spawnKnown(state string) bool {
	for _, s := range spawnStateList {
		if s == state {
			return true
		}
	}
	return false
}

// PutTaskSpawn records the spawn request of task id, written by author. It
// answers the stored record and whether this write applied. A write whose
// updated_at is not newer than the stored one applies nothing. The first
// applied write into a terminal state writes one task comment in the same
// transaction, keyed by task, request, attempt and state.
func (s *Store) PutTaskSpawn(ctx context.Context, id int64, in TaskSpawnInput, author string) (TaskSpawn, bool, error) {
	if id < 1 {
		return TaskSpawn{}, false, ErrTaskNotFound
	}
	if author == "" || !validText(author, 128) {
		return TaskSpawn{}, false, ErrTaskCommentInvalid
	}
	if err := checkTaskSpawnInput(in); err != nil {
		return TaskSpawn{}, false, err
	}
	in.UpdatedAt = in.UpdatedAt.UTC()
	var out TaskSpawn
	var applied bool
	err := s.retryTx(ctx, func() error {
		var err error
		out, applied, err = s.putTaskSpawnOnce(ctx, id, in, author)
		return err
	})
	if err != nil {
		return TaskSpawn{}, false, err
	}
	return out, applied, nil
}

// putTaskSpawnOnce is one whole PutTaskSpawn transaction (PutTaskSpawn retries
// it once on MySQL after a deadlock or lock-wait timeout).
func (s *Store) putTaskSpawnOnce(ctx context.Context, id int64, in TaskSpawnInput, author string) (TaskSpawn, bool, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return TaskSpawn{}, false, err
	}
	defer release()
	defer tx.Rollback(ctx)
	// A terminal write may comment, so it takes the comment insert lock before
	// its task row, the order ReplyDots uses (see
	// lockorder.go). Holding the task row first would cycle with them.
	if spawnTerminal(in.State) {
		if err := s.lockTx(ctx, tx, taskCommentInsertLock); err != nil {
			return TaskSpawn{}, false, err
		}
	}
	// One task row, read under a key share lock so the task cannot be deleted
	// while its spawn record is written. Locking no second task row keeps this
	// transaction below the task-mutation lock (lockorder.go level 2).
	var taskState string
	err = tx.QueryRow(ctx, s.q(`SELECT state FROM tasks WHERE id=$1 FOR KEY SHARE`, `SELECT state FROM tasks WHERE id=? FOR SHARE`), id).Scan(&taskState)
	if errors.Is(err, ErrNoRows) {
		return TaskSpawn{}, false, ErrTaskNotFound
	}
	if err != nil {
		return TaskSpawn{}, false, err
	}
	var startID *int64
	var startUntil *string
	if c := in.StartCondition; c != nil {
		if c.TaskID == id {
			return TaskSpawn{}, false, spawnInvalid("start_condition.task_id names the task itself")
		}
		// A plain read: the blocking task is not locked here.
		var exists bool
		if err := tx.QueryRow(ctx, s.q(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)`), c.TaskID).Scan(&exists); err != nil {
			return TaskSpawn{}, false, err
		}
		if !exists {
			return TaskSpawn{}, false, spawnInvalid("start_condition.task_id names no task")
		}
		startID = &c.TaskID
		startUntil = &c.Until
	}
	var applied bool
	if s.dialect == mysqlDialect {
		applied, err = putTaskSpawnRecordMySQL(ctx, tx, id, in, startID, startUntil)
	} else {
		var tag int64
		tag, err = tx.Exec(ctx, `INSERT INTO task_spawn_records(task_id,request_id,job,role,state,reason,attempt,host,next_check_at,expires_at,updated_at,start_task_id,start_until)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT(task_id) DO UPDATE SET request_id=EXCLUDED.request_id, job=EXCLUDED.job, role=EXCLUDED.role, state=EXCLUDED.state, reason=EXCLUDED.reason, attempt=EXCLUDED.attempt, host=EXCLUDED.host, next_check_at=EXCLUDED.next_check_at, expires_at=EXCLUDED.expires_at, updated_at=EXCLUDED.updated_at, start_task_id=EXCLUDED.start_task_id, start_until=EXCLUDED.start_until
WHERE task_spawn_records.updated_at < EXCLUDED.updated_at`,
			id, in.RequestID, in.Job, in.Role, in.State, in.Reason, in.Attempt, in.Host, in.NextCheckAt, in.ExpiresAt, in.UpdatedAt, startID, startUntil)
		applied = tag == 1
	}
	if err != nil {
		return TaskSpawn{}, false, err
	}
	if applied && spawnTerminal(in.State) {
		if err := s.spawnNoticeTx(ctx, tx, id, in, author); err != nil {
			return TaskSpawn{}, false, err
		}
	}
	stored, _, err := readTaskSpawn(ctx, tx, id)
	if err != nil {
		return TaskSpawn{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TaskSpawn{}, false, err
	}
	if stored == nil {
		return TaskSpawn{}, false, ErrTaskNotFound
	}
	return *stored, applied, nil
}

// putTaskSpawnRecordMySQL is PutTaskSpawn's upsert on MySQL: the write applies
// only when updated_at is newer than the stored one. ON DUPLICATE KEY UPDATE
// cannot say whether it applied (found-rows counts a matched row as 1, as an
// insert), so the stored row is locked and compared first. A concurrent first
// insert that wins the race makes ours fail as a duplicate: the loop reads the
// committed row and decides again.
func putTaskSpawnRecordMySQL(ctx context.Context, tx transaction, id int64, in TaskSpawnInput, startID *int64, startUntil *string) (bool, error) {
	for attempt := 0; ; attempt++ {
		var newer bool
		err := tx.QueryRow(ctx, `SELECT updated_at < ? FROM task_spawn_records WHERE task_id=? FOR UPDATE`, in.UpdatedAt, id).Scan(&newer)
		switch {
		case err == nil:
			if !newer {
				return false, nil
			}
			_, err = tx.Exec(ctx, `UPDATE task_spawn_records SET request_id=?,job=?,role=?,state=?,reason=?,attempt=?,host=?,next_check_at=?,expires_at=?,updated_at=?,start_task_id=?,start_until=? WHERE task_id=?`,
				in.RequestID, in.Job, in.Role, in.State, in.Reason, in.Attempt, in.Host, in.NextCheckAt, in.ExpiresAt, in.UpdatedAt, startID, startUntil, id)
			return err == nil, err
		case !errors.Is(err, ErrNoRows):
			return false, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO task_spawn_records(task_id,request_id,job,role,state,reason,attempt,host,next_check_at,expires_at,updated_at,start_task_id,start_until)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, in.RequestID, in.Job, in.Role, in.State, in.Reason, in.Attempt, in.Host, in.NextCheckAt, in.ExpiresAt, in.UpdatedAt, startID, startUntil)
		if errors.Is(err, errUniqueViolation) && attempt == 0 {
			continue
		}
		return err == nil, err
	}
}

// spawnNoticeTx writes the task comment for a terminal spawn state, once per
// notice key. The caller holds the comment insert lock (PutTaskSpawn takes it
// before the task row). The notice and comment run in a savepoint: when the
// YouTrack mirror refuses the comment (ErrYTCommentUnsendable, comments
// canonical in YouTrack) the savepoint is rolled back and the comment is
// skipped, so the spawn record itself still applies. A later applied write for
// the same key tries the comment again.
func (s *Store) spawnNoticeTx(ctx context.Context, tx transaction, id int64, in TaskSpawnInput, author string) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer sp.Rollback(ctx)
	if err := s.writeSpawnNoticeTx(ctx, sp, id, in, author); err != nil {
		if errors.Is(err, ErrYTCommentUnsendable) {
			return nil
		}
		return err
	}
	return sp.Commit(ctx)
}

func (s *Store) writeSpawnNoticeTx(ctx context.Context, tx transaction, id int64, in TaskSpawnInput, author string) error {
	key := fmt.Sprintf("%d|%s|%d|%s", id, in.RequestID, in.Attempt, in.State)
	if dialectFor(tx) == mysqlDialect {
		return s.writeSpawnNoticeMySQL(ctx, tx, id, key, in, author)
	}
	n, err := tx.Exec(ctx, `INSERT INTO task_spawn_notices(notice_key,task_id,created_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, key, id, time.Now().UTC())
	if err != nil {
		return err
	}
	if n != 1 {
		return nil
	}
	body := spawnNoticeBody(in)
	if err := CheckTaskCommentBody(body); err != nil {
		return err
	}
	var c TaskComment
	err = scanTaskComment(tx.QueryRow(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,$2,$3,$4) RETURNING `+taskCommentColumns, id, body, author, time.Now().UTC()), &c)
	if err != nil {
		return err
	}
	_, _, err = s.enqueueYTCommentTx(ctx, tx, c)
	return err
}

func spawnNoticeBody(in TaskSpawnInput) string {
	body := fmt.Sprintf("spawn %s request=%s attempt=%d job=%s", in.State, in.RequestID, in.Attempt, in.Job)
	if in.Reason != "" {
		body += " reason=" + in.Reason
	}
	return body
}

// writeSpawnNoticeMySQL is writeSpawnNoticeTx on MySQL. The notice key is the
// primary key, so "already written" is the duplicate-key error of a plain
// INSERT (found-rows makes ON DUPLICATE KEY UPDATE report a matched row as 1,
// like a new one). A failed statement leaves the savepoint usable on MySQL.
func (s *Store) writeSpawnNoticeMySQL(ctx context.Context, tx transaction, id int64, key string, in TaskSpawnInput, author string) error {
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO task_spawn_notices(notice_key,task_id,created_at) VALUES(?,?,?)`, key, id, now); err != nil {
		if errors.Is(err, errUniqueViolation) {
			return nil
		}
		return err
	}
	body := spawnNoticeBody(in)
	if err := CheckTaskCommentBody(body); err != nil {
		return err
	}
	commentID, err := mysqlInsertTaskCommentTx(ctx, tx, id, author, body, now)
	if err != nil {
		return err
	}
	_, _, err = s.enqueueYTCommentTx(ctx, tx, TaskComment{ID: commentID, TaskID: id, Body: body, Author: author, CreatedAt: now})
	return err
}

// spawnRecordSelect reads one task_spawn_records row, its blocking task's
// state (NULL when there is no start condition) in the join below.
const spawnRecordSelect = `r.request_id,r.job,r.role,r.state,r.reason,r.attempt,r.host,r.next_check_at,r.expires_at,r.updated_at,r.start_task_id,r.start_until,b.state`

// attachTaskSpawns sets Spawn on each task that has a record, in one read.
// Reads only; a task without a record keeps Spawn nil.
func attachTaskSpawns(ctx context.Context, q poolQuerier, xs []Task) error {
	index := map[int64]int{}
	ids := make([]int64, 0, len(xs))
	for i, x := range xs {
		index[x.ID] = i
		ids = append(ids, x.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	query, args := `SELECT r.task_id,`+spawnRecordSelect+` FROM task_spawn_records r LEFT JOIN tasks b ON b.id=r.start_task_id WHERE r.task_id=ANY($1)`, []any{ids}
	if dialectFor(q) == mysqlDialect {
		query = `SELECT r.task_id,` + spawnRecordSelect + ` FROM task_spawn_records r LEFT JOIN tasks b ON b.id=r.start_task_id WHERE r.task_id IN (` + placeholders(len(ids)) + `)`
		args = make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID int64
		var sp TaskSpawn
		var startID *int64
		var startUntil, blockerState *string
		if err := rows.Scan(&taskID, &sp.RequestID, &sp.Job, &sp.Role, &sp.State, &sp.Reason, &sp.Attempt, &sp.Host, &sp.NextCheckAt, &sp.ExpiresAt, &sp.UpdatedAt, &startID, &startUntil, &blockerState); err != nil {
			return err
		}
		if startID != nil && startUntil != nil {
			start := &TaskSpawnStart{TaskID: *startID, Until: *startUntil}
			if blockerState != nil {
				start.TaskState = *blockerState
			}
			start.Met = blockerState != nil && spawnConditionMet(start.Until, start.TaskState)
			sp.StartCondition = start
		}
		if i, ok := index[taskID]; ok {
			xs[i].Spawn = &sp
		}
	}
	return rows.Err()
}

// readTaskSpawn reads the spawn record of task id, nil when it has none.
// The bool is true when the task exists.
func readTaskSpawn(ctx context.Context, q poolQuerier, id int64) (*TaskSpawn, bool, error) {
	xs := []Task{{ID: id}}
	if err := attachTaskSpawns(ctx, q, xs); err != nil {
		return nil, false, err
	}
	return xs[0].Spawn, true, nil
}

// TaskStateEntry is one row of GET /v1/tasks/states: the task's state and
// category, the open tasks that block it, and (when a condition applies)
// whether that condition holds.
type TaskStateEntry struct {
	ID           int64   `json:"id"`
	State        string  `json:"state"`
	Category     string  `json:"category"`
	Blockers     []int64 `json:"blockers"`
	ConditionMet *bool   `json:"condition_met,omitempty"`
}

// TaskStatesSnapshot is GET /v1/tasks/states/snapshot: the tasks asked for in
// request order after de-duplication, the ids that name no task, and the
// reading transaction's start time, AsOf. The rows are one snapshot, at least
// as new as AsOf; AsOf does not date it. Every requested id appears exactly
// once, in Tasks or in Missing, so a caller can tell "missing" from "not
// asked".
type TaskStatesSnapshot struct {
	AsOf    time.Time        `json:"as_of"`
	Tasks   []TaskStateEntry `json:"tasks"`
	Missing []int64          `json:"missing"`
}

// TaskStates answers ids in the order given, after dropping repeats and ids
// that name no task. until, when set, is the level condition_met is judged
// against; when empty each task's own start condition level applies.
// TaskStatesSnapshot reads the same thing and names the ids this one drops.
func (s *Store) TaskStates(ctx context.Context, ids []int64, until string) ([]TaskStateEntry, error) {
	out, err := s.TaskStatesSnapshot(ctx, ids, until)
	return out.Tasks, err
}

// TaskStatesSnapshot answers ids in the order given, after dropping repeats,
// in one REPEATABLE READ READ ONLY transaction that also reads as_of.
// until, when set, is the level condition_met is judged against; when empty
// each task's own start condition level applies. Blockers are the open tasks
// (not settled) with a blocks relation to the task, plus the start-condition
// task while its condition is unmet; missing ids are ascending.
func (s *Store) TaskStatesSnapshot(ctx context.Context, ids []int64, until string) (TaskStatesSnapshot, error) {
	var out TaskStatesSnapshot
	if len(ids) == 0 || len(ids) > TaskStatesMax {
		return out, spawnInvalid(fmt.Sprintf("ids must name 1 to %d tasks", TaskStatesMax))
	}
	if until != "" && until != "merged" && until != "deployed" {
		return out, spawnInvalid("until must be merged or deployed")
	}
	seen := map[int64]bool{}
	want := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id < 1 {
			return out, spawnInvalid("ids must be task ids")
		}
		if !seen[id] {
			seen[id] = true
			want = append(want, id)
		}
	}
	tx, err := s.beginSnapshotA5(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	// as_of is the transaction's start time: now() is fixed at BEGIN, while
	// the MVCC snapshot belongs to the first statement after it. The snapshot
	// is therefore at least as new as as_of, never older, and every row below
	// comes from that one snapshot. pgx returns the value in the local zone.
	if err := tx.QueryRow(ctx, s.q(`SELECT now()`, `SELECT UTC_TIMESTAMP(6)`)).Scan(&out.AsOf); err != nil {
		return out, err
	}
	out.AsOf = out.AsOf.UTC()
	states := map[int64]string{}
	rows, err := tx.Query(ctx, s.q(`SELECT id,state FROM tasks WHERE id=ANY($1)`, `SELECT id,state FROM tasks WHERE id IN (`+placeholders(len(want))+`)`), a5Int64Args(s, want)...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			rows.Close()
			return out, err
		}
		states[id] = state
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	blockers := map[int64][]int64{}
	rows, err = tx.Query(ctx, s.q(`SELECT r.dst_id,b.id,b.state FROM task_relations r JOIN tasks b ON b.id=r.src_id WHERE r.kind='blocks' AND r.archived_at IS NULL AND r.dst_id=ANY($1) ORDER BY r.dst_id,b.id`, `SELECT r.dst_id,b.id,b.state FROM task_relations r JOIN tasks b ON b.id=r.src_id WHERE r.kind='blocks' AND r.archived_at IS NULL AND r.dst_id IN (`+placeholders(len(want))+`) ORDER BY r.dst_id,b.id`), a5Int64Args(s, want)...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var dst, src int64
		var state string
		if err := rows.Scan(&dst, &src, &state); err != nil {
			rows.Close()
			return out, err
		}
		if !IsSettledState(state) {
			blockers[dst] = append(blockers[dst], src)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	present := make([]Task, 0, len(want))
	for _, id := range want {
		if _, ok := states[id]; ok {
			present = append(present, Task{ID: id})
		} else {
			out.Missing = append(out.Missing, id)
		}
	}
	if out.Missing == nil {
		out.Missing = []int64{}
	}
	sort.Slice(out.Missing, func(i, j int) bool { return out.Missing[i] < out.Missing[j] })
	if err := attachTaskSpawns(ctx, tx, present); err != nil {
		return out, err
	}
	out.Tasks = make([]TaskStateEntry, 0, len(present))
	for _, x := range present {
		state := states[x.ID]
		e := TaskStateEntry{ID: x.ID, State: state, Category: TaskCategory(state), Blockers: []int64{}}
		e.Blockers = append(e.Blockers, blockers[x.ID]...)
		if c := x.Spawn; c != nil && c.StartCondition != nil {
			level := c.StartCondition.Until
			if until != "" {
				level = until
			}
			met := spawnConditionMet(level, c.StartCondition.TaskState)
			e.ConditionMet = &met
			if !met && c.StartCondition.TaskState != "" && !containsID(e.Blockers, c.StartCondition.TaskID) {
				e.Blockers = append(e.Blockers, c.StartCondition.TaskID)
			}
		}
		sort.Slice(e.Blockers, func(i, j int) bool { return e.Blockers[i] < e.Blockers[j] })
		out.Tasks = append(out.Tasks, e)
	}
	return out, nil
}

func containsID(xs []int64, id int64) bool {
	for _, x := range xs {
		if x == id {
			return true
		}
	}
	return false
}

// WorkStatusQueuedTask is one open task whose spawn request has not ended:
// the work-status page lists it with the state, reason and start condition.
type WorkStatusQueuedTask struct {
	ID      int64     `json:"id"`
	Title   string    `json:"title"`
	Project *string   `json:"project"`
	State   string    `json:"state"`
	Spawn   TaskSpawn `json:"spawn"`
}

// readQueuedSpawnsTx lists the tasks with a spawn request still in flight,
// oldest request first. hkOpenOnly keeps the tasks hk's own copy holds open:
// the hk-only page's test. The YouTrack-backed page passes false, because
// YouTrack, not hk's copy, says whether the task is open.
func readQueuedSpawnsTx(ctx context.Context, q poolQuerier, hkOpenOnly bool) ([]WorkStatusQueuedTask, error) {
	rows, err := q.Query(ctx, sqlFor(q, `SELECT t.id,t.title,t.project,t.state FROM task_spawn_records r JOIN tasks t ON t.id=r.task_id WHERE r.state NOT IN ('landed','refused','given_up','cancelled') AND (NOT $1 OR t.state NOT IN ('merged','deployed','completed','dropped','duplicate')) ORDER BY r.updated_at,t.id`, `SELECT t.id,t.title,t.project,t.state FROM task_spawn_records r JOIN tasks t ON t.id=r.task_id WHERE r.state NOT IN ('landed','refused','given_up','cancelled') AND (NOT ? OR t.state NOT IN ('merged','deployed','completed','dropped','duplicate')) ORDER BY r.updated_at,t.id`), hkOpenOnly)
	if err != nil {
		return nil, err
	}
	var metas []WorkStatusQueuedTask
	for rows.Next() {
		var m WorkStatusQueuedTask
		if err := rows.Scan(&m.ID, &m.Title, &m.Project, &m.State); err != nil {
			rows.Close()
			return nil, err
		}
		metas = append(metas, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []WorkStatusQueuedTask{}
	if len(metas) == 0 {
		return out, nil
	}
	xs := make([]Task, 0, len(metas))
	for _, m := range metas {
		xs = append(xs, Task{ID: m.ID})
	}
	if err := attachTaskSpawns(ctx, q, xs); err != nil {
		return nil, err
	}
	for i, m := range metas {
		if xs[i].Spawn == nil {
			continue
		}
		m.Spawn = *xs[i].Spawn
		out = append(out, m)
	}
	return out, nil
}
