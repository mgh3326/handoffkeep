# Plane mirror pilot (#764)

The Plane mirror is a one-way external projection of the hk task queue for
human visibility. hk stays the sole write source of truth: Plane is never
read back into hk, and drift resolves one direction — hk wins. This document
is the writer spec and the exit plan required by the pilot decision.

## Scope

- Direction: hk → Plane only. The mirror never pulls, and no Plane field is
  ever treated as authoritative.
- Selection: non-terminal hk tasks only. `merged` and `dropped` tasks leave
  the board — a terminal transition enqueues `work_item_remove`, which
  deletes the remote work item and drops the `plane_issues` link. Terminal
  history stays in hk.
- Identity: one hk task = one Plane work item, keyed by the external id
  `hk:task/<id>`. The id is embedded in the work item's description as the
  adoption marker, so a retried create finds the existing item instead of
  duplicating it.
- Instance: a single serving instance runs the writer. The drain holds
  PostgreSQL advisory lock `824180175`; a second instance started
  accidentally polls the lock and writes nothing.

## Projected fields (allowlist)

The payload shape (`store.PlaneOutboxPayload`) is structurally closed — it
has no field that could carry a forbidden value:

| Payload field | Source | Notes |
| --- | --- | --- |
| `external_id` | task id | `hk:task/<id>` |
| `name` | task title | truncated to 140 runes; withheld entirely when sensitive (below) |
| `state` | task state | derived through `LinearTaskStateMapping`; empty for `needs_decision` (remote state kept) |
| `lane` | task lane | sent as the `lane:<name>` label |
| `kind` | task kind | sent as the `kind:<name>` label |
| `priority` | task priority | mapped onto Plane's `none/low/medium/high/urgent` |
| `project` | task project | maps to a Plane **project**, not a label — see below |

Never projected, by construction: `report_path`, notes, comments, `refs`,
description/body documents, decision content, and any credential material.

### Sensitive titles

A title carrying defect-reproduction vectors (shell pipes, `rm -rf`,
substitution, injection fragments), key paths (`/home/...`, `~/.ssh`,
`.env`, `*.pem`, Windows user dirs), host or network identifiers (IPs,
internal/prod-qualified hostnames, emails), or credential-shaped material
(token assignments, provider key prefixes, PEM/JWT/SSH bodies) is replaced
with `hk:task/<id> <kind> task (title withheld)`. The check is fail-closed:
an unrecognized but odd title is withheld rather than shipped. Tests cover
the classes and common bypass shapes in `internal/store/plane_test.go`.

### Project mapping

The hk `project` field maps to a Plane **project**, resolved through
`HK_PLANE_PROJECT_MAP` (`hkName=IDENT,...`) with `HK_PLANE_DEFAULT_PROJECT`
as the catch-all. Rationale: project is hk's first-class grouping dimension
(#763) and the pilot's comparison criteria include grouping and filters —
Plane projects are the only remote construct that preserves project-level
filtering, dashboards, and swimlane grouping. Labels carry the remaining
allowlisted classification (lane, kind). A task whose project is unmapped
and uncovered by the default fails closed; the writer never invents a
remote home for it. Plane cannot re-parent a work item, so a project
remap replaces the item (create under the new project, then delete the
stale one) — hk wins even across an immutable remote parent.

### State mapping

There is no second hk-to-remote state table. `PlaneStateForTask` reads
`LinearTaskStateMapping` (the sole mapping) and derives the Plane name from
the Linear result: `Backlog`→Backlog, `In Progress`→In Progress,
`In Review`→In Progress (Plane's default workflow has no review column),
terminal names→Done/Cancelled. `needs_decision` is `Mutate:false` — the
remote keeps whatever state it had. Anything undeclared returns
`ErrPlaneStateUndeclared` and fails the projection closed.

## Operations

`plane_outbox` mirrors `linear_outbox`: per-task ordered ops
(`work_item_create`, `work_item_update`, `work_item_remove`) written in the
same transaction as the task mutation, so a task can never commit a change
the mirror missed. The drain processes rows under the advisory lock,
marks them `sent`, `skipped` (already in sync), `dryrun`, `failed`
(permanent) or leaves them `pending` with backoff (transient/ambiguous).
Ambiguous-response-loss is handled by marker adoption, same as Linear.

`GET /v1/plane/status` reports pending/failed/dry-run counts and the last
error. `handoffkeep plane plan` evaluates a read-only `tasks export`
snapshot: mirrored count, terminal/archive churn, unmapped projects,
sensitive-title count, Linear-250 headroom, and an API-calls/day estimate.
`handoffkeep plane reconcile` is a read-only drift inspection over the
remote; the server-side reconciler runs the same pass daily and — when
live — writes a `report/plane/reconcile/<date>` document and enqueues one
corrective `work_item_update` per drifted task carrying the full hk
snapshot.

## Credentials and enabling live writes

The writer reads its credential from `HK_PLANE_API_KEY` only — never a
flag, never a file, never logged. Configuration:

| Variable / flag | Purpose |
| --- | --- |
| `HK_PLANE_API_KEY` | workspace API key; required for any remote access |
| `HK_PLANE_API_URL` | base URL (defaults to `https://api.plane.so`) |
| `HK_PLANE_WORKSPACE` | workspace slug; required when sync is enabled |
| `HK_PLANE_PROJECT_MAP` | `hkProject=IDENT,...` |
| `HK_PLANE_DEFAULT_PROJECT` | identifier for unmapped/unset hk projects |
| `serve --plane-sync` | enables the outbox drain + daily reconciler (default off) |
| `serve --plane-live` | permits real remote writes |

`--plane-sync` without `--plane-live` is the pilot posture: every op is
logged as the exact request it would send and the outbox row is marked
`dryrun`. Live writes additionally require `HK_PLANE_API_KEY`, so a
sync-enabled instance with no key can never write — it only prints planned
operations. The reconciler is added only when a client exists.

## Exit plan (Plane trial ends ~2026-10-04)

The pilot ends before the Plane trial does. Steps, in order:

1. **Writer stop**: restart the serving instance without `--plane-sync` (or
   without `--plane-live` first to observe). The advisory lock releases;
   no new outbox work drains. Task writes in hk are unaffected — the
   outbox is additive.
2. **Copy/compare**: run `handoffkeep plane reconcile` (read-only) and
   save the drift report; `tasks export` remains the canonical record.
   The mirror's content is reproducible from hk, so nothing needs
   preserving except the reconciliation evidence.
3. **Mark ended**: record the pilot verdict in this task's decision
   (recommendation from `docs/plane-linear-comparison.md`), and set the
   Plane workspace's hk-mirrored projects aside rather than deleting
   during the trial window.
4. **Credential revoke**: delete the `HK_PLANE_API_KEY` from the operator
   secret store and revoke it in the Plane workspace settings. hk never
   stored the key; only the environment held it.

Rollback at any point is `plane-sync` off + credential revoke; the
`plane_issues`/`plane_outbox` tables are inert without the drain.
