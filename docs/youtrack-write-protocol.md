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

`yt_entity_map(entity_kind, hk_key, yt_id, yt_parent_id, op_id)` holds the PG map
rows an op commits **in the same transaction** as its `applied` (a comment appended
by op 17 maps `comment/op:17` to its YouTrack comment id). Both directions are
unique, so one YouTrack id can never name two hk identities.

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
otherwise `multiple_matches`, `marker_deleted`, `marker_mismatch` or
`no_evidence`). An op whose kind is not registered is `rejected /
unsupported_kind` when sent, and a failed render is `unsupported_kind` (send) or
`reconcile_failed` (search). `comment_append` (`kind_comment.go`),
`issue_create` (`kind_issue.go`, below) and `issue_fields` (`kind_fields.go`,
below) are the registered kinds.

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

**Operator view.** `GET /v1/yt/ops?state=&task=&after_id=&limit=` and
`handoffkeep youtrack ops [--state S] [--task N] [--after-id N] [--limit N]`
(`handoffkeep yt …` is the same command). `state` is `pending` (default:
prepared, dispatched, remote_unknown), `all`, or one state. Each op shows id,
kind, lane, target, tenant, actor, state, `status` (`pending` for every unresolved
state, `remote_unknown` included), reason code, result id, attempts and times;
never the body, the raw error or the key. A full page carries
`next_after_id`.

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

**Catch-up** (hk 3897). **Off by default**: it runs only with
`HANDOFFKEEP_YT_COMMENTS_CATCHUP=on` (unset or `off` runs none; any other
value fails startup), and stays off until the legacy comment push refuses to
post while the mode is `canary` or `on` (hk 3907). A legacy push that posts a
comment and ends without linking it (a lost response, a failed link insert, a
dead process) leaves no local trace, so the catch-up would copy that comment
again. When enabled, before each `Drive` pass the worker in mode on runs
`Store.CatchUpYTComments`: up to 200 comments (`ytCommentsCatchUpLimit`),
oldest first, on a bound task, above the binding's `comments_from`, not
written by the YouTrack pull, not already mirrored by the legacy push
(`youtrack_comment_links`), with no ownership link and no op under
`task-comment:<id>`. These are comments written after the binding that the
enqueue did not copy: a writer that does not enqueue (a dots reply) or a
comment written while the mode was not `on`. Each gets, in its own
transaction, exactly what the enqueue would have written (`ytCommentOnOpTx`),
with the op's `created_at` the pass's time; the transaction takes the task's
lane lock first and reads the link and the op again, and the op's unique
`(principal, kind, idem_key)` keeps two workers to one op. It logs
`ytwrite.comment_catchup task_id=<id> comment_id=<id> op_id=<id>`; a
disagreeing source gets no op and `ytwrite.comment_catchup_skipped_ambiguous
task_id=<id> comment_id=<id> source=<source>`, once per worker process (the
worker leaves the comment out of its later passes; a restart looks again). A
failed pass logs `ytwrite.comment_catchup_failed`. No YouTrack call.

**The catch-up and the legacy push.** The legacy push (`youtrack sync`,
`UnpushedYouTrackComments`) keeps selecting every comment with no link, in
every mode, and posts each before it links it. The catch-up never runs
beside it: when a pass finds comments it tries the sync lock
(`pg_try_advisory_lock(8241873404)`, `TryYouTrackSyncLock`), which every sync
run of this release and of the releases before it holds for its whole run,
across its selection, its POSTs and its `youtrack_comment_links` inserts. A
held lock defers the pass (`ytwrite.comment_catchup_deferred comments=<n>
reason=sync_running`, nothing written); otherwise the pass holds the lock
until it ends and runs each comment's transaction on the lock's own
connection (no second pool connection, so a pool of one cannot hang), and
each transaction re-reads the push's link under the lane lock, so a comment a
finished sync posted and linked is left. A sync that
starts during a pass exits 3 ("another youtrack sync is running") and its
next run goes on. Neither waits for the lock, so the lock order is
unchanged.

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

- `handoffkeep youtrack comments-preflight [--project P] [--days N] [--all]` is
  read-only: it reads issues from YouTrack and writes nothing. It opens the
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
  [--by NAME]` classifies the same way and publishes each `verifies` binding. A
  refused publication counts as `ambiguous` and the run goes on; the report
  adds `bound_now`. A re-run reads nothing from YouTrack for a bound task.
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

**Stop and rollback.** Set `HANDOFFKEEP_YT_COMMENTS_MODE=canary` **before**
`shiftgate rollback`: a release older than this one fails startup on `on`.
In mode `canary` the ops prepared in mode `on` for tasks outside the canary
are held, not lost: they stay `prepared`, visible in `yt ops`, and their
comments are owned by their links, so neither the legacy push nor the import
sends them. Setting `on` again releases them. There is no unbind command yet
and no recovery of held ops; both come with the automatic binder (follow-up).

**Residuals.**

- A task created after the backfill has no binding until the backfill runs
  again; its comments go through the legacy push and the import meanwhile,
  and the binding never copies them (`comments_from`).
- With the catch-up enabled, a sync that posts and ends without linking
  (lost response, failed link insert, dead process) leaves the comment
  unlinked, and the next catch-up pass can copy it again: the legacy push's
  own retry reads the issue's comments first, the catch-up does not. Hence
  the gate above, until hk 3907.
- The enqueue reads its sources without locks, so a project change or mirror
  write that commits during a comment's transaction is caught by the next
  comment, not this one; the issue binding itself cannot change under the
  lane lock.
- A YouTrack-side move of a bound issue to another project is not seen
  locally: the binding keeps the stable internal id, so comments still land
  on the moved issue.

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
  comment that copies nothing `yt_only_bot` (written by the bot key's user)
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

## Kind issue_create (hk 3898)

hk creates a task's YouTrack issue itself, at most once per task, and
publishes the task's binding in the transaction that records the op applied,
so later comment and field ops have a target. `HANDOFFKEEP_YT_ISSUES_MODE` is
`off` (the default) or `on`, read like the comments mode; either mode on
starts the write worker and needs `YOUTRACK_URL` and `YOUTRACK_BOT_API_KEY`.
Turning it on ends the YouTrack import (decision D3); the operator does that.

**Op.** Principal `hk:task-issues`, key `task-issue:<task id>`, lane
`task:<id>`, target the YouTrack project id, payload the title, the hk
project and the YouTrack project's short name. In mode on, every task creator
(`CreateTask`, intake, ticket sets, dots, dispositions) prepares it in the
task's own transaction when the task's project has a
`youtrack_projects` row with a project id; otherwise no op and one
`ytwrite.issue_skipped` line. A task the protocol cannot render (an actor
with ` · `, a title quoting the marker) gets the op rejected `invalid_issue`.
`handoffkeep youtrack issues-backfill [--project P] [--dry-run]` prepares the
op for every task with no binding, bound to the host of `YOUTRACK_URL`; it
calls no YouTrack, skips a task that already has its op, and so is idempotent.
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
rejected. A text `hk 프로젝트` field (the importer's shape) is sent as text, an
enum field (the legacy push's) as an enum value. Then the project's issues
with `hk ID` = task id (one GET, checked against the project id and the
field). Exactly one, whose summary names the task (`SummaryNamesTask`), is
adopted (`AdoptedError`): no POST, the op applies with its id and its map rows
are marked `Adopted`. Any other hit (two or more, or one that does not name the task) is
`rejected / ambiguous_issue`, no POST. None: exactly one POST with the
project, summary, description and only the `hk ID` and `hk 프로젝트` fields
(state, lane, kind, grade and tags are the later snapshot op's). The reply
classification is the shared one; a failed search is `not_sent` (above).

**Evidence.** The same search. Exactly one issue with the task's `hk ID`,
whose description is exactly the rendered text, applies the op; a quoted or
copied marker is not ownership. Zero, two exact hits, an edited description,
or a hit beside another issue with that `hk ID` keep it `remote_unknown`;
there is never an automatic re-create.

**Binding.** Applying the op (send, evidence or `yt ops resolve`) takes the
task's lane lock, first checks every mapping hk keeps (the task's binding and
map row, and the issue's), then writes the task's `yt_entity_map` row
(parent: the project) and inserts `yt_task_bindings` with `origin =
'op_applied'` and `op_id`, in one transaction (migration `20261111-yt-task-binding-origin` adds `origin`,
default `import_verified`, and `op_id`, and a trigger that refuses to apply
an `issue_create` op without its binding, so a resolver that does not know the
kind cannot close one). `issue_readable` is left empty: the protocol carries
only the stable id. `comments_from` is 0 for an issue the op created (the
catch-up copies the task's comments) and the task's highest comment id for an
adopted one (it already holds that history). Nothing is overwritten: the same
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
with `--adopted` the issue existed before (its binding starts after the task's
latest comment). For an `issue_create` op the CLI always sends the choice (API
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
  unmapped HK bucket): skipped, `ytwrite.fields_skipped_moved` (Y4c);
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
is rejected again is retried on the following call, not in a loop.

Mode off prepares nothing and still advances the cursor, so a change made
while off reaches YouTrack with the task's next change. Each pass logs one
`ytwrite.fields_feed tasks= prepared= unchanged= deleted= unbound=
other_tenant= moved= invalid=` line.

**Op.** Principal and actor `hk:task-fields`, lane `task:<id>` (with the
task's comments and its `issue_create`, so the writes to one issue stay in
order), target the bound issue id, payload the binding's YouTrack project id
and the snapshot. `PayloadSHA` covers the snapshot, the issue and the tenant.

**Snapshot** (`ytwrite.TaskIssueSnapshot`, the import's mapping in
`youtrack/import/client.js` `issue()`). Summary `#<id> <title>`, whitespace
collapsed, cut to 250 runes. Fields, every one in every op (no value clears
it): `hk ID`, `hk 프로젝트` (the project, `Unassigned` for none), `hk 상태` (the
Korean label), `hk 우선순위`, `레인`, `종류`, `등급` and `티어` (the title's `(grade
G, T)` marker, else `refs.linear`), `PR`, `작업 job`, `멈춤` (`멈춤` when stalled),
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

`레인`, `영역` and `마일스톤` are open sets hk owns (a new lane appears in hk
first, hk 4051). When nothing above refuses, a value of theirs missing from
the project's bundle is added to that bundle (`POST
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
snapshot field equal (a field the issue lacks counts as empty) applies the
op; anything else is one mismatch and keeps it `remote_unknown`. Resending an
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
for pacing; the next run passes the sent ones as unchanged. `--dry-run`
applies the same rule (an unchanged hash whose op was rejected counts as
prepared) and writes nothing. It
prints one JSON summary, `scanned prepared unchanged unbound moved
other_tenant invalid` per hk project and in `total`, and exits 6 without
touching the database unless `HANDOFFKEEP_YT_FIELDS_MODE=on`.

**Not here.** Tags and issue links (parent, relations) stay as the import set
them; documents, decisions and a project move are later slices. 멈춤, the
Korean title and the milestone name are read when the task row changes: a
task that becomes stalled with no change, a new translation or a renamed
milestone reaches the issue with the task's next change.

## Article bindings (hk 4053)

The import made a YouTrack article for most hk documents, and hk kept no
record of which. `yt_article_bindings` records it, one row per document, so
the document kind (Y5b) updates that article instead of creating a second.
No mode reads the table yet; nothing changes until Y5b ships.

**Row.** `document_id` (primary key), `tenant`, `article_id` (stable internal
id), `article_readable`, `project_id`, `part` (0: the whole document, or the
index article of a split one), `generation` (0; Y5c owns it), `content_sha`
(sha256 of the content the article held when it was verified; NULL for a
split document's index), `origin` (`import_verified` here; `op_applied` is
for Y5b), `op_id`, `needs_update`, `bound_by`, `bound_at`, and
`UNIQUE(tenant, article_id)`. `Store.AddYTArticleBinding` writes it: the same
binding again is a no-op; any other binding for the document, or another
document's binding to the article, is `yt_article_binding_conflict` and
changes nothing. It also refuses a document that changed (body, task or
project) since it was verified, and an excluded one.

**Scope.** The import's own filter (`youtrack/import/client.js`): a document
with no task is in project `/shared` (YouTrack project `SHRDOC`), one of a
task with no project in `/unassigned` (`HK`), any other in its task's hk
project (its `youtrack_projects` row; a row still naming `HK`, `SHRDOC` or
`DEC` counts as unmapped). `screens/*`, `review/mwr-*` and a task's reports
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

The article is first read by one GET of `<SHORT>-A-<document id>`, the
readable id the import's article key (`articleKey`, the document id) would
give it. A project whose first such GET verifies keeps that lookup
(`lookup: "readable_id"`); a document whose GET then misses falls back to the
summary. A project whose first GET does not verify is matched by summary from
here on (`lookup: "summary"`): the whole article list is read once (id,
idReadable, summary, project, parent; 100 a page) and the matched article's
content read by id. Which lookup the live import allows is UNVERIFIED until
the first preflight (hk 4053 report).

**Commands.** Both open the database directly and verify against the tenant
of `YOUTRACK_URL`; `--project` is an hk project, `/shared` or `/unassigned`.

- `handoffkeep youtrack articles-preflight [--project P]` is read-only
  (`store.OpenReadOnly`; exit 2 without migration `20261112-yt-article-bindings`).
  The JSON report has, per project, its `lookup` and a count and up to 10
  document ids for `bound`, `matched`, `sha_differs`, `missing`, `ambiguous`,
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
10 ids, in all and per project). It is a count for the document cutover; it
does not change the audit's exit.

**Migration** `20261112-yt-article-bindings` (expand-only, no `After`): the
new table with its primary key and unique index. Locks only on the new, empty
table; no foreign key. It writes no row, and the previous release neither
reads nor writes the table.

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
match, ambiguous evidence, caller cancellation.

## Not yet (later slices)

- Turning the mode on in production (handoffkeep-design with the operator,
  after deploy), and the authority epoch (design v2.3).
- An automatic binder for new tasks, `comments-unbind`, and recovery of ops
  held after a stop (hk 3810 follow-up).
- The event-feed row an applied op commits, parent/child settling (b3),
  article generations (b2), claims with a fencing generation (v2.2).
- A direct GET of the created comment before answering `applied`; the live
  canary measures index latency, marker preservation and read-after-write.
