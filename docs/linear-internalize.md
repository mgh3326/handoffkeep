# hk as the tracker; Linear as a bounded projection

Status: proposed design, 2026-11-03. This note changes no runtime behavior.
Implementation follows seven separately reviewed units plus urgent unit 1b. Unit 3 starts only after
this note is merged and the coordinating desk gives the go-ahead.

## Evidence and current boundaries

hk owns tasks, task events, documents, structured decisions, placement records,
and delivery receipts. Linear remains an operator's mobile view and a channel
for requesting work from external assistants. An issue is a projection or an
intake source; it is never the sole durable record of work.

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

At hk base `7f92db1`, [#114](https://github.com/mgh3326/handoffkeep/pull/114)
provides the transactional work-board outbox and reconciler, and
[#117](https://github.com/mgh3326/handoffkeep/pull/117) provides the console shell
and task views. [#115](https://github.com/mgh3326/handoffkeep/pull/115) has since merged:
reuse its change feed; do not create a second feed.
Before each implementation unit's console work, inspect the separately delivered
design documents. Follow an available design; otherwise reuse existing tokens
and mark the PR `design pending ROB-1484`, without blocking work.

Task creation with `--linear` and `dots request` is separately in progress.
The existing decision webhook, operator checks and decision request identity
must be reused.

There are two current policy gaps: the board can create an issue for every
claimed/placed task, and the retired legacy terminal outbox can enqueue immediate
archival. Neither behavior establishes the target projection or retention
policy below. This note does not claim those gaps are already fixed.

## Capability map

“hk” means canonical data, with a Linear copy only under the projection policy.
“Keep” means retain the external interaction; “Drop” means no replacement in
this seven-unit effort.

| Linear capability | Disposition and hk equivalent | Delivery / evidence |
|---|---|---|
| Issue title, description, state, priority, project | hk task and event history; console board/detail | Existing; projection gate required |
| Parent and sub-issues | hk task relations, one parent per child, navigable detail | Unit 2 |
| Blocks, blocked by, related issues | hk directed dependency edges and symmetric related edges | Unit 2 |
| Triage | hk intake state for assistant results, app mentions and operator notes | Unit 3 |
| Due dates and overdue lists | Task `due_at`; reuse decision `DueAt`; console overdue view | Unit 3; decision deadline projected |
| Releases / issue-to-release links | Release rows derived from `deploy/<date>/<repo>-<sha>` documents, with task/PR links | Unit 4; Linear release copy conditional |
| Project status updates | Weekly hk document from merged PRs, deploys, open decisions and assistant results | Unit 5; optional Linear update |
| App mentions / assignment | Keep Linear entry point; authenticated operator action links or creates hk triage task | Unit 6 |
| Labels and policy | hk labels for mirror, archive and priority rules, editable in console | Unit 7; enforce policy in writers |
| Assignee, delegate, agent activity | hk responsible owner distinct from executing identity, using existing task/placement/job provenance | Preserve now; do not invent a user-directory service |
| Human decision issue and comment answer | hk versioned decision request, resolution and notification outbox; keep Linear phone interaction | Existing [decision contract](linear-decisions.md) |
| Assistant request and result | hk task/document/notification history; keep Linear assistant interaction | Existing webhook + separately owned request work |
| Comments, event timeline, PR/review links | hk task comments/events/refs and existing placement projection | Existing; preserve user-authored Linear text |
| Documents / attachments / search | hk documents and attachment store, console document browser/search | Existing; no bulk copy to Linear |
| Boards, list views, filters, command navigation | hk console workspace, project/state/priority filters | Existing #117; extend views in owning unit |
| Mobile interaction and notifications | Keep Linear mobile and existing notifications; hk outbox receipts remain authoritative | No native mobile client in this effort |
| Bootstrap and incremental reads | hk #115 model feed and current-row fetch, console revalidation | Depend on #115; no parallel sync engine |
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

### hk-owned limit management (implementation awaits confirmation)

hk is to be the sole Linear writer and own limit management. Before each issue
create, and after board or state changes, hk compares the non-archived issue
count with the configured watermark. The existing daily Dagu archival job stays
as a backstop. Unit 1b is a PR series: admission (eligibility plus serialized capacity checks,
with unknown counts pending) ships first. The following PR implements archival
behind a disabled flag; enabling it awaits the confirmation below.

The [official Free plan](https://linear.app/pricing) supports **250 issues and
two teams** (verified 2026-11-03); archived issues are outside that active quota
and unlimited, as reported by the separate plan investigation. The operational archive watermarks are 210 and 180. New creation refuses at
245, preserving five slots below the vendor ceiling; unknown counts stay pending. Count the non-archived issues in the plan's applicable scope,
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

At 210, archive eligible closed issues oldest first until at most 180 remain,
using closure time then issue id as deterministic ordering and paging the whole
scope. If protected/recent issues prevent reaching 180, keep creates pending and
notify the operator. Re-running must converge without duplicate copies. Retain
hk links after archival. Recent-activity/reference windows and full plan scope
remain configuration decisions to confirm before implementation.

**Open question: are archived Linear issues still searchable/openable by the
operator and dots?** The separate plan/archival investigation is checking this.
The hk archiver is implemented behind a flag that defaults off; enabling it
waits for the coordinating desk's confirmation after that answer. Existing daily
watermark automation is already live and copies to hk first; it remains a backstop.

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

Urgent unit 1b applies admission to the live board before unit 2, with tests
that ineligible tasks create no issue. Unit 2 is relation-only between existing
mirrored endpoints and never creates endpoint issues. Unit 7 moves the
shared policy's configurable selections into labels; it must not delay the
bounded-create guard until the end of the rollout. Archival activation is gated on the confirmation above.

## Data and ownership contracts

### Relations (unit 2)

Store canonical task edges in hk with foreign keys and audit provenance. Expose
field-scoped add/remove/list operations through `/v1`, the remote client and
`tasks relate`, and show links in console detail. Normalize `blocked_by` into the
inverse of `blocks`; normalize symmetric `related` endpoints. Parentage is a
single-parent acyclic graph; self-links and missing endpoints are rejected.
Dependency cycles are rejected, separately from parent cycles. Repeated add and
remove converge. A relation edit cannot transition, claim or reassign a task.
Preserve existing protected-job write authorization for relation mutations.

The board projects edges only when both endpoints already have eligible issue
links. Missing links leave the edge in hk; a later board reconciliation can
project it. Deletions remove only relations managed by hk, preserving external
relations. A retry uses stable relation identity and verifies ambiguous writes.

### Triage and deadlines (unit 3)

Extend the existing state map and transition validation explicitly; triage must
not be claimable as runnable work before acceptance. Store task `due_at` as a
nullable UTC timestamp with a field-scoped set/clear operation. Decisions already
have `DecisionRequest.DueAt`; reuse it and the existing overdue classifier.
Passing a deadline changes visibility, not approval, resolution or default action.
Map mirrored decision timestamps to Linear calendar dates in a documented time
zone; hk retains the exact timestamp. Clearing a deadline clears its projection.

### Releases and digests (units 4–5)

A release has a stable source document key, repository, commit, deployment time
and task/PR associations. Upsert by source key; keep the original deploy document
and distinguish a merge from deployment evidence. Invalid or incomplete deploy
records remain inspectable without invented associations. The release view links
to its source and shipped tasks. Do not run deployments from this view.

A weekly digest uses a stable project/week key and explicit interval/time zone,
source links and generation timestamp. Page all sources; mark missing or partial
inputs instead of treating absence as zero. Re-generation updates the same hk
document. A Linear project update is optional and idempotent. The plan investigation and [official pricing](https://linear.app/pricing)
confirm Releases are excluded from Free. Unit 4 therefore ships hk-only releases,
without Linear release mirroring. Plan changes would require a separate check.

### Intake and policy labels (units 6–7)

Reuse the signed webhook, delivery-id deduplication and `operatorActed` gate for
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
  unit 7 makes policy editable; enforcement stays at admission, reconciliation
  and archive writes. A visible label alone is not a gate.
- **[§10: agents as first-class users](https://github.com/mgh3326/linear-ref-analysis/blob/800c44bad3b1ed1df6036a01b46c5f2d6879dd59/comparison/reusable-patterns.md#10-에이전트를-1급-사용자로):**
  record authenticated actor and executor separately from the responsible
  owner. Keep operator provenance checks; an agent cannot answer for a human.
- **[§11: backoff plus suspend](https://github.com/mgh3326/linear-ref-analysis/blob/800c44bad3b1ed1df6036a01b46c5f2d6879dd59/comparison/reusable-patterns.md#11-지수-백오프--서스펜드-이중-제어):**
  transient failures use bounded retry; permission/validation failures need a
  visible suspended/failed state and explicit repair. Preserve existing retry
  contracts until deliberately migrated; do not silently copy Android timings.

## Delivery and verification

One PR per numbered unit, in order. Unit 1 is this document alone. Unit 1b
implements mirror admission next, followed by relations; unit 3 waits for merged unit 1 and an explicit coordinating go.
No merge, deployment or production migration is part of these PRs.

For units 2–7, exercise store/API/remote/CLI behavior with the test DB and fake
Linear transport. Run `go test -p 2` only on touched packages locally; DB tests
skip when no test database is configured and must execute in CI. Do not start a
local database or run a full Go suite on the development Mac. Run relevant
Vitest tests, typecheck, and rebuild the committed bundle for console changes.
The full suite runs on a non-Mac CI host. Review findings need an explicit
fix/dismiss/defer disposition; the coordinating desk owns independent review,
CI acceptance, merge and deployment.

Projection tests cover each qualifying reason, exclusions, priority unset,
non-operator/bot intake, duplicate delivery, delayed create after policy change,
unmirrored relation endpoints, archived/terminal links, budget boundary,
copy-before-archive failure, and retries after ambiguous remote acceptance.
Runtime deployment and live Linear verification remain separate evidence.
