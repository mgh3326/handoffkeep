# Releases

The operator or desk cuts a release by pushing a tag matching `vMAJOR.MINOR.PATCH`, such as `v1.2.3`. The release workflow refuses tags outside that form; it does not create or push tags itself.

Each release contains `handoffkeep_darwin_amd64`, `handoffkeep_darwin_arm64`, `handoffkeep_linux_amd64`, and `handoffkeep_linux_arm64`, plus `SHA256SUMS` with exactly four `sha256sum`-format lines: each digest is followed by two spaces and the bare asset name. Download an asset from `https://github.com/mgh3326/handoffkeep/releases/download/<tag>/handoffkeep_<os>_<arch>`.

The release notes list the `node-ops hk-cli-install` inputs: `tag`, `os_arch`, and the asset's `sha256` from `SHA256SUMS`. `handoffkeep version` reports the stamped release tag; an unstamped build reports `release=unknown`.

## Dry run on pull requests

Every pull request and manual run of the release workflow builds the same
four assets for the tag `v0.0.0`. It checks that `SHA256SUMS` has four
matching lines and that `handoffkeep version` reports `release=v0.0.0` and
the checked-out revision. It publishes nothing. Only a pushed `v*` tag runs
the Postgres-backed test job and the publish job.

## How hosts get it

No workflow connects to a host. On each host, the fleet updater
(`fleet-update`) reads the latest release, or the tag pinned for that host.
It verifies the asset against `SHA256SUMS` and swaps the binary atomically.
It keeps the old binary as `handoffkeep.prev` and restarts the owning
service. If the version or service check fails, it restores
`handoffkeep.prev`. `node-ops hk-cli-install` remains a separate path.

## Pin, hold, roll back

- **Pin or hold:** set `version=vX.Y.Z` on the host's line in the fleet-update
  manifest. Remove the pin to follow `latest` again.
- **Roll back the fleet:** pin the previous good tag. Alternatively, mark the
  bad release as a pre-release or delete it, so that `latest` points to the
  previous release on the next run.
- **Roll back one host now:** copy `handoffkeep.prev` over `handoffkeep`, then
  pin the tag so that the next run does not reinstall the bad release.
