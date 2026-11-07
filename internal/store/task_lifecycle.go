package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// ErrTaskMergePath refuses close-as-merged for a task whose state has no
// manual path to merged. The console offers the action only for reviewing,
// researching and developing tasks; everything else is refused here.
var ErrTaskMergePath = errors.New("task_merge_path")

// Console lifecycle write sources. Every operator click carries an
// idempotency key; the event pair (source, source_ref) makes a replay find the
// recorded event instead of applying the change again.
const (
	lifecycleSourceCloseMerged    = "ui-close-merged"
	lifecycleSourceCloseCompleted = "ui-close-completed"
	lifecycleSourceDrop           = "ui-drop"
	lifecycleSourceDuplicate      = "ui-duplicate"
	lifecycleSourceHold           = "ui-hold"
	lifecycleSourceReopen         = "ui-reopen"
	lifecycleSourceRelane         = "ui-relane"
)

// HoldResult is the task's state before and after a hold or a hold release.
type HoldResult struct {
	Previous string `json:"previous"`
	State    string `json:"state"`
}

// RelaneResult is an operator relane: the lane it left, the lane this key
// moved it to, and whether this key moved it. Lane is the recorded target,
// not the task's current lane, so a replay after a later move still reports
// the move this key made. A replay reports Changed true again so its lane
// notices can be re-sent; the hub dedupes by the event ids derived from the
// key.
type RelaneResult struct {
	Previous string
	Lane     string
	Title    string
	Changed  bool
}

func validLifecycleKey(key string) bool {
	return key != "" && validText(key, 128)
}

// CloseTaskMerged closes a task as merged in one call. A reviewing task gets
// the single reviewing -> merged event. A researching or developing task gets
// the intermediate reviewing step and then merged, both recorded in one
// transaction so no reader sees the half path. path is the states traversed,
// starting at the state the task had before this call.
func (s *Store) CloseTaskMerged(ctx context.Context, id int64, evidence, by, note, key string) (Task, []string, error) {
	evidence = strings.TrimSpace(evidence)
	if !validLifecycleKey(key) || by == "" || !validText(by, 128) || !validText(note, MaxBytes) || !validText(evidence, 1024) {
		return Task{}, nil, errors.New("invalid task lifecycle action")
	}
	if err := guard.Reject(evidence); err != nil {
		return Task{}, nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return Task{}, nil, err
	}
	var x Task
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, nil, ErrTaskNotFound
		}
		return Task{}, nil, err
	}
	path, recorded, err := recordedLifecyclePath(ctx, tx, id, lifecycleSourceCloseMerged, key)
	if err != nil {
		return Task{}, nil, err
	}
	if recorded {
		return x, path, tx.Commit(ctx)
	}
	from := x.State
	var steps []string
	switch from {
	case StateReviewing:
		steps = []string{StateMerged}
	case StateDeveloping, StateResearching:
		steps = []string{StateReviewing, StateMerged}
	default:
		if IsSettledState(from) {
			return Task{}, nil, ErrTaskConflict
		}
		return Task{}, nil, fmt.Errorf("%w: close-as-merged requires reviewing, researching or developing", ErrTaskMergePath)
	}
	path = []string{from}
	for index, to := range steps {
		ref := key
		if index < len(steps)-1 {
			ref = key + ":" + StateReviewing
		}
		x, err = s.transitionTaskTx(ctx, tx, id, TransitionInput{
			To: to, Closure: "resolved", Evidence: evidence, By: by, Note: note,
			Source: lifecycleSourceCloseMerged, SourceRef: ref,
		}, false)
		if err != nil {
			return Task{}, nil, err
		}
		path = append(path, to)
	}
	return x, path, tx.Commit(ctx)
}

// recordedLifecyclePath returns the states a recorded close-as-merged call
// traversed: the from of its first event followed by every to.
func recordedLifecyclePath(ctx context.Context, tx pgx.Tx, id int64, source, key string) ([]string, bool, error) {
	rows, err := tx.Query(ctx, `SELECT "from","to" FROM task_events WHERE task_id=$1 AND source=$2 AND source_ref=ANY($3) ORDER BY id`, id, source, []string{key, key + ":" + StateReviewing})
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var path []string
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return nil, false, err
		}
		if len(path) == 0 {
			path = append(path, from)
		}
		path = append(path, to)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return path, len(path) > 0, nil
}

// CloseTaskCompleted closes a decide, ops or verify task with a result link.
// The store's existing guards apply: the kind must be completable and the
// evidence must be an https URL or an hk:doc key.
func (s *Store) CloseTaskCompleted(ctx context.Context, id int64, evidence, by, note, key string) (Task, error) {
	return s.operatorTransition(ctx, id, lifecycleSourceCloseCompleted, key, TransitionInput{
		To: StateCompleted, Closure: "resolved", Evidence: strings.TrimSpace(evidence), By: by, Note: note,
	}, false)
}

// DropTask closes a task as dropped with one of the store's closure types:
// cancelled, obsolete or superseded. The reason is both the event note and
// the closure evidence.
func (s *Store) DropTask(ctx context.Context, id int64, closure, reason, by, key string) (Task, error) {
	switch closure {
	case "cancelled", "obsolete", "superseded":
	default:
		return Task{}, fmt.Errorf("%w: drop closure must be cancelled, obsolete or superseded", ErrClosureInvalid)
	}
	return s.operatorTransition(ctx, id, lifecycleSourceDrop, key, TransitionInput{
		To: StateDropped, Closure: closure, Evidence: reason, By: by, Note: reason,
	}, false)
}

// DuplicateTask closes a task as a duplicate of duplicateOf, which the store
// writes as a duplicate_of relation and the closure evidence names.
func (s *Store) DuplicateTask(ctx context.Context, id, duplicateOf int64, reason, by, key string) (Task, error) {
	if duplicateOf < 0 {
		return Task{}, errors.New("invalid task lifecycle action")
	}
	return s.operatorTransition(ctx, id, lifecycleSourceDuplicate, key, TransitionInput{
		To: StateDuplicate, Closure: "superseded", DuplicateOf: duplicateOf, By: by, Note: reason,
	}, false)
}

// HoldTask parks a backlog or unstarted task in icebox, or releases an icebox
// task to to (backlog by default, unstarted for an undo of a hold that started
// there). The result names both states so the console's undo can restore the
// exact previous one.
func (s *Store) HoldTask(ctx context.Context, id int64, held bool, to, by, note, key string) (HoldResult, error) {
	if !validLifecycleKey(key) || by == "" || !validText(by, 128) || !validText(note, MaxBytes) {
		return HoldResult{}, errors.New("invalid task lifecycle action")
	}
	if err := guard.Reject(note); err != nil {
		return HoldResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return HoldResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return HoldResult{}, err
	}
	var x Task
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return HoldResult{}, ErrTaskNotFound
		}
		return HoldResult{}, err
	}
	var previous, state string
	err = tx.QueryRow(ctx, `SELECT "from","to" FROM task_events WHERE task_id=$1 AND source=$2 AND source_ref=$3`, id, lifecycleSourceHold, key).Scan(&previous, &state)
	if err == nil {
		return HoldResult{Previous: previous, State: state}, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return HoldResult{}, err
	}
	target := to
	if held {
		if x.State != StateBacklog && x.State != StateUnstarted {
			return HoldResult{}, fmt.Errorf("%w: only backlog and unstarted tasks can be held", ErrTaskConflict)
		}
		target = StateIcebox
	} else {
		if x.State != StateIcebox {
			return HoldResult{}, fmt.Errorf("%w: only a held task can be released", ErrTaskConflict)
		}
		if target == "" {
			target = StateBacklog
		}
		if target != StateBacklog && target != StateUnstarted {
			return HoldResult{}, errors.New("invalid task lifecycle action: hold release targets backlog or unstarted")
		}
	}
	if _, err := s.transitionTaskTx(ctx, tx, id, TransitionInput{
		To: target, By: by, Note: note, Source: lifecycleSourceHold, SourceRef: key,
	}, false); err != nil {
		return HoldResult{}, err
	}
	return HoldResult{Previous: x.State, State: target}, tx.Commit(ctx)
}

// ReopenTaskBy reopens a settled or held task with a reason and an idempotency
// key. It is ReopenTask with the operator replay marker on its event.
func (s *Store) ReopenTaskBy(ctx context.Context, id int64, by, reason, key string) (Task, error) {
	return s.operatorTransition(ctx, id, lifecycleSourceReopen, key, TransitionInput{
		To: StateUnstarted, By: by, Note: reason,
	}, true)
}

// OperatorRelaneTask moves a task to another lane under an idempotency key.
// It is RelaneTask with replay detection on its relane event.
func (s *Store) OperatorRelaneTask(ctx context.Context, id int64, to, by, note, key string) (RelaneResult, error) {
	if !validLifecycleKey(key) || by == "" || !validText(by, 128) {
		return RelaneResult{}, errors.New("invalid task lifecycle action")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RelaneResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return RelaneResult{}, err
	}
	var x Task
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RelaneResult{}, ErrTaskNotFound
		}
		return RelaneResult{}, err
	}
	var previous, recordedTo string
	err = tx.QueryRow(ctx, `SELECT "from","to" FROM task_events WHERE task_id=$1 AND source=$2 AND source_ref=$3`, id, lifecycleSourceRelane, key).Scan(&previous, &recordedTo)
	if err == nil {
		return RelaneResult{Previous: previous, Lane: recordedTo, Title: x.Title, Changed: true}, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RelaneResult{}, err
	}
	before := x.Lane
	x, changed, err := s.relaneTaskTx(ctx, tx, id, to, by, note, false, lifecycleSourceRelane, key)
	if err != nil {
		return RelaneResult{}, err
	}
	return RelaneResult{Previous: before, Lane: x.Lane, Title: x.Title, Changed: changed}, tx.Commit(ctx)
}

// operatorTransition applies one manual transition under an idempotency key:
// the event carries (source, key), and a replay returns the task as recorded
// without writing a second event.
func (s *Store) operatorTransition(ctx context.Context, id int64, source, key string, in TransitionInput, reopen bool) (Task, error) {
	if !validLifecycleKey(key) || in.By == "" || !validText(in.By, 128) {
		return Task{}, errors.New("invalid task lifecycle action")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return Task{}, err
	}
	var x Task
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, ErrTaskNotFound
		}
		return Task{}, err
	}
	var recorded bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_events WHERE task_id=$1 AND source=$2 AND source_ref=$3)`, id, source, key).Scan(&recorded); err != nil {
		return Task{}, err
	}
	if recorded {
		return x, tx.Commit(ctx)
	}
	in.Source, in.SourceRef = source, key
	x, err = s.transitionTaskTx(ctx, tx, id, in, reopen)
	if err != nil {
		return Task{}, err
	}
	return x, tx.Commit(ctx)
}

// ListKnownTaskLanes lists every lane a current row uses: the same union the
// relane write trusts when allowNewLane is false.
func (s *Store) ListKnownTaskLanes(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM (
		SELECT lane AS name FROM tasks
		UNION SELECT parent_lane AS name FROM tasks WHERE parent_lane <> ''
		UNION SELECT owner_lane AS name FROM relay_events
		UNION SELECT lane AS name FROM chat_questions
	) lanes WHERE name <> '' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lanes := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		lanes = append(lanes, name)
	}
	return lanes, rows.Err()
}
