# Cross-host job records

`/v1/jobs` stores one record per fleet job: runtug claims a job id on the
executing host and merges metadata as the job moves
`claimed → spawned → completed | escalated | lost`. All routes use the same
bearer authentication as `/v1/tasks`. The design is
`design/2026-10-30/cross-host-jobs` (§2.3, §3.1–3.2).

Job ids follow runtug's `validID`: `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`
(conventionally `<task>-<role>-<suffix>`). `host` is the node's
`HUB_MACHINE_ID`: `^[a-z0-9-]{1,32}$`.

## Host grants

Every write requires the writing client's `HANDOFFKEEP_HOSTS_<CLIENT>` line
to list the host. A client with no line, or a host not on its line, gets
`403 {"error":"job_host_forbidden"}` — unlike quota presence there is no
warn mode. A claim binds the record to its `host`; every later write must
come from a client granted that same host. Any client granted a host may
write for it, so a second client that shares the grant can take over.
Reads are open to every authenticated client.

## Routes

- `POST /v1/jobs` — `{job_id, task_id, host, at}` claims an id. A new claim
  returns 201 with the record; replaying an identical claim returns 200; a
  claim that differs in `task_id` or `host` on a taken id returns 409
  `job_id_taken`.
- `PUT /v1/jobs/{job}` — merges an update over the record. The allowlisted
  fields include `state`, `role`, `label`, `family`, `profile`, `model`,
  `pool`, `owner_lane`, `parent_lane`, `parent_job`, `pane_id`,
  `worktree` (`{origin, branch, head}`; `branch` is `null` on a detached
  worktree), `report_doc`, `result`, `usage` and the context fields below. Any other field returns 400.
  Brief text, command lines, pane output and absolute local paths are
  never stored.
- `GET /v1/jobs/{job}` — returns the record, or 404 `not_found`.
- `GET /v1/jobs?task=N` — returns `{"jobs": [...]}`, oldest claim first.

## Write rules

- States only move forward: `claimed → spawned → completed | escalated |
  lost`. `completed` needs a recorded `spawned`; `escalated` and `lost` may
  end a claim that never spawned. A backward move, a skip to `completed`, or
  a write to a terminal record outside the late-write set returns 409
  `job_state_conflict`.
- `usage` is the harness session's token usage:
  `{input_tokens, output_tokens, reasoning_tokens, cache_read_tokens,
  cache_write_tokens, source}`. Counts are integers >= 0, and a missing or
  `null` count is unknown, never 0. `source` (at most 64 bytes,
  secret-scanned) names the record they came from, such as
  `claude-session-jsonl`. Each write replaces the stored usage as a whole,
  and the server sets `usage.recorded_at`. A record that never received usage
  has no `usage` key. No prompt or response text is stored.
- `result` is a JSON object of at most 64 KiB, secret-scanned like a text
  attachment (a secret-shaped value returns 400 `secret_like_content`,
  including behind `\uXXXX` escapes), accepted only in the same request as
  `state:"completed"`, and write-once — the record is terminal
  afterwards.
- Every stored free-text field is scanned the same way: a secret-shaped
  value in `label`, `role`, `worktree` and friends returns 400
  `secret_like_content`. `worktree.origin` must be an `http`, `https`,
  `ssh` or `git` URL (any scheme casing) with no userinfo, a bare remote name such as `origin`, or the scp-style
  form `git@host:owner/repo`. Any other user part, a `file://` URL or a local
  path (`/…`, `~/…`, `./…`, `C:\…`) returns 400. `parent_job` and a claimed
  `job_id` are secret-scanned too. A `result` string with NUL or an unpaired
  surrogate escape returns 400 instead of reaching Postgres.

## Error codes

| Status | Code | When |
| --- | --- | --- |
| 400 | `invalid_context` | malformed id/host/state, unknown field, oversized or non-object result, result without `completed` |
| 400 | `secret_like_content` | a secret-shaped value in `result` |
| 403 | `job_host_forbidden` | no `HOSTS` line, or the host not on it |
| 404 | `not_found` | unknown job id |
| 409 | `job_id_taken` | a different claim on a taken id |
| 409 | `job_state_conflict` | backward move or write to a terminal record |

## Job context and skills

Claims may omit `task_id` or send null, and may send `at` as an RFC 3339
client time. The server records it as `claimed_at`; `created_at` is the
receipt time. A replay ignores `at`.

Updates accept `harness` and `effort` alongside `model`. An `at` requires
`state`. Entering `spawned` records `spawned_at` and `spawned_received_at`;
entering a terminal state records `ended_at` and `ended_received_at`.
`ready_at` is accepted on a non-terminal update when the stored state is
`spawned` and records `ready_received_at`. Each pair is set once; a missing
client `at` leaves its client time unknown. Client times more than one hour
in the future are invalid.

A report can be pinned with `report_sha256` (64 lowercase hexadecimal
characters) and `report_version` (positive integer) when `report_doc` is
stored or sent in the same update. A completed result may include `summary`,
a string of 1–500 Unicode characters. Reads expose `summary` with source
`worker`, or, for terminal jobs without a worker summary, `server_fallback`.
The fallback uses the end state, optional `end_reason` and `diff_stat`
(`files`, `insertions`, `deletions`); these outcome fields are accepted only
on the transition into a terminal state. The server computes the fallback on
read and does not store it.

`skills` replaces the entire array of `{name, source, version_or_hash, calls}`.
It has at most 100 unique name/source pairs; names are 1–128 bytes, sources
1–64 bytes, versions at most 128 bytes, and calls nonnegative. A nonempty
array requires `skills_supported:true`; false requires an empty or absent
array. A zero call count means the skill was available but never called.
Terminal updates may contain only `usage`, `skills`, and `skills_supported`.
Other terminal updates return `job_state_conflict`; invalid values and field
combinations return `invalid_context` (secret-shaped values retain
`secret_like_content`).

`GET /v1/job-skills?since=RFC3339&limit=N` reports skill use by harness and
model, never-used skills and supported jobs without calls. The defaults are
30 days and 100 jobs; the limit is 1–1000. Invalid parameters return
`invalid_context`. `handoffkeep jobs skills-report` accepts `--since` as a
duration such as `30d` or `72h`, or RFC 3339, and `--limit`.

A deployment that has accepted a task-less claim requires a binary that can
read a null `task_id`. Rolling back to a binary predating this change can make
reads of those records fail; deploy a compatible binary before accepting such
claims, and keep it available for rollback.
