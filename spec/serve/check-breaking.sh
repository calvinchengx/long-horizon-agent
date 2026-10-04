#!/usr/bin/env bash
# Fails when spec/serve/openapi.json breaks a client of the API at BASE (a git ref) without a new
# major version (info.version). Within a major version the API only grows (spec/serve/README.md).
set -euo pipefail
base="${1:?usage: spec/serve/check-breaking.sh <base git ref>}"
root="$(git rev-parse --show-toplevel)"
contract="spec/serve/openapi.json"
if ! git -C "$root" cat-file -e "$base:$contract" 2>/dev/null; then
  echo "no $contract at $base: nothing to compare"
  exit 0
fi
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
git -C "$root" archive "$base" spec | tar -x -C "$tmp" # with the files it \$refs
major() { python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["info"]["version"].split(".")[0])' "$1"; }
if [ "$(major "$tmp/$contract")" != "$(major "$root/$contract")" ]; then
  echo "a new major API version: breaking changes are allowed"
  exit 0
fi
cd "$root/go"
go run github.com/oasdiff/oasdiff@v1.11.7 breaking "$tmp/$contract" "$root/$contract" --fail-on ERR
echo "no breaking change against $base"
