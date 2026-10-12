#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'usage: %s TAG OUTPUT_DIR\n' "${0##*/}" >&2
  exit 2
}

[[ $# -eq 2 ]] || usage

tag=$1
output_dir=$2

if [[ ! $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'invalid release tag: %s\n' "$tag" >&2
  exit 1
fi

target_text=${HANDOFFKEEP_RELEASE_TARGETS:-'darwin_amd64 darwin_arm64 linux_amd64 linux_arm64'}
read -r -a targets <<< "$target_text"
if ((${#targets[@]} == 0)); then
  printf 'release target list is empty\n' >&2
  exit 1
fi

for ((i = 0; i < ${#targets[@]}; i++)); do
  target=${targets[$i]}
  case "$target" in
    darwin_amd64|darwin_arm64|linux_amd64|linux_arm64) ;;
    *)
      printf 'unknown release target: %s\n' "$target" >&2
      exit 1
      ;;
  esac
  for ((j = 0; j < i; j++)); do
    if [[ ${targets[$j]} == "$target" ]]; then
      printf 'duplicate release target: %s\n' "$target" >&2
      exit 1
    fi
  done
done

command -v sha256sum >/dev/null 2>&1 || {
  printf 'sha256sum is required\n' >&2
  exit 1
}

script_dir=$(cd -- "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/.." && pwd)
if [[ $output_dir != /* ]]; then
  output_dir="$PWD/$output_dir"
fi

stage_dir=$(mktemp -d)
trap 'rm -rf "$stage_dir"' EXIT

cd "$repo_root"
for target in "${targets[@]}"; do
  IFS=_ read -r goos goarch <<< "$target"
  asset="handoffkeep_${target}"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
    -buildvcs=true \
    -trimpath \
    -ldflags "-X main.releaseTag=$tag" \
    -o "$stage_dir/$asset" \
    ./cmd/handoffkeep
done

(
  cd "$stage_dir"
  : > SHA256SUMS
  for target in "${targets[@]}"; do
    sha256sum "handoffkeep_${target}" >> SHA256SUMS
  done
)

mkdir -p "$output_dir"
for target in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
  rm -f "$output_dir/handoffkeep_${target}"
done
rm -f "$output_dir/SHA256SUMS"
for target in "${targets[@]}"; do
  cp "$stage_dir/handoffkeep_${target}" "$output_dir/"
done
cp "$stage_dir/SHA256SUMS" "$output_dir/"

printf '%s\n' "$tag"
