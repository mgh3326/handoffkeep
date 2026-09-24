package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// A decision request is the structured record of one question a lane puts to
// the operator (#618, hk:doc task/2026-09-24/console-req-1 수정 AC). It
// extends the existing decision model instead of adding a table: the current
// request lives in tasks.refs.decision_request next to the existing
// refs.decision_options (its choices), and every write appends a task_events
// row whose refs snapshot keeps the full request. History is read back from
// those snapshots, so the record is append-only without a new table.
//
// Invariants:
//   - one current request per task; a different request replaces an open one
//     only when the caller names it (supersedes), so nothing is silently
//     overwritten, and a new request starts with no resolution — a revision
//     never inherits an older answer;
//   - the recommendation, the no-response action and the response deadline
//     are separate fields: the default may differ from the recommendation;
//   - nothing here applies a default on its own. A passed deadline is only
//     "deadline passed, not applied" until a default_applied resolution
//     carrying a receipt is recorded;
//   - the request's status is independent of the task state: requests may be
//     open on backlog or in_progress tasks, and an open request left on a
//     merged or dropped task stays open (shown as uncleaned) until resolved.

const (
	DecisionRequestOpen           = "open"
	DecisionRequestAnswered       = "answered"
	DecisionRequestDefaultApplied = "default_applied"
	DecisionRequestWithdrawn      = "withdrawn"
)

// Byte limits. Labels keep the existing 120-byte option limit (bytes, not
// characters: 40 Hangul syllables fill it); longer outcome text belongs in the
// request document (Doc).
const (
	DecisionRequestActionMaxBytes  = 300
	DecisionRequestReasonMaxBytes  = 500
	DecisionRequestReceiptMaxBytes = 512
	DecisionRequestTextMaxBytes    = 1000
	DecisionOptionLabelMaxBytes    = 120
)

// TaskEventDecision marks task_events rows that record a decision request or
// its resolution without changing state. Their from/to both carry the
// current state; readers that interpret "to" as a state change must filter
// kind='transition' (every existing reader already does).
const TaskEventDecision = "decision"

var (
	// ErrDecisionRequestOpen refuses a different request while one is open
	// and the caller did not name it in supersedes.
	ErrDecisionRequestOpen = errors.New("decision_request_open")
	// ErrDecisionRequestStale refuses a write addressed to a request that is
	// not the task's current one (superseded, or never existed).
	ErrDecisionRequestStale = errors.New("decision_request_stale")
	// ErrDecisionRequestResolved refuses a second, different resolution.
	ErrDecisionRequestResolved = errors.New("decision_request_resolved")
)

// ErrInvalidDecisionRequest wraps every validation failure so the API can
// return the specific reason while still classifying it as a bad request.
var ErrInvalidDecisionRequest = errors.New("invalid_decision_request")

func invalidDecisionRequest(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidDecisionRequest, reason)
}

type DecisionRequest struct {
	ID         string `json:"id"`
	Revision   int    `json:"revision"`
	Supersedes string `json:"supersedes,omitempty"`
	Status     string `json:"status"`
	Question   string `json:"question"`
	// Reason explains the recommendation; the recommended key itself is the
	// existing DecisionOption.Recommended flag.
	Reason         string              `json:"reason,omitempty"`
	DefaultAction  string              `json:"default_action"`
	DefaultOption  string              `json:"default_option,omitempty"`
	DefaultTrigger string              `json:"default_trigger,omitempty"`
	DueAt          *time.Time          `json:"due_at,omitempty"`
	Doc            string              `json:"doc,omitempty"`
	RequestedBy    string              `json:"requested_by"`
	RequestedAt    time.Time           `json:"requested_at"`
	Resolution     *DecisionResolution `json:"resolution,omitempty"`
}

// DecisionResolution closes a request. By is the authenticated recorder;
// Responder optionally names who actually answered (e.g. "operator") when a
// lane records an answer given elsewhere.
type DecisionResolution struct {
	Kind      string    `json:"kind"`
	Option    string    `json:"option,omitempty"`
	Text      string    `json:"text,omitempty"`
	Receipt   string    `json:"receipt,omitempty"`
	Responder string    `json:"responder,omitempty"`
	By        string    `json:"by"`
	At        time.Time `json:"at"`
}

// DecisionRequestInput is the producer's request. Options carries the
// existing decision option model (keys A–F, one optional recommendation).
type DecisionRequestInput struct {
	Question       string          `json:"question"`
	Options        DecisionOptions `json:"options"`
	Reason         string          `json:"reason,omitempty"`
	DefaultAction  string          `json:"default_action"`
	DefaultOption  string          `json:"default_option,omitempty"`
	DefaultTrigger string          `json:"default_trigger,omitempty"`
	DueAt          *time.Time      `json:"due_at,omitempty"`
	Doc            string          `json:"doc,omitempty"`
	Supersedes     string          `json:"supersedes,omitempty"`
	// Block also moves the task to needs_decision in the same transaction
	// (a request that stops the work). Without it the state is unchanged.
	Block bool `json:"block,omitempty"`
}

type DecisionResolveInput struct {
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"`
	Option    string `json:"option,omitempty"`
	Text      string `json:"text,omitempty"`
	Receipt   string `json:"receipt,omitempty"`
	Responder string `json:"responder,omitempty"`
}

// DecisionRequestResult is what a producer needs to notify: the recorded
// request (its id is what pane notifications carry) and whether this call
// was a retry of an already recorded request.
type DecisionRequestResult struct {
	Task      Task            `json:"task"`
	Request   DecisionRequest `json:"request"`
	Duplicate bool            `json:"duplicate"`
}

func decisionRequestID(taskID int64, revision int) string {
	return "dr-" + strconv.FormatInt(taskID, 10) + "-" + strconv.Itoa(revision)
}

// validDecisionLine accepts one non-empty line of printable text.
func validDecisionLine(value string, max int) bool {
	if value != strings.TrimSpace(value) || value == "" || len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	return true
}

func optionKeys(options DecisionOptions) map[string]bool {
	keys := map[string]bool{}
	for _, option := range options.Options {
		keys[option.Key] = true
	}
	return keys
}

// ValidateDecisionOptionsDetailed names the first reason options are
// refused. It applies the same rules as validDecisionOptions.
func ValidateDecisionOptionsDetailed(x DecisionOptions) error {
	if len(x.Options) == 0 {
		return invalidDecisionRequest("at least one option is required")
	}
	if len(x.Options) > 6 {
		return invalidDecisionRequest("at most six options are allowed")
	}
	seen := map[string]bool{}
	recommended := 0
	for _, option := range x.Options {
		if !validDecisionOptionKey(option.Key) {
			return invalidDecisionRequest("option keys are single letters A-F")
		}
		if seen[option.Key] {
			return invalidDecisionRequest("duplicate option key " + option.Key)
		}
		seen[option.Key] = true
		if len(option.Label) > DecisionOptionLabelMaxBytes {
			return invalidDecisionRequest(fmt.Sprintf("option %s label is %d bytes; the limit is %d bytes (not characters) — put longer outcome text in the request document", option.Key, len(option.Label), DecisionOptionLabelMaxBytes))
		}
		if !validDecisionOptionLabel(option.Label) {
			return invalidDecisionRequest("option " + option.Key + " label must be one trimmed line without | or ;")
		}
		if option.Recommended {
			recommended++
		}
	}
	if recommended > 1 {
		return invalidDecisionRequest("at most one option may be recommended")
	}
	return nil
}

// ValidateDecisionRequestInput checks a request before anything is written.
// It is exported so the CLI can report the exact reason locally.
func ValidateDecisionRequestInput(in DecisionRequestInput) error {
	if strings.TrimSpace(in.Question) == "" {
		return invalidDecisionRequest("question is required")
	}
	if !validText(in.Question, MaxBytes) || !utf8.ValidString(in.Question) {
		return invalidDecisionRequest("question is not valid text")
	}
	if err := ValidateDecisionOptionsDetailed(in.Options); err != nil {
		return err
	}
	// The same bound as a needs_decision transition: the question and its
	// option line must fit one relay lane event.
	if len(in.Question+"\n"+FormatDecisionOptions(in.Options)) > RelayLaneEventMaxBytes {
		return invalidDecisionRequest("question and options exceed 2048 bytes — put detail in the request document")
	}
	if !validDecisionLine(in.DefaultAction, DecisionRequestActionMaxBytes) {
		return invalidDecisionRequest(fmt.Sprintf("default action is required: one line of at most %d bytes (write \"자동 적용 없음\" when nothing is applied on silence)", DecisionRequestActionMaxBytes))
	}
	if in.DefaultOption != "" && !optionKeys(in.Options)[in.DefaultOption] {
		return invalidDecisionRequest("default option must name a supplied option key")
	}
	if in.DefaultTrigger != "" && !validDecisionLine(in.DefaultTrigger, DecisionRequestActionMaxBytes) {
		return invalidDecisionRequest(fmt.Sprintf("default trigger must be one line of at most %d bytes", DecisionRequestActionMaxBytes))
	}
	if in.Reason != "" && !validDecisionLine(in.Reason, DecisionRequestReasonMaxBytes) {
		return invalidDecisionRequest(fmt.Sprintf("recommendation reason must be one line of at most %d bytes", DecisionRequestReasonMaxBytes))
	}
	if in.Doc != "" && !ValidBodyDoc(in.Doc) {
		return invalidDecisionRequest("doc must be a document key or key#section")
	}
	if in.Supersedes != "" && !strings.HasPrefix(in.Supersedes, "dr-") {
		return invalidDecisionRequest("supersedes must be a request id (dr-<task>-<revision>)")
	}
	return nil
}

// ValidateDecisionResolveInput checks the shape of a resolution; option
// membership is checked against the stored request.
func ValidateDecisionResolveInput(in DecisionResolveInput) error {
	if !strings.HasPrefix(in.RequestID, "dr-") || !validDecisionLine(in.RequestID, 64) {
		return invalidDecisionRequest("request_id is required (dr-<task>-<revision>)")
	}
	if in.Option != "" && !validDecisionOptionKey(in.Option) {
		return invalidDecisionRequest("option must be a single letter A-F")
	}
	if in.Text != "" && (!validText(in.Text, DecisionRequestTextMaxBytes) || !utf8.ValidString(in.Text) || strings.TrimSpace(in.Text) == "") {
		return invalidDecisionRequest(fmt.Sprintf("text must be at most %d bytes", DecisionRequestTextMaxBytes))
	}
	if in.Responder != "" && !validDecisionLine(in.Responder, 128) {
		return invalidDecisionRequest("responder must be one line of at most 128 bytes")
	}
	if in.Receipt != "" && !validDecisionLine(in.Receipt, DecisionRequestReceiptMaxBytes) {
		return invalidDecisionRequest(fmt.Sprintf("receipt must be one line of at most %d bytes", DecisionRequestReceiptMaxBytes))
	}
	switch in.Kind {
	case DecisionRequestAnswered:
		if in.Option == "" && in.Text == "" {
			return invalidDecisionRequest("an answer needs an option or answer text")
		}
		if in.Receipt != "" {
			return invalidDecisionRequest("receipt belongs to default_applied only")
		}
	case DecisionRequestDefaultApplied:
		// "Applied" is shown only with evidence of the application.
		if in.Receipt == "" {
			return invalidDecisionRequest("default_applied requires a receipt (the application event, PR, or document)")
		}
	case DecisionRequestWithdrawn:
		if in.Text == "" {
			return invalidDecisionRequest("withdrawn requires a reason text")
		}
		if in.Option != "" || in.Receipt != "" {
			return invalidDecisionRequest("withdrawn takes a reason text only")
		}
	default:
		return invalidDecisionRequest("kind must be answered, default_applied or withdrawn")
	}
	return nil
}

func sameDecisionOptions(a, b *DecisionOptions) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.AllowFree != b.AllowFree || len(a.Options) != len(b.Options) {
		return false
	}
	for i := range a.Options {
		if a.Options[i] != b.Options[i] {
			return false
		}
	}
	return true
}

func sameDue(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// sameDecisionRequest reports whether in re-sends the current request — the
// retry case that must return the same request_id instead of a new one.
func sameDecisionRequest(current DecisionRequest, currentOptions *DecisionOptions, in DecisionRequestInput) bool {
	return current.Question == in.Question && sameDecisionOptions(currentOptions, &in.Options) && current.Reason == in.Reason &&
		current.DefaultAction == in.DefaultAction && current.DefaultOption == in.DefaultOption &&
		current.DefaultTrigger == in.DefaultTrigger && sameDue(current.DueAt, in.DueAt) && current.Doc == in.Doc
}

func rejectDecisionRequestSecrets(in DecisionRequestInput) error {
	values := []string{in.Question, in.Reason, in.DefaultAction, in.DefaultTrigger, in.Doc}
	for _, option := range in.Options.Options {
		values = append(values, option.Label)
	}
	return guard.Reject(strings.Join(values, "\n"))
}

func insertDecisionEvent(ctx context.Context, tx pgx.Tx, id int64, state, by, note string, refs TaskRefs, at time.Time) error {
	encoded, err := json.Marshal(refs)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,refs,at,kind) VALUES($1,$2,$2,$3,$4,$5::jsonb,$6,'decision')`, id, state, by, note, string(encoded), at)
	return err
}

func isTerminalTaskState(state string) bool {
	return state == "merged" || state == "dropped"
}

// RecordDecisionRequest records a request on a task. A byte-identical
// re-send of the task's latest request returns it with Duplicate set and
// writes nothing. A different request while one is open is refused unless
// Supersedes names the open request; the new one gets the next revision and
// no resolution. New requests are refused on merged/dropped tasks.
func (s *Store) RecordDecisionRequest(ctx context.Context, taskID int64, by string, in DecisionRequestInput) (DecisionRequestResult, error) {
	if taskID < 1 {
		return DecisionRequestResult{}, invalidDecisionRequest("task id must be positive")
	}
	if by == "" || !validText(by, 128) {
		return DecisionRequestResult{}, invalidDecisionRequest("requester is required")
	}
	if in.DueAt != nil {
		due := in.DueAt.UTC()
		in.DueAt = &due
	}
	if err := ValidateDecisionRequestInput(in); err != nil {
		return DecisionRequestResult{}, err
	}
	if err := rejectDecisionRequestSecrets(in); err != nil {
		return DecisionRequestResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DecisionRequestResult{}, err
	}
	defer tx.Rollback(ctx)
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, taskID), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DecisionRequestResult{}, ErrTaskNotFound
		}
		return DecisionRequestResult{}, err
	}
	// Disposition items have their own closed options and operator route.
	if x.Refs.Disposition != nil {
		return DecisionRequestResult{}, invalidDecisionRequest("disposition items take no decision requests")
	}
	current := x.Refs.DecisionRequest
	if current != nil && in.Supersedes == "" && sameDecisionRequest(*current, x.Refs.DecisionOptions, in) {
		return DecisionRequestResult{Task: x, Request: *current, Duplicate: true}, nil
	}
	if isTerminalTaskState(x.State) {
		return DecisionRequestResult{}, ErrTaskTerminal
	}
	switch {
	case current == nil && in.Supersedes != "":
		return DecisionRequestResult{}, ErrDecisionRequestStale
	case current != nil && in.Supersedes != "" && in.Supersedes != current.ID:
		return DecisionRequestResult{}, ErrDecisionRequestStale
	case current != nil && current.Status == DecisionRequestOpen && in.Supersedes == "":
		return DecisionRequestResult{}, ErrDecisionRequestOpen
	}
	revision := 1
	if current != nil {
		revision = current.Revision + 1
	}
	now := time.Now().UTC()
	options := in.Options
	request := DecisionRequest{
		ID:             decisionRequestID(taskID, revision),
		Revision:       revision,
		Supersedes:     in.Supersedes,
		Status:         DecisionRequestOpen,
		Question:       in.Question,
		Reason:         in.Reason,
		DefaultAction:  in.DefaultAction,
		DefaultOption:  in.DefaultOption,
		DefaultTrigger: in.DefaultTrigger,
		DueAt:          in.DueAt,
		Doc:            in.Doc,
		RequestedBy:    by,
		RequestedAt:    now,
	}
	x.Refs.DecisionRequest = &request
	x.Refs.DecisionOptions = &options
	from, to := x.State, x.State
	if in.Block && x.State != "needs_decision" {
		if !taskTransitionAllowed(x.State, "needs_decision") {
			return DecisionRequestResult{}, ErrTaskConflict
		}
		to = "needs_decision"
	}
	encoded, err := json.Marshal(x.Refs)
	if err != nil {
		return DecisionRequestResult{}, err
	}
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET state=$2,refs=$3::jsonb,updated_at=$4 WHERE id=$1 RETURNING `+taskColumns, taskID, to, string(encoded), now), &x); err != nil {
		return DecisionRequestResult{}, err
	}
	note := "decision-request " + request.ID
	if request.Supersedes != "" {
		note += " (supersedes " + request.Supersedes + ")"
	}
	note += ": " + request.Question
	if from != to {
		// A blocking request is also the task's needs_decision transition;
		// its note keeps the question readers of that transition expect.
		if _, err = tx.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,refs,at) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7)`, taskID, from, to, by, request.Question, string(encoded), now); err != nil {
			return DecisionRequestResult{}, err
		}
		if s.LinearSyncEnabled() && x.Refs.Linear != nil && x.Refs.Linear.Sync {
			if err = enqueueLinearTaskTransition(ctx, tx, x, request.Question); err != nil {
				return DecisionRequestResult{}, err
			}
		}
	} else if err = insertDecisionEvent(ctx, tx, taskID, x.State, by, note, x.Refs, now); err != nil {
		return DecisionRequestResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return DecisionRequestResult{}, err
	}
	return DecisionRequestResult{Task: x, Request: request}, nil
}

func sameResolution(r DecisionResolution, in DecisionResolveInput) bool {
	return r.Kind == in.Kind && r.Option == in.Option && r.Text == in.Text && r.Receipt == in.Receipt && r.Responder == in.Responder
}

// ResolveDecisionRequest closes the task's current request as answered,
// default_applied (receipt required) or withdrawn. It never changes the task
// state and is allowed on merged/dropped tasks, which is how an uncleaned
// request is closed. A request that is not the current one is stale; an
// identical re-send of the recorded resolution is a duplicate.
func (s *Store) ResolveDecisionRequest(ctx context.Context, taskID int64, by string, in DecisionResolveInput) (DecisionRequestResult, error) {
	if taskID < 1 {
		return DecisionRequestResult{}, invalidDecisionRequest("task id must be positive")
	}
	if by == "" || !validText(by, 128) {
		return DecisionRequestResult{}, invalidDecisionRequest("recorder is required")
	}
	if err := ValidateDecisionResolveInput(in); err != nil {
		return DecisionRequestResult{}, err
	}
	if err := guard.Reject(strings.Join([]string{in.Text, in.Receipt, in.Responder}, "\n")); err != nil {
		return DecisionRequestResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DecisionRequestResult{}, err
	}
	defer tx.Rollback(ctx)
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, taskID), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DecisionRequestResult{}, ErrTaskNotFound
		}
		return DecisionRequestResult{}, err
	}
	request := x.Refs.DecisionRequest
	if request == nil || request.ID != in.RequestID {
		return DecisionRequestResult{}, ErrDecisionRequestStale
	}
	if request.Status != DecisionRequestOpen {
		if request.Resolution != nil && sameResolution(*request.Resolution, in) {
			return DecisionRequestResult{Task: x, Request: *request, Duplicate: true}, nil
		}
		return DecisionRequestResult{}, ErrDecisionRequestResolved
	}
	if in.Option != "" {
		if x.Refs.DecisionOptions == nil || !optionKeys(*x.Refs.DecisionOptions)[in.Option] {
			return DecisionRequestResult{}, invalidDecisionRequest("option " + in.Option + " is not one of the request's options")
		}
	}
	if in.Kind == DecisionRequestAnswered && in.Option == "" && (x.Refs.DecisionOptions == nil || !x.Refs.DecisionOptions.AllowFree) {
		return DecisionRequestResult{}, invalidDecisionRequest("this request does not allow a free answer; name an option")
	}
	now := time.Now().UTC()
	resolved := *request
	resolved.Status = in.Kind
	resolved.Resolution = &DecisionResolution{Kind: in.Kind, Option: in.Option, Text: in.Text, Receipt: in.Receipt, Responder: in.Responder, By: by, At: now}
	x.Refs.DecisionRequest = &resolved
	encoded, err := json.Marshal(x.Refs)
	if err != nil {
		return DecisionRequestResult{}, err
	}
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET refs=$2::jsonb,updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, taskID, string(encoded), now), &x); err != nil {
		return DecisionRequestResult{}, err
	}
	note := "decision-" + in.Kind + " " + resolved.ID
	if in.Option != "" {
		note += " " + in.Option
	}
	if in.Receipt != "" {
		note += " receipt " + in.Receipt
	}
	if in.Text != "" {
		note += ": " + in.Text
	}
	if err = insertDecisionEvent(ctx, tx, taskID, x.State, by, note, x.Refs, now); err != nil {
		return DecisionRequestResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return DecisionRequestResult{}, err
	}
	return DecisionRequestResult{Task: x, Request: resolved}, nil
}

// Display states. They are derived for reading only and never stored:
// "overdue" is a passed deadline with no default recorded as applied, and
// "uncleaned" is an open request on a merged/dropped task.
const (
	DecisionViewOpen           = "open"
	DecisionViewOverdue        = "overdue"
	DecisionViewUncleaned      = "uncleaned"
	DecisionViewAnswered       = "answered"
	DecisionViewDefaultApplied = "default_applied"
	DecisionViewWithdrawn      = "withdrawn"
	DecisionViewSuperseded     = "superseded"
)

// DecisionRequestState derives the display state. Only a recorded
// default_applied resolution (which always carries a receipt) reads as
// applied; time alone never does.
func DecisionRequestState(request DecisionRequest, taskState string, superseded bool, now time.Time) string {
	if superseded {
		return DecisionViewSuperseded
	}
	switch request.Status {
	case DecisionRequestAnswered:
		return DecisionViewAnswered
	case DecisionRequestDefaultApplied:
		if request.Resolution != nil && request.Resolution.Receipt != "" {
			return DecisionViewDefaultApplied
		}
	case DecisionRequestWithdrawn:
		return DecisionViewWithdrawn
	}
	if isTerminalTaskState(taskState) {
		return DecisionViewUncleaned
	}
	if request.DueAt != nil && now.After(*request.DueAt) {
		return DecisionViewOverdue
	}
	return DecisionViewOpen
}

// DecisionRequestEntry is one request as read back from a task: the latest
// recorded snapshot of that request with the options it was asked with.
type DecisionRequestEntry struct {
	Request      DecisionRequest
	Options      *DecisionOptions
	Current      bool
	SupersededBy string
}

// DecisionRequestHistory returns the task's requests, newest revision first.
// The current request comes from the task row; earlier ones from the latest
// task_events snapshot carrying their id. An earlier request whose last
// snapshot is still open was replaced by a later revision: it is superseded,
// and its options and resolution are its own — never the newer request's.
func DecisionRequestHistory(task Task) []DecisionRequestEntry {
	latest := map[string]DecisionRequestEntry{}
	order := []string{}
	for _, event := range task.Events {
		if event.Refs == nil || event.Refs.DecisionRequest == nil {
			continue
		}
		request := *event.Refs.DecisionRequest
		if _, ok := latest[request.ID]; !ok {
			order = append(order, request.ID)
		}
		latest[request.ID] = DecisionRequestEntry{Request: request, Options: event.Refs.DecisionOptions}
	}
	if task.Refs.DecisionRequest != nil {
		request := *task.Refs.DecisionRequest
		if _, ok := latest[request.ID]; !ok {
			order = append(order, request.ID)
		}
		latest[request.ID] = DecisionRequestEntry{Request: request, Options: task.Refs.DecisionOptions, Current: true}
	}
	supersededBy := map[string]string{}
	for _, id := range order {
		if s := latest[id].Request.Supersedes; s != "" {
			supersededBy[s] = id
		}
	}
	out := make([]DecisionRequestEntry, 0, len(order))
	for _, id := range order {
		entry := latest[id]
		if !entry.Current && entry.Request.Status == DecisionRequestOpen {
			entry.SupersededBy = supersededBy[id]
			if entry.SupersededBy == "" {
				// Replaced without a recorded link (never produced by this
				// package); still not the current request, so not open.
				entry.SupersededBy = "?"
			}
		}
		out = append(out, entry)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ListOpenDecisionRequests returns every task whose current request is open,
// whatever the task state: backlog and in_progress requests are listed, and
// merged/dropped ones are listed so the reader can show them as uncleaned.
func (s *Store) ListOpenDecisionRequests(ctx context.Context, limit int) ([]Task, error) {
	if limit < 1 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE refs->'decision_request'->>'status'='open' ORDER BY updated_at DESC,id DESC LIMIT $1`, limit)
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

// DecisionRequestCounts splits open requests the way every console surface
// shows them: pending (task not terminal) and uncleaned (task merged or
// dropped). It is the count the queue, the drawer and Decisions agree on.
func DecisionRequestCounts(tasks []Task) (pending, uncleaned int) {
	for _, task := range tasks {
		if task.Refs.DecisionRequest == nil || task.Refs.DecisionRequest.Status != DecisionRequestOpen {
			continue
		}
		if isTerminalTaskState(task.State) {
			uncleaned++
		} else {
			pending++
		}
	}
	return pending, uncleaned
}
