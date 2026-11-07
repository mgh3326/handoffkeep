// Package parity keeps the console write routes and the MCP tools that mirror
// them in one table. The companion test reads the console's route tables and
// fails when a write route has neither a mapped MCP tool nor a reasoned
// exception. docs/parity.md links here and carries the full capability matrix.
package parity

// ConsoleWrite is one write route reachable from the console.
//
// Source is the identity the console route table uses: a path literal, the
// name of a *PathRE regexp variable, a path constant name, or "prefix:" plus
// the argument of a strings.HasPrefix comparison. Route is the canonical path
// shown in docs/parity.md.
//
// Exactly one of MCP and Except is set. MCP names the tools that perform the
// same write; Except states why no MCP tool does, and starts with either
// "follow-up: hk <id> — " (a planned gap) or "operator-only: " (a deliberate
// exclusion).
type ConsoleWrite struct {
	Route  string
	Source string
	MCP    []string
	Except string
}

// ConsoleWrites is the map the test enforces. Add the console's route and an
// MCP tool or a reasoned exception before merging a new write route.
var ConsoleWrites = []ConsoleWrite{
	{
		Route:  "/ui/api/tasks/{id}/priority",
		Source: "taskPriorityPathRE",
		Except: "follow-up: hk 3465 — a dots priority write must carry the expected version and an idempotency key from the MCP write design.",
	},
	{
		Route:  "/ui/api/tasks/{id}/{action}",
		Source: "taskLifecyclePathRE",
		Except: "follow-up: hk 3465 — state and lane writes are planned through TransitionTask with an expected version and an idempotency key; decision_answer only closes a task while answering its request.",
	},
	{
		Route:  "/ui/api/decision-requests/resolve",
		Source: "/ui/api/decision-requests/resolve",
		MCP:    []string{"decision_answer"},
	},
	{
		Route:  "/ui/api/decision-requests/deny",
		Source: "/ui/api/decision-requests/deny",
		Except: "operator-only: a deny rejects one pending_confirmation attempt, which only the verified operator path produces; a bearer MCP caller has no attempt to deny.",
	},
	{
		Route:  "/ui/api/inbox/state",
		Source: "inboxWritePath",
		Except: "follow-up: hk 3474 — attention inbox read/done/snooze over MCP.",
	},
	{
		Route:  "/ui/api/inbox/undo",
		Source: "inboxUndoPath",
		Except: "follow-up: hk 3474 — attention inbox undo over MCP.",
	},
	{
		Route:  "/ui/api/nodes/{machine}/accepting",
		Source: "prefix:/ui/api/nodes/",
		Except: "operator-only: setting a fleet host's accepting flag controls dispatch, not task data, and stays on the console fleet page.",
	},
	{
		Route:  "/ui/dispositions/answer",
		Source: "/ui/dispositions/answer",
		Except: "follow-up: hk 3474 — disposition answers belong to the attention inbox work.",
	},
	{
		Route:  "/ui/dispositions/accept-batch",
		Source: "/ui/dispositions/accept-batch",
		Except: "follow-up: hk 3474 — bulk accept with per-item results is the MCP bulk-change design.",
	},
	{
		Route:  "/ui/dispositions/renotify",
		Source: "/ui/dispositions/renotify",
		Except: "follow-up: hk 3474 — re-notifying a recorded disposition belongs to the attention inbox.",
	},
	{
		Route:  "/ui/decisions/answer",
		Source: "/ui/decisions/answer",
		Except: "operator-only: the generic task/escalation/lane answer emits a lane event and transitions; structured requests use /ui/api/decision-requests/resolve, which maps to decision_answer.",
	},
	{
		Route:  "/ui/decisions/answer-batch",
		Source: "/ui/decisions/answer-batch",
		Except: "operator-only: the generic batch answer is superseded by structured requests; its MCP bulk equivalent is hk 3474.",
	},
	{
		Route:  "/ui/compose",
		Source: "/ui/compose",
		Except: "operator-only: compose sends one free-text lane event to a configured lane; it is a relay message, not task data.",
	},
	{
		Route:  "/ui/tasks/{id}/comments",
		Source: "taskCommentsPathRE",
		MCP:    []string{"task_comment"},
	},
}
