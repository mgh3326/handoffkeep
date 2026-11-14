# hk → YouTrack write protocol (hk 3667, hk 3697, hk 3739, hk 3810)

Status: **b1, comments.** The protocol core (b0) is live for one path: a new hk
comment also gets a protocol-tracked copy on the task's YouTrack issue, for a
task enrolled in the comment canary when `HANDOFFKEEP_YT_COMMENTS_MODE=canary`
(b1a), and for a task with a verified binding when it is `on` (hk 3810). PG
stays canonical for comments; the `task_comments` row is written exactly as
before. Default mode is `off` (no op row, no YouTrack call). Design: `design/2026-11-09/hk3667-youtrack-write-path`
(v2 wins), `consult/2026-11-09/hk3667-write-path-answer`,
`decision/2026-11-09/r9-data-map-memory-inbox` §4.

YouTrack has no transactions, no conditional update and no idempotency key. hk
supplies them with a durable op log. The rules live in `internal/ytwrite`; the
Postgres log is `internal/store/yt_write_ops.go`; the fake YouTrack, the
in-memory log and the contract suites are `internal/ytwrite/ytwritetest`.

## Op row (`yt_write_ops`)

One row per logical write, committed **before** any remote call.

| Column | Meaning |
|---|---|
| `id` | op id; appears in the provenance line and the body marker |
| `principal`, `kind`, `idem_key` | unique together: the idempotency key is scoped per authenticated principal and op kind, never derived from body text |
| `payload_sha`, `payload` | what the write means (a hash, to tell a replay from key reuse) and the immutable outbound payload (JSON); the exact text sent is a pure function of the op and this payload |
| `entity_kind`, `entity_id`, `yt_target_id` | the hk entity whose writes are ordered (`task`, `3697`) and the YouTrack id written to |
| `tenant` | the YouTrack host the op was bound to when it was prepared (b1a; `''` for an op prepared without one). Part of the write's identity: it is in `payload_sha`, immutable by trigger, and only a dispatcher configured for that tenant sends or searches for the op |
| `actor`, `created_at` | who the write is attributed to, and when (rendered in the provenance line) |
| `parent_id`, `child_seq` | parent/child plan for multi-step writes; stored now, settled by the first multi-step slice (b3) |
| `state` | `prepared → dispatched → applied \| rejected \| remote_unknown` |
| `yt_result_id` | YouTrack id of the created thing (set on `applied`) |
| `reason`, `last_error`, `attempts`, `reconciles` | latest cause code and detail; dispatch count; evidence-search count |
| `next_attempt_at`, `dispatched_at`, `resolved_at` | scheduling and lifecycle times (Go clock, passed in) |

`yt_entity_map(entity_kind, tenant, hk_key, yt_id, yt_parent_id, op_id)` holds
the PG map rows an op commits **in the same transaction** as its `applied` (a
comment appended by op 17 maps `comment/op:17` to its YouTrack comment id, on
the op's tenant). One hk identity names one YouTrack id, and one YouTrack id
names one hk identity **per tenant**: a YouTrack id belongs to its instance, so
two tenants' issues may share one (hk 3912, migration
`20261113-yt-entity-map-tenant`, which backfilled each row's tenant from the op
that wrote it).

A trigger makes the table append-only: identity and payload columns never change,
a resolved row never changes, only the lifecycle's moves are allowed, nothing is
deleted.

**Migration** `20261109-yt-write-ops` (expand-only): two new tables, three partial
indexes, one trigger function. It touches no existing table and takes no lock on
one (new, empty tables only); the previous release neither reads nor writes them.

## Lifecycle

```
Submit ─► prepared ─Claim─► dispatched ─┬─ 2xx + valid id ─────────► applied
              ▲                         ├─ 4xx (not 401/408/429) ──► rejected
              │                         ├─ 429 / 401 ──────────────► prepared (later next_attempt_at)
              └─────────────────────────┤   (the request was not processed)
                                        └─ 5xx, 408, transport error, ─► remote_unknown
                                           2xx without id, crash          │
                      applied ◄── exactly one exact-body match ───────────┤
                      rejected ◄─ operator: "it did not happen" ──────────┘
```

- **One attempt per dispatch.** The client never retries a create inside the call.
- **`prepared` is never sent; `dispatched` may have been.** Dispatch ownership is a
  compare-and-set committed before the remote call, so a crash in `prepared` is safe
  to resume and a crash in `dispatched` is uncertain. A `dispatched` op older than
  `StaleAfter` becomes `remote_unknown` (a crash between the claim and the send
  looks the same, so it stays unknown too: conservative, never a resend).
  `StaleAfter` must exceed `DispatchTimeout`: `New` refuses a value at or below it,
  so a send still in flight is never recovered as stale.
- **No automatic re-create.** `remote_unknown` is left only by positive evidence or
  by an operator. Evidence: enumerate **all** comments of the target (paged, no
  assumption about order) and find exactly one whose text equals the rendered body,
  marker included. Absence, several matches, a deleted match, a marker with other
  text, or a failed search all keep `remote_unknown` (each with its own reason
  code). Search is a candidate finder, never negative proof.
- **429 honours Retry-After** as the server sent it, seconds or HTTP date, to the
  second: `next_attempt_at` is the server's deadline and no request (the POST, or
  the evidence search after a listing 429) is made before it. The only cap is a
  sanity bound for absurd values, `ytwrite.RetryAfterCap` (30 days): a longer
  wait is recorded as 30 days, never dropped. 401 waits for a repaired token. Both
  put the op back to `prepared` (a listing 429 keeps it `remote_unknown`) with a
  later `next_attempt_at`, because the request was not processed.
- **Caller cancellation never cancels a write or its reconciliation**: everything
  after the claim runs on a context detached from the caller's. Each step is
  still bounded: the remote call by `DispatchTimeout`, and recording its outcome
  by `SettleTimeout`. A settlement that cannot finish in time (the server is
  stopping, the op row is locked) is abandoned: the op stays `dispatched`, durable
  and unrecorded, stale recovery makes it `remote_unknown`, and the marker search
  applies it. It is never marked `applied` without the ledger's own commit, and
  it is never sent again.

## Per-entity ordering

An op is claimed only when the ledger admits it (else `ClaimHeld`: it stays
`prepared`, nothing is sent) and every earlier op (smaller id) on the same
`(entity_kind, entity_id)` is `applied` or `rejected`. Admission is decided in the
claim's own transaction. `Due` leaves out what `Claim` would hold or block, so a
long held or blocked queue never fills a pass. The check and the insert of a
new op both take an advisory lane lock, so an op that commits after a later op's
check cannot be passed. A `remote_unknown` op therefore holds its entity's later
writes (they answer `pending / waiting_for_earlier_op`) until it resolves. Other
entities are not held.

## What a caller gets

Every path (HTTP, MCP, CLI) gets one `ytwrite.Result`:

| Field | |
|---|---|
| `OpID` | the handle; keep it, and `Lookup` it |
| `Status` | `pending`, `applied` or `rejected`; **pending is never success** |
| `State`, `Reason` | lifecycle state and a stable cause code (`rate_limited`, `remote_unknown`, `waiting_for_earlier_op`, `http_404`, …) |
| `RetryAfter` | when to look again |
| `YTID` | the YouTrack id, only when applied |
| `Replayed` | the key already named a recorded op |

`AppendComment` returns an error only before the op row is durable (invalid request,
`ErrKeyConflict`, ledger down). Pending is promised only after durability. The same
key and payload replays the recorded result; the same key with another payload is
`ErrKeyConflict` (HTTP 409 in b1a). An empty key generates one, which cannot
deduplicate: the caller must keep the `OpID`.

## Provenance (decision §3)

REST cannot set a comment's author or time, so every hk-written comment is

```
작성: <actor> · <UTC time> · hk op <id>

<body>

<!-- hk-op:<id> -->
```

(`ytwrite.ProvenanceLine`, `ParseProvenance`, `RenderComment`; time is RFC 3339 UTC
with seconds; the time is the op's `created_at`, so a resend or a reconciliation
renders the same bytes). A caller's body may not contain `<!-- hk-op:` and an actor
may not contain ` · `, so marker-shaped text is data, never evidence.

## Kinds (hk 3864)

An op's `kind` picks its YouTrack side from the registry `Protocol.Kinds`
(`internal/ytwrite/kind.go`). A `ytwrite.Kind` has three behaviors and no more:

- `Render(op)`: the exact text written, deterministic in the op, with the op
  marker when the kind's evidence is a marker search (`issue_fields` writes a
  whole snapshot and compares all of it instead).
- `Send(ctx, remote, op, text)`: exactly one remote attempt, no retry inside.
- `Evidence(ctx, remote, op, text)`: enumerate the target and classify every
  candidate carrying the marker as an exact live match, an exact deleted one, or
  a mismatch; a failed search returns its error, with `RetryAfter` from a 429.
- `MapRows(op, ytID)`: the `yt_entity_map` rows committed with the op's
  `applied`, on every path (send, evidence, operator resolve). The comment
  kind returns the one `comment` row (`op:<id>`) it always wrote.

A kind may also refuse in `Send` before it writes anything: a
`*RefusedError` rejects the op with its reason, and a `*NotSentError` (a read
before the write failed) puts it back to `prepared` (`not_sent`, after the
reconcile backoff; a 429 or 401 waits as for a write, another 4xx rejects).
Neither is ever `remote_unknown`: nothing was sent.

Everything else is the protocol's and the same for every kind: claim and
ordering, the reply classification above, and the evidence rule (exactly one
exact live match with a valid id, nothing deleted or mismatched, is `applied`;
otherwise `multiple_matches`, `marker_deleted`, `marker_mismatch`,
`adoption_unconfirmed` (below) or `no_evidence`). An op whose kind is not registered is `rejected /
unsupported_kind` when sent, and a failed render is `unsupported_kind` (send) or
`reconcile_failed` (search). `comment_append` (`kind_comment.go`),
`issue_create` (`kind_issue.go`, below), `issue_fields` (`kind_fields.go`,
below), `issue_move` (`kind_move.go`, below), and `decision_create` and
`decision_fields` (`kind_decision.go`, below) are the registered kinds.

**Precondition: the bot sees every issue.** A kind's search (the comments of
an issue, the issues of a project with a task's `hk ID`) is complete only if
the bot account can see every issue in the mapped YouTrack projects; YouTrack
offers no way to prove a search covered issues hidden from the caller. An
issue restricted from the bot would be missed, and `issue_create` would create
a second one. The import and the legacy push make the same assumption
(handoffkeep-design, hk 3898): keep the bot in every group that can see
issues of a mapped project. A new kind registers itself from its own file (`registerKind`)
and adds its admission gate in the store (`registerYTKindGate`, below), so
kinds added in parallel edit no shared list.

The tenant's worker ledger, `Store.YTTenantLedger(tenant)`, admits a prepared op
with one condition for every kind: `o.tenant = <tenant> AND ((o.kind =
'comment_append' AND <comment gate>) OR …)`. A kind with no gate admits nothing.
The comment gate is the canary or binding check below, read from
`HANDOFFKEEP_YT_COMMENTS_MODE`.

## Remote contract (`ytwrite.Remote`)

- `CreateComment` makes exactly one attempt. A nil error with an empty or
  invalid id is a 2xx the protocol cannot trust: `remote_unknown`.
- `ListComments` returns a **complete and consistent** set or an error. An
  offset-paging adapter must use `EnumerateStableComments`: two full reads per
  attempt, accepted only when both give the same set of `(id, text, updated,
  deleted)`, at most three attempts, then `ErrInconsistentEnumeration`. One read
  of more than `MaxEnumeratedComments` (10 000) comments is
  `ErrEnumerationTooLarge`. Any listing error keeps the op `remote_unknown`
  (`reconcile_failed`); a 429 listing is a `RemoteError` whose `RetryAfter`
  delays the next search at least that long.
- Recovery is per attempt: `RecoverStale(id, attempts, cutoff)` moves a
  `dispatched` op to `remote_unknown` only if that same dispatch attempt is
  still the stale one (compare-and-set on `attempts` and `dispatched_at`).

## YouTrack adapter (`internal/youtrack/write_remote.go`)

One request per call: no retry inside the call, redirects are not followed (a
POST is never replayed), `Retry-After` (seconds or HTTP date) is passed up as
sent (an HTTP date is read against the adapter's clock, `WriteRemote.Now`; only
waits past `ytwrite.RetryAfterCap` are cut to it), and the bot key is removed from
every error text, including a body read error, before it can reach the op log.
Listings request `id,text,created,updated,deleted,author(id)`; the
author is for the comment audit (`youtrack audit`), never for evidence.

| Single POST observation | Adapter returns | Op |
|---|---|---|
| 2xx with a usable id | id | `applied` |
| 2xx without a usable id, unreadable body | `"", nil` | `remote_unknown` (`no_id_in_reply`) |
| timeout, connection loss, cancel, 3xx | transport error | `remote_unknown` |
| 408, 5xx | `RemoteError` | `remote_unknown` (`http_5xx`) |
| 401 | `RemoteError` | `prepared`, retried after the auth wait |
| 429 | `RemoteError` + wait | `prepared` until `Retry-After` (kept as sent, up to 30 days) |
| other 4xx (400, 403, 404, 409, 422, …) | `RemoteError` | `rejected` (`http_<status>`) |

## Slice b1a: the comment canary

**Gate.** `HANDOFFKEEP_YT_COMMENTS_MODE` = unset or `off` (default), `canary`,
or `on` (next section), trimmed and exactly in lower case. Any other value, and
`canary` or `on` without `YOUTRACK_URL` and `YOUTRACK_BOT_API_KEY`, stops
`serve` at startup (fail closed). `off` needs no YouTrack configuration.
`canary` and `on` need the two keys but do not turn on serve's legacy read-through
(the priority/lane overlay, YouTrack search and the lane/priority outbox):
that has its own gate, `HANDOFFKEEP_YT_READTHROUGH_MODE` (default `off`,
`docs/youtrack.md`), and the worker reads the keys itself.

**Enrollment** (`handoffkeep youtrack canary add <task> [--by NAME]`, direct DB
like `youtrack projects`; also `remove <task>` and `list`). `add` reads the
imported issue `<SHORT>-<task id>`, where `<SHORT>` is the exact
`youtrack_projects` row of the task's project (no `OTHER` fallback), and binds
the task only when the issue answers with that readable id, its summary starts
with `#<task id>` followed by a space or nothing (`#37` does not name task 3;
`#037` and `#3739` do not name task 37), it is in the mapped project, and the
old mirror link (`task_origins`) and any `yt_entity_map` task row name the same
issue on the same tenant. The binding keeps the tenant and the stable internal
issue id, so a rename or move does not change the target. Enrollment writes no
op row. Publication (`AddYTCommentCanary`) is handed the candidate it was
verified against and reads the task's mappings again in its own transaction
(after the task's lane lock, with FOR SHARE on the `youtrack_projects` and
`task_origins` rows); a project mapping, mirror link or `yt_entity_map` task row
that changed since verification refuses the enrollment (`yt_canary_conflict`):
verify again.

**Enqueue.** Every comment entry point (REST and the CLI through it, console,
MCP `task_comment`) goes through `CreateTaskComment` / `CreateMCPTaskComment`.
When the gate is `canary` and the task is enrolled, the same transaction writes
the comment, its op (principal `hk:task-comments`, key `task-comment:<comment
id>`, lane `("task", "<id>")`, target the bound internal issue id, actor the
comment author, time the comment time), its ownership row
`yt_comment_write_links(comment_id, op_id)`, and, with the task's first op, the
`yt_entity_map` task row. The op is bound to the tenant the task is enrolled on
at that moment (`yt_write_ops.tenant`). An op failure fails the comment. Text PG accepts but
the protocol cannot send (a quoted `<!-- hk-op:` marker, an actor with ` · `)
and a task whose map row names another issue commit the comment with a
**locally rejected** op; nothing is sent. Handlers never call `AppendComment`.
The YouTrack activity pull and dots replies do not enqueue.

**Projection metadata.** The REST create answer (`POST /v1/tasks/{id}/comments`,
201), the CLI's `comment` output (it prints that answer) and the MCP
`task_comment` envelope keep every key and type they had and add one object,
`youtrack_projection`:

| Field | |
|---|---|
| `status` | `none` (mode off or task not enrolled: no op), `pending` (an op will copy it), `rejected` (refused locally; nothing will be sent) or, only on an MCP replay of a comment already copied, `applied` |
| `op_id` | the op, when there is one; see it in the operator view |
| `reason` | only with `rejected`: the op's stable reason code (`invalid_comment`, `ambiguous_issue`), never its free-form error |

A fresh comment is never reported `applied`: the local commit is not a YouTrack
write. An MCP replay reports the original comment's op and creates none. The
console form answer is unchanged.

**Dispatch.** One worker in `serve` runs `Drive` every 5 s, sequentially, over
`Store.YTTenantLedger(tenant)`, the ledger scoped to the tenant `YOUTRACK_URL`
names:

- *Tenant binding.* It lists, claims and searches only ops bound to its own
  tenant. An op prepared while its task was enrolled on tenant A is never sent to
  or searched for on tenant B, even after the task is removed and enrolled again
  on B with the same internal issue id: re-enrollment never retargets an op. Such
  an op stays `prepared` (or `remote_unknown`), visible in the operator view with
  its `tenant`, until a worker on A handles it or the operator resolves it.
- *Admission in the claim.* `Claim` admits a prepared op only when the gate is
  on and the task is still enrolled with the op's issue on the op's tenant, read
  inside the claim's transaction under the task's lane lock. The canary's writers
  (`youtrack canary add|remove`) take the same lane lock, so a removal either
  commits first (the claim holds the op: `ClaimHeld`, reason `held`, it stays
  `prepared` and is never rejected for it) or waits for a claim that already
  committed. There is no separate check before the claim.
- *Fair passes.* `Due` leaves held prepared ops out before its limit (50), so
  any number of held lanes cannot crowd out an admitted lane; `remote_unknown`
  ops of the tenant are always listed, because their evidence search is
  read-only and must go on after a removal.
- *Bounded stop.* The worker uses a 3 s dispatch timeout and a 1 s settlement
  timeout, 4 s together, under serve's 5 s worker stop budget, so it returns
  before the store closes. A call in flight when serve stops finishes or times
  out (`remote_unknown`); a settlement that cannot finish in 1 s leaves the op
  `dispatched`, and the next worker's stale recovery and marker search apply it
  without a second POST. A stopping worker claims nothing new.

Two serve processes may both run the worker; the ledger's claim is the
boundary.

**No double copy.** The legacy mirror push (`youtrack sync`) skips comments
with an ownership row. The import export marks them (`hk_op_id` on
`/v1/import/comments` rows) and `youtrack/import/client.js` skips a comment by
that ownership only; a PG comment whose body merely quotes `<!-- hk-op:` is
imported like any other. The activity pull drops a YouTrack comment as hk's own
echo, whoever its author, only when both hold: its text is exactly in the
rendered form (provenance line naming op N, body, blank line, op N's marker as
the end: `ytwrite.RenderedOpID`), and op N is a comment op hk wrote to that same
issue (`Store.YTCommentEchoOp`). A comment that quotes a marker, names an op hk
does not have, or names an op written to another issue is pulled as an ordinary
comment.

**Operator view.** `GET /v1/yt/ops?state=&kind=&task=&after_id=&limit=` and
`handoffkeep youtrack ops [--state S] [--kind K] [--task N] [--after-id N] [--limit N]`
(`handoffkeep yt …` is the same command). `state` is `pending` (default:
prepared, dispatched, remote_unknown), `all`, or one state; `kind` is one
registered op kind (an unknown kind is refused). Each op shows id,
kind, lane, target, tenant, actor, state, `status` (`pending` for every unresolved
state, `remote_unknown` included), reason code, result id, attempts
and times; never the body or the key. A full page carries `next_after_id`.

**Operator resolve.** `handoffkeep youtrack ops resolve <op id> --applied <yt id>
[--adopted]` or `… --not-applied --by <name> --note <text>` posts the decision
through `POST /v1/yt/ops/{id}/resolve` (operator bearer only). It refuses an op
that is not `remote_unknown` (409 `ytwrite_state`) and never resends.

**Held-op recovery** (`store.RecoverHeldYTOps`, hk 4491). A task's
`yt_task_bindings` row admits its hk comment, `issue_fields` and `issue_move`
ops. When the row is deleted or names another issue, the ops it admitted stay
`prepared`, the claim answers `held`, and every later op on the task's lane is
blocked behind them; `yt ops resolve` accepts only `remote_unknown`. The comment
worker therefore runs a pass every 5 minutes, before the catch-up. A candidate
is a `prepared` op of one of those kinds that has been prepared for more than
1 hour (`store.YTHeldRecoveryGrace`), whose kind's write mode is on in this
process, whose tenant is the worker's own, and whose gate does not admit it. Each candidate is decided in its own
transaction under the task's lane lock, from the rows as they are then:

| Class | Test | Action |
|---|---|---|
| `held_binding` | the task has no binding | rejected, `binding_removed` |
| `held_binding_changed` | the task is bound to another issue on the op's tenant | rejected, `binding_changed` |
| `held_other_tenant` | the task's binding is on another tenant | kept: that tenant's worker may own the op |
| `held_authority_youtrack` | one of the first two, and the op's kind is canonical in YouTrack (`comments` for a comment, `tasks` for fields and moves) | kept: the op may be the only delivery of the content; the operator restores the binding or resolves the op |
| `held_mode` | the kind's write mode is not on | never read: turning the mode on releases the op |

The authority row is read `FOR SHARE` under the lane lock, so a switch to
youtrack either waits for the pass and then counts the op as resolved, or has
committed and the pass keeps the op. The pass never retargets an op and never
touches a `dispatched` or `remote_unknown` op, and it never acts on another
tenant's op. The scan leaves out the ops it would keep for a standing reason
(`held_other_tenant`, `held_authority_youtrack`), so they cannot hide a later
op; a pass examines at most 1000 ops oldest first, and the next pass reaches
the rest. Each rejection logs `ytwrite.held_op_rejected op_id= task_id=
kind= reason=`; the rejected-class alert then names the op with its reason.

**Alerts** (`handoffkeep-design` lane, outbox kind `yt_write_op`). The alert
worker raises two classes: `stuck` (an op prepared or dispatched for more
than 10 minutes, or `remote_unknown` for more than 10 minutes since its creation) and `rejected` (an op rejected, or applied with reason
`binding_conflict`). Per class and 10-minute window it writes one notice that
covers every op of the class not yet announced, for example
`[ytwrite] 2 stuck (dispatched: 1, prepared: 1) op ids 12, 15 | handoffkeep yt ops`.
A prepared op of a binding-gated kind (an hk comment, `issue_fields`,
`issue_move`) is counted under why it is held, not under `prepared`:
`held_binding` (its task has no binding), `held_binding_changed` (the task is
bound to another issue on the op's tenant), `held_other_tenant` (bound on
another tenant), `held_authority_youtrack` (the content is canonical in
YouTrack, so the pass below keeps the op) and `held_mode` (the kind's write
mode is not on in this process); an op its gate admits stays `prepared`. For
example `[ytwrite] 4 stuck (held_binding: 3, prepared: 1) op ids …`.
`yt_write_op_alerts` records each op's announcement per class, so an op is
named once; an op that a window's notice could not take (that window already
has one for the class) is named by the next window. A `remote_unknown` op is
stuck until the operator resolves it. `last_error` (the detail behind the reason:
the YouTrack message or the operator's note) is shown only to the operator's
client; other tokens get the view without it.

**Migration** `20261112-yt-write-op-alert-batch` (expand-only): the table
`yt_write_op_alerts` (no foreign key, so `yt_write_ops` takes no lock), written
only by the alert worker, and a backfill from the per-op notices of
`20261110-yt-write-op-alert` (`yt-op-<id>-pending|rejected`), so an op already
announced is not announced again.

**Migration** `20261109-yt-comment-canary` (expand-only, after
`20261109-yt-write-ops`; unreleased, so the tenant column was added to it in
place): tables `yt_comment_canary` and `yt_comment_write_links`, the column
`yt_write_ops.tenant TEXT NOT NULL DEFAULT ''` and a trigger that keeps it
immutable. Locks, all on `yt_write_ops`, which no earlier release reads or
writes: ACCESS EXCLUSIVE briefly for `ADD COLUMN` (a constant default rewrites
no row), SHARE ROW EXCLUSIVE for `CREATE TRIGGER` and for the link table's
foreign key. No lock on `tasks` or `task_comments` (no foreign key to either).
The previous release does not read or write any of these objects.

**Logs.** `ytwrite.comment_enqueued`, `ytwrite.op_state` (one per recorded
transition), `ytwrite.outcome_record_failed` (a settlement that did not finish),
`ytwrite.drive`, `ytwrite.drive_failed`, `ytwrite.comments_enabled`.

## Mode on (hk 3810)

`HANDOFFKEEP_YT_COMMENTS_MODE=on` copies the new comments of every task that
has a **binding**: one `yt_task_bindings` row, the only authority mode on
reads for where a task's comments go. The canary keeps its meaning (mode
`canary` reads only `yt_comment_canary`, as above); in mode `on` the canary set
is not read for routing, only checked for agreement. The worker runs in both
modes. b1a's Dispatch, Projection metadata and No double copy hold with
"enrolled" read as "bound"; Enqueue and Admission differ as below.

**Binding.** `yt_task_bindings(task_id, tenant, issue_id, issue_readable,
project_id, bound_by, bound_at, comments_from)`, one row per task and one task
per `(tenant, issue_id)`. It is written only by `youtrack comments-backfill`
(`Store.AddYTTaskBinding`), after the canary's verification
(`youtrack.VerifyBinding`: the enrollment rules above, and the task's
`youtrack_projects` row must carry the YouTrack project id). A task with no hk
project (hk 3897) verifies the same way against the import's bucket: issue
`HK-<task id>` in the one YouTrack project whose short name is `HK`, resolved
from the project list at the start of each run (no such project, or two,
refuses the whole run with exit 2). hk keeps no `youtrack_projects` row for
`HK` (a reserved short name). Publication
reads the task's mappings again under its locks and refuses
(`yt_binding_conflict`) when they differ from the verified ones, when any
source below disagrees, or when the task is bound elsewhere; the same binding
again is a no-op. A binding writes no op and no map row, and old comments are
never copied: `comments_from` is the task's highest comment id, read in the
publication's transaction, and only a comment above it is ever copied.

**Cross-checks.** Each source hk keeps, when present, must agree with the
binding (`ytBindingDisagreement`, plain reads, no network):

| Source (log `source=`) | Disagrees when |
|---|---|
| `task_origins` | the task's YouTrack mirror link names another issue, or another non-empty tenant |
| `yt_entity_map_task` | the task's map row names another issue |
| `yt_entity_map_issue` | a map row gives the issue to another task |
| `yt_comment_canary_task` | the task's canary row names another issue or tenant |
| `yt_comment_canary_issue` | a canary row gives the tenant's issue to another task |
| `yt_task_bindings_issue` | another task's binding holds the issue (publication and preflight only) |
| `youtrack_projects` | the task's project is unmapped or maps to another YouTrack project id, or a task with no project is bound to a project some `youtrack_projects` row maps |

**Enqueue.** In the comment's transaction, after the comment lock and the
comment's KEY SHARE on its task: the task's lane lock, then the binding. No
binding: no op and no link, so the legacy push and the import carry the
comment as before. A disagreeing source: no op and no link either, and one
`ytwrite.comment_skipped_ambiguous task_id=<id> comment_id=<id>
source=<source>` line (no body, no credential). Otherwise the op, its link and
the first op's map anchor are written as in the canary, with the binding's
tenant and issue. An identity problem never makes a rejected op with a link,
which would leave the comment on no path; an anchor that still conflicts after
the checks fails the comment. A body or actor the protocol cannot send keeps
b1a's locally rejected op and link.

**Catch-up** (hk 3897). **On by default in mode `on`**: it runs before each
`Drive` pass unless `HANDOFFKEEP_YT_COMMENTS_CATCHUP=off` (unset or `on` runs
it; any other value fails startup). `off` still runs the issue-create
catch-up for `op_applied` bindings (`CatchUpYTIssueComments`, hk 3914). The
default flipped with the removals (hk 4426): the legacy comment push that
could post a comment and end without linking it no longer exists, and nothing
else delivers a comment now, so a comment with no op would stay in PG only.
While comments are canonical in YouTrack the pass prepares nothing (hk 4122).
When it runs, the worker in mode on runs `Store.CatchUpYTComments`: up to 200
comments (`ytCommentsCatchUpLimit`),
oldest first, on a bound task, above the binding's `comments_from`, not
written by the YouTrack pull, not already mirrored (`youtrack_comment_links`
is frozen since the push's removal, but its rows still count), with no
ownership link and no op under
`task-comment:<id>`. These are comments written after the binding that the
enqueue did not copy: a writer that does not enqueue (a dots reply) or a
comment written while the mode was not `on`. Each gets, in its own
transaction, exactly what the enqueue would have written (`ytCommentOnOpTx`),
with the op's `created_at` the pass's time; the transaction reads the comments
authority row `FOR SHARE` first, then takes the task's
lane lock and reads the link and the op again, and the op's unique
`(principal, kind, idem_key)` keeps two workers to one op. It logs
`ytwrite.comment_catchup task_id=<id> comment_id=<id> op_id=<id>`; a
disagreeing source gets no op and `ytwrite.comment_catchup_skipped_ambiguous
task_id=<id> comment_id=<id> source=<source>`, once per worker process (the
worker leaves the comment out of its later passes; a restart looks again). A
failed pass logs `ytwrite.comment_catchup_failed`. No YouTrack call.

**The catch-up's sync lock.** No legacy push runs any more, but a pass that
finds comments still tries the sync lock (`pg_try_advisory_lock(8241873404)`,
`TryYouTrackSyncLock`) and works only while it holds it: the lock is what an
authority switch to youtrack tries first (`SetYTAuthority`), so no switch
commits in the middle of a pass, and each comment's transaction reads the
authority row `FOR SHARE` besides. A held lock defers the pass
(`ytwrite.comment_catchup_deferred comments=<n> reason=sync_running`, nothing
written) — another catch-up pass or a previous release's sync can hold it —
and the pass holds the lock until it ends, running each comment's
transaction on the lock's own connection (no second pool connection, so a
pool of one cannot hang). Both sides only try the lock, never wait for it,
so the lock order is unchanged.

**Legacy sync is refused while the protocol owns writes (hk 3907).** The
legacy push and the adoption are a second writer to the same YouTrack objects,
so `youtrack sync` (with or without `--dry-run`) and `youtrack adopt` exit 5
before any store or YouTrack call while `HANDOFFKEEP_YT_COMMENTS_MODE` is
`canary` or `on`, or `HANDOFFKEEP_YT_ISSUES_MODE` is `on`. The stderr line
names the variable. An unparsable value of either is exit 2, so the guard
fails closed. With both off, the commands run as before. The other
subcommands keep their behaviour: `ops`, `schema plan`, `comments-preflight`,
`audit` without `--enqueue`, `comments-backfill`, `canary`, `projects` and
`schema apply` do not push task or comment content, and `audit --enqueue` writes
ops through the protocol. This retires the residual above that waited for hk 3907.

**Admission.** In mode on the worker admits a prepared op only when it is
bound to the worker's tenant and its task has a binding with the op's issue
on that tenant, read in the claim under the lane lock that the binding's
writer also takes. `Due` lists by the same predicate. A canary-era op whose
task has no matching binding is held, not rejected; mode `canary` admits by
the canary set again.

**Issue ownership.** `AddYTTaskBinding` and `AddYTCommentCanary` both take
the issue ownership lock, then FOR UPDATE on the task row, then the lane lock,
and each refuses what the other's table says otherwise: an enrollment whose
binding names another issue or tenant, or an issue bound to another task, is
`yt_canary_conflict`. A `canary add` from the previous release takes none of
this; the enqueue cross-checks catch what it could write (Rollout).

**Commands.** Both open the database directly, like `youtrack canary`, and
verify against the tenant the worker uses (the host of `YOUTRACK_URL`).

- `handoffkeep youtrack comments-preflight [--project P] [--days N] [--all]
  [--task N]` is read-only: it reads issues from YouTrack and writes nothing. It opens the
  database without migrating, on connections whose every transaction is READ
  ONLY, and refuses (exit 2) a database that does not record migrations
  `20261110-yt-task-bindings` and `20261111-yt-binding-comments-from`: deploy
  the release first. Candidates are
  the tasks with a comment in the last N days (default 14), every task in the
  canary set and every task with an unresolved comment op; `--all` makes every
  task a candidate (bind-all, hk 3897; `--days` then sets only the comment
  volume window, and the report says `"all": true`); `--project` keeps one hk
  project's. A task with no project is reported under project `""`. A bound task is checked locally only and comes out
  `bound` or `bound_ambiguous` (a source disagrees, or it is bound on another
  tenant); an unbound one is verified in YouTrack and
  comes out `verifies`, `no_issue` (no project, an unmapped project or one
  with no YouTrack id, no such issue), `ambiguous` (any other refusal, or a
  source that disagrees with the would-be binding) or `error` (a failed
  read). The JSON report has, per hk project, a count and up to 10 task ids
  for each class; `comments_total` and `max_comments_per_hour` (comments an
  hk entry point wrote in the window, in all and in the busiest UTC hour);
  and `unresolved_ops_uncovered`, the unresolved comment ops (count, up to 10
  op ids) that mode on would hold because their task has no consistent
  binding on this tenant for the op's issue. Each `bound_ambiguous`,
  `no_issue` and `ambiguous` task logs `youtrack.comments_classified
  task_id=<id> class=<class> detail="..."` to stderr, and each `error` task
  `youtrack.comments_classify_failed task_id=<id> err="..."`.
- `handoffkeep youtrack comments-backfill [--project P] [--days N] [--all]
  [--task N] [--by NAME]` classifies the same way and publishes each `verifies`
  binding. A refused publication counts as `ambiguous` and the run goes on; the
  report adds `bound_now`. A re-run reads nothing from YouTrack for a bound
  task. `--task N` (both commands; not with `--project` or `--all`) makes that
  one task the only candidate, so the operator binds exactly the task they
  unbound; the report says `"task": N`.
- `handoffkeep youtrack comments-unbind <task> [--by NAME] [--reason TEXT]
  [--force]` removes the task's binding (hk 4490). It opens the database
  directly and calls no YouTrack. A task with no binding answers `"removed":
  false` and writes nothing. It refuses, writing nothing, while an op of the
  task's lane is `dispatched` or `remote_unknown` (an uncertain write may have
  landed on the old issue: `yt ops resolve` it first), while the task has an
  unresolved `issue_create` op, while the task is enrolled in the comment canary
  (which routes comments without the binding: `canary remove` it first), and,
  with comments canonical in YouTrack,
  while a hk comment op is `prepared` unless `--force`. Otherwise, in one
  transaction, it rejects every `prepared` op of the lane that the binding
  admits (the hk `comment_append`, `issue_fields` and `issue_move` ops on the
  binding's tenant and issue, and a comment queued with no target; not an op of
  another tenant or issue, which stays for the recovery pass, and not a
  document comment's copy, which reads its article binding) with reason
  `binding_removed` (`unbind_forced` under `--force`), deletes the binding, drops the
  task's pending issue intent (so the issue worker cannot bind it again),
  deletes the task's `yt_entity_map` row when it names the binding's issue on
  the binding's tenant (hk 4536; `"anchor_released": true`) and appends an
  `unbound` row to `yt_binding_history`. It retargets nothing and leaves
  `task_origins` and the canary alone. The result lists
  `rejected` ops and the `released` comment ids (hk keeps them; the comment
  gets no op) or, under comments authority `youtrack` with `--force`, the
  `stranded` ones (nothing will carry them). `comments-backfill --task N`
  binds the task again once its other records agree with the issue it
  verifies. The map row is the routing anchor the task's first op wrote, so
  releasing it is what lets a verified issue other than the old one bind; the
  old issue stays on record in the `unbound` history row and in the ops. The
  mirror link and the canary row have their own owners, so a replacement that
  is a different issue is still refused naming the source that disagrees
  (`task_origins`) until its owner reconciles it, and so is a map row that
  names another issue or tenant (`yt_entity_map_task`). A task unbound before
  hk 4536 keeps its map row, and unbinding it again writes nothing. A later
  comment on an unbound task gets no op. A binding published after an unbind
  also appends one `task` `U` row to the change feed (`sync_log`), whatever
  fields mode the CLI process runs in, so serve's `task_fields` consumer
  prepares the task's `issue_move` and `issue_fields` ops for the new binding
  (hk 4539); with serve's fields mode off it prepares nothing, as for any
  change made while off. Run it only while every release
  that can serve (a blue-green overlap, a rollback target) is this one or
  later: an earlier release's issue ownership check does not read
  `yt_binding_history`, and the map row it would otherwise find is released
  with the binding, so its project change would prepare an `issue_create`
  for an unbound task that has no mirror link or canary row.
- Each YouTrack read has its own 15 s timeout and each task 60 s; there is no
  budget for the whole run. A task in class `error` makes either command exit
  1 after the report.

**Volume.** The worker carries at most one `Drive` at a time, every 5 s
(`ytCommentsInterval`); a `Drive` that outlasts the tick is followed by the
next one at once. A pass lists up to 50 due ops (`ytCommentsPassLimit`), at
most one per task lane (the lane guard), and `Drive` repeats the pass, up to
16 times, while a pass resolves something, so one `Drive` moves at most 800
ops, and a burst on one task at most 16. The POSTs are sequential and each
is bounded by the 3 s dispatch timeout. So the 5 s tick does not bound a
burst on distinct tasks; the dispatch round trip and the database time of
each op do: about 18 000 to 36 000 ops per hour at the 100 to 200 ms the read
path measured, and 1 200 per hour at the floor, every POST timing out. A
burst on one task moves at most 16 ops per tick (11 520 per hour). Compare
`max_comments_per_hour` from the preflight; no limit was changed. A 429
returns that op to `prepared` until the `Retry-After` the server sent (5 s
without one, at most 30 days) and holds its lane behind it; it does not
pause the other lanes, so under a tenant-wide 429 each due lane makes one
POST per pass and each waits its own Retry-After.

**Migration** `20261110-yt-task-bindings` (expand-only; no `After`, it needs
no other table and runs after the 20261109 entries in ID order): the new
table `yt_task_bindings` with its
primary key and `UNIQUE(tenant, issue_id)`. Locks only on the new, empty
table; no foreign key, so no lock on `tasks`. It writes no row, and the
previous release neither reads nor writes the table: it keeps serving on the
migrated schema (`scripts/test-previous-release-schema.sh`).

**Migration** `20261111-yt-binding-comments-from` (expand-only, `After`
`20261110-yt-task-bindings`, whose table it alters): `ALTER TABLE
yt_task_bindings ADD COLUMN comments_from BIGINT NOT NULL DEFAULT 0` (catalog
only, ACCESS EXCLUSIVE on `yt_task_bindings` until the migration commits) and
a backfill of every existing row to its task's highest comment id. The
previous release never names the column; a binding it inserts gets 0, and the
catch-up would then copy all of that task's comments, so `comments-backfill`
runs from this release only.

**Migration** `20261113-yt-binding-history` (expand-only, `After`
`20261110-yt-task-bindings` and `20261111-yt-task-binding-origin`, whose table
and column it uses): the new append-only table `yt_binding_history` (one row
per binding published by `AddYTTaskBinding` and per `comments-unbind`; no
foreign key) and a wider CHECK on `yt_task_bindings.origin` that also admits
`auto_verified` (ACCESS EXCLUSIVE on `yt_task_bindings` for one statement; the
new CHECK admits every value the old one did). The previous release keeps
serving on the migrated schema: its binding insert gets the default origin,
its reads scan `origin` as a string, and it never names the new table
(`scripts/test-previous-release-schema.sh`).

**Rollout.**

1. Deploy with `HANDOFFKEEP_YT_COMMENTS_MODE=canary`.
2. `handoffkeep youtrack comments-preflight` (read-only).
3. `handoffkeep youtrack comments-backfill`.
4. `comments-preflight` again: `unresolved_ops_uncovered` is 0; review every
   `ambiguous`, `bound_ambiguous` and `error` task.
5. `hk-env set HANDOFFKEEP_YT_COMMENTS_MODE=on`, only on a release that knows
   `on`.
6. After 10 minutes, `handoffkeep yt ops` (pending) and `yt ops --state
   rejected`.

During the rollout nobody runs `youtrack canary add` from the previous
release: it does not take the ownership lock or check bindings.

**Stop and rollback.** `handoffkeep youtrack comments-drain [--tenant T]
[--wait 10m] [--reject-held] [--task N]` is a read-only monitor unless
`--reject-held`를 지정하지 않으면 읽기 전용 감시다. 이 명령은 op를 보내지 않는다.
코멘트 모드가 `on`이면 이 테넌트의 작업 바인딩 op(`comment_append`, `issue_fields`,
`issue_move`)를 `ready`, `held_binding`, `held_binding_changed`,
`held_other_tenant`, `held_authority_youtrack`, `remote_unknown`으로 분류한다.
모드가 `off` 또는 `canary`이면 준비된 op는 `held_mode`다. 발송된 op는 워커가
정산할 때까지 `ready`로 센다. 대상과 일치하는 바인딩은 바로 허용되므로 현재 복구
코드에는 `held_ambiguous` 사례가 없다. 결과에는 각 클래스의 개수가 나온다.
`--wait`는 `ready`가 없어지거나 지정 시간이 끝날 때까지 확인한다. 종료 코드 `0`은
ready op가 없음을, `3`은 ready op가 남았음을, `4`는 `youtrack ops resolve`가 필요한
`remote_unknown` op가 있음을 뜻한다. 종료 코드 `2`는 설정 오류이며, 코멘트 모드가
`on`이 아닐 때 `--reject-held`를 지정한 경우도 포함한다.

서빙 중인 릴리스가 남아 있을 때 다음 순서로 중단한다.

1. `handoffkeep youtrack comments-drain --wait 10m`을 실행하고 종료 코드 `0`일 때만
   진행한다. `3`이면 serve가 ready op를 아직 비우지 못했다. `4`이면
   `handoffkeep youtrack ops --state remote_unknown`으로 op를 확인해 각각 해결하거나
   중단한다.
2. 결과에 안전하게 닫을 held op가 있으면
   `handoffkeep youtrack comments-drain --reject-held`를 실행한다. 이 명령은 해당
   데이터의 정본이 hk일 때만 `held_binding`과 `held_binding_changed`를 거부한다.
   `held_mode`, `held_other_tenant`, YouTrack 정본 데이터는 거부하지 않는다. 다시
   결과를 확인하고 남은 hold는 담당 테넌트·모드·바인딩을 해결한다.
3. 큐를 비운 뒤에만 `HANDOFFKEEP_YT_COMMENTS_MODE=canary`(또는 `off`)로 바꾸고
   `shiftgate rollback`을 실행한다. `canary`/`off`에서는 준비된 쓰기가 대기하며,
   다시 `on`으로 바꾸면 현재 바인딩이 허용하는 op가 풀린다.
4. 롤백 뒤 `handoffkeep youtrack comments-preflight`를 실행해 중단한 릴리스가 만든
   바인딩이 계속 남아 있는지 확인한다.

`comments-unbind`는 더 이른 릴리스가 더 이상 서빙할 수 없을 때만 실행한다. 이전
릴리스는 바인딩 이력을 읽지 않으므로, 그렇지 않으면 바인딩을 지운 작업에
`issue_create`를 준비할 수 있다. 현재 세대 안에서는 바인딩 테이블을 아는 릴리스가
더 새로운 릴리스가 기록한 행을 계속 읽을 수 있다.

**Residuals.**

- 자동 바인더는 선택 기능이며 `HANDOFFKEEP_YT_BIND` 기본값은 `off`다. `on`이면
  10분마다 코멘트 변경을 스캔하고, 기존 이슈와 코멘트 이력을 확인한 뒤, 증거
  워터마크가 있는 바인딩을 기록하고 YouTrack에 없다고 확인된 코멘트의 op를 준비한다.
  이슈를 만들거나 운영자의 unbind를 되돌리지 않는다. 모호하거나 완전성을 확인할 수
  없는 이력은 거부되어 운영자 확인이 필요하며, 일시 오류는 백오프 후 재시도한다.
- held-op 복구는 코멘트 모드 `on`에서 catch-up 전에 5분마다 돈다. 한 시간 지난 뒤
  바인딩이 없거나 바뀐 op를 데이터 정본이 hk인 경우에만 거부한다. YouTrack이 정본인
  op와 다른 테넌트가 소유한 op는 그대로 둔다. `held_mode`는 중단 상태이며, 모드를
  다시 켜면 바인딩이 허용하는 op가 풀린다.
- drain은 감시자이지 디스패처가 아니다. 서빙 워커가 없으면 ready op가 남고 대기
  시간이 끝나면 종료 코드 `3`을 반환한다.
- enqueue는 매핑 출처를 잠금 없이 읽으므로 코멘트 트랜잭션 중 커밋된 프로젝트 변경
  또는 미러 쓰기는 그 코멘트가 아니라 다음 코멘트에서 감지된다. 이슈 바인딩 자체는
  lane lock 아래에서 바뀌지 않는다.
- YouTrack에서 바인딩된 이슈를 다른 프로젝트로 옮겨도 로컬에서는 알 수 없다.
  바인딩이 안정적인 내부 id를 유지하므로 코멘트는 이동한 이슈에 계속 간다.

## Comment audit (hk 3899)

`handoffkeep youtrack audit [--project P] [--task N] [--enqueue] [--limit N]`
compares hk's comments with YouTrack's, task by task, and is the gate for the
comment cutover: `missing_in_yt`, `duplicate` and `unbound` all 0, or each
one explained. Like `comments-backfill` it opens the database directly and
reads YouTrack with the bot key, against the tenant of `YOUTRACK_URL`.
Without `--enqueue` it writes nothing: `store.OpenReadOnly`, and it refuses
(exit 2) a database without `20261110-yt-task-bindings`.

Tasks are every task (or `--project`'s, or `--task`), in id order, listed a
page at a time, at most `--limit` (default all), audited one at a time. For each:

- **Bound on this tenant.** Its issue's comments are listed stably
  (`EnumerateStableComments`, deleted ones left out) and matched with the
  task's comments (`ytwrite.AuditComments`), strongest evidence first. A
  `youtrack_comment_links` value that is a YouTrack id (not `noise:` or
  `adopted:`) on an hk comment the pull did not write names that comment's
  copy. Else a comment in the rendered form of op N (`RenderedOpID`) copies
  the hk comment whose own comment op is N. Else one that ends in the legacy
  push's trailer (`\n\n<!-- hk-comment:N -->`, nothing after it) copies
  comment N, unless its text is also another comment's exact body: then it
  is `ambiguous_copy`, assigned to neither, and a comment it names with no
  other copy is listed there, not as missing (never enqueued). Any other
  copies an hk comment whose body equals its text
  exactly (the import wrote bodies as they are; no trimming). Comments the
  activity pull wrote into hk (author `operator:youtrack`, never enqueued)
  are left out, and so are their YouTrack originals, by linked id only
  (`youtrack_comment_links`; an equal text is no evidence). Classes: `present` (one copy),
  `missing_in_yt` (none), `duplicate` (two or more), `adopted_unmatched`
  (none, but the adoption's `adopted:` link says the Python mirror posted it
  in a form hk cannot identify; never enqueued), and for a YouTrack
  comment that copies nothing `yt_only_bot` (written by the bot key's user;
  a bot comment whose rendered form names a `comment_append` op of the
  task_docs principal `hk:doc-comments` on the same issue is `doc_comment`,
  hk 4245, `ytwrite.ReclassifyDocOps`)
  or `yt_only_human`.
- **Not bound here**, by reason under `unbound`: `no_project`, `no_issue`
  (unmapped project, no YouTrack project id, no such issue), `ambiguous` (the
  issue does not verify, or a local source disagrees), `verifies` (it would
  bind, but `comments-backfill` has not bound it) and `other_tenant`. The
  audit never binds.

The JSON report has, per hk project and in `total`, a count and up to 10
items for each class (task id with comment id, op id and YouTrack ids, or
task id with a detail). Exit 0 when clean; 4 when it ran in full and
found a `missing_in_yt`, `duplicate`, `adopted_unmatched` or
`ambiguous_copy` comment or an unbound task; 1 when a
task's reads failed (class `error`), an enqueue failed, or the run stopped.

**`--enqueue`** opens the database as the backfill does and, for each
`missing_in_yt` comment only, prepares its op through the mode-on enqueue
(`Store.EnqueueYTAuditComment`, which runs `enqueueYTCommentOnTx`): the same
principal, the key `task-comment:<comment id>`, the binding's tenant and
issue, the comment's author and time, its link and map anchor. A comment that
already has an op is left as it is (`linked`), so a second run writes nothing;
a binding a source disagrees with writes nothing (`skipped`). The worker sends
the ops when the mode is `on`. `duplicate` and `yt_only` comments are only
listed: the audit never deletes or edits anything in YouTrack.

**Rate.** One read at a time. A 429 is waited out as the protocol does (the
server's `Retry-After`, 5 s without one) and the read repeated, at most 5
times; a wait over 10 minutes stops the run (`stopped`, exit 1). Each read
(an issue GET, or one issue's full listing) has 60 s.

## Rebuild audit (hk 4121)

`handoffkeep youtrack audit --rebuild [--project P] [--sample-comments N]
[--seed S] [--json]` is the restore drill, the C3 gate of the switch (hk 3833
§1.2, §3.3): it rebuilds what hk needs from YouTrack reads and PG's id maps
only (the binding tables and `yt_comment_write_links`), and diffs the result
with PG. Besides YouTrack it reads PG's side and these id maps:
`yt_task_bindings`, `yt_article_bindings`, `yt_decision_bindings`,
`yt_comment_write_links` with `yt_write_ops` (an op's kind, issue and time),
and `youtrack_comment_links` (legacy and pulled comments). It is read-only: `store.OpenReadOnly`, GETs only, and it refuses
(exit 2) a database without `20261110-yt-task-bindings`. The tenant is
`YOUTRACK_URL`'s, as for the comment audit.

| Target | Scope | Rebuilt from | Equal when |
|---|---|---|---|
| documents | every `yt_article_bindings` row on the tenant; under `--project P`, only those of P's tasks — a document with no task is excluded | the article's content; for an `op_applied` binding the body inside hk's rendered form (`ytwrite.ParseArticleContent`; another form differs) | its sha256 is `documents.sha256`. A binding that names the import's split index (articles-backfill stored no content sha) is `exception_not_updated` (D4), not a failure, and its article is not read; the current body size decides nothing, so a document that grew past 240 KiB after a single-part import is compared. A body quoting the op marker whose article holds it escaped (hk 4295) is the exception `escaped_marker` |
| comments | `--sample-comments` random bound tasks (default 200, by `md5("<seed>:<task id>")`) and the 20 bound tasks with the most comments; under `--project P`, only P's tasks are sampled | the issue's comments, listed stably | every hk comment has exactly one copy with the same (author, time, body), and every YouTrack comment copies one |
| tasks | every `yt_task_bindings` row on the tenant; under `--project P`, only bindings whose task is in P | the issue | hk ID → id, the description's title line → title, the project → project |
| decisions | every `yt_decision_bindings` row on the tenant | the DEC issue's description | question, options and answer equal (the Y6 block) |

`--project P` narrows tasks, the comment sample and documents to P; it never
narrows decisions (below). A clean result under `--project P` certifies only
what was checked: bindings outside P, and documents with no task, were not
read, so their differences would not have shown.

**Comments.** A comment in the rendered form of op N (`RenderedOpID`) whose op
is one of the task's comments' (`yt_comment_write_links`) gives its author and
time from the provenance line, to the second, and its body from between that
line and the marker; a catch-up op (`CatchUpYTComments`) names its own later
time, so a provenance time equal to the op's and not the comment's is the
exception `op_time` (the comment's time is PG-only). A comment in that form
whose op is another of hk's ops for the issue is left out
(`other_hk_writes`); one whose op hk's ledger does not have for the issue
differs. The import's copies of the task's reports and briefs are
expected once each, as the import wrote them: `ytwrite.DocComments` of each
document tied to the task (as it is now), at the document's creation time
(to the millisecond; no later save changes it). The author is not compared:
`PutDocument` overwrites `documents.created_by` on every changed body, so the
import-time author is PG-only (`not_rebuildable`). A comment that fits an expected copy's (body, time) and an hk
comment's tuple too is assigned to neither and differs as `ambiguous:
document copy or task comment`. Otherwise a comment matching an expected
copy's (body, time) uses it up and is left out (`document_copies`), so a task comment with the same text
still matches its hk comment and an extra copy is not hidden; a comment that
names a task document but matches no expected copy differs. Any other comment is an import's: its author is the
YouTrack user's login or name (the import named each user after its hk actor)
and its time the created time, to the millisecond; it is matched with the hk
comment of the same (time, author, body), since imported comments have no id
map. A legacy mirror copy (its `youtrack_comment_links` id, or the
`<!-- hk-comment:N -->` trailer) carries only the body: an equal body is the
exception `legacy_mirror`, not a pass. Left out on both sides and counted: the
activity pull's comments and their YouTrack originals (`pulled_left_out`). The seed is printed; `--seed S` repeats a
sample on the same data.

**Tasks.** The id is the `hk ID` field (else the description's `hk task: #N`
line). The title is the description's title line in whichever form wrote it:
issue_fields' `RenderFields` (whitespace collapsed, so PG's title is compared
collapsed), issue_create's `RenderIssue`, or the import's `Original hk title:`
(the last two exact; the import's title may span lines and ends where its
metadata block — `제목(한국어):`, `Body document:`, `PR:`, `Job:`, `Parent
task:` — begins). The project is the `hk 프로젝트` field (else the
description's project; both, when present, must agree), with `Unassigned`
read as none.

**Decisions.** The DEC issue's description is read in the Y6 form only:
`RenderDecision`'s block (a decision_fields snapshot), or the block between
decision_create's provenance line and marker. An issue still in the import's
form (`답변:` lines, no question block or options) differs, as does a binding
whose decision is gone from hk. PG's side is the projection's own text
(`CanonicalDecision`). DEC issues live in one project, so `--project` does not
narrow decisions.

**Output.** Per target: `checked`, `equal`, `differ`, `exceptions` and `errors`
(up to 10 ids each, with a one-line reason), and `not_rebuildable`: what of the
target stays PG-only (hk 3833 §1.5), listed so a clean result is not read as
covering it. A target whose table is not in the schema yet is `skipped` with
the count it would have checked. The report also gives the wall time. Text by
default, JSON with `--json`. Exit 0 when nothing differs; 4 when something
differs; 1 when a read failed or the run stopped.

**Rate.** One read at a time; a 429 is waited out as in the comment audit (the
server's `Retry-After`, 5 s without one, at most 5 tries; a wait over 10
minutes stops the run).

## Kind issue_create (hk 3898)

hk creates a task's YouTrack issue itself, at most once per task, and
publishes the task's binding in the transaction that records the op applied,
so later comment and field ops have a target. `HANDOFFKEEP_YT_ISSUES_MODE` is
`off` (the default) or `on`, read like the comments mode; either mode on
starts the write worker and needs `YOUTRACK_URL` and `YOUTRACK_BOT_API_KEY`.
Turning it on ends the YouTrack import (decision D3); the operator does that.
At startup the worker resolves the unique `HK` project from YouTrack's project
list. A task with no hk project uses that bucket, matching the import's
`HK-<task id>` issue; hk keeps no `youtrack_projects` row for the bucket.

**Op.** Principal `hk:task-issues`, key `task-issue:<task id>`, lane
`task:<id>`, target the YouTrack project id, payload the title, the hk
project and the YouTrack project's short name. In mode on, every task creator
(`CreateTask`, intake, ticket sets, dots, dispositions) prepares it in the
task's own transaction when its project has a `youtrack_projects` row with a
project id, or when its project is empty (to the `HK` bucket); otherwise no op,
one `ytwrite.issue_skipped` line and a `yt_issue_intents` row on the gate's
tenant (migration `20261113-yt-issue-intents`, hk 3942). An intent is made
only when the task is created. Before each Drive pass the write worker in mode
on prepares the op of every intent of its own tenant whose task's project now
has its mapping, under the task's lane lock, and drops the intent; an intent of
another tenant is never consumed there. Setting or changing the project of an
unbound task also prepares its issue_create when no create op already exists,
on its intent's tenant when it has one (and drops the intent); a move to an
unmapped project records nothing. A task that got an issue another way
meanwhile only loses its intent. `issues-backfill` reads the task under its
lane lock; on another tenant than an intent's it leaves that task alone
(counted `other_tenant`); on the intent's tenant it prepares the op and drops
the intent.

**Project changes (hk 3909, 4318).** A move never touches a prepared op: its
claim does. The claim that dispatches an issue_create op reads the task's
project under the lane lock and, when it maps to another YouTrack project (or
to the same one under another hk project), sends the op there: the same op,
with the same id, key (`task-issue:<id>`) and place on the lane, gets the
current project's `yt_target_id`, payload and payload hash in the update that
dispatches it (`ytwrite.issue_retargeted` line). The `yt_write_ops` guard
allows exactly that change (migration `20261113-yt-issue-claim-target`). A
task whose project now maps to no YouTrack project (or that has none in a
process that does not know the `HK` bucket) has its op sent as it was
prepared, as before hk 4318. An op deferred back to `prepared` is retargeted
at its next claim; one already sent is never changed, and a move that commits
after the claim is followed like any bound task's (`issue_move`). Comments
queued behind the op stay behind it and take their target from the binding
it publishes, which names that op, so the previous release reads the same
state the same way: it creates the issue where the op was prepared.
A task the protocol cannot render (an actor
with ` · `, a title quoting the marker) gets the op rejected `invalid_issue`.
`handoffkeep youtrack issues-backfill [--project P] [--dry-run]` prepares the
op for every task with no binding, bound to the host of `YOUTRACK_URL`; it
reads the YouTrack project list to resolve the `HK` bucket, so a task with no
project gets its op in that bucket (a process that does not know the bucket
counts it `unmapped`), but does not search for or create issues. It skips a task that already has its op, and so is idempotent.
It also skips (counts as `owned`) a task hk already ties to an issue: a
binding, a canary enrollment, a task map row or a YouTrack `task_origins` row
recorded earlier (a pull or `youtrack adopt`), read again under the task's
lane lock. Reading the origin row is the only contact with the legacy sync's
data; nothing coordinates with it.

**Text.** Summary `#<id> <title>` on one line, cut to 250 runes. Description:

```
작성: <actor> · <UTC> · hk op <op id>
hk task: #<task id> · project: <hk project>
제목(원문): <title>

<!-- hk-op:<op id> -->
```

`ytwrite.ParseIssue` reads it back (`ParseIssue(RenderIssue(x)) == x`).

**Send.** First the project's fields (one admin GET): without `hk ID`, or
without `hk 프로젝트` (for an enum field, without the task's project among its
live values), the op is held `not_sent` until the schema is repaired, never
rejected. The issue value follows the project field's type: a simple
`hk 프로젝트` field (`SimpleProjectCustomField`, the importer's shape) as a
primitive string (`SimpleIssueCustomField`), a formatted text one
(`TextProjectCustomField`) as `TextIssueCustomField` with a `{"text": …}`
value, and an enum field (the legacy push's) as an enum value; sending a
primitive to a text field is a 4xx that would reject the op for good. Then the
project's issues
with `hk ID` = task id (one GET, checked against the project id and the
field). Exactly one, whose summary names the task (`SummaryNamesTask`), is
adopted (`AdoptedError`): no POST, the op applies with its id and its map rows
are marked `Adopted`. Any other hit (two or more, or one that does not name the task) is
`rejected / ambiguous_issue`, no POST. None: exactly one POST with the
project, summary, description and only the `hk ID` and `hk 프로젝트` fields
(state, lane, kind, grade and tags are the later snapshot op's). The reply
classification is the shared one; a failed search is `not_sent` (above), and
so is a create the dispatch budget cannot start: the field read and the search
share one budget, and an expired one means nothing was sent, so the op is
prepared again instead of becoming `remote_unknown` with no evidence to find.

**Evidence.** The same search. Exactly one issue with the task's `hk ID`,
whose description is exactly the rendered text, applies the op; a quoted or
copied marker is not ownership. The one issue whose summary names the task and
whose description carries no marker of this op's is the issue Send adopts
(this kind's create always writes the marker), so an adoption whose settlement
was lost (a crash or a failed apply after Send adopted) is reported as that
adoption. Before it applies an adoption, the dispatch records it
(`yt_write_op_adoptions`: op, attempt, issue id; migration
`20261114-yt-issue-adoption`, hk 4570). The attempt that adopts sends
nothing, an op that leaves `dispatched` other than back to `prepared` is never
dispatched again, and the attempts before it ended deferred (429, 401,
`not_sent`), which the protocol treats as not processed; so the search applies
the adoption, with its map rows marked `Adopted`, when the op's record names
that same issue for its last attempt. Without the record (the record write was
lost too, or a previous release dispatched the op) the search never applies
it: the ledger cannot tell that crash from a POST whose reply was lost,
nothing bounds when such a POST lands, and an applied op is never searched
again, so a late create would be a second issue no one sees. The op stays
`remote_unknown / adoption_unconfirmed` with the issue id in its detail, and
`yt ops resolve <op> --applied <issue id> --adopted` applies it with its map
rows marked `Adopted` (hk 3911; a POST that lands first is a second hit,
`marker_mismatch`).
Zero, two exact hits, an edited description, or a hit beside another issue
with that `hk ID` keep it `remote_unknown`; there is never an automatic
re-create.

**Binding.** Applying the op (send, evidence or `yt ops resolve`) takes the
task's lane lock, first checks every mapping hk keeps (the task's binding and
map row, and the issue's), then writes the task's `yt_entity_map` row
(parent: the project) and inserts `yt_task_bindings` with `origin =
'op_applied'` and `op_id`, in one transaction (migration `20261111-yt-task-binding-origin` adds `origin`,
default `import_verified`, and `op_id`, and a trigger that refuses to apply
an `issue_create` op without its binding, so a resolver that does not know the
kind cannot close one). `issue_readable` is left empty: the protocol carries
only the stable id. `comments_from` is 0 for an issue the op created (the
catch-up copies the task's comments) and, for an adopted one, the task's
highest comment id when the op was prepared (`yt_issue_comment_bounds`, hk
4301): the issue already holds the history before that, but not the comments
written while the op was pending. An op a previous release prepared has no
such row and starts at the task's highest comment id at the apply. A comment
written while the op was pending gets no op (the task has no binding yet,
except one queued behind the op under comments authority youtrack), so in comments mode on
the write worker runs the catch-up for `op_applied` bindings before every
Drive pass even when `HANDOFFKEEP_YT_COMMENTS_CATCHUP` is off
(`CatchUpYTIssueComments`, hk 3914). Nothing is overwritten: the same
issue is a no-op; a task bound or mapped to another issue or whose YouTrack
origin names another issue, or the issue bound or mapped to another task,
keeps the op applied with reason
`binding_conflict`, writes neither map row nor binding and logs
`ytwrite.binding_conflict task_id= op_id= issue_id=`. `yt ops --state
rejected` lists such ops and the alert worker raises them as rejected.

**Admission.** Mode on, the op's tenant is the worker's, and the task has no
binding or one with `op_id` = this op. A binding from the import holds the op.
While the op is unresolved, `comments-backfill` binding and canary enrollment
of the task are refused. An operator's `yt ops resolve <op> --applied <issue id>`
states the issue's provenance, which the ledger did not record: by default the
op created it (`comments_from` 0, the catch-up copies the task's comments);
with `--adopted` the issue existed before (its binding starts after the
comments the task had when the op was prepared, as above). For an `issue_create` op the CLI always sends the choice (API
`"adopted": true|false`; it reads the op's kind from `GET /v1/yt/ops` first),
and for any other op it sends the old request shape, which a server that
predates the field accepts. A resolution of an `issue_create` op that omits it
(an older client) is refused `ytwrite_invalid` and the op stays
`remote_unknown`.

**Precondition: no legacy writer.** Issue mode coordinates with no other
writer of task issues. Before `HANDOFFKEEP_YT_ISSUES_MODE=on`: hk 3907 must be
deployed (the legacy `youtrack sync` refuses to write while the comments or
issues mode is on), and the YouTrack import must have ended (D3). Without them
the legacy push or the import can create or link a task's issue beside the
op, and the op's search does not see it in time.

A rejected create stays rejected; a retry is a later operator command.

## Kind issue_fields (hk 4003)

After the import ended (2026-11-11), a task's state, priority, lane and the
other fields the import set reach YouTrack only through this kind: one op is
the full snapshot of the task's issue, written to the task's bound issue in
one update. `HANDOFFKEEP_YT_FIELDS_MODE` is `off` (the default) or `on`, read
like the issues mode; on alone starts the write worker (with the YouTrack
credentials) and makes the legacy `youtrack sync` and `adopt` refuse to run.

**Feed consumer `task_fields`** (`internal/store/yt_fields.go`; the name is
the feed's `[a-z0-9_]` cursor key). It reads every `task` change from
`sync_log` (hk 3997) and, in mode on, for each changed task:

- deleted: skipped;
- no binding: skipped and counted. When its `issue_create` is applied, the
  transaction that publishes the binding also prepares the task's first
  snapshot (as the task is then), behind the create on the lane: the binding
  is no task change, so the feed would not see it;
- a binding on another tenant: skipped and counted;
- its hk project no longer maps to the binding's YouTrack project (the rule
  `ytBindingDisagreement` uses; a task with no project agrees with the
  unmapped HK bucket): its issue moves first (`issue_move`, below), and the
  snapshot names the project the issue belongs in (hk 4055);
- otherwise its snapshot, from the current row, its current Korean title,
  its milestone's name, its refs and its 멈춤 as of the pass, is prepared
  through `prepareYTSnapshotTx`: no op when the snapshot hash equals the last
  one prepared (an `updated_at`-only change) and that op is not `rejected`,
  else one op keyed `issue_fields:<task>:<seq>:<sha>` (A -> B -> A is three
  ops).

**Rejected snapshots** (hk 4057, every snapshot kind). `prepareYTSnapshotTx`
reads the state of `yt_snapshot_state.last_op_id` (in the same transaction,
after the state row's lock, by id, with no lock of its own) when the hash is
unchanged. A `rejected` op will never be sent, so the same snapshot is
prepared again as a new op (the next seq, a new key) on the next call; any
other state (`prepared`, `dispatched`, `remote_unknown`, `applied`) or a
missing op row keeps the skip. One call prepares at most one op: a retry that
is rejected again is retried on the following call, not in a loop. An
`issue_move` is not a snapshot and does not take this retry (hk 4111, Kind
issue_move below).

Mode off prepares nothing and still advances the cursor, so a change made
while off reaches YouTrack with the task's next change. Each pass logs one
`ytwrite.fields_feed tasks= prepared= unchanged= deleted= unbound=
other_tenant= moved= move_skipped= invalid=` line.

**Op.** Principal and actor `hk:task-fields`, lane `task:<id>` (with the
task's comments, its `issue_create` and its `issue_move`, so the writes to one
issue stay in order), target the bound issue id, payload the YouTrack project
the issue belongs in (the binding's, or the target of a move in flight) and the
snapshot. `PayloadSHA` covers the snapshot, the issue and the tenant.

**Snapshot** (`ytwrite.TaskIssueSnapshot`, the import's mapping in
`youtrack/import/client.js` `issue()`). Summary `#<id> <title>`, whitespace
collapsed, cut to 250 runes. Fields, every one in every op (no value clears
it): `hk ID`, `hk 프로젝트` (the project, `Unassigned` for none), `hk 상태` (the
Korean label), `hk 우선순위`, `레인`, `종류`, `등급` and `티어` (the title's `(grade
G, T)` marker, else `refs.grade`/`refs.tier`), `PR`, `작업 job`, `멈춤` (`멈춤` when stalled),
`마감` (epoch ms), `영역`, `마일스톤` (the milestone's name), `예상 소요` (whole
minutes). The description is a stable hk block with no op marker and no
provenance line, so the same task renders the same text; empty lines after
the title are left out:

```
hk task: #<task id> · project: <hk project>
제목(원문): <title>
제목(한국어): <ko>
본문 문서: <body document key>
PR: <url>
Job: <job id>
Parent task: #<parent id>
```

`ytwrite.ParseFields` reads it back (`ParseFields(RenderFields(x)) == x`).

**Send.** The project's fields (one admin GET; a failure is `not_sent`), then
exactly one `POST /api/issues/<id>` with the summary, the description and
every field in the shape the project's field takes (a text or enum `hk
프로젝트`, a single or multi-value `멈춤`; null, or `[]` for a multi-value field,
clears). A closed-set value (`hk 상태`, `종류`, `등급`, `티어`, `hk 우선순위`,
`멈춤`) missing from (or archived in) its field's bundle, a field the project
lacks with a value to set, or a type the snapshot cannot be written as is
`rejected / field_refused` before the POST, with `last_error` naming the field
and the value; a 4xx from the POST is `rejected / http_4xx`. Either raises the
rejected-op alert, and the next feed change or backfill run prepares the
snapshot again (Rejected snapshots above). No map rows.

`레인`, `영역` and `마일스톤` are open sets hk owns (a new value appears in hk
first, hk 4051). `영역` and `마일스톤` are the import's string fields (hk
4305), so bundle handling applies only to projects that carry them as enums.
YouTrack enum names are case-insensitively unique, so a value differing from
a bundle value only in case is not added: the update sends the bundle's
spelling, and an archived one is un-archived (hk 4234). Otherwise, when
nothing above refuses, a value of an enum-shaped field missing from the
project's bundle is added to that bundle (`POST
/api/admin/customFieldSettings/bundles/enum/<bundle>/values` with `$type` and
`name`), and an archived one is un-archived (`POST .../values/<value id>` with
`archived: false`), never duplicated. The add goes to the bundle the project's
field uses, so a bundle several projects share gains the value for all of
them; no per-project bundle is made. Each add logs
`ytwrite.bundle_value_added project_id= field= value= bundle_id= action=`
(`added`, `unarchived`, or `present` when another 4xx answer turns out to be a
concurrent add and a re-read finds the value live). A 403 on the add is
`rejected / field_refused` ("no permission to add bundle value"); any other
failure is `not_sent`, as a failed field read is. The update itself is still
the one POST above.

**Evidence.** A GET of the issue: the summary, the description and every
snapshot field equal (a field the issue lacks counts as empty; an open-set
value on a bundle-backed issue field — an enum or a state — compares
case-insensitively, since the issue carries the bundle's spelling, while the
same value on a plain text field compares exactly) applies the op; anything
else is one mismatch and keeps it `remote_unknown`. Resending an
identical snapshot would be harmless, but the shared rule holds: no automatic
resend.

**Admission.** Mode on, the op's tenant is the worker's, and the op's target
is the issue the task is bound to on that tenant now. An op of a task rebound
since stays prepared.

**Backfill** (hk 4022). The consumer sends a snapshot only on a task's next
change after its cursor, so a bound task unchanged since the import keeps the
import's fields. `handoffkeep youtrack fields-backfill [--project P] [--task
N] [--limit N] [--dry-run]` runs on the server host (it opens
`HANDOFFKEEP_DB_URL` and names the tenant from `YOUTRACK_URL`, calling no
YouTrack) and walks the tasks in id order, each in its own transaction under
its lane lock, through the consumer's rule above: an unchanged snapshot
prepares nothing unless its last op was rejected, so a second run is a no-op
once the first run's ops are pending or sent, a run re-prepares exactly the
rejected ones, and a run beside the feed prepares one op per snapshot. It only prepares; serve's write worker sends the
ops and honours YouTrack's 429s. `--limit` stops after that many prepared ops,
for pacing, counting `issue_move` ops with the `issue_fields` ones (a moved
task makes two, hk 4112); the next run passes the sent ones as unchanged. `--dry-run`
applies the same rule (an unchanged hash whose op was rejected counts as
prepared) and writes nothing. It
prints one JSON summary, `scanned ops prepared unchanged unbound moved
move_skipped other_tenant invalid` per hk project (`ops` is every op made,
moves included) and in `total`, and exits 6 without
touching the database unless `HANDOFFKEEP_YT_FIELDS_MODE=on`.

**Not here.** Tags and issue links (parent, relations) stay as the import set
them; documents are a later slice (a project move and decisions: below). 멈춤, the
Korean title and the milestone name are read when the task row changes: a
task that becomes stalled with no change, a new translation or a renamed
milestone reaches the issue with the task's next change.

## Article bindings (hk 4053)

The import made a YouTrack article for most hk documents, and hk kept no
record of which. `yt_article_bindings` records it, one row per document, so
the document kind (Y5b) updates that article instead of creating a second.
The document kinds (hk 4206, below) read it in `HANDOFFKEEP_YT_DOCS_MODE=on`.

**Row.** `document_id` (primary key), `tenant`, `article_id` (stable internal
id), `article_readable`, `project_id`, `part` (0: the whole document, or the
index article of a split one), `generation` (0; Y5c owns it), `content_sha`
(sha256 of the content the article held when it was verified; NULL for a
split document's index), `origin` (`import_verified` here; `op_applied` is
for an applied `article_create`), `op_id`, `needs_update`, `bound_by`, `bound_at`, and
`UNIQUE(tenant, article_id)`. `Store.AddYTArticleBinding` writes it: the same
binding again is a no-op; any other binding for the document, or another
document's binding to the article, is `yt_article_binding_conflict` and
changes nothing. It also refuses a document that changed (body, task or
project) since it was verified, and an excluded one.

**Scope.** The import's own filter (`youtrack/import/client.js`): a document
with no task is in project `/shared` (the YouTrack project named `Shared
documents`), one of a task with no project in `/unassigned` (`Unassigned hk
tasks`), any other in its task's hk project (its `youtrack_projects` row; a
row whose YouTrack id is one of those two projects counts as unmapped). The
two are found by name, not short name: the import gives them `SHRDOC` and
`HK` with a numeric tail when an hk project took the name first (`SHRDOC2`),
and the readable-id lookup uses the short name YouTrack reports. No such
project, or two, refuses the run (exit 2). `screens/*`, `review/mwr-*` and a task's reports
(`reports/mwr-*/...`, `reports/<job>/report.md`) are `excluded`: they became
issue comments (Y5d).

**Verification.** An article verifies when it is a top-level article of the
document's YouTrack project and its summary is the import's rendering:
`[<prefix>] <key> [<kind>]` cut to 250 code points (`store.YTArticleSummary`),
or that and ` (분할 문서)` for the index of a document over 240 KiB
(`store.YTArticleIndexSummary`). `internal/store/testdata` holds the golden,
generated from `client.js` itself. Then:

- one article, and its content's sha256 equals `documents.sha256`: `matched`;
- one article with other content, or a split document's index: `sha_differs`,
  bound with `needs_update` true;
- none: `missing`; two or more: `ambiguous`, never bound.

The whole article list is read once per run (id, idReadable, summary,
project, parent; 100 a page), before the first document that needs a lookup
and outside its 60 s budget. It is read twice, as `EnumerateStableComments`
reads comments (decision-4): a YouTrack project whose articles (ids,
summaries, parents) differ between the two reads, or repeat within one (an
offset page shifted), binds nothing. Its documents are class `error` with
reason `list_unstable`, the report sets `list_unstable: true` on it, and the
command exits 1; run it again. Other projects proceed. The article is then read by one GET of
`<SHORT>-A-<document id>`, the readable id the import's article key
(`articleKey`, the document id) would give it. A project whose first such GET
verifies keeps that lookup (`lookup: "readable_id"`); a document whose GET
then misses falls back to the summary. A project whose first GET does not
verify is matched by summary from here on (`lookup: "summary"`), and the
matched article's content is read by id. On both paths a second top-level
article of the project with the document's summary makes it `ambiguous`. 

**A long rendering** is never bound (decisions 5 and 6 of hk 4053). This
applies when the full rendering `[<prefix>] <key> [<kind>]` is 242 code
points or longer (`store.YTArticleSummaryCut`, whose comment has the full
argument). Below 242, both strings a document can match are shorter than 250:
the summary is at most 241 and the index at most 249. Every cut rendering of
any key is at least 250 (a cut summary is exactly 250, a cut index at least
258), and an uncut rendering names one key, so a match is the document's own.
From 242 on, a summary or index can equal another, longer key's cut summary,
and neither a unique article nor equal content proves ownership, since two
documents can share a body. Such a document is `ambiguous`, also counted
under `summary_cut`, with no YouTrack read. It stays unbound, and Y5b puts it
on its exception list rather than creating an article. Which lookup the live import allows is UNVERIFIED until
the first preflight (hk 4053 report).

**Commands.** Both open the database directly and verify against the tenant
of `YOUTRACK_URL`; `--project` is an hk project, `/shared` or `/unassigned`.

**Precondition.** No YouTrack article writer runs during a backfill: the
import has ended (D3), and the legacy docs push (`youtrack sync`) is off. hk
cannot fence YouTrack writes. The double read detects a writer during the
enumeration but not after it, so an article created between the list and a
binding is outside what the backfill checks.

- `handoffkeep youtrack articles-preflight [--project P]` is read-only
  (`store.OpenReadOnly`; exit 2 without migration `20261112-yt-article-bindings`).
  The JSON report has, per project, its `lookup` and a count and up to 10
  document ids for `bound`, `matched`, `sha_differs`, `missing`, `ambiguous` (and its subset `summary_cut`),
  `excluded` and `error`. A bound or excluded document is classified without
  a YouTrack read.
- `handoffkeep youtrack articles-backfill [--project P] [--by NAME]` binds
  every `matched` and `sha_differs` document and adds `bound_now`; a refused
  publication counts as `ambiguous` and the run goes on. A re-run binds
  nothing and reads nothing for a bound document.
- Each read has 15 s and each document 60 s; the article list, read once, has 10 minutes. A document in
  class `error` exits 1 after the report.

**Audit.** `youtrack audit` (without `--task`) adds `documents`: the in-scope
documents and those with no binding on its tenant (`unbound`, count and up to
10 ids, in all and per project), and the two classes the document kinds write
no op for (hk 4206), `exception_not_updated` and
`exception_unbound_imported`, in all and per project. It is a count for the document cutover; it
does not change the audit's exit.

**Migration** `20261112-yt-article-bindings` (expand-only, no `After`): the
new table with its primary key and unique index. Locks only on the new, empty
table; no foreign key. It writes no row, and the previous release neither
reads nor writes the table.

## Kind issue_move (hk 4055)

A task's hk project can change while its issue stays in the YouTrack project
of the old one. The issue then moves: `issue_move` (`kind_move.go`) sends one
POST that sets the issue's project, and the task_fields consumer
(`internal/store/yt_move.go`) prepares it. Moves have their own mode,
`HANDOFFKEEP_YT_MOVES_MODE` (`off` or `on`, default `off`, hk 4111). Moves are
prepared by the task_fields consumer, so `on` needs
`HANDOFFKEEP_YT_FIELDS_MODE=on`: moves on with fields off is a startup error
(hk 4117), never a gate that looks on and does nothing. Off, a
moved task is skipped as it was before moves existed: no move, no snapshot,
logged as `ytwrite.fields_skipped_moved` and counted as `moved`. Turning the mode
off admits no issue_move op, so a move already prepared is held until it is
on again.

**Prepare.** For a bound task (on the worker's tenant) the feed decides the
project its issue belongs in: the YouTrack project its hk project maps to; a
task with no hk project stays in the HK bucket its issue is in. The issue is
on its way to the target of its latest unresolved move (an `issue_move` op not
yet applied or rejected), or else to its binding's project. When that differs
from the belonging project, one move op is prepared, keyed
`issue_move:<task>:<seq>:<sha>` (`yt_snapshot_state`, kind `issue_move`; the
sha is the target's, so a repeat of a move already prepared is no new op), and
the snapshot naming the belonging project follows it on the lane. A task moved
away and back before the first move applied gets a move back, so the issue
ends where the task is. A move is prepared once per target
(`prepareYTOnceTx`, hk 4111): the hk 4057 retry of rejected snapshots does not
apply, so a move YouTrack refused is not prepared again on the task's next
change or a backfill run, only when the task's project maps to another target
(a new sha). Until then `ytwrite.move_stuck` is logged, counted as
`move_skipped=`, and the task's snapshot is prepared for the project the
issue is in (or on its way to), so its fields keep reaching it there. A move
the operator's unbind closed (reason `binding_removed` or `unbind_forced`, hk
4539) is no refusal: YouTrack never saw it, so once the task is bound again
the same move is prepared as a new op (the next seq).

**A snapshot behind a refused move** (hk 4111). The lane treats a rejected
op as resolved, so an `issue_fields` op prepared behind a move that was then
rejected would be claimed with a payload naming the target project while the
issue is still in the old one. At claim, once its lane is clear, an
`issue_fields` op whose payload project is not the binding's `project_id` (on
its tenant and issue) is rejected `issue_not_moved`, in the claim's own
transaction, and never sent; the rejected-op alert reports it and the next
change or backfill prepares the snapshot for the project the issue is in.

**Unmapped target** (hk 4118). When the task's hk project maps to no
YouTrack project (an hk project no `youtrack_projects` row names), or is
cleared while its issue sits in a mapped project, there is nothing to move
to: no move is prepared, the issue stays in its binding's project (or the
target of a move already in flight), and the task's snapshot is still
prepared for it there, so its fields do not freeze. The feed logs
`ytwrite.move_skipped_unmapped task_id= project= issue_id= binding_project_id=`
and counts it as `move_skipped=` on the `ytwrite.fields_feed` line.

**Send.** One `POST /api/issues/{id}` with `{"project":{"id":<target>}}`,
asking for `id,project(id)`. The move is settled by the reply only when it
names the same issue in the target project; any other reply is uncertain
(`remote_unknown`), and the op is never sent again on its own.

**Evidence.** A GET of the issue's project. The target project is the one
match (a lost reply, the move happened: applied). Any other project is no
evidence (`no_evidence`: the operator decides).

**Apply.** In the apply transaction, under the task's lane lock (the lock an
`issue_create` apply takes): the binding keeps its issue and takes the op's
target as its `project_id`. `issue_readable` is cleared, not updated: the
protocol carries only the stable id, and an `issue_create` binding leaves it
empty for the same reason. A binding that no longer names the op's issue
(rebound since the op was prepared) is `ErrConflictingResult`: the apply
rolls back and the op stays unresolved.

**Admission.** The moves gate: `HANDOFFKEEP_YT_MOVES_MODE` on, and the op's
target is the issue the task is bound to on the op's tenant now.

**Not here.** A move made in YouTrack itself is not seen locally (see the
note in the comment path above). hk moves only the issues it binds.

## Document comments (hk 4xxx)

After the import ended, a task-tied report or brief reaches its task's issue
only through this path. It is a `comment_append` op, so it has no kind of its
own: its principal is `hk:doc-comments`, and the comment gate admits it only
while `HANDOFFKEEP_YT_DOC_COMMENTS_MODE` is `on` (default `off`). The hk-comment
modes never admit a document op, so turning this mode off stops them even when
the comments mode is on.

**Feed consumer `task_docs`** (`internal/store/yt_doc_comments.go`) reads every
`document` change from `sync_log`. For each document with a task whose key is
a report (`reports/mwr-*`, or `reports/<job>/report.md`) or a brief
(`brief/<date>/<job>`), and whose task is bound (`yt_task_bindings`):

- one op per comment body of `ytwrite.DocComments`, keyed
  `doc-comment:<doc id>:<sha>` (parts after the first: `:part:<n>`), so a
  changed document (a new sha) gets new comments and a re-save with the same
  body prepares nothing (the key already exists);
- no task, a key that is neither, or a missing sha: skipped;
- unbound task, or a binding a local source disagrees with: skipped and
  counted (`ytwrite.doc_comments_feed ... unbound= disagree=`);
- a key already taken by an op with another payload (the author or the
  binding's target changed): counted as a conflict and logged, never fatal.

Mode off prepares nothing and still advances the cursor, so a document changed
while off is not copied unless it changes again. The cursor starts at the
feed's max_sync_id, so documents before it (the import's) are not re-posted.

**Bodies** (`ytwrite.DocComments`) are the import's (`youtrack/import/client.js`
`documentIssueComments`): a report is `# <key>` (with ` (part i/n)` when split),
a blank line and the part, split at `DocCommentPartBytes-1024` bytes
(`DocCommentPartBytes` is the import's `SAFE_PART_BYTES`, 240 KiB) on whole
lines, cut between characters for a longer line; a brief is one
`Article: [brief] <key> [brief] (hk document key: <key>)` line (title cut to
250 runes). `RenderComment` then adds the provenance line and the op marker, so
`youtrack audit` can read them as hk's (`RenderedOpID`). The golden test
`TestDocCommentsSplitReportMatchesImport` pins the parts' byte lengths and
SHA-256 to the import's output.

**Cap.** A document part may be up to `MaxDocCommentBodyBytes` (the import's
240 KiB, `CheckDocComment`). hk's own comments take YouTrack's text bound
(`TextMaxChars`, 20,000,000 characters; `internal/ytwrite/bounds.go`, measured
2026-11-13), the same bound as a task title, task text and an issue description.

**Audit.** A document copy is not an hk comment. One the import posted
(`documentIssueComments`' `# <key>` report parts and `Article:` brief lines)
classifies as `doc_comment` (`ytwrite.ImportedDocCommentKey`); a protocol
document copy lists as `yt_only_bot`. Neither class is counted by the
audit's gate.

## Authority (hk 4122)

Which side is canonical, per content kind, is a row in `yt_authority`
(migration `20261112-yt-authority`, expand-only: one new table seeded with one
`hk` row per kind). The kinds are `tasks`, `comments`, `documents` and
`decisions`; the authority is `hk` (the default) or `youtrack`. A kind with no
row reads as `hk`. The modes stay env values; the authority is not one, so a
release rollback never meets an authority value it cannot parse (consult hk
3833 §3.4).

**Authority hk** is today's behaviour exactly.

**Authority youtrack** for a kind:

- `/v1/import/*` answers every page of that kind's resources with
  `{"items": [], "next_cursor": null}` (200), after the request's own
  validation: `projects` and `tasks` are tasks, `comments` and
  `comment-authors` comments, `documents`, `task-documents` and
  `document-authors` documents, `decisions` decisions. The other kinds' pages
  are unchanged.
- `youtrack sync` and `youtrack adopt` refuse (exit 5, before any YouTrack
  call) while any kind is youtrack, naming the kinds, even with every mode off.
  They check while holding the sync lock, which a switch to youtrack needs
  too, so no switch commits during a run. The mode guard (hk 3907) still
  refuses first while a mode is on.
- `youtrack audit` reports `"repair_direction": "youtrack_to_pg"` (otherwise
  `"pg_to_youtrack"`) when comments are youtrack, and `--enqueue` prepares
  nothing (`"enqueue_skipped"` says why). Each enqueue transaction also
  reads the authority itself, so a run that started before a switch stops
  enqueuing at the switch. Its classes and exit codes are unchanged.
- A comment that would stay only in PG is refused with 400
  `{"error": "yt_comment_unsendable", "reason": ...}` and nothing is written:
  one the protocol can never send (`ytwrite.CheckCommentContent`: the author
  or the body, for example a body quoting the op marker), and one that gets no
  op or a born-rejected one (the comments mode is off, the task is not bound,
  or not enrolled in canary, or a mapping disagrees). Under hk the same
  comments commit as before.
- One exception (hk 4263): in comments mode `on`, a comment on a task with no
  binding whose `issue_create` op is `prepared`, `dispatched` or
  `remote_unknown` (a task created a moment ago) is accepted. Its
  `comment_append` op (same key and render) is prepared on the create's
  tenant with no target, linked to the comment, and queued behind the create
  on the task's lane; its projection is `pending`. The claim that dispatches
  it fills the target, and the payload hash with it, from the binding that
  create published (`origin = 'op_applied'`, `op_id` the create); the
  `yt_write_ops` guard allows exactly that change (migration
  `20261112-yt-comment-queued-target`). A create that ends rejected, or applied
  with `binding_conflict`, publishes no such binding: the claim rejects the op
  in its own transaction, `rejected / issue_not_bound`, so it is never
  dispatched without a target (and never reaches stale recovery or
  `remote_unknown`). Nothing is sent, and the rejected-op alert reports it.
  `Send` also refuses an empty target as a backstop. The claim also checks
  the binding against every local source, as a fresh comment's enqueue does
  (for example a task moved to another project while its create was pending):
  on a disagreement the op is `rejected / ambiguous_issue` and nothing is
  sent; it never waits in the lane for a later move (hk 4457's hold was
  taken out of PR #383 because a hold that waits for a later op on a
  strictly ordered lane can wedge it). Instead (hk 4569) the comment worker's
  requeue pass (`RequeueYTMovedComments`, before each Drive pass in mode
  `on`) copies such a comment again once the binding agrees with every local
  source (the `issue_move` applied, or the task came back to the issue's
  project): a new `comment_append` op, key
  `task-comment:<comment id>:requeue:<rejected op id>`, targeting the binding's
  issue, prepared at the tail of the task's lane, so it waits only for ops
  with smaller ids. The comment's `yt_comment_write_links` row moves from the
  rejected op to the new one, and the projection reads the new op. Only an op
  rejected `ambiguous_issue` with no target and no attempt, whose binding is
  the one the task's own applied `issue_create` published, is copied again;
  the rejected op itself never changes, and the rejected-op alert still
  reports it. The copy lands after any comment already on the lane, so the
  issue shows it after them. Logs: `ytwrite.comment_requeued`,
  `ytwrite.comment_requeue_skipped`. Migration `20261113-yt-comment-hold`
  stays registered and unused. A task with no
  pending create, a body
  the protocol cannot send, and a mapping disagreement are still refused.
  The previous release's comment gate never admits a targetless op, so it
  holds such an op rather than send it. A dots reply (`ReplyDots`), which prepares no op
  under hk, gets its op in the reply's transaction under youtrack, or the whole
  reply is refused the same way. Every comment writer reads the comments row
  `FOR SHARE` (creating it as hk first if it is missing), so a switch waits for
  a comment in flight and a comment sees a switch that committed first.

**Commands.** `handoffkeep youtrack authority show` prints every kind's row
(`GET /v1/yt/authority`). `handoffkeep youtrack authority set <kind|all>
<hk|youtrack> --by NAME --note TEXT` (`POST /v1/yt/authority`, operator token
only) switches in one transaction and prints every row before and after. The
server first takes the legacy sync lock (a running `youtrack sync`, `adopt`
or comment catch-up makes it refuse), then checks, for each kind it changes to
youtrack, that its own write mode is on (tasks: `HANDOFFKEEP_YT_ISSUES_MODE` and `HANDOFFKEEP_YT_FIELDS_MODE`;
comments: `HANDOFFKEEP_YT_COMMENTS_MODE=on`; documents:
`HANDOFFKEEP_YT_DOC_COMMENTS_MODE` and `HANDOFFKEEP_YT_DOCS_MODE`; decisions: `HANDOFFKEEP_YT_DECISIONS_MODE`) and that the kind has no `prepared`, `dispatched` or
`remote_unknown` op (by principal: `hk:task-issues` and `hk:task-fields`,
`hk:task-comments`, `hk:doc-comments` and `hk:documents`, `hk:decisions`), and that its feed
consumers have read every change (`task_fields` for tasks, `task_docs` and
`document_articles` for documents, and for documents no in-scope document
whose content YouTrack lacks with no op writing it: a binding whose article
is not the document's body, or a document created after the import with no
binding (no create yet, no YouTrack project, a rejected create);
`store.ytArticlesUnwrittenTx`, read from the state at the switch, not from a
record of a catch-up), `decision_issues` for decisions; a consumer
that has never run, or whose cursor is behind `sync_state.pruned_through`,
refuses). Every op preparation holds its kind's row
`FOR SHARE`, so the switch counts an op whose transaction was open when it
started. Any refusal is 409
`yt_authority_refused` naming each kind and reason, and no kind changes. A kind
already on the target is left as it was. Setting `hk` always succeeds. Each
changed kind logs `ytwrite.authority_set kind= authority= switched_by= note=`.

### Switch runbook (consult hk 3833 §3.2, operator A53)

Before the day, with no freeze: each kind's mode on, `youtrack audit`
rehearsed until it is clean, a restore drill rehearsed, and the delta audit
timed. There is no full YouTrack export (A53).

1. Freeze: no new spawns, no hk deploys, no `youtrack_projects` edits and no
   task project moves. PG writes need not stop; their ops follow.
2. Confirm the import grant is gone (D3): the hk auth file has no
   `HANDOFFKEEP_TOKEN_youtrack` or `HANDOFFKEEP_IMPORT_READONLY_youtrack` line.
3. `youtrack ops`: no pending op, no `remote_unknown`, and only explained
   `rejected` ones.
4. Full `youtrack audit`: `unbound` 0 (or explained per task), `missing_in_yt`
   0, `duplicate` 0, and the `yt_only` lists reviewed.
5. The restore check (consult §3.3): no diff outside the allowed exception
   list. Documents
   over 240 KiB are on a "not updated" exception list at the first switch
   (D4).
6. `youtrack authority set <kind> youtrack --by NAME --note TEXT` for each
   kind, or `all`. Check that the printed
   `after` rows are youtrack and `youtrack authority show` agrees.
7. Lift the freeze. For the next day, run the audit periodically and keep the
   pending-op alert on.

### Rollback

`youtrack authority set <kind|all> hk --by NAME --note TEXT`: the effects
above stop at once and nothing else changes. The only reverse repair is the
audit's `yt_only` items (what people wrote in YouTrack meanwhile). A release
rollback leaves the rows alone, but the previous release does not read
`yt_authority`: while it serves, every kind behaves as hk (import pages,
legacy push, unsendable comments). Set the kinds to hk before rolling back a
release, or switch them again once the release that reads the table is back.

## Kinds decision_create and decision_fields (hk 4054)

A decision request's DEC issue carries its whole text. The import wrote a
DEC issue per decision with summary `D-<id> <question>` cut to 250 runes and
only the answer, the recommendation and the default action in the
description; the options and the rest of a long question never reached
YouTrack. Arbitration, receipts, option ids and question hashes stay in PG.
`HANDOFFKEEP_YT_DECISIONS_MODE` is `off` (the default) or `on`, read like the
other modes; on alone starts the write worker, needs the YouTrack credentials,
reads the DEC project's id once at start (a failed read stops the start), and
makes the legacy `youtrack sync` (which also mirrors DEC issues) and `adopt`
refuse to run.

**Change source.** The DEC issue is keyed by `decision_requests.id`. The
`decision` rows the feed had are a task's `refs.decision_request` (keyed by
request id); a decision from a chat question, a lane event, a job escalation
or a task flag, and its answer, never touch that. Migration
`20261112-decision-record-feed` adds a row trigger on `decision_requests`
that journals every write in the writer's transaction as a `decision` row
whose model id is `record:<decision_requests id>` (no new model value: the
`/v1/sync` client refuses unknown models). So `ResolveDecision` and every
other answer path emit their row in the answer's transaction.

**Feed consumer `decision_issues`** (`internal/store/yt_decisions.go`). In
mode on, for each changed decision: deleted, skipped; no binding and no
`decision_create` op yet, one `decision_create` op (key
`decision-issue:<id>`, at most one per decision; a rejected one is the
operator's); no binding and an op already, skipped; a binding on another
tenant, skipped; bound, its snapshot through `prepareYTSnapshotTx` (no op
when unchanged, such as a snooze). Mode off prepares nothing and advances the
cursor. Each pass logs `ytwrite.decisions_feed decisions= create_prepared=
create_rejected= create_exists= fields_prepared= unchanged= deleted=
other_tenant= invalid=`.

**Op.** Principal and actor `hk:decisions`, lane `decision:<id>`. A
`decision_create` targets the DEC project id and carries the decision's text
as it was and the project's short name; a `decision_fields` targets the bound
issue and carries the project id and the snapshot.

**Text** (`ytwrite.RenderDecision`, read back by `ParseDecision`;
`ParseDecision(RenderDecision(x)) == CanonicalDecision(x)`). Summary
`D-<id> <question>` on one line, cut to 250 runes. The hk block:

```
hk decision: D-<id>
연결 작업: #<task>
질문:
> <question, every line quoted>
선택지:
1. [<key>] <label>
권장: <recommended>
기본 동작: <default action>
답:
> <answer, every line quoted>
답한 사람: <who>
답한 경로: <channel>
답한 시각: <UTC>
```

`질문:` is always there; every other line or section is left out when empty.
The question and the answer keep their lines (CR LF to LF, trailing
whitespace and edge blank lines dropped); every other value is one line. The
channel is the latest `decision_resolutions.channel` of a structured
request's answer. A `decision_create` description is the provenance line, the
block, a blank line and the op marker; a `decision_fields` description is the
block alone, so the same decision renders the same text. A decision whose
text quotes the marker gets its create rejected `invalid_issue`.

**Snapshot fields** (the import's): `hk ID` and `결정 ID` (the decision id),
`결정 상태` (열림, 답변됨, 대체됨, 만료됨; expired is the read-time status of
an open request past its expiry, read when the row changes), `레인`, `권장`,
`결정 기한` (epoch ms), `연결 task ID`. Every field is in every op; empty
clears.

**Expiry sweep (hk 4116).** An open request's expiry changes no row, so the
feed never sees it. `catchUpYTDecisionExpiries` runs after each feed pass,
at most once a minute, and prepares the snapshot of every bound decision
(on the gate's tenant) whose `expires_at` passed since the last completed
sweep; the first sweep of a process covers every expired decision. The
snapshot is keyed by its hash, so each expiry prepares one op. A failed
sweep keeps its window and runs again.

**Send.** `decision_create`: the project's fields (one admin GET; without
`hk ID` or `결정 ID` the op is held `not_sent`), then the DEC issues with
`결정 ID` = id (one GET). Exactly one whose summary names the decision
(the import's `D-<id>` or the legacy `youtrack sync` mirror's `[결정 <id>]`,
then a space or the end) is adopted, no POST; any other hit is
`rejected / ambiguous_issue`; none is exactly one POST with the project,
summary, description and only `hk ID` and `결정 ID`. `decision_fields` is the
`issue_fields` write on a decision lane (one admin GET, one POST, the same
`field_refused` rule).

**Evidence.** `decision_create`: the same search; exactly one issue with the
decision's `결정 ID` whose description is exactly the rendered text applies
the op, anything else keeps it `remote_unknown`. `decision_fields`: a GET of
the issue compared with the whole snapshot, as for `issue_fields`.

**Binding** (`yt_decision_bindings`, migration
`20261112-yt-decision-bindings`: `decision_id` text primary key, `tenant`,
`issue_id`, `issue_readable`, `project_id`, `origin`, `op_id`, `bound_by`,
`bound_at`, unique `(tenant, issue_id)`). Two writers. Applying a
`decision_create` op (send, evidence or `yt ops resolve`) takes the
decision's lane lock, checks the decision's binding and map row and the
issue's, then writes the map row (parent: the project) and the binding with
`origin = 'op_applied'` and `op_id`, and prepares the first snapshot, all in
one transaction; a conflict applies the op with `binding_conflict` and
writes nothing. A trigger refuses to apply a `decision_create` op without its
binding. `handoffkeep youtrack decisions-backfill [--apply] [--by NAME]
[--limit N]` checks every unbound decision: one GET of `DEC-<id>`, bound
only when its readable id is `DEC-<id>`, it is in the DEC project, its
summary names the decision and its `결정 ID` equals the id
(`youtrack.VerifyDecisionIssue`), with `origin = 'import_verified'`. When
that issue is missing or does not verify and the legacy sync recorded another
issue for the decision (`youtrack_decision_mirrors`), that issue is read and
bound under the same rules except the readable id, which YouTrack chose
(`verified_legacy` in the report). When both `DEC-<id>` and a different
mirrored issue verify, neither is bound (`ambiguous`). Without
`--apply` it is the preflight: the database is opened read-only and nothing
is written. A decision whose `decision_create` is unresolved is refused
(`pending_op`). With `--apply` and mode on in the command's environment, each
new binding also prepares the decision's first snapshot, already-bound
decisions prepare their `decision_fields` snapshot if their snapshot sha
differs (`fields_prepared` in the report, unchanged under `unchanged`), and
missing decisions get a `decision_create` op prepared (`create_prepared` in the
report; same adopt rule as the feed); with mode off, a binding gets its full
text with the decision's next change.

**Admission.** Mode on and the op's tenant is the worker's. `decision_create`:
the decision has no binding, or one this op published (a backfill binding
holds it). `decision_fields`: the op's target is the decision's bound issue
on that tenant now.

## Kinds article_create and article_update (hk 4206)

After the switch, documents are canonical in YouTrack, and the import no
longer writes them. `HANDOFFKEEP_YT_DOCS_MODE` is `off` (the default) or
`on`, read like the other modes; on alone starts the write worker, needs the
YouTrack credentials, reads the `/shared` and `/unassigned` bucket projects
(`Shared documents`, `Unassigned hk tasks`) once at start by name, as the
articles backfill does (none, two or a failed read stops the start), and
makes the legacy `youtrack sync` (which also writes articles) and `adopt`
refuse to run.

**Scope.** Every document the import made an article for
(`store.YTArticleExcluded` false) except the mergewake reviewer briefs
(`briefs/mwr-*`, the same class as the `review/mwr-*` prompts; 461 of 1,226
unbound documents on production, 2026-11-12): all but `screens/*`,
`review/mwr-*`, `briefs/mwr-*` and a task's reports, which Y5d posts as
comments. `YTArticleExcluded` stays the import's own filter, which the
backfill binds against. A task's brief is in scope: the
import made its article, and Y5d's pointer comment names it
(handoffkeep-design's answer, hk 4206). `deploy/*` is written too.

**Feed consumer `document_articles`** (`internal/store/yt_articles.go`). In
mode on, for each changed document, in order:

- deleted or out of scope: nothing;
- a binding on another tenant: skipped;
- a body over 240 KiB (`YTArticleSafePartBytes`, the import's
  `SAFE_PART_BYTES`: it would split), or a binding to a split document's
  index (no `content_sha`): `exception_not_updated`, no op (D4; no Y5c now);
- no binding and created before the import ended (`store.YTImportEndedAt`,
  2026-11-11T12:00:00Z): `exception_unbound_imported`, no op. The import made
  its article or the backfill could not bind it (`summary_cut`, `missing`,
  `ambiguous`);
- bound, and `content_sha` equals `documents.sha256` with `needs_update`
  false: unchanged, no op. Otherwise one `article_update` through
  `prepareYTSnapshotTx` (no op when the snapshot is the one last prepared);
- no binding and created after the import ended: one `article_create` (key
  `document-article:<id>`, at most one per document; a rejected one is the
  operator's) in its task's project's `youtrack_projects` row, or in the
  bucket's project (no project row: skipped, `no_project`). One exception
  (hk 4295): a first create rejected `invalid_issue` with no attempt (the
  prepare-time `CheckArticle` refusal, which before hk 4295 refused every
  body quoting the op marker) made no article, so once the request checks
  the document gets one retry, key `document-article:<id>:retry-<rejected op
  id>` (`ytwrite.ArticleRetryKey`), with the same payload. The retry is
  prepared only when the request checks, so it is never itself retryable;
  any other rejection, or one after a claim, stays the operator's. The feed
  and the rescan both find such a document: the rescan lists every unbound
  post-import document whose create is not held, so no document change is
  needed.
- no binding, created after the import ended, and a decision whose body
  amends a policy document with no bound article on the tenant:
  `parent_pending`, no op. The catch-up and the rescan retry it once that
  policy is bound.

**Catch-up.** The consumer's cursor starts at the feed's end, so each
process in mode on runs, after the cursor exists, one full catch-up: every
document that needs an op by the same rules (unbound and created after the
import with no create, or bound with `needs_update` or another
`content_sha`) is decided in its own transaction. Then, at most once a
minute, every worker rescans the unbound documents from the shared state,
so a document skipped as `no_project` gets its create from whichever worker
rescans after its `youtrack_projects` mapping is added (a mapping makes no
document feed row). A bound document is the feed's after the full catch-up.
Each logs `ytwrite.articles_catchup full=`.

Mode off prepares nothing and advances the cursor. Each pass logs
`ytwrite.articles_feed documents= create_prepared= create_rejected=
create_exists= update_prepared= unchanged= exception_not_updated=
exception_unbound_imported= out_of_scope= deleted= other_tenant= no_project=
invalid=`. A document waiting on its policy's article logs
`ytwrite.article_parent_pending`; see Parent.

**Op.** Principal and actor `hk:documents`, lane `document:<id>`. An
`article_create` targets the project id and carries the summary, the body
and the project's short name; an `article_update` targets the bound article
and carries the summary and the content.

**Text.** The summary is the import's (`store.YTArticleSummary`:
`[<prefix>] <key> [<kind>]` cut to 250 code points). An `article_create`'s
content is the provenance line, a blank line, the body, a blank line and the
op marker. An `article_update`'s content depends on the binding's
origin: an imported article (`import_verified`) gets the body alone, as the
import wrote it; an article hk created (`op_applied`) keeps hk's format, the
update op's own provenance line, the body and the update op's marker
(rendered exactly as a create). Either way `content_sha` is the body's
sha256 after the update.

**Quoted marker** (hk 4295). In every article content (both kinds, both
origins) each `<!-- hk-op:` in the body is sent as `<`, U+200B (zero-width
space), `!-- hk-op:` (`ytwrite.EscapeArticleBody`): YouTrack shows the same
text, in prose and inside code alike (an HTML entity such as `&lt;` would
show literally inside a code span), and no marker scan matches it, so the
op's own marker stays the only real one. A body without the prefix is sent
byte for byte. Evidence compares against this same rendering, and
`content_sha` stays the sha256 of the document's body (not the escaped
one), so the binding equals `documents.sha256`. The escape is one-way: a
body may already hold the escaped form, so readers escape the document and
compare; they never unescape the article. `ytwrite.ParseArticleContent`
reads hk's rendered form back (op id and the escaped body).

**Legacy-synced documents.** A document the legacy `youtrack sync` already
wrote (a `youtrack_doc_articles` row for its key) never gets a second
article. Its `article_create` carries that article's id, the legacy project
(the `youtrack_projects` mapping of the ledger's hk project, which the legacy
sync wrote to; not the create's target) and the document's legacy marker
(`<!-- hk-marker:hkdoc<sha256(key)[:12]> -->`, `ytwrite.LegacyDocMarker`).
Send reads the article (one GET, no search). It is adopted (no POST) when it
is in the legacy project and carries the marker; it may be a child, since the
legacy sync put every document under its prefix's index article. The apply
binds it in the legacy project with `origin = 'import_verified'` and
`needs_update`, so an `article_update` rewrites it with the raw body.
Anything else (gone, another project, no marker) rejects the op
`legacy_synced_unverified`, and so does, at prepare time, a legacy article
the sync split into parts or whose legacy project has no YouTrack id. A
rejected document stays unbound and holds the documents authority switch. Any other failed read leaves the op `not_sent`.

**Parent** (hk 3995). A payload may name a parent: `parent_article_id` (the
article it goes under) or `parent_family` (the family whose root it goes
under); setting both is refused by `CheckArticle`. A payload with neither,
as every create made before this shipped, stays top-level, and so does a
legacy-synced document (it is adopted where it is). The parent is decided at
prepare time and kept in the payload, so an op prepared earlier stays
top-level.
- A decision whose body has an `amends: policy/<key>` line (the first such
  line; the value must be a `policy/` key) goes under the article of that
  policy document, bound on the tenant. Until that policy has one, the prepare
  writes no op: outcome `parent_pending`, logged per document as
  `ytwrite.article_parent_pending`; the catch-up and the rescan retry it.
- Any other document whose key has a `/` goes under its family root: the
  key's first segment. A segment that is not `[A-Za-z0-9][A-Za-z0-9_.-]{0,63}`
  gets no parent, logged `ytwrite.article_family_skipped`, so no create is
  refused for its key.
- A family root is a top-level article: summary the family, content
  `Family root for <family> documents.` and the marker
  `<!-- hk-family:<family> -->` (`ytwrite.FamilyRootMarker`). Send finds it
  by summary and marker: none, and it creates one before the document; one,
  it is the parent; two or more, the op is `rejected / ambiguous_issue` (no
  POST). Root creation is not an op, so no op records it. Residual risk: two
  first creates of one family in one project, running at once, can make two
  roots; the next create of that family then refuses `ambiguous_issue` until
  an operator merges them.
- Evidence looks for the root without creating it: no root, no match.

**Send.** `article_create`: one search of the project for the summary
(`GET /api/articles`, query `project: <SHORT> "<summary>"`, at most 100 hits,
a full page is an error and the op is held `not_sent`), keeping the
top-level articles of the project id whose summary is exact, and one GET of
each kept hit's content. Exactly one carrying the op's marker is adopted, no
POST; two are `rejected / ambiguous_issue`; none is exactly one POST with
the project, summary and content. `article_update`: one POST to the article
with the summary and the rendered content. Whether the live search syntax finds a
created article is UNVERIFIED until the first live create (hk 4206 report).

**Evidence.** `article_create`: the same search; one article carrying the
marker whose content is exactly the rendered text applies the op, one
carrying the marker with other content keeps it `remote_unknown`.
`article_update`: one GET of the article; its summary equal and its
content's sha256 equal to the rendered content's (with the provenance line
and marker for an `op_applied` binding) applies the op, anything else keeps it
`remote_unknown`.

A binding `youtrack articles-backfill` publishes in
`HANDOFFKEEP_YT_DOCS_MODE=on` whose article is not the document's body gets
its `article_update` in the binding's own transaction. Once documents are
canonical in YouTrack, a backfill binding queues no update: it keeps what was
verified (the article's `content_sha` and `needs_update`) and records the
document's current body as its post-switch baseline (`yt_snapshot_state`
kind `article_baseline`). While documents stay canonical in YouTrack, no
update writes a body equal to that baseline (outcome `superseded`, also on a
restart's catch-up); an hk edit after it is written. A document written
after the switch is not bound at all (refused `yt_article_binding_conflict`:
its body may be an hk edit the article lacks, which a baseline would drop and
a write could overwrite the article with; the operator reconciles it). After
a rollback to hk, every worker's periodic rescan (which also covers bound
documents that hold a baseline) writes the body with no restart and no
document change, spending the baseline as it prepares the update; the
documents switch counts it unwritten until then. An applied
`article_update` clears it too.

**Binding.** Applying an `article_create` (send, evidence or `yt ops
resolve`) takes the document's lane lock, checks the document's binding and
`document` map row and the article's, then writes the map row (parent: the
project) and the binding (`origin = 'op_applied'`, `op_id`, part 0,
`content_sha` the sha256 of the body the op wrote, `needs_update` true when
the article was adopted rather than written), and prepares the document's
update when its body changed since the op was prepared, all in one
transaction; a conflict applies the op with `binding_conflict` and writes
nothing. Applying an `article_update` sets the binding's `content_sha` to
the content's sha256 and `needs_update` to false in the same transaction.

**Admission.** Mode on and the op's tenant is the worker's.
`article_create`: the document has no binding, or one this op published (a
backfill binding holds it). `article_update`: the op's target is the
document's bound article on that tenant now.

## Tests

`ytwritetest.RunProtocol` (public contract) and `RunLedger` (compare-and-set rules)
run against both the in-memory ledger (`go test ./internal/ytwrite/`, no database)
and the Postgres ledger (`internal/store`, `testdb`, CI). The fake YouTrack can:
commit then drop the reply, delay a commit beyond reconciliation, answer 2xx without
an id, 429 with Retry-After, 5xx (with or without the commit), 4xx, fail the comment
listing, page at a server cap. Covered: no false done, no duplicate create, same key
with another payload is a conflict, concurrent same-key callers send once, crash at
each ledger/remote boundary (before and after the commit of prepare, claim, apply,
mark-unknown, defer), two writes to one entity in order, evidence paging and exact
match, ambiguous evidence, caller cancellation. `ytwritetest.RunIssueProtocol`
runs the same fault cases for `issue_create` (lost and delayed replies, 2xx
without an id, 429, 401, 5xx, 4xx, operator resolution, concurrent workers,
a crash at each boundary) over both ledgers (hk 3910).

## Not yet (later slices)

- 운영자와 배포 뒤 운영 환경에서 모드를 켜는 절차, authority epoch (design v2.3).
- 다른 테넌트가 소유하거나 YouTrack이 정본이어서 drain 후에도 남는 held op의 복구
  또는 수동 해결. drain은 이를 보고하지만 해당 소유자의 권한을 가져오지 않는다.
- 적용된 op가 커밋하는 event-feed 행, parent/child 정산 (b3), 분할 문서의 article
  세대 (Y5c), fencing generation이 있는 claim (v2.2).
- `applied` 응답 전에 생성된 코멘트를 직접 GET하는 절차. 라이브 canary에서 인덱스
  지연, marker 보존, 쓰기 직후 읽기를 측정한다.
