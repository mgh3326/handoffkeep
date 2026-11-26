package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/telemetry"
)

// Event sources (git show 1e3d2fa8:docs/linear-internalize.md, "Event-driven transitions").
const (
	SourcePanewireJob     = "panewire.job"
	SourceMergewakePR     = "mergewake.pr"
	SourceMergewakeReview = "mergewake.review"
	SourceMergewakeMerge  = "mergewake.merge"
	SourceMergewakeRevert = "mergewake.revert"
	SourceDeploy          = "hk.deploy"
	SourceAutoClose       = "hk.auto_close"
	SourceSweep           = "hk.sweep"
)

// TaskEventSource marks a source event recorded with applied=false. The state
// does not change, so from and to both carry the current state and the row is
// not a transition: readers that take "to" as a state change filter
// kind='transition' and skip it.
const TaskEventSource = "source_event"

// revertedPRField names the reverted PR on a mergewake.revert row (rule 10):
// the row's to_value is the reverted PR URL, applied or not.
const revertedPRField = "reverted_pr"

// heldMergeField marks a mergewake.merge row that mergeHoldTx held (hk 4172),
// as opposed to one a gate refused. Only a held merge names a PR an automatic
// close may adopt (heldMergePRTx).
const heldMergeField = "merge_held"

// taskEventPRRE is the PR URL shape an event names. A source_ref of a
// mergewake event is a PR URL (rule 10).
var taskEventPRRE = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[0-9]+$`)

// sourceEventRule is one row of the event table: the target and the states it
// applies from. panewire.job is absent: its target comes from the job role.
type sourceEventRule struct {
	to   string
	from map[string]bool
}

func states(xs ...string) map[string]bool {
	out := map[string]bool{}
	for _, x := range xs {
		out[x] = true
	}
	return out
}

var sourceEventTable = map[string]sourceEventRule{
	SourceMergewakePR:     {StateReviewing, states(StateBacklog, StateUnstarted, StateResearching, StateDeveloping)},
	SourceMergewakeReview: {StateReviewing, states(StateBacklog, StateUnstarted, StateResearching, StateDeveloping)},
	SourceMergewakeMerge:  {StateMerged, states(StateBacklog, StateUnstarted, StateResearching, StateDeveloping, StateReviewing)},
	SourceMergewakeRevert: {StateDeveloping, states(StateMerged, StateDeployed)},
	SourceDeploy:          {StateDeployed, states(StateMerged)},
	SourceSweep:           {StateIcebox, states(StateBacklog)},
}

// forwardRank is rule 2's order. backlog and unstarted come before it.
var forwardRank = map[string]int{StateResearching: 1, StateDeveloping: 2, StateReviewing: 3, StateMerged: 4, StateDeployed: 5}

// sourceEventTarget applies the event table and rules 2-4 to a state. It
// returns the target, or "" and why the event is recorded without effect.
// to overrides the table for panewire.job, whose target is the job role's.
func sourceEventTarget(source, from, to string) (string, string) {
	rule, ok := sourceEventTable[source]
	if source == SourcePanewireJob {
		rule, ok = sourceEventRule{to: to, from: states(StateResearching)}, to != ""
	}
	switch {
	case !ok:
		return "", "unknown source"
	case from == StateTriage:
		return "", "the task is in triage; an event does not bypass the triage gate"
	case !rule.from[from] && TaskCategory(from) == CategoryCanceled:
		return "", "the task is canceled; an event does not revive it"
	case !rule.from[from]:
		return "", fmt.Sprintf("%s does not apply from %s", source, from)
	}
	// Only revert moves back and only the sweep leaves the order (rule 2).
	if source != SourceMergewakeRevert && source != SourceSweep && forwardRank[rule.to] <= forwardRank[from] {
		return "", fmt.Sprintf("%s does not move %s forward", source, from)
	}
	return rule.to, ""
}

// MaxEventTasks caps the other tasks one event names (TaskSourceEvent.Tasks).
const MaxEventTasks = 32

// TaskSourceEvent is one integration event addressed to one task, and to the
// other tasks the PR names in Tasks (hk 3294: one Mergewake-Task line each,
// a bundle PR's children). Each named task gets the event as task_id does:
// its own change link, rules and recorded row.
// source_ref is the PR URL, and for a revert the revert PR URL (rule 10).
type TaskSourceEvent struct {
	Source     string     `json:"source"`
	SourceRef  string     `json:"source_ref"`
	TaskID     int64      `json:"task_id"`
	Tasks      []int64    `json:"tasks,omitempty"`
	PR         string     `json:"pr,omitempty"`
	RevertedPR string     `json:"reverted_pr,omitempty"`
	RevertPR   string     `json:"revert_pr,omitempty"`
	At         *time.Time `json:"at,omitempty"`
	// The fields below feed the work record (changes.go); none changes what
	// an event does to the task. HeadSHA and BaseSHA are allowed on every
	// mergewake source, MergeSHA only on mergewake.merge, Reviews only on
	// mergewake.review.
	HeadSHA  string            `json:"head_sha,omitempty"`
	BaseSHA  string            `json:"base_sha,omitempty"`
	MergeSHA string            `json:"merge_sha,omitempty"`
	Reviews  []TaskEventReview `json:"reviews,omitempty"`
}

// TaskEventResult says what an event did. Redelivered is set when the same
// (task, source, source_ref) was already recorded: nothing changed this time.
// Tasks is the result for each task in TaskSourceEvent.Tasks, in that order;
// a named task that does not exist has an empty State and the reason
// task_not_found, and nothing is written for it.
type TaskEventResult struct {
	Applied     bool              `json:"applied"`
	Redelivered bool              `json:"redelivered"`
	State       string            `json:"state"`
	Reason      string            `json:"reason,omitempty"`
	Task        Task              `json:"task"`
	Tasks       []TaskEventResult `json:"tasks,omitempty"`
	// held is set when the close-on-merge rule (mergeHoldTx) recorded the
	// merge without applying it: the PR still merged, so it propagates.
	held bool
}

// normalizeTaskSourceEvent checks an API event. pr defaults to source_ref for
// pr, review and merge; revert_pr defaults to source_ref for a revert, which
// must name the reverted PR.
func normalizeTaskSourceEvent(in TaskSourceEvent) (TaskSourceEvent, error) {
	if in.TaskID < 1 {
		return in, invalidChangeWrite("task_id must be positive")
	}
	if !taskEventPRRE.MatchString(in.SourceRef) {
		return in, invalidChangeWrite("source_ref must be a PR URL")
	}
	// One PR is one spelling from here on: the event key, refs.pr and every
	// comparison of them see /pull/0042 as /pull/42, as a manual move to merged
	// already does (PR #385 review).
	in.SourceRef, in.PR = canonicalPRURL(in.SourceRef), canonicalPRURL(in.PR)
	in.RevertPR, in.RevertedPR = canonicalPRURL(in.RevertPR), canonicalPRURL(in.RevertedPR)
	switch in.Source {
	case SourceMergewakePR, SourceMergewakeReview, SourceMergewakeMerge:
		if in.PR == "" {
			in.PR = in.SourceRef
		}
		if in.PR != in.SourceRef || in.RevertedPR != "" || in.RevertPR != "" {
			return in, invalidChangeWrite("pr must equal source_ref, and reverted_pr and revert_pr belong to a revert")
		}
	case SourceMergewakeRevert:
		if in.RevertPR == "" {
			in.RevertPR = in.SourceRef
		}
		if in.RevertPR != in.SourceRef || !taskEventPRRE.MatchString(in.RevertedPR) || (in.PR != "" && in.PR != in.RevertedPR) {
			return in, invalidChangeWrite("a revert needs reverted_pr, and source_ref is the revert PR URL")
		}
		in.PR = in.RevertedPR
	default:
		return in, invalidChangeWrite("unknown source")
	}
	if err := checkEventChangeFields(in); err != nil {
		return in, err
	}
	if len(in.Tasks) > MaxEventTasks {
		return in, fmt.Errorf("invalid task event: tasks names at most %d tasks", MaxEventTasks)
	}
	seen := map[int64]bool{in.TaskID: true}
	for _, id := range in.Tasks {
		if id < 1 || seen[id] {
			return in, errors.New("invalid task event: tasks must be positive, distinct and not task_id")
		}
		seen[id] = true
	}
	return in, nil
}

// sourceEvent is one event as the store applies it.
type sourceEvent struct {
	Source, SourceRef    string
	PR                   string
	MergeSHA             string
	RevertedPR, RevertPR string
	// To is the target of a panewire.job event; the table decides the rest.
	To          string
	At          time.Time
	Note        string
	CommentBody string
	CommentKey  string
	// Named is the tasks the event addresses itself (task_id and tasks). Each
	// gets the event in its own turn, so propagation and the rule 9 parent
	// row skip them rather than take their (task, source, source_ref) key.
	Named map[int64]bool
	// Pending is the named tasks whose own turn has not run yet. A merge (or
	// the deploy it fires) defers auto-closing a parent only while the
	// parent's turn is still pending: once it ran — held or applied — the
	// settling child's turn closes the parent itself.
	Pending map[int64]bool
}

// ApplyTaskSourceEvent applies one mergewake event to a task in one
// transaction and records it, applied or not.
func (s *Store) ApplyTaskSourceEvent(ctx context.Context, in TaskSourceEvent) (TaskEventResult, error) {
	in, err := normalizeTaskSourceEvent(in)
	if err != nil {
		return TaskEventResult{}, err
	}
	now := time.Now().UTC()
	at := now
	if in.At != nil && in.At.Before(now) {
		at = in.At.UTC()
	}
	var res TaskEventResult
	err = s.retryTx(ctx, func() error {
		var err error
		res, err = s.applyTaskSourceEventOnce(ctx, in, at, now)
		return err
	})
	if err != nil {
		return TaskEventResult{}, err
	}
	return res, nil
}

// applyTaskSourceEventOnce is one attempt of ApplyTaskSourceEvent's transaction.
func (s *Store) applyTaskSourceEventOnce(ctx context.Context, in TaskSourceEvent, at, now time.Time) (TaskEventResult, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return TaskEventResult{}, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if err = s.lockTx(ctx, tx, 8241873001); err != nil {
		return TaskEventResult{}, err
	}
	if in.Source == SourceMergewakePR || in.Source == SourceMergewakeMerge || in.Source == SourceMergewakeRevert {
		if err = s.lockTx(ctx, tx, taskCommentInsertLock); err != nil {
			return TaskEventResult{}, err
		}
	}
	// The named tasks follow task_id, each under its own row lock; the
	// level-2 lock held above serializes them with every other holder. A
	// turn's Pending is the named tasks still to come, so a child whose
	// parent was already served closes the parent in its own turn.
	var res TaskEventResult
	order := append([]int64{in.TaskID}, in.Tasks...)
	for i, id := range order {
		pending := map[int64]bool{}
		for _, later := range order[i+1:] {
			pending[later] = true
		}
		r, found, err := s.applyTaskSourceEventTx(ctx, tx, id, in, at, now, pending)
		if err != nil {
			return TaskEventResult{}, err
		}
		if i == 0 {
			if !found {
				return TaskEventResult{}, ErrTaskNotFound
			}
			res = r
			continue
		}
		if !found {
			r = TaskEventResult{Reason: "task_not_found"}
		}
		res.Tasks = append(res.Tasks, r)
	}
	if in.Source == SourceMergewakeRevert {
		if err = s.propagateRevertTx(ctx, tx, in, at, now); err != nil {
			return TaskEventResult{}, err
		}
	}
	return res, tx.Commit(ctx)
}

// propagateRevertTx carries a revert to every other task that implements the
// reverted PR and is closed on it: a task closed by naming the PR (hk 4322)
// or a sibling the merge propagated to. The event names one task, so without
// this those stay closed on undone work. A task whose refs.pr is the PR is
// reached too, linked or not. Each goes through recordChangeTx and
// applySourceEventTx as a named task does, in ascending id, under the
// task-mutation lock the caller holds.
func (s *Store) propagateRevertTx(ctx context.Context, tx transaction, in TaskSourceEvent, at, now time.Time) error {
	repo, n, ok := parsePRURL(in.PR)
	if !ok {
		return nil
	}
	if err := srcEvChangesGate(tx); err != nil {
		// B5 dependency: the lookup below reads change_tasks and changes.
		return err
	}
	named := map[int64]bool{in.TaskID: true}
	for _, id := range in.Tasks {
		named[id] = true
	}
	// A task closed on the PR without a link (refs.pr set by a manual
	// reviewing -> merged, or by a propagated merge before siblings were
	// linked) is reached by its refs.pr, as a deploy reaches it. A bundle child
	// with no PR of its own is reached by the bundle parent's PR that delivered
	// it (taskDeliveredPRSQL, hk 4844).
	var linked []Task
	if dialectFor(tx) == mysqlDialect {
		// Unreachable until B5 removes srcEvChangesGate; the text is ready.
		var err error
		linked, err = srcEvLockedTasksMySQL(ctx, tx, `SELECT t.id FROM tasks t WHERE t.state IN (?,?) AND ((`+srcEvDeliveredPRMySQL+`)=?
		OR EXISTS (SELECT 1 FROM change_tasks ct JOIN changes c ON c.id=ct.change_id WHERE ct.task_id=t.id AND c.repo=? AND c.pr_number=? AND ct.relation='implements'))
		ORDER BY t.id`, StateMerged, StateDeployed, in.PR, repo, n)
		if err != nil {
			return err
		}
	} else {
		rows, err := tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks t WHERE t.state IN ($3,$4) AND ((`+taskDeliveredPRSQL+`)=$5
		OR EXISTS (SELECT 1 FROM change_tasks ct JOIN changes c ON c.id=ct.change_id WHERE ct.task_id=t.id AND c.repo=$1 AND c.pr_number=$2 AND ct.relation='implements'))
		ORDER BY t.id FOR UPDATE`, repo, n, StateMerged, StateDeployed, in.PR)
		if err != nil {
			return err
		}
		linked, err = collectRows(rows, func(row row) (Task, error) {
			var x Task
			return x, scanTask(row, &x)
		})
		if err != nil {
			return err
		}
	}
	// Each linked task gets the revert in its own turn, so rule 9 skips a
	// linked parent rather than take its event key (as for a named one).
	var todo []Task
	for _, x := range linked {
		if !named[x.ID] {
			todo = append(todo, x)
		}
	}
	for _, x := range todo {
		named[x.ID] = true
	}
	for _, x := range todo {
		if err := srcEvRecordChangeTx(ctx, tx, x, in, at, now); err != nil {
			return err
		}
		ev := sourceEvent{Source: in.Source, SourceRef: in.SourceRef, PR: in.PR, MergeSHA: in.MergeSHA, RevertedPR: in.RevertedPR, RevertPR: in.RevertPR, At: at, Named: named,
			Note: fmt.Sprintf("the reverted PR also delivered this task; reverted with task #%d", in.TaskID)}
		if _, err := s.applySourceEventTx(ctx, tx, x, ev, now); err != nil {
			return err
		}
	}
	return nil
}

// applyTaskSourceEventTx applies one event to task id: it locks the row,
// writes the change rows, applies the event and propagates a merge. found is
// false, with nothing written, when the task does not exist.
func (s *Store) applyTaskSourceEventTx(ctx context.Context, tx transaction, id int64, in TaskSourceEvent, at, now time.Time, pending map[int64]bool) (TaskEventResult, bool, error) {
	var x Task
	if err := scanTask(tx.QueryRow(ctx, s.q(`SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, `SELECT `+taskColumns+` FROM tasks WHERE id=? FOR UPDATE`), id), &x); err != nil {
		if errors.Is(err, ErrNoRows) {
			return TaskEventResult{}, false, nil
		}
		return TaskEventResult{}, false, err
	}
	// The change rows describe the PR, so they are written for a redelivered
	// or not-applied event too.
	if err := srcEvRecordChangeTx(ctx, tx, x, in, at, now); err != nil {
		return TaskEventResult{}, false, err
	}
	named := map[int64]bool{in.TaskID: true}
	for _, n := range in.Tasks {
		named[n] = true
	}
	ev := sourceEvent{Source: in.Source, SourceRef: in.SourceRef, PR: in.PR, MergeSHA: in.MergeSHA, RevertedPR: in.RevertedPR, RevertPR: in.RevertPR, At: at, Named: named, Pending: pending}
	res, err := s.applySourceEventTx(ctx, tx, x, ev, now)
	if err != nil {
		return TaskEventResult{}, false, err
	}
	// A held merge still landed the PR: the other tasks on it each run their
	// own rules, the hold included.
	if (res.Applied || res.held) && in.Source == SourceMergewakeMerge {
		if err = s.propagateMergeTx(ctx, tx, res.Task.ID, ev, now); err != nil {
			return TaskEventResult{}, false, err
		}
	}
	return res, true, nil
}

// propagateMergeTx is the cross-task half of a merge (hk 3766): a PR merge
// that applied to task src, or was held there (hk 3294), applies in the same transaction to every other
// non-settled task whose refs.pr is that PR. Each goes through
// applySourceEventTx, so the same rules, claims, not-applied rows and deploy
// record lookup hold for it. Settled tasks are not touched. The caller holds
// the task-mutation lock (level 2), which serializes the sibling row locks.
func (s *Store) propagateMergeTx(ctx context.Context, tx transaction, src int64, ev sourceEvent, now time.Time) error {
	if _, _, ok := parsePRURL(ev.PR); ok {
		if err := srcEvChangesGate(tx); err != nil {
			// B5 dependency: the change lookup and linkChangeTaskTx below.
			return err
		}
	}
	var siblings []Task
	if dialectFor(tx) == mysqlDialect {
		var err error
		siblings, err = srcEvLockedTasksMySQL(ctx, tx, `SELECT id FROM tasks WHERE `+srcEvRefsPRMySQL+`=? AND id<>? AND state NOT IN (`+settledStatesSQL+`) ORDER BY id`, ev.PR, src)
		if err != nil {
			return err
		}
	} else {
		rows, err := tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE refs->>'pr'=$1 AND id<>$2 AND state NOT IN (`+settledStatesSQL+`) ORDER BY id FOR UPDATE`, ev.PR, src)
		if err != nil {
			return err
		}
		siblings, err = collectRows(rows, func(row row) (Task, error) {
			var x Task
			err := scanTask(row, &x)
			return x, err
		})
		if err != nil {
			return err
		}
	}
	var changeID int64
	if repo, n, ok := parsePRURL(ev.PR); ok {
		if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT id FROM changes WHERE repo=$1 AND pr_number=$2`, `SELECT id FROM changes WHERE repo=? AND pr_number=?`), repo, n).Scan(&changeID); err != nil && !errors.Is(err, ErrNoRows) {
			return err
		}
	}
	for _, x := range siblings {
		if ev.Named[x.ID] {
			continue
		}
		// The sibling implements the PR too: link it first, so a later revert
		// of the PR holds the sibling's own close (unresolvedRevertSQL).
		if changeID != 0 {
			// changeID is 0 on MySQL: srcEvChangesGate refused above.
			if err := linkChangeTask(ctx, tx, changeID, x.ID, ChangeImplements, true, now); err != nil {
				return err
			}
		}
		sib := ev
		sib.Note = fmt.Sprintf("the PR is also task #%d's; applied with it", src)
		if _, err := s.applySourceEventTx(ctx, tx, x, sib, now); err != nil {
			return err
		}
	}
	return nil
}

// revertNamesPRTx reports whether a mergewake.revert row on the task names pr
// as the reverted PR, applied or not (rules 9 and 10).
func revertNamesPRTx(ctx context.Context, q rowQuerier, taskID int64, pr string) (bool, error) {
	var found bool
	err := q.QueryRow(ctx, sqlFor(q, `SELECT EXISTS(SELECT 1 FROM task_events WHERE task_id=$1 AND source=$2 AND field=$3 AND to_value=$4)`, `SELECT EXISTS(SELECT 1 FROM task_events WHERE task_id=? AND source=? AND field=? AND to_value=?)`), taskID, SourceMergewakeRevert, revertedPRField, pr).Scan(&found)
	return found, err
}

// liveJobTx reports whether the task's bound job is still claimed or spawned.
func liveJobTx(ctx context.Context, q rowQuerier, x Task) (bool, error) {
	if x.Refs.JobID == "" {
		return false, nil
	}
	var live bool
	err := q.QueryRow(ctx, sqlFor(q, `SELECT EXISTS(SELECT 1 FROM jobs WHERE job_id=$1 AND task_id=$2 AND state IN ('claimed','spawned'))`, `SELECT EXISTS(SELECT 1 FROM jobs WHERE job_id=? AND task_id=? AND state IN ('claimed','spawned'))`), x.Refs.JobID, x.ID).Scan(&live)
	return live, err
}

// sourceEventRecordedTx reports whether task taskID already has a row for
// (source, sourceRef), applied or not: that event's turn on the task is over,
// and applySourceEventTx answers a redelivery with no change.
func sourceEventRecordedTx(ctx context.Context, q rowQuerier, taskID int64, source, sourceRef string) (bool, error) {
	var recorded bool
	err := q.QueryRow(ctx, sqlFor(q, `SELECT EXISTS(SELECT 1 FROM task_events WHERE task_id=$1 AND source=$2 AND source_ref=$3)`, `SELECT EXISTS(SELECT 1 FROM task_events WHERE task_id=? AND source=? AND source_ref=?)`), taskID, source, sourceRef).Scan(&recorded)
	return recorded, err
}

// applySourceEventTx applies one event to x, which the caller has locked in
// tx. A (task, source, source_ref) already recorded changes nothing. Every
// other event is recorded: applied with the move, or with applied=false and
// the reason in the note.
func (s *Store) applySourceEventTx(ctx context.Context, tx transaction, x Task, ev sourceEvent, now time.Time) (TaskEventResult, error) {
	recorded, err := sourceEventRecordedTx(ctx, tx, x.ID, ev.Source, ev.SourceRef)
	if err != nil {
		return TaskEventResult{}, err
	}
	if recorded {
		return TaskEventResult{Redelivered: true, State: x.State, Task: x}, nil
	}
	commentBody := ev.CommentBody
	commentKey := ev.SourceRef
	if ev.CommentKey != "" {
		commentKey = ev.CommentKey
	}
	if commentBody == "" {
		switch ev.Source {
		case SourceMergewakePR:
			_, n, ok := parsePRURL(ev.PR)
			if ok {
				commentBody = fmt.Sprintf("PR #%d opened: %s", n, ev.PR)
			}
		case SourceMergewakeMerge:
			_, n, ok := parsePRURL(ev.PR)
			if ok {
				sha := ev.MergeSHA
				if len(sha) > 7 {
					sha = sha[:7]
				}
				if sha == "" {
					commentBody = fmt.Sprintf("Merged: PR #%d", n)
				} else {
					commentBody = fmt.Sprintf("Merged: PR #%d (%s)", n, sha)
				}
			}
		case SourceMergewakeRevert:
			_, n, ok := parsePRURL(ev.PR)
			if ok {
				commentBody = fmt.Sprintf("Reverted: PR #%d", n)
			}
		case SourceDeploy:
		}
	}
	writeEventComment := func() error {
		if commentBody == "" {
			return nil
		}
		if err := s.insertSystemTaskCommentTx(ctx, tx, x.ID, ev.Source, commentKey, commentBody, now); err != nil {
			log.Printf("task_event_comment_failed task_id=%d source=%q err=%q", x.ID, ev.Source, err)
		}
		return nil
	}
	if x.State == StateMerged && x.Refs.PR == "" && ev.Source == SourceMergewakeMerge && ev.PR != "" {
		// A parent that an automatic close moved to merged before its own PR was
		// named adopts it (hk 4172). Any other merged task is not adopting.
		hasSub, err := hasSubTasksTx(ctx, tx, x.ID)
		if err != nil {
			return TaskEventResult{}, err
		}
		if hasSub {
			// hk 5112: an explicit task is closed by hand, so adoption (which
			// records the PR and then deploys it) does not run. The merge is
			// recorded as held: the task stays merged, and a later automatic close
			// can still adopt the PR (heldMergePRTx).
			if x.CloseRule == CloseRuleExplicit {
				return s.recordExplicitHeldMergeTx(ctx, tx, x, ev)
			}
			return s.adoptMergedPRTx(ctx, tx, x, ev, now, writeEventComment)
		}
	}
	field, revertedPR := "", ""
	if ev.Source == SourceMergewakeRevert {
		field, revertedPR = revertedPRField, ev.RevertedPR
	}
	to, reason := sourceEventTarget(ev.Source, x.State, ev.To)
	if reason == "" && ev.Source == SourceMergewakeMerge && canCompleteKind(x.Kind) {
		// A decide, verify, research or ops task has no change to land. The merge of the
		// PR it names closes it at completed, with the PR as the result link
		// (the closure below takes ev.SourceRef as the evidence).
		to = StateCompleted
	}
	// hk 5112 (close_rule explicit): a close the event would make is recorded,
	// not applied. It is checked before any change or decision read, so an
	// explicit task never reaches the parts of the close that are not ported.
	// A merge held this way still landed its PR, so it is recorded as a held
	// merge (merge_held): siblings on the PR propagate, and a later automatic
	// close can adopt the PR (heldMergePRTx).
	held := false
	if reason == "" && explicitHoldsClose(x, to) {
		reason = explicitCloseReason
		held = ev.Source == SourceMergewakeMerge
	}
	if reason == "" && ev.PR != "" && (ev.Source == SourceMergewakeMerge || ev.Source == SourceDeploy) {
		reverted, err := revertNamesPRTx(ctx, tx, x.ID, ev.PR)
		if err != nil {
			return TaskEventResult{}, err
		}
		if reverted && ev.Source == SourceMergewakeMerge {
			reason = "the PR was reverted; relanding takes a new PR"
		} else if reverted {
			reason = "the deploy lists only the reverted PR"
		}
	}
	if reason == "" && ev.Source == SourceMergewakeMerge {
		if reason, err = mergeHoldTx(ctx, tx, x.ID, ev.PR); err != nil {
			return TaskEventResult{}, err
		}
		held = reason != ""
	}
	if reason != "" {
		note := joinNote(ev.Note, "not applied: "+reason)
		if held {
			field = heldMergeField
		}
		if _, _, err := insertEvent(ctx, tx, taskEventInput{
			TaskID: x.ID, Kind: TaskEventSource, From: x.State, To: x.State, By: ev.Source, Note: note, Refs: x.Refs, At: ev.At,
			Source: ev.Source, SourceRef: ev.SourceRef, Field: field, ToValue: revertedPR, Auto: true, NotApplied: true,
		}); err != nil {
			return TaskEventResult{}, err
		}
		return TaskEventResult{State: x.State, Reason: reason, Task: x, held: held}, nil
	}

	from := x.State
	// Rule 7: an event that moves an unclaimed task claims it for its source,
	// with source_ref as the no-job reason. Rule 9: a revert does the same
	// when the task has no live job. The sweep parks work; it claims nothing.
	noJob := ""
	claim := x.ClaimedBy == ""
	if !claim && ev.Source == SourceMergewakeRevert {
		live, err := liveJobTx(ctx, tx, x)
		if err != nil {
			return TaskEventResult{}, err
		}
		if !live {
			// The ended job no longer binds the task; the revert claims it.
			claim, x.Refs.JobID = true, ""
		}
	}
	if claim && ev.Source != SourceSweep {
		x.ClaimedBy, noJob = ev.Source, ev.SourceRef
	}
	switch ev.Source {
	case SourceMergewakeMerge:
		x.Refs.PR = ev.PR
	case SourceMergewakeRevert:
		x.Refs.RevertPR = ev.RevertPR
	}
	decisionBefore := decisionBeforeOf(x)
	x.State, x.UpdatedAt = to, now
	if err := applyLifecycle(&x, from, to, now, &TaskClosure{Type: "resolved", Evidence: ev.SourceRef}); err != nil {
		return TaskEventResult{}, err
	}
	if x, err = writeTaskTx(ctx, tx, x); err != nil {
		return TaskEventResult{}, err
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{
		TaskID: x.ID, From: from, To: to, By: ev.Source, Note: ev.Note, Refs: x.Refs, At: ev.At, NoJob: noJob,
		Source: ev.Source, SourceRef: ev.SourceRef, Field: field, ToValue: revertedPR, Auto: true,
	}); err != nil {
		return TaskEventResult{}, err
	}
	if err = writeEventComment(); err != nil {
		return TaskEventResult{}, err
	}
	if err = s.mirrorTaskDecisionsFlowTx(ctx, tx, decisionBefore, x, ev.Source, ev.Note, 0, now); err != nil {
		return TaskEventResult{}, err
	}
	deferNamedParentClose := x.ParentID != nil && ev.Pending[*x.ParentID] &&
		(ev.Source == SourceMergewakeMerge || ev.Source == SourceDeploy)
	if deferNamedParentClose {
		// The parent's own turn moves it only while it is open, or merged
		// with no PR to adopt (hk 4172). A parent already deployed — or
		// merged with its PR — cannot move there; this turn must run the
		// rollup close itself (autoCloseTx deploys a merged parent whose
		// sub-tasks deployed).
		var pstate, ppr string
		if err = tx.QueryRow(ctx, s.q(`SELECT state,COALESCE(refs->>'pr','') FROM tasks WHERE id=$1`, `SELECT state,COALESCE(`+srcEvRefsPRMySQL+`,'') FROM tasks WHERE id=?`), *x.ParentID).Scan(&pstate, &ppr); err != nil {
			return TaskEventResult{}, err
		}
		deferNamedParentClose = !IsSettledState(pstate) || (pstate == StateMerged && ppr == "")
	}
	if deferNamedParentClose {
		// The parent's pending turn is the merge of ev.PR (a deploy applied in
		// a merge's turn carries the PR of that merge). When the parent already
		// has that row — a held merge the same event or its redelivery recorded
		// earlier — its turn is a no-op redelivery that rolls nothing up, so
		// this turn closes the parent itself (PR #385 review).
		recorded, err := sourceEventRecordedTx(ctx, tx, *x.ParentID, SourceMergewakeMerge, ev.PR)
		if err != nil {
			return TaskEventResult{}, err
		}
		deferNamedParentClose = !recorded
	}
	if err = s.afterSourceMoveTx(ctx, tx, x, from, ev.Source, now, deferNamedParentClose); err != nil {
		return TaskEventResult{}, err
	}
	if ev.Source == SourceMergewakeRevert && x.ParentID != nil && !ev.Named[*x.ParentID] {
		// Rule 9: a parent that auto-closed stays closed; the revert is
		// recorded on it.
		var p Task
		if err = scanTask(tx.QueryRow(ctx, s.q(`SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, `SELECT `+taskColumns+` FROM tasks WHERE id=? FOR UPDATE`), *x.ParentID), &p); err != nil {
			return TaskEventResult{}, err
		}
		if TaskCategory(p.State) == CategoryCompleted {
			if _, _, err = insertEvent(ctx, tx, taskEventInput{
				TaskID: p.ID, Kind: TaskEventSource, From: p.State, To: p.State, By: ev.Source, Refs: p.Refs, At: ev.At,
				Note:   fmt.Sprintf("not applied: sub-task #%d was reverted; the parent stays %s", x.ID, p.State),
				Source: ev.Source, SourceRef: ev.SourceRef, Field: field, ToValue: revertedPR, Auto: true, NotApplied: true,
			}); err != nil {
				return TaskEventResult{}, err
			}
		}
	}
	if to == StateMerged {
		if x, err = s.deployOnMergedTx(ctx, tx, x, now, ev.Named, ev.Pending); err != nil {
			return TaskEventResult{}, err
		}
	}
	return TaskEventResult{Applied: true, State: x.State, Task: x}, nil
}

// recordExplicitHeldMergeTx records a merge that an explicit task (hk 5112)
// may not adopt: the row is not applied and marked as a held merge, and neither
// refs.pr nor the state changes.
func (s *Store) recordExplicitHeldMergeTx(ctx context.Context, tx transaction, x Task, ev sourceEvent) (TaskEventResult, error) {
	if _, _, err := insertEvent(ctx, tx, taskEventInput{
		TaskID: x.ID, Kind: TaskEventSource, From: x.State, To: x.State, By: ev.Source, Note: joinNote(ev.Note, "not applied: "+explicitCloseReason), Refs: x.Refs, At: ev.At,
		Source: ev.Source, SourceRef: ev.SourceRef, Field: heldMergeField, Auto: true, NotApplied: true,
	}); err != nil {
		return TaskEventResult{}, err
	}
	return TaskEventResult{State: x.State, Reason: explicitCloseReason, Task: x, held: true}, nil
}

// adoptMergedPRTx is named-parent PR adoption (hk 4172): a merge names a task
// that an automatic close already moved to merged, with no PR of its own (its
// merge was held on an open sub-task, or came after the close). The task
// adopts the PR, so a deploy record that lists it can apply. Adoption does not
// deploy the task past a sub-task that is merged and not yet deployed.
func (s *Store) adoptMergedPRTx(ctx context.Context, tx transaction, x Task, ev sourceEvent, now time.Time, writeComment func() error) (TaskEventResult, error) {
	reverted, err := revertNamesPRTx(ctx, tx, x.ID, ev.PR)
	if err != nil {
		return TaskEventResult{}, err
	}
	if reverted {
		reason := "the PR was reverted; relanding takes a new PR"
		if _, _, err := insertEvent(ctx, tx, taskEventInput{
			TaskID: x.ID, Kind: TaskEventSource, From: x.State, To: x.State, By: ev.Source, Note: joinNote(ev.Note, "not applied: "+reason), Refs: x.Refs, At: ev.At,
			Source: ev.Source, SourceRef: ev.SourceRef, Auto: true, NotApplied: true,
		}); err != nil {
			return TaskEventResult{}, err
		}
		return TaskEventResult{State: x.State, Reason: reason, Task: x}, nil
	}
	x.Refs.PR = ev.PR
	x.UpdatedAt = now
	if x, err = writeTaskTx(ctx, tx, x); err != nil {
		return TaskEventResult{}, err
	}
	// Same-state row: kind source_event, so transition readers skip it.
	if _, _, err = insertEvent(ctx, tx, taskEventInput{
		TaskID: x.ID, Kind: TaskEventSource, From: x.State, To: x.State, By: ev.Source, Note: joinNote(ev.Note, "adopted the PR; the task was already merged"), Refs: x.Refs, At: ev.At,
		Source: ev.Source, SourceRef: ev.SourceRef, Auto: true,
	}); err != nil {
		return TaskEventResult{}, err
	}
	// The merge comment precedes the rule-8 deploy comment, as on the
	// ordinary merge path.
	if err = writeComment(); err != nil {
		return TaskEventResult{}, err
	}
	roll, err := subTaskRollupTx(ctx, tx, x.ID)
	if err != nil {
		return TaskEventResult{}, err
	}
	// The deploy also waits for the task's other changes: an open or
	// unrelanded implements change (other than the incoming PR) holds it.
	holds, err := taskHoldsTx(ctx, tx, x.ID, ev.PR)
	if err != nil {
		return TaskEventResult{}, err
	}
	if roll.merged == 0 && holds.none() {
		if x, err = s.deployOnMergedTx(ctx, tx, x, now, ev.Named, ev.Pending); err != nil {
			return TaskEventResult{}, err
		}
	}
	return TaskEventResult{Applied: true, State: x.State, Task: x}, nil
}

// hasSubTasksTx reports whether task taskID has any sub-task.
func hasSubTasksTx(ctx context.Context, tx transaction, taskID int64) (bool, error) {
	var has bool
	err := tx.QueryRow(ctx, sqlFor(tx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE parent_id=$1)`, `SELECT EXISTS(SELECT 1 FROM tasks WHERE parent_id=?)`), taskID).Scan(&has)
	return has, err
}

// heldMergePRTx is the PR of the newest held merge that named task taskID, when
// that PR's change is merged and not reverted, or "". A merge held by
// mergeHoldTx records no PR on the task; an automatic close to merged adopts it
// (hk 4172), so the task does not stay merged with no PR of its own.
func heldMergePRTx(ctx context.Context, tx transaction, taskID int64) (string, error) {
	if err := srcEvChangesGate(tx); err != nil {
		// B5 dependency: the join below reads changes.
		return "", err
	}
	var pr string
	query := `SELECT e.source_ref FROM task_events e JOIN changes c ON (` + prURLSQL + `)=e.source_ref
		WHERE e.task_id=$1 AND e.source=$2 AND e.field=$3 AND NOT e.applied AND c.state='merged' AND c.reverted_by_change_id IS NULL
		ORDER BY e.id DESC LIMIT 1`
	if dialectFor(tx) == mysqlDialect {
		query = `SELECT e.source_ref FROM task_events e JOIN changes c ON CONCAT('https://github.com/',c.repo,'/pull/',c.pr_number)=e.source_ref
		WHERE e.task_id=? AND e.source=? AND e.field=? AND NOT e.applied AND c.state='merged' AND c.reverted_by_change_id IS NULL
		ORDER BY e.id DESC LIMIT 1`
	}
	err := tx.QueryRow(ctx, query, taskID, SourceMergewakeMerge, heldMergeField).Scan(&pr)
	if errors.Is(err, ErrNoRows) {
		return "", nil
	}
	return pr, err
}

// prURLSQL is the PR URL of changes row c.
const prURLSQL = `'https://github.com/'||c.repo||'/pull/'||c.pr_number::text`

// openImplementsSQL lists the PR URLs of a task's implements changes that are
// still open ($1 the task), other than the PR in $2 (repo) and $3 (number).
// A closed (unmerged) change does not hold a task; a merged one is done.
const openImplementsSQL = `SELECT ` + prURLSQL + ` FROM change_tasks ct JOIN changes c ON c.id=ct.change_id
	WHERE ct.task_id=$1 AND ct.relation='implements' AND c.state='open' AND NOT (c.repo=$2 AND c.pr_number=$3) ORDER BY c.id`

// unresolvedRevertSQL is the condition, on changes row c linked to the task
// whose id is the SQL expression task, that c is an implements change of the
// task that was reverted and not relanded: no merged, not reverted
// implements change of the task was opened after the revert merged.
// Relanding takes a new PR (rule 10), so a PR opened before the revert
// merged does not reland it, whatever order its rows were written in.
// opened_at is the time of the first event that named the PR (its
// mergewake.pr event, else its merge); the revert's merged_at is the revert
// event's time.
func unresolvedRevertSQL(task string) string {
	return `ct.task_id=` + task + ` AND ct.relation='implements' AND c.reverted_by_change_id IS NOT NULL
	AND NOT EXISTS (SELECT 1 FROM change_tasks rt JOIN changes r ON r.id=rt.change_id JOIN changes rv ON rv.id=c.reverted_by_change_id
		WHERE rt.task_id=` + task + ` AND rt.relation='implements' AND r.state='merged' AND r.reverted_by_change_id IS NULL AND r.opened_at>COALESCE(rv.merged_at,rv.created_at))`
}

func unresolvedRevertMySQL(task string) string {
	return `ct.task_id=` + task + ` AND ct.relation='implements' AND c.reverted_by_change_id IS NOT NULL
	AND NOT EXISTS (SELECT 1 FROM change_tasks rt JOIN changes r ON r.id=rt.change_id JOIN changes rv ON rv.id=c.reverted_by_change_id
		WHERE rt.task_id=` + task + ` AND rt.relation='implements' AND r.state='merged' AND r.reverted_by_change_id IS NULL AND r.opened_at>COALESCE(rv.merged_at,rv.created_at))`
}

// openSubTasksSQL lists unsettled sub-tasks filed no later than the PR that
// delivers the parent. Later children are follow-ups, not pieces of that PR.
const openSubTasksSQL = `SELECT id FROM tasks WHERE parent_id=$1 AND state NOT IN (` + settledStatesSQL + `) AND created_at<=$2 ORDER BY id`
const allOpenSubTasksSQL = `SELECT id FROM tasks WHERE parent_id=$1 AND state NOT IN (` + settledStatesSQL + `) ORDER BY id`

// mergeHoldTx is the close-on-merge rule (hk 3294): a merge closes a task
// only when it delivers the task's last open piece. It returns why the merge
// of pr is held, or "" when it may apply. A task is held while another of its
// implements changes is open (the task closes on that change's merge), while
// one of its implements changes is reverted and not relanded, or while one of
// its sub-tasks is not settled (a parent closes through auto_close_parent
// when its last sub-task settles).
func mergeHoldTx(ctx context.Context, tx transaction, taskID int64, pr string) (string, error) {
	h, err := taskHoldsTx(ctx, tx, taskID, pr)
	if err != nil {
		return "", err
	}
	return h.mergeReason(), nil
}

// taskHolds is what holds a task's close: its implements changes still open
// (other than the PR being merged), its implements changes reverted and not
// relanded (unresolvedRevertSQL), and its pre-PR sub-tasks not yet settled. It
// is the one read of the close-on-merge holds, shared by the merge gate, the
// automatic close of a parent (closeGateTx) and the drift report.
type taskHolds struct {
	OpenPRs      []string
	RevertedPRs  []string
	OpenSubTasks []int64
}

// none reports whether nothing holds the task's close.
func (h taskHolds) none() bool {
	return len(h.OpenPRs) == 0 && len(h.RevertedPRs) == 0 && len(h.OpenSubTasks) == 0
}

// pieceReason says why the task's own changes hold its close, or "" for none.
func (h taskHolds) pieceReason() string {
	if len(h.OpenPRs) > 0 {
		return "implements change " + strings.Join(h.OpenPRs, ", ") + " is still open; the task closes when its last implements change merges"
	}
	if len(h.RevertedPRs) > 0 {
		return "implements change " + strings.Join(h.RevertedPRs, ", ") + " was reverted and not relanded; relanding takes a new PR"
	}
	return ""
}

// mergeReason says why a merge of the task is held, or "" for none: its own
// changes first, then its sub-tasks that are not settled.
func (h taskHolds) mergeReason() string {
	if reason := h.pieceReason(); reason != "" {
		return reason
	}
	if len(h.OpenSubTasks) > 0 {
		ids := make([]string, len(h.OpenSubTasks))
		for i, id := range h.OpenSubTasks {
			ids[i] = fmt.Sprintf("#%d", id)
		}
		return "sub-tasks " + strings.Join(ids, ", ") + " are not settled; the parent closes when its last sub-task does (auto_close_parent)"
	}
	return ""
}

// taskHoldsTx reads the holds on task taskID. pr, when set, is the PR the
// caller is merging: it is not an open hold on the task.
func taskHoldsTx(ctx context.Context, tx transaction, taskID int64, pr string) (taskHolds, error) {
	var h taskHolds
	if err := srcEvChangesGate(tx); err != nil {
		// B5 dependency: the first two holds read change_tasks and changes.
		return h, err
	}
	repo, n, _ := parsePRURL(pr)
	openQuery := openImplementsSQL
	revertedQuery := `SELECT ` + prURLSQL + ` FROM change_tasks ct JOIN changes c ON c.id=ct.change_id WHERE ` + unresolvedRevertSQL("$1") + ` ORDER BY c.id`
	subTaskQuery := openSubTasksSQL
	revertedArgs := []any{taskID}
	if dialectFor(tx) == mysqlDialect {
		openQuery = `SELECT CONCAT('https://github.com/',c.repo,'/pull/',c.pr_number) FROM change_tasks ct JOIN changes c ON c.id=ct.change_id WHERE ct.task_id=? AND ct.relation='implements' AND c.state='open' AND NOT (c.repo=? AND c.pr_number=?) ORDER BY c.id`
		revertedQuery = `SELECT CONCAT('https://github.com/',c.repo,'/pull/',c.pr_number) FROM change_tasks ct JOIN changes c ON c.id=ct.change_id WHERE ` + unresolvedRevertMySQL("?") + ` ORDER BY c.id`
		subTaskQuery = `SELECT id FROM tasks WHERE parent_id=? AND state NOT IN (` + settledStatesSQL + `) AND created_at<=? ORDER BY id`
		revertedArgs = append(revertedArgs, taskID)
	}
	rows, err := tx.Query(ctx, openQuery, taskID, repo, n)
	if err != nil {
		return h, err
	}
	if h.OpenPRs, err = collectRows(rows, rowTo[string]); err != nil {
		return h, err
	}
	rows, err = tx.Query(ctx, revertedQuery, revertedArgs...)
	if err != nil {
		return h, err
	}
	if h.RevertedPRs, err = collectRows(rows, rowTo[string]); err != nil {
		return h, err
	}
	openedAt, found, err := taskHoldOpenedAtTx(ctx, tx, taskID, pr)
	if err != nil {
		return h, err
	}
	if !found {
		// Without an implements change there is no PR boundary. Preserve the
		// planned-piece rule and treat every unsettled child as a hold.
		query := allOpenSubTasksSQL
		if dialectFor(tx) == mysqlDialect {
			query = `SELECT id FROM tasks WHERE parent_id=? AND state NOT IN (` + settledStatesSQL + `) ORDER BY id`
		}
		rows, err = tx.Query(ctx, query, taskID)
	} else {
		rows, err = tx.Query(ctx, subTaskQuery, taskID, openedAt)
	}
	if err != nil {
		return h, err
	}
	h.OpenSubTasks, err = collectRows(rows, rowTo[int64])
	return h, err
}

// taskHoldOpenedAtTx returns the implements change that establishes the
// boundary between planned pieces and PR follow-ups. An incoming PR is the
// boundary for its merge event; other readers use the task's earliest live
// implements change. A missing boundary keeps the conservative old behavior.
func taskHoldOpenedAtTx(ctx context.Context, tx transaction, taskID int64, pr string) (time.Time, bool, error) {
	var openedAt time.Time
	var err error
	if repo, number, ok := parsePRURL(pr); ok {
		err = tx.QueryRow(ctx, sqlFor(tx, `SELECT opened_at FROM changes WHERE repo=$1 AND pr_number=$2`, `SELECT opened_at FROM changes WHERE repo=? AND pr_number=?`), repo, number).Scan(&openedAt)
	} else if dialectFor(tx) == mysqlDialect {
		err = tx.QueryRow(ctx, `SELECT c.opened_at FROM change_tasks ct JOIN changes c ON c.id=ct.change_id
		WHERE ct.task_id=? AND ct.relation='implements' AND c.state<>'closed' AND c.reverted_by_change_id IS NULL
		ORDER BY c.opened_at,c.id LIMIT 1`, taskID).Scan(&openedAt)
	} else {
		err = tx.QueryRow(ctx, `SELECT c.opened_at FROM change_tasks ct JOIN changes c ON c.id=ct.change_id
		WHERE ct.task_id=$1 AND ct.relation='implements' AND c.state<>'closed' AND c.reverted_by_change_id IS NULL
		ORDER BY c.opened_at,c.id LIMIT 1`, taskID).Scan(&openedAt)
	}
	if errors.Is(err, ErrNoRows) {
		return time.Time{}, false, nil
	}
	return openedAt, err == nil, err
}

// followUpSubTaskTx reports whether childID was created after the parent PR's
// opened_at. It protects an already merged parent from a later follow-up close
// while preserving hk 3294/4172 for pieces filed before that PR opened.
func followUpSubTaskTx(ctx context.Context, tx transaction, parentID, childID int64, pr string) (bool, error) {
	openedAt, found, err := taskHoldOpenedAtTx(ctx, tx, parentID, pr)
	if err != nil || !found {
		return false, err
	}
	var createdAt time.Time
	err = tx.QueryRow(ctx, sqlFor(tx, `SELECT created_at FROM tasks WHERE id=$1 AND parent_id=$2`, `SELECT created_at FROM tasks WHERE id=? AND parent_id=?`), childID, parentID).Scan(&createdAt)
	if errors.Is(err, ErrNoRows) {
		return false, nil
	}
	return createdAt.After(openedAt), err
}

// pieceHoldTx is the part of mergeHoldTx about the task's own changes: it
// returns why the task may not close while one of its implements changes
// other than pr ("" for none) is open, or one is reverted and not relanded.
// An automatic close of a parent passes it too (closeGateTx).
func pieceHoldTx(ctx context.Context, tx transaction, taskID int64, pr string) (string, error) {
	h, err := taskHoldsTx(ctx, tx, taskID, pr)
	if err != nil {
		return "", err
	}
	return h.pieceReason(), nil
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// afterSourceMoveTx runs what every state change runs after an automatic
// move: the hierarchy rules. A merge — and the deploy record it applies in
// the same turn (rule 8) — defers auto-closing a parent whose own turn is
// still pending in the same event, so the parent's turn does the close.
func (s *Store) afterSourceMoveTx(ctx context.Context, tx transaction, x Task, from, by string, now time.Time, deferNamedParentClose bool) error {
	if deferNamedParentClose {
		return nil
	}
	return s.afterStateChangeTx(ctx, tx, x, from, by, now)
}

// deployRef is the source_ref of an hk.deploy event: the record key and the
// PR URL.
func deployRef(key, pr string) string { return key + "|" + pr }

// deployOnMergedTx is rule 8: whenever a task enters merged, in the same
// transaction, look for a successful deploy record that lists its PR, newest
// record key first. A match applies hk.deploy and moves the task on to
// deployed. task_events is append-only, so the deploy event is recorded here
// when it applies; applyDeployRecordTx addresses only merged tasks and never
// leaves an earlier applied=false row to flip. The PR matched is the task's
// delivering PR (taskDeliveredPRSQL: a bundle child's is its parent's).
func (s *Store) deployOnMergedTx(ctx context.Context, tx transaction, x Task, now time.Time, named, pending map[int64]bool) (Task, error) {
	if x.State != StateMerged {
		return x, nil
	}
	pr, err := deliveredPRTx(ctx, tx, x.ID)
	if err != nil || !taskEventPRRE.MatchString(pr) {
		return x, err
	}
	record, found, err := deployRecordForPRTx(ctx, tx, pr)
	if err != nil || !found {
		return x, err
	}
	body, commentKey := deployCommentDetails(record)
	ev := sourceEvent{Source: SourceDeploy, SourceRef: deployRef(record.Key, pr), PR: pr, CommentBody: body, CommentKey: commentKey, At: now, Named: named, Pending: pending, Note: "deploy record " + record.Key + " lists the PR; applied when the task entered merged"}
	res, err := s.applySourceEventTx(ctx, tx, x, ev, now)
	if err != nil {
		return x, err
	}
	return res.Task, nil
}

// deployRecordForPRTx is the newest successful record whose included_prs
// lists pr. A record is matched through its projection, or parsed from the
// document body when the projection is stale.
func deployRecordForPRTx(ctx context.Context, tx transaction, pr string) (StoredDeployRecord, bool, error) {
	if dialectFor(tx) == mysqlDialect {
		return deployRecordForPRMySQL(ctx, tx, pr)
	}
	// The record prefix is the documents_key_c range:
	// strpos(key,'deploy/') = 1 scanned the whole table, and every task that
	// enters merged calls this (hk 4923).
	lo, hi := keyPrefixBounds("deploy/")
	rows, err := tx.Query(ctx, `SELECT d.key,d.sha256,d.created_at,p.record,p.decoded,CASE WHEN p.key IS NULL THEN d.body ELSE NULL END FROM documents d LEFT JOIN deploy_record_projection p ON p.key=d.key AND p.sha256=d.sha256 WHERE d.key COLLATE "C" >= $2 AND d.key COLLATE "C" < $3 AND ((p.decoded AND strpos(convert_from(p.record,'UTF8'),$1)>0) OR (p.key IS NULL AND strpos(d.body,$1)>0)) ORDER BY d.key COLLATE "C" DESC`, pr, lo, hi)
	if err != nil {
		return StoredDeployRecord{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var record StoredDeployRecord
		var raw []byte
		var decoded *bool
		var body *string
		if err = rows.Scan(&record.Key, &record.SHA256, &record.CreatedAt, &raw, &decoded, &body); err != nil {
			return StoredDeployRecord{}, false, err
		}
		if body != nil {
			record = parseStoredDeployRecord(record.Key, record.SHA256, *body, record.CreatedAt)
		} else {
			if err = json.Unmarshal(raw, &record.Record); err != nil {
				return StoredDeployRecord{}, false, err
			}
			record.Decoded = *decoded
		}
		prs, ok := deployedPRs(record)
		if !ok {
			continue
		}
		for _, listed := range prs {
			if listed == pr {
				return record, true, nil
			}
		}
	}
	if err = rows.Err(); err != nil {
		return StoredDeployRecord{}, false, err
	}
	return StoredDeployRecord{}, false, nil
}

// deployRecordKeyForPRTx preserves callers that need only the record key.
func deployRecordKeyForPRTx(ctx context.Context, tx transaction, pr string) (string, error) {
	record, found, err := deployRecordForPRTx(ctx, tx, pr)
	if err != nil || !found {
		return "", err
	}
	return record.Key, nil
}

// deployRecordedPRsTx answers which of prs some deploy record lists, in one
// statement for the whole set. It is deployRecordKeyForPRTx batched: the same
// predicate per PR (the body, or the projection's record), the same membership
// test (deployedPRs), so a page of PRs costs one read instead of one scan per
// PR (hk 5058). The result holds the listed PRs only.
func deployRecordedPRsTx(ctx context.Context, tx transaction, prs []string) (map[string]bool, error) {
	listed := map[string]bool{}
	if len(prs) == 0 {
		return listed, nil
	}
	wanted := make(map[string]bool, len(prs))
	for _, pr := range prs {
		wanted[pr] = true
	}
	lo, hi := keyPrefixBounds("deploy/")
	var query string
	var args []any
	if dialectFor(tx) == mysqlDialect {
		query, args = deployRecordsForPRsMySQL(lo, hi, prs)
	} else {
		query, args = deployRecordsForPRsPG(lo, hi, prs)
	}
	rows, err := tx.Query(telemetry.Label(ctx, "deploy_record.listed_prs"), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var record StoredDeployRecord
		var raw []byte
		var decoded *bool
		var body *string
		if err = rows.Scan(&record.Key, &record.SHA256, &record.CreatedAt, &raw, &decoded, &body); err != nil {
			return nil, err
		}
		if body != nil {
			record = parseStoredDeployRecord(record.Key, record.SHA256, *body, record.CreatedAt)
		} else {
			if err = json.Unmarshal(raw, &record.Record); err != nil {
				return nil, fmt.Errorf("decode deploy record projection: %w", err)
			}
			record.Decoded = *decoded
		}
		recordPRs, ok := deployedPRs(record)
		if !ok {
			continue
		}
		for _, pr := range recordPRs {
			if wanted[pr] {
				listed[pr] = true
			}
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return listed, nil
}

// deployRecordsForPRsPG is the PostgreSQL statement behind deployRecordedPRsTx:
// deployRecordForPRTx's predicate, once per PR, joined by OR. $1 and $2 bound
// the deploy/ key range; each PR is bound once and used in both of its
// predicates.
func deployRecordsForPRsPG(lo, hi string, prs []string) (string, []any) {
	args := []any{lo, hi}
	preds := make([]string, 0, len(prs))
	for _, pr := range prs {
		args = append(args, pr)
		n := len(args)
		preds = append(preds, fmt.Sprintf(`((p.decoded AND strpos(convert_from(p.record,'UTF8'),$%d)>0) OR (p.key IS NULL AND strpos(d.body,$%d)>0))`, n, n))
	}
	return `SELECT d.key,d.sha256,d.created_at,p.record,p.decoded,CASE WHEN p.key IS NULL THEN d.body ELSE NULL END FROM documents d LEFT JOIN deploy_record_projection p ON p.key=d.key AND p.sha256=d.sha256 WHERE d.key COLLATE "C" >= $1 AND d.key COLLATE "C" < $2 AND (` + strings.Join(preds, " OR ") + `)`, args
}

// deployRecordsForPRsMySQL is the MySQL statement behind deployRecordedPRsTx,
// with deployRecordForPRMySQL's predicate once per PR.
func deployRecordsForPRsMySQL(lo, hi string, prs []string) (string, []any) {
	args := []any{lo, hi}
	preds := make([]string, 0, len(prs))
	for _, pr := range prs {
		preds = append(preds, "(LOCATE(?,d.body)>0 OR (p.decoded=TRUE AND JSON_CONTAINS(CAST(p.record AS JSON),JSON_QUOTE(?),'$.included_prs')))")
		args = append(args, pr, pr)
	}
	return "SELECT d.`key`,d.sha256,d.created_at,p.record,p.decoded," + deployRecordBodyFallbackMySQL +
		" FROM documents d LEFT JOIN deploy_record_projection p ON p.`key`=d.`key` AND p.sha256=d.sha256" +
		" WHERE d.`key`>=? AND d.`key`<? AND (" + strings.Join(preds, " OR ") + ")", args
}

func deployCommentDetails(record StoredDeployRecord) (body, key string) {
	sha := ""
	if record.Record.HeadSHA != nil {
		sha = *record.Record.HeadSHA
	}
	if sha == "" && record.Record.DeployedRef != nil && gitSHARE.MatchString(*record.Record.DeployedRef) {
		sha = *record.Record.DeployedRef
	}
	shortSHA := sha
	if len(shortSHA) > 7 {
		shortSHA = shortSHA[:7]
	}
	if shortSHA == "" {
		shortSHA = "unknown"
	}
	tag := ""
	if record.Record.DeployedRef != nil && *record.Record.DeployedRef != sha {
		tag = *record.Record.DeployedRef
	}
	body = fmt.Sprintf("Deployed %s on %s", shortSHA, record.Record.Service)
	if tag != "" {
		body = fmt.Sprintf("Deployed %s (%s) on %s", shortSHA, tag, record.Record.Service)
	}
	key = sha
	if key == "" && record.Record.DeployedRef != nil && *record.Record.DeployedRef != "" {
		// No commit id: the deployed ref (a tag such as v1.2) is the deploy's
		// identity shared by every service's record for it; the per-service
		// record key would write one comment per service.
		key = *record.Record.DeployedRef
	}
	if key == "" {
		key = record.Key
	}
	return body, key
}

// applyDeployRecordTx is the hk.deploy event: a document stored under a
// deploy record key whose body is a successful record with included_prs
// moves every merged task whose refs.pr is listed to deployed. A failed
// result, a null included_prs or an invalid record moves and records nothing,
// and so does a listed PR whose task is not merged (a reverted PR, or a merge
// not yet reported: rule 8 applies the record when the task enters merged).
func (s *Store) applyDeployRecordTx(ctx context.Context, tx transaction, record StoredDeployRecord, now time.Time) error {
	key := record.Key
	prs, ok := deployedPRs(record)
	if !ok || len(prs) == 0 {
		return nil
	}
	if err := s.lockTx(ctx, tx, 8241873001); err != nil {
		return err
	}
	if err := s.lockTx(ctx, tx, taskCommentInsertLock); err != nil {
		return err
	}
	var tasks []Task
	if dialectFor(tx) == mysqlDialect {
		args := []any{StateMerged}
		for _, pr := range prs {
			args = append(args, pr)
		}
		var err error
		tasks, err = srcEvLockedTasksMySQL(ctx, tx, `SELECT t.id FROM tasks t WHERE t.state=? AND (`+srcEvDeliveredPRMySQL+`) IN (`+placeholders(len(prs))+`) ORDER BY t.id`, args...)
		if err != nil {
			return err
		}
	} else {
		rows, err := tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks t WHERE t.state=$2 AND `+taskDeliveredPRSQL+` = ANY($1) ORDER BY t.id FOR UPDATE`, prs, StateMerged)
		if err != nil {
			return err
		}
		tasks, err = collectRows(rows, func(row row) (Task, error) {
			var x Task
			err := scanTask(row, &x)
			return x, err
		})
		if err != nil {
			return err
		}
	}
	for _, x := range tasks {
		pr, err := deliveredPRTx(ctx, tx, x.ID)
		if err != nil {
			return err
		}
		body, commentKey := deployCommentDetails(record)
		ev := sourceEvent{Source: SourceDeploy, SourceRef: deployRef(key, pr), PR: pr, CommentBody: body, CommentKey: commentKey, At: now, Note: "deploy record " + key + " lists the PR"}
		if _, err = s.applySourceEventTx(ctx, tx, x, ev, now); err != nil {
			return err
		}
	}
	return nil
}

// applyJobRoleTx is the panewire.job event for a job record that gained a
// role: the task bound to the job moves forward only, from researching to
// developing when the role and the task kind say so.
func (s *Store) applyJobRoleTx(ctx context.Context, tx transaction, j Job, now time.Time) error {
	if j.TaskID == nil || j.Role == "" {
		return nil
	}
	if err := s.lockTx(ctx, tx, 8241873001); err != nil {
		return err
	}
	var x Task
	err := scanTask(tx.QueryRow(ctx, s.q(`SELECT `+taskColumns+` FROM tasks WHERE id=$1 AND refs->>'job_id'=$2 FOR UPDATE`, `SELECT `+taskColumns+` FROM tasks WHERE id=? AND `+srcEvRefsJobIDMySQL+`=? FOR UPDATE`), j.TaskID, j.JobID), &x)
	if errors.Is(err, ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ev := sourceEvent{
		Source: SourcePanewireJob, SourceRef: j.JobID + "/role/" + j.Role, To: substateForJobRole(j.Role, x.Kind), At: now,
		Note: fmt.Sprintf("job %s has role %s", j.JobID, j.Role),
	}
	_, err = s.applySourceEventTx(ctx, tx, x, ev, now)
	return err
}
