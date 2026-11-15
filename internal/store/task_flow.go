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
)

var (
	ErrClosureInvalid = errors.New("closure_invalid")
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
	TaskID   int64
	Kind     string
	From, To string
	By, Note string
	Refs     TaskRefs
	Rebind   *TaskJobRebind
	// Into, when set, receives the moved activity clock so the Task a caller
	// returns agrees with the row.
	Into       *Task
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
// already exists changes nothing; a redelivered event is a no-op. The
// returned boolean reports whether the activity clock moved.
func insertEvent(ctx context.Context, q rowQuerier, e taskEventInput) (id int64, moved bool, err error) {
	if e.Kind == "" {
		e.Kind = TaskEventTransition
	}
	encoded, err := json.Marshal(struct {
		TaskRefs
		JobRebind *TaskJobRebind `json:"job_rebind,omitempty"`
	}{e.Refs, e.Rebind})
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
	if err != nil || !isActivityEvent(e.Auto, e.Source) {
		return id, false, err
	}
	var taskID int64
	err = q.QueryRow(ctx, `UPDATE tasks SET last_activity_at = GREATEST(last_activity_at, $2) WHERE id = $1 AND last_activity_at < $2 RETURNING id`, e.TaskID, e.At).Scan(&taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, false, nil
	}
	if err == nil && e.Into != nil {
		e.Into.LastActivityAt = e.At.UTC().Truncate(time.Microsecond)
	}
	return id, err == nil, err
}

// applyLifecycle sets the lifecycle timestamps for a move from one state to
// another. started_at keeps its first value; completed_at and canceled_at
// describe the current state, so leaving the category clears them.
func applyLifecycle(x *Task, from, to string, now time.Time, c *TaskClosure) error {
	if closureWrittenBy(from, to) {
		if c == nil {
			return fmt.Errorf("%w: evidence is required", ErrClosureInvalid)
		}
		rule := taskClosureRules[to]
		evidence := strings.TrimSpace(c.Evidence)
		if len(evidence) < 1 || len(evidence) > rule.EvidenceMaxBytes || !validText(evidence, rule.EvidenceMaxBytes) {
			return fmt.Errorf("%w: evidence must be 1..%d bytes", ErrClosureInvalid, rule.EvidenceMaxBytes)
		}
		if rule.RejectsSecretLike && guard.Reject(evidence) != nil {
			return fmt.Errorf("%w: evidence contains secret-like content", ErrClosureInvalid)
		}
		valid := false
		for _, allowed := range rule.Types {
			if c.Type == allowed {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("%w: type is not allowed for target state", ErrClosureInvalid)
		}

		x.Refs.Closure = &TaskClosure{Type: c.Type, Evidence: evidence}
	} else if !IsSettledState(to) {
		x.Refs.Closure = nil
	}

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
	return nil
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
	// Match the claim path's key-presence exclusion, including malformed/null refs.
	if hasDisposition {
		return Task{}, fmt.Errorf("%w: disposition items cannot be claimed directly", ErrTaskConflict)
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
	// A job already registered for this task with a consult or research role
	// refines the spawn rule; a job not yet registered leaves the kind alone.
	var jobRole string
	if jobID != "" {
		if err := tx.QueryRow(ctx, `SELECT role FROM jobs WHERE job_id=$1 AND task_id=$2`, jobID, id).Scan(&jobRole); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Task{}, err
		}
	}
	to := substateForJobRole(jobRole, x.Kind)
	now := time.Now().UTC()
	x.State, x.ClaimedBy, x.UpdatedAt = to, by, now
	if jobID != "" {
		x.Refs.JobID = jobID
	}
	if err := applyLifecycle(&x, from, to, now, nil); err != nil {
		return Task{}, err
	}
	x, err := writeTaskTx(ctx, tx, x)
	if err != nil {
		return Task{}, err
	}
	if _, moved, err := insertEvent(ctx, tx, taskEventInput{TaskID: id, From: from, To: to, By: by, Refs: x.Refs, At: now, NoJob: noJob}); err != nil {
		return Task{}, err
	} else if moved {
		x.LastActivityAt = now.Truncate(time.Microsecond)
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
	Closure     string
	Evidence    string
	By          string
	Note        string
	Refs        *TaskRefs
	NoJob       string
	DuplicateOf int64
	// ClearOnly turns the transition into clearing the awaiting_decision flag:
	// it is refused on a task that is not flagged, and the state is unchanged.
	ClearOnly bool
	// Priority, Kind and Project apply only to a move out of triage to
	// backlog or unstarted: they set the task's values as part of the exit,
	// and the triage gate judges the values the task leaves with.
	Priority *int
	Kind     string
	Project  *string
	// Set applies a triage exit to the task's children still in triage in
	// the same transaction: an accept moves them to the same state, each
	// inheriting priority, kind and project unless it has its own; a decline
	// drops them. Declining a ticket set's parent declines its triage
	// children with or without Set.
	Set bool
	// Source and SourceRef name an idempotent producer of this transition.
	// The event carries them, so a replayed write with the same pair finds
	// the recorded event instead of applying the transition again.
	Source    string
	SourceRef string
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

func (s *Store) transitionTask(ctx context.Context, id int64, in TransitionInput, reopen bool) (Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
	x, err := s.transitionTaskTx(ctx, tx, id, in, reopen)
	if err != nil {
		return Task{}, err
	}
	return x, tx.Commit(ctx)
}

// transitionTaskTx is transitionTask inside a caller's transaction: an
// operator action that needs an intermediate step (close-as-merged) records
// both events under one commit. The caller holds tx open and commits.
func (s *Store) transitionTaskTx(ctx context.Context, tx pgx.Tx, id int64, in TransitionInput, reopen bool) (Task, error) {
	noJob := taskReasonText(in.NoJob)
	by, note, refs := in.By, in.Note, in.Refs
	if in.To == "" || !validText(by, 128) || by == "" || !validText(note, MaxBytes) || !validText(noJob, MaxBytes) || (refs != nil && !validTaskRefs(*refs)) || in.DuplicateOf < 0 {
		return Task{}, errors.New("invalid task transition")
	}
	if (in.Priority != nil && *in.Priority < 0) || (in.Kind != "" && !taskKinds[in.Kind]) || (in.Project != nil && !validName(*in.Project)) {
		return Task{}, errors.New("invalid task transition: priority must not be negative, kind must be one of " + strings.Join(TaskKinds(), ", ") + ", and project a project name")
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
		if refs.Closure != nil {
			return Task{}, fmt.Errorf("%w: closure refs are not patchable", ErrClosureInvalid)
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
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return Task{}, err
	}

	var x Task
	var hasDisposition bool
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+`,refs ? 'disposition' FROM tasks WHERE id=$1 FOR UPDATE`, id), &x, &hasDisposition); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, ErrTaskNotFound
		}
		return Task{}, err
	}
	decisionBefore := decisionBeforeOf(x)
	action, to, err := resolveTransitionTarget(x, in.To)
	if err != nil {
		return Task{}, err
	}
	if action == actMove && to == StateMerged && refs != nil && refs.PR != "" {
		if canonical := canonicalPRURL(refs.PR); canonical != refs.PR {
			fixed := *refs
			fixed.PR = canonical
			refs = &fixed
		}
	}
	if reopen {
		action, to = actMove, StateUnstarted
	}
	// hk 4322: a task a merged PR delivered closes as merged in one step from
	// any open state before reviewing when the transition names that PR
	// (refs.pr) and its change record shows it merged. The PR becomes the
	// closure evidence.
	now := time.Now().UTC()
	var deliveredBy int64
	if action == actMove && to == StateMerged && !reopen && refs != nil && refs.PR != "" && deliveredOpenStates[x.State] {
		if deliveredBy, err = mergedChangeTx(ctx, tx, refs.PR); err != nil {
			return Task{}, err
		}
		if x.Kind == "" {
			return Task{}, fmt.Errorf("%w: a triage task without a kind cannot close as merged; set its kind first", ErrTaskConflict)
		}
		// The delivered task implements the PR, like a task its merge event
		// names, so the PR's deploy and revert reach it (deployedTasksTx,
		// propagateRevertTx). It is linked before the hold below so a reland
		// PR counts as one; a refusal rolls the link back.
		if err = linkChangeTaskTx(ctx, tx, deliveredBy, id, ChangeImplements, true, now); err != nil {
			return Task{}, err
		}
		// The close-on-merge rule a merge event applies (hk 3294) holds here
		// too: another implements change still open, a reverted piece not
		// relanded or an open sub-task keeps the task open.
		hold, err := mergeHoldTx(ctx, tx, id, refs.PR)
		if err != nil {
			return Task{}, err
		}
		if hold != "" {
			return Task{}, fmt.Errorf("%w: %s", ErrTaskConflict, hold)
		}
		if strings.TrimSpace(note) == "" {
			note = "delivered by merged PR " + refs.PR
		}
	}
	if in.ClearOnly {
		if !x.AwaitingDecision {
			return Task{}, fmt.Errorf("%w: only a task awaiting a decision can clear its flag", ErrTaskConflict)
		}
		action, to = actClearFlag, x.State
	}

	from := x.State
	if in.DuplicateOf != 0 && (action != actMove || to != StateDuplicate) {
		return Task{}, errors.New("invalid task transition: duplicate_of applies only to a duplicate transition")
	}
	// An old-name transition on a task that is already there changes no state,
	// but a caller's refs patch (PR, head, report) still lands and is recorded.
	if action == actNoop && refs == nil {
		return x, nil
	}
	if reopen && !(IsSettledState(from) || from == StateIcebox) {
		return Task{}, fmt.Errorf("%w: only a settled or icebox task reopens; from %s a task can move to %s", ErrTaskConflict, from, allowedNextStates(from))
	}
	// The unset kind survives only a decline from triage; a reopen would put
	// it in unstarted, which the kind constraint refuses.
	if reopen && x.Kind == "" {
		return Task{}, fmt.Errorf("%w: this task was declined in triage without a kind; file it again instead", ErrTaskConflict)
	}
	if IsSettledState(from) && !reopen && !mergedCompletes(from, to) {
		return Task{}, transitionConflict(from, to)
	}
	if reopen && strings.TrimSpace(note) == "" {
		return Task{}, errors.New("reopen requires a non-empty reason")
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
			return Task{}, fmt.Errorf("%w: a disposition item cannot move back to backlog or change its origin", ErrTaskConflict)
		}
	}

	switch action {
	case actSetFlag:
		if x.AwaitingDecision {
			return Task{}, fmt.Errorf("%w: %s is already awaiting a decision", ErrTaskConflict, from)
		}
		if IsSettledState(from) {
			return Task{}, fmt.Errorf("%w: a settled task cannot be flagged as awaiting a decision; from %s a task can move to %s", ErrTaskConflict, from, allowedNextStates(from))
		}
	case actClearFlag, actNoop:
	default:
		// A flagged task may also go back to backlog, the way the old
		// needs_decision state did; the flag is cleared by the move.
		backToBacklog := x.AwaitingDecision && to == StateBacklog && !IsSettledState(from)
		if !backToBacklog && deliveredBy == 0 && !taskTransitionAllowed(from, to) {
			return Task{}, transitionConflict(from, to)
		}
		// An implement task completes on closure evidence like the other kinds;
		// the evidence rules in applyLifecycle still apply (hk 4050).
		if to == StateCompleted && from != StateMerged && !canCompleteKind(x.Kind) && x.Kind != "implement" {
			return Task{}, fmt.Errorf("%w: completed applies to implement, decide, ops, verify and research tasks", ErrTaskConflict)
		}
		// An implement task completes only with no unmerged PR and no open
		// sub-task; a refusal names them (hk 4050).
		if to == StateCompleted && x.Kind == "implement" {
			blockers, err := implementCompletionBlockers(ctx, tx, x.ID)
			if err != nil {
				return Task{}, err
			}
			if blockers != "" {
				return Task{}, fmt.Errorf("%w: implement cannot complete while %s", ErrTaskConflict, blockers)
			}
		}
		// A task waiting on a decision leaves only the way the old
		// needs_decision state did: back to backlog, parked, or dropped.
		if x.AwaitingDecision {
			switch to {
			case StateBacklog, StateIcebox, StateDropped, StateDuplicate:
			default:
				return Task{}, fmt.Errorf("%w: %s is awaiting a decision; it can move only to backlog, icebox, dropped or duplicate until the decision is answered", ErrTaskConflict, from)
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
	leavingTriage := action == actMove && from == StateTriage
	accepting := leavingTriage && (to == StateBacklog || to == StateUnstarted)
	triageFields := in.Priority != nil || in.Kind != "" || in.Project != nil
	if triageFields && !accepting {
		return Task{}, errors.New("invalid task transition: priority, kind and project apply only to leaving triage for backlog or unstarted")
	}
	if in.Set && !leavingTriage {
		return Task{}, errors.New("invalid task transition: set applies only to leaving triage")
	}
	before := x
	if triageFields {
		if err := applyTriageFieldsTx(ctx, tx, &x, in.Priority, in.Kind, in.Project); err != nil {
			return Task{}, err
		}
	}
	// The triage gate: a task leaves triage for backlog or unstarted only with
	// a priority above 0, a project and a kind; declining needs a reason.
	if leavingTriage {
		if err := triageGate(x, to, note); err != nil {
			return Task{}, err
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
	var duplicateEdge bool
	if action == actMove && taskClosureRules[to].DuplicateOf {
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
		// A reopen ends any dots run on the request: its run token is revoked
		// in this transaction, with the claim it belonged to (dots_run_token.go).
		if _, err := supersedeDotsRunTokensTx(ctx, tx, id, now); err != nil {
			return Task{}, err
		}
	}
	flagWas := x.AwaitingDecision
	// Asking a question of a parked task brings it back to backlog, as the old
	// hold -> needs_decision edge did.
	unpark := action == actSetFlag && from == StateIcebox
	switch action {
	case actSetFlag:
		x.AwaitingDecision = true
		if unpark {
			x.State = StateBacklog
			if err := applyLifecycle(&x, from, StateBacklog, now, nil); err != nil {
				return Task{}, err
			}
		}
	case actClearFlag:
		x.AwaitingDecision = false
	case actNoop:
	default:
		x.AwaitingDecision = false
		x.State = to
		var closure *TaskClosure
		if closureWrittenBy(from, to) {
			rule := taskClosureRules[to]
			typ, evidence := in.Closure, strings.TrimSpace(in.Evidence)
			if typ == "" {
				typ = rule.DefaultType
			}
			switch to {
			case StateMerged:
				if evidence == "" {
					evidence = x.Refs.PR
				}
				if evidence == "" {
					evidence = note
				}
			case StateDropped:
				if evidence == "" {
					evidence = note
				}
			case StateDuplicate:
				var dst int64
				if err := tx.QueryRow(ctx, `SELECT dst_id FROM task_relations WHERE src_id=$1 AND kind=$2 AND archived_at IS NULL ORDER BY id LIMIT 1`, id, RelationDuplicateOf).Scan(&dst); err != nil {
					return Task{}, err
				}
				evidence = fmt.Sprintf("duplicate of #%d", dst)
			}
			closure = &TaskClosure{Type: typ, Evidence: evidence}
		}
		if err := applyLifecycle(&x, from, to, now, closure); err != nil {
			return Task{}, err
		}
	}
	x.UpdatedAt = now
	if triageFields {
		if err = writeTriageFieldsTx(ctx, tx, before, x, by, now); err != nil {
			return Task{}, err
		}
	}
	if x, err = writeTaskTx(ctx, tx, x); err != nil {
		return Task{}, err
	}
	if action == actMove || action == actNoop || unpark {
		if unpark {
			to = StateBacklog
		}
		if _, moved, err := insertEvent(ctx, tx, taskEventInput{TaskID: id, From: from, To: to, By: by, Note: note, Refs: x.Refs, At: now, NoJob: recordedNoJob, Source: in.Source, SourceRef: in.SourceRef}); err != nil {
			return Task{}, err
		} else if moved {
			x.LastActivityAt = now.Truncate(time.Microsecond)
		}
	}
	var flagEvent int64
	if x.AwaitingDecision != flagWas {
		ev := taskEventInput{TaskID: id, Kind: TaskEventAwaiting, From: x.State, To: x.State, By: by, Note: note, Refs: x.Refs, At: now, Field: "awaiting_decision", FromValue: fmt.Sprint(flagWas), ToValue: fmt.Sprint(x.AwaitingDecision)}
		var moved bool
		if flagEvent, moved, err = insertEvent(ctx, tx, ev); err != nil {
			return Task{}, err
		}
		if moved {
			x.LastActivityAt = now.Truncate(time.Microsecond)
		}
	}
	if err = mirrorTaskDecisionsTx(ctx, tx, decisionBefore, x, by, note, flagEvent, now); err != nil {
		return Task{}, err
	}
	if action == actMove && from == StateDuplicate {
		// Leaving duplicate (reopen) archives its relation: the state and the
		// duplicate_of edge exist together or not at all.
		if err = archiveLiveRelationsTx(ctx, tx, id, RelationDuplicateOf, by, now); err != nil {
			return Task{}, err
		}
	}
	if action == actMove || unpark {
		if err = s.afterStateChangeTx(ctx, tx, x, from, by, now); err != nil {
			return Task{}, err
		}
	}
	if leavingTriage {
		// A delivered close is not a decline: the set's children keep their
		// own state, with --set too.
		cascade := in.Set && deliveredBy == 0
		if !accepting && !cascade && deliveredBy == 0 {
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_sets WHERE parent_task_id=$1)`, id).Scan(&cascade); err != nil {
				return Task{}, err
			}
		}
		if cascade {
			if err = s.cascadeTriageTx(ctx, tx, x, accepting, by, note, now); err != nil {
				return Task{}, err
			}
		}
	}
	// Rule 8: entering merged by hand also looks for a deploy record that
	// already lists the task's PR.
	if action == actMove && to == StateMerged {
		if x, err = s.deployOnMergedTx(ctx, tx, x, now, nil, nil); err != nil {
			return Task{}, err
		}
	}
	return x, nil
}

// deliveredOpenStates are the open states a task closes from as merged by
// naming the merged PR that delivered it (hk 4322). reviewing is not listed:
// reviewing -> merged is an edge of TaskTransitions, where --pr stays a
// reference and is not verified.
var deliveredOpenStates = map[string]bool{
	StateTriage: true, StateBacklog: true, StateUnstarted: true,
	StateResearching: true, StateDeveloping: true,
}

// mergedChangeTx returns the id of the change record of pr when it shows the
// PR merged. hk holds no GitHub credential, so the change record (written by
// mergewake's PR and merge events) is the evidence it can check; a PR without
// one, or not merged, is refused.
func mergedChangeTx(ctx context.Context, tx pgx.Tx, pr string) (int64, error) {
	repo, n, ok := parsePRURL(pr)
	if !ok {
		return 0, errors.New("invalid task transition: --pr must be a GitHub pull request URL to close a task as merged")
	}
	var id int64
	var state string
	var reverted bool
	err := tx.QueryRow(ctx, `SELECT id,state,reverted_by_change_id IS NOT NULL FROM changes WHERE repo=$1 AND pr_number=$2`, repo, n).Scan(&id, &state, &reverted)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: hk has no change record of %s, so it cannot verify the merge; the merge event records it", ErrTaskConflict, pr)
	}
	if err != nil {
		return 0, err
	}
	if state != ChangeMerged {
		return 0, fmt.Errorf("%w: %s is %s, not merged", ErrTaskConflict, pr, state)
	}
	if reverted {
		return 0, fmt.Errorf("%w: %s was reverted; relanding takes a new PR", ErrTaskConflict, pr)
	}
	return id, nil
}

// allowedNextStates lists, in state order, the states a task in from can move
// to. Refusals name them so the caller can pick a legal move (hk 2863).
func allowedNextStates(from string) string {
	var allowed []string
	for _, state := range TaskStates() {
		if taskTransitionAllowed(from, state) {
			allowed = append(allowed, state)
		}
	}
	return strings.Join(allowed, ", ")
}

// implementCompletionBlockers names what keeps an implement task from
// completing: each change it implements that is not merged, and each sub-task
// not yet completed or canceled. It returns "" when nothing blocks it.
func implementCompletionBlockers(ctx context.Context, tx pgx.Tx, taskID int64) (string, error) {
	var parts []string
	rows, err := tx.Query(ctx, `SELECT c.repo,c.pr_number,c.state FROM change_tasks ct JOIN changes c ON c.id=ct.change_id WHERE ct.task_id=$1 AND ct.relation=$2 AND c.state<>$3 ORDER BY c.repo,c.pr_number`, taskID, ChangeImplements, ChangeMerged)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var repo, state string
		var n int
		if err := rows.Scan(&repo, &n, &state); err != nil {
			rows.Close()
			return "", err
		}
		parts = append(parts, fmt.Sprintf("unmerged PR %s#%d (%s)", repo, n, state))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	rows, err = tx.Query(ctx, `SELECT id,state FROM tasks WHERE parent_id=$1 ORDER BY id`, taskID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			rows.Close()
			return "", err
		}
		if c := TaskCategory(state); c != CategoryCompleted && c != CategoryCanceled {
			parts = append(parts, fmt.Sprintf("open sub-task %d (%s)", id, state))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strings.Join(parts, ", "), nil
}

// transitionConflict is the task_conflict of a refused manual move: it names
// the states the task can move to from where it is (hk 4322).
func transitionConflict(from, to string) error {
	msg := fmt.Sprintf("%s -> %s is not a transition; from %s a task can move to %s", from, to, from, allowedNextStates(from))
	if to == StateMerged && deliveredOpenStates[from] {
		msg += ", or to merged with --pr (refs.pr) naming the merged PR that delivered it"
	}
	if IsSettledState(from) {
		msg += " (unstarted through tasks reopen)"
	}
	return fmt.Errorf("%w: %s", ErrTaskConflict, msg)
}

// triageGate is the one check on a move out of triage: to backlog or
// unstarted only with a priority above 0, a project and a kind; to dropped or
// duplicate only with a reason.
func triageGate(x Task, to, note string) error {
	switch to {
	case StateBacklog, StateUnstarted:
		if x.Priority <= 0 || x.Project == nil || *x.Project == "" || !taskKinds[x.Kind] {
			return fmt.Errorf("%w: leaving triage needs a priority above 0, a project and a kind", ErrTriageGate)
		}
	default:
		if strings.TrimSpace(note) == "" {
			return fmt.Errorf("%w: declining a triage task requires a reason", ErrTriageGate)
		}
	}
	return nil
}

// applyTriageFieldsTx sets the values a task leaves triage with on x; a nil
// or empty field keeps the task's own. A project must be in the vocabulary
// and agree with the task's milestone.
func applyTriageFieldsTx(ctx context.Context, tx pgx.Tx, x *Task, priority *int, kind string, project *string) error {
	if priority != nil {
		x.Priority = *priority
	}
	if kind != "" {
		x.Kind = kind
	}
	if project != nil {
		if err := requireTaskProject(ctx, tx, project); err != nil {
			return err
		}
		if x.MilestoneID != nil && (x.Project == nil || *x.Project != *project) {
			return ErrMilestoneProject
		}
		p := *project
		x.Project = &p
	}
	return nil
}

// writeTriageFieldsTx stores priority, kind and project where x differs from
// before, with one event per changed field: a project event as
// SetTaskProject writes, a metadata event for priority and kind.
func writeTriageFieldsTx(ctx context.Context, tx pgx.Tx, before, x Task, by string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE tasks SET priority=$2,kind=$3,project=$4 WHERE id=$1`, x.ID, x.Priority, x.Kind, x.Project); err != nil {
		return err
	}
	if before.Priority != x.Priority {
		if _, _, err := insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: "metadata", From: before.State, To: before.State, By: by, Refs: before.Refs, At: now, Field: "priority", FromValue: fmt.Sprint(before.Priority), ToValue: fmt.Sprint(x.Priority)}); err != nil {
			return err
		}
	}
	if before.Kind != x.Kind {
		if _, _, err := insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: "metadata", From: before.State, To: before.State, By: by, Refs: before.Refs, At: now, Field: "kind", FromValue: before.Kind, ToValue: x.Kind}); err != nil {
			return err
		}
	}
	from, to := "", ""
	if before.Project != nil {
		from = *before.Project
	}
	if x.Project != nil {
		to = *x.Project
	}
	if from != to {
		if _, _, err := insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: TaskEventProject, From: from, To: to, By: by, Refs: before.Refs, At: now, Field: "project", FromValue: from, ToValue: to}); err != nil {
			return err
		}
	}
	return nil
}

// cascadeTriageTx applies a parent's triage exit to its children still in
// triage, under the caller's task-mutation lock. An accept moves each child to
// the parent's state, inheriting priority, kind and project where the child
// has none; every child then passes the same gate. A decline drops each
// child: a duplicate parent does not make its children duplicates of anything.
func (s *Store) cascadeTriageTx(ctx context.Context, tx pgx.Tx, parent Task, accepting bool, by, note string, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE parent_id=$1 AND state=$2 ORDER BY id FOR UPDATE`, parent.ID, StateTriage)
	if err != nil {
		return err
	}
	var children []Task
	for rows.Next() {
		var c Task
		if err = scanTask(rows, &c); err != nil {
			rows.Close()
			return err
		}
		children = append(children, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	to, childNote := parent.State, note
	if !accepting {
		to = StateDropped
		childNote = fmt.Sprintf("declined with set #%d: %s", parent.ID, note)
	}
	for _, c := range children {
		before := c
		decisionBefore := decisionBeforeOf(c)
		if accepting {
			if c.Priority <= 0 {
				c.Priority = parent.Priority
			}
			if c.Kind == "" {
				c.Kind = parent.Kind
			}
			if c.Project == nil || *c.Project == "" {
				c.Project = parent.Project
			}
		}
		if err = triageGate(c, to, childNote); err != nil {
			return fmt.Errorf("sub-task #%d: %w", c.ID, err)
		}
		if accepting {
			if err = writeTriageFieldsTx(ctx, tx, before, c, by, now); err != nil {
				return err
			}
		}
		c.State, c.AwaitingDecision, c.UpdatedAt = to, false, now
		if err := applyLifecycle(&c, StateTriage, to, now, &TaskClosure{Type: "cancelled", Evidence: note}); err != nil {
			return err
		}
		if c, err = writeTaskTx(ctx, tx, c); err != nil {
			return err
		}
		if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: c.ID, From: StateTriage, To: to, By: by, Note: childNote, Refs: c.Refs, At: now}); err != nil {
			return err
		}
		if err = mirrorTaskDecisionsTx(ctx, tx, decisionBefore, c, by, childNote, 0, now); err != nil {
			return err
		}
		if err = s.afterStateChangeTx(ctx, tx, c, StateTriage, by, now); err != nil {
			return err
		}
	}
	return nil
}
