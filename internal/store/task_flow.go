package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

var (
	// ErrTriageGate rejects a move out of triage that lacks a priority above
	// zero, a project or a kind.
	ErrTriageGate = errors.New("triage_gate")
	// ErrDuplicateOfRequired rejects entering duplicate without a duplicate_of
	// relation: the state and the relation exist together or not at all.
	ErrDuplicateOfRequired = errors.New("duplicate_of_required")
	// ErrTaskParentCycle rejects a parent link that would make a task its own
	// ancestor.
	ErrTaskParentCycle = errors.New("parent_cycle")
	// ErrTaskParentInvalid rejects a parent that does not exist or is the task
	// itself.
	ErrTaskParentInvalid = errors.New("parent_invalid")
	// ErrRelationCycle rejects a blocks or duplicate_of edge that would close a
	// cycle.
	ErrRelationCycle = errors.New("relation_cycle")
)

// taskEventInput is one task_events row. Kind defaults to transition and
// Applied to true; NotApplied records an event with no effect.
type taskEventInput struct {
	TaskID     int64
	Kind       string
	From, To   string
	By, Note   string
	Refs       TaskRefs
	At         time.Time
	NoJob      string
	Source     string
	SourceRef  string
	Field      string
	FromValue  string
	ToValue    string
	Auto       bool
	NotApplied bool
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// insertEvent appends a task event. A row with a source and source_ref that
// already exists changes nothing (inserted=false): a redelivered event is a
// no-op.
func insertEvent(ctx context.Context, q rowQuerier, e taskEventInput) (id int64, inserted bool, err error) {
	if e.Kind == "" {
		e.Kind = TaskEventTransition
	}
	encoded, err := json.Marshal(e.Refs)
	if err != nil {
		return 0, false, err
	}
	err = q.QueryRow(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,refs,at,kind,no_job,source,source_ref,field,from_value,to_value,auto,applied)
		VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (task_id,source,source_ref) WHERE source<>'' AND source_ref<>'' DO NOTHING RETURNING id`,
		e.TaskID, e.From, e.To, e.By, e.Note, string(encoded), e.At, e.Kind, e.NoJob, e.Source, e.SourceRef, e.Field, e.FromValue, e.ToValue, e.Auto, !e.NotApplied).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// applyLifecycle sets the lifecycle timestamps for a move from one state to
// another. started_at keeps its first value; completed_at and canceled_at
// describe the current state, so leaving the category clears them.
func applyLifecycle(x *Task, from, to string, now time.Time) {
	switch TaskCategory(to) {
	case CategoryStarted:
		if x.StartedAt == nil {
			t := now
			x.StartedAt = &t
		}
		x.CompletedAt, x.CanceledAt = nil, nil
	case CategoryCompleted:
		if x.CompletedAt == nil || TaskCategory(from) != CategoryCompleted {
			t := now
			x.CompletedAt = &t
		}
		x.CanceledAt = nil
	case CategoryCanceled:
		if x.CanceledAt == nil || TaskCategory(from) != CategoryCanceled {
			t := now
			x.CanceledAt = &t
		}
		x.CompletedAt = nil
	default:
		x.CompletedAt, x.CanceledAt = nil, nil
	}
	if from == StateTriage && to != StateTriage {
		t := now
		x.TriagedAt = &t
	}
	if to == StateTriage && from != StateTriage {
		t := now
		x.StartedTriageAt = &t
	}
}

// writeTaskTx stores the mutable state of x: state, refs, claimant, the
// awaiting flag and the lifecycle timestamps.
func writeTaskTx(ctx context.Context, q rowQuerier, x Task) (Task, error) {
	encoded, err := json.Marshal(x.Refs)
	if err != nil {
		return Task{}, err
	}
	var out Task
	err = scanTask(q.QueryRow(ctx, `UPDATE tasks SET state=$2,refs=$3::jsonb,updated_at=$4,claimed_by=$5,awaiting_decision=$6,started_at=$7,completed_at=$8,canceled_at=$9,triaged_at=$10,started_triage_at=$11 WHERE id=$1 RETURNING `+taskColumns,
		x.ID, x.State, string(encoded), x.UpdatedAt, x.ClaimedBy, x.AwaitingDecision, x.StartedAt, x.CompletedAt, x.CanceledAt, x.TriagedAt, x.StartedTriageAt), &out)
	return out, err
}

// claimTaskTx claims a backlog or unstarted task. The sub-state comes from
// the task kind (the spawn rule); a job record can move it later. The guard
// for entering started holds as before: a claim carries job_id or a recorded
// no-job reason.
func (s *Store) claimTaskTx(ctx context.Context, tx pgx.Tx, id int64, by, jobID, noJob string) (Task, error) {
	if !validText(by, 128) || by == "" {
		return Task{}, errors.New("invalid task claimant")
	}
	if !validText(jobID, 4096) {
		return Task{}, errors.New("invalid task claim job_id")
	}
	noJob = taskReasonText(noJob)
	if !validText(noJob, MaxBytes) {
		return Task{}, errors.New("invalid task claim no_job")
	}
	if jobID != "" && noJob != "" {
		return Task{}, errors.New("invalid task claim: job_id and no_job are exclusive")
	}
	if err := guard.Reject(jobID); err != nil {
		return Task{}, err
	}
	if err := guard.Reject(noJob); err != nil {
		return Task{}, err
	}
	var x Task
	var hasDisposition bool
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+`,refs ? 'disposition' FROM tasks WHERE id=$1 FOR UPDATE`, id), &x, &hasDisposition); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, ErrTaskConflict
		}
		return Task{}, err
	}
	// Match NextTask's key-presence exclusion, including malformed/null refs.
	if hasDisposition {
		return Task{}, fmt.Errorf("%w: disposition items cannot be claimed directly", ErrTaskConflict)
	}

	if err := s.wrkTaskFenceTx(ctx, tx, x, "", true); err != nil {
		return Task{}, err
	}
	if err := s.wrkFence(ctx, tx, wrkrecords.LegacyIntent{JobID: jobID, Binding: true}); err != nil {
		return Task{}, err
	}
	if !IsClaimableState(x.State) || x.AwaitingDecision {
		// A replayed claim — same claimant, same non-empty job id — is a
		// no-op: the row is returned as recorded and no second event is
		// written. A jobless claim replays only for the same claimant on a
		// row still carrying no job_id, and only when the caller again
		// supplies its no-job reason. Every other claim of a task that is
		// not claimable is a conflict, with the reason wrapped on the
		// sentinel so HTTP callers can surface it.
		replay := !x.AwaitingDecision && x.ClaimedBy == by && x.Refs.JobID == jobID && (jobID != "" || noJob != "")
		if replay {
			return x, nil
		}
		return Task{}, claimConflict(x, by, jobID)
	}
	// A claim lands the task in a started state, so it must establish the
	// task/job link itself: either job_id binds the claimant's job or a
	// recorded no-job reason marks the deliberate exception. Bare claims stay
	// refused so started rows always carry accountable linkage.
	if jobID == "" && noJob == "" {
		return Task{}, fmt.Errorf("%w: claim requires job_id or a no-job reason", ErrTaskJobRequired)
	}
	from := x.State
	to := substateForKind(x.Kind)
	now := time.Now().UTC()
	x.State, x.ClaimedBy, x.UpdatedAt = to, by, now
	if jobID != "" {
		x.Refs.JobID = jobID
	}
	applyLifecycle(&x, from, to, now)
	x, err := writeTaskTx(ctx, tx, x)
	if err != nil {
		return Task{}, err
	}
	if _, _, err := insertEvent(ctx, tx, taskEventInput{TaskID: id, From: from, To: to, By: by, Refs: x.Refs, At: now, NoJob: noJob}); err != nil {
		return Task{}, err
	}
	if s.LinearSyncEnabled() && x.Refs.Linear != nil && x.Refs.Linear.Sync {
		if err := enqueueLinearTaskTransition(ctx, tx, x, ""); err != nil {
			return Task{}, err
		}
	}
	if err := s.recordLinearBoardTaskTx(ctx, tx, x, ""); err != nil {
		return Task{}, err
	}
	return x, nil
}

// claimConflict names why a non-claimable task refused a claim. The sentinel
// stays ErrTaskConflict so errors.Is and the HTTP 409 mapping are unchanged;
// the wrapped reason is what the API copies into the response body.
func claimConflict(x Task, by, jobID string) error {
	switch {
	case x.AwaitingDecision:
		return fmt.Errorf("%w: task is waiting on a decision", ErrTaskConflict)
	case x.ClaimedBy == "":
		return fmt.Errorf("%w: task state %s cannot be claimed", ErrTaskConflict, x.State)
	case x.ClaimedBy != by:
		return fmt.Errorf("%w: already claimed by %s", ErrTaskConflict, x.ClaimedBy)
	case jobID != "" && x.Refs.JobID != jobID:
		if x.Refs.JobID == "" {
			return fmt.Errorf("%w: already claimed without job_id", ErrTaskConflict)
		}
		return fmt.Errorf("%w: already claimed with job_id %s", ErrTaskConflict, x.Refs.JobID)
	case IsStartedState(x.State):
		return fmt.Errorf("%w: already claimed", ErrTaskConflict)
	default:
		return fmt.Errorf("%w: task state %s cannot be claimed", ErrTaskConflict, x.State)
	}
}

// TransitionInput is one manual transition. To may be a current state or an
// old name (see ParseStateFilter's sibling resolveTransitionTarget).
// DuplicateOf names the task a duplicate transition points at; it is required
// when the task has no live duplicate_of relation yet.
type TransitionInput struct {
	To          string
	By          string
	Note        string
	Refs        *TaskRefs
	NoJob       string
	DuplicateOf int64
	// ClearOnly turns the transition into clearing the awaiting_decision flag:
	// it is refused on a task that is not flagged, and the state is unchanged.
	ClearOnly bool
}

// TransitionTask is TransitionTaskWith without a duplicate_of target.
func (s *Store) TransitionTask(ctx context.Context, id int64, to, by, note string, refs *TaskRefs, noJob string) (Task, error) {
	return s.TransitionTaskWith(ctx, id, TransitionInput{To: to, By: by, Note: note, Refs: refs, NoJob: noJob})
}

// ClearAwaitingDecision answers a flagged task: it clears awaiting_decision
// and leaves the work state alone. A task that is not waiting is a conflict.
// Existing answer paths keep their guards: an open structured request must be
// resolved first, and only the operator's web answer resolves a disposition
// item.
func (s *Store) ClearAwaitingDecision(ctx context.Context, id int64, by, note string) (Task, error) {
	return s.transitionTask(ctx, id, TransitionInput{To: legacyClaimed, By: by, Note: note, ClearOnly: true}, false)
}

// ReopenTask is the only exit from a settled state (merged, deployed, dropped
// or duplicate) and the way icebox returns to unstarted. The reason is
// persisted as the note of an append-only transition event in the same
// transaction.
func (s *Store) ReopenTask(ctx context.Context, id int64, by, reason string) (Task, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Task{}, errors.New("reopen requires a non-empty reason")
	}
	return s.transitionTask(ctx, id, TransitionInput{To: StateUnstarted, By: by, Note: reason}, true)
}

// TransitionTaskWith applies one manual transition. A settled task leaves
// only through ReopenTask.
func (s *Store) TransitionTaskWith(ctx context.Context, id int64, in TransitionInput) (Task, error) {
	if in.To == legacyReopened {
		return s.ReopenTask(ctx, id, in.By, in.Note)
	}
	return s.transitionTask(ctx, id, in, false)
}

type transitionAction int

const (
	actMove transitionAction = iota
	actSetFlag
	actClearFlag
	actNoop
)

// resolveTransitionTarget turns the requested name into an action and a
// target state, following "Old names on input" in the design.
func resolveTransitionTarget(x Task, to string) (transitionAction, string, error) {
	switch to {
	case legacyNeedsDecision:
		return actSetFlag, x.State, nil
	case legacyClaimed, legacyInProgress:
		switch {
		case x.AwaitingDecision:
			return actClearFlag, x.State, nil
		case to == legacyInProgress && x.State == StateReviewing:
			return actMove, StateDeveloping, nil
		case IsStartedState(x.State):
			return actNoop, x.State, nil
		}
		return actMove, substateForKind(x.Kind), nil
	case legacyVerifying, legacyJoin:
		return actMove, StateReviewing, nil
	case legacyHold:
		return actMove, StateIcebox, nil
	}
	if !taskStates[to] {
		return actMove, "", errors.New("invalid task transition")
	}
	return actMove, to, nil
}

// fenceTarget is the target the wrk receipt fence judges. The fence predates
// the new names: it asks for a joined receipt on join and merged and an end
// receipt on dropped, so a caller that sent the old name keeps its receipt
// requirement and a duplicate counts as dropped.
func fenceTarget(requested, canonical string) string {
	if requested == legacyJoin {
		return legacyJoin
	}
	return FenceTarget(canonical)
}

// FenceTarget maps a requested state name to the target the wrk receipt fence
// understands: a duplicate counts as dropped and reviewing as verifying, so a
// new name carries the same receipt requirement its old name did.
func FenceTarget(to string) string {
	switch to {
	case StateDuplicate:
		return StateDropped
	case StateReviewing:
		return legacyVerifying
	}
	return to
}

func (s *Store) transitionTask(ctx context.Context, id int64, in TransitionInput, reopen bool) (Task, error) {
	noJob := taskReasonText(in.NoJob)
	by, note, refs := in.By, in.Note, in.Refs
	if in.To == "" || !validText(by, 128) || by == "" || !validText(note, MaxBytes) || !validText(noJob, MaxBytes) || (refs != nil && !validTaskRefs(*refs)) || in.DuplicateOf < 0 {
		return Task{}, errors.New("invalid task transition")
	}
	if err := guard.Reject(note); err != nil {
		return Task{}, err
	}
	if err := guard.Reject(noJob); err != nil {
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
		if refs.DecisionRequest != nil {
			return Task{}, errors.New("invalid task transition: decision_request is written by tasks decision-request only")
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
	var hasDisposition bool
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+`,refs ? 'disposition' FROM tasks WHERE id=$1 FOR UPDATE`, id), &x, &hasDisposition); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, ErrTaskNotFound
		}
		return Task{}, err
	}
	action, to, err := resolveTransitionTarget(x, in.To)
	if err != nil {
		return Task{}, err
	}
	if reopen {
		action, to = actMove, StateUnstarted
	}
	if in.ClearOnly {
		if !x.AwaitingDecision {
			return Task{}, ErrTaskConflict
		}
		action, to = actClearFlag, x.State
	}

	if err := s.wrkTaskFenceTx(ctx, tx, x, fenceTarget(in.To, to), refs != nil || reopen); err != nil {
		return Task{}, err
	}
	if refs != nil && refs.JobID != "" {
		if err := s.wrkFence(ctx, tx, wrkrecords.LegacyIntent{JobID: refs.JobID, Binding: true}); err != nil {
			return Task{}, err
		}
	}
	from := x.State
	if action == actNoop {
		return x, tx.Commit(ctx)
	}
	if reopen && !(IsSettledState(from) || from == StateIcebox) {
		return Task{}, ErrTaskConflict
	}
	if IsSettledState(from) && !reopen {
		return Task{}, ErrTaskConflict
	}
	if reopen && strings.TrimSpace(note) == "" {
		return Task{}, errors.New("reopen requires a non-empty reason")
	}
	if in.DuplicateOf != 0 && (action != actMove || to != StateDuplicate) {
		return Task{}, errors.New("invalid task transition: duplicate_of applies only to a duplicate transition")
	}
	// Placed after the row lock and before the transition-table check so every
	// generic caller (API, MCP, CLI, UI generic decision forms) is refused while
	// a disposition item awaits the operator, whatever edge it asks for. The
	// operator's answer uses AnswerDisposition, which does not come through here.
	if hasDisposition {
		if reopen {
			return Task{}, fmt.Errorf("%w: disposition items cannot be reopened", ErrTaskConflict)
		}
		if x.AwaitingDecision {
			return Task{}, ErrDispositionOperatorOnly
		}
		// A disposition item never returns to backlog: its closed options and
		// its origin are fixed at creation, in every state: re-pointing
		// origin_pr/origin_task would detach the recorded facts from their
		// source and let one origin own two items over time.
		if (action == actMove && to == StateBacklog) || (refs != nil && (refs.DecisionOptions != nil || refs.OriginPR != "" || refs.OriginTask != 0)) {
			return Task{}, ErrTaskConflict
		}
	}

	switch action {
	case actSetFlag:
		if x.AwaitingDecision || IsSettledState(from) {
			return Task{}, ErrTaskConflict
		}
	case actClearFlag:
	default:
		if !taskTransitionAllowed(from, to) {
			return Task{}, ErrTaskConflict
		}
		// A task waiting on a decision leaves only the way the old
		// needs_decision state did: back to backlog, parked, or dropped.
		if x.AwaitingDecision {
			switch to {
			case StateBacklog, StateIcebox, StateDropped, StateDuplicate:
			default:
				return Task{}, ErrTaskConflict
			}
		}
	}
	// Once a task carries a structured request, its question and choices
	// change only through a new request revision: a generic needs_decision
	// question or option patch would pair new text with the recorded
	// request's id (or an old request's choices with a new question).
	if x.Refs.DecisionRequest != nil && (action == actSetFlag || (refs != nil && refs.DecisionOptions != nil)) {
		if x.Refs.DecisionRequest.Status == DecisionRequestOpen {
			return Task{}, ErrDecisionRequestOpen
		}
		return Task{}, invalidDecisionRequest("this task's decisions are recorded with tasks decision-request; record a new request instead")
	}
	// Answering a flagged task resumes it. Checked here, under the row lock,
	// so a request recorded after a caller's pre-check still cannot be left
	// open behind a resumed task: resolve it with tasks decision-resolve
	// first. Parking, dropping and returning to backlog stay allowed.
	if action == actClearFlag && x.Refs.DecisionRequest != nil && x.Refs.DecisionRequest.Status == DecisionRequestOpen {
		return Task{}, ErrDecisionRequestOpen
	}
	if action == actSetFlag && strings.TrimSpace(note) == "" {
		return Task{}, errors.New("needs_decision requires question")
	}
	// The triage gate: a task leaves triage for backlog or unstarted only with
	// a priority above 0, a project and a kind; declining needs a reason.
	if action == actMove && from == StateTriage {
		switch to {
		case StateBacklog, StateUnstarted:
			if x.Priority <= 0 || x.Project == nil || *x.Project == "" || !taskKinds[x.Kind] {
				return Task{}, fmt.Errorf("%w: leaving triage needs a priority above 0, a project and a kind", ErrTriageGate)
			}
		default:
			if strings.TrimSpace(note) == "" {
				return Task{}, errors.New("declining a triage task requires a reason")
			}
		}
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
	if x.Refs.Disposition != nil && action == actSetFlag {
		x.Refs.Disposition.Answer = nil
	}
	if action == actSetFlag && x.Refs.DecisionOptions != nil {
		finalText := note + "\n" + FormatDecisionOptions(*x.Refs.DecisionOptions)
		if len([]byte(finalText)) > RelayLaneEventMaxBytes {
			return Task{}, errors.New("decision question and options exceed 2048 bytes")
		}
	}
	// The task/job-linkage guard (decision/2026-09-27/task-job-linkage item
	// 2): entering a started state without both claimed_by and refs.job_id is
	// refused unless the caller records a no-job reason, which the event below
	// persists. Runs under the row lock, after the refs merge, so a concurrent
	// claim cannot interleave between the check and the write. Moves within
	// started, and out of it, are free — the guard only watches the entry.
	recordedNoJob := ""
	if action == actMove && IsStartedState(to) && !IsStartedState(from) {
		if x.ClaimedBy == "" || x.Refs.JobID == "" {
			if noJob == "" {
				return Task{}, fmt.Errorf("%w: transition to %s requires claimed_by and refs.job_id, or a no-job reason", ErrTaskJobRequired, in.To)
			}
			recordedNoJob = noJob
		}
	}
	now := time.Now().UTC()
	var duplicateEdge bool
	if action == actMove && to == StateDuplicate {
		if in.DuplicateOf != 0 {
			if _, err = addRelationTx(ctx, tx, id, RelationDuplicateOf, in.DuplicateOf, by, now); err != nil {
				return Task{}, err
			}
			duplicateEdge = true
		} else if duplicateEdge, err = hasLiveRelationTx(ctx, tx, id, RelationDuplicateOf); err != nil {
			return Task{}, err
		}
		if !duplicateEdge {
			return Task{}, fmt.Errorf("%w: a duplicate task points at the task it duplicates", ErrDuplicateOfRequired)
		}
	}
	if reopen {
		x.ClaimedBy = ""
		x.Refs.JobID = ""
		x.Refs.PR = ""
		x.Refs.HeadSHA = ""
		x.Refs.ReportPath = ""
	}
	flagWas := x.AwaitingDecision
	switch action {
	case actSetFlag:
		x.AwaitingDecision = true
	case actClearFlag:
		x.AwaitingDecision = false
	default:
		x.AwaitingDecision = false
		x.State = to
		applyLifecycle(&x, from, to, now)
	}
	x.UpdatedAt = now
	if x, err = writeTaskTx(ctx, tx, x); err != nil {
		return Task{}, err
	}
	if action == actMove {
		if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: id, From: from, To: to, By: by, Note: note, Refs: x.Refs, At: now, NoJob: recordedNoJob}); err != nil {
			return Task{}, err
		}
	}
	if x.AwaitingDecision != flagWas {
		ev := taskEventInput{TaskID: id, Kind: TaskEventAwaiting, From: x.State, To: x.State, By: by, Note: note, Refs: x.Refs, At: now, Field: "awaiting_decision", FromValue: fmt.Sprint(flagWas), ToValue: fmt.Sprint(x.AwaitingDecision)}
		if _, _, err = insertEvent(ctx, tx, ev); err != nil {
			return Task{}, err
		}
	}
	if action == actMove && from == StateDuplicate {
		// Leaving duplicate (reopen) archives its relation: the state and the
		// duplicate_of edge exist together or not at all.
		if err = archiveLiveRelationsTx(ctx, tx, id, RelationDuplicateOf, by, now); err != nil {
			return Task{}, err
		}
	}
	if action == actMove {
		if err = s.afterStateChangeTx(ctx, tx, x, from, by, now); err != nil {
			return Task{}, err
		}
	}
	if s.LinearSyncEnabled() && x.Refs.Linear != nil && x.Refs.Linear.Sync {
		if err = enqueueLinearTaskTransition(ctx, tx, x, note); err != nil {
			return Task{}, err
		}
	}
	if err := s.recordLinearBoardTaskTx(ctx, tx, x, note); err != nil {
		return Task{}, err
	}
	return x, tx.Commit(ctx)
}
