#!/usr/bin/env bash
# Fails if this repo's checked-in copy of either cross-language
# conformance fixture (testdata/bucketing-vectors.json,
# testdata/rollout-outcome-vectors.json) has drifted from
# rollfuse/sdk-conformance-fixtures, the public authoritative source (see
# that repo's README; it exists because the actual source of truth,
# apps/api's evaluation domain, lives in the private rollfuse/rollfuse
# monorepo and cannot be fetched by this repo's CI directly).
#
# harden-sdk-runtime task 1.3. Complements rollfuse/rollfuse's own
# scripts/check-conformance-fixture-drift.sh, which checks this repo's
# copy from the other direction; this one catches a copy edited directly
# in this repo without waiting for that CI to next run.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURES_BASE_URL="https://raw.githubusercontent.com/rollfuse/sdk-conformance-fixtures/main"

check_file() {
  local name="$1"
  local local_copy="$REPO_ROOT/testdata/$name"
  local remote_copy
  remote_copy="$(mktemp)"

  if [ ! -f "$local_copy" ]; then
    echo "error: local copy not found at $local_copy" >&2
    rm -f "$remote_copy"
    return 1
  fi

  if ! curl -fsSL "$FIXTURES_BASE_URL/$name" -o "$remote_copy"; then
    echo "error: failed to fetch $FIXTURES_BASE_URL/$name" >&2
    rm -f "$remote_copy"
    return 1
  fi

  if ! diff -q "$local_copy" "$remote_copy" > /dev/null; then
    echo "error: testdata/$name has drifted from rollfuse/sdk-conformance-fixtures." >&2
    echo "Update testdata/$name to match (see that repo's README) before merging." >&2
    diff "$local_copy" "$remote_copy" || true
    rm -f "$remote_copy"
    return 1
  fi

  rm -f "$remote_copy"
  echo "OK: testdata/$name matches rollfuse/sdk-conformance-fixtures."
}

status=0
check_file "bucketing-vectors.json" || status=1
check_file "rollout-outcome-vectors.json" || status=1

exit "$status"
