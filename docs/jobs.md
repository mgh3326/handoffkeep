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

- `POST /v1/jobs` — `{job_id, task_id, host}` claims an id. A new claim
  returns 201 with the record; replaying an identical claim returns 200; a
  claim that differs in `task_id` or `host` on a taken id returns 409
  `job_id_taken`.
- `PUT /v1/jobs/{job}` — merges an update over the record. The allowlisted
  fields are `state`, `role`, `label`, `family`, `profile`, `model`,
  `pool`, `owner_lane`, `parent_lane`, `parent_job`, `pane_id`,
  `worktree` (`{origin, branch, head}`; `branch` is `null` on a detached
  worktree), `report_doc` and `result`. Any other field returns 400.
  Brief text, command lines, pane output and absolute local paths are
  never stored.
- `GET /v1/jobs/{job}` — returns the record, or 404 `not_found`.
- `GET /v1/jobs?task=N` — returns `{"jobs": [...]}`, oldest claim first.

## Write rules

- States only move forward: `claimed → spawned → completed | escalated |
  lost`. A backward move, or any write to a terminal record, returns 409
  `job_state_conflict`.
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
