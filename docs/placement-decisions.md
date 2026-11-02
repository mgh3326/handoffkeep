# Placement decision API

`placement_decisions` stores runtug's immutable record of each placement attempt.
It includes refused and forced placements. hk purges rows older than 180 days,
up to 1,000 rows per hourly retention run. There is no job foreign key, because
a refused placement may have no job record.

The schema extends section 6 of the hk 2649 channel audit with `account_key`,
`family`, `model`, `role`, `pane_id`, and `repo`. `source='forced'` is the forced
flag. Candidates keep the scores, signal snapshots, and skip reasons from that
decision. The schema is in
[linear_board_migration.go](../internal/store/linear_board_migration.go).

## POST /v1/placements

Authentication is the node's existing hk bearer credential. That credential
must have exactly one allowed host in its `HANDOFFKEEP_HOSTS_<CLIENT>` grant.
Absent or multi-host grants return `403 placement_node_required`. The server
sets `decided_on` from that grant. Request values for `id` and `decided_on` are
ignored.

The maximum request size is 64 KiB. Unknown fields and trailing JSON fail with
`400 invalid_context`. Validation lives in the store.

| Field | Type | Contract |
|---|---|---|
| `job_id` | string | Required runtug job id. A refused attempt still uses its proposed id. |
| `attempt` | integer | Required, 1 through 1,000,000. Retry with the same value. |
| `task_id` | integer or null | Owning hk work-board task. Required for `placed`; otherwise optional. Must exist when supplied. |
| `decided_at` | RFC 3339 timestamp | Required. Within the last 180 days, at most five minutes ahead of the server. Stored at microsecond precision. |
| `tool_version` | string | runtug version. |
| `source` | string | Required `auto` or `forced`. |
| `ranking` | string | Required. Producer values include `score`, `load`, `wake`, `forced`, and `unknown`. |
| `state_source` | string | Required. For example `hub` or `direct`. A fallback reason belongs in `reason`. |
| `chosen_host` | string or null | Final executing host. Required for `placed`. |
| `outcome` | string | Required `placed`, `refused`, or `failed`. |
| `reason` | string | Placement or failure reason. |
| `profile`, `pool` | string | Selected profile and pool. |
| `account_key` | string | Opaque account alias or fingerprint. No login, email, path, or credential. Never rendered on the board. |
| `family` | string | Model family, required for `placed`. Produces `model:<family>`. |
| `model` | string | Selected model id. |
| `role` | string | `lead`, `worker`, `review`, or `consult`. Required for `placed`. |
| `pane_id` | string | Executing pane, when known. Later `/v1/jobs/{job}` writes can supply the pane. |
| `repo` | string | Repository name for `repo:<name>`. |
| `candidates` | array | Required, at most 64 candidates. An empty array is valid for a forced placement. |

Text fields are capped at 256 bytes. Host ids use the existing fleet alphabet.
Secret-shaped text is rejected. Candidate `outcome` is capped at 64 bytes.

Each candidate has required `host` and `outcome`, plus optional `reason`.
Optional numeric fields are `score`, `signal_age_s`, `cpu_busy_short`,
`cpu_busy_5m`, `mem_available`, `swap_in_mbps`, and `power`. Optional integer
fields are `bridged_jobs`, `active_jobs`, `tasks`, and `task_slots`.
`power_source` is `ac` or `battery` when present. Numeric values must be finite.
Unknown candidate fields are rejected, so producer additions need an hk update.

A successful insert returns `201` with the stored row. An identical retry of
`(job_id, attempt)` returns `200` with that row. A changed payload at the same key
returns `409 placement_conflict`. The decision and any board outbox write commit
together. A failed transaction leaves neither behind.

## Producer behavior

runtug records the final placement once per spawn attempt, including forced
spawns. The write has a two-second budget. A failure warns without blocking the
spawn. The local `job.placement` event remains the fallback record. A retry
preserves the original timestamp, attempt number, and payload.

The metadata describes the final executing session. A review placement names
the owning work-board task, even when the reviewer runs in the `reviews`
workspace or `mergewake-findings` lane. The producer maps runtug's roles to the
four canonical role values before sending the row.

The runtug producer is a separate PR. This hk change does not deploy it or
change runtug's relay protocol.

## GET /v1/placements

Any authenticated hk client can read the log. The response is
`{"placements": [...]}`. Filters are `job`, `host`, `since`, and `limit`.
`host` matches `chosen_host`; `since` is an RFC 3339 timestamp and defaults to
180 days ago. `limit` defaults to 100 and is capped at 1,000. Rows sort by
`decided_at DESC, id DESC`.

Completed-job duration remains a read-time join to `jobs`, using
`jobs.updated_at - jobs.created_at` when `jobs.state='completed'`. Lost-job time
is not a completion duration.

## Board events

Placement writes supply host, family, model, profile, role, repository, and pane.
Job updates supply later pane and completion details. Task claims, transitions,
rebinds, decision requests, and WRK claims queue updates in their transactions.

Existing relay ingestion supplies terminal events. `job.completed` and
`job.joined` map to READY, `job.escalate` maps to ESC, and `job.lost` and
`job.revoked` map to BLOCKED. Lane events use the existing one-line form:

```text
[<job-id>] READY | PR and head
[<job-id>] ESC | question
[<job-id>] BLOCKED | reason
[<job-id>] FINDINGS | count and verdict
[<job-id>] MERGED | PR
```

An exact bracketed job id, the event's `job_id`, or its explicit `pr` field
associates an event with a task. The existing mergewake form also works:

```text
[mergewake] FINDINGS handoffkeep#123 head=abc1234 | 2 finding(s) for lane work
```

The writer matches that PR against the task's URL or repository-number reference.
A placed review's explicit owning task takes precedence over its separate job
task. Lane names alone never associate a task.
The producer includes the findings count and verdict in the event text.
READY, ESC, BLOCKED, FINDINGS, and MERGED append a short comment. Comments
carry at most 1,800 runes of event text; hk retains the full record. A replay of
the same relay event appends none.

The work-board state mapping is `backlog` and `reopened` to Backlog, `claimed`
and `in_progress` to In Progress, an open PR or READY to In Review, `merged` or
MERGED to Done, and `dropped` to Canceled. `verifying` and `join` also map to
In Review. A late READY cannot reopen a terminal hk task.

The writer uses Linear's documented
[GraphQL API](https://linear.app/developers/graphql) and its
[official schema](https://github.com/linear/linear/blob/master/packages/sdk/src/schema.graphql).
