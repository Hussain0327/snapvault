#!/bin/bash
#
# Integration tests for snapvault-fsck against real repositories written by
# the Go implementation (override with SNAPVAULT_CLI, e.g. a Java wrapper).
#
# Usage: integration_test.sh <path-to-snapvault-fsck>

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <path-to-snapvault-fsck>" >&2
  exit 2
fi

readonly FSCK="$1"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
readonly CLI="${SNAPVAULT_CLI:-${SCRIPT_DIR}/../../go/build/snapvault}"

if [[ ! -x "${CLI}" ]]; then
  echo "snapvault CLI not found at ${CLI}; build it first (make go)" >&2
  exit 1
fi

WORK="$(mktemp -d)"
readonly WORK
trap 'rm -rf "${WORK}"' EXIT

failures=0

# Runs a command, records its output, and checks its exit status.
function expect_status() {
  local expected="$1"
  local description="$2"
  shift 2
  local status=0
  "$@" > "${WORK}/last-output" 2>&1 || status=$?
  if [[ "${status}" -ne "${expected}" ]]; then
    echo "FAIL: ${description}: exit ${status}, want ${expected}" >&2
    cat "${WORK}/last-output" >&2
    failures=$((failures + 1))
  fi
}

# Checks that the previous expect_status command printed a needle.
function expect_output_contains() {
  local needle="$1"
  local description="$2"
  if ! grep -q -- "${needle}" "${WORK}/last-output"; then
    echo "FAIL: ${description}: output lacks '${needle}'" >&2
    cat "${WORK}/last-output" >&2
    failures=$((failures + 1))
  fi
}

readonly DATA="${WORK}/data"
mkdir -p "${DATA}/nested"
printf 'file one\n' > "${DATA}/one.txt"
printf 'file two\n' > "${DATA}/nested/two.txt"
ln -s one.txt "${DATA}/link"
mkdir "${DATA}/hollow"
"${CLI}" init "${DATA}" > /dev/null
"${CLI}" -C "${DATA}" snapshot -m 'first snapshot' > /dev/null

expect_status 0 'clean repository passes' "${FSCK}" "${DATA}"
expect_output_contains '0 errors' 'clean repository reports no errors'

# A corrupted object must fail the check.
corrupted="$(find "${DATA}/.snapvault/objects" -type f -size +0c | head -1)"
readonly corrupted
cp "${corrupted}" "${WORK}/saved-object"
printf '\x00\x00\x00\x00' | dd of="${corrupted}" bs=1 count=4 \
  conv=notrunc status=none
expect_status 1 'corrupted object fails' "${FSCK}" "${DATA}"
cp "${WORK}/saved-object" "${corrupted}"
expect_status 0 'repaired repository passes again' "${FSCK}" "${DATA}"

# A missing referenced object must fail the check.
rm "${corrupted}"
expect_status 1 'missing object fails' "${FSCK}" "${DATA}"
expect_output_contains 'does not exist' 'missing object is named'
cp "${WORK}/saved-object" "${corrupted}"

# Rewinding the ref makes the newer snapshot's objects unreachable, which
# warns without failing.
first_commit="$(cat "${DATA}/.snapvault/refs/heads/main")"
readonly first_commit
printf 'changed\n' > "${DATA}/one.txt"
"${CLI}" -C "${DATA}" snapshot -m 'second snapshot' > /dev/null
printf '%s\n' "${first_commit}" > "${DATA}/.snapvault/refs/heads/main"
expect_status 0 'unreachable objects only warn' "${FSCK}" "${DATA}"
expect_output_contains 'unreachable' 'unreachable objects are reported'

# An interrupted restore marker warns without failing.
printf '%s\n%s\n' "${first_commit}" "${DATA}" \
  > "${DATA}/.snapvault/restore-in-progress"
expect_status 0 'restore marker only warns' "${FSCK}" "${DATA}"
expect_output_contains 'interrupted' 'restore marker is reported'
rm "${DATA}/.snapvault/restore-in-progress"

expect_status 2 'missing argument is a usage error' "${FSCK}"
expect_status 1 'non-repository fails' "${FSCK}" "${WORK}"

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} integration check(s) failed" >&2
  exit 1
fi
echo 'all integration tests passed'
