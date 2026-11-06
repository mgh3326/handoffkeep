// Package store owns PostgreSQL persistence and validation.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

const MaxBytes = 64 << 10
const DocumentMaxBytes = 512 << 10
const CheckpointKeep = 500
const MemoryKeep = 2000

var ErrTrigramUnavailable = errors.New("PostgreSQL pg_trgm extension unavailable: grant CREATE on the database/schema or install pg_trgm as an administrator")
var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
var kinds = map[string]bool{"checkpoint": true, "handoff": true, "decision": true, "open_question": true, "next_action": true}
var memoryTypes = map[string]bool{"user": true, "feedback": true, "project": true, "reference": true}
var documentKinds = map[string]bool{"brief": true, "report": true, "answer": true, "handoff": true, "note": true, "other": true, "decision": true, "research": true, "design": true, "policy": true, "evidence": true}
var documentOriginClasses = map[string]bool{"authored": true, "job": true, "import": true}
var taskKinds = map[string]bool{"implement": true, "verify": true, "fix": true, "decide": true, "ops": true}

// TaskKinds is the server's task kind vocabulary — the single source of truth
// client hints render so the two cannot drift.
func TaskKinds() []string {
	xs := make([]string, 0, len(taskKinds))
	for k := range taskKinds {
		xs = append(xs, k)
	}
	sort.Strings(xs)
	return xs
}

var relayEventKinds = map[string]bool{"job.completed": true, "job.escalate": true, "job.joined": true, "job.lost": true, "job.revoked": true, "lane.event": true}

// PostgreSQL can infer a broader partial index from a narrower ON CONFLICT
// predicate. Keep both predicates explicit so a one-sided edit is testable.
const relayJobIndexPredicate = "kind IN ('job.completed','job.escalate','job.joined','job.lost','job.revoked')"
const relayJobConflictPredicate = "kind IN ('job.completed','job.escalate','job.joined','job.lost','job.revoked')"

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
	// ErrBenchCatalogSolGrade enforces the Sol-profile rule on the
	// server: Sol profiles may only ever be graded S+. The catalog write path
	// alone admits one placeholder shape that makes no grade claim — grade C
	// with score null — for the #594 E6 measurement rungs; any real
	// claim below S+ (a scored C, or an unscored A/B/S) still fails here.
	ErrBenchCatalogSolGrade = errors.New("bench_catalog_sol_grade")
	// ErrDecisionRefRequired rejects an area exception row without the
	// decision record or task comment that approved it — an exception is
	// never automatic.
	ErrDecisionRefRequired = errors.New("decision_ref_required")
	// ErrBenchAreaExceptionProfileUnknown rejects an exception whose
	// (profile, effort) has no bench_catalog row, live or retired — no
	// exception may name a profile the catalog does not know.
	ErrBenchAreaExceptionProfileUnknown = errors.New("bench_area_exception_profile_unknown")
	// ErrBenchAreaExceptionConflict rejects a write that would leave prefer
	// and exclude live for one (profile, effort, area) key. Flipping a live
	// row's action must go through retirement first.
	ErrBenchAreaExceptionConflict = errors.New("bench_area_exception_action_conflict")
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
	// ErrTaskJobRequired rejects a claim or a transition that would leave a
	// task in claimed/in_progress without claimed_by and refs.job_id
	// (decision/2026-09-27/task-job-linkage). A jobless task needs a recorded
	// no-job reason instead.
	ErrTaskJobRequired = errors.New("task_job_required")
	// ErrTaskProjectRequired rejects a create that carries no project. Every
	// new task row must be classified so the queue stays groupable
	// (decision/2026-09-27/hk-queue-cleanup, option A item 2).
	ErrTaskProjectRequired = errors.New("task_project_required")
	// ErrTaskProjectUnknown rejects a project outside the server-configured
	// task_projects vocabulary — wrong spellings would fragment the groups
	// the field exists to provide.
	ErrTaskProjectUnknown = errors.New("task_project_unknown")
	// ErrTaskAreaUnknown rejects an area outside the TaskAreas vocabulary.
	// The text is the allowed set, so every surface that reports it names
	// the values a caller may use.
	ErrTaskAreaUnknown = errors.New("invalid_request")
	// ErrBenchRepsRetryable marks a batch write Postgres aborted on its own:
	// a deadlock victim (SQLSTATE 40P01) or a serialization failure (40001).
	// The transaction rolled back, nothing was written, and the client may
	// retry the identical batch — unlike a 409 conflict the failure says
	// nothing about the rows.
	ErrBenchRepsRetryable = errors.New("bench_reps_retryable")
)

// TaskRefs holds the durable links that let a captain resume work without
// embedding credentials or implementation details in the queue itself.
type TaskRefs struct {
	PR              string           `json:"pr,omitempty"`
	HeadSHA         string           `json:"head_sha,omitempty"`
	ReportPath      string           `json:"report_path,omitempty"`
	JobID           string           `json:"job_id,omitempty"`
	DecisionOptions *DecisionOptions `json:"decision_options,omitempty"`
	// DecisionRequest is the task's current structured decision request
	// (#618). It is written only by RecordDecisionRequest and
	// ResolveDecisionRequest; while it exists, decision_options are its
	// choices and are not patchable by a generic transition.
	DecisionRequest *DecisionRequest `json:"decision_request,omitempty"`
	Linear          *TaskLinear      `json:"linear,omitempty"`
	// OriginPR and OriginTask are the typed "this row came from" relations
	// shared with #494 (hk:doc design/2026-09-21/task493-disposition-contract
	// §3). Unknown refs keys are dropped by the next transition, so these names
	// are part of the durable contract, not presentation.
	OriginPR    string       `json:"origin_pr,omitempty"`
	OriginTask  int64        `json:"origin_task,omitempty"`
	Disposition *Disposition `json:"disposition,omitempty"`
	// RevertPR is the PR that reverted this task's merge. Only a
	// mergewake.revert event writes it (rule 9).
	RevertPR string `json:"revert_pr,omitempty"`
}

type TaskLinear struct {
	MirrorReason string   `json:"mirror_reason,omitempty"`
	IssueID      string   `json:"issue_id,omitempty"`
	Identifier   string   `json:"identifier,omitempty"`
	Sync         bool     `json:"sync"`
	Tier         string   `json:"tier,omitempty"`
	Grade        string   `json:"grade,omitempty"`
	Brief        string   `json:"brief,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	Report       string   `json:"report,omitempty"`
	Verify       string   `json:"verify,omitempty"`
	Decision     string   `json:"decision,omitempty"`
	DeploySHA    string   `json:"deploy_sha,omitempty"`
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

// CheckDecisionOptions is exported for local producers such as the CLI.
func CheckDecisionOptions(x DecisionOptions) error {
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
	// TaskEventProject marks task_events rows written by SetTaskProject.
	// Like relane rows they carry classification values, not states, in
	// from/to — the same readers that filter kind must skip them.
	TaskEventProject = "project"
	// TaskEventArea marks task_events rows written by SetTaskArea. from/to
	// carry area names ("" for unset), never states.
	TaskEventArea = "area"
	// TaskEventStateMigration marks rows written by the state migration and
	// its rollback; from/to carry state names of the old and new vocabulary.
	TaskEventStateMigration = "state_migration"
	// TaskEventAwaiting marks a change to the awaiting_decision flag. The
	// state does not change, so the row is not a transition.
	TaskEventAwaiting = "awaiting_decision"
	// TaskEventParent marks a parent_id change.
	TaskEventParent = "parent"
	// TaskEventRelation marks a relation added or removed.
	TaskEventRelation = "relation"
	// TaskEventAutoClose marks an auto-close evaluation that changed nothing:
	// the skip when every child was canceled, and a child reopened under a
	// completed parent.
	TaskEventAutoClose = "auto_close"
)

// taskAreas is the closed task area vocabulary, in display order. Go has no
// constant arrays, so the list is unexported and TaskAreas hands out copies:
// no importing package can change what is accepted or advertised, and
// changing the list is a code change, never a runtime write. An area is
// routing metadata. It is not part of any claim or receipt and is not an
// authority input.
var taskAreas = [...]string{"blender", "unity", "web-qa", "ui-design", "image", "web-frontend", "go-backend", "python", "trading", "shell-wrk", "docs", "research", "ops"}

// TaskAreas returns the closed area vocabulary in display order. The slice is
// a fresh copy on every call; writing to it changes nothing.
func TaskAreas() []string {
	return append([]string(nil), taskAreas[:]...)
}

// ValidTaskArea reports whether v is exactly one of the TaskAreas(). There is no
// case folding, trimming or normalisation: a near miss is not an area.
func ValidTaskArea(v string) bool {
	for _, a := range taskAreas {
		if v == a {
			return true
		}
	}
	return false
}

// checkTaskAreaFilter validates a list filter. It is tri-state like the
// project filter: nil means no filter, "" selects tasks with no area, and any
// other value must be in the vocabulary. An unknown value is an error, not an
// empty result, so a misspelt filter cannot read as "no such tasks".
func checkTaskAreaFilter(area *string) error {
	if area == nil || *area == "" {
		return nil
	}
	return CheckTaskArea(area)
}

// CheckTaskArea validates an optional area; the CLI and API call it so the
// refusal text is the same everywhere. nil means unset and is always
// valid; any non-nil value, the empty string included, must be in the
// vocabulary.
func CheckTaskArea(area *string) error {
	if area == nil || ValidTaskArea(*area) {
		return nil
	}
	return fmt.Errorf("%w: area must be one of %s", ErrTaskAreaUnknown, strings.Join(taskAreas[:], ", "))
}

// SetsAwaiting reports whether the event began a wait on a decision: an
// awaiting_decision event that set the flag, or, in history written before the
// flag, a transition into the old needs_decision state.
func (e TaskEvent) SetsAwaiting() bool {
	return (e.Kind == TaskEventAwaiting && e.ToValue == "true") || (e.Kind == TaskEventTransition && e.To == legacyNeedsDecision)
}

type TaskEvent struct {
	ID     int64  `json:"id"`
	TaskID int64  `json:"task_id"`
	Kind   string `json:"kind"`
	From   string `json:"from"`
	To     string `json:"to"`
	By     string `json:"by"`
	Note   string `json:"note,omitempty"`
	// NoJob records why a transition into claimed/in_progress was allowed
	// without claimed_by + refs.job_id. It is set only on events where the
	// exemption was actually used; linked transitions leave it empty.
	NoJob string    `json:"no_job,omitempty"`
	Refs  *TaskRefs `json:"refs,omitempty"`
	// Rebind records the old/new job link for a task-job takeover. It is
	// encoded in the event refs snapshot, not in the task's current refs.
	Rebind *TaskJobRebind `json:"rebind,omitempty"`
	At     time.Time      `json:"at"`
	// Source and SourceRef name the automatic producer of an event (for
	// example migration or hk.auto_close). A redelivered (source, source_ref)
	// pair changes nothing. Field, FromValue and ToValue describe a change to
	// a field other than the state; Auto marks events hk wrote on its own;
	// Applied is false for an event recorded without effect.
	Source    string `json:"source,omitempty"`
	SourceRef string `json:"source_ref,omitempty"`
	Field     string `json:"field,omitempty"`
	FromValue string `json:"from_value,omitempty"`
	ToValue   string `json:"to_value,omitempty"`
	Auto      bool   `json:"auto,omitempty"`
	Applied   bool   `json:"applied"`
}

// TaskJobRebind is the immutable audit payload for a task job-link rebind.
type TaskJobRebind struct {
	FromJob string `json:"from_job"`
	ToJob   string `json:"to_job"`
	By      string `json:"by"`
	Reason  string `json:"reason"`
}

// Task.BodyDoc points at the hk document holding the task's body ("key" or
// the transitional "key#section"). The body text stays in documents; tasks
// keep only this pointer (hk:doc decision/2026-09-21/task536-body-storage-approved).
type Task struct {
	ID             int64     `json:"id"`
	Lane           string    `json:"lane"`
	ParentLane     string    `json:"parent_lane,omitempty"`
	Title          string    `json:"title"`
	Kind           string    `json:"kind"`
	State          string    `json:"state"`
	Priority       int       `json:"priority"`
	Refs           TaskRefs  `json:"refs"`
	ClaimedBy      string    `json:"claimed_by,omitempty"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	LastActivityAt time.Time `json:"last_activity_at"`
	BodyDoc        string    `json:"body_doc,omitempty"`
	// Project is the task's entry in the server-configured task_projects
	// vocabulary. NULL on rows created before the field existed — legacy
	// rows keep it (no backfill) and render as the "no project" bucket.
	// The field is always emitted, so a null means an unset project rather
	// than a server that predates the column.
	Project *string `json:"project"`
	// Area is the task's entry in the TaskAreas vocabulary. NULL on rows
	// created before the field existed and on any task nobody has tagged;
	// always emitted, so a null means unset.
	Expected *string `json:"expected"`
	Area     *string `json:"area"`
	// Category is derived from State by the fixed table in states.go and is
	// never stored; API rows carry it read-only.
	Category string `json:"category"`
	// AwaitingDecision is the flag that replaced the needs_decision state: the
	// task keeps its work state while it waits on the operator.
	AwaitingDecision bool `json:"awaiting_decision"`
	// ParentID is the single, acyclic parent link. It is unrelated to
	// ParentLane and Refs.OriginTask.
	ParentID *int64 `json:"parent_id"`
	// Lifecycle timestamps, set when the task first enters the matching
	// category; NULL on rows that predate them (no backfill).
	StartedAt       *time.Time `json:"started_at"`
	CompletedAt     *time.Time `json:"completed_at"`
	CanceledAt      *time.Time `json:"canceled_at"`
	TriagedAt       *time.Time `json:"triaged_at"`
	StartedTriageAt *time.Time `json:"started_triage_at"`
	MilestoneID     *int64     `json:"milestone_id"`
	Labels          []int64    `json:"labels"`
	DueAt           *time.Time `json:"due_at"`
	SLAStartedAt    *time.Time `json:"sla_started_at"`
	SLAMediumRiskAt *time.Time `json:"sla_medium_risk_at"`
	SLAHighRiskAt   *time.Time `json:"sla_high_risk_at"`
	SLABreachesAt   *time.Time `json:"sla_breaches_at"`
	SLARisk         string     `json:"sla_risk"`
	// Source and SourceRef name the producer of a task that came in through
	// triage intake or a ticket set; (source, source_ref) is unique. Tasks
	// filed with tasks add leave both empty.
	Source    string `json:"source,omitempty"`
	SourceRef string `json:"source_ref,omitempty"`
	// Stalled is computed at read time: a started task with no
	// awaiting_decision and no activity within started_stalled_hours.
	Stalled   bool           `json:"stalled"`
	Children  []TaskChild    `json:"children,omitempty"`
	Relations []TaskRelation `json:"relations,omitempty"`
	Events    []TaskEvent    `json:"events,omitempty"`
}

// RelayEvent is a durable report from a worker to its owning lane.  Delivery
// state belongs here rather than in the relay hub so it survives restarts.
type RelayEvent struct {
	WrkReceipt     *wrkrecords.Receipt `json:"wrk_receipt,omitempty"`
	ReportSHA      string              `json:"report_sha256,omitempty"`
	ID             int64               `json:"id"`
	Kind           string              `json:"kind"`
	JobID          string              `json:"job_id"`
	Epoch          int                 `json:"epoch"`
	OwnerLane      string              `json:"owner_lane"`
	Machine        string              `json:"machine"`
	PaneID         string              `json:"pane_id"`
	ReportPath     string              `json:"report_path"`
	ReportLastLine string              `json:"report_last_line"`
	Question       string              `json:"question"`
	PR             string              `json:"pr"`
	Head           string              `json:"head"`
	Reason         string              `json:"reason"`
	EventID        string              `json:"event_id"`
	Text           string              `json:"text"`
	EventTime      *time.Time          `json:"event_time"`
	ReceivedAt     time.Time           `json:"received_at"`
	DeliveredAt    *time.Time          `json:"delivered_at"`
	DeliveredTo    string              `json:"delivered_to"`
	Attempts       int                 `json:"attempts"`
}

type Store struct {
	pool        *pgxpool.Pool
	linearSync  atomic.Bool
	linearBoard atomic.Bool
	wrk         atomic.Pointer[wrkrecords.Engine]
	// autoCloseOff inverts the auto_close_parent setting so the zero Store has
	// it on, the default.
	autoCloseOff atomic.Bool
	// stalledHours is started_stalled_hours; zero means the default.
	stalledHours atomic.Int64
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
	// TaskID names the task the document belongs to; nil is unknown or none.
	TaskID *int64 `json:"task_id,omitempty"`
	// OriginClass is authored, job or import; empty is unknown (NULL).
	OriginClass string `json:"origin_class,omitempty"`
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
	// BenchOutcome's columns are part of the insert-once content; later
	// outcomes go through PatchBenchRepOutcome.
	BenchOutcome
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

// BenchAreaException is one row of the (profile, effort, area)-keyed routing
// exception list (hk 1944, K5 of hk 1937). It sits next to the grade catalog
// as canon but is deliberately not a grade row: a grade stays one axis, and
// an exception only adjusts routing within or under it. Effort "" means
// every effort of that profile; Action is the closed pair exclude|prefer.
// DecidedBy and DecidedAt are server-set on write — unlike the catalog,
// where the caller supplies provenance, the exception writer is always the
// authenticated operator client. ExpiresWithModelID names the model id the
// evidence was measured on; readers deactivate a row whose catalog model id
// moved on.
type BenchAreaException struct {
	Profile            string     `json:"profile"`
	Effort             string     `json:"effort"`
	Area               string     `json:"area"`
	Action             string     `json:"action"`
	EvidenceRefs       []string   `json:"evidence_refs"`
	DecisionRef        string     `json:"decision_ref"`
	DecidedBy          string     `json:"decided_by"`
	DecidedAt          time.Time  `json:"decided_at"`
	ExpiresWithModelID string     `json:"expires_with_model_id"`
	RetiredAt          *time.Time `json:"retired_at"`
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
	p, err := newPool(ctx, url)
	if err != nil {
		return nil, err
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	s := &Store{pool: p}
	if err = retryDeadlock(ctx, migrateAttempts, s.migrateIfBehind); err != nil {
		p.Close()
		return nil, err
	}
	return s, nil
}

// migrateIfBehind runs Store.migrate only when the schema is missing a version
// it writes. On a current schema it takes no table lock, so opening a store
// beside live writers cannot deadlock with them (lockorder.go, level 1).
func (s *Store) migrateIfBehind(ctx context.Context) error {
	current, err := migrateCurrent(ctx, s.pool)
	if err != nil || current {
		return err
	}
	return s.migrate(ctx)
}

// migrateAttempts bounds retryDeadlock around Store.migrate. A migration that
// has DDL to run takes table locks in statement order, which no row-level
// lock order can match, so while another process writes it can be chosen as
// a deadlock victim (lockorder.go, level 1). Its transaction is idempotent and
// rolled back whole, so running it again is safe.
const migrateAttempts = 3

// retryDeadlock runs f up to attempts times while it fails with a deadlock
// (SQLSTATE 40P01). It is the fallback for a transaction that cannot follow
// the lock order, not a substitute for it.
func retryDeadlock(ctx context.Context, attempts int, f func(context.Context) error) error {
	var err error
	for i := 0; i < attempts; i++ {
		var pgErr *pgconn.PgError
		if err = f(ctx); !errors.As(err, &pgErr) || pgErr.Code != "40P01" {
			return err
		}
		log.Printf("store.deadlock_retry attempt=%d err=%q", i+1, err.Error())
	}
	return err
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
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
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
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'task_events' AND column_name = 'kind') THEN ALTER TABLE task_events ADD COLUMN kind TEXT NOT NULL DEFAULT 'transition' CHECK(kind IN ('transition','relane')); END IF; END $$`,
		// task_events.no_job records why a transition into claimed/in_progress
		// ran without claimed_by + refs.job_id (the explicit exception under
		// decision/2026-09-27/task-job-linkage). Constant default keeps the
		// add metadata-only on existing databases.
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'task_events' AND column_name = 'no_job') THEN ALTER TABLE task_events ADD COLUMN no_job TEXT NOT NULL DEFAULT ''; END IF; END $$`}
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
	if err := migrateOnce(ctx, tx, 7, func() error {
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
		return execAll(ctx, tx, v7)
	}); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 9, func() error {
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
		return execAll(ctx, tx, v9)
	}); err != nil {
		return err
	}
	v10 := []string{
		`CREATE TABLE IF NOT EXISTS linear_issues (task_id BIGINT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE, issue_id TEXT NOT NULL, identifier TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS linear_outbox (id BIGSERIAL PRIMARY KEY, task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, seq INTEGER NOT NULL, op TEXT NOT NULL CHECK(op IN ('issue_create','issue_state','terminal_comment','terminal_archive')), payload JSONB NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','sent','failed','skipped')) DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TIMESTAMPTZ NOT NULL, last_error TEXT NOT NULL DEFAULT '', remote_id TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, UNIQUE(task_id,seq))`,
		`CREATE INDEX IF NOT EXISTS linear_outbox_pending ON linear_outbox(next_attempt_at,task_id,seq) WHERE state='pending'`,
		`INSERT INTO schema_version(version) VALUES (10) ON CONFLICT DO NOTHING`,
	}
	if err := execAll(ctx, tx, v10); err != nil {
		return err
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
	if err := execAll(ctx, tx, v11); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 12, func() error {
		v12 := []string{
			`CREATE TABLE IF NOT EXISTS bench_catalog (profile TEXT, effort TEXT NOT NULL DEFAULT '', model_id TEXT NOT NULL, pool TEXT NOT NULL, grade TEXT NOT NULL CHECK(grade IN ('S+','S','A+','A','B','C')), score DOUBLE PRECISION, gate TEXT NOT NULL DEFAULT 'default' CHECK(gate IN ('default','escalation','consult_only')), gate_reason TEXT, benchmark_source TEXT, benchmark_annotation TEXT, boundary_version TEXT NOT NULL DEFAULT '', deviation_ref TEXT NOT NULL, decided_at TIMESTAMPTZ NOT NULL, decided_by TEXT NOT NULL, retired_at TIMESTAMPTZ, PRIMARY KEY(profile, effort))`,
			`CREATE INDEX IF NOT EXISTS bench_catalog_pool ON bench_catalog(pool) WHERE retired_at IS NULL`,
			`INSERT INTO bench_catalog(profile, effort, model_id, pool, grade, boundary_version, deviation_ref, decided_at, decided_by) SELECT profile, '', '', '', grade, boundary_version, deviation_ref, decided_at, decided_by FROM bench_grades ON CONFLICT (profile, effort) DO NOTHING`,
			`INSERT INTO schema_version(version) VALUES (12)`,
		}
		return execAll(ctx, tx, v12)
	}); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 13, func() error {
		// Version 13 admits the two hub terminal signals. Rebuild the partial
		// index in the same transaction as the CHECK so a newly accepted kind
		// always has the same five-field idempotency key as older job events.
		v13 := []string{
			`ALTER TABLE relay_events DROP CONSTRAINT relay_events_kind_check`,
			`ALTER TABLE relay_events ADD CONSTRAINT relay_events_kind_check CHECK(kind IN ('job.completed','job.escalate','job.joined','job.lost','job.revoked','lane.event'))`,
			`DROP INDEX relay_events_idempotency`,
			`CREATE UNIQUE INDEX relay_events_idempotency ON relay_events(kind, job_id, epoch, report_path, reason) WHERE ` + relayJobIndexPredicate,
			`INSERT INTO schema_version(version) VALUES (13)`,
		}
		return execAll(ctx, tx, v13)
	}); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 14, func() error {
		// Version 14 (#618) admits kind='decision' task_events: a decision
		// request or its resolution recorded without a state change. They
		// cannot be 'transition' rows — readers of "to"='merged' (deploys,
		// disposition summary) would count a same-state row on a merged
		// task, which is exactly where an uncleaned request is closed. The
		// CHECK swap takes an ACCESS EXCLUSIVE lock, so it is version gated
		// and runs once, like v7. It is 14, not 13: #627 (relay_events
		// job.lost/revoked) claims 13, and two different blocks behind one
		// version number would leave the second deploy's DDL unapplied.
		//
		// The swap is one ALTER and the new CHECK is NOT VALID, so the
		// ACCESS EXCLUSIVE lock covers catalog changes only — no scan of
		// task_events while claims and transitions wait. New rows are checked
		// immediately. Existing rows need no re-check: each one already
		// satisfied the replaced CHECK (transition, relane), a subset of the
		// new one. The constraint therefore reads convalidated=false; an
		// operator may run `ALTER TABLE task_events VALIDATE CONSTRAINT
		// task_events_kind_check` later, which takes SHARE UPDATE EXCLUSIVE
		// and does not block reads or writes. Not run here: migrate holds its
		// locks until its single transaction commits, so validating inside
		// it would bring the scan back under the exclusive lock.
		v14 := []string{
			`ALTER TABLE task_events DROP CONSTRAINT IF EXISTS task_events_kind_check, ADD CONSTRAINT task_events_kind_check CHECK(kind IN ('transition','relane','decision')) NOT VALID`,
			`INSERT INTO schema_version(version) VALUES (14)`,
		}
		return execAll(ctx, tx, v14)
	}); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 15, func() error {
		// Version 15 (#763) adds the task project: tasks.project is a
		// nullable column classified against task_projects, the
		// server-configured closed vocabulary. Existing rows stay NULL —
		// classification of legacy rows is an explicit operator action
		// (tasks project <id> <p>), never a guess by migration.
		//
		// The seed is the #762 vocabulary written with ON CONFLICT DO
		// NOTHING, so a vocabulary the operator already extended is
		// preserved verbatim. The kind CHECK swap admits 'project' events
		// and follows the v14 pattern: NOT VALID, lock-heavy DDL runs once
		// behind the version gate, existing rows need no re-check.
		v15 := []string{
			`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS project TEXT`,
			`CREATE TABLE IF NOT EXISTS task_projects (name TEXT PRIMARY KEY CHECK(name ~ '^[A-Za-z0-9._-]{1,128}$'), created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL)`,
			`INSERT INTO task_projects(name,created_by,created_at) SELECT name,'seed',now() FROM (VALUES('admiral'),('agent-skills'),('auto_trader'),('auto_trader-operator'),('brewdial'),('experiment'),('fillwire'),('fleet-ops'),('handoffkeep'),('herdr'),('other'),('paneglance'),('panewire'),('plane-pilot'),('remy-client'),('robin-prefect-automations'),('scopefuel'),('wrk')) v(name) ON CONFLICT (name) DO NOTHING`,
			`ALTER TABLE task_events DROP CONSTRAINT IF EXISTS task_events_kind_check, ADD CONSTRAINT task_events_kind_check CHECK(kind IN ('transition','relane','decision','project')) NOT VALID`,
			`INSERT INTO schema_version(version) VALUES (15)`,
		}
		return execAll(ctx, tx, v15)
	}); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 16, func() error {
		v16 := []string{
			`ALTER TABLE chat_questions ADD COLUMN conversation_id TEXT NOT NULL DEFAULT 'operator-desk'`,
			`ALTER TABLE chat_messages ADD COLUMN conversation_id TEXT NOT NULL DEFAULT 'operator-desk'`,
			`ALTER TABLE chat_messages ADD COLUMN source_channel TEXT NOT NULL DEFAULT 'legacy'`,
			`ALTER TABLE chat_messages ADD COLUMN origin_event_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE chat_messages ADD COLUMN origin_timestamp TIMESTAMPTZ`,
			`ALTER TABLE chat_messages ADD COLUMN semantic_hash TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE chat_messages DROP CONSTRAINT chat_messages_relay_state_check, ADD CONSTRAINT chat_messages_relay_state_check CHECK(relay_state IN ('stored','delivered','failed','not_sent'))`,
			`UPDATE chat_messages SET relay_state='not_sent',delivered_at=NULL WHERE author='desk'`,
			`ALTER TABLE chat_messages ADD CONSTRAINT chat_messages_author_relay_check CHECK((author='desk' AND relay_state='not_sent' AND delivered_at IS NULL) OR (author='operator' AND relay_state<>'not_sent'))`,
			`CREATE UNIQUE INDEX chat_messages_event_key ON chat_messages(conversation_id,source_channel,origin_event_id) WHERE origin_event_id<>''`,
			`DROP INDEX chat_messages_undelivered`,
			`CREATE INDEX chat_messages_undelivered ON chat_messages(id ASC) WHERE author='operator' AND relay_state='stored'`,
			`CREATE TABLE chat_message_questions (message_id BIGINT NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE, question_id TEXT NOT NULL, relation_kind TEXT NOT NULL CHECK(relation_kind IN ('posted','reply','resolve')), question_text TEXT NOT NULL, PRIMARY KEY(message_id,question_id,relation_kind))`,
			`CREATE INDEX chat_message_questions_question ON chat_message_questions(question_id,message_id)`,
			// Historic relay rows identify the message and Q explicitly. Do not infer
			// links from timestamps, body text, or lane proximity.
			`DO $$ BEGIN IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='relay_events' AND column_name='event_id') THEN INSERT INTO chat_message_questions(message_id,question_id,relation_kind,question_text) SELECT m.id,q.id,'reply',q.body FROM relay_events e JOIN chat_messages m ON m.id=CASE WHEN e.event_id ~ '^chat-[0-9]{1,18}(-[0-9a-f]{8})?$' THEN substring(e.event_id FROM '^chat-([0-9]+)')::bigint END AND m.author='operator' JOIN chat_questions q ON q.id=e.question WHERE e.kind='lane.event' AND e.reason='operator_chat' AND e.question<>'' ON CONFLICT DO NOTHING; END IF; END $$`,
			`INSERT INTO schema_version(version) VALUES (16)`,
		}
		return execAll(ctx, tx, v16)
	}); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 17, func() error {
		// Version 17 (MGH-36 PR-1) gives chat questions a monotonic revision
		// the assistant answer path compares against, and the single-answer
		// pointer answer_message_id. The notification outbox is hk's durable
		// record of lane notifications owed for assistant-path resolutions;
		// event_id is deterministic so a replayed write inserts nothing.
		// ADD COLUMN with a constant DEFAULT is metadata-only; both ALTERs
		// take a brief ACCESS EXCLUSIVE lock, so they sit behind the version
		// gate like v14/v16 instead of running at every start.
		v17 := []string{
			`ALTER TABLE chat_questions ADD COLUMN revision INTEGER NOT NULL DEFAULT 1`,
			`ALTER TABLE chat_questions ADD COLUMN answer_message_id BIGINT`,
			`CREATE TABLE notification_outbox (id BIGSERIAL PRIMARY KEY, kind TEXT NOT NULL CHECK(kind IN ('decision_answered','chat_answer')), target_lane TEXT NOT NULL, event_id TEXT NOT NULL UNIQUE, text TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, sent_at TIMESTAMPTZ, hub_row_id BIGINT)`,
			`CREATE INDEX notification_outbox_unsent ON notification_outbox(id) WHERE sent_at IS NULL`,
			`INSERT INTO schema_version(version) VALUES (17)`,
		}
		return execAll(ctx, tx, v17)
	}); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 18, func() error {
		// Version 18 (hk 1940) adds the task area: tasks.area is a nullable
		// column validated against the TaskAreas code constant, not a table
		// and not a CHECK, because the vocabulary changes with the code and a
		// CHECK would freeze a copy of it in the schema. Existing rows stay
		// NULL; tagging them is an explicit later action, never a guess by
		// migration. The kind CHECK swap admits 'area' events and follows
		// the v14/v15 pattern: NOT VALID, lock-heavy DDL runs once behind
		// the version gate, existing rows need no re-check.
		v18 := []string{
			`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS area TEXT`,
			`ALTER TABLE task_events DROP CONSTRAINT IF EXISTS task_events_kind_check, ADD CONSTRAINT task_events_kind_check CHECK(kind IN ('transition','relane','decision','project','area')) NOT VALID`,
			`INSERT INTO schema_version(version) VALUES (18)`,
		}
		return execAll(ctx, tx, v18)
	}); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 19, func() error {
		// Version 19 (hk 1944) adds the bench area exception list: per-area
		// routing exceptions kept next to, but separate from, the grade
		// catalog. The area column is validated in code against the TaskAreas
		// constant (same reason as v18 — a CHECK would freeze a copy of the
		// vocabulary), the action CHECK is closed, and evidence_refs carries
		// at least one ref enforced by the write path. Retired rows stay in
		// place; there is no DELETE.
		v19 := []string{
			`CREATE TABLE IF NOT EXISTS bench_area_exceptions (profile TEXT NOT NULL, effort TEXT NOT NULL DEFAULT '', area TEXT NOT NULL, action TEXT NOT NULL CHECK(action IN ('exclude','prefer')), evidence_refs TEXT[] NOT NULL, decision_ref TEXT NOT NULL, decided_by TEXT NOT NULL, decided_at TIMESTAMPTZ NOT NULL, expires_with_model_id TEXT NOT NULL, retired_at TIMESTAMPTZ, PRIMARY KEY(profile, effort, area))`,
			`CREATE INDEX IF NOT EXISTS bench_area_exceptions_area ON bench_area_exceptions(area) WHERE retired_at IS NULL`,
			`INSERT INTO schema_version(version) VALUES (19)`,
		}
		return execAll(ctx, tx, v19)
	}); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 21, func() error {
		return execAll(ctx, tx, []string{
			`ALTER TABLE tasks DROP CONSTRAINT tasks_state_check, ADD CONSTRAINT tasks_state_check CHECK(state IN ('backlog','reopened','claimed','in_progress','verifying','join','hold','needs_decision','merged','dropped'))`,
			`INSERT INTO schema_version(version) VALUES (21)`,
		})
	}); err != nil {
		return err
	}

	if err := migrateOnce(ctx, tx, 22, func() error {
		var priorLockTimeout string
		if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
			return err
		}
		if err := execAll(ctx, tx, []string{`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS expected TEXT`, `INSERT INTO schema_version(version) VALUES (22)`}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	if err := migrateOnce(ctx, tx, 25, func() error {
		// Version 25 (HK-2) adds the cross-host job record: runtug claims a
		// job_id on the executing host and merges metadata as the job moves
		// claimed -> spawned -> a terminal state. CREATE TABLE/INDEX take no
		// lock beyond the catalog; both are additive and IF NOT EXISTS.
		v25 := []string{
			`CREATE TABLE IF NOT EXISTS jobs (job_id TEXT PRIMARY KEY CHECK(job_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'), task_id BIGINT NOT NULL, host TEXT NOT NULL CHECK(host ~ '^[a-z0-9-]{1,32}$'), state TEXT NOT NULL CHECK(state IN ('claimed','spawned','completed','escalated','lost')), role TEXT NOT NULL DEFAULT '', label TEXT NOT NULL DEFAULT '', family TEXT NOT NULL DEFAULT '', profile TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', pool TEXT NOT NULL DEFAULT '', owner_lane TEXT NOT NULL DEFAULT '', parent_lane TEXT NOT NULL DEFAULT '', parent_job TEXT NOT NULL DEFAULT '', pane_id TEXT NOT NULL DEFAULT '', worktree JSONB, report_doc TEXT NOT NULL DEFAULT '', result JSONB, created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS jobs_task ON jobs(task_id, created_at ASC, job_id ASC)`,
			`INSERT INTO schema_version(version) VALUES (25)`,
		}
		return execAll(ctx, tx, v25)
	}); err != nil {
		return err
	}

	if err := migrateOnce(ctx, tx, 26, func() error {
		// Version 26 (hk 2572) admits the dots notification kinds so the Linear
		// webhook can queue lane notifications on the existing outbox. The CHECK
		// swap takes a brief ACCESS EXCLUSIVE lock on notification_outbox; NOT
		// VALID skips re-checking existing rows, which all hold older kinds.
		v26 := []string{
			`ALTER TABLE notification_outbox DROP CONSTRAINT notification_outbox_kind_check`,
			`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check CHECK(kind IN ('decision_answered','chat_answer','dots_report','dots_decision','dots_followup')) NOT VALID`,
			`INSERT INTO schema_version(version) VALUES (26)`,
		}
		return execAll(ctx, tx, v26)
	}); err != nil {
		return err
	}

	if err := migrateOnce(ctx, tx, 27, func() error {
		// Version 27 (hk 2615) adds the tables for operator decisions through
		// Linear: which issue carries which request, and the comments, closes and Discord
		// posts still owed. Creating the foreign key takes a brief SHARE ROW
		// EXCLUSIVE lock on tasks; nothing else existing is touched.
		v27 := []string{
			`CREATE TABLE linear_decision_issues (request_id TEXT PRIMARY KEY, task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, ver TEXT NOT NULL CHECK(ver ~ '^[0-9a-f]{8}$'), issue_id TEXT NOT NULL UNIQUE, identifier TEXT NOT NULL, url TEXT NOT NULL, superseded_by TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
			`CREATE TABLE linear_decision_ops (id BIGSERIAL PRIMARY KEY, dedupe_key TEXT NOT NULL UNIQUE, op TEXT NOT NULL CHECK(op IN ('comment','close','discord')), issue_id TEXT NOT NULL, body TEXT NOT NULL DEFAULT '', state_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','sent','failed','skipped')), attempts INT NOT NULL DEFAULT 0, next_attempt_at TIMESTAMPTZ NOT NULL, last_error TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
			`CREATE INDEX linear_decision_ops_pending ON linear_decision_ops(next_attempt_at, id) WHERE state='pending'`,
			`INSERT INTO schema_version(version) VALUES (27)`,
		}
		return execAll(ctx, tx, v27)
	}); err != nil {
		return err
	}

	// Lane-knownness probes for relane run once per batch item. These
	// indexes keep each probe an index lookup instead of a full scan —
	// the existing relay_events index on owner_lane is partial and
	// cannot serve a bare owner_lane probe. They must run after the
	// relay_events and chat_questions CREATE TABLEs above: under a
	// test-only search_path an earlier statement would resolve to the
	// public table (or fail where no table exists yet).
	if err := execAll(ctx, tx, []string{
		`CREATE INDEX IF NOT EXISTS relay_events_owner_lane ON relay_events(owner_lane)`,
		`CREATE INDEX IF NOT EXISTS chat_questions_lane ON chat_questions(lane)`,
	}); err != nil {
		return err
	}
	if err := migrateTaskComments(ctx, tx); err != nil {
		return err
	}
	if err := migrateQuota(ctx, tx); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 28, func() error {
		// Version 28 (hk 2639) admits explicit unrated catalog and grade rows.
		// Each CHECK swap takes ACCESS EXCLUSIVE once; NOT VALID avoids a
		// scan under that lock because every existing grade remains allowed.
		return execAll(ctx, tx, []string{
			`ALTER TABLE bench_grades DROP CONSTRAINT bench_grades_grade_check, ADD CONSTRAINT bench_grades_grade_check CHECK(grade IN ('S+','S','A+','A','B','C','unrated')) NOT VALID`,
			`ALTER TABLE bench_catalog DROP CONSTRAINT bench_catalog_grade_check, ADD CONSTRAINT bench_catalog_grade_check CHECK(grade IN ('S+','S','A+','A','B','C','unrated')) NOT VALID`,
			`INSERT INTO schema_version(version) VALUES (28)`,
		})
	}); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 29, func() error { return execAll(ctx, tx, linearBoardV29) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 30, func() error {
		// Version 30 adds the transactional change feed. CREATE TRIGGER takes
		// SHARE ROW EXCLUSIVE on the four source tables; new tables are private
		// to this migration until commit. Existing rows enter through bootstrap.
		return execAll(ctx, tx, syncMigrations)
	}); err != nil {
		return err
	}

	if err := migrateOnce(ctx, tx, 31, func() error { return execAll(ctx, tx, linearAdmissionV31) }); err != nil {
		return err
	}

	// Version 32 moves tasks.state to the eleven-state model; see
	// migrateStatesV32 for the steps, the locks and their cost.
	if err := migrateOnce(ctx, tx, stateMigrationVersion, func() error { return migrateStatesV32(ctx, tx) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 33, func() error { return execAll(ctx, tx, modelV33) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 34, func() error { return execAll(ctx, tx, eventsV34) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 35, func() error { return execAll(ctx, tx, decisionsV35) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 36, func() error { return execAll(ctx, tx, ledgerV36) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 37, func() error { return execAll(ctx, tx, intakeV37) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 38, func() error { return execAll(ctx, tx, decisionNotifyV38) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 39, func() error { return execAll(ctx, tx, jobUsageV39) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 40, func() error { return execAll(ctx, tx, benchOutcomesV40) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 41, func() error { return execAll(ctx, tx, originsV41) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 42, func() error { return execAll(ctx, tx, changesV42) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 43, func() error { return execAll(ctx, tx, decisionResolutionsV43) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 44, func() error { return execAll(ctx, tx, lastActivityV44) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 45, func() error { return execAll(ctx, tx, jobContextV45) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 46, func() error { return execAll(ctx, tx, decisionDenyV46) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 47, func() error { return execAll(ctx, tx, decisionSessionV47) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 48, func() error { return execAll(ctx, tx, decisionDeliveriesV48) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 49, func() error { return execAll(ctx, tx, verificationsV49) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 50, func() error { return execAll(ctx, tx, dotsResultsV50) }); err != nil {
		return err
	}
	if err := migrateOnce(ctx, tx, 51, func() error { return execAll(ctx, tx, changeStateV51) }); err != nil {
		return err
	}

	if err := migrateOnce(ctx, tx, 52, func() error { return execAll(ctx, tx, standingOrdersV52) }); err != nil {
		return err
	}

	return tx.Commit(ctx)
}
func validName(x string) bool        { return nameRE.MatchString(x) }
func validText(x string, n int) bool { return len(x) <= n && !strings.ContainsRune(x, 0) }

// taskReasonText normalizes a no-job reason before it is judged: a string
// made only of spaces and Unicode format characters (zero-width space,
// BOM, soft hyphen …) is as empty as "" — it must not satisfy the
// exception's "recorded reason" requirement while rendering blank.
func taskReasonText(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cc, r)
	})
}

// CheckTaskNoJob normalizes a no-job reason exactly as the transition and
// claim guards do and refuses a reason that can never be recorded:
// oversized or matching the secret filter. All-invisible input normalizes
// to ""; the caller decides whether empty means refuse or fall back.
// Routes that write before the guarded transition (decision resolve)
// must call this first so a refused reason cannot leave a partial write.
func CheckTaskNoJob(noJob string) (string, error) {
	noJob = taskReasonText(noJob)
	if !validText(noJob, MaxBytes) {
		return "", ErrTaskJobRequired
	}
	if err := guard.Reject(noJob); err != nil {
		return "", err
	}
	return noJob, nil
}

const benchBatchMax = 1000

var benchGradeValues = map[string]bool{"S+": true, "S": true, "A+": true, "A": true, "B": true, "C": true, "unrated": true}

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

// benchEffortRanks orders the effort ladder pacelever publishes
// (low<medium<high<xhigh<max). The profile-default row ("") and unknown effort
// strings are exempt from the monotonicity rule — effort is an open column on
// purpose. In ladder views "" leads the profile's rows and unknown efforts
// sort after the known rungs.
var benchEffortRanks = map[string]int{"low": 0, "medium": 1, "high": 2, "xhigh": 3, "max": 4}

// benchSolProfiles restricts measured Sol placements to S+. Unrated rows
// make no grade claim. Catalog writes also accept the legacy C/null seed
// shape during the transition to explicit unrated placeholders.
var benchSolProfiles = map[string]bool{"codex-sol": true, "kiro-sol": true}

// benchSolPlaceholder accepts an unscored unrated row or the legacy C/null
// placeholder. A scored row stays subject to the S+ rule.
func benchSolPlaceholder(x BenchCatalogEntry) bool {
	return (x.Grade == "unrated" || x.Grade == "C") && x.Score == nil
}

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
	case "C":
		return 5
	case "unrated":
		return 6
	default:
		return 7
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

var benchAreaExceptionActions = map[string]bool{"exclude": true, "prefer": true}

// benchAreaExceptionMaxRefs bounds one exception's evidence list — the rows
// are canon like catalog rows, not telemetry, so a handful of refs is the
// expected shape.
const benchAreaExceptionMaxRefs = 64

// Numeric refs are capped at 18 digits so every value fits an int64.
var benchAreaExceptionTaskRefRE = regexp.MustCompile(`^hk:task/[0-9]{1,18}$`)
var benchAreaExceptionSrvRefRE = regexp.MustCompile(`^srv:[0-9]{1,18}$`)

// validBenchAreaExceptionRef accepts exactly the three evidence ref shapes:
// hk:task/N, hk:doc/KEY and srv:N. A document key must pass the store's own
// document key validator and use printable ASCII only (0x21-0x7e): no
// whitespace, control or non-ASCII bytes. Anything else — other schemes,
// bare numbers — is not evidence.
func validBenchAreaExceptionRef(ref string) bool {
	switch {
	case benchAreaExceptionTaskRefRE.MatchString(ref):
		return true
	case benchAreaExceptionSrvRefRE.MatchString(ref):
		return true
	case strings.HasPrefix(ref, "hk:doc/"):
		key := strings.TrimPrefix(ref, "hk:doc/")
		for i := 0; i < len(key); i++ {
			if key[i] < 0x21 || key[i] > 0x7e {
				return false
			}
		}
		return validDocKey(key)
	}
	return false
}

// checkBenchAreaException validates one exception row against every rule the
// write path enforces without consulting the database: required fields, the
// closed action, the K1 area vocabulary, and the evidence ref shapes. The
// (profile, effort) catalog lookup happens inside the upsert transaction.
func checkBenchAreaException(x BenchAreaException) error {
	if !validBenchRequiredText(x.Profile) {
		return errors.New("invalid bench area exception profile")
	}
	if !validText(x.Effort, 200) {
		return errors.New("invalid bench area exception effort")
	}
	if err := CheckTaskArea(&x.Area); err != nil {
		return err
	}
	if !benchAreaExceptionActions[x.Action] {
		return errors.New("invalid bench area exception action")
	}
	if len(x.EvidenceRefs) < 1 || len(x.EvidenceRefs) > benchAreaExceptionMaxRefs {
		return errors.New("invalid bench area exception evidence_refs")
	}
	for _, ref := range x.EvidenceRefs {
		if !validBenchAreaExceptionRef(ref) {
			return errors.New("invalid bench area exception evidence_refs")
		}
	}
	if strings.TrimSpace(x.DecisionRef) == "" {
		return ErrDecisionRefRequired
	}
	if !validBenchText(x.DecisionRef) {
		return errors.New("invalid bench area exception decision_ref")
	}
	if !validBenchRequiredText(x.ExpiresWithModelID) {
		return errors.New("invalid bench area exception expires_with_model_id")
	}
	if !validBenchClient(x.DecidedBy) {
		return ErrDecidedByRequired
	}
	return nil
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

// BenchRepConflict names one input row whose (created_by, origin_id) slot is
// already held by a different rep: its position in the batch, the client's
// key, and the server id of the row that holds it.
type BenchRepConflict struct {
	Index    int   `json:"index"`
	OriginID int64 `json:"origin_id"`
	ServerID int64 `json:"conflict_server_id"`
}

// BenchRepConflictError rejects a whole reps batch: at least one input row
// shares its (created_by, origin_id) key with a stored row whose content
// differs. Reps content is immutable through this endpoint — several hosts can
// authenticate under one client identity and their per-host origin ids
// collide by construction, so an update-in-place here silently erases another
// host's rep.
type BenchRepConflictError struct {
	Conflicts []BenchRepConflict
}

func (e *BenchRepConflictError) Error() string {
	return fmt.Sprintf("bench_rep_conflict: %d row(s) share a (created_by, origin_id) key with different content", len(e.Conflicts))
}

// benchRepKey is the client-side row identity inside one batch.
type benchRepKey struct {
	createdBy string
	originID  int64
}

// benchRepsBatchErr translates Postgres errors that abort the batch
// transaction without writing anything into ErrBenchRepsRetryable so the
// API can answer 503 instead of a 400 that wrongly blames the rows.
func benchRepsBatchErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001") {
		return fmt.Errorf("%w: %w", ErrBenchRepsRetryable, err)
	}
	return err
}

func benchRepPtrEq[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// sameBenchRepInput reports whether two input rows carry identical rep
// content, so a repeated (created_by, origin_id) key inside one batch is the
// same resend the ON CONFLICT gate already accepts. recorded_at compares
// after microsecond truncation — that is the precision timestamptz keeps, so
// two spellings of the same stored instant still count as identical. id and
// created_at are server-stamped and are not inputs.
func sameBenchRepInput(a, b BenchRep) bool {
	return a.Profile == b.Profile &&
		a.RecordedAt.Truncate(time.Microsecond).Equal(b.RecordedAt.Truncate(time.Microsecond)) &&
		benchRepPtrEq(a.ModelID, b.ModelID) &&
		benchRepPtrEq(a.TaskRef, b.TaskRef) &&
		benchRepPtrEq(a.Tier, b.Tier) &&
		benchRepPtrEq(a.Role, b.Role) &&
		benchRepPtrEq(a.Rounds, b.Rounds) &&
		benchRepPtrEq(a.BlockersFound, b.BlockersFound) &&
		benchRepPtrEq(a.Completed, b.Completed) &&
		benchRepPtrEq(a.InputTokens, b.InputTokens) &&
		benchRepPtrEq(a.OutputTokens, b.OutputTokens) &&
		benchRepPtrEq(a.Notes, b.Notes) &&
		benchRepPtrEq(a.Effort, b.Effort) &&
		benchRepPtrEq(a.Grade, b.Grade) &&
		benchRepPtrEq(a.TableGrade, b.TableGrade) &&
		a.BenchOutcome.same(b.BenchOutcome)
}

// upsertBenchRepInsert is one race-free statement: insert, or — when the
// (created_by, origin_id) key is taken — a no-op update gated on every
// carried field being identical (IS NOT DISTINCT FROM is NULL-safe;
// recorded_at is TIMESTAMPTZ so the comparison is by instant). The DO UPDATE
// lock means a transaction racing on the same key waits for the winner and
// then re-evaluates the WHERE against the committed row, so content
// comparison cannot observe a torn or pre-commit state. A returned row is a
// fresh insert or an identical resend; no row means a different rep holds
// the key.
var upsertBenchRepInsert = func() string {
	cols := append([]string{"origin_id", "profile", "model_id", "task_ref", "tier", "role", "rounds", "blockers_found", "completed", "input_tokens", "output_tokens", "notes", "recorded_at", "effort", "grade", "table_grade", "created_by", "created_at"}, benchOutcomeColumns...)
	params := make([]string, len(cols))
	var same []string
	for i, c := range cols {
		params[i] = fmt.Sprintf("$%d", i+1)
		if c != "origin_id" && c != "created_by" && c != "created_at" {
			same = append(same, "bench_reps."+c+" IS NOT DISTINCT FROM EXCLUDED."+c)
		}
	}
	return `INSERT INTO bench_reps(` + strings.Join(cols, ",") + `) VALUES(` + strings.Join(params, ",") + `) ON CONFLICT (created_by,origin_id) DO UPDATE SET origin_id=EXCLUDED.origin_id WHERE ` + strings.Join(same, " AND ") + ` RETURNING id`
}()

// UpsertBenchReps inserts one row per input rep and returns the server ids in
// input order. A rep whose (created_by, origin_id) key is already taken is an
// idempotent resend only when its content is field-for-field identical to the
// stored row; any difference rejects the complete batch with
// *BenchRepConflictError. Existing rows are never updated — DO UPDATE
// carried under a colliding key is what destroyed reps across shared-token
// hosts, and the WHERE gate above keeps the id without changing any column.
// (Every attempted insert still consumes one BIGSERIAL candidate, conflict or
// not, so rejected collisions remain visible as id gaps.)
func (s *Store) UpsertBenchReps(ctx context.Context, xs []BenchRep) ([]int64, error) {
	if len(xs) < 1 || len(xs) > benchBatchMax {
		return nil, errors.New("invalid bench reps")
	}
	seen := make(map[benchRepKey]int, len(xs))
	for i, x := range xs {
		if !validBenchRep(x) {
			return nil, errors.New("invalid bench rep")
		}
		if err := x.BenchOutcome.valid(); err != nil {
			return nil, err
		}
		for _, value := range []*string{x.TaskRef, x.Notes} {
			if value != nil {
				if err := guard.Reject(*value); err != nil {
					return nil, err
				}
			}
		}
		// A key repeated inside one batch is an idempotent resend only when
		// the rows are identical. Differing repeats are rejected up front:
		// letting the second row hit ON CONFLICT against the first row's
		// uncommitted insert would name a conflict_server_id that rolls back
		// and never identifies a stored row.
		k := benchRepKey{createdBy: x.CreatedBy, originID: x.OriginID}
		if j, ok := seen[k]; ok {
			if !sameBenchRepInput(xs[j], x) {
				return nil, fmt.Errorf("invalid bench reps: rows %d and %d share created_by=%q origin_id=%d with different content", j, i, x.CreatedBy, x.OriginID)
			}
			continue
		}
		seen[k] = i
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	ids := make([]int64, 0, len(xs))
	var conflicts []BenchRepConflict
	for i, x := range xs {
		var id int64
		args := append([]any{x.OriginID, x.Profile, x.ModelID, x.TaskRef, x.Tier, x.Role, x.Rounds, x.BlockersFound, x.Completed, x.InputTokens, x.OutputTokens, x.Notes, x.RecordedAt, x.Effort, x.Grade, x.TableGrade, x.CreatedBy, now}, x.BenchOutcome.values()...)
		err = tx.QueryRow(ctx, upsertBenchRepInsert, args...).Scan(&id)
		if err == nil {
			ids = append(ids, id)
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, benchRepsBatchErr(err)
		}
		var existing int64
		if err = tx.QueryRow(ctx, `SELECT id FROM bench_reps WHERE created_by=$1 AND origin_id=$2`, x.CreatedBy, x.OriginID).Scan(&existing); err != nil {
			return nil, benchRepsBatchErr(err)
		}
		conflicts = append(conflicts, BenchRepConflict{Index: i, OriginID: x.OriginID, ServerID: existing})
	}
	if len(conflicts) > 0 {
		return nil, &BenchRepConflictError{Conflicts: conflicts}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, benchRepsBatchErr(err)
	}
	return ids, nil
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
	rows, err := s.pool.Query(ctx, `SELECT `+benchRepColumns+` FROM bench_reps WHERE ($1='' OR profile=$1) AND ($2='' OR grade=$2) AND ($3='' OR effort=$3) ORDER BY id DESC LIMIT $4`, profile, grade, effort, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchRep{}
	for rows.Next() {
		x, err := scanBenchRep(rows)
		if err != nil {
			return nil, err
		}
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
	rows, err := s.pool.Query(ctx, `SELECT `+benchRepColumns+` FROM bench_reps WHERE task_ref=$1 ORDER BY id ASC LIMIT $2`, taskRef, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchRep{}
	for rows.Next() {
		x, err := scanBenchRep(rows)
		if err != nil {
			return nil, err
		}
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
		if benchSolProfiles[x.Profile] && x.Grade != "S+" && x.Grade != "unrated" {
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
		// Refuse a scored Sol transition atomically, including when a catalog
		// writer created the default row after this transaction began.
		tag, err := tx.Exec(ctx, `INSERT INTO bench_catalog(profile,effort,model_id,pool,grade,boundary_version,deviation_ref,decided_at,decided_by) VALUES($1,'','','',$2,$3,$4,$5,$6) ON CONFLICT (profile,effort) DO UPDATE SET grade=EXCLUDED.grade,boundary_version=EXCLUDED.boundary_version,deviation_ref=EXCLUDED.deviation_ref,decided_at=EXCLUDED.decided_at,decided_by=EXCLUDED.decided_by,retired_at=NULL WHERE NOT ($7 AND EXCLUDED.grade='unrated' AND bench_catalog.score IS NOT NULL)`, x.Profile, x.Grade, x.BoundaryVersion, x.DeviationRef, x.DecidedAt, x.DecidedBy, benchSolProfiles[x.Profile])
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			return 0, ErrBenchCatalogSolGrade
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
		if benchSolProfiles[x.Profile] && x.Grade != "S+" && !benchSolPlaceholder(x) {
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
// effort strings and unrated rows do not participate.
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
		if _, ok := benchEffortRanks[x.Effort]; !ok || x.Grade == "unrated" {
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
	rows, err := s.pool.Query(ctx, `SELECT profile,effort,model_id,pool,grade,score,gate,gate_reason,benchmark_source,benchmark_annotation,boundary_version,deviation_ref,decided_at,decided_by,retired_at FROM bench_catalog WHERE ($1='' OR pool=$1) AND ($1='' OR gate<>'consult_only') AND ($2 OR retired_at IS NULL) ORDER BY pool, CASE grade WHEN 'S+' THEN 0 WHEN 'S' THEN 1 WHEN 'A+' THEN 2 WHEN 'A' THEN 3 WHEN 'B' THEN 4 WHEN 'C' THEN 5 ELSE 6 END, profile, CASE effort WHEN '' THEN 0 WHEN 'low' THEN 1 WHEN 'medium' THEN 2 WHEN 'high' THEN 3 WHEN 'xhigh' THEN 4 WHEN 'max' THEN 5 ELSE 6 END, effort`, pool, includeRetired)
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

// upsertBenchAreaException is one race-free statement. The DO UPDATE lock
// serializes writers on the key: a racer waits for the winner and then
// re-evaluates the WHERE against the committed row. The WHERE admits the
// write unless both sides would be live with different actions — an
// unconditional update for a retired row or a retirement, a same-action
// upsert otherwise. Nothing is written when prefer and exclude would both
// be live; the empty RETURNING surfaces that as ErrBenchAreaExceptionConflict.
const upsertBenchAreaException = `INSERT INTO bench_area_exceptions(profile,effort,area,action,evidence_refs,decision_ref,decided_by,decided_at,expires_with_model_id,retired_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (profile,effort,area) DO UPDATE SET action=EXCLUDED.action,evidence_refs=EXCLUDED.evidence_refs,decision_ref=EXCLUDED.decision_ref,decided_by=EXCLUDED.decided_by,decided_at=EXCLUDED.decided_at,expires_with_model_id=EXCLUDED.expires_with_model_id,retired_at=EXCLUDED.retired_at WHERE bench_area_exceptions.retired_at IS NOT NULL OR EXCLUDED.retired_at IS NOT NULL OR bench_area_exceptions.action = EXCLUDED.action RETURNING profile`

// UpsertBenchAreaExceptions is the operator write path for the area
// exception list, called only after the API has stamped DecidedBy and
// DecidedAt. The batch is atomic: every row validates first, each
// (profile, effort) must name a bench_catalog row — live or retired — and
// prefer and exclude may never both be live for one key, whether the
// conflict comes from the stored row or from another row in this batch.
func (s *Store) UpsertBenchAreaExceptions(ctx context.Context, xs []BenchAreaException) (int, error) {
	if len(xs) < 1 || len(xs) > benchBatchMax {
		return 0, errors.New("invalid bench area exceptions")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	for _, x := range xs {
		if err := checkBenchAreaException(x); err != nil {
			return 0, err
		}
		if err := guard.Reject(x.DecisionRef); err != nil {
			return 0, err
		}
		if err := guard.Reject(x.ExpiresWithModelID); err != nil {
			return 0, err
		}
		for _, ref := range x.EvidenceRefs {
			if err := guard.Reject(ref); err != nil {
				return 0, err
			}
		}
		if x.DecidedAt.IsZero() {
			x.DecidedAt = now
		}
		var known bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bench_catalog WHERE profile=$1 AND effort=$2)`, x.Profile, x.Effort).Scan(&known); err != nil {
			return 0, err
		}
		if !known {
			return 0, fmt.Errorf("%w: %s effort %q", ErrBenchAreaExceptionProfileUnknown, x.Profile, x.Effort)
		}
		var key string
		err = tx.QueryRow(ctx, upsertBenchAreaException, x.Profile, x.Effort, x.Area, x.Action, x.EvidenceRefs, x.DecisionRef, x.DecidedBy, x.DecidedAt, x.ExpiresWithModelID, x.RetiredAt).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("%w: %s effort %q area %s", ErrBenchAreaExceptionConflict, x.Profile, x.Effort, x.Area)
		}
		if err != nil {
			return 0, err
		}
	}
	return len(xs), tx.Commit(ctx)
}

// ListBenchAreaExceptions returns exception rows ordered by area, profile
// and effort rung. Retired rows are hidden unless includeRetired is set; the
// area filter is exact against the TaskAreas vocabulary.
func (s *Store) ListBenchAreaExceptions(ctx context.Context, area string, includeRetired bool) ([]BenchAreaException, error) {
	if area != "" {
		if err := CheckTaskArea(&area); err != nil {
			return nil, err
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT profile,effort,area,action,evidence_refs,decision_ref,decided_by,decided_at,expires_with_model_id,retired_at FROM bench_area_exceptions WHERE ($1='' OR area=$1) AND ($2 OR retired_at IS NULL) ORDER BY area, profile, CASE effort WHEN '' THEN 0 WHEN 'low' THEN 1 WHEN 'medium' THEN 2 WHEN 'high' THEN 3 WHEN 'xhigh' THEN 4 WHEN 'max' THEN 5 ELSE 6 END, effort`, area, includeRetired)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchAreaException{}
	for rows.Next() {
		var x BenchAreaException
		if err := rows.Scan(&x.Profile, &x.Effort, &x.Area, &x.Action, &x.EvidenceRefs, &x.DecisionRef, &x.DecidedBy, &x.DecidedAt, &x.ExpiresWithModelID, &x.RetiredAt); err != nil {
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
	if x.RevertPR != "" && !taskEventPRRE.MatchString(x.RevertPR) {
		return false
	}
	if x.Disposition != nil && !validDisposition(x) {
		return false
	}
	return true
}

func validTaskLinear(x TaskLinear) bool {
	if !validLinearMirrorReason(x.MirrorReason) {
		return false
	}
	if !validText(x.IssueID, 256) || !validText(x.Identifier, 256) {
		return false
	}
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
		values = append(values, x.Linear.IssueID, x.Linear.Identifier, x.Linear.Tier, x.Linear.Grade, x.Linear.Brief, x.Linear.Report, x.Linear.Verify, x.Linear.Decision, x.Linear.DeploySHA)
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
		if patch.Linear.MirrorReason != "" {
			merged.MirrorReason = patch.Linear.MirrorReason
		}
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

func scanTask(row interface{ Scan(...any) error }, x *Task, extra ...any) error {
	var refs []byte
	dest := []any{&x.ID, &x.Lane, &x.ParentLane, &x.Title, &x.Kind, &x.State, &x.Priority, &refs, &x.ClaimedBy, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt, &x.LastActivityAt, &x.BodyDoc, &x.Project, &x.Area, &x.Expected, &x.AwaitingDecision, &x.ParentID, &x.StartedAt, &x.CompletedAt, &x.CanceledAt, &x.TriagedAt, &x.StartedTriageAt, &x.MilestoneID, &x.DueAt, &x.SLAStartedAt, &x.SLAMediumRiskAt, &x.SLAHighRiskAt, &x.SLABreachesAt, &x.Source, &x.SourceRef, &x.Labels}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return err
	}
	x.Category = TaskCategory(x.State)
	x.SLARisk = slaRisk(time.Now().UTC(), x.SLAStartedAt, x.SLAMediumRiskAt, x.SLAHighRiskAt, x.SLABreachesAt)
	return json.Unmarshal(refs, &x.Refs)
}

const taskBaseColumns = `id,lane,parent_lane,title,kind,state,priority,refs,claimed_by,created_by,created_at,updated_at,last_activity_at,body_doc,project,area,expected,awaiting_decision,parent_id,started_at,completed_at,canceled_at,triaged_at,started_triage_at,milestone_id,due_at,sla_started_at,sla_medium_risk_at,sla_high_risk_at,sla_breaches_at,source,source_ref`
const taskColumns = taskBaseColumns + `,ARRAY(SELECT label_id FROM task_labels WHERE task_id=tasks.id ORDER BY label_id)`

// taskProjectQuerier is the row-query seam requireTaskProject runs against:
// the caller's transaction, so membership is judged in the same snapshot the
// insert commits under.
type taskProjectQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// requireTaskProject is the single create-time gate every new task row
// passes through — CreateTask and CreateDisposition alike. A nil or empty
// project is refused as required, a name outside the task_projects
// vocabulary (or outside the lane-alphabet shape) as unknown.
func requireTaskProject(ctx context.Context, q taskProjectQuerier, project *string) error {
	if project == nil || *project == "" {
		return fmt.Errorf("%w: new tasks require --project <name> (the allowed set is listed by tasks projects)", ErrTaskProjectRequired)
	}
	if !validName(*project) {
		return fmt.Errorf("%w: project names match the lane alphabet [A-Za-z0-9._-]{1,128}", ErrTaskProjectUnknown)
	}
	var known bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_projects WHERE name=$1)`, *project).Scan(&known); err != nil {
		return err
	}
	if !known {
		return fmt.Errorf("%w: %q is not in the server's project set (the allowed set is listed by tasks projects)", ErrTaskProjectUnknown, *project)
	}
	return nil
}

// normalizeExpected accepts a Go duration or whole days in the range 1m..90d.
func normalizeExpected(value string) (string, bool) {
	var d time.Duration
	if strings.HasSuffix(value, "d") {
		day := strings.TrimSuffix(value, "d")
		if day == "" {
			return "", false
		}
		for _, c := range day {
			if c < '0' || c > '9' {
				return "", false
			}
		}
		n, err := strconv.ParseUint(day, 10, 32)
		if err != nil || n > 90 {
			return "", false
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(value)
		if err != nil {
			return "", false
		}
	}
	if d < time.Minute || d > 90*24*time.Hour {
		return "", false
	}
	return d.String(), true
}

func ValidExpected(value string) bool { _, ok := normalizeExpected(value); return ok }

// CreateTask files a task in backlog without the duplicate check; tasks add
// and POST /v1/tasks go through CreateTaskChecked.
func (s *Store) CreateTask(ctx context.Context, x Task) (Task, error) {
	return s.createTask(ctx, x, nil)
}

// DuplicateCheck is what a caller of CreateTaskChecked answers to the
// duplicate check: Link relates the new task to an existing one as related,
// New files it without looking. The zero value runs the check.
type DuplicateCheck struct {
	Link int64
	New  bool
}

// CreateTaskChecked is CreateTask behind the duplicate check: when a started,
// unstarted or backlog task has a similar title, the create is refused with a
// *SimilarTasksError listing them, unless check carries Link or New.
func (s *Store) CreateTaskChecked(ctx context.Context, x Task, check DuplicateCheck) (Task, error) {
	if check.Link < 0 || (check.Link != 0 && check.New) {
		return x, errors.New("invalid task: link and new are exclusive")
	}
	return s.createTask(ctx, x, &check)
}

func (s *Store) createTask(ctx context.Context, x Task, check *DuplicateCheck) (Task, error) {
	if x.Expected != nil {
		normalized, ok := normalizeExpected(*x.Expected)
		if !ok {
			return x, errors.New("invalid task: expected must be between 1m and 90d")
		}
		x.Expected = &normalized
	}
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
	if x.Refs.DecisionRequest != nil {
		return x, errors.New("invalid task: decision requests use tasks decision-request")
	}
	if x.Source != "" || x.SourceRef != "" {
		return x, errors.New("invalid task: source and source_ref are set by intake and ticket sets")
	}
	if err := guard.Reject(x.Title); err != nil {
		return x, err
	}
	if err := CheckTaskArea(x.Area); err != nil {
		return x, err
	}
	if err := rejectTaskRefs(x.Refs); err != nil {
		return x, err
	}
	refs, err := json.Marshal(x.Refs)
	if err != nil {
		return x, errors.New("invalid task refs")
	}
	x.State, x.ClaimedBy = StateBacklog, ""
	x.CreatedAt = time.Now().UTC()
	x.UpdatedAt = x.CreatedAt
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return x, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return x, err
	}

	if x.Refs.JobID != "" && wrkTablesExist(ctx, tx) {
		if _, err = tx.Exec(ctx, `INSERT INTO wrk_job_locks(job_id) VALUES($1) ON CONFLICT DO NOTHING`, x.Refs.JobID); err != nil {
			return x, err
		}
		var locked string
		if err = tx.QueryRow(ctx, `SELECT job_id FROM wrk_job_locks WHERE job_id=$1 FOR UPDATE`, x.Refs.JobID).Scan(&locked); err != nil {
			return x, err
		}
		if err = s.wrkFence(ctx, tx, wrkrecords.LegacyIntent{JobID: x.Refs.JobID, Binding: true}); err != nil {
			return x, err
		}
	}
	if err = requireTaskProject(ctx, tx, x.Project); err != nil {
		return x, err
	}
	if x.ParentID != nil {
		if err = checkParentTx(ctx, tx, 0, *x.ParentID); err != nil {
			return x, err
		}
	}
	if check != nil && check.Link != 0 {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, check.Link).Scan(&exists); err != nil {
			return x, err
		}
		if !exists {
			return x, fmt.Errorf("invalid task: link %d does not exist", check.Link)
		}
	}
	if check != nil && check.Link == 0 && !check.New {
		similar, err := similarOpenTasks(ctx, tx, []string{x.Title})
		if err != nil {
			return x, err
		}
		if len(similar) > 0 {
			return x, &SimilarTasksError{Tasks: similar}
		}
	}
	parentID := x.ParentID
	if err = scanTask(tx.QueryRow(ctx, `INSERT INTO tasks(lane,parent_lane,title,kind,state,priority,refs,claimed_by,created_by,created_at,updated_at,last_activity_at,body_doc,project,area,expected,parent_id) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11,$10,$12,$13,$14,$15,$16) RETURNING `+taskColumns, x.Lane, x.ParentLane, x.Title, x.Kind, x.State, x.Priority, string(refs), x.ClaimedBy, x.CreatedBy, x.CreatedAt, x.UpdatedAt, x.BodyDoc, x.Project, x.Area, x.Expected, parentID), &x); err != nil {
		return x, err
	}
	if check != nil && check.Link != 0 {
		if _, err = addRelationTx(ctx, tx, x.ID, RelationRelated, check.Link, x.CreatedBy, x.CreatedAt); err != nil {
			return x, err
		}
	}
	if s.LinearSyncEnabled() && x.Refs.Linear != nil && x.Refs.Linear.Sync {
		if err = enqueueLinearTaskCreate(ctx, tx, x); err != nil {
			return x, err
		}
	}
	return x, tx.Commit(ctx)
}

func (s *Store) ClaimTask(ctx context.Context, id int64, by, jobID, noJob string) (Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return Task{}, err
	}

	x, err := s.claimTaskTx(ctx, tx, id, by, jobID, noJob)
	if err != nil {
		return Task{}, err
	}
	return x, tx.Commit(ctx)
}

// RebindTaskJob atomically moves an active task's job link to a new job. The
// current job and task state are checked under the row lock; a completed replay
// of the same rebind is a no-op when its audit event is still the latest rebind.
func (s *Store) RebindTaskJob(ctx context.Context, id int64, fromJob, toJob, by, reason string) (Task, error) {
	reason = taskReasonText(reason)
	if !validText(fromJob, 4096) || fromJob == "" || !validText(toJob, 4096) || toJob == "" || fromJob == toJob {
		return Task{}, errors.New("invalid task rebind job ids")
	}
	if !validText(by, 128) || by == "" || !validText(reason, MaxBytes) || reason == "" {
		return Task{}, errors.New("invalid task rebind claimant or reason")
	}
	for _, value := range []string{fromJob, toJob, reason} {
		if err := guard.Reject(value); err != nil {
			return Task{}, err
		}
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
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, ErrTaskNotFound
		}
		return Task{}, err
	}

	if err := s.wrkTaskFenceTx(ctx, tx, x, "", true); err != nil {
		return Task{}, err
	}
	if err := s.wrkFence(ctx, tx, wrkrecords.LegacyIntent{JobID: toJob, Binding: true}); err != nil {
		return Task{}, err
	}

	if (x.State == StateResearching || x.State == StateDeveloping) && x.Refs.JobID == toJob && x.ClaimedBy == by {
		var replay bool
		err = tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM task_events e
			WHERE e.task_id=$1 AND e.refs->'job_rebind'->>'from_job'=$2
			  AND e.refs->'job_rebind'->>'to_job'=$3 AND e.refs->'job_rebind'->>'by'=$4
			  AND e.id=(SELECT MAX(e2.id) FROM task_events e2 WHERE e2.task_id=$1 AND e2.refs ? 'job_rebind')
		)`, id, fromJob, toJob, by).Scan(&replay)
		if err != nil {
			return Task{}, err
		}
		if replay {
			return x, tx.Commit(ctx)
		}
	}
	if (x.State != StateResearching && x.State != StateDeveloping) || x.Refs.JobID != fromJob {
		current := x.Refs.JobID
		if current == "" {
			current = "(none)"
		}
		return Task{}, fmt.Errorf("%w: cannot rebind from job %q to %q; current job_id is %q (state=%s)", ErrTaskConflict, fromJob, toJob, current, x.State)
	}

	now := time.Now().UTC()
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET refs=refs||jsonb_build_object('job_id',$2::text),claimed_by=$3,updated_at=$4 WHERE id=$1 RETURNING `+taskColumns, id, toJob, by, now), &x); err != nil {
		return Task{}, err
	}
	rebind := &TaskJobRebind{FromJob: fromJob, ToJob: toJob, By: by, Reason: reason}
	if _, moved, err := insertEvent(ctx, tx, taskEventInput{TaskID: id, From: x.State, To: x.State, By: by, Note: reason, Refs: x.Refs, Rebind: rebind, At: now}); err != nil {
		return Task{}, err
	} else if moved {
		x.LastActivityAt = now.Truncate(time.Microsecond)
	}
	if err := s.recordLinearBoardTaskTx(ctx, tx, x, reason); err != nil {
		return Task{}, err
	}
	return x, tx.Commit(ctx)
}

// NextTask claims the oldest highest-priority runnable task in one
// transaction. Like ClaimTask, the claim must carry jobID or a recorded
// noJob reason; a bare pull is refused with ErrTaskJobRequired.
func (s *Store) NextTask(ctx context.Context, lane, by, jobID, noJob string) (Task, error) {
	if !validName(lane) {
		return Task{}, errors.New("invalid task lane")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return Task{}, err
	}
	// Naming an enrolled job is an explicit protected claim attempt, even
	// when selection would otherwise exclude every protected task (S1 #14).
	if err = s.wrkFence(ctx, tx, wrkrecords.LegacyIntent{JobID: jobID, Binding: true}); err != nil {
		return Task{}, err
	}

	var id int64
	selection := `SELECT id FROM tasks WHERE lane=$1 AND state IN ('backlog','unstarted') AND NOT awaiting_decision AND NOT (refs ? 'disposition')`
	if wrkTablesExist(ctx, tx) {
		// Same enrollment predicate as Snapshot.ProtectedTask: only the latest
		// subject claim at activation, including ended predecessor enrollment.
		selection += ` AND NOT EXISTS(SELECT 1 FROM wrk_records wr WHERE wr.kind='claim' AND wr.payload->>'role' IN ('builder','worker') AND wr.payload->>'task_id'=tasks.id::text AND NOT EXISTS(SELECT 1 FROM wrk_records newer WHERE newer.job_id=wr.job_id AND newer.kind='claim' AND newer.seq>wr.seq) AND EXISTS(SELECT 1 FROM wrk_records a WHERE a.job_id=wr.job_id AND a.kind='activate'))`
	}
	selection += ` ORDER BY priority DESC,created_at ASC,id ASC FOR UPDATE SKIP LOCKED LIMIT 1`
	err = tx.QueryRow(ctx, selection, lane).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, ErrQueueEmpty
	}
	if err != nil {
		return Task{}, err
	}
	x, err := s.claimTaskTx(ctx, tx, id, by, jobID, noJob)
	if err != nil {
		return Task{}, err
	}
	return x, tx.Commit(ctx)
}

// knownTaskLanesTx reports whether name is a lane some current row uses. hk
// has no lane table, so the known set is the union of every column that
// carries a live lane name.
func knownTaskLanesTx(ctx context.Context, tx pgx.Tx, name string) (bool, error) {
	var known bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE lane=$1)
		OR EXISTS(SELECT 1 FROM tasks WHERE parent_lane=$1)
		OR EXISTS(SELECT 1 FROM relay_events WHERE owner_lane=$1)
		OR EXISTS(SELECT 1 FROM chat_questions WHERE lane=$1)`, name).Scan(&known)
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
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return Task{}, false, err
	}

	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, false, ErrTaskNotFound
		}
		return Task{}, false, err
	}

	if err := s.wrkTaskFenceTx(ctx, tx, x, "", true); err != nil {
		return Task{}, false, err
	}
	if IsSettledState(x.State) {
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
	x.UpdatedAt = time.Now().UTC()
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET lane=$2,updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, id, to, x.UpdatedAt), &x); err != nil {
		return Task{}, false, err
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: id, Kind: "relane", From: from, To: to, By: by, Note: note, Refs: x.Refs, Into: &x, At: x.UpdatedAt, Field: "lane", FromValue: from, ToValue: to}); err != nil {
		return Task{}, false, err
	}
	if err := s.recordLinearBoardTaskTx(ctx, tx, x, note); err != nil {
		return Task{}, false, err
	}
	return x, true, tx.Commit(ctx)
}

// SetTaskProject reclassifies one task within the server-configured project
// vocabulary. It follows RelaneTask's shape — row lock, append-only event —
// but writes kind='project' events whose from/to carry project names, never
// states. Unlike a relane it is allowed on terminal rows: project is
// classification metadata, so legacy history can still be bucketed without
// falsifying lane-level reports. A no-op change records no event.
func (s *Store) SetTaskProject(ctx context.Context, id int64, project, by, note string) (x Task, changed bool, err error) {
	return s.SetTaskProjectWithMetadata(ctx, id, project, by, note, nil)
}

func (s *Store) SetTaskProjectWithMetadata(ctx context.Context, id int64, project, by, note string, patch *ProjectMetadataPatch) (x Task, changed bool, err error) {
	if !validName(project) || by == "" || !validText(by, 128) || !validText(note, MaxBytes) {
		return Task{}, false, errors.New("invalid task project change")
	}
	if err := guard.Reject(note); err != nil {
		return Task{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return Task{}, false, err
	}

	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, false, ErrTaskNotFound
		}
		return Task{}, false, err
	}

	if err := s.wrkTaskFenceTx(ctx, tx, x, "", true); err != nil {
		return Task{}, false, err
	}
	var known bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_projects WHERE name=$1)`, project).Scan(&known); err != nil {
		return Task{}, false, err
	}
	if !known {
		return Task{}, false, fmt.Errorf("%w: %q is not in the server's project set (the allowed set is listed by tasks projects)", ErrTaskProjectUnknown, project)
	}
	from := ""
	if x.Project != nil {
		from = *x.Project
	}
	if from != project && x.MilestoneID != nil {
		return Task{}, false, ErrMilestoneProject
	}
	if patch != nil {
		if _, err = patchProjectMetadataTx(ctx, tx, project, *patch); err != nil {
			return Task{}, false, err
		}
	}
	if from == project {
		return x, false, tx.Commit(ctx)
	}
	x.UpdatedAt = time.Now().UTC()
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET project=$2,updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, id, project, x.UpdatedAt), &x); err != nil {
		return Task{}, false, err
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: id, Kind: "project", From: from, To: project, By: by, Note: note, Refs: x.Refs, Into: &x, At: x.UpdatedAt, Field: "project", FromValue: from, ToValue: project}); err != nil {
		return Task{}, false, err
	}
	if err := s.recordLinearBoardTaskTx(ctx, tx, x, note); err != nil {
		return Task{}, false, err
	}
	return x, true, tx.Commit(ctx)
}

// SetTaskArea tags one task from the TaskAreas vocabulary; a nil area clears
// the tag. It follows SetTaskProject's shape: row lock, append-only
// kind='area' event whose from/to carry area names ("" for unset), and no
// event when the value is unchanged. Like project it is allowed on terminal
// rows: area is routing metadata, not lifecycle.
func (s *Store) SetTaskArea(ctx context.Context, id int64, area *string, by, note string) (x Task, changed bool, err error) {
	if err := CheckTaskArea(area); err != nil {
		return Task{}, false, err
	}
	if by == "" || !validText(by, 128) || !validText(note, MaxBytes) {
		return Task{}, false, errors.New("invalid task area change")
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
	from, to := "", ""
	if x.Area != nil {
		from = *x.Area
	}
	if area != nil {
		to = *area
	}
	if from == to {
		return x, false, tx.Commit(ctx)
	}
	x.UpdatedAt = time.Now().UTC()
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET area=$2,updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, id, area, x.UpdatedAt), &x); err != nil {
		return Task{}, false, err
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: id, Kind: "area", From: from, To: to, By: by, Note: note, Refs: x.Refs, Into: &x, At: x.UpdatedAt, Field: "area", FromValue: from, ToValue: to}); err != nil {
		return Task{}, false, err
	}
	if err := s.recordLinearBoardTaskTx(ctx, tx, x, note); err != nil {
		return Task{}, false, err
	}
	return x, true, tx.Commit(ctx)
}

// ListTaskProjects returns the server-configured project vocabulary in
// name order. This is the set tasks add --project accepts.
func (s *Store) ListTaskProjects(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM task_projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// AddTaskProject extends the server-configured project vocabulary. Names
// share the lane alphabet; created=false when the name already exists.
func (s *Store) AddTaskProject(ctx context.Context, name, by string) (bool, error) {
	if !validName(name) || by == "" || !validText(by, 128) {
		return false, errors.New("invalid task project")
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO task_projects(name,created_by,created_at) VALUES($1,$2,now()) ON CONFLICT (name) DO NOTHING`, name, by)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) ListTasks(ctx context.Context, lane, state, parentLane string, project, area *string, limit int) ([]Task, error) {
	return s.ListTasksFiltered(ctx, TaskQuery{Lane: lane, State: state, ParentLane: parentLane, Project: project, Area: area, Limit: limit})
}

// ListTasksPage returns a stable ID-ordered page for callers that must visit
// the complete task set. afterID is exclusive; ListTasks retains its queue
// priority ordering for existing callers.
func (s *Store) ListTasksPage(ctx context.Context, lane, state, parentLane string, project, area *string, afterID int64, limit int) ([]Task, error) {
	return s.ListTasksFiltered(ctx, TaskQuery{Lane: lane, State: state, ParentLane: parentLane, Project: project, Area: area, AfterID: &afterID, Limit: limit})
}

// TaskQuery is a task list filter. A nil AfterID keeps the queue's priority
// order; a set one pages by id. Stalled keeps only stalled tasks.
type TaskQuery struct {
	Lane, State, ParentLane string
	Project, Area           *string
	Stalled                 bool
	AfterID                 *int64
	// ChangeID keeps the tasks linked to that change, ordered by id.
	ChangeID *int64
	Limit    int
}

// ListTasksFiltered lists tasks matching q and marks stalled rows.
func (s *Store) ListTasksFiltered(ctx context.Context, tq TaskQuery) ([]Task, error) {
	if !validTaskQuery(tq.Lane, tq.State, tq.ParentLane, tq.Project) || (tq.AfterID != nil && *tq.AfterID < 0) {
		return nil, errors.New("invalid task query")
	}
	if err := checkTaskAreaFilter(tq.Area); err != nil {
		return nil, err
	}
	limit := tq.Limit
	if limit < 1 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	q, args := `SELECT `+taskColumns+` FROM tasks WHERE 1=1`, []any{}
	if tq.AfterID != nil {
		args = append(args, *tq.AfterID)
		q += fmt.Sprintf(" AND id>$%d", len(args))
	}
	if tq.Lane != "" {
		args = append(args, tq.Lane)
		q += fmt.Sprintf(" AND lane=$%d", len(args))
	}
	if tq.ParentLane != "" {
		args = append(args, tq.ParentLane)
		q += fmt.Sprintf(" AND parent_lane=$%d", len(args))
	}
	if tq.State != "" {
		q += stateClause(tq.State, &args)
	}
	if tq.Project != nil {
		if *tq.Project == "" {
			q += " AND project IS NULL"
		} else {
			args = append(args, *tq.Project)
			q += fmt.Sprintf(" AND project=$%d", len(args))
		}
	}
	if tq.Area != nil {
		if *tq.Area == "" {
			q += " AND area IS NULL"
		} else {
			args = append(args, *tq.Area)
			q += fmt.Sprintf(" AND area=$%d", len(args))
		}
	}
	if tq.ChangeID != nil {
		args = append(args, *tq.ChangeID)
		q += fmt.Sprintf(" AND id IN (SELECT task_id FROM change_tasks WHERE change_id=$%d)", len(args))
	}
	now := time.Now().UTC()
	if tq.Stalled {
		args = append(args, now.Add(-s.StartedStalledHours()))
		q += fmt.Sprintf(" AND state IN ("+startedStatesSQL+") AND NOT awaiting_decision AND "+taskActivitySQL+" < $%d", len(args))
	}
	args = append(args, limit)
	if tq.AfterID != nil || tq.ChangeID != nil {
		q += fmt.Sprintf(" ORDER BY id ASC LIMIT $%d", len(args))
	} else {
		q += fmt.Sprintf(" ORDER BY priority DESC,created_at ASC,id ASC LIMIT $%d", len(args))
	}
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, s.markStalled(ctx, s.pool, out, now)
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

	// The flag that replaced needs_decision is not a state; it is reported
	// beside the states under its own key.
	var awaiting int
	if err = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM tasks WHERE awaiting_decision`).Scan(&awaiting); err != nil {
		return nil, nil, err
	}
	counts[AwaitingDecisionKey] = awaiting
	rows, err = s.pool.Query(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE state IN ('researching','developing','reviewing')
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
	one := []Task{x}
	if err := s.markStalled(ctx, s.pool, one, time.Now().UTC()); err != nil {
		return Task{}, false, err
	}
	x = one[0]
	rows, err := s.pool.Query(ctx, `SELECT id,task_id,"from","to","by",note,refs,at,kind,no_job,source,source_ref,field,from_value,to_value,auto,applied FROM task_events WHERE task_id=$1 ORDER BY at,id`, id)
	if err != nil {
		return Task{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var e TaskEvent
		var refs []byte
		if err := rows.Scan(&e.ID, &e.TaskID, &e.From, &e.To, &e.By, &e.Note, &refs, &e.At, &e.Kind, &e.NoJob, &e.Source, &e.SourceRef, &e.Field, &e.FromValue, &e.ToValue, &e.Auto, &e.Applied); err != nil {
			return Task{}, false, err
		}
		if len(refs) != 0 {
			var snapshot struct {
				TaskRefs
				JobRebind *TaskJobRebind `json:"job_rebind,omitempty"`
			}
			if err := json.Unmarshal(refs, &snapshot); err != nil {
				return Task{}, false, err
			}
			e.Refs = &snapshot.TaskRefs
			e.Rebind = snapshot.JobRebind
		}
		x.Events = append(x.Events, e)
	}
	if err := rows.Err(); err != nil {
		return Task{}, false, err
	}
	rows.Close()
	if x.Children, err = s.ListTaskChildren(ctx, id); err != nil {
		return Task{}, false, err
	}
	if x.Relations, err = s.ListTaskRelations(ctx, id); err != nil {
		return Task{}, false, err
	}
	return x, true, nil
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
	// Panewire sends the durable job event filename as event_id. Job relay
	// deduplication still uses the five-field key, not that producer filename.
	if x.JobID == "" || x.Text != "" {
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return x, false, err
	}
	defer tx.Rollback(ctx)
	// With the work board on, recordLinearBoardRelayTx below locks every task
	// the event matches: the task-mutation lock comes first, before the job
	// lock (lock order, lockorder.go).
	// Mirroring an answer or a job's end updates decision records that name a
	// task or a job, which needs the same lock (decision_records.go).
	if s.LinearBoardEnabled() || relayTouchesDecisions(x) {
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
			return x, false, err
		}
	}
	if x.JobID != "" && wrkTablesExist(ctx, tx) {
		if _, err = tx.Exec(ctx, `INSERT INTO wrk_job_locks(job_id) VALUES($1) ON CONFLICT DO NOTHING`, x.JobID); err != nil {
			return x, false, err
		}
		var locked string
		if err = tx.QueryRow(ctx, `SELECT job_id FROM wrk_job_locks WHERE job_id=$1 FOR UPDATE`, x.JobID).Scan(&locked); err != nil {
			return x, false, err
		}
	}
	if err = s.wrkFence(ctx, tx, wrkrecords.LegacyIntent{JobID: x.JobID, Kind: x.Kind, Head: x.Head, Digest: x.ReportSHA, Receipt: x.WrkReceipt}); err != nil {
		return x, false, err
	}
	if x.Kind == "job.completed" || x.Kind == "job.joined" {
		snapshot, e := loadWrkSnapshot(ctx, tx)
		if e != nil {
			return x, false, e
		}
		if snapshot.ReceiptRequired(x.JobID) {
			receipt := x.WrkReceipt
			if receipt == nil {
				_, receipt = WrkPrincipal(ctx)
			}
			if receipt == nil {
				return x, false, wrkrecords.Error("wrk_receipt_required")
			}
			err = scanRelayEvent(tx.QueryRow(ctx, `SELECT `+relayEventColumns+` FROM relay_events WHERE kind=$1 AND job_id=$2 AND report_path=$3 AND epoch=0 AND reason=''`, x.Kind, x.JobID, "wrk:"+x.JobID+":"+intString(receipt.Seq)), &x)
			if err != nil {
				return x, false, wrkrecords.Error("wrk_unavailable")
			}
			hydrateWrkRelay(ctx, tx, &x)
			return x, false, tx.Commit(ctx)
		}
	}

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
		query += ` ON CONFLICT (kind,job_id,epoch,report_path,reason) WHERE ` + relayJobConflictPredicate + ` DO UPDATE SET attempts=relay_events.attempts+1`
	}
	err = scanRelayEventCreated(tx.QueryRow(ctx, query+` RETURNING `+relayEventColumns+`,(xmax=0) AS created`, x.Kind, x.JobID, x.Epoch, x.OwnerLane, x.Machine, x.PaneID, x.ReportPath, x.ReportLastLine, x.Question, x.PR, x.Head, x.Reason, x.EventID, x.Text, x.EventTime), &x, &created)
	if err != nil {
		return x, false, err
	}
	if created {
		if err = s.recordLinearBoardRelayTx(ctx, tx, x); err != nil {
			return x, false, err
		}
		if err = mirrorRelayDecisionsTx(ctx, tx, x); err != nil {
			return x, false, err
		}
	}
	hydrateWrkRelay(ctx, tx, &x)
	return x, created, tx.Commit(ctx)
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
		hydrateWrkRelay(ctx, s.pool, &x)
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
	hydrateWrkRelay(ctx, s.pool, &x)
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
	err = rows.Err()
	rows.Close() // Release the connection before canonical hydration reads.
	if err != nil {
		return nil, err
	}
	s.hydrateWrkRelays(ctx, out)
	return out, nil
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
	err = rows.Err()
	rows.Close() // Release the connection before canonical hydration reads.
	if err != nil {
		return nil, err
	}
	s.hydrateWrkRelays(ctx, out)
	return out, nil
}

// EventWatermarks are the durable high-water marks the UI's SSE poller
// compares. SyncMaxID is the change feed's committed watermark
// (sync_state.max_sync_id, docs/sync-feed.md): it moves on document, task,
// task comment and decision request writes, which leave the other two alone.
type EventWatermarks struct {
	RelayMaxID     int64 `json:"relay_max_id"`
	TaskEventMaxID int64 `json:"task_event_max_id"`
	SyncMaxID      int64 `json:"sync_max_id"`
}

// EventWatermarks reads the current watermarks in one snapshot.
func (s *Store) EventWatermarks(ctx context.Context) (EventWatermarks, error) {
	var w EventWatermarks
	err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT MAX(id) FROM relay_events),0),COALESCE((SELECT MAX(id) FROM task_events),0),COALESCE((SELECT max_sync_id FROM sync_state WHERE singleton),0)`).Scan(&w.RelayMaxID, &w.TaskEventMaxID, &w.SyncMaxID)
	return w, err
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

// awaitingEventSQL selects the events that set the awaiting_decision flag: the
// current awaiting_decision rows and, for history, the transitions into the
// old needs_decision state. History keeps the old vocabulary as written.
const awaitingEventSQL = `((kind='awaiting_decision' AND to_value='true') OR (kind='transition' AND "to"='needs_decision'))`

// TaskDecision is an unresolved task together with the original question
// recorded when the task last began waiting on a decision.
type TaskDecision struct {
	Task     Task
	Question string
}

// awaitingTaskDecisionSQL is the task half of the decision inbox, read as
// "tasks": awaiting a decision, not a disposition, and without an open
// structured request (that request is listed on its own).
const awaitingTaskDecisionSQL = `awaiting_decision AND NOT (refs ? 'disposition') AND COALESCE(refs->'decision_request'->>'status','')<>'open'`

// openLaneDecisionSQL is the lane.event half of the decision inbox, read as
// relay_events "e": a [decision-needed] event with no later
// [decision-answered] reply from its lane.
const openLaneDecisionSQL = `e.kind='lane.event' AND e.text LIKE '[decision-needed]%' AND NOT EXISTS (
			SELECT 1 FROM relay_events resolved WHERE resolved.kind='lane.event' AND resolved.owner_lane=e.owner_lane
			AND resolved.text LIKE '[decision-answered] #' || e.id::text || ':%' AND resolved.id>e.id
		)`

// ListOpenTaskDecisions implements the decision inbox task definition.
// Disposition items are listed separately by ListOpenDispositions: they are
// answered only through the operator disposition routes. Tasks whose current
// structured request is open are listed by ListOpenDecisionRequests instead,
// so one question never appears twice (or with two answer paths).
func (s *Store) ListOpenTaskDecisions(ctx context.Context, limit int) ([]TaskDecision, error) {
	if limit < 1 {
		limit = 1000
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixedTaskColumns("t")+`,
		COALESCE((SELECT note FROM task_events WHERE task_id=t.id AND `+awaitingEventSQL+` ORDER BY at DESC,id DESC LIMIT 1),'')
		FROM tasks t WHERE `+awaitingTaskDecisionSQL+` ORDER BY t.updated_at DESC,t.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskDecision{}
	for rows.Next() {
		var x TaskDecision
		if err := scanTask(rows, &x.Task, &x.Question); err != nil {
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
			AND resolved.kind IN ('job.joined','job.completed','job.lost','job.revoked') AND resolved.id>e.id
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
	err = rows.Err()
	rows.Close() // Release the connection before canonical hydration reads.
	if err != nil {
		return nil, err
	}
	s.hydrateWrkRelays(ctx, out)
	return out, nil
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
		WHERE `+openLaneDecisionSQL+` ORDER BY e.id DESC LIMIT $1`, limit)
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
	err = rows.Err()
	rows.Close() // Release the connection before canonical hydration reads.
	if err != nil {
		return nil, err
	}
	s.hydrateWrkRelays(ctx, out)
	return out, nil
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
		if errors.Is(e, pgx.ErrNoRows) {
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
	if (x.OriginClass != "" && !documentOriginClasses[x.OriginClass]) || (x.TaskID != nil && *x.TaskID < 1) {
		return x, false, errors.New("invalid document")
	}
	if e := guard.Reject(x.Key + "\n" + x.Body); e != nil {
		return x, false, e
	}
	sum := sha256.Sum256([]byte(x.Body))
	x.SHA256 = hex.EncodeToString(sum[:])
	now := time.Now().UTC()
	var existing string
	var existingTask *int64
	var existingClass *string
	e := s.pool.QueryRow(ctx, "SELECT sha256,task_id,origin_class FROM documents WHERE key=$1", x.Key).Scan(&existing, &existingTask, &existingClass)
	// Unchanged only when the body and every supplied new field already match;
	// a field left out never counts as a change and keeps its stored value.
	if e == nil && existing == x.SHA256 &&
		(x.TaskID == nil || (existingTask != nil && *existingTask == *x.TaskID)) &&
		(x.OriginClass == "" || (existingClass != nil && *existingClass == x.OriginClass)) {
		// Answer with what is stored, not with the omitted input fields.
		x.TaskID = existingTask
		x.OriginClass = ""
		if existingClass != nil {
			x.OriginClass = *existingClass
		}
		return x, false, nil
	}
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return x, false, e
	}
	// A deploy record moves the merged tasks it lists in the same transaction
	// (hk.deploy); any other document is a plain upsert.
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return x, false, e
	}
	defer tx.Rollback(ctx)
	// applyDeployRecordTx locks the listed tasks: the task-mutation lock comes
	// before the document row (lock order, lockorder.go).
	if IsDeployRecordKey(x.Key) {
		if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); e != nil {
			return x, false, e
		}
	}
	if x.TaskID != nil {
		var taskExists bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, *x.TaskID).Scan(&taskExists); e != nil {
			return x, false, e
		}
		if !taskExists {
			return x, false, ErrTaskNotFound
		}
	}
	// task_id and origin_class keep the stored value when the caller omits
	// them; the foreign key takes KEY SHARE on one task row (lockorder.go).
	e = tx.QueryRow(ctx, `INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at,task_id,origin_class) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8,$9,$10) ON CONFLICT(key) DO UPDATE SET kind=EXCLUDED.kind,session=EXCLUDED.session,job=EXCLUDED.job,body=EXCLUDED.body,sha256=EXCLUDED.sha256,created_by=EXCLUDED.created_by,updated_at=EXCLUDED.updated_at,task_id=COALESCE(EXCLUDED.task_id,documents.task_id),origin_class=COALESCE(EXCLUDED.origin_class,documents.origin_class) RETURNING id,created_at,updated_at,task_id,COALESCE(origin_class,'')`, x.Key, x.Kind, x.Session, x.Job, x.Body, x.SHA256, x.CreatedBy, now, x.TaskID, nullIfEmpty(x.OriginClass)).Scan(&x.ID, &x.CreatedAt, &x.UpdatedAt, &x.TaskID, &x.OriginClass)
	if e != nil {
		return x, true, e
	}
	if IsDeployRecordKey(x.Key) {
		if e = s.applyDeployRecordTx(ctx, tx, x.Key, x.Body, now); e != nil {
			return x, true, e
		}
	}
	return x, true, tx.Commit(ctx)
}
func (s *Store) GetDocument(ctx context.Context, key string) (Document, bool, error) {
	if !validDocKey(key) {
		return Document{}, false, errors.New("invalid document key")
	}
	var x Document
	e := s.pool.QueryRow(ctx, `SELECT id,key,kind,session,job,body,sha256,created_by,created_at,updated_at,task_id,COALESCE(origin_class,'') FROM documents WHERE key=$1`, key).Scan(&x.ID, &x.Key, &x.Kind, &x.Session, &x.Job, &x.Body, &x.SHA256, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt, &x.TaskID, &x.OriginClass)
	if e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
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
	e := s.pool.QueryRow(ctx, `SELECT id,key,kind,session,job,body,sha256,created_by,created_at,updated_at,task_id,COALESCE(origin_class,'') FROM documents WHERE id=$1`, id).Scan(&x.ID, &x.Key, &x.Kind, &x.Session, &x.Job, &x.Body, &x.SHA256, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt, &x.TaskID, &x.OriginClass)
	if e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return x, false, nil
		}
		return x, false, e
	}
	return x, true, nil
}

// ListDocuments lists documents without bodies. task, when above zero, keeps
// only the documents that name that task.
func (s *Store) ListDocuments(ctx context.Context, prefix, kind, session string, task int64, limit int) ([]Document, error) {
	if !validText(prefix, 512) || !validText(session, 128) || (kind != "" && !documentKinds[kind]) || task < 0 {
		return nil, errors.New("invalid document query")
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, e := s.pool.Query(ctx, `SELECT id,key,kind,session,job,sha256,created_by,created_at,updated_at,task_id,COALESCE(origin_class,'') FROM documents WHERE ($1='' OR key LIKE $1 || '%') AND ($2='' OR kind=$2) AND ($3='' OR session=$3) AND ($4::bigint=0 OR task_id=$4::bigint) ORDER BY key LIMIT $5`, prefix, kind, session, task, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		var x Document
		if e = rows.Scan(&x.ID, &x.Key, &x.Kind, &x.Session, &x.Job, &x.SHA256, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt, &x.TaskID, &x.OriginClass); e != nil {
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
		if errors.Is(e, pgx.ErrNoRows) {
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
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
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
		q += ` WHERE r.ref_kind=$1 AND r.ref_id=$2 ORDER BY a.created_at DESC,a.sha256,r.ref_kind,r.ref_id LIMIT $3`
		args = []any{refKind, refID, limit}
	} else {
		q += ` ORDER BY a.created_at DESC,a.sha256,r.ref_kind,r.ref_id LIMIT $1`
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
	if e != nil && errors.Is(e, pgx.ErrNoRows) {
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
	rows, e := s.pool.Query(ctx, `SELECT scope,key,session,kind,title,snippet,refs,created_at FROM (`+strings.Join(parts, " UNION ALL ")+") r ORDER BY created_at DESC,scope,key,session,title LIMIT $3", session, q, limit+1)
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
