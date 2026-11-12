# hk-follow: push deploy for handoffkeep

hk 3509 implements hk:doc `decision/2026-11-08/hk-push-rollout-shiftgate` r3.
A green main head reaches ncp without a person, and nothing reaches into
ncp: no SSH, no tunnel, no deploy key.

```
main push → ci.yml (ci-ok) ─┬→ deploy.yml (deploy-ok) → promote job ──moves──▶ ghcr …/handoffkeep:deploy
            image.yml ──────┘   (…/handoffkeep:<sha>)                                   │
                                                                                        ▼ every 30 s
                               ncp: hk-follow.timer → hk-follow tick → shiftgate deploy handoffkeep, handoffkeep-work
                                                                     → commit status `deployed`, hk deploy record, lane event
```

| File | Role |
|---|---|
| `hk-follow` | the script. `promote <sha>` runs in deploy.yml; `tick` runs on ncp; `gate <sha> <deployed-sha>` prints the verdict; `state` and `reset` manage the tick state. |
| `hk-follow.service`, `hk-follow.timer` | systemd units for ncp (a oneshot every 30 s after the last run). |
| `test-hk-follow.sh` | decision tests against a fake GitHub, registry, hk, shiftgate and healthz. CI runs it in the light tier (`auto-tag-test`). |
| `mutants.py` | removes one invariant check at a time; the tests must fail on each. Run it after you change the gate or the rollout loop. |

## Invariants

| | Rule | Where |
|---|---|---|
| I1 | Only a sha with ci-ok and deploy-ok success that is on main and newer than the deployed sha is promoted or installed. | `gate`, run by both `promote` and `tick`. `tick` also requires the `deploy` index to carry exactly the child manifests of `:<sha>`, so a tag mover cannot bind other images to a good sha. |
| I2 | If the release adds a migration since the deployed sha, it ships only with an approval record. | `gate` (`migrations`, approval lookup), both callers. |
| I3 | The rollout matches hk-ship: `handoffkeep`, then `handoffkeep-work`. Each `/healthz` `vcs_revision` must equal the sha, or the rollout stops. | `tick` |
| I4 | A failed rollout leaves the previous colour serving and is never retried by itself. Only a new digest runs. | `tick`: the attempt is written to `state.json` before the first shiftgate call. A digest held by shiftgate is not deployed. |
| I5 | One rollout at a time. | `tick`: `flock` on `$HK_FOLLOW_STATE_DIR/lock`; also, systemd never starts a oneshot unit twice. |
| I6 | No new credential on ncp beyond a GitHub token and the existing hk token. | The registry is read anonymously (the package is public). |

### The gate

`gate` prints one verdict and its reason:

| Verdict | Meaning | `promote` | `tick` |
|---|---|---|---|
| `promote ok` | Every check passed. | moves the tag | rolls out |
| `noop identical` / `noop serving` | The candidate is already deployed. | nothing | records the digest as handled |
| `hold migration` | A migration was added and no approval exists. | posts `deploy-promote` pending and leaves the tag | posts `deployed` pending, then re-checks each tick |
| `wait <reason>` | A check has no answer yet (GitHub, hk or healthz unreachable; ci-ok still running). | fails the job (re-run it) | re-checks next tick |
| `refuse <reason>` | The candidate must not ship (`ci-ok_failure`, `deploy-ok_failure`, `not_on_main`, `not_newer`, `tag_not_bound`, `no_revision`, `image_missing`, …). | fails the job, `deploy-promote` failure; `not_newer` and `not_on_main` (a diagnostic dispatch on an old or unmerged head) only log and pass | `deployed` failure; the digest is done |

- **ci-ok** comes from the same sources release.yml reads: the `ci-ok` job of a ci.yml run on the sha (a github-actions check run), or a `ci-ok` commit status posted by ci-any-ref.
- **deploy-ok** is the commit status that deploy.yml's status job posts.
- **Migrations** are files in `internal/store/` changed between the deployed sha and the candidate (`compare/<deployed>...<sha>`). A file counts if it meets any of these:
  - It is a non-test `.go` file, added, renamed or copied, whose name contains `migration`.
  - Its patch adds a `registerMigration(` call.
  - It is a store `.go` file whose patch GitHub omitted.
  - The comparison lists 300 files, GitHub's cap. Then every file counts, because the list may be partial.

### The approval record

To approve a migration, write the hk doc `deploy-approval/handoffkeep/<sha>`. Its body must contain a line that is exactly `approve <sha>`, where `<sha>` is the full 40-character sha. Put the migration review above that line:

```bash
handoffkeep doc put --key deploy-approval/handoffkeep/<sha> --kind decision --file review.md
gh workflow run deploy.yml -f sha=<sha>    # re-runs the tier and promotes
```

The approval covers that sha only. A later sha that still carries the same unreleased migration needs its own approval. The deploy record names the approval in `approval_ref`. hk 3507 (T-e) is the planned step that will dispatch deploy.yml automatically once an approval is written.

## Report shape (hk 3523)

Every result appears in four places. Consumers should read them in this order.

1. **Commit statuses** on the candidate sha.

   | Context | Writer | `success` | `pending` | `failure` |
   |---|---|---|---|---|
   | `deploy-promote` | the promote job | `promoted deploy=<digest12> from <sha12>` | `deploy-held:<reason> …` | `deploy-refused:<reason>` |
   | `deployed` | hk-follow | `deployed on ncp: handoffkeep,handoffkeep-work` | `rolling out on ncp` or `deploy-held:<reason>` | `failed at <step> on ncp; not retried`, `deploy-refused:<reason>`, or `rollout interrupted …` |

   A sha whose `deployed` status is success is serving on both services. The failure steps are:
   - `shiftgate_<svc>_exit_<rc>`: shiftgate exited 1 (failed), 3 (recovery required) or 78 (configuration refused).
   - `healthz_<svc>`: after `HK_FOLLOW_HEALTH_WAIT` seconds, the service still served another revision.
   - `held_<svc>`: shiftgate holds the digest, for example after a rollback.
   - `interrupted`: the tick died mid-rollout.

2. **hk deploy records**. For each service that hk-follow attempted, it writes the doc `deploy/<service>/<UTC yyyymmddThhmmssZ>` (kind `report`). It uses the `deploy-record/v0` shape that hk-ship writes, with `source: "hk-follow"`:

   ```json
   {"schema":"deploy-record/v0","service":"handoffkeep","target":"ncp",
    "head_sha":"<sha>","deployed_ref":"ghcr.io/mgh3326/handoffkeep@sha256:<deploy index digest>",
    "previous_ref":"<sha served before>","deployed_at":"<RFC3339 UTC>",
    "result":"success|failed","failed_step":null,"job_id":null,
    "approval_ref":"deploy-approval/handoffkeep/<sha>|null",
    "included_prs":["https://github.com/mgh3326/handoffkeep/pull/<n>", …],
    "source":"hk-follow","release":null}
   ```

   When `handoffkeep-work` fails after `handoffkeep` succeeded, the `handoffkeep` record says `success` (it serves the new sha) and the `handoffkeep-work` record says `failed`. A hold or a refusal writes no record; the status and the lane event carry it. `included_prs` is every `(#N)` in the commit subjects between the two shas.

3. **Lane event** to `handoffkeep-design`. The text is one line starting `[hk-follow] handoffkeep <sha12> deployed | FAILED at <step> | held | refused | rollout interrupted`. It is sent by `panewire emit --kind lane.event --type info` when panewire is installed; otherwise the line is logged as `hk_follow.notify_skipped`.

4. **Host state and log**. `hk-follow state` prints `/var/lib/hk-follow/state.json`: `{digest, sha, verdict, reason, at}`, where `verdict` is one of `deployed`, `failed`, `refuse`, `noop`, `hold`, `wait` or `started`. The journal (`journalctl -u hk-follow`) has one `hk_follow.<event> key=value` line per decision: `gate`, `deployed`, `failed`, `service_deployed`, `busy`, `wait`, `status_failed`, `record_failed` and `dry_run`.

## Install on ncp (handoffkeep-design)

Nothing here is installed by the PR. These are the steps the design session runs (T-a):

1. **Prerequisites.** `bash`, `curl`, `jq`, `flock` (util-linux), `sha256sum`, and shiftgate with the `handoffkeep` and `handoffkeep-work` services. Both services must answer `/healthz` on `127.0.0.1:8800` and `:8801`; set `HK_FOLLOW_SERVICES` to change that. You also need the hk CLI with its usual env, and optionally panewire.
2. **GitHub token.** Create a fine-grained token for `mgh3326/handoffkeep` only, with Contents read, Checks read and Commit statuses write, and nothing else. Commit statuses write is the only write: hk-follow reports through it. Store it in Infisical and render it as `GH_TOKEN` in the env file. Do not print it.
3. **Env file** `/etc/hk-follow/env` (mode 0600). It holds `GH_TOKEN=…` and the hk CLI's `HANDOFFKEEP_URL` and `HANDOFFKEEP_TOKEN`, plus the Access pair if the CLI goes through Cloudflare. It may also hold overrides: `HK_FOLLOW_SERVICES`, `HK_FOLLOW_TARGET`, `HK_FOLLOW_HEALTH_WAIT`, `SHIFTGATE`, `HANDOFFKEEP_CLI`, `PANEWIRE`. The defaults are at the top of `hk-follow`.
4. **Files.** Run `install -m 0755 deploy/hk-follow/hk-follow /usr/local/bin/hk-follow`, then copy both units to `/etc/systemd/system/`.
5. **GitHub secrets for the promote job.** The job reads hk only to look up an approval: `HK_PROMOTE_URL`, `HK_PROMOTE_TOKEN`, `HK_PROMOTE_CF_ACCESS_CLIENT_ID`, `HK_PROMOTE_CF_ACCESS_CLIENT_SECRET`. Use a token that only reads docs. Without these secrets, a release with a migration fails closed (`deploy-refused:approval_unreadable`); a release without one promotes normally.
6. **Bootstrap the tag.** Promote compares against the sha the `deploy` tag names, so the tag must exist before the first promote. Point it at the sha ncp serves now; the image built for that sha already exists:

   ```bash
   sha=$(curl -fsS http://127.0.0.1:8800/healthz | jq -r .vcs_revision)
   docker buildx imagetools create \
     --annotation "index:org.opencontainers.image.revision=$sha" \
     --tag ghcr.io/mgh3326/handoffkeep:deploy ghcr.io/mgh3326/handoffkeep:$sha
   ```

   Run this from any host logged in to GHCR with `write:packages`. The first tick then finds that sha already serving and records `noop serving`.
7. **Dry run.** Run `sudo -E hk-follow tick --dry-run` with the env file loaded. It prints the gate verdict and the shiftgate commands it would run, and writes nothing.
8. **Enable.** Run `systemctl daemon-reload && systemctl enable --now hk-follow.timer`, then `journalctl -u hk-follow -f`.
9. After two successful push deploys, turn off the mbp rollout and the hk-deploy wrapper (T-f).

## Operations

- **Pause.** `systemctl stop hk-follow.timer`. Do this before deploying a digest by hand.
- **Roll back.** Run `shiftgate rollback <svc>`. shiftgate holds the digest it rolled back from, and hk-follow's state already marks that digest handled, so nothing redeploys it. The next promote (a new sha) deploys normally. A source revert and a DB rollback are separate steps (r2 §2).
- **Retry a failed digest.** Fix the cause, then run `hk-follow reset`. The next tick evaluates the digest again. If shiftgate holds it (an image failure), clear the hold first with an explicit `shiftgate deploy <svc> --digest <d>`.

## Where this differs from r3

- **`shiftgate deploy --digest`, not `shiftgate follow --tag deploy`.** hk-follow resolves the tag once, checks that digest, and deploys exactly that digest. `follow` would resolve the tag again, so a tag moved between the check and the rollout would ship unchecked. hk-follow keeps follow's semantics itself: it skips a digest shiftgate holds, and it never retries a digest it attempted.
- **The sha travels as an OCI index annotation, not an image label.** `promote` uses `imagetools create --annotation`, which writes a new index over the unchanged child manifests. Images built before this PR lack a label, and the annotation works on them too. `tick` checks that the child manifests are the same as `:<sha>`'s.
- **Approval is an hk doc with a fixed key, not a decision request.** The promote job holds a read-only hk credential and cannot open a request. A doc key per sha is something both callers can check without parsing a free-form answer.
- **ci-ok is read as release.yml reads it.** On main heads it is a check run, not a commit status.
