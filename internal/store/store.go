// Package store owns PostgreSQL persistence and validation.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

const MaxBytes = 64 << 10
const DocumentMaxBytes = 512 << 10
const CheckpointKeep = 500
const MemoryKeep = 2000

var ErrTrigramUnavailable = errors.New("PostgreSQL pg_trgm extension unavailable: grant CREATE on the database/schema or install pg_trgm as an administrator")
var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
var kinds = map[string]bool{"checkpoint": true, "handoff": true, "decision": true, "open_question": true, "next_action": true}
var memoryTypes = map[string]bool{"user": true, "feedback": true, "project": true, "reference": true}
var documentKinds = map[string]bool{"brief": true, "report": true, "answer": true, "handoff": true, "note": true, "other": true}
var taskKinds = map[string]bool{"implement": true, "verify": true, "fix": true, "decide": true, "ops": true}
var taskStates = map[string]bool{"backlog": true, "claimed": true, "in_progress": true, "verifying": true, "join": true, "hold": true, "needs_decision": true, "merged": true, "dropped": true}
var relayEventKinds = map[string]bool{"job.completed": true, "job.escalate": true, "job.joined": true, "lane.event": true}

const RelayLaneEventMaxBytes = 2048

var (
	ErrTaskConflict       = errors.New("task_conflict")
	ErrTaskNotFound       = errors.New("task_not_found")
	ErrRelayEventNotFound = errors.New("relay_event_not_found")
	// ErrQueueEmpty is deliberately distinct from a missing task.  It lets
	// queue consumers treat an empty lane as an expected terminal condition.
	ErrQueueEmpty = errors.New("queue_empty")
)

// TaskRefs holds the durable links that let a captain resume work without
// embedding credentials or implementation details in the queue itself.
type TaskRefs struct {
	PR         string `json:"pr,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	ReportPath string `json:"report_path,omitempty"`
	JobID      string `json:"job_id,omitempty"`
}

type TaskEvent struct {
	ID     int64     `json:"id"`
	TaskID int64     `json:"task_id"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	By     string    `json:"by"`
	Note   string    `json:"note,omitempty"`
	Refs   *TaskRefs `json:"refs,omitempty"`
	At     time.Time `json:"at"`
}

type Task struct {
	ID         int64       `json:"id"`
	Lane       string      `json:"lane"`
	ParentLane string      `json:"parent_lane,omitempty"`
	Title      string      `json:"title"`
	Kind       string      `json:"kind"`
	State      string      `json:"state"`
	Priority   int         `json:"priority"`
	Refs       TaskRefs    `json:"refs"`
	ClaimedBy  string      `json:"claimed_by,omitempty"`
	CreatedBy  string      `json:"created_by"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
	Events     []TaskEvent `json:"events,omitempty"`
}

// RelayEvent is a durable report from a worker to its owning lane.  Delivery
// state belongs here rather than in the relay hub so it survives restarts.
type RelayEvent struct {
	ID             int64      `json:"id"`
	Kind           string     `json:"kind"`
	JobID          string     `json:"job_id"`
	Epoch          int        `json:"epoch"`
	OwnerLane      string     `json:"owner_lane"`
	Machine        string     `json:"machine"`
	PaneID         string     `json:"pane_id"`
	ReportPath     string     `json:"report_path"`
	ReportLastLine string     `json:"report_last_line"`
	Question       string     `json:"question"`
	PR             string     `json:"pr"`
	Head           string     `json:"head"`
	Reason         string     `json:"reason"`
	EventID        string     `json:"event_id"`
	Text           string     `json:"text"`
	EventTime      *time.Time `json:"event_time"`
	ReceivedAt     time.Time  `json:"received_at"`
	DeliveredAt    *time.Time `json:"delivered_at"`
	DeliveredTo    string     `json:"delivered_to"`
	Attempts       int        `json:"attempts"`
}

type Store struct{ pool *pgxpool.Pool }

// Refs stores repeatable named references. Its decoder accepts legacy scalar
// values so checkpoints written by the pre-array API remain readable.
type Refs map[string][]string

func (r *Refs) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := Refs{}
	for key, value := range raw {
		var many []string
		if err := json.Unmarshal(value, &many); err == nil {
			out[key] = many
			continue
		}
		var one string
		if err := json.Unmarshal(value, &one); err != nil {
			return err
		}
		out[key] = []string{one}
	}
	*r = out
	return nil
}

type Checkpoint struct {
	ID        int64     `json:"id"`
	Session   string    `json:"session"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Refs      Refs      `json:"refs"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}
type Memory struct {
	Agent       string    `json:"agent"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Type        string    `json:"type"`
	Content     string    `json:"content,omitempty"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}
type Document struct {
	ID        int64     `json:"id"`
	Key       string    `json:"key"`
	Kind      string    `json:"kind"`
	Session   string    `json:"session"`
	Job       string    `json:"job"`
	Body      string    `json:"body,omitempty"`
	SHA256    string    `json:"sha256"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Attachment is immutable binary metadata. Object bytes are kept in R2; PostgreSQL
// holds only the content address, provenance, and references.
type Attachment struct {
	SHA256       string    `json:"sha256"`
	SizeBytes    int64     `json:"size_bytes"`
	MIME         string    `json:"mime"`
	OriginalName string    `json:"original_name"`
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
	RefKind      string    `json:"ref_kind,omitempty"`
	RefID        string    `json:"ref_id,omitempty"`
}
type AttachmentUsage struct {
	Month      string `json:"month"`
	Puts       int64  `json:"puts_month"`
	Gets       int64  `json:"gets_month"`
	BytesAdded int64  `json:"bytes_added_month"`
	TotalBytes int64  `json:"bytes_total"`
}
type SearchResult struct {
	Scope     string    `json:"scope"`
	Key       string    `json:"key"`
	Session   string    `json:"session"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Snippet   string    `json:"snippet"`
	Refs      Refs      `json:"refs,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func Open(ctx context.Context, url string) (*Store, error) {
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		return nil, errors.New("HANDOFFKEEP_DB_URL must be a PostgreSQL URL")
	}
	p, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	s := &Store{p}
	if err = s.migrate(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() { s.pool.Close() }
func (s *Store) migrate(ctx context.Context) error {
	// Multiple test binaries (and multiple service replicas at deployment) may
	// open the same database concurrently. Serialize additive DDL so PostgreSQL
	// catalog creation cannot race before IF NOT EXISTS observes the first row.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(824180045)`); err != nil {
		return err
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`, `INSERT INTO schema_version(version) VALUES (1) ON CONFLICT DO NOTHING`,
		`CREATE EXTENSION IF NOT EXISTS pg_trgm`,
		`CREATE TABLE IF NOT EXISTS checkpoints (id BIGSERIAL PRIMARY KEY, session TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('checkpoint','handoff','decision','open_question','next_action')), title TEXT NOT NULL, body TEXT NOT NULL, refs JSONB NOT NULL DEFAULT '{}'::jsonb, created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS checkpoints_session_created ON checkpoints(session, created_at DESC, id DESC)`, `CREATE INDEX IF NOT EXISTS checkpoints_fts ON checkpoints USING GIN (to_tsvector('simple', title || ' ' || body))`, `CREATE INDEX IF NOT EXISTS checkpoints_trgm ON checkpoints USING GIN ((title || ' ' || body) gin_trgm_ops)`,
		`CREATE TABLE IF NOT EXISTS memory (agent TEXT NOT NULL, name TEXT NOT NULL, description TEXT NOT NULL, memory_type TEXT NOT NULL CHECK(memory_type IN ('user','feedback','project','reference')), content TEXT NOT NULL, updated_by TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, UNIQUE(agent,name))`, `CREATE INDEX IF NOT EXISTS memory_agent_updated ON memory(agent, updated_at DESC, name DESC)`, `CREATE INDEX IF NOT EXISTS memory_fts ON memory USING GIN (to_tsvector('simple', name || ' ' || description || ' ' || content))`, `CREATE INDEX IF NOT EXISTS memory_trgm ON memory USING GIN ((name || ' ' || description || ' ' || content) gin_trgm_ops)`,
		`CREATE TABLE IF NOT EXISTS documents (id BIGSERIAL PRIMARY KEY, key TEXT UNIQUE NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('brief','report','answer','handoff','note','other')), session TEXT NOT NULL DEFAULT '', job TEXT NOT NULL DEFAULT '', body TEXT NOT NULL, sha256 TEXT NOT NULL, created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`, `CREATE INDEX IF NOT EXISTS documents_prefix ON documents(key)`, `CREATE INDEX IF NOT EXISTS documents_fts ON documents USING GIN (to_tsvector('simple', key || ' ' || body))`, `CREATE INDEX IF NOT EXISTS documents_trgm ON documents USING GIN ((key || ' ' || body) gin_trgm_ops)`, `INSERT INTO schema_version(version) VALUES (2) ON CONFLICT DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS attachments (sha256 TEXT PRIMARY KEY CHECK(sha256 ~ '^[0-9a-f]{64}$'), size_bytes BIGINT NOT NULL CHECK(size_bytes >= 0), mime TEXT NOT NULL, original_name TEXT NOT NULL, created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS attachment_refs (sha256 TEXT NOT NULL REFERENCES attachments(sha256), ref_kind TEXT NOT NULL CHECK(ref_kind IN ('checkpoint','document','memory','none')), ref_id TEXT NOT NULL DEFAULT '', PRIMARY KEY(sha256,ref_kind,ref_id))`,
		`CREATE INDEX IF NOT EXISTS attachment_refs_target ON attachment_refs(ref_kind,ref_id)`,
		`CREATE TABLE IF NOT EXISTS attachment_usage (month TEXT PRIMARY KEY CHECK(month ~ '^[0-9]{4}-[0-9]{2}$'), puts BIGINT NOT NULL DEFAULT 0, gets BIGINT NOT NULL DEFAULT 0, bytes_added BIGINT NOT NULL DEFAULT 0)`,
		`INSERT INTO schema_version(version) VALUES (3) ON CONFLICT DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS tasks (id BIGSERIAL PRIMARY KEY, lane TEXT NOT NULL, parent_lane TEXT NOT NULL DEFAULT '', title TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('implement','verify','fix','decide','ops')), state TEXT NOT NULL CHECK(state IN ('backlog','claimed','in_progress','verifying','join','hold','needs_decision','merged','dropped')), priority INTEGER NOT NULL DEFAULT 0, refs JSONB NOT NULL DEFAULT '{}'::jsonb, claimed_by TEXT NOT NULL DEFAULT '', created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS tasks_lane_state_priority ON tasks(lane, state, priority DESC, created_at ASC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS tasks_parent_lane_state_priority ON tasks(parent_lane, state, priority DESC, created_at ASC, id ASC)`,
		`CREATE TABLE IF NOT EXISTS task_events (id BIGSERIAL PRIMARY KEY, task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, "from" TEXT NOT NULL, "to" TEXT NOT NULL, "by" TEXT NOT NULL, note TEXT NOT NULL DEFAULT '', at TIMESTAMPTZ NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS task_events_task_at ON task_events(task_id, at ASC, id ASC)`,
		`INSERT INTO schema_version(version) VALUES (4) ON CONFLICT DO NOTHING`,
		// Version 5 is additive: historic events retain NULL refs while all new
		// events record the complete refs snapshot for their transition.
		`ALTER TABLE task_events ADD COLUMN IF NOT EXISTS refs JSONB`,
		`CREATE OR REPLACE FUNCTION handoffkeep_task_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'task_events is append-only'; RETURN NULL; END; $$`,
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'task_events_append_only' AND tgrelid = 'task_events'::regclass) THEN CREATE TRIGGER task_events_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON task_events FOR EACH STATEMENT EXECUTE FUNCTION handoffkeep_task_events_append_only(); END IF; END $$`,
		`INSERT INTO schema_version(version) VALUES (5) ON CONFLICT DO NOTHING`}
	stmts = append(stmts,
		`CREATE TABLE IF NOT EXISTS relay_events (id BIGSERIAL PRIMARY KEY, kind TEXT NOT NULL CHECK(kind IN ('job.completed','job.escalate','job.joined')), job_id TEXT NOT NULL, epoch INTEGER NOT NULL DEFAULT 0, owner_lane TEXT NOT NULL, machine TEXT NOT NULL DEFAULT '', pane_id TEXT NOT NULL DEFAULT '', report_path TEXT NOT NULL DEFAULT '', report_last_line TEXT NOT NULL DEFAULT '', question TEXT NOT NULL DEFAULT '', pr TEXT NOT NULL DEFAULT '', head TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '', event_time TIMESTAMPTZ, received_at TIMESTAMPTZ NOT NULL, delivered_at TIMESTAMPTZ, delivered_to TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS relay_events_idempotency ON relay_events(kind, job_id, epoch, report_path, reason)`,
		`CREATE INDEX IF NOT EXISTS relay_events_undelivered ON relay_events(owner_lane, id ASC) WHERE delivered_at IS NULL`,
		`INSERT INTO schema_version(version) VALUES (6) ON CONFLICT DO NOTHING`)
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			if q == `CREATE EXTENSION IF NOT EXISTS pg_trgm` {
				return ErrTrigramUnavailable
			}
			return err
		}
	}
	var v7Applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=7)`).Scan(&v7Applied); err != nil {
		return err
	}
	if !v7Applied {
		// Version 7 extends relay_events without changing historic job.* rows.
		// This is deliberately schema-version gated: dropping a constraint or
		// rebuilding an index takes an ACCESS EXCLUSIVE lock, so repeating it
		// at every process start would block relay writers without a migration.
		v7 := []string{
			`ALTER TABLE relay_events ADD COLUMN IF NOT EXISTS event_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE relay_events ADD COLUMN IF NOT EXISTS text TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE relay_events DROP CONSTRAINT IF EXISTS relay_events_kind_check`,
			`ALTER TABLE relay_events ADD CONSTRAINT relay_events_kind_check CHECK(kind IN ('job.completed','job.escalate','job.joined','lane.event'))`,
			`DROP INDEX IF EXISTS relay_events_idempotency`,
			`CREATE UNIQUE INDEX relay_events_idempotency ON relay_events(kind, job_id, epoch, report_path, reason) WHERE kind IN ('job.completed','job.escalate','job.joined')`,
			`CREATE UNIQUE INDEX relay_events_lane_event_idempotency ON relay_events(owner_lane, event_id) WHERE kind='lane.event'`,
			`CREATE INDEX relay_events_undelivered_kind_id ON relay_events(kind, id ASC) WHERE delivered_at IS NULL`,
			`INSERT INTO schema_version(version) VALUES (7)`,
		}
		for _, q := range v7 {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
func validName(x string) bool        { return nameRE.MatchString(x) }
func validText(x string, n int) bool { return len(x) <= n && !strings.ContainsRune(x, 0) }

func validTaskRefs(x TaskRefs) bool {
	for _, v := range []string{x.PR, x.HeadSHA, x.ReportPath, x.JobID} {
		if !validText(v, 4096) {
			return false
		}
	}
	return true
}

func rejectTaskRefs(x TaskRefs) error {
	return guard.Reject(strings.Join([]string{x.PR, x.HeadSHA, x.ReportPath, x.JobID}, "\n"))
}

// mergeTaskRefs applies only fields provided by a transition. Empty fields are
// omitted by every supported client and therefore preserve the prior value.
func mergeTaskRefs(old, patch TaskRefs) TaskRefs {
	if patch.PR != "" {
		old.PR = patch.PR
	}
	if patch.HeadSHA != "" {
		old.HeadSHA = patch.HeadSHA
	}
	if patch.ReportPath != "" {
		old.ReportPath = patch.ReportPath
	}
	if patch.JobID != "" {
		old.JobID = patch.JobID
	}
	return old
}

func validTask(x Task) bool {
	return validName(x.Lane) && (x.ParentLane == "" || validName(x.ParentLane)) && x.Title != "" && validText(x.Title, MaxBytes) && taskKinds[x.Kind] && x.CreatedBy != "" && validText(x.CreatedBy, 128) && validTaskRefs(x.Refs)
}

func scanTask(row interface{ Scan(...any) error }, x *Task) error {
	var refs []byte
	if err := row.Scan(&x.ID, &x.Lane, &x.ParentLane, &x.Title, &x.Kind, &x.State, &x.Priority, &refs, &x.ClaimedBy, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt); err != nil {
		return err
	}
	return json.Unmarshal(refs, &x.Refs)
}

const taskColumns = `id,lane,parent_lane,title,kind,state,priority,refs,claimed_by,created_by,created_at,updated_at`

func (s *Store) CreateTask(ctx context.Context, x Task) (Task, error) {
	if !validTask(x) {
		return x, errors.New("invalid task")
	}
	if err := guard.Reject(x.Title); err != nil {
		return x, err
	}
	if err := rejectTaskRefs(x.Refs); err != nil {
		return x, err
	}
	refs, err := json.Marshal(x.Refs)
	if err != nil {
		return x, errors.New("invalid task refs")
	}
	x.State, x.ClaimedBy = "backlog", ""
	x.CreatedAt = time.Now().UTC()
	x.UpdatedAt = x.CreatedAt
	err = scanTask(s.pool.QueryRow(ctx, `INSERT INTO tasks(lane,parent_lane,title,kind,state,priority,refs,claimed_by,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11) RETURNING `+taskColumns, x.Lane, x.ParentLane, x.Title, x.Kind, x.State, x.Priority, string(refs), x.ClaimedBy, x.CreatedBy, x.CreatedAt, x.UpdatedAt), &x)
	return x, err
}

// TaskTransitions is the single authoritative task state graph. README.md is
// checked against this table by TestTaskTransitionDocumentation.
var TaskTransitions = map[string]map[string]bool{
	"backlog":        {"claimed": true, "hold": true, "dropped": true},
	"claimed":        {"in_progress": true, "hold": true, "needs_decision": true, "dropped": true},
	"in_progress":    {"verifying": true, "join": true, "hold": true, "needs_decision": true, "dropped": true},
	"verifying":      {"in_progress": true, "merged": true, "hold": true, "needs_decision": true, "dropped": true},
	"join":           {"in_progress": true, "merged": true, "needs_decision": true, "dropped": true},
	"hold":           {"backlog": true, "needs_decision": true, "dropped": true},
	"needs_decision": {"backlog": true, "claimed": true, "hold": true, "dropped": true},
}

func taskTransitionAllowed(from, to string) bool {
	return TaskTransitions[from][to]
}

func (s *Store) claimTaskTx(ctx context.Context, tx pgx.Tx, id int64, by string) (Task, error) {
	if !validText(by, 128) || by == "" {
		return Task{}, errors.New("invalid task claimant")
	}
	var x Task
	if err := scanTask(tx.QueryRow(ctx, `UPDATE tasks SET state='claimed', claimed_by=$2, updated_at=$3 WHERE id=$1 AND state='backlog' RETURNING `+taskColumns, id, by, time.Now().UTC()), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, ErrTaskConflict
		}
		return Task{}, err
	}
	refs, err := json.Marshal(x.Refs)
	if err != nil {
		return Task{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,refs,at) VALUES($1,'backlog','claimed',$2,'',$3::jsonb,$4)`, id, by, string(refs), x.UpdatedAt); err != nil {
		return Task{}, err
	}
	return x, nil
}

func (s *Store) ClaimTask(ctx context.Context, id int64, by string) (Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
	x, err := s.claimTaskTx(ctx, tx, id, by)
	if err != nil {
		return Task{}, err
	}
	return x, tx.Commit(ctx)
}

// NextTask claims the oldest highest-priority runnable task in one transaction.
func (s *Store) NextTask(ctx context.Context, lane, by string) (Task, error) {
	if !validName(lane) {
		return Task{}, errors.New("invalid task lane")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
	var id int64
	err = tx.QueryRow(ctx, `SELECT id FROM tasks WHERE lane=$1 AND state='backlog' ORDER BY priority DESC,created_at ASC,id ASC FOR UPDATE SKIP LOCKED LIMIT 1`, lane).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, ErrQueueEmpty
	}
	if err != nil {
		return Task{}, err
	}
	x, err := s.claimTaskTx(ctx, tx, id, by)
	if err != nil {
		return Task{}, err
	}
	return x, tx.Commit(ctx)
}

func (s *Store) TransitionTask(ctx context.Context, id int64, to, by, note string, refs *TaskRefs) (Task, error) {
	if !taskStates[to] || !validText(by, 128) || by == "" || !validText(note, MaxBytes) || (refs != nil && !validTaskRefs(*refs)) {
		return Task{}, errors.New("invalid task transition")
	}
	if err := guard.Reject(note); err != nil {
		return Task{}, err
	}
	if refs != nil {
		if err := rejectTaskRefs(*refs); err != nil {
			return Task{}, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, ErrTaskNotFound
		}
		return Task{}, err
	}
	from := x.State
	if !taskTransitionAllowed(from, to) {
		return Task{}, ErrTaskConflict
	}
	if to == "needs_decision" && strings.TrimSpace(note) == "" {
		return Task{}, errors.New("needs_decision requires question")
	}
	if refs != nil {
		x.Refs = mergeTaskRefs(x.Refs, *refs)
	}
	encoded, err := json.Marshal(x.Refs)
	if err != nil {
		return Task{}, err
	}
	x.UpdatedAt = time.Now().UTC()
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET state=$2,refs=$3::jsonb,updated_at=$4 WHERE id=$1 RETURNING `+taskColumns, id, to, string(encoded), x.UpdatedAt), &x); err != nil {
		return Task{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,refs,at) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7)`, id, from, to, by, note, string(encoded), x.UpdatedAt); err != nil {
		return Task{}, err
	}
	return x, tx.Commit(ctx)
}

func (s *Store) ListTasks(ctx context.Context, lane, state, parentLane string, limit int) ([]Task, error) {
	if (lane != "" && !validName(lane)) || (parentLane != "" && !validName(parentLane)) || (state != "" && !taskStates[state]) {
		return nil, errors.New("invalid task query")
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	q, args := `SELECT `+taskColumns+` FROM tasks WHERE 1=1`, []any{}
	if lane != "" {
		args = append(args, lane)
		q += fmt.Sprintf(" AND lane=$%d", len(args))
	}
	if parentLane != "" {
		args = append(args, parentLane)
		q += fmt.Sprintf(" AND parent_lane=$%d", len(args))
	}
	if state != "" {
		args = append(args, state)
		q += fmt.Sprintf(" AND state=$%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY priority DESC,created_at ASC,id ASC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
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

func (s *Store) GetTask(ctx context.Context, id int64) (Task, bool, error) {
	var x Task
	if err := scanTask(s.pool.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, false, nil
		}
		return Task{}, false, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,task_id,"from","to","by",note,refs,at FROM task_events WHERE task_id=$1 ORDER BY at,id`, id)
	if err != nil {
		return Task{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var e TaskEvent
		var refs []byte
		if err := rows.Scan(&e.ID, &e.TaskID, &e.From, &e.To, &e.By, &e.Note, &refs, &e.At); err != nil {
			return Task{}, false, err
		}
		if len(refs) != 0 {
			e.Refs = &TaskRefs{}
			if err := json.Unmarshal(refs, e.Refs); err != nil {
				return Task{}, false, err
			}
		}
		x.Events = append(x.Events, e)
	}
	return x, true, rows.Err()
}

const relayEventColumns = `id,kind,job_id,epoch,owner_lane,machine,pane_id,report_path,report_last_line,question,pr,head,reason,event_id,text,event_time,received_at,delivered_at,delivered_to,attempts`

func scanRelayEvent(row interface{ Scan(...any) error }, x *RelayEvent) error {
	return row.Scan(
		&x.ID, &x.Kind, &x.JobID, &x.Epoch, &x.OwnerLane, &x.Machine,
		&x.PaneID, &x.ReportPath, &x.ReportLastLine, &x.Question, &x.PR,
		&x.Head, &x.Reason, &x.EventID, &x.Text, &x.EventTime, &x.ReceivedAt, &x.DeliveredAt,
		&x.DeliveredTo, &x.Attempts,
	)
}

func scanRelayEventCreated(row interface{ Scan(...any) error }, x *RelayEvent, created *bool) error {
	return row.Scan(
		&x.ID, &x.Kind, &x.JobID, &x.Epoch, &x.OwnerLane, &x.Machine,
		&x.PaneID, &x.ReportPath, &x.ReportLastLine, &x.Question, &x.PR,
		&x.Head, &x.Reason, &x.EventID, &x.Text, &x.EventTime, &x.ReceivedAt, &x.DeliveredAt,
		&x.DeliveredTo, &x.Attempts, created,
	)
}

func validRelayEvent(x RelayEvent) bool {
	if !relayEventKinds[x.Kind] || x.OwnerLane == "" || !validText(x.OwnerLane, MaxBytes) {
		return false
	}
	// Validate every request-owned text column for every kind before the
	// lane.event-specific contract below. Otherwise an authenticated sender
	// could use ignored job-shaped fields as an unbounded storage bypass.
	for _, v := range []string{x.Kind, x.JobID, x.OwnerLane, x.Machine, x.PaneID, x.ReportPath, x.ReportLastLine, x.Question, x.PR, x.Head, x.Reason, x.EventID, x.Text} {
		if !validText(v, MaxBytes) {
			return false
		}
	}
	if x.Kind == "lane.event" {
		return x.EventID != "" && validText(x.EventID, MaxBytes) && validLaneEventText(x.Text)
	}
	if x.JobID == "" || x.EventID != "" || x.Text != "" {
		return false
	}
	return true
}

func validLaneEventText(value string) bool {
	if len(value) == 0 || len(value) > RelayLaneEventMaxBytes {
		return false
	}
	for _, r := range value {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	return true
}

// AppendRelayEvent inserts a durable relay event. Its conflict update is part
// of the same statement so concurrent duplicate submissions share one row.
func (s *Store) AppendRelayEvent(ctx context.Context, x RelayEvent) (RelayEvent, bool, error) {
	if !validRelayEvent(x) {
		return x, false, errors.New("invalid relay event")
	}
	if err := guard.Reject(x.Question); err != nil {
		return x, false, err
	}
	if err := guard.Reject(x.Reason); err != nil {
		return x, false, err
	}
	// These fields are server-owned. Zero-value strings also normalize omitted
	// request fields to the non-NULL defaults required by the idempotency key.
	x.ID, x.ReceivedAt, x.DeliveredAt, x.DeliveredTo, x.Attempts = 0, time.Time{}, nil, "", 0
	var created bool
	query := `INSERT INTO relay_events(kind,job_id,epoch,owner_lane,machine,pane_id,report_path,report_last_line,question,pr,head,reason,event_id,text,event_time,received_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,now())`
	if x.Kind == "lane.event" {
		query += ` ON CONFLICT (owner_lane,event_id) WHERE kind='lane.event' DO UPDATE SET attempts=relay_events.attempts+1`
	} else {
		query += ` ON CONFLICT (kind,job_id,epoch,report_path,reason) WHERE kind IN ('job.completed','job.escalate','job.joined') DO UPDATE SET attempts=relay_events.attempts+1`
	}
	err := scanRelayEventCreated(s.pool.QueryRow(ctx, query+` RETURNING `+relayEventColumns+`,(xmax=0) AS created`, x.Kind, x.JobID, x.Epoch, x.OwnerLane, x.Machine, x.PaneID, x.ReportPath, x.ReportLastLine, x.Question, x.PR, x.Head, x.Reason, x.EventID, x.Text, x.EventTime), &x, &created)
	if err != nil {
		return x, false, err
	}
	return x, created, nil
}

// MarkRelayEventDelivered records the first successful delivery. Subsequent
// calls are intentionally idempotent and return the original delivery time.
func (s *Store) MarkRelayEventDelivered(ctx context.Context, id int64, machine, pane string) (RelayEvent, error) {
	if id < 1 || !validText(machine, MaxBytes) || !validText(pane, MaxBytes) {
		return RelayEvent{}, errors.New("invalid relay event delivery")
	}
	var x RelayEvent
	err := scanRelayEvent(s.pool.QueryRow(ctx, `UPDATE relay_events SET delivered_at=now(),delivered_to=$2 WHERE id=$1 AND delivered_at IS NULL RETURNING `+relayEventColumns, id, machine+"/"+pane), &x)
	if err == nil {
		return x, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RelayEvent{}, err
	}
	if err = scanRelayEvent(s.pool.QueryRow(ctx, `SELECT `+relayEventColumns+` FROM relay_events WHERE id=$1`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RelayEvent{}, ErrRelayEventNotFound
		}
		return RelayEvent{}, err
	}
	return x, nil
}

// ListRelayEvents returns relay records in durable insertion order.
func (s *Store) ListRelayEvents(ctx context.Context, lane string, undelivered bool, limit int) ([]RelayEvent, error) {
	return s.ListRelayEventsPage(ctx, lane, "", undelivered, 0, limit)
}

// ListRelayEventsPage is the additive cursor form of ListRelayEvents. afterID
// is exclusive so a caller can safely advance using the last row it observed.
func (s *Store) ListRelayEventsPage(ctx context.Context, lane, kind string, undelivered bool, afterID int64, limit int) ([]RelayEvent, error) {
	if !validText(lane, MaxBytes) || (kind != "" && !relayEventKinds[kind]) || afterID < 0 {
		return nil, errors.New("invalid relay event query")
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	q, args := `SELECT `+relayEventColumns+` FROM relay_events WHERE 1=1`, []any{}
	if lane != "" {
		args = append(args, lane)
		q += fmt.Sprintf(" AND owner_lane=$%d", len(args))
	}
	if kind != "" {
		args = append(args, kind)
		q += fmt.Sprintf(" AND kind=$%d", len(args))
	}
	if undelivered {
		q += " AND delivered_at IS NULL"
	}
	if afterID > 0 {
		args = append(args, afterID)
		q += fmt.Sprintf(" AND id>$%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY id ASC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RelayEvent{}
	for rows.Next() {
		var x RelayEvent
		if err := scanRelayEvent(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) CreateCheckpoint(ctx context.Context, x Checkpoint) (Checkpoint, error) {
	if !validName(x.Session) || !kinds[x.Kind] || x.Title == "" || !validText(x.Title, MaxBytes) || !validText(x.Body, MaxBytes) || !validName(x.CreatedBy) {
		return x, errors.New("invalid checkpoint")
	}
	if e := guard.Reject(x.Title + "\n" + x.Body); e != nil {
		return x, e
	}
	refs, e := json.Marshal(x.Refs)
	if e != nil || len(refs) > MaxBytes {
		return x, errors.New("invalid checkpoint refs")
	}
	if e = guard.Reject(string(refs)); e != nil {
		return x, e
	}
	x.CreatedAt = time.Now().UTC()
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return x, e
	}
	defer tx.Rollback(ctx)
	e = tx.QueryRow(ctx, `INSERT INTO checkpoints(session,kind,title,body,refs,created_by,created_at) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7) RETURNING id`, x.Session, x.Kind, x.Title, x.Body, string(refs), x.CreatedBy, x.CreatedAt).Scan(&x.ID)
	if e != nil {
		return x, e
	}
	_, e = tx.Exec(ctx, `DELETE FROM checkpoints WHERE session=$1 AND id IN (SELECT id FROM checkpoints WHERE session=$1 ORDER BY created_at DESC,id DESC OFFSET $2)`, x.Session, CheckpointKeep)
	if e != nil {
		return x, e
	}
	return x, tx.Commit(ctx)
}
func (s *Store) RecentCheckpoints(ctx context.Context, session, kind string, limit int) ([]Checkpoint, error) {
	if !validName(session) || (kind != "" && !kinds[kind]) {
		return nil, errors.New("invalid checkpoint query")
	}
	if limit < 1 {
		limit = 3
	}
	if limit > CheckpointKeep {
		limit = CheckpointKeep
	}
	q := `SELECT id,session,kind,title,body,refs,created_by,created_at FROM checkpoints WHERE session=$1`
	args := []any{session}
	if kind != "" {
		q += " AND kind=$2 ORDER BY created_at DESC,id DESC LIMIT $3"
		args = append(args, kind, limit)
	} else {
		q += " ORDER BY created_at DESC,id DESC LIMIT $2"
		args = append(args, limit)
	}
	rows, e := s.pool.Query(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Checkpoint{}
	for rows.Next() {
		var x Checkpoint
		var r []byte
		if e = rows.Scan(&x.ID, &x.Session, &x.Kind, &x.Title, &x.Body, &r, &x.CreatedBy, &x.CreatedAt); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(r, &x.Refs); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) PutMemory(ctx context.Context, x Memory) (Memory, error) {
	if !validName(x.Agent) || !validName(x.Name) || !memoryTypes[x.Type] || !validText(x.Description, MaxBytes) || !validText(x.Content, MaxBytes) || !validName(x.UpdatedBy) {
		return x, errors.New("invalid memory")
	}
	if e := guard.Reject(x.Description + "\n" + x.Content); e != nil {
		return x, e
	}
	x.UpdatedAt = time.Now().UTC()
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return x, e
	}
	defer tx.Rollback(ctx)
	_, e = tx.Exec(ctx, `INSERT INTO memory(agent,name,description,memory_type,content,updated_by,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(agent,name) DO UPDATE SET description=EXCLUDED.description,memory_type=EXCLUDED.memory_type,content=EXCLUDED.content,updated_by=EXCLUDED.updated_by,updated_at=EXCLUDED.updated_at`, x.Agent, x.Name, x.Description, x.Type, x.Content, x.UpdatedBy, x.UpdatedAt)
	if e != nil {
		return x, e
	}
	_, e = tx.Exec(ctx, `DELETE FROM memory WHERE agent=$1 AND name IN (SELECT name FROM memory WHERE agent=$1 ORDER BY updated_at DESC,name DESC OFFSET $2)`, x.Agent, MemoryKeep)
	if e != nil {
		return x, e
	}
	return x, tx.Commit(ctx)
}
func (s *Store) ListMemory(ctx context.Context, agent string, content bool) ([]Memory, error) {
	if !validName(agent) {
		return nil, errors.New("invalid memory agent")
	}
	cols := "agent,name,description,memory_type,updated_by,updated_at"
	if content {
		cols = "agent,name,description,memory_type,content,updated_by,updated_at"
	}
	rows, e := s.pool.Query(ctx, "SELECT "+cols+" FROM memory WHERE agent=$1 ORDER BY name", agent)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Memory{}
	for rows.Next() {
		var x Memory
		if content {
			e = rows.Scan(&x.Agent, &x.Name, &x.Description, &x.Type, &x.Content, &x.UpdatedBy, &x.UpdatedAt)
		} else {
			e = rows.Scan(&x.Agent, &x.Name, &x.Description, &x.Type, &x.UpdatedBy, &x.UpdatedAt)
		}
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) GetMemory(ctx context.Context, agent, name string) (Memory, bool, error) {
	if !validName(agent) || !validName(name) {
		return Memory{}, false, errors.New("invalid memory")
	}
	var x Memory
	e := s.pool.QueryRow(ctx, `SELECT agent,name,description,memory_type,content,updated_by,updated_at FROM memory WHERE agent=$1 AND name=$2`, agent, name).Scan(&x.Agent, &x.Name, &x.Description, &x.Type, &x.Content, &x.UpdatedBy, &x.UpdatedAt)
	if e != nil {
		if strings.Contains(e.Error(), "no rows") {
			return x, false, nil
		}
		return x, false, e
	}
	return x, true, nil
}
func validDocKey(k string) bool {
	return len(k) > 0 && len(k) <= 512 && !strings.HasPrefix(k, "/") && !strings.Contains(k, "..") && !strings.ContainsRune(k, 0)
}
func (s *Store) PutDocument(ctx context.Context, x Document) (Document, bool, error) {
	if !validDocKey(x.Key) || !documentKinds[x.Kind] || !validText(x.Session, 128) || !validText(x.Job, 128) || !validText(x.Body, DocumentMaxBytes) || !validName(x.CreatedBy) {
		return x, false, errors.New("invalid document")
	}
	if e := guard.Reject(x.Key + "\n" + x.Body); e != nil {
		return x, false, e
	}
	sum := sha256.Sum256([]byte(x.Body))
	x.SHA256 = hex.EncodeToString(sum[:])
	now := time.Now().UTC()
	var existing string
	e := s.pool.QueryRow(ctx, "SELECT sha256 FROM documents WHERE key=$1", x.Key).Scan(&existing)
	if e == nil && existing == x.SHA256 {
		return x, false, nil
	}
	if e != nil && !strings.Contains(e.Error(), "no rows") {
		return x, false, e
	}
	e = s.pool.QueryRow(ctx, `INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8) ON CONFLICT(key) DO UPDATE SET kind=EXCLUDED.kind,session=EXCLUDED.session,job=EXCLUDED.job,body=EXCLUDED.body,sha256=EXCLUDED.sha256,created_by=EXCLUDED.created_by,updated_at=EXCLUDED.updated_at RETURNING id,created_at,updated_at`, x.Key, x.Kind, x.Session, x.Job, x.Body, x.SHA256, x.CreatedBy, now).Scan(&x.ID, &x.CreatedAt, &x.UpdatedAt)
	return x, true, e
}
func (s *Store) GetDocument(ctx context.Context, key string) (Document, bool, error) {
	if !validDocKey(key) {
		return Document{}, false, errors.New("invalid document key")
	}
	var x Document
	e := s.pool.QueryRow(ctx, `SELECT id,key,kind,session,job,body,sha256,created_by,created_at,updated_at FROM documents WHERE key=$1`, key).Scan(&x.ID, &x.Key, &x.Kind, &x.Session, &x.Job, &x.Body, &x.SHA256, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt)
	if e != nil {
		if strings.Contains(e.Error(), "no rows") {
			return x, false, nil
		}
		return x, false, e
	}
	return x, true, nil
}
func (s *Store) ListDocuments(ctx context.Context, prefix, kind, session string, limit int) ([]Document, error) {
	if !validText(prefix, 512) || !validText(session, 128) || (kind != "" && !documentKinds[kind]) {
		return nil, errors.New("invalid document query")
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, e := s.pool.Query(ctx, `SELECT id,key,kind,session,job,sha256,created_by,created_at,updated_at FROM documents WHERE ($1='' OR key LIKE $1 || '%') AND ($2='' OR kind=$2) AND ($3='' OR session=$3) ORDER BY key LIMIT $4`, prefix, kind, session, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		var x Document
		if e = rows.Scan(&x.ID, &x.Key, &x.Kind, &x.Session, &x.Job, &x.SHA256, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func validAttachmentRef(kind, id string) bool {
	if kind == "none" {
		return id == ""
	}
	if kind == "checkpoint" {
		_, e := strconv.ParseInt(id, 10, 64)
		return e == nil && id != ""
	}
	if kind == "document" {
		return validDocKey(id)
	}
	if kind == "memory" {
		a, n, ok := strings.Cut(id, "/")
		return ok && validName(a) && validName(n)
	}
	return false
}
func monthNow() string { return time.Now().UTC().Format("2006-01") }
func (s *Store) GetAttachment(ctx context.Context, sha string) (Attachment, bool, error) {
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sha) {
		return Attachment{}, false, errors.New("invalid attachment sha")
	}
	var x Attachment
	e := s.pool.QueryRow(ctx, `SELECT sha256,size_bytes,mime,original_name,created_by,created_at FROM attachments WHERE sha256=$1`, sha).Scan(&x.SHA256, &x.SizeBytes, &x.MIME, &x.OriginalName, &x.CreatedBy, &x.CreatedAt)
	if e != nil {
		if strings.Contains(e.Error(), "no rows") {
			return x, false, nil
		}
		return x, false, e
	}
	return x, true, nil
}

// PutAttachment records an immutable object and its reference after R2 PUT has
// succeeded. It serializes quota accounting with a locked monthly usage row.
func (s *Store) PutAttachment(ctx context.Context, x Attachment, storageCap, putCap int64) (Attachment, bool, error) {
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(x.SHA256) || x.SizeBytes < 0 || x.MIME == "" || !validText(x.OriginalName, 512) || !validName(x.CreatedBy) || !validAttachmentRef(x.RefKind, x.RefID) {
		return x, false, errors.New("invalid attachment")
	}
	if x.RefKind == "" {
		x.RefKind = "none"
	}
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return x, false, e
	}
	defer tx.Rollback(ctx)
	month := monthNow()
	if _, e = tx.Exec(ctx, `INSERT INTO attachment_usage(month) VALUES($1) ON CONFLICT DO NOTHING`, month); e != nil {
		return x, false, e
	}
	var puts, total int64
	if e = tx.QueryRow(ctx, `SELECT puts FROM attachment_usage WHERE month=$1 FOR UPDATE`, month).Scan(&puts); e != nil {
		return x, false, e
	}
	if e = tx.QueryRow(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM attachments`).Scan(&total); e != nil {
		return x, false, e
	}
	var present bool
	e = tx.QueryRow(ctx, `SELECT true FROM attachments WHERE sha256=$1`, x.SHA256).Scan(&present)
	if e != nil && !strings.Contains(e.Error(), "no rows") {
		return x, false, e
	}
	if !present {
		if total+x.SizeBytes > storageCap {
			return x, false, ErrAttachmentStorageCap
		}
		if puts+1 > putCap {
			return x, false, ErrAttachmentPutCap
		}
		x.CreatedAt = time.Now().UTC()
		if _, e = tx.Exec(ctx, `INSERT INTO attachments(sha256,size_bytes,mime,original_name,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6)`, x.SHA256, x.SizeBytes, x.MIME, x.OriginalName, x.CreatedBy, x.CreatedAt); e != nil {
			return x, false, e
		}
		if _, e = tx.Exec(ctx, `UPDATE attachment_usage SET puts=puts+1,bytes_added=bytes_added+$2 WHERE month=$1`, month, x.SizeBytes); e != nil {
			return x, false, e
		}
	} else {
		if e = tx.QueryRow(ctx, `SELECT size_bytes,mime,original_name,created_by,created_at FROM attachments WHERE sha256=$1`, x.SHA256).Scan(&x.SizeBytes, &x.MIME, &x.OriginalName, &x.CreatedBy, &x.CreatedAt); e != nil {
			return x, false, e
		}
	}
	if _, e = tx.Exec(ctx, `INSERT INTO attachment_refs(sha256,ref_kind,ref_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, x.SHA256, x.RefKind, x.RefID); e != nil {
		return x, false, e
	}
	return x, !present, tx.Commit(ctx)
}

// CheckAttachmentCapacity is the preflight fuse before an R2 PUT. PutAttachment
// repeats these checks in its transaction to make accounting authoritative.
func (s *Store) CheckAttachmentCapacity(ctx context.Context, size, storageCap, putCap int64) error {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	m := monthNow()
	if _, e = tx.Exec(ctx, `INSERT INTO attachment_usage(month) VALUES($1) ON CONFLICT DO NOTHING`, m); e != nil {
		return e
	}
	var puts, total int64
	if e = tx.QueryRow(ctx, `SELECT puts FROM attachment_usage WHERE month=$1 FOR UPDATE`, m).Scan(&puts); e != nil {
		return e
	}
	if e = tx.QueryRow(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM attachments`).Scan(&total); e != nil {
		return e
	}
	if total+size > storageCap {
		return ErrAttachmentStorageCap
	}
	if puts+1 > putCap {
		return ErrAttachmentPutCap
	}
	return tx.Commit(ctx)
}

var ErrAttachmentStorageCap = errors.New("attachment_storage_cap")
var ErrAttachmentPutCap = errors.New("attachment_put_cap")

func (s *Store) RecordAttachmentGet(ctx context.Context, sha string, getCap int64) error {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var exists bool
	if e = tx.QueryRow(ctx, `SELECT true FROM attachments WHERE sha256=$1`, sha).Scan(&exists); e != nil {
		return e
	}
	m := monthNow()
	if _, e = tx.Exec(ctx, `INSERT INTO attachment_usage(month) VALUES($1) ON CONFLICT DO NOTHING`, m); e != nil {
		return e
	}
	var gets int64
	if e = tx.QueryRow(ctx, `SELECT gets FROM attachment_usage WHERE month=$1 FOR UPDATE`, m).Scan(&gets); e != nil {
		return e
	}
	// Reads deliberately remain available above the free-tier cap. The value is
	// still exposed so operators can see that the fuse threshold was crossed.
	_ = getCap
	if _, e = tx.Exec(ctx, `UPDATE attachment_usage SET gets=gets+1 WHERE month=$1`, m); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Store) ListAttachments(ctx context.Context, refKind, refID string, limit int) ([]Attachment, error) {
	if refKind != "" && !validAttachmentRef(refKind, refID) {
		return nil, errors.New("invalid attachment ref")
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	q := `SELECT a.sha256,a.size_bytes,a.mime,a.original_name,a.created_by,a.created_at,r.ref_kind,r.ref_id FROM attachments a JOIN attachment_refs r ON r.sha256=a.sha256`
	args := []any{limit}
	if refKind != "" {
		q += ` WHERE r.ref_kind=$1 AND r.ref_id=$2 ORDER BY a.created_at DESC LIMIT $3`
		args = []any{refKind, refID, limit}
	} else {
		q += ` ORDER BY a.created_at DESC LIMIT $1`
	}
	rows, e := s.pool.Query(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		var x Attachment
		if e = rows.Scan(&x.SHA256, &x.SizeBytes, &x.MIME, &x.OriginalName, &x.CreatedBy, &x.CreatedAt, &x.RefKind, &x.RefID); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) AttachmentUsage(ctx context.Context) (AttachmentUsage, error) {
	x := AttachmentUsage{Month: monthNow()}
	e := s.pool.QueryRow(ctx, `SELECT puts,gets,bytes_added FROM attachment_usage WHERE month=$1`, x.Month).Scan(&x.Puts, &x.Gets, &x.BytesAdded)
	if e != nil && strings.Contains(e.Error(), "no rows") {
		e = nil
	}
	if e != nil {
		return x, e
	}
	e = s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM attachments`).Scan(&x.TotalBytes)
	return x, e
}
func (s *Store) AttachmentObjectCount(ctx context.Context) (int64, error) {
	var n int64
	return n, s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM attachments`).Scan(&n)
}
func (s *Store) Search(ctx context.Context, q, scope, session string, limit int) ([]SearchResult, error) {
	if strings.TrimSpace(q) == "" || len(q) > 512 || !validText(session, 128) || (scope != "all" && scope != "ctx" && scope != "docs" && scope != "memory") {
		return nil, errors.New("invalid search")
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	parts := []string{}
	if scope == "all" || scope == "ctx" {
		parts = append(parts, `SELECT 'ctx' scope,'' key,session,kind,title,ts_headline('simple',body,plainto_tsquery('simple',$2),'MaxWords=24,MinWords=8') snippet,refs,created_at FROM checkpoints WHERE ($1='' OR session=$1) AND (to_tsvector('simple',title || ' ' || body) @@ plainto_tsquery('simple',$2) OR title || ' ' || body ILIKE '%' || $2 || '%')`)
	}
	if scope == "all" || scope == "memory" {
		parts = append(parts, `SELECT 'memory' scope,'' key,agent session,memory_type kind,name title,ts_headline('simple',content,plainto_tsquery('simple',$2),'MaxWords=24,MinWords=8') snippet,'{}'::jsonb refs,updated_at created_at FROM memory WHERE ($1='' OR agent=$1) AND (to_tsvector('simple',name || ' ' || description || ' ' || content) @@ plainto_tsquery('simple',$2) OR name || ' ' || description || ' ' || content ILIKE '%' || $2 || '%')`)
	}
	if scope == "all" || scope == "docs" {
		parts = append(parts, `SELECT 'docs' scope,key,session,kind,key title,ts_headline('simple',body,plainto_tsquery('simple',$2),'MaxWords=24,MinWords=8') snippet,'{}'::jsonb refs,created_at FROM documents WHERE ($1='' OR session=$1) AND (to_tsvector('simple',key || ' ' || body) @@ plainto_tsquery('simple',$2) OR key || ' ' || body ILIKE '%' || $2 || '%')`)
	}
	rows, e := s.pool.Query(ctx, `SELECT scope,key,session,kind,title,snippet,refs,created_at FROM (`+strings.Join(parts, " UNION ALL ")+") r ORDER BY created_at DESC LIMIT $3", session, q, limit)
	if e != nil {
		return nil, fmt.Errorf("search: %w", e)
	}
	defer rows.Close()
	out := []SearchResult{}
	for rows.Next() {
		var x SearchResult
		var refs []byte
		if e = rows.Scan(&x.Scope, &x.Key, &x.Session, &x.Kind, &x.Title, &x.Snippet, &refs, &x.CreatedAt); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(refs, &x.Refs); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
