# hk as the tracker; Linear as a bounded projection

Status: approved direction, 2026-11-03, with operator Amendment 1 folded in. The
operator answered the questions below on 2026-11-04, which confirms the state table
and migration map. This note changes no runtime behavior. S0 through S6a have
shipped; [As built](#as-built) records what changed from this plan.

hk tracks work the way Linear does, adapted to a fleet where agents do the work
and events move the state. Two failures motivate this: work silently falls
through, and "open" does not mean "in progress". **Open now means `started` and
nothing else.** Backlog is not open.

## Decisions

Each item is a choice the approved direction left open. The operator took the
proposed answer for every item except Q11, where the alternative stands.

| # | Question | Decision | Not chosen |
|---|---|---|---|
| Q1 | The approved states have no paused state. Where does `hold` go? | `icebox`: unclaimable, not open, revivable | `backlog` plus a new not-claimable flag |
| Q2 | Where does `needs_decision` go? | An `awaiting_decision` flag on the task, which keeps its work state | Keep `needs_decision` as a twelfth state, contradicting "fixed sub-states" |
| Q3 | Is a `claimed` task whose job has ended still in progress? | No: it migrates to `unstarted` | Keep it `started` and let the stalled flag report it |
| Q4 | Old CLIs reject the new state names after a claim. How do we roll out? | Update the CLI on every host, then migrate the server | The server keeps sending old names to old clients, which adds a compatibility layer |
| Q5 | A parent's children are all canceled. Does the parent complete? | No: it stays where it is and the skip is recorded | The parent moves to `dropped` |
| Q6 | Does an `icebox` child count as closed for auto-close? | No: parked work is not finished | Yes, as with `dropped` and `duplicate` |
| Q7 | Does the stale-backlog sweep run as soon as it deploys? | No: it stays off until the desk reviews a dry-run count and enables it | It runs at deploy with the 14-day default |
| Q8 | S2 needs mergewake to send events to hk. Who owns that change? | A separate mergewake PR, tracked as its own task; S2 ships only the hk endpoint | The lead widens S2 to a second repository |
| Q9 | Should desk promises and follow-ups be lightweight checklist items that never count as open? | No checklist type: they are sub-tasks in `triage` or `backlog`, which are not open | A checklist on the task, as an earlier task title proposed |
| Q10 | Model additions need a slice. Where do they go? | A new slice, S1b, after S1; it needs its own task | Fold them into S1, which is already the largest migration |
| Q11 | mergewake merges on review pass and CI runs afterward. Do schema PRs under blocking paths still need CI before merge? | No: S1 and S1b follow the same post-merge flow as other PRs, and the deploy gate carries migration safety | CI before merge for S1 and S1b |
| Q12 | A repo session re-emits a ticket set with changed content. What happens? | The call is refused with a conflict and changes nothing | hk adds the new children and leaves existing ones unchanged |

## State model

`tasks.state` holds one of twelve leaf states. The category is derived from the
leaf by a fixed table and is never stored, so a category and sub-state cannot
disagree. API rows add a read-only `category` field.

| Category | State | Meaning | Open | Claimable | Leaves by |
|---|---|---|---|---|---|
| `triage` | `triage` | Arrived from an integration; not accepted yet | No | No | Triage gate |
| `backlog` | `backlog` | Accepted, not committed; the sweep can park it | No | Yes | Claim, manual move, sweep |
| `unstarted` | `unstarted` | Committed, nobody working; the sweep skips it | No | Yes | Claim, manual move |
| `started` | `researching` | A job is investigating; no change is expected yet | **Yes** | No | Event, manual move |
| `started` | `developing` | A job is producing a change | **Yes** | No | Event, manual move |
| `started` | `reviewing` | A PR is open or under review | **Yes** | No | Merge event, manual move |
| `completed` | `merged` | Merged. Final for a repository with no deploy stream | No | No | Deploy event, revert event, reopen |
| `completed` | `deployed` | A deploy record includes the merge | No | No | Revert event, reopen |
| `completed` | `completed` | Non-code work completed with a result link | No | No | Reopen |
| `canceled` | `dropped` | Will not be done | No | No | Reopen |
| `canceled` | `duplicate` | Another task covers it; requires a `duplicate_of` relation | No | No | Reopen |
| `canceled` | `icebox` | Parked; can be revived | No | No | Revive, reopen |

`backlog` and `unstarted` differ in one way that matters: the sweep parks stale
`backlog` tasks and never touches `unstarted`. `tasks next` serves both, in the
existing order, so agents that poll it keep working. `tasks add` still creates
`backlog`; integrations create `triage` (S5).

### Waiting on a decision is a flag, not a state

Waiting on the operator is independent of how far the work has progressed, so it
does not belong on the same axis (Q2). S1 adds `tasks.awaiting_decision`:

- Setting it requires a question, as `needs_decision` does today.
- A flagged task cannot be claimed, and the sweep and the stalled flag skip it.
- Answering clears it. Existing answer paths keep their guards, including the
  rule that only the operator's web answer resolves a disposition item.
- S3 replaces the flag with "an open decision request exists for this task".

### Manual transitions

| From | To |
|---|---|
| `triage` | `backlog`, `unstarted` (both through the triage gate); `dropped`, `duplicate` (reason required) |
| `backlog` | `unstarted`, `researching`, `developing`, `icebox`, `dropped`, `duplicate`, `completed` (decide, ops, verify; result link required) |
| `unstarted` | `backlog`, `researching`, `developing`, `icebox`, `dropped`, `duplicate`, `completed` (decide, ops, verify; result link required) |
| `researching` | `developing`, `reviewing`, `unstarted`, `icebox`, `dropped`, `duplicate`, `completed` (decide, ops, verify; result link required) |
| `developing` | `researching`, `reviewing`, `unstarted`, `icebox`, `dropped`, `duplicate`, `completed` (decide, ops, verify; result link required) |
| `reviewing` | `developing` (fix needed), `merged`, `unstarted`, `icebox`, `dropped`, `duplicate`, `completed` (decide, ops, verify; result link required) |
| `merged` | `unstarted` (reopen) |
| `deployed`, `completed`, `dropped`, `duplicate` | `unstarted` (reopen) |
| `icebox` | `backlog` (revive), `unstarted` (reopen) |

**Closure.** Each new settled transition records a type and evidence in `refs.closure`. `merged`, `deployed` and `completed` accept `resolved`; `dropped` accepts `cancelled`, `obsolete` or `superseded`; `duplicate` accepts `superseded`. Evidence must be 1–1024 bytes after trimming and contain no secrets. Completed requires an HTTP(S) result link or `hk:doc` reference. A closure absent on rows closed before v41 means unknown. Reopening clears it from the task while its event retains the snapshot.

There is no edge from `triage` to `started`. Moving into `started` still needs
`claimed_by` plus `refs.job_id` or a recorded no-job reason, as `claimed` and
`in_progress` require today. A task cannot enter `merged` from `researching` or
`developing` by hand; only a merge event can skip `reviewing`. No manual edge enters
`deployed`: deploy records are the strict gate for that state. The `reviewing` to
`merged` edge stays for repositories mergewake does not merge. That move takes the PR
through the existing reference flag and stores it in `refs.pr`, so a deploy record can
match it. Decide, ops and verify tasks can instead enter `completed` with a result link.

## Event-driven transitions

People rarely move state by hand. Each source event either moves the task
forward or is recorded with no effect.

| Source | Event | Moves the task to | Applies from |
|---|---|---|---|
| `panewire.job` | Spawn bound to the task | `researching` or `developing`, see below | `backlog`, `unstarted` |
| `mergewake.pr` | PR opened carrying `Mergewake-Task: <id>` | `reviewing` | `backlog`, `unstarted`, `researching`, `developing` |
| `mergewake.review` | Review started | `reviewing` | Same as above |
| `mergewake.merge` | mergewake merged the PR after its review passed | `merged` | Any `backlog`, `unstarted` or `started` state |
| `mergewake.revert` | Revert PR merged, for example after post-merge CI failed | `developing` | `merged`, `deployed` |
| `hk.deploy` | Successful deploy record lists the PR | `deployed` | `merged` |
| `hk.auto_close` | Last child closed | `merged` or `deployed`, see Hierarchy | Any non-terminal state except `triage` |
| `hk.sweep` | Backlog idle past the setting | `icebox` | `backlog` |

**Choosing the sub-state on spawn.** A spawn reaches hk first as a claim with a job
id, then as a job record that names the role. The claim sets the sub-state from the
task kind, and the job record can move it forward. A job role of `consult` or `research` gives
`researching`. Otherwise the task kind decides: `implement`, `fix` and `ops` give
`developing`; `verify` and `decide` give `researching`.

**Deploy matching.** A deploy record already carries `included_prs` and `result`
(`internal/ui/deploys.go`), so hk matches a record's PR URLs against `refs.pr` and
needs no Git ancestry check. A failed result or a null `included_prs` moves
nothing. A repository with no deploy stream stays at `merged`, which is not
treated as overdue.

**Rules every automatic transition follows:**

1. It records its source. `task_events` gains `source` and `source_ref`, with a
   unique partial index on `(task_id, source, source_ref)`, so a redelivered event
   changes nothing.
2. It only moves forward. The order is `researching`, `developing`, `reviewing`,
   `merged`, `deployed`. A late or out-of-order event is recorded with
   `applied=false` and leaves the state alone. Only a person moves a task backward.
   There are two exceptions. `hk.sweep` applies only to idle `backlog` tasks, and
   `icebox` is outside this order. `mergewake.revert` moves a completed task back,
   by rule 9.
3. It does not bypass triage. An event for a `triage` task is recorded and shown
   in the triage queue.
4. It does not revive canceled work. A merge event for a canceled task is recorded
   and appears in the ledger's stalled section so that someone looks at it.
5. A job that ends does not move the task. A `started` task with no live job
   reaches the stalled flag instead.
6. A closed PR that was not merged moves nothing.
7. It satisfies the guard on entering `started`. An event that moves an unclaimed
   task sets `claimed_by` to its source and records `source_ref` as the no-job
   reason. An existing claim and job binding stay as they are.
8. A deploy can arrive before its merge event. A deploy record applies `hk.deploy`
   only to tasks already in `merged` and records nothing for a listed PR whose task
   is not merged yet, because `task_events` is append-only and an early
   `applied=false` row would hold the `(task_id, source, source_ref)` key. Whenever a
   task enters `merged`, in the same transaction, hk looks for a successful deploy
   record that lists the task's PR. If it finds one, the task moves on to `deployed`
   and one applied `hk.deploy` row is recorded with the source_ref the deploy path
   would use (record key and PR URL), so either arrival order ends with one row. A
   merge event sets `refs.pr` to the merged PR, so the match uses the PR that
   actually merged.
9. A revert undoes a merge. The event names the reverted PR and the revert PR. hk
   moves the task to `developing`, records the revert PR in `refs.revert_pr`, and
   applies rule 7's claim provenance when the task has no live job. The task then
   reaches the stalled flag unless someone picks it up. A later deploy record listing
   only the reverted PR moves nothing. A parent that auto-closed stays closed, and the
   event is recorded on it.
10. A reverted PR cannot merge the task again. The `source_ref` of a merge event is
    the PR URL, and that of a revert event is the revert PR URL. A revert event always
    records the reverted PR, even when it arrives early and has `applied=false`. Before
    applying a merge event, hk checks for a revert row naming that PR. If one exists,
    the merge is recorded with `applied=false`. This covers a revert that arrives
    before its merge and a merge event redelivered after a revert. Relanding the change
    takes a new PR.

When the merge event moves a task to `merged`, that says the change merged, not that
CI passed. CI runs after the merge, and failure arrives as a revert. How mergewake
decides to revert is its own contract; hk only receives the event.

Moving a task out of `started` by event or sweep does not resolve an open decision
on it. S3 defines when a decision is superseded.

## Hierarchy and relations

- **Parent and sub-tasks.** `tasks.parent_id` is a single-parent, acyclic link.
  There is no separate checklist type. It is unrelated to the existing
  `parent_lane` and `refs.origin_task`.
- **Relations.** `task_relations` holds `blocks`, `duplicate_of` and `related`.
  Edges are unique. `related` is symmetric and stored in normalized order.
  Self-links and `blocks` or `duplicate_of` cycles are refused. Repeated adds and
  removes converge.
- **Duplicates.** Adding `duplicate_of` and entering `duplicate` happen in one
  transaction; neither exists without the other.
- **`blocks` is informational.** It appears in task detail and the ledger. It does
  not gate a claim and no relation edit transitions a task, except `duplicate_of`.

**Auto-close** (`auto_close_parent`, default on; Linear's `autoCloseParentIssues`)
runs in the transaction of the child's transition, once every child is settled. A
child is settled when it is `merged`, `deployed`, `dropped` or `duplicate`. Any child
in `triage`, `backlog`, `unstarted`, a `started` state or `icebox` blocks auto-close.

| Children | Parent result |
|---|---|
| Any not settled, including `icebox` (Q6) | No change |
| At least one completed, all completed ones `deployed` | `deployed`, which each such child reached through a deploy record |
| At least one completed, some only `merged` | `merged`; a later deploy event can advance it |
| All `dropped` or `duplicate` (Q5) | No change; a `hk.auto_close` event is recorded with `applied=false` |

The Linear schema says only that the parent closes when its sub-issues are closed,
and does not name a target state, so the table above is hk's own rule. Reopening a
child records an event on a completed parent and does not reopen it. Auto-close
does not close child tasks when a parent closes.

## Triage intake

Worker follow-ups, review findings deferred to a task, dots requests and Linear
app mentions land in `triage`. Intake is idempotent on `UNIQUE(source, source_ref)`:
a repeat returns the existing task. A task leaves triage for `backlog` or
`unstarted` only with a priority above 0, a project and a kind. Declining to
`dropped` or `duplicate` needs only a reason. Every exit records `triaged_at` and
`triaged_by`.
`tasks.kind` is required today, so S5 adds an unset value that is valid only in
`triage`.

`tasks add` checks for near-duplicates among `started`, `unstarted` and `backlog`
tasks using Postgres trigram or full-text matching, and recallbeam once it is
deployed. When it finds any, it shows them and requires `--link <id>` or `--new`.

### Ticket sets

Repo-session tickets are the main intake. The operator researches and decides in a
disposable repo session, which emits tickets into hk and is then thrown away, in the
style of `to-tickets`: tracer-bullet vertical slices with blocking edges. Because the
session is gone afterward, hk must hold everything the work needs, and one call must
either land the whole set or nothing.

`POST /v1/ticket-sets` and `handoffkeep tickets emit` take one JSON document:

```json
{
  "source": "repo-session",
  "source_ref": "<set id chosen by the session>",
  "parent": {"title": "...", "body": "..."},
  "children": [
    {"ref": "a", "title": "...", "body": "..."},
    {"ref": "b", "title": "...", "body": "...", "blocked_by": ["a"]}
  ]
}
```

- **Sections.** Each ticket body must have the headings `Current`, `Desired`,
  `Key interfaces`, `Out of scope` and `Acceptance`. A ticket missing one is refused.
  The body is stored as the task's `body_doc`. mergewake already reads `Acceptance`
  from a linked task's body as the closed contract when a PR has none.
- **Atomic.** One transaction creates the parent, the children, their parent links
  and their `blocks` edges. A `blocked_by` entry names a `ref` in the set or an
  existing task id. An unknown reference or a cycle refuses the whole call.
- **Idempotent.** The parent uses `(source, source_ref)` and each child uses
  `(source, source_ref + "/" + ref)`. hk stores a hash of the payload. Repeating the
  same payload returns the same ids. A changed payload is refused with
  `ticket_set_conflict` and changes nothing (Q12).
- **One triage item.** The parent and children all start in `triage`. The triage
  queue and the ledger show the set as one row: the parent's title, its child count
  and its age. A child appears on its own only after it leaves triage.
- **Accepting a set.** The triage gate on the parent can apply to the whole set in
  one transaction. Children inherit priority, project and kind unless they set their
  own. Declining the parent declines every child still in `triage`.
- **Duplicate check.** A ticket set does not stop for `--link` or `--new`, since no
  one is left to answer. Similar tasks are listed on the triage row instead.

## Model additions

The operator's direction is to grow hk's model from the reference projects: add what
serves the fleet now and prune it later if it proves unnecessary. Each addition below
records its source, and each lands in the slice shown. Paths are relative to each
reference repository, at the commits listed under Evidence.

| Addition | hk shape | Source | Slice |
|---|---|---|---|
| Relations with soft-delete | `task_relations.archived_at`; removing an edge sets it and adding the edge again clears it | Linear `IssueRelation.archivedAt`, `clients/web/notes/schema.graphql` L10370 | S1 |
| Activity history | `task_events` gains `field`, `from_value`, `to_value` and `auto`. It already has Linear's from/to shape, so it stays the one history table | Linear `IssueHistory`, `schema.graphql` L9791–L9870 | S1 for state; S1b for other fields |
| Lifecycle timestamps | `tasks.started_at`, `completed_at`, `canceled_at`, `triaged_at`, `started_triage_at` | Linear `Issue`, `schema.graphql` L9138–L9150 | S1 |
| Projects | `task_projects` gains `status`, `start_at` and `target_at` | Linear `Issue.project`, `schema.graphql` L9188; Asana `/project_set_start_and_due_date`, `extracted/lunadb-mutation-paths.txt` L894 | S1b |
| Milestones | `project_milestones(project, name, description, target_at, status, sort_order)`, `tasks.milestone_id` | Linear `ProjectMilestone`, `schema.graphql` L15752; `ProjectMilestoneStatus` L15935 | S1b |
| Labels | `labels(name, color, parent_id, is_group, retired_at, edit_level)`, `task_labels` | Linear `IssueLabel`, `schema.graphql` L10101. See the note on `edit_level` | S1b |
| Due dates and SLA | `tasks.due_at`, `sla_started_at`, `sla_medium_risk_at`, `sla_high_risk_at`, `sla_breaches_at`. Risk is a comparison at read time, and passing a time changes visibility only | Linear `Issue`, `schema.graphql` L9152–L9160; `SlaStatus` L19042; Slack `saved.*`, `extracted/web-api-methods.txt` L266–L273 | S1b |
| Subscribers | `task_subscribers(task_id, subscriber, source, created_at, archived_at)` | Linear `Issue.subscribers`, `schema.graphql` L9240; Asana `/conversation_add_follower`, `lunadb-mutation-paths.txt` L186; Slack `subscriptions.thread.add`, `web-api-methods.txt` L312 | S1b |
| Status updates with health | `status_updates(project, health, body, snapshot, diff, created_by, created_at)`; health is `on_track`, `at_risk` or `off_track`, set by a person; staleness is computed at read time | Linear `ProjectUpdate`, `schema.graphql` L16588–L16629; `ProjectUpdateHealthType` L16697; Asana `/status_update_conversation_set_color_status`, `lunadb-mutation-paths.txt` L1067 | S4 |
| Ticket sets | See Ticket sets | `mattpocock/skills` `skills/engineering/to-tickets/SKILL.md` | S5 |

**`edit_level` has no reference source.** It comes from the operator's list. A search
of all four reference repositories for `edit level` and `editLevel` found nothing, and
the nearest material is Notion's permission groups
(`extracted/api-endpoints.txt` L59). hk defines it as a closed set: `anyone`, `desk`
or `operator`, the lowest level allowed to attach or remove the label. Label policy
actions stay a later unit.

**Notion adds little.** Its extracted surface here is mostly endpoint names, and a
search of `extracted/api-endpoints.txt` and `comparison/` found no task history,
follower or comment model to adopt. No addition depends on it beyond the note above.

## Declarative automation

There is no rules engine. Each setting is one value:

| Setting | Default | Effect | Skips |
|---|---|---|---|
| `backlog_icebox_days` | 14 | Moves an idle `backlog` task to `icebox` | Flagged or open-decision tasks; parents with open children |
| `started_stalled_hours` | 24 | Flags a `started` task; moves nothing | Flagged or open-decision tasks |
| `auto_close_parent` | on | See Hierarchy | None |

"Activity" and "event" mean the newest of a `task_events` row, a comment, or an
update to a job bound to the task. Stalled is a comparison at read time, so it
needs no job. The sweep records `hk.sweep` and can be undone by reviving the task.
It stays off until the desk enables it (Q7), because the first run would otherwise
park every backlog task older than 14 days at once.

## Decision requests

One object, a `decision_requests` row, mirrors each question held for the operator
in six places. The `source` column names the place and `source_ref` the record:

1. `task_flag`: the `awaiting_decision` flag set without a structured request
   (the old `needs_decision`). `source_ref` is `<task id>/<awaiting event id>`,
   so asking again after an answer is a new row.
2. `job_escalate`: a `job.escalate` event.
3. `lane_event`: a `lane.event` beginning `[decision-needed]`. It closes only on an
   exact `[decision-answered] #<id>:` reply from the same owner lane
   (`ListOpenLaneDecisions`).
4. `task_request`: the structured `refs.decision_request`; `source_ref` is the
   request id.
5. `chat`: a `chat_questions` row.
6. `linear`: a request posted as a Linear issue. Once the issue exists, it is where
   the request is answered, so its `task_request` row is superseded by the
   `linear` row (`superseded_by` `linear:<request id>`): one question, one open row.

Disposition items are the subset of the first source that only the operator's web
answer can resolve; they keep that rule.

Each writer upserts its row in its own transaction, keyed by `(source,
source_ref)`, so a redelivery adds nothing. Each existing answer path marks the row
answered, with who and when, in the same transaction, and only when the source
itself stops counting the question open. The source's record stays the authority.

A row is superseded when its task reaches a completed or canceled state (`icebox`
included), when its job reaches `completed` or `lost`, or when a `job.completed`,
`job.joined`, `job.lost` or `job.revoked` event arrives for its job (the events that
close an escalation). A new request revision supersedes the one it names, and a
withdrawn request or chat question is superseded as `withdrawn`. Nothing else closes
a row, and superseding a row never changes its source.

A row has a snooze and an expiry. `POST /v1/decision-requests/{id}/snooze {until}`
hides an open row from the default list until that time. Passing `due_at` or
`expires_at` changes how a row is listed (`overdue`, or status `expired`), never its
stored status or its source: nothing applies a default on its own, which is the
existing invariant in `internal/store/decision_request.go`. No source sets
`expires_at` yet. `GET /v1/decision-requests` and `handoffkeep decisions list` read
the rows, oldest first.

Questions already open when this ships get no row. **The 118 open lane asks are not
closed by this work.** `handoffkeep decisions backfill --dry-run` reads every
question each source counts as open (`GET /v1/decision-requests/sources`, read
only) and classifies it, first rule first: `orphan` (names no task or job hk knows),
`superseded_candidate` (its task is completed or canceled, or its job completed or
lost), `live` (its task is started, or its job claimed or spawned),
`stale_candidate` (older than 14 days), else `live`. It prints a report and writes
nothing. Closing any item waits for an explicit operator OK.

## Desk ledger

`GET /v1/ledger?consumer=desk` and `handoffkeep ledger` return a server-computed
view on the existing [change feed](sync-feed.md). No second feed is created. The
sections and their order are fixed:

1. **In flight:** `started` tasks, each with its last event and next trigger.
2. **Waiting on operator:** open decision requests.
3. **Stalled:** `started` tasks past the setting, plus recorded events that need a
   look (rule 4 above).
4. **Triage:** the queue and its oldest item, with each ticket set as one row.
5. **Changed since the last ack:** the feed delta from this consumer's cursor.

A task appears in one of the first three sections: a task waiting on a decision is
listed only there, and a stalled task only under Stalled. Each consumer has a read
cursor, and `handoffkeep since` shows the delta. Reading never advances the cursor;
only an explicit ack does, so a session that compacts before acting sees the same
changes again. On `410 {"reset":true}`, the client rebuilds from the current view.

## Migrating existing states

| Current state | Condition | New state | Other change |
|---|---|---|---|
| `backlog` | | `backlog` | |
| `reopened` | | `unstarted` | A reopen was deliberate, so the sweep must not park it |
| `claimed` | Live job or a recorded no-job reason | Sub-state by the spawn rule | |
| `claimed` | Job ended or missing (Q3) | `unstarted` | Clears `claimed_by`; keeps `refs.job_id` until the next claim |
| `in_progress` | | Sub-state by the spawn rule | |
| `verifying` | | `reviewing` | |
| `join` | | `reviewing` | The Linear mapping already treats it as In Review |
| `merged` | | `merged` | Not backfilled to `deployed`; S2 applies only to new deploy records |
| `dropped` | | `dropped` | |
| `hold` | (Q1) | `icebox` | |
| `needs_decision` | Disposition item | `backlog` | Sets `awaiting_decision`; the operator-only rule still applies |
| `needs_decision` | Other, with a live job | Mapped from the state it left | Sets `awaiting_decision` |
| `needs_decision` | Other, no live job | `unstarted` | Sets `awaiting_decision` |

"A live job" means a `jobs` row for the task in `claimed` or `spawned`. "The state
it left" comes from the `from` of its newest transition into `needs_decision`.

**History stays as written.** `task_events` keeps its old state names. Readers of
history must accept both vocabularies: `internal/fleetmetrics` looks for
`claimed` and `verifying`, and `ListMergedTaskEvents` for `merged`.

**Code that must change with S1:**

- `TaskTransitions`, `tasks_state_check`, and the README diagram, which a test
  checks against the table.
- Claim and `NextTask`, which test for `backlog` or `reopened`.
- `internal/remote/remote.go`, which rejects a claim response not in `claimed` or
  `in_progress` (see Rollout).
- `LinearTaskStateMapping` and `internal/store/linear_board.go`, which uses
  `reopened` as its re-open signal. The signal becomes the reopen event.
- `internal/ui/dashboard.go`, `internal/store/dashboard.go`, `disposition.go`,
  `store.go` queries, and `web/console/src/shared/states.ts`.
- The export contract in `docs/contract.md`, whose `by_state` lists every canonical
  state.

| hk state | Linear state | Terminal |
|---|---|---|
| `triage`, `backlog`, `unstarted` | Backlog | No |
| `researching`, `developing` | In Progress | No |
| `reviewing` | In Review | No |
| `merged`, `deployed` | Done | Yes |
| `dropped`, `duplicate`, `icebox` | Canceled | Yes |

A flagged task sends no state update, which matches `needs_decision` today. The
dry-run counts how many mirrored `hold` tasks would show as Canceled.

**Old names on input.** The API and CLI keep accepting old names so that existing
callers, such as a mergewake sink configured with `in_progress`, keep working:

| Old input | Treated as |
|---|---|
| `in_progress` | The spawn rule's sub-state |
| `verifying`, `join` | `reviewing` |
| `hold` | `icebox` |
| `needs_decision` | Set `awaiting_decision` |
| `claimed` | The claim path |
| `reopened` | The reopen path |

State filters on list and export accept the old names too. Responses use only new
names.

### Migration mechanics

The migration takes the next schema version when it merges. #124 currently claims
32, so S1 expects 33 and renumbers if needed. (S1 took 32; see As built.) It runs in one transaction:

1. Creates `task_state_migration(task_id, legacy_state, legacy_claimed_by, new_state, rule)`,
   recording every changed row.
2. Adds the columns and the new state values to the constraint.
3. Updates `tasks` by the map and appends one `task_events` row per changed task,
   with a new kind `state_migration` and `source='migration'`. Consumers that read
   `to` as a state already filter by kind.
4. Drops the old values from the constraint.

The `ALTER TABLE` on `tasks` takes `ACCESS EXCLUSIVE`, and the update emits one
`sync_log` row per changed task; S1 states both costs from dry-run counts. The
migration enqueues no Linear operations. The reconciler corrects any mirrored issue
afterward, subject to projection policy.

**Dry-run.** `handoffkeep tasks migrate-states --dry-run` prints counts per
`(legacy_state, rule, new_state)` and writes nothing. The S1 PR body carries the
counts from a test database. Production counts come from the desk, which runs the
command as part of the rollout.

### Rollout and rollback

Each schema PR carries this runbook in its body. Production migrations run only at
deploy, by the desk, with the operator's approval.

Schema PRs get no CI before merge (Q11), so this runbook is where migration safety
sits. A migration first runs in production at deploy, so reverting a schema PR before
then undoes it completely. After deploy, only the rollback below undoes it.

0. **Confirm CI on the merged commit.** The migration and rollback round-trip tests
   need a database, so CI on main is the first place they are certain to have run.
   Do not deploy until they pass there.
1. **Update clients first (Q4).** The S1 CLI accepts old and new names in responses.
   Install it on every host and confirm the version. Until then, an old CLI reports
   failure on a claim that the server has already applied.
2. **Run the dry-run** and compare the counts with the PR body's expectations.
3. **Deploy the server.** The migration runs at startup.
4. **Verify:** counts by state match the dry-run, a claim succeeds on each host, and
   no task holds an old state name.

**Rollback.** A rollback script does the following, then the previous binary is
restored:

1. Restores `state` and `claimed_by` from `task_state_migration` for each task whose
   state has not changed since the migration.
2. Maps every other task, including tasks created after the migration, by the table
   below.
3. Restores the old constraint and removes the new schema version's row.

| New state | Legacy state |
|---|---|
| Any state with `awaiting_decision` set | `needs_decision` |
| `triage`, `backlog` | `backlog` |
| `unstarted` | `reopened`, `completed` (decide, ops, verify; result link required) |
| `researching`, `developing` | `in_progress` |
| `reviewing` | `verifying`, `completed` (decide, ops, verify; result link required) |
| `merged`, `deployed` | `merged` |
| `dropped`, `duplicate` | `dropped` |
| `icebox` | `hold` |

The script appends one `state_migration` event per changed task and does not rewrite
earlier `task_events`. Events written under the new model keep their new names, so
old metrics do not count transitions from that window. The added columns and tables
stay in place unused; the previous binary names its columns and ignores them.

Anything recorded only in the new model is lost: sub-state precision, `triage`,
`deployed`, and the `duplicate` distinction. The S1 PR tests the round trip against
this table.

## Slices

One PR per slice, in order. mergewake merges on review pass (Q11), and the desk
deploys. There is no coordinator lane: READY and ESC go to the desk.

| Slice | Tier | Scope |
|---|---|---|
| S0 | T1 | This document |
| S1 | T2 | State categories and sub-states, parent and sub-tasks, relations, auto-close, migration of existing rows |
| S1b | T2 | Model additions: projects, milestones, labels, due dates and SLA, subscribers, other-field history (Q10) |
| S2 | T2 | Event-driven transitions including merge and revert from mergewake, stalled flag, auto-icebox setting (Q8) |
| S3 | T1 | Decision requests; dry-run classification of the 118 asks, report only |
| S4 | T1 | Ledger view, consumer cursor, `handoffkeep ledger`, status updates with health |
| S5 | T1 | Triage intake API, ticket sets, duplicate check at `tasks add` |
| S6 | T2 | Discord answer path: a console link, then buttons. The design goes to the desk before any code |

The earlier seven-unit plan maps onto this as follows: unit 1b's admission has
shipped (#120), and its archival PR stays as planned under Projection policy. Unit 2
(relations) moves to S1, and intake from unit 3 and unit 6 moves to S5. Due dates and
label storage move to S1b, and status updates to S4. Deadline projection, releases,
weekly digests and label policy actions are outside these slices and remain planned as
written below; they need their own go-ahead.

## As built

S0 through S6a merged between 2026-11-04 and 2026-11-05. Read the plan above for the
reasoning and this section for what shipped. Where they differ, this section is right.

| Slice | PR | Schema | Follow-ups |
|---|---|---|---|
| S0 | #126, #127, #128 | none | |
| S1 | #132 | 32 | |
| S1b | #133 | 33 | #134 and #135: milestone edits no longer overwrite or silently clear other fields |
| S2 | #136 | 34 | #137: `tasks event` keeps the values of single-dash flags |
| Lock order | #138 | none | Not planned. Store locks are taken in one documented order, and `Open` skips migration DDL when the schema is current |
| S3 | #139 | 35 | |
| S4 | #141 | 36 | #143: the ledger places every started task, including one behind an expired request or past the 1000-row cap |
| S5 | #142 | 37 | |
| S6a | #144 | 38 | |
| S6b | not started | | Held for a separate operator approval |

`internal/store/migration.go` lists the function that owns each schema version.

**Deploys.** Release v0.0.14 shipped S1, S1b, S2, #137 and #138 at schema 34. Release
v0.0.20 shipped S3, S4, #143, S5, S6a and a CI change (#140) at schema 38. In both, the CLI went to
every host before the server started.

### Differences from the plan

- **Schema numbers.** #124 closed without merging, so S1 took 32, not 33. S5 was written as 36,
  then renumbered to 37 after S4 merged first. S6a was written as 37 and moved to 38
  after S5 merged.
- **S6 split in two.** S6a is outbound only: one Discord webhook message per open
  decision request, edited when the request closes, and a daily digest. Each message
  links to the console, whose base URL is `HANDOFFKEEP_CONSOLE_URL`. A failed first
  post is retried only when Discord shows it created nothing (a 5xx, a 429 or a
  failed dial). A process that stops between the post and its commit can still post
  a request twice. S6b adds answer buttons, which need an interactions endpoint. It waits for
  its own operator approval.
- **Duplicate check.** `tasks add` uses `pg_trgm` title similarity over backlog,
  unstarted and started tasks and answers `409 similar_tasks` unless the caller passes
  `--link <id>` or `--new`. There is no FTS fallback, because `pg_trgm` is already
  required. Recallbeam search is not wired in.
- **Decision backfill.** S3 classified the open lane asks in a dry-run report and
  closed none of them. Closing waits for an explicit operator OK.
- **The mergewake sender.** The hk endpoint shipped in S2. mergewake sends `pr`,
  `review`, `merge` and `revert` events from its own repository, in a separate PR (Q8).
- **The deploy gate needs links.** A deploy record moves a `merged` task to `deployed`
  only when `included_prs` lists full PR URLs and the task's `refs.pr` is one of them.
  A task that carries only `refs.job_id` is not matched, so the desk links
  `refs.pr` and writes a correction record.
- **Client first (Q4).** A host still on a CLI older than S1 rejected the new state
  names in claim responses. Step 1 of the rollout is not optional.

## Evidence and current boundaries

hk owns tasks, task events, documents, structured decisions, placement records,
and delivery receipts. Linear remains an operator's mobile view and a channel
for requesting work from external assistants. An issue is a projection or an
intake source; it is never the sole durable record of work.

State and automation semantics come from `linear-ref-analysis` commit
`29c629d3872209d169c15c9b628a6c5d86614f9b`, in `clients/web/notes/schema.graphql`:
`triageEnabled` and `requirePriorityToLeaveTriage` (L19729), `autoClosePeriod`,
`autoCloseStateId` and `autoCloseParentIssues` (L19796), and `IssueRelationType`
(L10433). hk leaves out Linear's `similar` relation. Typed requests and
server-computed views come from `asana-ref-analysis` commit
`a452fbe153d2d351a4c17ffa90647307603f1eff` (`ActionRequest`, `ConfirmedQuery`).
Model additions also use `slack-ref-analysis` commit
`1527e86406904d4329f52d3968044ccc334ae493`, `notion-ref-analysis` commit
`944de3f78d515ac7f261ef292372c3c0c52c21e2`, and `mattpocock/skills` commit
`4ae2317ddb22b046e26455e4f9c9b3aaaf62212d`.

Required reference reading, pinned to `linear-ref-analysis` commit
`800c44bad3b1ed1df6036a01b46c5f2d6879dd59`:

- [Reusable patterns](https://github.com/mgh3326/linear-ref-analysis/blob/800c44bad3b1ed1df6036a01b46c5f2d6879dd59/comparison/reusable-patterns.md):
  observed client patterns, with application conditions and evidence limits.
- [Comparison index](https://github.com/mgh3326/linear-ref-analysis/blob/800c44bad3b1ed1df6036a01b46c5f2d6879dd59/comparison/INDEX.md):
  separates evidence sources and confidence; Windows remains unanalyzed where
  the index says so.
- [Web API surface](https://github.com/mgh3326/linear-ref-analysis/blob/800c44bad3b1ed1df6036a01b46c5f2d6879dd59/clients/web/notes/api-surface.md):
  observed task, triage, due-date, label, owner and delegate fields. Workspace
  observations do not establish plan entitlements.
- [Web sync engine](https://github.com/mgh3326/linear-ref-analysis/blob/800c44bad3b1ed1df6036a01b46c5f2d6879dd59/clients/web/notes/sync-engine.md):
  bootstrap, model deltas, transaction journal and partial loading. Some write
  transport and binary-format conclusions remain inferred; hk need not copy
  the protocol, binary codec, CRDT or browser database.

[#114](https://github.com/mgh3326/handoffkeep/pull/114) provides the transactional
work-board outbox and reconciler, and
[#117](https://github.com/mgh3326/handoffkeep/pull/117) provides the console shell
and task views. [#115](https://github.com/mgh3326/handoffkeep/pull/115) provides
the change feed: reuse it; do not create a second feed.
The ROB-1484 screen design is available for relations, triage/deadlines,
releases, weekly digests and label policy at desktop 1280 and mobile 390. Follow
the saved layouts plus the revised draft's corrections: established priority
names without invented numeric mapping, separate task/PR state, PR number and
exact due time. Mark console PRs `design: ROB-1484`. Where touched, keep a visible
priority name in mobile rows and accessible row labels, and add a compact
status/priority summary under the task-detail title. The design review's source
findings do not substitute for rendering the implemented console.

Task creation with `--linear` and `dots request` is separately in progress.
The existing decision webhook, operator checks and decision request identity
must be reused.

## Capability map

“hk” means canonical data, with a Linear copy only under the projection policy.
“Keep” means retain the external interaction; “Drop” means no replacement in
this effort.

| Linear capability | Disposition and hk equivalent | Delivery / evidence |
|---|---|---|
| Issue title, description, state, priority, project | hk task and event history; console board/detail | Existing; states change in S1; projection gate required |
| Workflow state categories | Fixed categories and sub-states; not configurable per team | S1 |
| Automatic state changes from Git and deploys | Source events with recorded provenance | S2 |
| Parent and sub-issues | hk `parent_id`, one parent per child, navigable detail | S1 |
| Blocks, blocked by, duplicate, related issues | hk `task_relations` | S1 |
| Auto-close parent; auto-close and auto-archive periods | `auto_close_parent`, `backlog_icebox_days`, `started_stalled_hours` | S1, S2 |
| Triage | hk `triage` state and intake API | S5 |
| Due dates, SLA and overdue lists | Task `due_at` and SLA timestamps; reuse decision `DueAt`; console overdue view | S1b; Linear projection planned separately |
| Project milestones | `project_milestones`, `tasks.milestone_id` | S1b |
| Subscribers | `task_subscribers` | S1b |
| Releases / issue-to-release links | Release rows derived from `deploy/<service>/<timestamp>` documents, with task/PR links | Planned, outside S0–S6; hk-only |
| Project status updates | `status_updates` with health; weekly hk document from merged PRs, deploys, open decisions and assistant results | Status updates in S4; weekly digest planned, with optional Linear update |
| App mentions / assignment | Keep Linear entry point; authenticated operator action links or creates hk triage task | S5 |
| Bulk ticket creation with blocking edges | Ticket sets from repo sessions | S5 |
| Labels and policy | hk labels with edit level; later, mirror, archive and priority rules editable in console | Storage in S1b; policy actions planned; enforce policy in writers |
| Assignee, delegate, agent activity | hk responsible owner distinct from executing identity, using existing task/placement/job provenance | Preserve now; do not invent a user-directory service |
| Human decision issue and comment answer | hk decision request and notification outbox; keep Linear phone interaction | S3; existing [decision contract](linear-decisions.md) |
| Assistant request and result | hk task/document/notification history; keep Linear assistant interaction | Existing webhook + separately owned request work |
| Comments, event timeline, PR/review links | hk task comments/events/refs and existing placement projection | Existing; preserve user-authored Linear text |
| Documents / attachments / search | hk documents and attachment store, console document browser/search | Existing; no bulk copy to Linear |
| Boards, list views, filters, command navigation | hk console workspace, project/state/priority filters; desk ledger | Existing #117; ledger in S4 |
| Mobile interaction and notifications | Keep Linear mobile and existing notifications; hk outbox receipts remain authoritative | No native mobile client in this effort |
| Bootstrap and incremental reads | hk #115 model feed and current-row fetch, console revalidation | Existing; no parallel sync engine |
| Offline mutation queue | Durable server outboxes, visible retry/failure state | Preserve existing paths; full browser offline editing deferred |
| Custom views / favorites / templates | Keep optional personal Linear views; hk uses existing saved navigation | Drop parity work in this effort |
| Cycles, estimates, SLA automation, initiatives, roadmaps | Keep existing Linear use if needed | Drop new hk implementations without a separate requirement |
| Rich collaborative editing, presence, desktop shell | Existing hk document/task editing is sufficient here | Drop protocol and client parity work |
| Customer/support integrations and analytics | Keep independently configured services | Drop replication into hk |

## Projection policy

Evaluate eligibility from committed hk state and authenticated intake provenance.
A task is eligible when at least one of these holds:

1. It is an explicit assistant request, including a `dots request`.
2. It carries a structured operator decision that is mirrored to Linear.
3. It is explicitly operator-visible, or has hk priority 1 or 2. Priority 0, if
   present as an unset value, must not accidentally satisfy the numeric filter.
4. The operator opened, mentioned or assigned the hk app on its Linear issue.
   Record that provenance and reuse that exact issue; do not create a second one.

Never mirror ordinary priority 3+ internal work without one of those reasons:
worker/review/consult bookkeeping, fixture/test tasks, placement/session events,
bulk imported history, scratch notes, telemetry, and documents are not reasons
by themselves. A relation, parent, PR link or project membership must not pull
an otherwise ineligible task into Linear. External payload text cannot grant
operator-visible status or impersonate the operator. Test-only records must
never reach the live writer.

All issue creation paths (legacy task sync, work board, explicit task creation,
assistant requests, decisions, webhook intake, reconciliation and retry) must
share the policy decision. Existing link updates are allowed for maintenance;
removing eligibility does not delete history or recreate an archived issue.
An archived issue is reopened only by a fresh qualifying operator/request action
and capacity admission. A terminal task is not freshly mirrored merely because
its historical priority qualifies; reuse an existing issue for its final update.

Use a pure eligibility result with reason, plus a separate capacity admission:
`mirror eligibility -> reuse link or admit creation -> durable outbox -> receipt`.
Recheck current policy immediately before a queued create: an old payload must
not bypass a later exclusion. Before reusing a ROB id, reserve one-to-one task/issue ownership; refuse
another task's issue or a decision issue, and retain historical conflicts for
explicit repair. Each saved projection advances its version. Late snapshots
use the newest committed fields while retaining their event/comment identity;
per-task outbox ordering serializes delivery.

A duplicate delivery uses the existing task/issue link and stable id; it does not consume another issue slot. A relations writer
never creates endpoint issues.

### hk-owned limit management (ships flag-off; desk enables after merge)

hk is to be the sole Linear writer and own limit management. Before each issue
create, and after board or state changes, hk compares the non-archived issue
count with the configured watermark. The existing daily Dagu archival job stays
as a backstop. Unit 1b is a PR series: admission (eligibility plus serialized capacity checks,
with unknown counts pending) ships first. The following PR implements archival
behind a disabled flag; enabling it awaits the confirmation below.

The [published Free plan](https://linear.app/pricing) lists **250 issues and two
teams**. The operator tested this workspace and confirmed **275 active issues**;
use that observed entitlement as the configurable hard limit. Archived issues
are outside the active quota and unlimited. Defaults are **270** for creation
(five-slot margin) and **230→200** for the watermark backstop. The issue limit
and both watermarks are environment/policy configuration; lowering the hard limit
to 250 yields a 245 create ceiling. A vendor capacity rejection must persist an
at-limit hold even when a prior visible count appeared below the configured
ceiling; unknown scope/counts stay pending.

Count the non-archived issues in the plan's applicable scope,
including closed issues. hk is to be the sole writer; any out-of-band writes
are outside the serialization guarantee. Do not interpret
“active” as only unfinished workflow states. If scope/count cannot be established,
leave new creates pending and report uncertainty. Serialize capacity admission
across writers and recover ambiguous creates by stable id before releasing a
reservation when this later limit-management unit is implemented.

An early archive candidate must satisfy **all** of these: it is closed, has no
link from open hk tasks, open decisions or ongoing dots requests/assistant threads,
has no recent activity,
and has a verified durable copy in hk. Keep issues linked to open work, recently
referenced issues, and issues with a `keep` label. Retain original identifiers,
content and timestamps in the copy; copy failure cannot authorize archival.

At 230, archive eligible closed issues oldest first until at most 200 remain,
using closure time then issue id as deterministic ordering and paging the whole
scope. If protected/recent issues prevent reaching 200, keep creates pending and
notify the operator. Re-running must converge without duplicate copies. Retain
hk links after archival. The idle/recent-reference window is configurable; enabling archival and
verifying the deployed credential scope belong to the desk.

**Resolved archive-access question:** archived issues remain in full search and
their links work; paginated GraphQL reads hide them unless
`includeArchived:true`. Editing needs unarchive. These are the separate
investigation's findings; exact search-function defaults, every external view,
restore-at-capacity behavior and relation visibility remain unverified. Preserve
relation types/endpoints and parent data in the hk copy rather than infer loss
from a filtered view.

The desk confirmed the archive design. Implement early archival after a short,
configurable idle period when every protection above passes, with the 230→200
watermark as the hard backstop. Ship flag-off; only the desk enables it after
merge. Historical paginated reads must include archived resources; quota and
candidate inventory reads intentionally count/select non-archived issues. The
existing daily archival job remains a backstop. No automatic unarchive or
replacement issue creation is implied by opening a history link.

The retired immediate-archive path is
`internal/store/linear.go:enqueueLinearTaskTransition`: terminal transitions
append `LinearOpTerminalArchive` after state/comment operations.
`Store.CreateTask`, `Store.claimTaskTx` and `Store.transitionTask` use that
legacy queue only with `LinearSyncEnabled()` and `refs.linear.sync`.
`EnableLinearSync` and `ProcessNextLinearOutbox` have no production caller in
the inspected tree; the live work-board worker drains only `work_board`.
**Approved disposition:** unit 1b stops enqueueing immediate terminal archives
and settles old queued terminal-archive operations as skipped, leaving archival
exclusively to the watermark owner and daily backstop. Terminal status alone is
insufficient under the new eligibility rule.

Urgent unit 1b applies admission to the live board before S1, with tests
that ineligible tasks create no issue. Relation projection is relation-only between
existing mirrored endpoints and never creates endpoint issues. The later labels unit
moves the shared policy's configurable selections into labels; it must not delay the
bounded-create guard until the end of the rollout. Archival activation is gated on the confirmation above.

## Later units, outside S0–S6

### Relation projection

The board projects relations only when both endpoints already have eligible issue
links. Missing links leave the edge in hk; a later board reconciliation can
project it. Deletions remove only relations managed by hk, preserving external
relations. A retry uses stable relation identity and verifies ambiguous writes.
Relation mutations keep the existing protected-job write authorization.

### Deadline projection

S1b stores task `due_at` as a nullable UTC timestamp with a field-scoped set/clear
operation. Decisions already have `DecisionRequest.DueAt`; reuse it and the existing
overdue classifier. Passing a deadline changes visibility, not approval, resolution
or default action. Map mirrored decision timestamps to Linear calendar dates in a
documented time zone; hk retains the exact timestamp. Clearing a deadline clears its
projection.

### Releases and digests

A release has a stable source document key, repository, commit, deployment time
and task/PR associations. Upsert by source key; keep the original deploy document
and distinguish a merge from deployment evidence. Invalid or incomplete deploy
records remain inspectable without invented associations. The release view links
to its source and shipped tasks. Do not run deployments from this view.

A weekly digest uses a stable project/week key and explicit interval/time zone,
source links and generation timestamp. Page all sources; mark missing or partial
inputs instead of treating absence as zero. Re-generation updates the same hk
document. A Linear project update is optional and idempotent. The plan investigation and [official pricing](https://linear.app/pricing)
confirm Releases are excluded from Free, so releases stay hk-only without Linear
release mirroring. Plan changes would require a separate check.

### Linear intake and policy labels

S5 reuses the signed webhook, delivery-id deduplication and `operatorActed` gate for
issue mentions/assignment. Verify both actor and target app; text alone cannot
authorize intake. Reuse an existing link before creating a triage task and queue
its notification in the same transaction. Redelivery and webhook loops must not
create tasks or notifications twice. Ordinary reads of Linear are not intake.

Store label definitions, task membership and policy meaning separately from
presentation. A closed set of policy actions interprets labels; arbitrary label
text cannot become executable rules. Console edits use existing authenticated
write gates and leave an audit event. Invalid/conflicting policy is visible and
fails closed for new creates; labels never bypass human approval or job guards.

## Applying the observed patterns

- **[§1: field-scoped mutation](https://github.com/mgh3326/linear-ref-analysis/blob/800c44bad3b1ed1df6036a01b46c5f2d6879dd59/comparison/reusable-patterns.md#1-속성-단위-뮤테이션-field-scoped-mutation):**
  relations, deadline and label edits have narrow payloads and audit events.
  They must not replace unrelated task refs or a whole stale task snapshot.
- **[§4: per-entity worker queue](https://github.com/mgh3326/linear-ref-analysis/blob/800c44bad3b1ed1df6036a01b46c5f2d6879dd59/comparison/reusable-patterns.md#4-엔티티별-워커-큐):**
  extend existing typed outboxes for board, decision and notification delivery.
  Preserve per-task order and ensure one failing entity cannot starve another.
- **[§9: policy as labels](https://github.com/mgh3326/linear-ref-analysis/blob/800c44bad3b1ed1df6036a01b46c5f2d6879dd59/comparison/reusable-patterns.md#9-정책을-라벨로-표현):**
  a later unit makes policy editable; enforcement stays at admission, reconciliation
  and archive writes. A visible label alone is not a gate.
- **[§10: agents as first-class users](https://github.com/mgh3326/linear-ref-analysis/blob/800c44bad3b1ed1df6036a01b46c5f2d6879dd59/comparison/reusable-patterns.md#10-에이전트를-1급-사용자로):**
  record authenticated actor and executor separately from the responsible
  owner. Keep operator provenance checks; an agent cannot answer for a human.
- **[§11: backoff plus suspend](https://github.com/mgh3326/linear-ref-analysis/blob/800c44bad3b1ed1df6036a01b46c5f2d6879dd59/comparison/reusable-patterns.md#11-지수-백오프--서스펜드-이중-제어):**
  transient failures use bounded retry; permission/validation failures need a
  visible suspended/failed state and explicit repair. Preserve existing retry
  contracts until deliberately migrated; do not silently copy Android timings.

## Delivery and verification

No merge, deployment or production migration is part of these PRs.

Each slice exercises store/API/remote/CLI behavior with the test DB and, where it
touches Linear, a fake Linear transport. Every transition and constraint a slice adds
has a test. Run `go test -p 2` only on touched packages locally; DB tests skip when
no test database is configured and must execute in CI. Do not start a local database
or run a full Go suite on the development Mac. Run relevant Vitest tests, typecheck,
and rebuild the committed bundle for console changes. The full suite runs on a
non-Mac CI host. Review findings need an explicit fix/dismiss/defer disposition.

Projection tests cover each qualifying reason, exclusions, priority unset,
non-operator/bot intake, duplicate delivery, delayed create after policy change,
unmirrored relation endpoints, archived/terminal links, budget boundary,
copy-before-archive failure, and retries after ambiguous remote acceptance.
Runtime deployment and live Linear verification remain separate evidence.
