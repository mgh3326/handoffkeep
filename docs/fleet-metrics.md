# Fleet operating metrics (`handoffkeep fleet-metrics`)

`fleet-metrics` prints the five operating metrics of
hk:doc advice/2026-09-24/fleet-structure-astra Q6 for a time window, each with
the coverage of the identifiers it needs (#644, spec hk:doc
task/2026-09-24/fleet-advice-g4). The first seven days form a baseline; no
target is set.

The one rule every metric follows: **a missing identifier or timestamp lowers
COVERAGE; it is never filled in as 0**. A task without a claim time is not a
0-hour lead time, a lineage without a verify transition is not 0 rounds, a
request closed without a recorded answer is not a 0-hour answer, a task with no
linked job history is not "normal path", and a builder whose end was not
observed is not an empty slot. A distribution with no observation prints
`not observed (n=0)`.

## Output surface: a daily hk report doc (not a console page)

Chosen: one hk document per day, `report/<date>/fleet-metrics-baseline`
(the day's run appended), produced by a manual command. Why:

- **Sources live off the server.** Metric 3 and 5 read wrk/panewire job
  directories and completion-sentinel logs on the machine that spawned the
  jobs, and metric 2 reads `scopefuel reps`, which are per-machine. The
  handoffkeep server (NCP) has none of these; a console page would need new
  agents or secrets on the server and a deploy — both out of scope.
- **A baseline is a frozen series.** Seven daily values must stay as they
  were computed. A live page recomputes; a document keeps the day's numbers,
  and `--snapshot-out` keeps the exact input next to them.
- **No schema change, no deploy.** The command only issues GETs and local
  read commands. The console can render the same `--format json` later.

## Running it

```bash
handoffkeep fleet-metrics --machine mac-personal --since 7d \
  --slots mac-personal=3@2026-09-23T18:03:41+09:00 \
  --slots mac-personal=4@2026-09-23T19:40:00+09:00 \
  --slots mac-personal=6@2026-09-24T17:01:00+09:00 \
  --snapshot-out ~/work/herdr-inbox/fleet-metrics/$(date +%F).json > today.md

handoffkeep fleet-metrics --from-snapshot ~/work/herdr-inbox/fleet-metrics/2026-09-24.json   # reproduce
```

| Flag | Meaning |
| --- | --- |
| `--machine ID` | hub machine id of the machine running the command (owns the default job root and `[local]` slots). Required for a live run. |
| `--since 7d\|36h\|RFC3339`, `--until RFC3339` | window; default the 7 days ending now |
| `--jobs-root PATH[=MACHINE]` | job-directory root, repeatable; default `~/work/herdr-inbox/jobs=<machine>` |
| `--reps-file PATH[=SOURCE]` | a saved `scopefuel reps list` from another machine, repeatable |
| `--no-reps-local` | skip `scopefuel reps list` on this machine |
| `--hosts-config PATH` | wrk `hosts.toml` for slot capacity (`[local] max_active`, `[hosts.X] capacity`) |
| `--slots MACHINE=N[@RFC3339]` | slot capacity; a dated list is a timeline and replaces `hosts.toml` for that machine. Minutes before the first dated entry have no declared capacity and leave the denominator |
| `--no-github`, `--no-herdr` | skip `gh api` PR facts / `herdr agent list` liveness |
| `--snapshot-out FILE`, `--from-snapshot FILE` | save the collected input / compute from a saved input only |
| `--format md\|json` | markdown report (default) or the JSON report |

Reads, all read-only: `GET /v1/tasks/export`, `GET /v1/tasks/{id}` (events),
`GET /v1/tasks/{id}/comments`, `GET /v1/relay/events` (paged by `after_id`),
`GET /v1/documents?prefix=deploy/` + `GET /v1/documents/{key}`; job
directories and `completion-sentinel.log` under each job root;
`scopefuel reps list`; `gh api repos/{repo}/pulls/{n}` and
`gh api repos/{repo}/compare/{merge}...{deployed}`; `herdr agent list`. The
command writes nothing except the `--snapshot-out` file.

### Filling the seven-day baseline

Day 1 is in hk:doc report/2026-09-24/fleet-metrics-baseline. For each of the
next six days, at about the same time of day, run the command above with
`--since 7d` (a rolling seven-day window ending at the run) and the same
`--slots` timeline plus any new slot decision, save the snapshot, and append
the day's output as a new `## Day N` section to the same document
(`handoffkeep doc get` → edit → `handoffkeep doc put`). Keep the command line in
the section. Scheduling the run is a separate operator approval.

## Metrics as implemented

Percentiles are nearest-rank over observed values (hours unless noted).

1. **Order → usable result.** Population: tasks that reached `merged` in the
   window. Order = first `→ claimed` transition. The task's PRs come from
   `refs.pr` (task or any event), a linked job's relay/job event PR, or a PR URL
   in an event note. Merge = GitHub `merged_at` (falls back to the hk `merged`
   transition time, reported in coverage). Usable = for a repo with a
   deploy-record stream (`handoffkeep`, `auto_trader`, `panewire`, `scopefuel`,
   `brewdial`), the first `result=success` deploy record at or after the merge
   that lists the PR in `included_prs` (`<repo>#<n>`), has `deployed_ref` equal
   to the merge commit, or whose ref GitHub `compare` says contains it; for any
   other repo, the merge. A merge older than the service's first deploy record
   is **unknown** (deploys before 2026-09-23 were not recorded), not "deployed
   at the first record". Phases: implement (order → first verifying),
   verify_fix (first verifying → merge), decision_wait (time in needs_decision
   inside order → merge), install (merge → usable). Open work: tasks in
   claimed…hold, age since first claim.
2. **Rounds per contract lineage.** Lineage = tasks joined by
   `refs.origin_task` (a new task/job does not reset it). Rounds = number of
   `→ verifying` transitions across the lineage; share over 3. Fix rounds are
   `verifying → in_progress`; their cause is read only from an explicit tag in
   the transition note: `cause:regression` or `cause:requirement`. The latest
   scopefuel `role=impl` rep per task is compared as a second source.
3. **Empty task slots while ready work existed.** A task slot holds one task
   unit — a builder job plus the worker/tester jobs whose `owner_lane` is the
   builder's lane (hk:doc task/2026-09-23/hub-placement-task-slots). A unit
   holds its slot from its claim until the first of: `job.reaped`/`job.lost`/
   `job.revoked`/`quota_pool.release`, the first sentinel `err:agent_not_found`
   sample, every linked task reaching merged/dropped, or collection time if the
   pane is live. Each minute a unit is **working** when the builder or any of
   its workers is sampled `working` by the completion sentinel, otherwise
   **held-idle** (alive, not WORKING: waiting on its tester, CI, an answer or a
   merge); without sentinel samples the job events decide (after
   joined/completed/escalate → held). A held-idle builder is never an empty
   slot. Empty = capacity − working − held-idle. Ready = at least one task in
   `backlog` that minute (replayed from task events). Eligibility
   (quota/account/profile/verifier) is recorded by no source, so the share is
   an **upper bound**. A unit whose end was not observed (no terminal record,
   pane not live, task open or unlinked) may or may not hold its slot: those
   minutes are reported as a range `[lower … upper]`, never as empty.
4. **Decision-request dwell.** Requests: every `→ needs_decision` transition
   (task, or disposition item), every `job.escalate` relay event, every
   `[decision-needed]` lane event. Answer: the transition out of
   needs_decision (by `operator:<email>` = operator on the web, otherwise a
   resolver); for escalations the lane event whose `event_id` carries
   `decision-escalation-<id>-`; for lane decisions `[decision-answered] #<id>:`
   on the same lane (the console's own open-decision rules). Delivery: the
   answer lane event's `delivered_at`. Consumption: the next task transition
   after the answer, or the job's next relay event for an escalation; lane
   decisions have no consumption record. An escalation whose job moved on with
   no answer event is "closed without recorded answer" (coverage), not 0h.
   hk has no auto-default writer, so auto-default answers are `n/a`.
5. **Normal path.** Population: tasks reaching merged/dropped in the window
   (dropped stays in the denominator). A task is classified only if at least
   one linked job has event history. Repairs: `job.reclaim` or epoch > 1
   (re-inject), `job.lost` before the job completed/joined/was reaped or
   `job.revoked` (manual recovery), and explicit tags in a task event note or
   task comment: `repair:reinject`, `repair:manual`, `repair:config`,
   `repair:state`. Normal = merged with no repair.

## Identifier linkage

| Identifier | Provided by | Links to | Where the link breaks |
| --- | --- | --- | --- |
| task_id | hk `tasks` / `task_events` | everything | — (primary key) |
| job_id | `refs.job_id` on the task or an event; job directory name; relay `job_id`; hub `/v1/jobs` | task ↔ job | about 40% of merged tasks carry no `refs.job_id`; the report falls back to `refs.report_path` (`…/jobs/<job_id>/…`), an exact known job id in an event note, and the `<task>-…` / `t<task>-…` naming convention, and prints which tier linked each task. Jobs spawned on other machines have no local job directory. |
| attempt | job `epoch` in flat job events and relay events; `job.reclaim` | re-inject detection | epoch is on completion records only; claims carry none. There is no hk attempt id. |
| contract revision | — | lineage | **no source**: `tasks.body_doc` names a document but documents keep only the current sha256, and briefs are not versioned. Lineage uses `refs.origin_task` instead. |
| PR / PR SHA | `refs.pr`, `refs.head_sha`; relay `pr`/`head`; GitHub merge commit | task ↔ PR ↔ deploy | about 30% of merged tasks have no PR link of any tier; `refs.head_sha` is the reviewed head, not the merge commit — GitHub supplies the merge SHA. Deploy records name PRs as free text (`handoffkeep#45 (…)`) or a ref; the pre-2026-09-23 deploys were never recorded, and one record (`deploy/scopefuel/…`) is YAML without a `result`. |
| event_id | `task_events.id`, `relay_events.id` / `event_id`, job event `seq` | decision request ↔ answer ↔ delivery | task decisions answered with a plain `tasks transition` leave no lane event, so delivery is unobservable for them; flat job completion records carry no timestamp (the file mtime is used and labelled). |

## Tests and mutants

`go test ./internal/fleetmetrics` runs one fixture test per metric, each with a
missing-identifier variant that must lower coverage while leaving the value
unchanged. The parsers are tested on verbatim copies of a job directory,
`scopefuel reps list` output and two deploy-record bodies. Mutants live on disk
in `internal/fleetmetrics/testdata/mutants/*.json`;
`scripts/fleetmetrics-mutants.sh` applies each to a throwaway copy and requires
an assertion failure (a panic or build error does not count).
