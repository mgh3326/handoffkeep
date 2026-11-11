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

- `Render(op)`: the exact text written, deterministic in the op, with the op marker.
- `Send(ctx, remote, op, text)`: exactly one remote attempt, no retry inside.
- `Evidence(ctx, remote, op, text)`: enumerate the target and classify every
  candidate carrying the marker as an exact live match, an exact deleted one, or
  a mismatch; a failed search returns its error, with `RetryAfter` from a 429.

Everything else is the protocol's and the same for every kind: claim and
ordering, the reply classification above, and the evidence rule (exactly one
exact live match with a valid id, nothing deleted or mismatched, is `applied`;
otherwise `multiple_matches`, `marker_deleted`, `marker_mismatch` or
`no_evidence`). An op whose kind is not registered is `rejected /
unsupported_kind` when sent, and a failed render is `unsupported_kind` (send) or
`reconcile_failed` (search). `comment_append` (`kind_comment.go`) is the only
kind so far. A new kind registers itself from its own file (`registerKind`)
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
project_id, bound_by, bound_at)`, one row per task and one task per
`(tenant, issue_id)`. It is written only by `youtrack comments-backfill`
(`Store.AddYTTaskBinding`), after the canary's verification
(`youtrack.VerifyBinding`: the enrollment rules above, and the task's
`youtrack_projects` row must carry the YouTrack project id). Publication
reads the task's mappings again under its locks and refuses
(`yt_binding_conflict`) when they differ from the verified ones, when any
source below disagrees, or when the task is bound elsewhere; the same binding
again is a no-op. A binding writes no op and no map row, and old comments are
never copied: a task bound later gets ops for the comments written after the
binding only.

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
| `youtrack_projects` | the task has no project, its project is unmapped, or it maps to another YouTrack project id |

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

- `handoffkeep youtrack comments-preflight [--project P] [--days N]` is
  read-only: it reads issues from YouTrack and writes nothing. It opens the
  database without migrating, on connections whose every transaction is READ
  ONLY, and refuses (exit 2) a database that does not record migration
  `20261110-yt-task-bindings`: deploy the release first. Candidates are
  the tasks with a comment in the last N days (default 14), every task in the
  canary set and every task with an unresolved comment op; `--project` keeps
  one hk project's. A bound task is checked locally only and comes out
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
- `handoffkeep youtrack comments-backfill [--project P] [--days N] [--by
  NAME]` classifies the same way and publishes each `verifies` binding. A
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
  again; its comments go through the legacy push and the import meanwhile.
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

Tasks are every task (or `--project`'s, or `--task`), in id order, at most
`--limit` (default all), one at a time. For each:

- **Bound on this tenant.** Its issue's comments are listed stably
  (`EnumerateStableComments`, deleted ones left out) and matched with the
  task's comments (`ytwrite.AuditComments`). A YouTrack comment in the
  rendered form of op N (`RenderedOpID`) copies the hk comment whose own
  comment op is N; any other copies an hk comment whose body equals its text
  exactly (the import wrote bodies as they are; no trimming). Comments the
  activity pull wrote into hk (author `operator:youtrack`, never enqueued)
  are left out, and so are their YouTrack originals
  (`youtrack_comment_links`). Classes: `present` (one copy),
  `missing_in_yt` (none), `duplicate` (two or more), and for a YouTrack
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
found a `missing_in_yt` or `duplicate` comment or an unbound task; 1 when a
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
