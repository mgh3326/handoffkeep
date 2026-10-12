#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_script=${1:-"$repo_root/scripts/build-release.sh"}
tmp_dir=$(mktemp -d)
trap 'rm -rf -- "$tmp_dir"' EXIT

host_goos=$(go env GOOS)
host_goarch=$(go env GOARCH)
host_target="${host_goos}_${host_goarch}"
tag=v9.8.7
release_dir="$tmp_dir/release"
mkdir -p "$release_dir"
HANDOFFKEEP_RELEASE_TARGETS="$host_target" "$build_script" "$tag" "$release_dir" > "$tmp_dir/build.out"

asset="$release_dir/handoffkeep_${host_target}"
[[ -f $asset ]] || { printf 'missing host asset: %s\n' "$asset" >&2; exit 1; }
[[ $(wc -l < "$release_dir/SHA256SUMS") -eq 1 ]] || {
  printf 'expected one SHA256SUMS line for the host target\n' >&2
  exit 1
}
(cd "$release_dir" && sha256sum -c SHA256SUMS)
version_output=$("$asset" version)
[[ $version_output == *"release=$tag"* ]] || {
  printf 'release stamp missing from host asset: %s\n' "$version_output" >&2
  exit 1
}
expected_rev=$(git -C "$repo_root" rev-parse HEAD)
[[ $version_output == *"rev=$expected_rev"* ]] || {
  printf 'VCS revision missing from host asset: %s\n' "$version_output" >&2
  exit 1
}

bad_tags=(v1.2 1.2.3 v1.2.3-rc1 v1.2.3/extra)
index=0
for bad_tag in "${bad_tags[@]}"; do
  bad_dir="$tmp_dir/bad-$index"
  mkdir -p "$bad_dir"
  if HANDOFFKEEP_RELEASE_TARGETS="$host_target" "$build_script" "$bad_tag" "$bad_dir" > "$tmp_dir/bad.out" 2> "$tmp_dir/bad.err"; then
    printf 'bad tag was accepted: %s\n' "$bad_tag" >&2
    exit 1
  fi
  if [[ -n $(find "$bad_dir" -type f -print -quit) ]]; then
    printf 'bad tag created output files: %s\n' "$bad_tag" >&2
    exit 1
  fi
  index=$((index + 1))
done

unknown_dir="$tmp_dir/unknown-target"
mkdir -p "$unknown_dir"
if HANDOFFKEEP_RELEASE_TARGETS=windows_amd64 "$build_script" "$tag" "$unknown_dir" > "$tmp_dir/unknown.out" 2> "$tmp_dir/unknown.err"; then
  printf 'unknown target was accepted\n' >&2
  exit 1
fi
if [[ -n $(find "$unknown_dir" -type f -print -quit) ]]; then
  printf 'unknown target created output files\n' >&2
  exit 1
fi

unstamped="$tmp_dir/handoffkeep-unstamped"
(
  cd "$repo_root"
  CGO_ENABLED=0 go build -buildvcs=true -trimpath -o "$unstamped" ./cmd/handoffkeep
)
unstamped_output=$("$unstamped" version)
[[ $unstamped_output == *'release=unknown'* ]] || {
  printf 'unstamped build did not print release=unknown: %s\n' "$unstamped_output" >&2
  exit 1
}

printf 'build-release contract tests passed for %s\n' "$host_target"
