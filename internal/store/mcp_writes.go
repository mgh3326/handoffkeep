package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// The MCP write contract (hk 3465). Every write a chatgpt-channel (dots)
// principal makes goes through a function here or in comments.go, records an
// audit row, and replays its first result when the same idempotency key comes
// back from the same actor.
var (
	// ErrRevisionConflict refuses a write whose expected_updated_at is no longer
	// the task's updated_at. Nothing is applied; the caller re-reads the task.
	ErrRevisionConflict = errors.New("revision_conflict")
	// ErrOperatorOnlyDecision refuses a dots answer to a request whose effective
	// policy is operator_direct. Only the verified operator path answers it.
	ErrOperatorOnlyDecision = errors.New("operator_only_decision")
	// ErrInvalidTaskUpdate refuses a task_update call with no field, a missing
	// key or expected revision, or an out-of-range priority.
	ErrInvalidTaskUpdate = errors.New("invalid_task_update")
	// ErrInvalidMCPWrite refuses a write whose tool, actor, channel, author or key
	// is missing or too long for the audit row.
	ErrInvalidMCPWrite = errors.New("invalid_mcp_write")
	// ErrIdempotencyKeyReused refuses a key that an earlier write of the same
	// tool and actor recorded for a different task.
	ErrIdempotencyKeyReused = errors.New("idempotency_key_reused")
)

const (
	mcpTaskUpdateTool   = "task_update"
	mcpTaskUpdateSource = "mcp-task-update"
)

// MCPWrite identifies one MCP write: who made it (Actor, the authenticated
// principal), through which credential (Channel), the author label the
// record shows (Author; "dots" for the chatgpt channel), and the caller's
// idempotency key (Key, optional).
type MCPWrite struct {
	Tool    string
	Actor   string
	Channel string
	Author  string
	Key     string
}

func (w MCPWrite) validate() error {
	if w.Tool == "" || !validText(w.Tool, 64) || w.Actor == "" || !validText(w.Actor, 320) ||
		w.Channel == "" || !validText(w.Channel, 32) || w.Author == "" || !validText(w.Author, 128) {
		return ErrInvalidMCPWrite
	}
	return CheckIdempotencyKey(w.Key)
}

// maxIdempotencyKeyBytes is the longest idempotency key an MCP write records.
const maxIdempotencyKeyBytes = 128

// CheckIdempotencyKey refuses a key the audit row cannot hold: one longer than
// maxIdempotencyKeyBytes, or one with a NUL byte, which PostgreSQL text refuses
// (SQLSTATE 22021). An empty key passes; each write decides whether it needs one.
func CheckIdempotencyKey(key string) error {
	if !validText(key, maxIdempotencyKeyBytes) {
		return fmt.Errorf("%w: idempotency_key must be at most %d bytes and contain no NUL byte", ErrInvalidMCPWrite, maxIdempotencyKeyBytes)
	}
	return nil
}

// mcpWriteLock is the advisory lock id of one keyed write. The tool, actor and
// key are joined with NUL and hashed here, not in SQL: a NUL cannot appear in
// any of them, so the join is unambiguous, but PostgreSQL text refuses it.
func mcpWriteLock(w MCPWrite) int64 {
	sum := sha256.Sum256([]byte(w.Tool + "\x00" + w.Actor + "\x00" + w.Key))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// mcpWriteRef is the event and comment source ref for a keyed write. It is a
// hash of the actor and the key, so one caller's key cannot collide with
// another's, and the raw key never reaches a task event.
func mcpWriteRef(actor, key string) string {
	sum := sha256.Sum256([]byte(actor + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// replayMCPWriteTx loads the first result a keyed write recorded. It reports
// false when the key was never used.
func replayMCPWriteTx(ctx context.Context, tx pgx.Tx, w MCPWrite, out any) (bool, error) {
	if w.Key == "" {
		return false, nil
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT result FROM mcp_write_audit WHERE tool=$1 AND actor=$2 AND idempotency_key=$3`, w.Tool, w.Actor, w.Key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, out)
}

// RecordMCPDecisionRequest records a decision request for an MCP write. The
// request, its audit row and the replay check share one transaction under an
// advisory lock on the write's key, so two calls with the same key cannot both
// record a request, and a retry after a crash finds the first call's result.
// replayed reports that the key was already used; the returned result is then
// the first call's.
func (s *Store) RecordMCPDecisionRequest(ctx context.Context, taskID int64, in DecisionRequestInput, w MCPWrite) (DecisionRequestResult, bool, error) {
	if err := w.validate(); err != nil {
		return DecisionRequestResult{}, false, err
	}
	if w.Key == "" {
		return DecisionRequestResult{}, false, ErrInvalidMCPWrite
	}
	if err := checkDecisionRequestWrite(taskID, w.Author, &in); err != nil {
		return DecisionRequestResult{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DecisionRequestResult{}, false, err
	}
	defer tx.Rollback(ctx)
	// The key lock is taken before any task row, and no path takes a task row
	// and then this key, so it cannot close a cycle.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, mcpWriteLock(w)); err != nil {
		return DecisionRequestResult{}, false, err
	}
	var recorded DecisionRequestResult
	replayed, err := replayMCPWriteTx(ctx, tx, w, &recorded)
	if err != nil {
		return DecisionRequestResult{}, false, err
	}
	if replayed {
		if recorded.Task.ID != taskID {
			return DecisionRequestResult{}, false, ErrIdempotencyKeyReused
		}
		recorded.Duplicate = true
		return recorded, true, tx.Commit(ctx)
	}
	result, err := s.recordDecisionRequestTx(ctx, tx, taskID, w.Author, in)
	if err != nil {
		return DecisionRequestResult{}, false, err
	}
	if err := insertMCPWriteAuditTx(ctx, tx, w, taskID, result, time.Now().UTC()); err != nil {
		return DecisionRequestResult{}, false, err
	}
	return result, false, tx.Commit(ctx)
}

// insertMCPWriteAuditTx writes the audit row inside the write's transaction, so
// a write and its record commit together. A second use of one key by the same
// actor and tool is ErrIdempotencyKeyReused.
func insertMCPWriteAuditTx(ctx context.Context, tx pgx.Tx, w MCPWrite, taskID int64, result any, at time.Time) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO mcp_write_audit(tool,actor,channel,author,task_id,idempotency_key,result,at) VALUES($1,$2,$3,$4,NULLIF($5::bigint,0),$6,$7::jsonb,$8)`,
		w.Tool, w.Actor, w.Channel, w.Author, taskID, w.Key, string(raw), at)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrIdempotencyKeyReused
	}
	return err
}

// RecordMCPWrite writes the audit row for a write that committed in its own
// transaction (an intake or a decision request). A repeat of a keyed write
// keeps the first row. The write is idempotent by its own key, so a retry
// after a crash here writes the row on the next call.
func (s *Store) RecordMCPWrite(ctx context.Context, w MCPWrite, taskID int64, result any) error {
	if err := w.validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO mcp_write_audit(tool,actor,channel,author,task_id,idempotency_key,result,at) VALUES($1,$2,$3,$4,NULLIF($5::bigint,0),$6,$7::jsonb,$8) ON CONFLICT (tool, actor, idempotency_key) WHERE idempotency_key <> '' DO NOTHING`,
		w.Tool, w.Actor, w.Channel, w.Author, taskID, w.Key, string(raw), time.Now().UTC())
	return err
}

// TaskUpdateInput is one task_update call. ExpectedUpdatedAt is the revision
// the caller read; a task that moved since is a conflict. At least one change
// is set: State, Lane, Priority, or a label name in AddLabels or RemoveLabels.
// Note becomes the event note of each change.
type TaskUpdateInput struct {
	ExpectedUpdatedAt time.Time
	State             string
	Lane              string
	Priority          *int
	AddLabels         []string
	RemoveLabels      []string
	Note              string
}

// maxTaskUpdateLabels bounds the label names one call may add or remove.
const maxTaskUpdateLabels = 20

// TaskUpdateResult is what a task_update applied. Changed names the fields this
// call moved, and Revision is the task's updated_at after the write. A replay
// returns the first call's Changed and Revision with Replayed set; Task is the
// task as it is now.
type TaskUpdateResult struct {
	TaskID   int64     `json:"task_id"`
	Changed  []string  `json:"changed"`
	Revision time.Time `json:"revision"`
	Replayed bool      `json:"replayed,omitempty"`
	Task     Task      `json:"-"`
}

// UpdateTask applies a task_update call in one transaction. Order: lock the
// task, return the recorded result for a replayed key, refuse a stale revision,
// apply priority, state and lane through the same paths the console uses, then
// record the audit row. Any refusal leaves the task and the ledger untouched.
func (s *Store) UpdateTask(ctx context.Context, id int64, in TaskUpdateInput, w MCPWrite) (TaskUpdateResult, error) {
	var out TaskUpdateResult
	if w.Tool != mcpTaskUpdateTool || id < 1 || in.ExpectedUpdatedAt.IsZero() || w.Key == "" || !validLifecycleKey(w.Key) ||
		(in.State == "" && in.Lane == "" && in.Priority == nil && len(in.AddLabels) == 0 && len(in.RemoveLabels) == 0) ||
		(in.Priority != nil && (*in.Priority < 0 || *in.Priority > math.MaxInt32)) ||
		len(in.AddLabels) > maxTaskUpdateLabels || len(in.RemoveLabels) > maxTaskUpdateLabels ||
		!validText(in.Note, MaxBytes) || !validText(in.State, 64) || !validText(in.Lane, 128) ||
		!validLabelNames(in.AddLabels) || !validLabelNames(in.RemoveLabels) {
		return out, ErrInvalidTaskUpdate
	}
	if err := w.validate(); err != nil {
		return out, err
	}
	if err := guard.Reject(in.Note); err != nil {
		return out, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return out, err
	}
	var x Task
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrTaskNotFound
		}
		return out, err
	}
	var recorded TaskUpdateResult
	replayed, err := replayMCPWriteTx(ctx, tx, w, &recorded)
	if err != nil {
		return out, err
	}
	if replayed {
		if recorded.TaskID != id {
			return out, ErrIdempotencyKeyReused
		}
		recorded.Replayed = true
		recorded.Task = x
		return recorded, tx.Commit(ctx)
	}
	if !x.UpdatedAt.Equal(in.ExpectedUpdatedAt.UTC()) {
		return out, ErrRevisionConflict
	}
	// Each field's event gets its own source ref. task_events dedupes on
	// (task, source, source_ref), so one shared ref would drop every event after
	// the first. Replay reads the audit row, so the refs are free to differ.
	ref := mcpWriteRef(w.Actor, w.Key)
	changed := []string{}
	if in.Priority != nil && *in.Priority != x.Priority {
		if err := setTaskPriorityTx(ctx, tx, &x, *in.Priority, w.Author, in.Note, mcpTaskUpdateSource, ref+":priority"); err != nil {
			return out, err
		}
		changed = append(changed, "priority")
	}
	if in.State != "" && in.State != x.State {
		if _, err := s.transitionTaskTx(ctx, tx, id, TransitionInput{To: in.State, By: w.Author, Note: in.Note, Source: mcpTaskUpdateSource, SourceRef: ref + ":state"}, false); err != nil {
			return out, err
		}
		changed = append(changed, "state")
	}
	if in.Lane != "" && in.Lane != x.Lane {
		// relaneTaskTx refuses a blank note; a lane change without one gets this default.
		laneNote := in.Note
		if strings.TrimSpace(laneNote) == "" {
			laneNote = "lane changed by task_update"
		}
		_, laneChanged, err := s.relaneTaskTx(ctx, tx, id, in.Lane, w.Author, laneNote, false, mcpTaskUpdateSource, ref+":lane")
		if err != nil {
			return out, err
		}
		if laneChanged {
			changed = append(changed, "lane")
		}
	}
	if len(in.AddLabels)+len(in.RemoveLabels) > 0 {
		// The state and lane writes above update their own copy of the row;
		// reload it so the label events record the state the task now has.
		if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1`, id), &x); err != nil {
			return out, err
		}
	}
	labelsChanged, err := s.applyTaskUpdateLabels(ctx, tx, &x, in, w.Author)
	if err != nil {
		return out, err
	}
	if labelsChanged {
		changed = append(changed, "labels")
	}
	var final Task
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1`, id), &final); err != nil {
		return out, err
	}
	out = TaskUpdateResult{TaskID: id, Changed: changed, Revision: final.UpdatedAt, Task: final}
	if err := insertMCPWriteAuditTx(ctx, tx, w, id, out, time.Now().UTC()); err != nil {
		return TaskUpdateResult{}, err
	}
	return out, tx.Commit(ctx)
}

// validLabelNames reports whether every name is a non-empty label name of the
// store's size.
func validLabelNames(names []string) bool {
	for _, name := range names {
		if strings.TrimSpace(name) == "" || !validText(name, 256) {
			return false
		}
	}
	return true
}

// applyTaskUpdateLabels attaches the AddLabels names and detaches RemoveLabels,
// by label name, in the caller's transaction. A name the store does not know is
// ErrLabelNotFound and the whole call is refused.
func (s *Store) applyTaskUpdateLabels(ctx context.Context, tx pgx.Tx, x *Task, in TaskUpdateInput, by string) (bool, error) {
	changed := false
	for _, step := range []struct {
		names  []string
		attach bool
	}{{in.AddLabels, true}, {in.RemoveLabels, false}} {
		for _, name := range step.names {
			var labelID int64
			err := tx.QueryRow(ctx, `SELECT id FROM labels WHERE name=$1`, strings.TrimSpace(name)).Scan(&labelID)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, ErrLabelNotFound
			}
			if err != nil {
				return false, err
			}
			moved, err := s.setTaskLabelTx(ctx, tx, x, labelID, by, step.attach)
			if err != nil {
				return false, err
			}
			changed = changed || moved
		}
	}
	return changed, nil
}
