# hk as the tracker; Linear as a bounded projection

Status: approved direction, 2026-11-03; the state table and migration map below
await desk confirmation. This note changes no runtime behavior. No schema work
starts until the desk confirms it, including the open questions in the next
section.

hk tracks work the way Linear does, adapted to a fleet where agents do the work
and events move the state. Two failures motivate this: work silently falls
through, and "open" does not mean "in progress". **Open now means `started` and
nothing else.** Backlog is not open.

## Questions for the desk

Each item is a choice the approved direction leaves open. S1 implements the
default unless the desk answers otherwise.

| # | Question | Default | Alternative |
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

## State model

`tasks.state` holds one of eleven leaf states. The category is derived from the
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
| `completed` | `merged` | Merged. Final for a repository with no deploy stream | No | No | Deploy event, reopen |
| `completed` | `deployed` | A deploy record includes the merge | No | No | Reopen |
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
| `backlog` | `unstarted`, `researching`, `developing`, `icebox`, `dropped`, `duplicate` |
| `unstarted` | `backlog`, `researching`, `developing`, `icebox`, `dropped`, `duplicate` |
| `researching` | `developing`, `reviewing`, `unstarted`, `icebox`, `dropped`, `duplicate` |
| `developing` | `researching`, `reviewing`, `unstarted`, `icebox`, `dropped`, `duplicate` |
| `reviewing` | `developing` (fix needed), `merged`, `unstarted`, `icebox`, `dropped`, `duplicate` |
| `merged` | `deployed`, `unstarted` (reopen) |
| `deployed`, `dropped`, `duplicate` | `unstarted` (reopen) |
| `icebox` | `backlog` (revive), `unstarted` (reopen) |

There is no edge from `triage` to `started`. Moving into `started` still needs
`claimed_by` plus `refs.job_id` or a recorded no-job reason, as `claimed` and
`in_progress` require today. A task cannot enter `merged` from `researching` or
`developing` by hand; only a merge event can skip `reviewing`.

## Event-driven transitions

People rarely move state by hand. Each source event either moves the task
forward or is recorded with no effect.

| Source | Event | Moves the task to | Applies from |
|---|---|---|---|
| `panewire.job` | Spawn bound to the task | `researching` or `developing`, see below | `backlog`, `unstarted` |
| `mergewake.pr` | PR opened carrying `Mergewake-Task: <id>` | `reviewing` | `backlog`, `unstarted`, `researching`, `developing` |
| `mergewake.review` | Review started | `reviewing` | Same as above |
| `mergewake.merge` | PR merged | `merged` | Any `backlog`, `unstarted` or `started` state |
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
   `hk.sweep` is the one exception: `icebox` is outside this order, and the sweep
   applies only to idle `backlog` tasks.
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
8. A deploy can arrive before its merge event. Whenever a task enters `merged`, in
   the same transaction, hk looks for a successful deploy record that lists the
   task's PR. If it finds one, the task moves on to `deployed` and any `hk.deploy`
   row recorded with `applied=false` for that record is updated to `applied=true`.
   A merge event also sets `refs.pr` when it is empty, so the match has a PR to use.

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
| At least one completed, all completed ones `deployed` | `deployed` |
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

One object replaces the five places that hold a question for the operator:

1. Tasks in `needs_decision`.
2. `job.escalate` events beginning `[decision-needed]`.
3. `lane.event` events beginning `[decision-needed]`. These close only on an exact
   `[decision-answered] #<id>:` reply from the same owner lane
   (`ListOpenLaneDecisions`).
4. The structured `refs.decision_request`.
5. `chat_questions`.

Disposition items are the subset of the first source that only the operator's web
answer can resolve; they keep that rule.

A decision request has an answer, a snooze and an expiry. It is superseded when its
task or job goes terminal. Passing a deadline changes visibility only: nothing
applies a default on its own, which is the existing invariant in
`internal/store/decision_request.go`. Writers keep writing their current records,
and each adapter upserts a request keyed by source.

**The 118 open lane asks are not closed by this work.** S3 produces a dry-run
classification as a report. Closing any of them waits for an explicit operator OK.

## Desk ledger

`GET /v1/ledger?consumer=desk` and `handoffkeep ledger` return a server-computed
view on the existing [change feed](sync-feed.md). No second feed is created. The
sections and their order are fixed:

1. **In flight:** `started` tasks, each with its last event and next trigger.
2. **Waiting on operator:** open decision requests.
3. **Stalled:** `started` tasks past the setting, plus recorded events that need a
   look (rule 4 above).
4. **Triage:** the queue and its oldest item.
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
32, so S1 expects 33 and renumbers if needed. It runs in one transaction:

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
| `unstarted` | `reopened` |
| `researching`, `developing` | `in_progress` |
| `reviewing` | `verifying` |
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

One PR per slice, in order. The lead's PRs are gated independently; the desk merges
and deploys.

| Slice | Tier | Scope |
|---|---|---|
| S0 | T1 | This document |
| S1 | T2 | State categories and sub-states, parent and sub-tasks, relations, auto-close, migration of existing rows |
| S2 | T2 | Event-driven transitions, stalled flag, auto-icebox setting (Q8) |
| S3 | T1 | Decision requests; dry-run classification of the 118 asks, report only |
| S4 | T1 | Ledger view, consumer cursor, `handoffkeep ledger` |
| S5 | T1 | Triage intake API; duplicate check at `tasks add` |
| S6 | T2 | Discord answer path: a console link, then buttons. The design goes to the desk before any code |

The earlier seven-unit plan maps onto this as follows: unit 1b's admission has
shipped (#120), and its archival PR stays as planned under Projection policy. Unit 2
(relations) moves to S1, and intake from unit 3 and unit 6 moves to S5. Due dates,
releases, weekly digests and label policy are outside S0–S6 and remain planned as
written below; they need their own go-ahead.

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
| Due dates and overdue lists | Task `due_at`; reuse decision `DueAt`; console overdue view | Planned, outside S0–S6; decision deadline projected |
| Releases / issue-to-release links | Release rows derived from `deploy/<service>/<timestamp>` documents, with task/PR links | Planned, outside S0–S6; hk-only |
| Project status updates | Weekly hk document from merged PRs, deploys, open decisions and assistant results | Planned, outside S0–S6; optional Linear update |
| App mentions / assignment | Keep Linear entry point; authenticated operator action links or creates hk triage task | S5 |
| Labels and policy | hk labels for mirror, archive and priority rules, editable in console | Planned, outside S0–S6; enforce policy in writers |
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

### Deadlines

Store task `due_at` as a nullable UTC timestamp with a field-scoped set/clear
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
