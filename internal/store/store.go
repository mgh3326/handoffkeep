// Package store owns PostgreSQL persistence and validation.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

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
	ErrTaskConflict         = errors.New("task_conflict")
	ErrTaskNotFound         = errors.New("task_not_found")
	ErrRelayEventNotFound   = errors.New("relay_event_not_found")
	ErrDeviationRefRequired = errors.New("deviation_ref_required")
	ErrDecidedByRequired    = errors.New("decided_by_required")
	// ErrBenchCatalogMonotonicity is returned when a catalog batch would leave
	// a profile whose grade falls as effort rises (higher effort must not
	// degrade to a worse grade).
	ErrBenchCatalogMonotonicity = errors.New("bench_catalog_not_monotonic")
	// ErrBenchCatalogSolGrade enforces the scopefuel _SOL_PROFILES rule on the
	// server: Sol profiles may only ever be graded S+.
	ErrBenchCatalogSolGrade = errors.New("bench_catalog_sol_grade")
	// ErrQueueEmpty is deliberately distinct from a missing task.  It lets
	// queue consumers treat an empty lane as an expected terminal condition.
	ErrQueueEmpty = errors.New("queue_empty")
	// ErrTaskTerminal rejects a relane on a merged or dropped task. Terminal
	// rows are history: moving them between lanes never returns them to a
	// queue and would falsify lane-level end-state reports.
	ErrTaskTerminal = errors.New("task_terminal")
	// ErrTaskLaneUnknown rejects a relane target outside the set of lanes any
	// current row uses. It exists so a mistyped lane name cannot quietly
	// strand a task where no lane owner ever lists it.
	ErrTaskLaneUnknown = errors.New("unknown_lane")
)

// TaskRefs holds the durable links that let a captain resume work without
// embedding credentials or implementation details in the queue itself.
type TaskRefs struct {
	PR              string           `json:"pr,omitempty"`
	HeadSHA         string           `json:"head_sha,omitempty"`
	ReportPath      string           `json:"report_path,omitempty"`
	JobID           string           `json:"job_id,omitempty"`
	DecisionOptions *DecisionOptions `json:"decision_options,omitempty"`
	Linear          *TaskLinear      `json:"linear,omitempty"`
	// OriginPR and OriginTask are the typed "this row came from" relations
	// shared with #494 (hk:doc design/2026-09-21/task493-disposition-contract
	// §3). Unknown refs keys are dropped by the next transition, so these names
	// are part of the durable contract, not presentation.
	OriginPR    string       `json:"origin_pr,omitempty"`
	OriginTask  int64        `json:"origin_task,omitempty"`
	Disposition *Disposition `json:"disposition,omitempty"`
}

type TaskLinear struct {
	Sync      bool     `json:"sync"`
	Tier      string   `json:"tier,omitempty"`
	Grade     string   `json:"grade,omitempty"`
	Brief     string   `json:"brief,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Report    string   `json:"report,omitempty"`
	Verify    string   `json:"verify,omitempty"`
	Decision  string   `json:"decision,omitempty"`
	DeploySHA string   `json:"deploy_sha,omitempty"`
}

// DecisionOption is one bounded, operator-visible answer for a decision.
// Keys are deliberately compact so the same representation can be carried in
// a lane event without consuming much of its 2048-byte limit.
type DecisionOption struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Recommended bool   `json:"recommended,omitempty"`
}

// DecisionOptions describes the choices attached to a decision. AllowFree is
// true unless an API payload explicitly supplies false; command producers set
// it explicitly so their intent is unambiguous.
type DecisionOptions struct {
	Options   []DecisionOption `json:"options"`
	AllowFree bool             `json:"allow_free"`
}

// UnmarshalJSON preserves the wire default for older producers which omit
// allow_free while retaining an explicit false from newer ones.
func (x *DecisionOptions) UnmarshalJSON(data []byte) error {
	var raw struct {
		Options   []DecisionOption `json:"options"`
		AllowFree *bool            `json:"allow_free"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	x.Options = raw.Options
	x.AllowFree = true
	if raw.AllowFree != nil {
		x.AllowFree = *raw.AllowFree
	}
	return nil
}

// FormatDecisionOptions returns the closed, one-line lane-event syntax. Its
// callers first validate the value; this formatter never truncates data.
func FormatDecisionOptions(x DecisionOptions) string {
	parts := make([]string, 0, len(x.Options)+2)
	recommended := ""
	for _, option := range x.Options {
		parts = append(parts, option.Key+"|"+option.Label)
		if option.Recommended {
			recommended = option.Key
		}
	}
	if recommended != "" {
		parts = append(parts, "rec="+recommended)
	}
	if !x.AllowFree {
		parts = append(parts, "free=0")
	}
	return "[options] " + strings.Join(parts, ";")
}

// ParseDecisionOptions accepts only a complete option grammar on the final
// line. Invalid input is intentionally returned untouched: callers must never
// render a partially understood choice set as authoritative.
func ParseDecisionOptions(text string) (body string, options DecisionOptions, ok bool) {
	lineStart := strings.LastIndexByte(text, '\n')
	last := text
	if lineStart >= 0 {
		last = text[lineStart+1:]
	}
	if !strings.HasPrefix(last, "[options] ") {
		return text, DecisionOptions{}, false
	}
	raw := strings.TrimPrefix(last, "[options] ")
	if raw == "" {
		return text, DecisionOptions{}, false
	}
	parts := strings.Split(raw, ";")
	parsed := DecisionOptions{AllowFree: true}
	seen := map[string]bool{}
	stage := 0 // 0=options, 1=rec, 2=free
	for _, part := range parts {
		if part == "" {
			return text, DecisionOptions{}, false
		}
		switch {
		case strings.HasPrefix(part, "rec="):
			if stage != 0 || len(part) != len("rec=")+1 {
				return text, DecisionOptions{}, false
			}
			key := strings.TrimPrefix(part, "rec=")
			if !validDecisionOptionKey(key) || !seen[key] {
				return text, DecisionOptions{}, false
			}
			for index := range parsed.Options {
				parsed.Options[index].Recommended = parsed.Options[index].Key == key
			}
			stage = 1
		case part == "free=0":
			if stage == 2 {
				return text, DecisionOptions{}, false
			}
			parsed.AllowFree = false
			stage = 2
		default:
			if stage != 0 {
				return text, DecisionOptions{}, false
			}
			key, label, found := strings.Cut(part, "|")
			if !found || strings.Contains(label, "|") || !validDecisionOptionKey(key) || seen[key] || !validDecisionOptionLabel(label) || len(parsed.Options) == 6 {
				return text, DecisionOptions{}, false
			}
			seen[key] = true
			parsed.Options = append(parsed.Options, DecisionOption{Key: key, Label: label})
		}
	}
	if !validDecisionOptions(parsed) {
		return text, DecisionOptions{}, false
	}
	if lineStart < 0 {
		return "", parsed, true
	}
	return text[:lineStart], parsed, true
}

func validDecisionOptionKey(key string) bool {
	return len(key) == 1 && key[0] >= 'A' && key[0] <= 'F'
}

func validDecisionOptionLabel(label string) bool {
	if label != strings.TrimSpace(label) || len(label) == 0 || len(label) > 120 || strings.ContainsAny(label, "|;\n\r") {
		return false
	}
	for _, r := range label {
		if r == 0 || r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	return true
}

func validDecisionOptions(x DecisionOptions) bool {
	if len(x.Options) < 1 || len(x.Options) > 6 {
		return false
	}
	seen := map[string]bool{}
	recommended := 0
	for _, option := range x.Options {
		if !validDecisionOptionKey(option.Key) || seen[option.Key] || !validDecisionOptionLabel(option.Label) {
			return false
		}
		seen[option.Key] = true
		if option.Recommended {
			recommended++
		}
	}
	return recommended <= 1
}

// ValidateDecisionOptions is exported for local producers such as the CLI.
func ValidateDecisionOptions(x DecisionOptions) error {
	if !validDecisionOptions(x) {
		return errors.New("invalid decision options")
	}
	return nil
}

// TaskEvent kind values. "transition" rows carry state names in from/to and
// dominate history; "relane" rows carry lane names and are produced only by
// RelaneTask. Consumers that read "to" as a state must filter kind.
const (
	TaskEventTransition = "transition"
	TaskEventRelane     = "relane"
)

type TaskEvent struct {
	ID     int64     `json:"id"`
	TaskID int64     `json:"task_id"`
	Kind   string    `json:"kind"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	By     string    `json:"by"`
	Note   string    `json:"note,omitempty"`
	Refs   *TaskRefs `json:"refs,omitempty"`
	At     time.Time `json:"at"`
}

// Task.BodyDoc points at the hk document holding the task's body ("key" or
// the transitional "key#section"). The body text stays in documents; tasks
// keep only this pointer (hk:doc decision/2026-09-21/task536-body-storage-approved).
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
	BodyDoc    string      `json:"body_doc,omitempty"`
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

type Store struct {
	pool       *pgxpool.Pool
	linearSync atomic.Bool
}

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

type BenchScore struct {
	ModelID        string    `json:"model_id"`
	Effort         string    `json:"effort"`
	Harness        string    `json:"harness"`
	Source         string    `json:"source"`
	Metric         string    `json:"metric"`
	Score          *float64  `json:"score"`
	Rank           *int      `json:"rank"`
	CapturedAt     time.Time `json:"captured_at"`
	TimePerTaskMin *float64  `json:"time_per_task_min"`
	CostPerTaskUSD *float64  `json:"cost_per_task_usd"`
	Provenance     string    `json:"provenance"`
	UpdatedBy      string    `json:"updated_by"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type BenchRep struct {
	ID            int64     `json:"id"`
	OriginID      int64     `json:"origin_id"`
	Profile       string    `json:"profile"`
	ModelID       *string   `json:"model_id"`
	TaskRef       *string   `json:"task_ref"`
	Tier          *string   `json:"tier"`
	Role          *string   `json:"role"`
	Rounds        *int      `json:"rounds"`
	BlockersFound *int      `json:"blockers_found"`
	Completed     *int      `json:"completed"`
	InputTokens   *int64    `json:"input_tokens"`
	OutputTokens  *int64    `json:"output_tokens"`
	Notes         *string   `json:"notes"`
	RecordedAt    time.Time `json:"recorded_at"`
	Effort        *string   `json:"effort"`
	Grade         *string   `json:"grade"`
	TableGrade    *string   `json:"table_grade"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

type BenchGrade struct {
	Profile         string    `json:"profile"`
	Grade           string    `json:"grade"`
	BoundaryVersion string    `json:"boundary_version"`
	DeviationRef    string    `json:"deviation_ref"`
	DecidedAt       time.Time `json:"decided_at"`
	DecidedBy       string    `json:"decided_by"`
}

// BenchCatalogEntry is one row of the (profile, effort)-keyed canonical grade
// catalog. Unlike BenchGrade — where the server stamps the authenticated
// client into decided_by — DecidedBy here is caller-supplied provenance: the
// operator-only PUT records who or which decision produced the assignment.
// Effort "" is the profile-default row and also the compatibility projection
// point for the legacy bench_grades surface.
type BenchCatalogEntry struct {
	Profile             string     `json:"profile"`
	Effort              string     `json:"effort"`
	ModelID             string     `json:"model_id"`
	Pool                string     `json:"pool"`
	Grade               string     `json:"grade"`
	Score               *float64   `json:"score"`
	Gate                string     `json:"gate"`
	GateReason          *string    `json:"gate_reason"`
	BenchmarkSource     *string    `json:"benchmark_source"`
	BenchmarkAnnotation *string    `json:"benchmark_annotation"`
	BoundaryVersion     string     `json:"boundary_version"`
	DeviationRef        string     `json:"deviation_ref"`
	DecidedAt           time.Time  `json:"decided_at"`
	DecidedBy           string     `json:"decided_by"`
	RetiredAt           *time.Time `json:"retired_at"`
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
	Scope   string `json:"scope"`
	Key     string `json:"key"`
	Session string `json:"session"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
	Refs    Refs   `json:"refs,omitempty"`
	// Truncated is set on every row of a page that was cut at the result cap,
	// so a truncated page is never presented as the complete result set.
	Truncated bool      `json:"truncated,omitempty"`
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
	s := &Store{pool: p}
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
		`INSERT INTO schema_version(version) VALUES (5) ON CONFLICT DO NOTHING`,
		// Additive and idempotent at every start: the body document pointer
		// (#536). A constant default makes the ADD COLUMN metadata-only. The
		// catalog check comes first because ALTER TABLE takes an ACCESS
		// EXCLUSIVE lock on tasks even when IF NOT EXISTS finds the column —
		// every later start would queue behind open readers and stall the
		// queue behind it.
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'tasks' AND column_name = 'body_doc') THEN ALTER TABLE tasks ADD COLUMN IF NOT EXISTS body_doc TEXT NOT NULL DEFAULT ''; END IF; END $$`,
		// task_events.kind marks relane rows so state-reading consumers can
		// exclude them. Same shape as body_doc: catalog check first because
		// ALTER TABLE takes an ACCESS EXCLUSIVE lock even when the column
		// exists; the constant default makes the add metadata-only. The CHECK
		// closes the column to the two produced kinds.
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'task_events' AND column_name = 'kind') THEN ALTER TABLE task_events ADD COLUMN kind TEXT NOT NULL DEFAULT 'transition' CHECK(kind IN ('transition','relane')); END IF; END $$`}
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
	var v9Applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=9)`).Scan(&v9Applied); err != nil {
		return err
	}
	if !v9Applied {
		v9 := []string{
			`CREATE TABLE IF NOT EXISTS bench_scores (model_id TEXT NOT NULL, effort TEXT NOT NULL DEFAULT '', harness TEXT NOT NULL DEFAULT '', source TEXT NOT NULL, metric TEXT NOT NULL, score DOUBLE PRECISION, rank INTEGER, captured_at TIMESTAMPTZ NOT NULL, time_per_task_min DOUBLE PRECISION, cost_per_task_usd DOUBLE PRECISION, provenance TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, UNIQUE(model_id, effort, harness, source, metric))`,
			`CREATE INDEX IF NOT EXISTS bench_scores_model_source ON bench_scores(model_id, source)`,
			`CREATE INDEX IF NOT EXISTS bench_scores_source_metric ON bench_scores(source, metric, model_id, effort, harness)`,
			`CREATE TABLE IF NOT EXISTS bench_reps (id BIGSERIAL PRIMARY KEY, origin_id BIGINT NOT NULL CHECK(origin_id >= 1), profile TEXT NOT NULL, model_id TEXT, task_ref TEXT, tier TEXT, role TEXT, rounds INTEGER, blockers_found INTEGER, completed INTEGER, input_tokens BIGINT, output_tokens BIGINT, notes TEXT, recorded_at TIMESTAMPTZ NOT NULL, effort TEXT, grade TEXT, table_grade TEXT, created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, UNIQUE(created_by, origin_id))`,
			`CREATE INDEX IF NOT EXISTS bench_reps_profile_id ON bench_reps(profile, id DESC)`,
			`CREATE INDEX IF NOT EXISTS bench_reps_grade_effort_id ON bench_reps(grade, effort, id DESC)`,
			`CREATE TABLE IF NOT EXISTS bench_grades (profile TEXT PRIMARY KEY, grade TEXT NOT NULL CHECK(grade IN ('S+','S','A+','A','B','C')), boundary_version TEXT NOT NULL DEFAULT '', deviation_ref TEXT NOT NULL, decided_at TIMESTAMPTZ NOT NULL, decided_by TEXT NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS bench_grades_profile ON bench_grades(profile)`,
			`INSERT INTO schema_version(version) VALUES (9)`,
		}
		for _, q := range v9 {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
	}
	v10 := []string{
		`CREATE TABLE IF NOT EXISTS linear_issues (task_id BIGINT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE, issue_id TEXT NOT NULL, identifier TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS linear_outbox (id BIGSERIAL PRIMARY KEY, task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, seq INTEGER NOT NULL, op TEXT NOT NULL CHECK(op IN ('issue_create','issue_state','terminal_comment','terminal_archive')), payload JSONB NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','sent','failed','skipped')) DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TIMESTAMPTZ NOT NULL, last_error TEXT NOT NULL DEFAULT '', remote_id TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, UNIQUE(task_id,seq))`,
		`CREATE INDEX IF NOT EXISTS linear_outbox_pending ON linear_outbox(next_attempt_at,task_id,seq) WHERE state='pending'`,
		`INSERT INTO schema_version(version) VALUES (10) ON CONFLICT DO NOTHING`,
	}
	for _, q := range v10 {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	v11 := []string{
		`CREATE TABLE IF NOT EXISTS chat_questions (id TEXT PRIMARY KEY CHECK(id ~ '^Q-[0-9]{8}-[0-9]{2,}$'), lane TEXT NOT NULL, body TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','resolved','withdrawn')), created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, resolved_at TIMESTAMPTZ)`,
		`CREATE INDEX IF NOT EXISTS chat_questions_state_created ON chat_questions(state, created_at ASC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS chat_questions_created ON chat_questions(created_at ASC, id ASC)`,
		`CREATE TABLE IF NOT EXISTS chat_messages (id BIGSERIAL PRIMARY KEY, author TEXT NOT NULL CHECK(author IN ('operator','desk')), body TEXT NOT NULL, relay_state TEXT NOT NULL CHECK(relay_state IN ('stored','delivered','failed')), created_at TIMESTAMPTZ NOT NULL, delivered_at TIMESTAMPTZ)`,
		`CREATE INDEX IF NOT EXISTS chat_messages_created ON chat_messages(created_at ASC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS chat_messages_undelivered ON chat_messages(id ASC) WHERE delivered_at IS NULL`,
		`INSERT INTO schema_version(version) VALUES (11) ON CONFLICT DO NOTHING`,
	}
	for _, q := range v11 {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	var v12Applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=12)`).Scan(&v12Applied); err != nil {
		return err
	}
	if !v12Applied {
		v12 := []string{
			`CREATE TABLE IF NOT EXISTS bench_catalog (profile TEXT, effort TEXT NOT NULL DEFAULT '', model_id TEXT NOT NULL, pool TEXT NOT NULL, grade TEXT NOT NULL CHECK(grade IN ('S+','S','A+','A','B','C')), score DOUBLE PRECISION, gate TEXT NOT NULL DEFAULT 'default' CHECK(gate IN ('default','escalation','consult_only')), gate_reason TEXT, benchmark_source TEXT, benchmark_annotation TEXT, boundary_version TEXT NOT NULL DEFAULT '', deviation_ref TEXT NOT NULL, decided_at TIMESTAMPTZ NOT NULL, decided_by TEXT NOT NULL, retired_at TIMESTAMPTZ, PRIMARY KEY(profile, effort))`,
			`CREATE INDEX IF NOT EXISTS bench_catalog_pool ON bench_catalog(pool) WHERE retired_at IS NULL`,
			`INSERT INTO bench_catalog(profile, effort, model_id, pool, grade, boundary_version, deviation_ref, decided_at, decided_by) SELECT profile, '', '', '', grade, boundary_version, deviation_ref, decided_at, decided_by FROM bench_grades ON CONFLICT (profile, effort) DO NOTHING`,
			`INSERT INTO schema_version(version) VALUES (12)`,
		}
		for _, q := range v12 {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
	}
	if err := migrateTaskComments(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func validName(x string) bool        { return nameRE.MatchString(x) }
func validText(x string, n int) bool { return len(x) <= n && !strings.ContainsRune(x, 0) }

const benchBatchMax = 1000

var benchGradeValues = map[string]bool{"S+": true, "S": true, "A+": true, "A": true, "B": true, "C": true}

func validBenchRequiredText(x string) bool {
	return x != "" && len(x) <= 200 && validText(x, 200)
}

func validBenchText(x string) bool {
	return validText(x, MaxBytes)
}

func validBenchClient(x string) bool {
	return x != "" && validText(x, 128)
}

func validBenchScore(x BenchScore) bool {
	if !validBenchRequiredText(x.ModelID) || !validBenchRequiredText(x.Source) || !validBenchRequiredText(x.Metric) ||
		!validBenchText(x.Effort) || !validBenchText(x.Harness) || !validBenchText(x.Provenance) || !validBenchClient(x.UpdatedBy) || x.CapturedAt.IsZero() {
		return false
	}
	if x.Score != nil && (math.IsNaN(*x.Score) || math.IsInf(*x.Score, 0) || *x.Score < 0 || *x.Score > 100) {
		return false
	}
	if x.Rank != nil && *x.Rank < 1 {
		return false
	}
	for _, value := range []*float64{x.TimePerTaskMin, x.CostPerTaskUSD} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0) {
			return false
		}
	}
	return true
}

func validBenchRep(x BenchRep) bool {
	if x.OriginID < 1 || !validBenchText(x.Profile) || x.RecordedAt.IsZero() || !validBenchClient(x.CreatedBy) {
		return false
	}
	for _, value := range []*string{x.ModelID, x.TaskRef, x.Tier, x.Role, x.Notes, x.Effort, x.Grade, x.TableGrade} {
		if value != nil && !validBenchText(*value) {
			return false
		}
	}
	return true
}

func validBenchGrade(x BenchGrade) bool {
	return validBenchRequiredText(x.Profile) && benchGradeValues[x.Grade] && validBenchText(x.BoundaryVersion) && validBenchText(x.DeviationRef) && validBenchClient(x.DecidedBy)
}

var benchGateValues = map[string]bool{"default": true, "escalation": true, "consult_only": true}

// benchEffortRanks orders the effort ladder scopefuel publishes
// (low<medium<high<xhigh<max). The profile-default row ("") and unknown effort
// strings are exempt from the monotonicity rule — effort is an open column on
// purpose. In ladder views "" leads the profile's rows and unknown efforts
// sort after the known rungs.
var benchEffortRanks = map[string]int{"low": 0, "medium": 1, "high": 2, "xhigh": 3, "max": 4}

// benchSolProfiles mirrors scopefuel's _SOL_PROFILES: Sol profiles are S+ only.
var benchSolProfiles = map[string]bool{"codex-sol": true, "kiro-sol": true}

func benchGradeRank(grade string) int {
	switch grade {
	case "S+":
		return 0
	case "S":
		return 1
	case "A+":
		return 2
	case "A":
		return 3
	case "B":
		return 4
	default:
		return 5
	}
}

func validBenchCatalogEntry(x BenchCatalogEntry) bool {
	if !validBenchRequiredText(x.Profile) || !validText(x.Effort, 200) || !validBenchRequiredText(x.ModelID) ||
		!validBenchRequiredText(x.Pool) || !benchGradeValues[x.Grade] || !benchGateValues[x.Gate] ||
		!validBenchText(x.BoundaryVersion) || !validBenchText(x.DeviationRef) || !validBenchRequiredText(x.DecidedBy) {
		return false
	}
	if x.Score != nil && (math.IsNaN(*x.Score) || math.IsInf(*x.Score, 0) || *x.Score < 0 || *x.Score > 100) {
		return false
	}
	for _, value := range []*string{x.GateReason, x.BenchmarkSource, x.BenchmarkAnnotation} {
		if value != nil && !validBenchText(*value) {
			return false
		}
	}
	return true
}

func (s *Store) UpsertBenchScores(ctx context.Context, xs []BenchScore) (int, error) {
	if len(xs) < 1 || len(xs) > benchBatchMax {
		return 0, errors.New("invalid bench scores")
	}
	for _, x := range xs {
		if !validBenchScore(x) {
			return 0, errors.New("invalid bench score")
		}
		if err := guard.Reject(x.Provenance); err != nil {
			return 0, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	for _, x := range xs {
		_, err = tx.Exec(ctx, `INSERT INTO bench_scores(model_id,effort,harness,source,metric,score,rank,captured_at,time_per_task_min,cost_per_task_usd,provenance,updated_by,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (model_id,effort,harness,source,metric) DO UPDATE SET score=EXCLUDED.score,rank=EXCLUDED.rank,captured_at=EXCLUDED.captured_at,time_per_task_min=EXCLUDED.time_per_task_min,cost_per_task_usd=EXCLUDED.cost_per_task_usd,provenance=EXCLUDED.provenance,updated_by=EXCLUDED.updated_by,updated_at=EXCLUDED.updated_at`, x.ModelID, x.Effort, x.Harness, x.Source, x.Metric, x.Score, x.Rank, x.CapturedAt, x.TimePerTaskMin, x.CostPerTaskUSD, x.Provenance, x.UpdatedBy, now)
		if err != nil {
			return 0, err
		}
	}
	return len(xs), tx.Commit(ctx)
}

func (s *Store) ListBenchScores(ctx context.Context, modelID, source string, limit int) ([]BenchScore, error) {
	if !validBenchText(modelID) || !validBenchText(source) {
		return nil, errors.New("invalid bench score query")
	}
	if limit < 1 {
		limit = benchBatchMax
	}
	if limit > 5000 {
		limit = 5000
	}
	rows, err := s.pool.Query(ctx, `SELECT model_id,effort,harness,source,metric,score,rank,captured_at,time_per_task_min,cost_per_task_usd,provenance,updated_by,updated_at FROM bench_scores WHERE ($1='' OR model_id=$1) AND ($2='' OR source=$2) ORDER BY source,metric,model_id,effort,harness LIMIT $3`, modelID, source, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchScore{}
	for rows.Next() {
		var x BenchScore
		if err := rows.Scan(&x.ModelID, &x.Effort, &x.Harness, &x.Source, &x.Metric, &x.Score, &x.Rank, &x.CapturedAt, &x.TimePerTaskMin, &x.CostPerTaskUSD, &x.Provenance, &x.UpdatedBy, &x.UpdatedAt); err != nil {
			return nil, err
		}
		x.CapturedAt = x.CapturedAt.UTC()
		x.UpdatedAt = x.UpdatedAt.UTC()
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) UpsertBenchReps(ctx context.Context, xs []BenchRep) (int, error) {
	if len(xs) < 1 || len(xs) > benchBatchMax {
		return 0, errors.New("invalid bench reps")
	}
	for _, x := range xs {
		if !validBenchRep(x) {
			return 0, errors.New("invalid bench rep")
		}
		for _, value := range []*string{x.TaskRef, x.Notes} {
			if value != nil {
				if err := guard.Reject(*value); err != nil {
					return 0, err
				}
			}
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	for _, x := range xs {
		err = tx.QueryRow(ctx, `INSERT INTO bench_reps(origin_id,profile,model_id,task_ref,tier,role,rounds,blockers_found,completed,input_tokens,output_tokens,notes,recorded_at,effort,grade,table_grade,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) ON CONFLICT (created_by,origin_id) DO UPDATE SET profile=EXCLUDED.profile,model_id=EXCLUDED.model_id,task_ref=EXCLUDED.task_ref,tier=EXCLUDED.tier,role=EXCLUDED.role,rounds=EXCLUDED.rounds,blockers_found=EXCLUDED.blockers_found,completed=EXCLUDED.completed,input_tokens=EXCLUDED.input_tokens,output_tokens=EXCLUDED.output_tokens,notes=EXCLUDED.notes,recorded_at=EXCLUDED.recorded_at,effort=EXCLUDED.effort,grade=EXCLUDED.grade,table_grade=EXCLUDED.table_grade RETURNING id`, x.OriginID, x.Profile, x.ModelID, x.TaskRef, x.Tier, x.Role, x.Rounds, x.BlockersFound, x.Completed, x.InputTokens, x.OutputTokens, x.Notes, x.RecordedAt, x.Effort, x.Grade, x.TableGrade, x.CreatedBy, now).Scan(&x.ID)
		if err != nil {
			return 0, err
		}
	}
	return len(xs), tx.Commit(ctx)
}

func (s *Store) ListBenchReps(ctx context.Context, profile, grade, effort string, limit int) ([]BenchRep, error) {
	if !validBenchText(profile) || !validBenchText(grade) || !validBenchText(effort) {
		return nil, errors.New("invalid bench rep query")
	}
	if limit < 1 {
		limit = benchBatchMax
	}
	if limit > 5000 {
		limit = 5000
	}
	rows, err := s.pool.Query(ctx, `SELECT id,origin_id,profile,model_id,task_ref,tier,role,rounds,blockers_found,completed,input_tokens,output_tokens,notes,recorded_at,effort,grade,table_grade,created_by,created_at FROM bench_reps WHERE ($1='' OR profile=$1) AND ($2='' OR grade=$2) AND ($3='' OR effort=$3) ORDER BY id DESC LIMIT $4`, profile, grade, effort, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchRep{}
	for rows.Next() {
		var x BenchRep
		if err := rows.Scan(&x.ID, &x.OriginID, &x.Profile, &x.ModelID, &x.TaskRef, &x.Tier, &x.Role, &x.Rounds, &x.BlockersFound, &x.Completed, &x.InputTokens, &x.OutputTokens, &x.Notes, &x.RecordedAt, &x.Effort, &x.Grade, &x.TableGrade, &x.CreatedBy, &x.CreatedAt); err != nil {
			return nil, err
		}
		x.RecordedAt = x.RecordedAt.UTC()
		x.CreatedAt = x.CreatedAt.UTC()
		out = append(out, x)
	}
	return out, rows.Err()
}

// ListBenchRepsByTaskRef returns the reps recorded against one exact task_ref
// (for example "hk:task/42"). The match is exact equality: a task detail view
// must never borrow telemetry from a different or partially matching ref.
func (s *Store) ListBenchRepsByTaskRef(ctx context.Context, taskRef string, limit int) ([]BenchRep, error) {
	if taskRef == "" || !validBenchText(taskRef) {
		return nil, errors.New("invalid bench rep query")
	}
	if limit < 1 {
		limit = benchBatchMax
	}
	if limit > 5000 {
		limit = 5000
	}
	rows, err := s.pool.Query(ctx, `SELECT id,origin_id,profile,model_id,task_ref,tier,role,rounds,blockers_found,completed,input_tokens,output_tokens,notes,recorded_at,effort,grade,table_grade,created_by,created_at FROM bench_reps WHERE task_ref=$1 ORDER BY id ASC LIMIT $2`, taskRef, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchRep{}
	for rows.Next() {
		var x BenchRep
		if err := rows.Scan(&x.ID, &x.OriginID, &x.Profile, &x.ModelID, &x.TaskRef, &x.Tier, &x.Role, &x.Rounds, &x.BlockersFound, &x.Completed, &x.InputTokens, &x.OutputTokens, &x.Notes, &x.RecordedAt, &x.Effort, &x.Grade, &x.TableGrade, &x.CreatedBy, &x.CreatedAt); err != nil {
			return nil, err
		}
		x.RecordedAt = x.RecordedAt.UTC()
		x.CreatedAt = x.CreatedAt.UTC()
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) UpsertBenchGrades(ctx context.Context, xs []BenchGrade) (int, error) {
	if len(xs) < 1 || len(xs) > benchBatchMax {
		return 0, errors.New("invalid bench grades")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	for _, x := range xs {
		if strings.TrimSpace(x.DeviationRef) == "" {
			return 0, ErrDeviationRefRequired
		}
		if !validBenchGrade(x) {
			return 0, errors.New("invalid bench grade")
		}
		// The legacy path mirrors into the catalog, so it must not admit rows
		// the catalog itself would reject — the Sol rule applies here too.
		if benchSolProfiles[x.Profile] && x.Grade != "S+" {
			return 0, ErrBenchCatalogSolGrade
		}
		if err := guard.Reject(x.BoundaryVersion); err != nil {
			return 0, err
		}
		if err := guard.Reject(x.DeviationRef); err != nil {
			return 0, err
		}
		if x.DecidedAt.IsZero() {
			x.DecidedAt = now
		}
		_, err = tx.Exec(ctx, `INSERT INTO bench_grades(profile,grade,boundary_version,deviation_ref,decided_at,decided_by) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT (profile) DO UPDATE SET grade=EXCLUDED.grade,boundary_version=EXCLUDED.boundary_version,deviation_ref=EXCLUDED.deviation_ref,decided_at=EXCLUDED.decided_at,decided_by=EXCLUDED.decided_by`, x.Profile, x.Grade, x.BoundaryVersion, x.DeviationRef, x.DecidedAt, x.DecidedBy)
		if err != nil {
			return 0, err
		}
		// Mirror into the catalog's profile-default row so the canonical store
		// sees legacy writes; catalog-only columns (model_id, pool, gate, ...)
		// are preserved on conflict. A legacy grade write un-retires the row.
		_, err = tx.Exec(ctx, `INSERT INTO bench_catalog(profile,effort,model_id,pool,grade,boundary_version,deviation_ref,decided_at,decided_by) VALUES($1,'','','',$2,$3,$4,$5,$6) ON CONFLICT (profile,effort) DO UPDATE SET grade=EXCLUDED.grade,boundary_version=EXCLUDED.boundary_version,deviation_ref=EXCLUDED.deviation_ref,decided_at=EXCLUDED.decided_at,decided_by=EXCLUDED.decided_by,retired_at=NULL`, x.Profile, x.Grade, x.BoundaryVersion, x.DeviationRef, x.DecidedAt, x.DecidedBy)
		if err != nil {
			return 0, err
		}
	}
	return len(xs), tx.Commit(ctx)
}

func (s *Store) ListBenchGrades(ctx context.Context) ([]BenchGrade, error) {
	// The catalog is canonical: project its profile-default rows. The UNION
	// limb keeps bench_grades rows readable when a catalog row does not exist
	// yet (for example a write from a pre-migration binary during a rollback
	// skew window). Once any catalog effort='' row exists — even a retired one —
	// it alone speaks for the profile.
	rows, err := s.pool.Query(ctx, `SELECT profile,grade,boundary_version,deviation_ref,decided_at,decided_by FROM bench_catalog WHERE effort='' AND retired_at IS NULL UNION ALL SELECT g.profile,g.grade,g.boundary_version,g.deviation_ref,g.decided_at,g.decided_by FROM bench_grades g WHERE NOT EXISTS (SELECT 1 FROM bench_catalog c WHERE c.profile=g.profile AND c.effort='') ORDER BY profile`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchGrade{}
	for rows.Next() {
		var x BenchGrade
		if err := rows.Scan(&x.Profile, &x.Grade, &x.BoundaryVersion, &x.DeviationRef, &x.DecidedAt, &x.DecidedBy); err != nil {
			return nil, err
		}
		x.DecidedAt = x.DecidedAt.UTC()
		out = append(out, x)
	}
	return out, rows.Err()
}

// UpsertBenchCatalog is the operator write path for the canonical grade
// catalog. The batch is atomic: every row validates first, profile-default
// (effort=”) rows mirror into bench_grades so legacy readers keep working,
// and the merged post-write state must satisfy the per-profile effort
// monotonicity rule or the whole batch rolls back.
func (s *Store) UpsertBenchCatalog(ctx context.Context, xs []BenchCatalogEntry) (int, error) {
	if len(xs) < 1 || len(xs) > benchBatchMax {
		return 0, errors.New("invalid bench catalog")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	profiles := map[string]bool{}
	for _, x := range xs {
		if strings.TrimSpace(x.DeviationRef) == "" {
			return 0, ErrDeviationRefRequired
		}
		if strings.TrimSpace(x.DecidedBy) == "" {
			return 0, ErrDecidedByRequired
		}
		if x.Gate == "" {
			x.Gate = "default"
		}
		if !validBenchCatalogEntry(x) {
			return 0, errors.New("invalid bench catalog entry")
		}
		if benchSolProfiles[x.Profile] && x.Grade != "S+" {
			return 0, ErrBenchCatalogSolGrade
		}
		for _, value := range []*string{x.GateReason, x.BenchmarkSource, x.BenchmarkAnnotation} {
			if value != nil {
				if err := guard.Reject(*value); err != nil {
					return 0, err
				}
			}
		}
		if err := guard.Reject(x.BoundaryVersion); err != nil {
			return 0, err
		}
		if err := guard.Reject(x.DeviationRef); err != nil {
			return 0, err
		}
		if err := guard.Reject(x.DecidedBy); err != nil {
			return 0, err
		}
		if x.DecidedAt.IsZero() {
			x.DecidedAt = now
		}
		_, err = tx.Exec(ctx, `INSERT INTO bench_catalog(profile,effort,model_id,pool,grade,score,gate,gate_reason,benchmark_source,benchmark_annotation,boundary_version,deviation_ref,decided_at,decided_by,retired_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT (profile,effort) DO UPDATE SET model_id=EXCLUDED.model_id,pool=EXCLUDED.pool,grade=EXCLUDED.grade,score=EXCLUDED.score,gate=EXCLUDED.gate,gate_reason=EXCLUDED.gate_reason,benchmark_source=EXCLUDED.benchmark_source,benchmark_annotation=EXCLUDED.benchmark_annotation,boundary_version=EXCLUDED.boundary_version,deviation_ref=EXCLUDED.deviation_ref,decided_at=EXCLUDED.decided_at,decided_by=EXCLUDED.decided_by,retired_at=EXCLUDED.retired_at`, x.Profile, x.Effort, x.ModelID, x.Pool, x.Grade, x.Score, x.Gate, x.GateReason, x.BenchmarkSource, x.BenchmarkAnnotation, x.BoundaryVersion, x.DeviationRef, x.DecidedAt, x.DecidedBy, x.RetiredAt)
		if err != nil {
			return 0, err
		}
		if x.Effort == "" {
			if x.RetiredAt == nil {
				_, err = tx.Exec(ctx, `INSERT INTO bench_grades(profile,grade,boundary_version,deviation_ref,decided_at,decided_by) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT (profile) DO UPDATE SET grade=EXCLUDED.grade,boundary_version=EXCLUDED.boundary_version,deviation_ref=EXCLUDED.deviation_ref,decided_at=EXCLUDED.decided_at,decided_by=EXCLUDED.decided_by`, x.Profile, x.Grade, x.BoundaryVersion, x.DeviationRef, x.DecidedAt, x.DecidedBy)
			} else {
				_, err = tx.Exec(ctx, `DELETE FROM bench_grades WHERE profile=$1`, x.Profile)
			}
			if err != nil {
				return 0, err
			}
		}
		profiles[x.Profile] = true
	}
	if err := checkBenchCatalogMonotonicity(ctx, tx, profiles); err != nil {
		return 0, err
	}
	return len(xs), tx.Commit(ctx)
}

// checkBenchCatalogMonotonicity rejects a merged catalog state where a known
// higher-effort rung carries a worse grade than a lower rung of the same
// profile. Retired rows, the profile-default row (effort=”) and unknown
// effort strings do not participate.
func checkBenchCatalogMonotonicity(ctx context.Context, tx pgx.Tx, profiles map[string]bool) error {
	list := make([]string, 0, len(profiles))
	for p := range profiles {
		list = append(list, p)
	}
	rows, err := tx.Query(ctx, `SELECT profile,effort,grade FROM bench_catalog WHERE profile = ANY($1) AND retired_at IS NULL AND effort <> ''`, list)
	if err != nil {
		return err
	}
	defer rows.Close()
	byProfile := map[string][]BenchCatalogEntry{}
	for rows.Next() {
		var x BenchCatalogEntry
		if err := rows.Scan(&x.Profile, &x.Effort, &x.Grade); err != nil {
			return err
		}
		if _, ok := benchEffortRanks[x.Effort]; !ok {
			continue
		}
		byProfile[x.Profile] = append(byProfile[x.Profile], x)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for profile, entries := range byProfile {
		sort.Slice(entries, func(i, j int) bool { return benchEffortRanks[entries[i].Effort] < benchEffortRanks[entries[j].Effort] })
		best := benchGradeRank("C")
		for _, x := range entries {
			rank := benchGradeRank(x.Grade)
			if rank > best {
				return fmt.Errorf("%w: %s effort %s grade %s below lower effort", ErrBenchCatalogMonotonicity, profile, x.Effort, x.Grade)
			}
			best = rank
		}
	}
	return nil
}

// ListBenchCatalog returns catalog rows in ladder order — pool, then grade
// descending (S+ first), then profile, then effort rung. A pool filter turns
// the result into the subscription ladder for that pool and drops
// consult_only rows (they remain visible in the unfiltered listing). Retired
// rows are hidden unless includeRetired is set.
func (s *Store) ListBenchCatalog(ctx context.Context, pool string, includeRetired bool) ([]BenchCatalogEntry, error) {
	if !validText(pool, 200) {
		return nil, errors.New("invalid bench catalog query")
	}
	rows, err := s.pool.Query(ctx, `SELECT profile,effort,model_id,pool,grade,score,gate,gate_reason,benchmark_source,benchmark_annotation,boundary_version,deviation_ref,decided_at,decided_by,retired_at FROM bench_catalog WHERE ($1='' OR pool=$1) AND ($1='' OR gate<>'consult_only') AND ($2 OR retired_at IS NULL) ORDER BY pool, CASE grade WHEN 'S+' THEN 0 WHEN 'S' THEN 1 WHEN 'A+' THEN 2 WHEN 'A' THEN 3 WHEN 'B' THEN 4 ELSE 5 END, profile, CASE effort WHEN '' THEN 0 WHEN 'low' THEN 1 WHEN 'medium' THEN 2 WHEN 'high' THEN 3 WHEN 'xhigh' THEN 4 WHEN 'max' THEN 5 ELSE 6 END, effort`, pool, includeRetired)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchCatalogEntry{}
	for rows.Next() {
		var x BenchCatalogEntry
		if err := rows.Scan(&x.Profile, &x.Effort, &x.ModelID, &x.Pool, &x.Grade, &x.Score, &x.Gate, &x.GateReason, &x.BenchmarkSource, &x.BenchmarkAnnotation, &x.BoundaryVersion, &x.DeviationRef, &x.DecidedAt, &x.DecidedBy, &x.RetiredAt); err != nil {
			return nil, err
		}
		x.DecidedAt = x.DecidedAt.UTC()
		if x.RetiredAt != nil {
			t := x.RetiredAt.UTC()
			x.RetiredAt = &t
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func validTaskRefs(x TaskRefs) bool {
	for _, v := range []string{x.PR, x.HeadSHA, x.ReportPath, x.JobID} {
		if !validText(v, 4096) {
			return false
		}
	}
	if x.DecisionOptions != nil && !validDecisionOptions(*x.DecisionOptions) {
		return false
	}
	if x.Linear != nil && !validTaskLinear(*x.Linear) {
		return false
	}
	if x.OriginPR != "" && !originPRRE.MatchString(x.OriginPR) {
		return false
	}
	if x.OriginTask < 0 {
		return false
	}
	if x.Disposition != nil && !validDisposition(x) {
		return false
	}
	return true
}

func validTaskLinear(x TaskLinear) bool {
	if x.Tier != "" && x.Tier != "T0" && x.Tier != "T1" && x.Tier != "T2" && x.Tier != "T3" {
		return false
	}
	if x.Grade != "" && x.Grade != "S+" && x.Grade != "S" && x.Grade != "A+" && x.Grade != "A" && x.Grade != "B" && x.Grade != "C" {
		return false
	}
	for _, value := range []string{x.Brief, x.Report, x.Verify, x.Decision} {
		if value != "" && !validDocKey(value) {
			return false
		}
	}
	if x.DeploySHA != "" && !regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`).MatchString(x.DeploySHA) {
		return false
	}
	if len(x.Labels) > 64 {
		return false
	}
	for _, label := range x.Labels {
		if strings.TrimSpace(label) == "" || !validText(label, 128) {
			return false
		}
	}
	return true
}

func rejectTaskRefs(x TaskRefs) error {
	values := []string{x.PR, x.HeadSHA, x.ReportPath, x.JobID}
	if x.DecisionOptions != nil {
		for _, option := range x.DecisionOptions.Options {
			values = append(values, option.Label)
		}
	}
	if x.Linear != nil {
		values = append(values, x.Linear.Tier, x.Linear.Grade, x.Linear.Brief, x.Linear.Report, x.Linear.Verify, x.Linear.Decision, x.Linear.DeploySHA)
		values = append(values, x.Linear.Labels...)
	}
	values = append(values, x.OriginPR)
	if x.Disposition != nil {
		values = append(values, x.Disposition.Facts.ResidualDoc, x.Disposition.Facts.Install.Witness)
	}
	return guard.Reject(strings.Join(values, "\n"))
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
	if patch.DecisionOptions != nil {
		old.DecisionOptions = patch.DecisionOptions
	}
	if patch.OriginPR != "" {
		old.OriginPR = patch.OriginPR
	}
	if patch.OriginTask != 0 {
		old.OriginTask = patch.OriginTask
	}
	if patch.Linear != nil {
		merged := TaskLinear{}
		if old.Linear != nil {
			merged = *old.Linear
			merged.Labels = append([]string(nil), old.Linear.Labels...)
		}
		// TaskLinear is also used as a partial transition patch. The connector
		// can be opted in here, while an omitted/default false value must not
		// silently disable an already opted-in task.
		if patch.Linear.Sync {
			merged.Sync = true
		}
		if patch.Linear.Tier != "" {
			merged.Tier = patch.Linear.Tier
		}
		if patch.Linear.Grade != "" {
			merged.Grade = patch.Linear.Grade
		}
		if patch.Linear.Brief != "" {
			merged.Brief = patch.Linear.Brief
		}
		if len(patch.Linear.Labels) > 0 {
			merged.Labels = append([]string(nil), patch.Linear.Labels...)
		}
		if patch.Linear.Report != "" {
			merged.Report = patch.Linear.Report
		}
		if patch.Linear.Verify != "" {
			merged.Verify = patch.Linear.Verify
		}
		if patch.Linear.Decision != "" {
			merged.Decision = patch.Linear.Decision
		}
		if patch.Linear.DeploySHA != "" {
			merged.DeploySHA = patch.Linear.DeploySHA
		}
		old.Linear = &merged
	}
	return old
}

func validTask(x Task) bool {
	return validName(x.Lane) && (x.ParentLane == "" || validName(x.ParentLane)) && x.Title != "" && validText(x.Title, MaxBytes) && taskKinds[x.Kind] && x.CreatedBy != "" && validText(x.CreatedBy, 128) && validTaskRefs(x.Refs) && (x.BodyDoc == "" || ValidBodyDoc(x.BodyDoc))
}

// bodyDocKeyRE is the document key alphabet the console can fetch and link
// (internal/ui docKeyRE). A body_doc outside it could be stored but never
// shown, so it is refused at the pointer instead.
var bodyDocKeyRE = regexp.MustCompile(`^[A-Za-z0-9._\-/]{1,512}$`)

// BodyDocSectionMax bounds the transitional "#section" suffix of body_doc.
const BodyDocSectionMax = 200

// ValidBodyDoc checks only the shape of a task body pointer: "key" or
// "key#section". The document need not exist yet — tasks may be filed before
// their body is written, and the console reports a missing document itself.
// The section is a scroll position only; it is never hashed or verified.
// Whitespace and control characters are refused anywhere, so the pointer can
// never carry body text.
func ValidBodyDoc(v string) bool {
	key, section, hasSection := strings.Cut(v, "#")
	if !bodyDocKeyRE.MatchString(key) || strings.HasPrefix(key, "/") || strings.Contains(key, "..") || !validDocKey(key) {
		return false
	}
	if !hasSection {
		return true
	}
	if section == "" || len(section) > BodyDocSectionMax || !utf8.ValidString(section) {
		return false
	}
	for _, r := range section {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '#' {
			return false
		}
	}
	return true
}

func scanTask(row interface{ Scan(...any) error }, x *Task) error {
	var refs []byte
	if err := row.Scan(&x.ID, &x.Lane, &x.ParentLane, &x.Title, &x.Kind, &x.State, &x.Priority, &refs, &x.ClaimedBy, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt, &x.BodyDoc); err != nil {
		return err
	}
	return json.Unmarshal(refs, &x.Refs)
}

const taskColumns = `id,lane,parent_lane,title,kind,state,priority,refs,claimed_by,created_by,created_at,updated_at,body_doc`

func (s *Store) CreateTask(ctx context.Context, x Task) (Task, error) {
	if x.BodyDoc != "" && !ValidBodyDoc(x.BodyDoc) {
		return x, errors.New("invalid task: body_doc must be a document key or key#section")
	}
	if !validTask(x) {
		return x, errors.New("invalid task")
	}
	// Disposition items are born only through CreateDisposition, which moves
	// them to needs_decision inside the creating transaction.
	if x.Refs.Disposition != nil {
		return x, errors.New("invalid task: disposition items use CreateDisposition")
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return x, err
	}
	defer tx.Rollback(ctx)
	if err = scanTask(tx.QueryRow(ctx, `INSERT INTO tasks(lane,parent_lane,title,kind,state,priority,refs,claimed_by,created_by,created_at,updated_at,body_doc) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11,$12) RETURNING `+taskColumns, x.Lane, x.ParentLane, x.Title, x.Kind, x.State, x.Priority, string(refs), x.ClaimedBy, x.CreatedBy, x.CreatedAt, x.UpdatedAt, x.BodyDoc), &x); err != nil {
		return x, err
	}
	if s.LinearSyncEnabled() && x.Refs.Linear != nil && x.Refs.Linear.Sync {
		if err = enqueueLinearTaskCreate(ctx, tx, x); err != nil {
			return x, err
		}
	}
	return x, tx.Commit(ctx)
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
	if s.LinearSyncEnabled() && x.Refs.Linear != nil && x.Refs.Linear.Sync {
		if err := enqueueLinearTaskTransition(ctx, tx, x, ""); err != nil {
			return Task{}, err
		}
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
		// The disposition object (and its answer) is written only by
		// CreateDisposition and AnswerDisposition(Batch).
		if refs.Disposition != nil {
			return Task{}, errors.New("invalid task transition: disposition refs are not patchable")
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
	// Placed after the row lock and before the transition-table check so every
	// generic caller (API, MCP, CLI, UI generic decision forms) is refused while
	// a disposition item awaits the operator, whatever edge it asks for. The
	// operator's answer uses AnswerDisposition, which does not come through here.
	if x.Refs.Disposition != nil {
		if from == "needs_decision" {
			return Task{}, ErrDispositionOperatorOnly
		}
		// A disposition item never returns to backlog: NextTask would claim it.
		// Its closed options and its origin are fixed at creation, in every
		// state: re-pointing origin_pr/origin_task would detach the recorded
		// facts from their source and let one origin own two items over time.
		if to == "backlog" || (refs != nil && (refs.DecisionOptions != nil || refs.OriginPR != "" || refs.OriginTask != 0)) {
			return Task{}, ErrTaskConflict
		}
	}
	if !taskTransitionAllowed(from, to) {
		return Task{}, ErrTaskConflict
	}
	if to == "needs_decision" && strings.TrimSpace(note) == "" {
		return Task{}, errors.New("needs_decision requires question")
	}
	if refs != nil {
		x.Refs = mergeTaskRefs(x.Refs, *refs)
		// A patch is validated alone above; a disposition item's merged refs
		// must still be a valid disposition (one origin, closed options).
		if x.Refs.Disposition != nil && !validTaskRefs(x.Refs) {
			return Task{}, errors.New("invalid task transition: disposition refs")
		}
	}
	// Re-asking reopens the question: the previous answer no longer describes
	// the item and must not count as an answer, pending application, or a
	// pending notice. The old answer stays in the task_events refs snapshots.
	if x.Refs.Disposition != nil && to == "needs_decision" {
		x.Refs.Disposition.Answer = nil
	}
	if to == "needs_decision" && x.Refs.DecisionOptions != nil {
		finalText := note + "\n" + FormatDecisionOptions(*x.Refs.DecisionOptions)
		if len([]byte(finalText)) > RelayLaneEventMaxBytes {
			return Task{}, errors.New("decision question and options exceed 2048 bytes")
		}
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
	if s.LinearSyncEnabled() && x.Refs.Linear != nil && x.Refs.Linear.Sync {
		if err = enqueueLinearTaskTransition(ctx, tx, x, note); err != nil {
			return Task{}, err
		}
	}
	return x, tx.Commit(ctx)
}

// knownTaskLanesTx reports whether name is a lane some current row uses. hk
// has no lane table, so the known set is the union of every column that
// carries a live lane name.
func knownTaskLanesTx(ctx context.Context, tx pgx.Tx, name string) (bool, error) {
	var known bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM (
			SELECT lane FROM tasks UNION SELECT parent_lane FROM tasks
			UNION SELECT owner_lane FROM relay_events UNION SELECT lane FROM chat_questions
		) lanes WHERE lanes.lane=$1
	)`, name).Scan(&known)
	return known, err
}

// RelaneTask moves a task to another lane without touching state, priority,
// refs, or claimant. The move is recorded as an append-only task_events row
// with kind='relane' so transition readers never mistake a lane name for a
// state. The row lock serializes against TransitionTask: both can succeed,
// and their event order is lock order.
//
// changed is false when the task already sits in the target lane — a no-op
// writes no event, so rerunning a batch relane stays idempotent without
// falsifying history. Terminal tasks are refused outright: merged/dropped
// rows are history, and moving them would corrupt end-state reports without
// ever returning them to a queue. to must be a lane some current row uses
// unless allowNewLane is set, so a typo cannot strand a task in a lane no
// owner lists.
func (s *Store) RelaneTask(ctx context.Context, id int64, to, by, note string, allowNewLane bool) (x Task, changed bool, err error) {
	if !validName(to) || by == "" || !validText(by, 128) || strings.TrimSpace(note) == "" || !validText(note, MaxBytes) {
		return Task{}, false, errors.New("invalid task relane")
	}
	if err := guard.Reject(note); err != nil {
		return Task{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback(ctx)
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, false, ErrTaskNotFound
		}
		return Task{}, false, err
	}
	if x.State == "merged" || x.State == "dropped" {
		return Task{}, false, ErrTaskTerminal
	}
	if x.Lane == to {
		return x, false, tx.Commit(ctx)
	}
	if !allowNewLane {
		known, err := knownTaskLanesTx(ctx, tx, to)
		if err != nil {
			return Task{}, false, err
		}
		if !known {
			return Task{}, false, ErrTaskLaneUnknown
		}
	}
	from := x.Lane
	encoded, err := json.Marshal(x.Refs)
	if err != nil {
		return Task{}, false, err
	}
	x.UpdatedAt = time.Now().UTC()
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET lane=$2,updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, id, to, x.UpdatedAt), &x); err != nil {
		return Task{}, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,refs,at,kind) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,'relane')`, id, from, to, by, note, string(encoded), x.UpdatedAt); err != nil {
		return Task{}, false, err
	}
	return x, true, tx.Commit(ctx)
}

func (s *Store) ListTasks(ctx context.Context, lane, state, parentLane string, limit int) ([]Task, error) {
	if !validTaskQuery(lane, state, parentLane) {
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

// ListTasksPage returns a stable ID-ordered page for callers that must visit
// the complete task set. afterID is exclusive; ListTasks retains its queue
// priority ordering for existing callers.
func (s *Store) ListTasksPage(ctx context.Context, lane, state, parentLane string, afterID int64, limit int) ([]Task, error) {
	if !validTaskQuery(lane, state, parentLane) || afterID < 0 {
		return nil, errors.New("invalid task query")
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	q, args := `SELECT `+taskColumns+` FROM tasks WHERE id>$1`, []any{afterID}
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
	q += fmt.Sprintf(" ORDER BY id ASC LIMIT $%d", len(args))
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

// GlanceTasks returns the counts and active task slice needed by the compact UI
// API. Counts span every lane; active tasks have the API's stable newest-first
// ordering rather than the queue's priority ordering.
func (s *Store) GlanceTasks(ctx context.Context) (map[string]int, []Task, error) {
	counts := map[string]int{}
	rows, err := s.pool.Query(ctx, `SELECT state,COUNT(*) FROM tasks GROUP BY state`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return nil, nil, err
		}
		counts[state] = count
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = s.pool.Query(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE state IN ('claimed','in_progress','verifying','join')
		ORDER BY updated_at DESC,id DESC LIMIT 20`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	active := []Task{}
	for rows.Next() {
		var task Task
		if err := scanTask(rows, &task); err != nil {
			return nil, nil, err
		}
		active = append(active, task)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return counts, active, nil
}

func (s *Store) GetTask(ctx context.Context, id int64) (Task, bool, error) {
	var x Task
	if err := scanTask(s.pool.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, false, nil
		}
		return Task{}, false, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,task_id,"from","to","by",note,refs,at,kind FROM task_events WHERE task_id=$1 ORDER BY at,id`, id)
	if err != nil {
		return Task{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var e TaskEvent
		var refs []byte
		if err := rows.Scan(&e.ID, &e.TaskID, &e.From, &e.To, &e.By, &e.Note, &refs, &e.At, &e.Kind); err != nil {
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

// ListRelayEventsTimeline returns newest relay events first. beforeID is an
// exclusive descending cursor; dates are supplied as UTC bounds by the UI.
func (s *Store) ListRelayEventsTimeline(ctx context.Context, lane, kind string, since, until *time.Time, beforeID int64, limit int) ([]RelayEvent, error) {
	if !validText(lane, MaxBytes) || (kind != "" && !relayEventKinds[kind]) || beforeID < 0 {
		return nil, errors.New("invalid relay timeline query")
	}
	if limit < 1 {
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
	if since != nil {
		args = append(args, *since)
		q += fmt.Sprintf(" AND received_at >= $%d", len(args))
	}
	if until != nil {
		args = append(args, *until)
		q += fmt.Sprintf(" AND received_at < $%d", len(args))
	}
	if beforeID > 0 {
		args = append(args, beforeID)
		q += fmt.Sprintf(" AND id < $%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
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

// EventWatermarks returns the two durable IDs used by the UI's SSE poller.
func (s *Store) EventWatermarks(ctx context.Context) (relayMaxID, taskEventMaxID int64, err error) {
	err = s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT MAX(id) FROM relay_events),0),COALESCE((SELECT MAX(id) FROM task_events),0)`).Scan(&relayMaxID, &taskEventMaxID)
	return relayMaxID, taskEventMaxID, err
}

// LatestCheckpointsBySession returns one newest checkpoint for every session.
func (s *Store) LatestCheckpointsBySession(ctx context.Context, limit int) ([]Checkpoint, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT id,session,kind,title,body,refs,created_by,created_at FROM (
		SELECT DISTINCT ON (session) id,session,kind,title,body,refs,created_by,created_at
		FROM checkpoints ORDER BY session,created_at DESC,id DESC
	) latest ORDER BY created_at DESC,id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Checkpoint{}
	for rows.Next() {
		var x Checkpoint
		var refs []byte
		if err := rows.Scan(&x.ID, &x.Session, &x.Kind, &x.Title, &x.Body, &refs, &x.CreatedBy, &x.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(refs, &x.Refs); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// TaskDecision is an unresolved task together with the original question
// recorded on its latest transition into needs_decision.
type TaskDecision struct {
	Task     Task
	Question string
}

// ListOpenTaskDecisions implements the decision inbox task definition.
// Disposition items are listed separately by ListOpenDispositions: they are
// answered only through the operator disposition routes.
func (s *Store) ListOpenTaskDecisions(ctx context.Context, limit int) ([]TaskDecision, error) {
	if limit < 1 {
		limit = 1000
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT t.id,t.lane,t.parent_lane,t.title,t.kind,t.state,t.priority,t.refs,t.claimed_by,t.created_by,t.created_at,t.updated_at,
		COALESCE((SELECT note FROM task_events WHERE task_id=t.id AND kind='transition' AND "to"='needs_decision' ORDER BY at DESC,id DESC LIMIT 1),'')
		FROM tasks t WHERE t.state='needs_decision' AND NOT (t.refs ? 'disposition') ORDER BY t.updated_at DESC,t.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskDecision{}
	for rows.Next() {
		var x TaskDecision
		var refs []byte
		if err := rows.Scan(&x.Task.ID, &x.Task.Lane, &x.Task.ParentLane, &x.Task.Title, &x.Task.Kind, &x.Task.State, &x.Task.Priority, &refs, &x.Task.ClaimedBy, &x.Task.CreatedBy, &x.Task.CreatedAt, &x.Task.UpdatedAt, &x.Question); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(refs, &x.Task.Refs); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ListOpenEscalations implements the decision inbox job escalation definition.
func (s *Store) ListOpenEscalations(ctx context.Context, limit int) ([]RelayEvent, error) {
	if limit < 1 {
		limit = 1000
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT `+relayEventColumns+` FROM relay_events e
		WHERE e.kind='job.escalate' AND NOT EXISTS (
			SELECT 1 FROM relay_events resolved WHERE resolved.job_id=e.job_id
			AND resolved.kind IN ('job.joined','job.completed') AND resolved.id>e.id
		) AND NOT EXISTS (
			SELECT 1 FROM relay_events resolved WHERE resolved.kind='lane.event'
			AND resolved.owner_lane=e.owner_lane AND resolved.id>e.id
			AND resolved.event_id LIKE '%decision-escalation-' || e.id::text || '-%'
		) ORDER BY e.id DESC LIMIT $1`, limit)
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

// ListOpenLaneDecisions implements the decision inbox lane.event definition.
func (s *Store) ListOpenLaneDecisions(ctx context.Context, limit int) ([]RelayEvent, error) {
	if limit < 1 {
		limit = 1000
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT `+relayEventColumns+` FROM relay_events e
		WHERE e.kind='lane.event' AND e.text LIKE '[decision-needed]%' AND NOT EXISTS (
			SELECT 1 FROM relay_events resolved WHERE resolved.kind='lane.event' AND resolved.owner_lane=e.owner_lane
			AND resolved.text LIKE '[decision-answered] #' || e.id::text || ':%' AND resolved.id>e.id
		) ORDER BY e.id DESC LIMIT $1`, limit)
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
func (s *Store) GetDocumentByID(ctx context.Context, id int64) (Document, bool, error) {
	if id < 1 {
		return Document{}, false, errors.New("invalid document id")
	}
	var x Document
	e := s.pool.QueryRow(ctx, `SELECT id,key,kind,session,job,body,sha256,created_by,created_at,updated_at FROM documents WHERE id=$1`, id).Scan(&x.ID, &x.Key, &x.Kind, &x.Session, &x.Job, &x.Body, &x.SHA256, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt)
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
	if strings.TrimSpace(q) == "" || len(q) > 512 || !validText(session, 128) || (scope != "all" && scope != "ctx" && scope != "docs" && scope != "memory" && scope != "tasks") {
		return nil, errors.New("invalid search")
	}
	if scope == "tasks" {
		return s.searchTasks(ctx, q, session, limit)
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	out := []SearchResult{}
	// "#<n>" and bare-number queries are document-id lookups in the scopes
	// that contain documents (all, docs), the same way scope "tasks" resolves
	// task ids. The exact match always leads the page and a missing id yields
	// an empty result set rather than rows that merely mention the digits.
	exactDocKey := ""
	if scope == "all" || scope == "docs" {
		if m := numericIDQuery.FindStringSubmatch(strings.TrimSpace(q)); m != nil {
			id, e := strconv.ParseInt(m[1], 10, 64)
			if e != nil {
				return out, nil
			}
			row := s.pool.QueryRow(ctx, `SELECT key,session,kind,left(body,160),created_at FROM documents WHERE id=$2 AND ($1='' OR session=$1)`, session, id)
			var x SearchResult
			switch e := row.Scan(&x.Key, &x.Session, &x.Kind, &x.Snippet, &x.CreatedAt); {
			case e == nil:
				x.Scope, x.Title = "docs", x.Key
				out = append(out, x)
				exactDocKey = x.Key
			case errors.Is(e, pgx.ErrNoRows):
				return out, nil
			default:
				return nil, fmt.Errorf("search docs exact: %w", e)
			}
		}
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
	rows, e := s.pool.Query(ctx, `SELECT scope,key,session,kind,title,snippet,refs,created_at FROM (`+strings.Join(parts, " UNION ALL ")+") r ORDER BY created_at DESC LIMIT $3", session, q, limit+1)
	if e != nil {
		return nil, fmt.Errorf("search: %w", e)
	}
	defer rows.Close()
	for rows.Next() {
		var x SearchResult
		var refs []byte
		if e = rows.Scan(&x.Scope, &x.Key, &x.Session, &x.Kind, &x.Title, &x.Snippet, &refs, &x.CreatedAt); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(refs, &x.Refs); e != nil {
			return nil, e
		}
		if x.Scope == "docs" && x.Key == exactDocKey {
			continue
		}
		out = append(out, x)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	if len(out) > limit {
		out = out[:limit]
		for i := range out {
			out[i].Truncated = true
		}
	}
	if scope == "all" || scope == "docs" {
		if e = s.linkTaskDocs(ctx, out); e != nil {
			return nil, e
		}
	}
	return out, nil
}

// linkTaskDocs annotates docs-scope hits with the ids of tasks whose body_doc
// points at that document.  tasks.body_doc is additive DDL (#536) applied by
// migrate; the probe keeps a database opened by an older binary a no-op.  A
// transitional "key#section" pointer links to its key: the section is only a
// scroll position.
func (s *Store) linkTaskDocs(ctx context.Context, out []SearchResult) error {
	var hasBodyDoc bool
	if e := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema() AND column_name = 'body_doc' AND table_name = 'tasks')`).Scan(&hasBodyDoc); e != nil {
		return fmt.Errorf("search task docs probe: %w", e)
	}
	if !hasBodyDoc {
		return nil
	}
	keys := []string{}
	for _, x := range out {
		if x.Scope == "docs" {
			keys = append(keys, x.Key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	rows, e := s.pool.Query(ctx, `SELECT id, split_part(body_doc, '#', 1) FROM tasks WHERE body_doc <> '' AND split_part(body_doc, '#', 1) = ANY($1) ORDER BY id`, keys)
	if e != nil {
		return fmt.Errorf("search task docs: %w", e)
	}
	defer rows.Close()
	byDoc := map[string][]string{}
	for rows.Next() {
		var id int64
		var key string
		if e = rows.Scan(&id, &key); e != nil {
			return e
		}
		byDoc[key] = append(byDoc[key], strconv.FormatInt(id, 10))
	}
	if e = rows.Err(); e != nil {
		return e
	}
	for i := range out {
		if out[i].Scope == "docs" && len(byDoc[out[i].Key]) > 0 {
			if out[i].Refs == nil {
				out[i].Refs = Refs{}
			}
			out[i].Refs["tasks"] = byDoc[out[i].Key]
		}
	}
	return nil
}

var numericIDQuery = regexp.MustCompile(`^#?([0-9]+)$`)

// searchTasks implements scope "tasks": title FTS/ILIKE over the tasks table
// (all states, merged and dropped included) plus task_comments bodies when
// that table exists.  "#<n>" and bare-number queries are answered by exact id
// lookup first; the exact match always leads the page and a missing id yields
// an empty result set.  Default page is 20, hard cap 50; a truncated page sets
// Truncated on every returned row.  The session argument filters on lane.
func (s *Store) searchTasks(ctx context.Context, q, lane string, limit int) ([]SearchResult, error) {
	if limit < 1 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	var exactID int64 = -1
	if m := numericIDQuery.FindStringSubmatch(strings.TrimSpace(q)); m != nil {
		id, e := strconv.ParseInt(m[1], 10, 64)
		if e != nil {
			return []SearchResult{}, nil
		}
		exactID = id
	}
	refsExpr := `jsonb_strip_nulls(jsonb_build_object('id',t.id::text,'state',t.state,'lane',t.lane,'priority',t.priority::text,'pr',t.refs->>'pr','head_sha',t.refs->>'head_sha','report_path',t.refs->>'report_path'))`
	titleMatch := `(to_tsvector('simple',t.title) @@ plainto_tsquery('simple',$2) OR t.title ILIKE '%'||$4||'%' ESCAPE '\')`
	out := []SearchResult{}
	if exactID >= 0 {
		row := s.pool.QueryRow(ctx, `SELECT t.id::text,t.lane,t.kind,t.title,`+refsExpr+`,t.created_at FROM tasks t WHERE t.id=$2 AND ($1='' OR t.lane=$1)`, lane, exactID)
		var x SearchResult
		var refs []byte
		switch e := row.Scan(&x.Key, &x.Session, &x.Kind, &x.Title, &refs, &x.CreatedAt); {
		case e == nil:
			if e = json.Unmarshal(refs, &x.Refs); e != nil {
				return nil, e
			}
			x.Scope, x.Snippet = "tasks", x.Title
			out = append(out, x)
		case errors.Is(e, pgx.ErrNoRows):
			return out, nil
		default:
			return nil, fmt.Errorf("search tasks exact: %w", e)
		}
	}
	var comments bool
	if e := s.pool.QueryRow(ctx, `SELECT to_regclass('task_comments') IS NOT NULL`).Scan(&comments); e != nil {
		return nil, fmt.Errorf("search tasks probe: %w", e)
	}
	parts := []string{`SELECT 'tasks' scope,t.id::text key,t.lane session,t.kind,t.title,` +
		`COALESCE(NULLIF(ts_headline('simple',t.title,plainto_tsquery('simple',$2),'MaxWords=24,MinWords=8'),''),left(t.title,160)) snippet,` +
		refsExpr + ` refs,t.created_at,0 match_rank FROM tasks t WHERE ($1='' OR t.lane=$1) AND ` + titleMatch}
	if comments {
		// DISTINCT ON keeps one row per task no matter how many comments match —
		// otherwise a comment-heavy task would consume the LIMIT+1 window and the
		// page could report "complete" while other tasks went unfetched.
		parts = append(parts, `(SELECT DISTINCT ON (t.id) 'tasks' scope,t.id::text key,t.lane session,t.kind,t.title,`+
			`COALESCE(NULLIF(ts_headline('simple',c.body,plainto_tsquery('simple',$2),'MaxWords=24,MinWords=8'),''),left(c.body,160)) snippet,`+
			refsExpr+` refs,t.created_at,1 match_rank FROM task_comments c JOIN tasks t ON t.id=c.task_id WHERE ($1='' OR t.lane=$1) AND (to_tsvector('simple',c.body) @@ plainto_tsquery('simple',$2) OR c.body ILIKE '%'||$4||'%' ESCAPE '\') AND NOT `+titleMatch+` ORDER BY t.id, c.created_at DESC, c.id DESC)`)
	}
	rows, e := s.pool.Query(ctx, `SELECT scope,key,session,kind,title,snippet,refs,created_at FROM (`+strings.Join(parts, " UNION ALL ")+") r ORDER BY r.match_rank,r.created_at DESC LIMIT $3", lane, q, limit+1, like)
	if e != nil {
		return nil, fmt.Errorf("search tasks: %w", e)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for _, x := range out {
		seen[x.Key] = true
	}
	for rows.Next() {
		var x SearchResult
		var refs []byte
		if e = rows.Scan(&x.Scope, &x.Key, &x.Session, &x.Kind, &x.Title, &x.Snippet, &refs, &x.CreatedAt); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(refs, &x.Refs); e != nil {
			return nil, e
		}
		if seen[x.Key] {
			continue
		}
		seen[x.Key] = true
		out = append(out, x)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	if len(out) > limit {
		out = out[:limit]
		for i := range out {
			out[i].Truncated = true
		}
	}
	return out, nil
}
