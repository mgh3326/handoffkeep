#!/usr/bin/env bash
set -euo pipefail

if [[ -z "$PACKAGES" ]]; then
  exit 0
fi
if [[ "$SUITE" != test ]]; then
  echo 'packages requires suite=test' >&2
  exit 1
fi
if [[ ! "$PACKAGES" =~ ^\./[A-Za-z0-9_][A-Za-z0-9_./-]*(\ +\./[A-Za-z0-9_][A-Za-z0-9_./-]*)*$ ]] || [[ "$PACKAGES" == *..* ]]; then
  echo 'invalid packages: expected 1-20 space-separated Go package patterns without ..' >&2
  exit 1
fi
read -r -a patterns <<< "$PACKAGES"
if (( ${#patterns[@]} > 20 )); then
  echo 'invalid packages: expected 1-20 space-separated Go package patterns without ..' >&2
  exit 1
fi
