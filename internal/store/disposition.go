package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// A disposition item is an ordinary task row (kind=decide) whose refs carry a
// Disposition. It is born in needs_decision, and only the operator's
// authenticated web answer (AnswerDisposition / AnswerDispositionBatch) moves
// it out of that state; TransitionTask refuses every other caller. Silence is
// therefore inert: nothing in this package writes to an open item on its own.
// Design: hk:doc design/2026-09-21/task493-disposition-contract.

const DispositionSchema = "disposition/v1"

// DispositionBatchLimit bounds one operator batch action (ESC-2 in hk:doc
// decision/2026-09-21/task493-phase1-review). Items beyond it stay open and are
// shown as the next batch; they are never silently dropped.
const DispositionBatchLimit = 50

var (
	// ErrDispositionOperatorOnly rejects every path except the operator web
	// answer from moving a disposition item out of needs_decision.
	ErrDispositionOperatorOnly = errors.New("disposition_operator_only")
	ErrDispositionStale        = errors.New("disposition_stale")
	originPRRE                 = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9._-]{1,100}/[A-Za-z0-9._-]{1,100}/pull/[1-9][0-9]{0,9}$`)
	mergeSHARE                 = regexp.MustCompile(`^[0-9a-f]{40}$`)
	operatorEmailRE            = regexp.MustCompile(`^[^\s@]{1,128}@[^\s@]{1,128}$`)
)

// DispositionOptionLabels is the closed choice set. The key order is fixed.
var DispositionOptionLabels = []DecisionOption{
	{Key: "A", Label: "배포"},
	{Key: "B", Label: "후속 발주"},
	{Key: "C", Label: "잔여 수용"},
	{Key: "D", Label: "보류"},
	{Key: "E", Label: "조치 없음"},
}

var dispositionInstallStates = map[string]bool{"unknown": true, "not_installed": true, "partial": true, "installed": true, "not_applicable": true}

type Disposition struct {
	Schema      string             `json:"schema"`
	Facts       DispositionFacts   `json:"facts"`
	Recommended string             `json:"recommended"`
	Answer      *DispositionAnswer `json:"answer,omitempty"`
}

// DispositionFacts are recorded at creation with their source. They are not
// model judgement: merge facts come from gh pr view output validated by the
// CLI, or from hk itself for a parent-task origin.
type DispositionFacts struct {
	MergeSHA    string             `json:"merge_sha,omitempty"`
	MergedAt    *time.Time         `json:"merged_at,omitempty"`
	FactsSource string             `json:"facts_source"`
	FactsAsOf   time.Time          `json:"facts_as_of"`
	Install     DispositionInstall `json:"install"`
	ResidualN   int                `json:"residual_n"`
	ResidualDoc string             `json:"residual_doc,omitempty"`
}

// DispositionInstall absorbs #479: absence of a witness is "unknown", never
// "needs deploy".
type DispositionInstall struct {
	State        string     `json:"state"`
	TargetsPass  int        `json:"targets_pass,omitempty"`
	TargetsTotal int        `json:"targets_total,omitempty"`
	Witness      string     `json:"witness,omitempty"`
	AsOf         *time.Time `json:"as_of,omitempty"`
}

type DispositionAnswer struct {
	Key     string    `json:"key"`
	By      string    `json:"by"`
	EventID string    `json:"event_id"`
	BatchID string    `json:"batch_id,omitempty"`
	Gen     int64     `json:"gen"`
	At      time.Time `json:"at"`
}

func DispositionOptionLabel(key string) (string, bool) {
	for _, option := range DispositionOptionLabels {
		if option.Key == key {
			return option.Label, true
		}
	}
	return "", false
}

func dispositionDecisionOptions(recommended string) DecisionOptions {
	options := make([]DecisionOption, len(DispositionOptionLabels))
	copy(options, DispositionOptionLabels)
	for i := range options {
		options[i].Recommended = options[i].Key == recommended
	}
	return DecisionOptions{Options: options, AllowFree: false}
}

func validDisposition(refs TaskRefs) bool {
	d := refs.Disposition
	if d == nil || d.Schema != DispositionSchema {
		return false
	}
	if (refs.OriginPR == "") == (refs.OriginTask == 0) {
		return false
	}
	f := d.Facts
	switch f.FactsSource {
	case "gh-pr-view":
		if refs.OriginPR == "" || !mergeSHARE.MatchString(f.MergeSHA) || f.MergedAt == nil {
			return false
		}
	case "hk-task":
		if refs.OriginTask == 0 || f.MergeSHA != "" {
			return false
		}
	default:
		return false
	}
	if f.FactsAsOf.IsZero() || f.ResidualN < 0 || f.ResidualN > 10000 {
		return false
	}
	if f.ResidualDoc != "" && !validDocKey(f.ResidualDoc) {
		return false
	}
	if f.ResidualN > 0 && f.ResidualDoc == "" {
		return false
	}
	in := f.Install
	if !dispositionInstallStates[in.State] || in.TargetsPass < 0 || in.TargetsTotal < 0 || in.TargetsPass > in.TargetsTotal {
		return false
	}
	switch in.State {
	case "installed", "not_installed", "partial":
		if in.Witness == "" || !validDocKey(in.Witness) || in.TargetsTotal == 0 {
			return false
		}
		if in.State == "partial" && (in.TargetsPass == 0 || in.TargetsPass == in.TargetsTotal) {
			return false
		}
		if in.State == "installed" && in.TargetsPass != in.TargetsTotal {
			return false
		}
	default:
		if in.Witness != "" && !validDocKey(in.Witness) {
			return false
		}
	}
	if _, ok := DispositionOptionLabel(d.Recommended); !ok {
		return false
	}
	// The generic decision options must be exactly the closed set with the
	// same recommendation; the UI and inbox read them.
	if refs.DecisionOptions == nil || FormatDecisionOptions(*refs.DecisionOptions) != FormatDecisionOptions(dispositionDecisionOptions(d.Recommended)) {
		return false
	}
	if a := d.Answer; a != nil {
		if _, ok := DispositionOptionLabel(a.Key); !ok || !strings.HasPrefix(a.By, "operator:") || a.EventID == "" || a.Gen < 1 || a.At.IsZero() {
			return false
		}
		for _, v := range []string{a.By, a.EventID, a.BatchID} {
			if !validText(v, 256) {
				return false
			}
		}
	}
	return true
}

// DispositionInput is what the director supplies. Facts that hk can observe
// itself (the parent task for an origin_task) are read by the store.
type DispositionInput struct {
	Lane        string             `json:"lane"`
	Title       string             `json:"title"`
	OriginPR    string             `json:"origin_pr,omitempty"`
	OriginTask  int64              `json:"origin_task,omitempty"`
	MergeSHA    string             `json:"merge_sha,omitempty"`
	MergedAt    *time.Time         `json:"merged_at,omitempty"`
	Install     DispositionInstall `json:"install"`
	ResidualN   int                `json:"residual_n"`
	ResidualDoc string             `json:"residual_doc,omitempty"`
	Recommended string             `json:"recommended"`
	Note        string             `json:"note,omitempty"`
	CreatedBy   string             `json:"-"`
}

func dispositionOriginKey(originPR string, originTask int64) string {
	if originPR != "" {
		return "pr:" + originPR
	}
	return "task:" + strconv.FormatInt(originTask, 10)
}

// DispositionQuestion renders the deterministic question text from recorded
// facts. The optional director note follows on its own line.
func DispositionQuestion(refs TaskRefs, note string) string {
	d := refs.Disposition
	parts := []string{}
	if refs.OriginPR != "" {
		sha := d.Facts.MergeSHA
		if len(sha) > 7 {
			sha = sha[:7]
		}
		parts = append(parts, "처분: 출처 PR "+refs.OriginPR+" · merged "+sha)
	} else {
		parts = append(parts, "처분: 출처 태스크 #"+strconv.FormatInt(refs.OriginTask, 10))
	}
	install := "설치 " + d.Facts.Install.State
	if d.Facts.Install.TargetsTotal > 0 {
		install += fmt.Sprintf(" %d/%d", d.Facts.Install.TargetsPass, d.Facts.Install.TargetsTotal)
	}
	parts = append(parts, install)
	residual := fmt.Sprintf("잔여 %d", d.Facts.ResidualN)
	if d.Facts.ResidualDoc != "" {
		residual += " doc:" + d.Facts.ResidualDoc
	}
	parts = append(parts, residual)
	label, _ := DispositionOptionLabel(d.Recommended)
	parts = append(parts, "권고 "+d.Recommended+" "+label)
	text := strings.Join(parts, " · ")
	if strings.TrimSpace(note) != "" {
		text += "\n" + note
	}
	return text
}

func insertTaskEvent(ctx context.Context, tx pgx.Tx, id int64, from, to, by, note string, refs TaskRefs, at time.Time) (int64, error) {
	encoded, err := json.Marshal(refs)
	if err != nil {
		return 0, err
	}
	var eventID int64
	err = tx.QueryRow(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,refs,at) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7) RETURNING id`, id, from, to, by, note, string(encoded), at).Scan(&eventID)
	return eventID, err
}

// CreateDisposition creates at most one open (needs_decision) item per origin.
// The row passes backlog -> claimed -> needs_decision inside one transaction,
// so it is never observable in backlog and NextTask can never claim it.
// created=false returns the already open item for the same origin.
func (s *Store) CreateDisposition(ctx context.Context, in DispositionInput) (Task, bool, error) {
	if !validName(in.Lane) || strings.TrimSpace(in.Title) == "" || !validText(in.Title, MaxBytes) || in.CreatedBy == "" || !validText(in.CreatedBy, 128) || !validText(in.Note, 512) {
		return Task{}, false, errors.New("invalid disposition")
	}
	if err := guard.Reject(in.Title + "\n" + in.Note); err != nil {
		return Task{}, false, err
	}
	now := time.Now().UTC()
	refs := TaskRefs{OriginPR: in.OriginPR, OriginTask: in.OriginTask}
	facts := DispositionFacts{FactsAsOf: now, Install: in.Install, ResidualN: in.ResidualN, ResidualDoc: in.ResidualDoc}
	if in.OriginPR != "" {
		facts.FactsSource, facts.MergeSHA, facts.MergedAt = "gh-pr-view", in.MergeSHA, in.MergedAt
	} else {
		facts.FactsSource = "hk-task"
	}
	refs.Disposition = &Disposition{Schema: DispositionSchema, Facts: facts, Recommended: in.Recommended}
	options := dispositionDecisionOptions(in.Recommended)
	refs.DecisionOptions = &options
	if !validTaskRefs(refs) {
		return Task{}, false, errors.New("invalid disposition")
	}
	if err := rejectTaskRefs(refs); err != nil {
		return Task{}, false, err
	}
	question := DispositionQuestion(refs, in.Note)
	if len([]byte(question+"\n"+FormatDecisionOptions(options))) > RelayLaneEventMaxBytes {
		return Task{}, false, errors.New("disposition question and options exceed 2048 bytes")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "disposition:"+dispositionOriginKey(in.OriginPR, in.OriginTask)); err != nil {
		return Task{}, false, err
	}
	if in.OriginTask != 0 {
		var parentState string
		if err = tx.QueryRow(ctx, `SELECT state FROM tasks WHERE id=$1`, in.OriginTask).Scan(&parentState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return Task{}, false, ErrTaskNotFound
			}
			return Task{}, false, err
		}
		if parentState != "merged" && parentState != "dropped" {
			return Task{}, false, errors.New("invalid disposition: origin task is not terminal")
		}
	}
	var existing Task
	err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE state='needs_decision' AND refs ? 'disposition'
		AND (($1 <> '' AND refs->>'origin_pr' = $1) OR ($2 > 0 AND refs->>'origin_task' = $2::text)) ORDER BY id LIMIT 1`, in.OriginPR, in.OriginTask), &existing)
	if err == nil {
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Task{}, false, err
	}
	encoded, err := json.Marshal(refs)
	if err != nil {
		return Task{}, false, err
	}
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `INSERT INTO tasks(lane,parent_lane,title,kind,state,priority,refs,claimed_by,created_by,created_at,updated_at) VALUES($1,'',$2,'decide','needs_decision',0,$3::jsonb,$4,$4,$5,$5) RETURNING `+taskColumns, in.Lane, in.Title, string(encoded), in.CreatedBy, now), &x); err != nil {
		return Task{}, false, err
	}
	// The legal path is recorded explicitly so the event history never shows
	// an edge the transition table does not allow.
	if _, err = insertTaskEvent(ctx, tx, x.ID, "backlog", "claimed", in.CreatedBy, "disposition:create", refs, now); err != nil {
		return Task{}, false, err
	}
	if _, err = insertTaskEvent(ctx, tx, x.ID, "claimed", "needs_decision", in.CreatedBy, question, refs, now); err != nil {
		return Task{}, false, err
	}
	return x, true, tx.Commit(ctx)
}

func dispositionGen(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id int64) (int64, error) {
	var gen int64
	err := q.QueryRow(ctx, `SELECT id FROM task_events WHERE task_id=$1 AND "to"='needs_decision' ORDER BY at DESC,id DESC LIMIT 1`, id).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return gen, err
}

// DispositionAnswerInput identifies one operator answer. OperatorEmail must be
// the Cloudflare Access email identity; the caller (internal/ui) refuses
// service identities before reaching this method.
type DispositionAnswerInput struct {
	ID            int64
	Gen           int64
	Key           string
	OperatorEmail string
	EventID       string
	BatchID       string
}

// answerDispositionTx returns (task, skipReason, error). A non-empty skip
// reason means nothing was written for this item.
func answerDispositionTx(ctx context.Context, tx pgx.Tx, in DispositionAnswerInput, useRecommended bool) (Task, string, error) {
	var x Task
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, in.ID), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, "not_found", nil
		}
		return Task{}, "", err
	}
	if x.Refs.Disposition == nil {
		return x, "not_disposition", nil
	}
	if x.State != "needs_decision" {
		return x, "already_answered", nil
	}
	gen, err := dispositionGen(ctx, tx, in.ID)
	if err != nil {
		return Task{}, "", err
	}
	if gen != in.Gen {
		return x, "stale", nil
	}
	key := in.Key
	if useRecommended {
		key = x.Refs.Disposition.Recommended
	}
	label, ok := DispositionOptionLabel(key)
	if !ok {
		return x, "invalid_key", nil
	}
	now := time.Now().UTC()
	x.Refs.Disposition.Answer = &DispositionAnswer{Key: key, By: "operator:" + in.OperatorEmail, EventID: in.EventID, BatchID: in.BatchID, Gen: gen, At: now}
	encoded, err := json.Marshal(x.Refs)
	if err != nil {
		return Task{}, "", err
	}
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET state='claimed',refs=$2::jsonb,updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, in.ID, string(encoded), now), &x); err != nil {
		return Task{}, "", err
	}
	note := fmt.Sprintf("disposition-answer %s: %s gen=%d", key, label, gen)
	if in.BatchID != "" {
		note += " batch=" + in.BatchID
	}
	if _, err = insertTaskEvent(ctx, tx, in.ID, "needs_decision", "claimed", "operator:"+in.OperatorEmail, note, x.Refs, now); err != nil {
		return Task{}, "", err
	}
	return x, "", nil
}

func validAnswerIdentity(email, eventID, batchID string) bool {
	return operatorEmailRE.MatchString(email) && validText(email, 256) && eventID != "" && validText(eventID, 256) && validText(batchID, 256)
}

// AnswerDisposition records one operator answer. It is the only single-item
// path out of needs_decision for a disposition item.
func (s *Store) AnswerDisposition(ctx context.Context, in DispositionAnswerInput) (Task, error) {
	if in.ID < 1 || in.Gen < 1 || !validAnswerIdentity(in.OperatorEmail, in.EventID, in.BatchID) {
		return Task{}, errors.New("invalid disposition answer")
	}
	if _, ok := DispositionOptionLabel(in.Key); !ok {
		return Task{}, errors.New("invalid disposition answer")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
	x, skip, err := answerDispositionTx(ctx, tx, in, false)
	if err != nil {
		return Task{}, err
	}
	switch skip {
	case "":
	case "not_found":
		return Task{}, ErrTaskNotFound
	case "stale":
		return Task{}, ErrDispositionStale
	default:
		return Task{}, ErrTaskConflict
	}
	return x, tx.Commit(ctx)
}

type DispositionRef struct {
	ID  int64 `json:"id"`
	Gen int64 `json:"gen"`
}

type DispositionSkip struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// AnswerDispositionBatch accepts the recommended answer for exactly the items
// in refs (the server-signed snapshot) inside one transaction. Items created
// after the snapshot are not in refs and therefore cannot be touched; items
// whose question generation changed are skipped, never answered.
func (s *Store) AnswerDispositionBatch(ctx context.Context, refs []DispositionRef, email, batchID string, eventIDForLane func(lane string) string) ([]Task, []DispositionSkip, error) {
	if len(refs) == 0 || len(refs) > DispositionBatchLimit || batchID == "" || !validAnswerIdentity(email, "-", batchID) || eventIDForLane == nil {
		return nil, nil, errors.New("invalid disposition batch")
	}
	ordered := append([]DispositionRef(nil), refs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for i := range ordered {
		if ordered[i].ID < 1 || ordered[i].Gen < 1 || (i > 0 && ordered[i].ID == ordered[i-1].ID) {
			return nil, nil, errors.New("invalid disposition batch")
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	answered, skipped := []Task{}, []DispositionSkip{}
	for _, ref := range ordered {
		var lane string
		if err := tx.QueryRow(ctx, `SELECT lane FROM tasks WHERE id=$1`, ref.ID).Scan(&lane); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				skipped = append(skipped, DispositionSkip{ID: ref.ID, Reason: "not_found"})
				continue
			}
			return nil, nil, err
		}
		x, skip, err := answerDispositionTx(ctx, tx, DispositionAnswerInput{ID: ref.ID, Gen: ref.Gen, OperatorEmail: email, EventID: eventIDForLane(lane), BatchID: batchID}, true)
		if err != nil {
			return nil, nil, err
		}
		if skip != "" {
			skipped = append(skipped, DispositionSkip{ID: ref.ID, Reason: skip})
			continue
		}
		answered = append(answered, x)
	}
	return answered, skipped, tx.Commit(ctx)
}

// ApplyDisposition records that the director applied the operator's answer.
// It uses only legal transition-table edges, in one transaction:
// A/B/C claimed->in_progress->join->merged, D claimed->hold, E claimed->dropped.
func (s *Store) ApplyDisposition(ctx context.Context, id int64, by, note string) (Task, error) {
	if id < 1 || by == "" || !validText(by, 128) || !validText(note, 512) {
		return Task{}, errors.New("invalid disposition apply")
	}
	if err := guard.Reject(note); err != nil {
		return Task{}, err
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
	if x.Refs.Disposition == nil || x.Refs.Disposition.Answer == nil || x.State != "claimed" {
		return Task{}, ErrTaskConflict
	}
	var hops []string
	switch x.Refs.Disposition.Answer.Key {
	case "A", "B", "C":
		hops = []string{"in_progress", "join", "merged"}
	case "D":
		hops = []string{"hold"}
	default:
		hops = []string{"dropped"}
	}
	text := "disposition:apply " + x.Refs.Disposition.Answer.Key
	if strings.TrimSpace(note) != "" {
		text += " — " + note
	}
	now := time.Now().UTC()
	from := x.State
	for _, to := range hops {
		if !taskTransitionAllowed(from, to) {
			return Task{}, ErrTaskConflict
		}
		if _, err = insertTaskEvent(ctx, tx, id, from, to, by, text, x.Refs, now); err != nil {
			return Task{}, err
		}
		from = to
	}
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET state=$2,updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, id, from, now), &x); err != nil {
		return Task{}, err
	}
	return x, tx.Commit(ctx)
}

type OpenDisposition struct {
	Task     Task   `json:"task"`
	Gen      int64  `json:"gen"`
	Question string `json:"question"`
}

// ListOpenDispositions returns open items oldest first (the batch order).
func (s *Store) ListOpenDispositions(ctx context.Context, limit int) ([]OpenDisposition, error) {
	if limit < 1 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixedTaskColumns("t")+`,
		COALESCE((SELECT id FROM task_events WHERE task_id=t.id AND "to"='needs_decision' ORDER BY at DESC,id DESC LIMIT 1),0),
		COALESCE((SELECT note FROM task_events WHERE task_id=t.id AND "to"='needs_decision' ORDER BY at DESC,id DESC LIMIT 1),'')
		FROM tasks t WHERE t.state='needs_decision' AND t.refs ? 'disposition' ORDER BY t.created_at ASC,t.id ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OpenDisposition{}
	for rows.Next() {
		var x OpenDisposition
		var refs []byte
		if err := rows.Scan(&x.Task.ID, &x.Task.Lane, &x.Task.ParentLane, &x.Task.Title, &x.Task.Kind, &x.Task.State, &x.Task.Priority, &refs, &x.Task.ClaimedBy, &x.Task.CreatedBy, &x.Task.CreatedAt, &x.Task.UpdatedAt, &x.Gen, &x.Question); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(refs, &x.Task.Refs); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func prefixedTaskColumns(alias string) string {
	cols := strings.Split(taskColumns, ",")
	for i := range cols {
		cols[i] = alias + "." + cols[i]
	}
	return strings.Join(cols, ",")
}

// CountNeedsDecision splits needs_decision tasks, in one statement, into
// generic decisions and disposition items. The two are reported side by side,
// never summed.
func (s *Store) CountNeedsDecision(ctx context.Context) (decisions, dispositions int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE NOT (refs ? 'disposition')), COUNT(*) FILTER (WHERE refs ? 'disposition') FROM tasks WHERE state='needs_decision'`).Scan(&decisions, &dispositions)
	return decisions, dispositions, err
}

// ListUnnotifiedDispositions returns answered items whose answer event has not
// reached relay_events, newest first so a bounded list never hides the latest. The answer is the durable record; the lane event is a
// notification that can be re-sent with the same event ID.
func (s *Store) ListUnnotifiedDispositions(ctx context.Context, limit int) ([]Task, error) {
	if limit < 1 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixedTaskColumns("t")+` FROM tasks t
		WHERE t.refs ? 'disposition' AND t.refs->'disposition' ? 'answer'
		AND NOT EXISTS (SELECT 1 FROM relay_events r WHERE r.kind='lane.event' AND r.owner_lane=t.lane AND r.event_id=t.refs->'disposition'->'answer'->>'event_id')
		ORDER BY t.id DESC LIMIT $1`, limit)
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

// ListDispositionsByAnswerEvent returns the answered items that share one
// answer event ID (one item, or one lane of one batch), in id order.
func (s *Store) ListDispositionsByAnswerEvent(ctx context.Context, eventID string) ([]Task, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE refs ? 'disposition' AND refs->'disposition'->'answer'->>'event_id'=$1 ORDER BY id ASC LIMIT $2`, eventID, DispositionBatchLimit)
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

// DispositionSummary is the single definition behind the digest header, the
// CLI and the console. Every count is reconstructed from append-only
// task_events at AsOf, so a pasted header can be re-checked later.
type DispositionSummary struct {
	AsOf                  time.Time  `json:"as_of"`
	Open                  int        `json:"open"`
	OldestOpenDays        int        `json:"oldest_open_days"`
	OldestOpenID          int64      `json:"oldest_open_id,omitempty"`
	OldestOpenSince       *time.Time `json:"oldest_open_since,omitempty"`
	NextBatch             int        `json:"next_batch"`
	PendingApply          int        `json:"pending_apply"`
	OldestPendingApplyDay int        `json:"oldest_pending_apply_days"`
	Held                  int        `json:"held"`
	CoverageSupported     bool       `json:"coverage_supported"`
	CoverageEpoch         *time.Time `json:"coverage_epoch,omitempty"`
	MergedWithoutItem     int        `json:"merged_without_item"`
	BatchAnswers24h       int        `json:"batch_answers_24h"`
	Batches24h            int        `json:"batches_24h"`
	SingleAnswers24h      int        `json:"single_answers_24h"`
	Line                  string     `json:"line"`
	Detail                string     `json:"detail"`
}

func (s *Store) DispositionSummary(ctx context.Context, asOf time.Time) (DispositionSummary, error) {
	asOf = asOf.UTC()
	out := DispositionSummary{AsOf: asOf, OldestOpenDays: -1, OldestPendingApplyDay: -1}
	rows, err := s.pool.Query(ctx, `SELECT t.id, t.created_at, t.refs,
		(SELECT e."to" FROM task_events e WHERE e.task_id=t.id AND e.at<=$1 ORDER BY e.at DESC, e.id DESC LIMIT 1),
		(SELECT e.at FROM task_events e WHERE e.task_id=t.id AND e.at<=$1 ORDER BY e.at DESC, e.id DESC LIMIT 1)
		FROM tasks t WHERE t.refs ? 'disposition' AND t.created_at<=$1 ORDER BY t.created_at ASC, t.id ASC`, asOf)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	batches := map[string]bool{}
	for rows.Next() {
		var id int64
		var created time.Time
		var rawRefs []byte
		var state *string
		var at *time.Time
		if err := rows.Scan(&id, &created, &rawRefs, &state, &at); err != nil {
			return out, err
		}
		if out.CoverageEpoch == nil {
			epoch := created
			out.CoverageEpoch = &epoch
		}
		if state == nil {
			continue
		}
		switch *state {
		case "needs_decision":
			out.Open++
			if days := int(asOf.Sub(created) / (24 * time.Hour)); days > out.OldestOpenDays || (days == out.OldestOpenDays && created.Before(*out.OldestOpenSince)) {
				out.OldestOpenDays, out.OldestOpenID, out.OldestOpenSince = days, id, &created
			}
		case "claimed", "in_progress", "join":
			out.PendingApply++
			if days := int(asOf.Sub(*at) / (24 * time.Hour)); days > out.OldestPendingApplyDay {
				out.OldestPendingApplyDay = days
			}
		case "hold":
			out.Held++
		}
		var refs TaskRefs
		if err := json.Unmarshal(rawRefs, &refs); err != nil {
			return out, err
		}
		if refs.Disposition != nil && refs.Disposition.Answer != nil {
			a := refs.Disposition.Answer
			if !a.At.After(asOf) && asOf.Sub(a.At) <= 24*time.Hour {
				if a.BatchID != "" {
					out.BatchAnswers24h++
					batches[a.BatchID] = true
				} else {
					out.SingleAnswers24h++
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.Batches24h = len(batches)
	if out.Open > DispositionBatchLimit {
		out.NextBatch = out.Open - DispositionBatchLimit
	}
	if out.CoverageEpoch != nil {
		out.CoverageSupported = true
		// A merged PR since the contract epoch with no item naming it as
		// origin_pr. refs.pr still mixes origin and closing PRs (#494), so this
		// is a candidate count, not a verdict.
		if err := s.pool.QueryRow(ctx, `SELECT COUNT(DISTINCT t.refs->>'pr') FROM tasks t
			WHERE COALESCE(t.refs->>'pr','') <> '' AND NOT (t.refs ? 'disposition')
			AND EXISTS (SELECT 1 FROM task_events e WHERE e.task_id=t.id AND e."to"='merged' AND e.at >= $1 AND e.at <= $2)
			AND NOT EXISTS (SELECT 1 FROM tasks d WHERE d.refs ? 'disposition' AND d.refs->>'origin_pr' = t.refs->>'pr' AND d.created_at <= $2)`,
			*out.CoverageEpoch, asOf).Scan(&out.MergedWithoutItem); err != nil {
			return out, err
		}
	}
	out.Line, out.Detail = dispositionSummaryText(out)
	return out, nil
}

func dayText(days int) string {
	if days < 0 {
		return "—"
	}
	return strconv.Itoa(days) + "일"
}

func dispositionSummaryText(x DispositionSummary) (string, string) {
	line := fmt.Sprintf("미처분 %d · 최고령 %s", x.Open, dayText(x.OldestOpenDays))
	if x.NextBatch > 0 {
		line += fmt.Sprintf(" · 다음 묶음 %d건", x.NextBatch)
	}
	coverage := "집계 불가(시행 전)"
	if x.CoverageSupported {
		coverage = strconv.Itoa(x.MergedWithoutItem)
	}
	detail := fmt.Sprintf("적용 대기 %d(최고령 %s) · 보류 %d · 처분 항목 없는 머지(후보) %s · 최근 24h 일괄 %d건(%d회) · 개별 %d건 · as_of %s",
		x.PendingApply, dayText(x.OldestPendingApplyDay), x.Held, coverage, x.BatchAnswers24h, x.Batches24h, x.SingleAnswers24h, x.AsOf.Format(time.RFC3339))
	return line, detail
}
