# Container image (GHCR)

`.github/workflows/image.yml` builds the image on every PR (build + smoke only)
and pushes it to GHCR on each `main` push. This pipeline only *produces* the
image — the oci systemd deployment is unchanged.

- **Image**: `ghcr.io/mgh3326/handoffkeep`
- **Tags**: `<full commit sha>` (per push) and `main` (moving, latest main).
  `main` is mutable — pin the full-SHA tag or the `@sha256:` digest anywhere a
  specific build must be named (deploy verification compares digests).
- **Platforms**: `linux/amd64` and `linux/arm64` (multi-arch manifest).
- **Contents**: one static `handoffkeep` binary on `distroless/static-debian12`,
  running as `nonroot` (uid 65532). No shell, no source, no env files.
- **VCS stamp**: the binary is built inside the checkout, so `vcs.revision`
  equals the pushed commit. Verify any pulled image with:

  ```sh
  scripts/verify-image-vcs.sh ghcr.io/mgh3326/handoffkeep:<full-sha> <full-sha>
  ```

  (extracts `/handoffkeep` from the image and greps `vcs.revision=` — the same
  signal the deploy runbook reads via `strings`/`/healthz`).

- **Smoke** (what CI asserts): `serve -h` prints flag usage; bare `serve`
  without `HANDOFFKEEP_DB_URL`/`HANDOFFKEEP_AUTH_FILE` exits immediately with
  `HANDOFFKEEP_DB_URL and HANDOFFKEEP_AUTH_FILE are required` (no restart loop).

Local `docker build` works from a normal clone. From a *worktree* it does not
stamp: a worktree's `.git` is a file whose `gitdir:` target is outside the
build context, so the stamp silently drops. Build from a plain clone, or rely
on CI.

## Environment variable names (values intentionally not listed)

`serve` requires:

- `HANDOFFKEEP_DB_URL` — Postgres DSN
- `HANDOFFKEEP_AUTH_FILE` — path to the bearer-token file (mount it; never bake
  it into the image)

Optional server configuration:

- UI / Cloudflare Access gate (all-or-none): `HANDOFFKEEP_UI_CF_TEAM_DOMAIN`,
  `HANDOFFKEEP_UI_CF_AUD`, `HANDOFFKEEP_UI_ALLOWED_EMAILS`,
  `HANDOFFKEEP_UI_ALLOWED_SERVICE_NAMES`; lane views:
  `HANDOFFKEEP_UI_LANES`, `HANDOFFKEEP_UI_ADMIRAL_LANES`,
  `HANDOFFKEEP_UI_DIRECTOR_LANES`
- Hub proxy: `HANDOFFKEEP_HUB_URL`, `HANDOFFKEEP_HUB_TOKEN`
- Decision notices in Discord (see [decision-discord.md](decision-discord.md)):
  `HANDOFFKEEP_DISCORD_DECISIONS_WEBHOOK_URL`
- Outbox forwarding to the hub: `HANDOFFKEEP_NOTIFICATION_FORWARD=on`, with the
  hub proxy variables above, plus `HANDOFFKEEP_NOTIFICATION_FORWARD_SINCE`
  (RFC3339, required while forwarding is on). A first start sends only notices
  created at or after that instant; older unsent rows are settled as skipped
  (`skipped_at`/`skip_reason`, never `sent_at`) and the start log reports the
  cutoff and how many rows it skipped. A restart with the same value re-skips
  and re-sends nothing. Missing, unparsable or later than the server's clock
  (beyond a one-minute allowance): the server refuses to start.
  In a blue-green rollout, turn forwarding off on the colour being replaced
  before the new colour settles the backlog: an older release reads every
  unsent row (it does not know `skipped_at`), so its forwarder would post the
  settled rows if it ran; the two colours share the `HubForwardWorkerLock`.
  A rollback follows the same rule in reverse: unset
  `HANDOFFKEEP_NOTIFICATION_FORWARD` in the previous release's configuration
  before starting it, because that release cannot honour the cutoff and would
  post every row carrying `skipped_at`.
  Rows settled by mistake (a cutoff set too late) are recoverable, but the
  configuration must be corrected before the rows. First set
  `HANDOFFKEEP_NOTIFICATION_FORWARD_SINCE` on every colour that runs the
  forwarder (or turn forwarding off on the colour being replaced) to an instant
  at or before the earliest `created_at` of the rows to recover and not in the
  future, and restart with it. Lowering it alone sends nothing: rows already
  skipped stay skipped and are never listed. Then run the UPDATE for the
  affected event ids, scoped to the rows the corrected cutoff lists, so the
  affected count is the number that will be sent:

  ```sql
  UPDATE notification_outbox SET skipped_at=NULL, skip_reason=NULL
  WHERE event_id IN (...) AND skip_reason='before_forwarder_cutoff'
    AND sent_at IS NULL AND created_at >= '<corrected SINCE>';
  ```

  The next pass sends them, because `sent_at` was never set. An equal
  alternative is the reverse order: run the UPDATE while forwarding is off,
  then start with the corrected SINCE. While the running cutoff is still later
  than a row's `created_at`, an UPDATE has no effect: the next tick re-skips
  the row and it is never listed.
- Attachments / S3-compatible storage: `HK_S3_ENDPOINT`, `HK_S3_BUCKET`,
  `HK_S3_ACCESS_KEY_ID`, `HK_S3_SECRET_ACCESS_KEY`; optional connection settings:
  `HK_S3_REGION`, `HK_S3_BUCKET_LOOKUP`; limits:
  `HK_ATTACH_MAX_BYTES`, `HK_ATTACH_STORAGE_CAP_BYTES`,
  `HK_ATTACH_MONTHLY_PUT_CAP`, `HK_ATTACH_MONTHLY_GET_CAP`,
  `HK_ATTACH_MIME_ALLOW`; archive mode: `HANDOFFKEEP_ATTACH_ARCHIVE_MODE`
  (`off`, `record` or `on`, default `off`)
- alert webhook (removed with `r2usage`): `HK_ALERT_DISCORD_WEBHOOK`

Client subcommands (`mcp`, `ctx`, `doc`, `tasks`, …) use `HANDOFFKEEP_URL` and
`HANDOFFKEEP_TOKEN`. A Cloudflare Access service token for an hk URL behind
Access is configured with `HANDOFFKEEP_CF_ACCESS_CLIENT_ID` and
`HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET` (env or config.env; both required
together).
