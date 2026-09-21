# Container image (GHCR)

`.github/workflows/image.yml` builds the image on every PR (build + smoke only)
and pushes it to GHCR on each `main` push. This pipeline only *produces* the
image — the NCP systemd deployment is unchanged.

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
- `serve --linear-sync` (off by default): `HK_LINEAR_API_KEY`,
  `HK_LINEAR_TEAM_ID`, `HK_LINEAR_API_URL`
- Attachments / R2 (all-or-none): `HK_S3_ENDPOINT`, `HK_S3_BUCKET`,
  `HK_S3_ACCESS_KEY_ID`, `HK_S3_SECRET_ACCESS_KEY`; limits:
  `HK_ATTACH_MAX_BYTES`, `HK_ATTACH_STORAGE_CAP_BYTES`,
  `HK_ATTACH_MONTHLY_PUT_CAP`, `HK_ATTACH_MONTHLY_GET_CAP`,
  `HK_ATTACH_MIME_ALLOW`
- `r2usage`: `CF_API_TOKEN`, `CF_ACCOUNT_ID`; alert webhook:
  `HK_ALERT_DISCORD_WEBHOOK`

Client subcommands (`mcp`, `ctx`, `doc`, `tasks`, …) use `HANDOFFKEEP_URL` and
`HANDOFFKEEP_TOKEN`.
