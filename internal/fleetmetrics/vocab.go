package fleetmetrics

import "github.com/mgh3326/handoffkeep/internal/store"

// Task states appear in two vocabularies here. task_events keeps the names
// as they were written, so history from before the eleven-state model says
// claimed, in_progress, verifying, join, hold, needs_decision and reopened,
// and a task export from a server that has not migrated does too. Current
// rows and events say researching, developing, reviewing, unstarted and the
// rest. Every reader below accepts both.

const (
	oldClaimed       = "claimed"
	oldInProgress    = "in_progress"
	oldVerifying     = "verifying"
	oldJoin          = "join"
	oldNeedsDecision = "needs_decision"
	oldReopened      = "reopened"
)

// isMerged reports a completed task: merged or deployed.
func isMerged(state string) bool { return state == store.StateMerged || state == store.StateDeployed }

// isDropped reports a canceled, settled task: dropped or duplicate.
func isDropped(state string) bool {
	return state == store.StateDropped || state == store.StateDuplicate
}

// isSettled reports a task that reached its end: merged, deployed, dropped or
// duplicate.
func isSettled(state string) bool {
	return isMerged(state) || state == store.StateCompleted || isDropped(state)
}

// isReady reports a task waiting to be claimed.
func isReady(state string) bool {
	return state == store.StateBacklog || state == store.StateUnstarted || state == oldReopened
}

// isStartEntry reports the state a claim lands in.
func isStartEntry(state string) bool {
	return state == oldClaimed || state == store.StateResearching || state == store.StateDeveloping
}

// isReview reports a task under review.
func isReview(state string) bool { return state == oldVerifying || state == store.StateReviewing }

// isRework reports the state a task returns to when review finds a fix needed.
func isRework(state string) bool { return state == oldInProgress || state == store.StateDeveloping }

// isOpenWork reports open work: the started category, or the old active
// states. Backlog, unstarted and icebox are not open.
func isOpenWork(state string) bool {
	switch state {
	case oldClaimed, oldInProgress, oldVerifying, oldJoin:
		return true
	}
	return store.IsStartedState(state)
}

// isReopen reports an event that reopened a task: the old reopened state, or
// a settled or parked task returning to unstarted.
func isReopen(e store.TaskEvent) bool {
	return stateEvent(e) && (e.To == oldReopened || (e.To == store.StateUnstarted && (isSettled(e.From) || e.From == store.StateIcebox)))
}

// startsWait reports an event that began a wait on a decision: the
// awaiting_decision flag set, or a transition into the old needs_decision.
func startsWait(e store.TaskEvent) bool { return e.SetsAwaiting() }

// endsWait reports an event that ended one: the flag cleared, or a transition
// out of the old needs_decision.
func endsWait(e store.TaskEvent) bool {
	return (e.Kind == store.TaskEventAwaiting && e.ToValue == "false") || (stateEvent(e) && e.From == oldNeedsDecision && e.To != oldNeedsDecision)
}

// firstStart returns the first transition into a started state (the old
// claimed, or a claim's researching or developing) in the current work cycle.
func firstStart(t *store.Task) *store.TaskEvent {
	for i := cycleStart(t); i < len(t.Events); i++ {
		e := &t.Events[i]
		if stateEvent(*e) && isStartEntry(e.To) {
			return e
		}
	}
	return nil
}

// firstReview returns the first transition into review in the current cycle.
func firstReview(t *store.Task) *store.TaskEvent {
	for i := cycleStart(t); i < len(t.Events); i++ {
		e := &t.Events[i]
		if stateEvent(*e) && isReview(e.To) {
			return e
		}
	}
	return nil
}
