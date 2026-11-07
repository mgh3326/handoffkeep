package store

import "sort"

// Task states (docs/linear-internalize.md, "State model"). tasks.state holds
// one of twelve leaf states. The category is derived from the leaf by
// taskCategories and is never stored, so a category and a sub-state cannot
// disagree.
const (
	StateTriage      = "triage"
	StateBacklog     = "backlog"
	StateUnstarted   = "unstarted"
	StateResearching = "researching"
	StateDeveloping  = "developing"
	StateReviewing   = "reviewing"
	StateMerged      = "merged"
	StateDeployed    = "deployed"
	StateCompleted   = "completed"
	StateDropped     = "dropped"
	StateDuplicate   = "duplicate"
	StateIcebox      = "icebox"
)

// Task state categories.
const (
	CategoryTriage    = "triage"
	CategoryBacklog   = "backlog"
	CategoryUnstarted = "unstarted"
	CategoryStarted   = "started"
	CategoryCompleted = "completed"
	CategoryCanceled  = "canceled"
)

var taskCategories = map[string]string{
	StateTriage:      CategoryTriage,
	StateBacklog:     CategoryBacklog,
	StateUnstarted:   CategoryUnstarted,
	StateResearching: CategoryStarted,
	StateDeveloping:  CategoryStarted,
	StateReviewing:   CategoryStarted,
	StateMerged:      CategoryCompleted,
	StateDeployed:    CategoryCompleted,
	StateCompleted:   CategoryCompleted,
	StateDropped:     CategoryCanceled,
	StateDuplicate:   CategoryCanceled,
	StateIcebox:      CategoryCanceled,
}

// TaskStates lists the twelve states in display order.
func TaskStates() []string {
	return []string{StateTriage, StateBacklog, StateUnstarted, StateResearching, StateDeveloping, StateReviewing, StateMerged, StateDeployed, StateCompleted, StateDropped, StateDuplicate, StateIcebox}
}

// canCompleteKind reports which task kinds can close without a change.
func canCompleteKind(kind string) bool {
	return kind == "decide" || kind == "ops" || kind == "verify"
}

// TaskCategory derives the category of a state; "" for an unknown state.
func TaskCategory(state string) string { return taskCategories[state] }

// IsStartedState reports whether state is open: the started category and
// nothing else. Backlog, unstarted and icebox are not open.
func IsStartedState(state string) bool { return taskCategories[state] == CategoryStarted }

// IsSettledState reports whether a task in state counts as finished work for
// auto-close and as a terminal row for relane and the open-decision scans:
// merged, deployed, dropped and duplicate. icebox is parked, not finished.
func IsSettledState(state string) bool {
	switch state {
	case StateMerged, StateDeployed, StateCompleted, StateDropped, StateDuplicate:
		return true
	}
	return false
}

// IsClaimableState reports whether claim and NextTask serve state.
func IsClaimableState(state string) bool {
	return state == StateBacklog || state == StateUnstarted
}

// settledStatesSQL is IsSettledState as a SQL list.
const settledStatesSQL = `'merged','deployed','completed','dropped','duplicate'`

// startedStatesSQL is the started category as a SQL list.
const startedStatesSQL = `'researching','developing','reviewing'`

// closureEvidenceMaxBytes bounds a closure's evidence text. applyLifecycle and
// taskClosureRules read it from here, so a reader of the state machine cannot
// describe a different bound than the store enforces.
const closureEvidenceMaxBytes = 1024

// TaskClosureRule is what the store requires to enter one settled state: the
// allowed closure types, their default, the evidence bound, and the extra
// guards.
type TaskClosureRule struct {
	Types             []string `json:"types"`
	DefaultType       string   `json:"default_type"`
	EvidenceMaxBytes  int      `json:"evidence_max_bytes"`
	RejectsSecretLike bool     `json:"rejects_secret_like"`
	// ResultLink requires the completed state's https URL or hk:doc evidence.
	ResultLink bool `json:"result_link"`
	// DuplicateOf requires a live duplicate_of relation to the task it copies.
	DuplicateOf bool `json:"duplicate_of"`
}

// taskClosureRules is the one closure requirement table. applyLifecycle and
// transitionTaskTx enforce it, and TaskStateMachine reports it.
var taskClosureRules = map[string]TaskClosureRule{
	StateMerged:    {Types: []string{"resolved"}, DefaultType: "resolved", EvidenceMaxBytes: closureEvidenceMaxBytes, RejectsSecretLike: true},
	StateDeployed:  {Types: []string{"resolved"}, DefaultType: "resolved", EvidenceMaxBytes: closureEvidenceMaxBytes, RejectsSecretLike: true},
	StateCompleted: {Types: []string{"resolved"}, DefaultType: "resolved", EvidenceMaxBytes: closureEvidenceMaxBytes, RejectsSecretLike: true, ResultLink: true},
	StateDropped:   {Types: []string{"cancelled", "obsolete", "superseded"}, DefaultType: "cancelled", EvidenceMaxBytes: closureEvidenceMaxBytes, RejectsSecretLike: true},
	StateDuplicate: {Types: []string{"superseded"}, DefaultType: "superseded", EvidenceMaxBytes: closureEvidenceMaxBytes, RejectsSecretLike: true, DuplicateOf: true},
}

// TaskStateInfo is one state's row in the derived state machine. Nothing here
// is a second copy: TaskCategory, IsSettledState, IsClaimableState,
// TaskTransitions and taskClosureRules are the same values the store enforces.
type TaskStateInfo struct {
	Name               string           `json:"name"`
	Category           string           `json:"category"`
	Terminal           bool             `json:"terminal"`
	Claimable          bool             `json:"claimable"`
	AllowedTransitions []string         `json:"allowed_transitions"`
	Closure            *TaskClosureRule `json:"closure,omitempty"`
}

// TaskStateMachine returns every state in display order with its category,
// terminal and claimable flags, allowed manual transitions and closure rule.
func TaskStateMachine() []TaskStateInfo {
	states := TaskStates()
	out := make([]TaskStateInfo, 0, len(states))
	for _, name := range states {
		targets := make([]string, 0, len(TaskTransitions[name]))
		for _, target := range states {
			if TaskTransitions[name][target] {
				targets = append(targets, target)
			}
		}
		info := TaskStateInfo{
			Name: name, Category: TaskCategory(name), Terminal: IsSettledState(name),
			Claimable: IsClaimableState(name), AllowedTransitions: targets,
		}
		if rule, ok := taskClosureRules[name]; ok {
			info.Closure = &rule
		}
		out = append(out, info)
	}
	return out
}

// TaskCategories lists the categories in display order, derived from the state
// table so the two cannot drift.
func TaskCategories() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, state := range TaskStates() {
		category := TaskCategory(state)
		if !seen[category] {
			seen[category] = true
			out = append(out, category)
		}
	}
	return out
}

// TaskTransitions is the single authoritative manual task state graph.
// README.md is checked against this table by TestTaskTransitionDocumentation.
// Event-driven moves (S2) are not edges of this table. There is no edge from
// triage to a started state, and no manual edge enters deployed: deploy
// records are the strict gate for that state.
var TaskTransitions = map[string]map[string]bool{
	StateTriage:      {StateBacklog: true, StateUnstarted: true, StateDropped: true, StateDuplicate: true},
	StateBacklog:     {StateUnstarted: true, StateResearching: true, StateDeveloping: true, StateIcebox: true, StateDropped: true, StateDuplicate: true, StateCompleted: true},
	StateUnstarted:   {StateBacklog: true, StateResearching: true, StateDeveloping: true, StateIcebox: true, StateDropped: true, StateDuplicate: true, StateCompleted: true},
	StateResearching: {StateDeveloping: true, StateReviewing: true, StateUnstarted: true, StateIcebox: true, StateDropped: true, StateDuplicate: true, StateCompleted: true},
	StateDeveloping:  {StateResearching: true, StateReviewing: true, StateUnstarted: true, StateIcebox: true, StateDropped: true, StateDuplicate: true, StateCompleted: true},
	StateReviewing:   {StateDeveloping: true, StateMerged: true, StateUnstarted: true, StateIcebox: true, StateDropped: true, StateDuplicate: true, StateCompleted: true},
	StateMerged:      {StateUnstarted: true},
	StateDeployed:    {StateUnstarted: true},
	StateCompleted:   {StateUnstarted: true},
	StateDropped:     {StateUnstarted: true},
	StateDuplicate:   {StateUnstarted: true},
	StateIcebox:      {StateBacklog: true, StateUnstarted: true},
}

var taskStates = func() map[string]bool {
	states := map[string]bool{}
	for from, targets := range TaskTransitions {
		states[from] = true
		for to := range targets {
			states[to] = true
		}
	}
	return states
}()

func taskTransitionAllowed(from, to string) bool {
	return TaskTransitions[from][to]
}

// substateForKind is the spawn rule: a claim sets the sub-state from the task
// kind. implement, fix and ops give developing; verify and decide give
// researching.
func substateForKind(kind string) string {
	switch kind {
	case "verify", "decide":
		return StateResearching
	}
	return StateDeveloping
}

// substateForJobRole refines the spawn rule with a live job's role: consult
// and research give researching, otherwise the task kind decides.
func substateForJobRole(role, kind string) string {
	switch role {
	case "consult", "research":
		return StateResearching
	}
	return substateForKind(kind)
}

// Old state names. The API and CLI keep accepting them on input and in
// filters ("Old names on input" in the design); responses use only the
// twelve states above.
const (
	legacyClaimed       = "claimed"
	legacyInProgress    = "in_progress"
	legacyVerifying     = "verifying"
	legacyJoin          = "join"
	legacyHold          = "hold"
	legacyNeedsDecision = "needs_decision"
	legacyReopened      = "reopened"
)

// StateFilter is a parsed state filter: a set of states, plus an optional
// awaiting_decision requirement for the old needs_decision name.
type StateFilter struct {
	States   []string
	Awaiting bool
}

// ParseStateFilter resolves a filter value. A current state selects itself;
// an old name selects what it maps to. ok is false for an unknown name.
func ParseStateFilter(name string) (StateFilter, bool) {
	switch name {
	case legacyClaimed, legacyInProgress:
		return StateFilter{States: []string{StateResearching, StateDeveloping}}, true
	case legacyVerifying, legacyJoin:
		return StateFilter{States: []string{StateReviewing}}, true
	case legacyHold:
		return StateFilter{States: []string{StateIcebox}}, true
	case legacyReopened:
		return StateFilter{States: []string{StateUnstarted}}, true
	case legacyNeedsDecision:
		return StateFilter{Awaiting: true}, true
	}
	if taskStates[name] {
		return StateFilter{States: []string{name}}, true
	}
	return StateFilter{}, false
}

// AwaitingDecisionKey names the awaiting_decision flag where a count or a
// filter reports it beside the states.
const AwaitingDecisionKey = "awaiting_decision"

// LegacyTaskStates lists the state names tasks used before the twelve-state
// model. task_events keeps them as written, so history readers (dwell time,
// fleet metrics) accept both vocabularies.
func LegacyTaskStates() []string {
	return []string{legacyReopened, legacyClaimed, legacyInProgress, legacyVerifying, legacyJoin, legacyHold, legacyNeedsDecision}
}

// IsLegacyStateName reports whether name is one of the old input names.
func IsLegacyStateName(name string) bool {
	switch name {
	case legacyClaimed, legacyInProgress, legacyVerifying, legacyJoin, legacyHold, legacyNeedsDecision, legacyReopened:
		return true
	}
	return false
}

// AcceptedStateNames lists every name a state filter accepts, sorted.
func AcceptedStateNames() []string {
	out := append([]string(nil), TaskStates()...)
	out = append(out, legacyClaimed, legacyInProgress, legacyVerifying, legacyJoin, legacyHold, legacyNeedsDecision, legacyReopened)
	sort.Strings(out)
	return out
}
