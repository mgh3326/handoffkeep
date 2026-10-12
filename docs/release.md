# Releases

The operator or desk cuts a release by pushing a tag matching `vMAJOR.MINOR.PATCH`, such as `v1.2.3`. The release workflow refuses tags outside that form; it does not create or push tags itself.

Each release contains `handoffkeep_darwin_amd64`, `handoffkeep_darwin_arm64`, `handoffkeep_linux_amd64`, and `handoffkeep_linux_arm64`, plus `SHA256SUMS` with exactly four `sha256sum`-format lines: each digest is followed by two spaces and the bare asset name. Download an asset from `https://github.com/mgh3326/handoffkeep/releases/download/<tag>/handoffkeep_<os>_<arch>`.

The release notes list the `node-ops hk-cli-install` inputs: `tag`, `os_arch`, and the asset's `sha256` from `SHA256SUMS`. `handoffkeep version` reports the stamped release tag; an unstamped build reports `release=unknown`.
