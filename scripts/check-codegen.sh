#!/usr/bin/env bash
# Verify that internal/gen/playsbot is fresh: regenerate into a temporary
# directory and diff against the checked-in tree. Exit nonzero on drift.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cd "$root"
go run ./internal/tools/gen -outroot "$tmp" >/dev/null

if ! diff -r "$tmp/internal/gen/playsbot" "$root/internal/gen/playsbot" >"$tmp/check-codegen.diff" 2>&1; then
  echo "generated code is stale; run 'make codegen' and commit:" >&2
  head -40 "$tmp/check-codegen.diff" >&2
  exit 1
fi

echo "codegentest: generated code is up to date"
