package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// ExportLimitMax is the hard cap on task rows a single snapshot export may
// return. The handler applies the same bound to the limit query parameter.
const ExportLimitMax = 10000

// ErrInvalidExportQuery rejects an export filter with the same closed
// vocabulary and name constraints used by existing task reads.
var ErrInvalidExportQuery = errors.New("invalid_export_query")

// exportTxOptions pins the isolation and access mode every export runs under.
// Tests open transactions with the same options to prove the snapshot is
// repeatable-read and the transaction cannot write.
var exportTxOptions = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}

// exportSeam is a test-only barrier invoked inside the export transaction
// after the snapshot-establishing metadata, count, and watermark reads and
// before the row scan. Production code leaves it nil; it is unexported and
// unreachable from any network surface.
var exportSeam func(ctx context.Context)

// exportSnapshotSeam is a test-only barrier invoked inside the export
// transaction immediately after the snapshot-establishing query and before
// the total-count read, so tests can commit concurrent changes in that gap.
// Production code leaves it nil; it is unexported and unreachable from any
// network surface.
var exportSnapshotSeam func(ctx context.Context)

// validTaskQuery is the shared filter validation used by task list reads and
// the snapshot export.
func validTaskQuery(lane, state, parentLane string) bool {
	return (lane == "" || validName(lane)) && (parentLane == "" || validName(parentLane)) && (state == "" || taskStates[state])
}

// taskFilter renders the optional lane/parent_lane/state predicates shared by
// every export query so counts, watermarks, and rows cover one scope.
func taskFilter(lane, state, parentLane string) (string, []any) {
	where := ""
	args := []any{}
	if lane != "" {
		args = append(args, lane)
		where += fmt.Sprintf(" AND lane=$%d", len(args))
	}
	if parentLane != "" {
		args = append(args, parentLane)
		where += fmt.Sprintf(" AND parent_lane=$%d", len(args))
	}
	if state != "" {
		args = append(args, state)
		where += fmt.Sprintf(" AND state=$%d", len(args))
	}
	return where, args
}

// TaskExportSource reports the serving build. VCSRevision is the embedded
// vcs.revision stamp, or "unknown" when the binary was built without VCS
// stamping; a revision is never invented.
type TaskExportSource struct {
	VCSRevision string `json:"vcs_revision"`
}

// TaskExportScope is the normalized filter set actually applied to the
// export. Empty strings mean the filter was unset.
type TaskExportScope struct {
	Lane       string `json:"lane"`
	State      string `json:"state"`
	ParentLane string `json:"parent_lane"`
	Limit      int    `json:"limit"`
}

// TaskExportWatermark bounds the event streams at the export snapshot. A zero
// value means the table was empty at the snapshot (MAX(id) over an empty
// table is NULL, which the export emits as 0).
type TaskExportWatermark struct {
	TaskEventMaxID  int64 `json:"task_event_max_id"`
	RelayEventMaxID int64 `json:"relay_event_max_id"`
}

// TaskExportCounts authoritatively counts the filtered scope inside the
// snapshot. ByState has an entry for every canonical task state, including
// merged and dropped; ByLane has an entry for every lane present in the
// filtered set.
type TaskExportCounts struct {
	Total   int            `json:"total"`
	ByState map[string]int `json:"by_state"`
	ByLane  map[string]int `json:"by_lane"`
}

// TaskExport is one consistent snapshot of the task queue. SnapshotID,
// DBTime, Watermark, Counts, and Tasks are all read from a single
// REPEATABLE READ READ ONLY transaction, so concurrent commits can never
// appear half-visible. IDsSHA256 covers each returned row's decimal ASCII ID
// plus a newline; RowsSHA256 covers each returned row's deterministic JSON
// encoding (the same bytes each element of Tasks emits on the wire: Go
// encoding/json struct order, RFC3339Nano UTC timestamps, refs as nested
// JSON) plus a newline. Both digests over an empty result set equal the
// SHA-256 of empty input.
type TaskExport struct {
	SnapshotID   string              `json:"snapshot_id"`
	DBTime       time.Time           `json:"db_time"`
	Source       TaskExportSource    `json:"source"`
	Scope        TaskExportScope     `json:"scope"`
	Watermark    TaskExportWatermark `json:"watermark"`
	Counts       TaskExportCounts    `json:"counts"`
	RowsReturned int                 `json:"rows_returned"`
	Complete     bool                `json:"complete"`
	Truncated    bool                `json:"truncated"`
	IDsSHA256    string              `json:"ids_sha256"`
	RowsSHA256   string              `json:"rows_sha256"`
	Tasks        []Task              `json:"tasks"`
}

// ExportTasks returns a consistent task snapshot in one read-only
// repeatable-read transaction. Rows come back in ascending numeric ID order,
// at most limit rows. Truncated is exactly Counts.Total > limit; Complete is
// its inverse, so a complete export always returns every counted row.
func (s *Store) ExportTasks(ctx context.Context, lane, state, parentLane string, limit int) (TaskExport, error) {
	if !validTaskQuery(lane, state, parentLane) || limit < 1 || limit > ExportLimitMax {
		return TaskExport{}, ErrInvalidExportQuery
	}
	tx, err := s.pool.BeginTx(ctx, exportTxOptions)
	if err != nil {
		return TaskExport{}, err
	}
	defer tx.Rollback(ctx)

	out := TaskExport{
		Scope: TaskExportScope{Lane: lane, State: state, ParentLane: parentLane, Limit: limit},
		Tasks: []Task{},
		Counts: TaskExportCounts{
			ByState: map[string]int{},
			ByLane:  map[string]int{},
		},
	}
	for state := range taskStates {
		out.Counts.ByState[state] = 0
	}

	// The first statement fixes the repeatable-read snapshot; every value
	// below is read from that one snapshot.
	if err = tx.QueryRow(ctx, `SELECT pg_current_snapshot()::text, now()`).Scan(&out.SnapshotID, &out.DBTime); err != nil {
		return TaskExport{}, err
	}
	out.DBTime = out.DBTime.UTC()

	if exportSnapshotSeam != nil {
		exportSnapshotSeam(ctx)
	}

	where, args := taskFilter(lane, state, parentLane)
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM tasks WHERE 1=1`+where, args...).Scan(&out.Counts.Total); err != nil {
		return TaskExport{}, err
	}
	rows, err := tx.Query(ctx, `SELECT state,COUNT(*) FROM tasks WHERE 1=1`+where+` GROUP BY state`, args...)
	if err != nil {
		return TaskExport{}, err
	}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return TaskExport{}, err
		}
		out.Counts.ByState[state] = count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return TaskExport{}, err
	}
	rows, err = tx.Query(ctx, `SELECT lane,COUNT(*) FROM tasks WHERE 1=1`+where+` GROUP BY lane`, args...)
	if err != nil {
		return TaskExport{}, err
	}
	for rows.Next() {
		var lane string
		var count int
		if err := rows.Scan(&lane, &count); err != nil {
			rows.Close()
			return TaskExport{}, err
		}
		out.Counts.ByLane[lane] = count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return TaskExport{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT MAX(id) FROM task_events),0),COALESCE((SELECT MAX(id) FROM relay_events),0)`).Scan(&out.Watermark.TaskEventMaxID, &out.Watermark.RelayEventMaxID); err != nil {
		return TaskExport{}, err
	}

	if exportSeam != nil {
		exportSeam(ctx)
	}

	rowArgs := append(append([]any{}, args...), limit)
	rows, err = tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE 1=1`+where+fmt.Sprintf(` ORDER BY id ASC LIMIT $%d`, len(rowArgs)), rowArgs...)
	if err != nil {
		return TaskExport{}, err
	}
	for rows.Next() {
		var task Task
		if err := scanTask(rows, &task); err != nil {
			rows.Close()
			return TaskExport{}, err
		}
		task.CreatedAt = task.CreatedAt.UTC()
		task.UpdatedAt = task.UpdatedAt.UTC()
		out.Tasks = append(out.Tasks, task)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return TaskExport{}, err
	}

	idsHash := sha256.New()
	rowsHash := sha256.New()
	for _, task := range out.Tasks {
		idsHash.Write([]byte(strconv.FormatInt(task.ID, 10) + "\n"))
		encoded, err := json.Marshal(task)
		if err != nil {
			return TaskExport{}, err
		}
		rowsHash.Write(append(encoded, '\n'))
	}
	out.IDsSHA256 = hex.EncodeToString(idsHash.Sum(nil))
	out.RowsSHA256 = hex.EncodeToString(rowsHash.Sum(nil))
	out.RowsReturned = len(out.Tasks)
	out.Truncated = out.Counts.Total > limit
	out.Complete = !out.Truncated
	return out, tx.Commit(ctx)
}
