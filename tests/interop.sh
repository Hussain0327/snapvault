#!/bin/bash
#
# Cross-implementation interoperability suite: proves that the Java and Go
# implementations write byte-identical format-v1 repositories, read each
# other's output identically, and that snapvault-fsck verifies both.
#
# Run via `make interop`, which builds all three implementations first.

# TZ pins log timestamps for output comparison. LC_ALL is deliberately NOT
# exported: a C locale makes Java decode non-ASCII filenames as ASCII on
# Linux, breaking the café.txt fixture; sort calls pin their own locale.
set -euo pipefail
export TZ=UTC

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT
readonly JAVA_CLI="${ROOT}/java/snapvault"
readonly GO_CLI="${ROOT}/go/build/snapvault"
readonly FSCK="${ROOT}/cpp/build/snapvault-fsck"

for tool in "${JAVA_CLI}" "${GO_CLI}" "${FSCK}"; do
  if [[ ! -x "${tool}" ]]; then
    echo "missing ${tool}; run 'make java go cpp' first" >&2
    exit 1
  fi
done

WORK="$(mktemp -d)"
readonly WORK
trap 'rm -rf "${WORK}"' EXIT

failures=0

function fail() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}

function pass() {
  echo "ok: $1"
}

# Builds the shared fixture tree: nested directories, an empty directory,
# an executable, a symlink, and a name that exercises non-ASCII sorting.
function make_fixture() {
  local dir="$1"
  mkdir -p "${dir}/nested/deeper" "${dir}/hollow"
  printf 'alpha content\n' > "${dir}/alpha.txt"
  printf 'nested content\n' > "${dir}/nested/beta.txt"
  printf 'deep content\n' > "${dir}/nested/deeper/gamma.txt"
  printf '#!/bin/sh\necho hi\n' > "${dir}/tool.sh"
  chmod 755 "${dir}/tool.sh"
  printf 'unicode content\n' > "${dir}/café.txt"
  ln -s alpha.txt "${dir}/link"
}

# Lists an object database's files relative to the objects directory,
# excluding the given commit id, whose bytes contain a timestamp and so
# legitimately differ between repositories.
function list_objects_except_commit() {
  local repo="$1"
  local commit="$2"
  (cd "${repo}/.snapvault/objects" && find . -type f | LC_ALL=C sort) \
    | grep -v "^\./${commit:0:2}/${commit:2}$"
}

# --- Write one repository with each implementation. -------------------------

readonly JAVA_REPO="${WORK}/java-written"
make_fixture "${JAVA_REPO}"
"${JAVA_CLI}" init "${JAVA_REPO}" > /dev/null
"${JAVA_CLI}" -C "${JAVA_REPO}" snapshot -m 'interop snapshot' > /dev/null

readonly GO_REPO="${WORK}/go-written"
make_fixture "${GO_REPO}"
"${GO_CLI}" init "${GO_REPO}" > /dev/null
"${GO_CLI}" -C "${GO_REPO}" snapshot -m 'interop snapshot' > /dev/null

# --- Each implementation reads the other's repository as its own. -----------

if [[ "$("${GO_CLI}" -C "${JAVA_REPO}" diff)" == "No changes." ]]; then
  pass "go diff is clean over the Java-written repository"
else
  fail "go diff over the Java-written repository"
fi
if [[ "$("${JAVA_CLI}" -C "${GO_REPO}" diff)" == "No changes." ]]; then
  pass "java diff is clean over the Go-written repository"
else
  fail "java diff over the Go-written repository"
fi

for repo in "${JAVA_REPO}" "${GO_REPO}"; do
  if diff <("${JAVA_CLI}" -C "${repo}" log) \
          <("${GO_CLI}" -C "${repo}" log) > /dev/null; then
    pass "java and go print identical logs for $(basename "${repo}")"
  else
    fail "log output differs for $(basename "${repo}")"
  fi
  if diff <("${JAVA_CLI}" -C "${repo}" log --oneline) \
          <("${GO_CLI}" -C "${repo}" log --oneline) > /dev/null; then
    pass "java and go print identical oneline logs for $(basename "${repo}")"
  else
    fail "oneline log output differs for $(basename "${repo}")"
  fi
done

# --- Equal content must produce equal object ids in both languages. ---------

java_commit="$(cat "${JAVA_REPO}/.snapvault/refs/heads/main")"
readonly java_commit
go_commit="$(cat "${GO_REPO}/.snapvault/refs/heads/main")"
readonly go_commit
if diff <(list_objects_except_commit "${JAVA_REPO}" "${java_commit}") \
        <(list_objects_except_commit "${GO_REPO}" "${go_commit}") \
        > /dev/null; then
  pass "tree and blob object ids are identical across implementations"
else
  fail "object databases differ beyond the commit object"
fi

# --- Cross restore: each implementation materializes the other's snapshot. --

readonly REFERENCE="${WORK}/reference"
make_fixture "${REFERENCE}"

function verify_restore() {
  local label="$1"
  local restored="$2"
  if diff -r "${REFERENCE}" "${restored}" > /dev/null; then
    pass "${label}: restored tree matches the fixture"
  else
    fail "${label}: restored tree differs from the fixture"
  fi
  if [[ "$(readlink "${restored}/link")" == "alpha.txt" ]]; then
    pass "${label}: symlink target survived"
  else
    fail "${label}: symlink target lost"
  fi
  if [[ -x "${restored}/tool.sh" ]]; then
    pass "${label}: executable bit survived"
  else
    fail "${label}: executable bit lost"
  fi
  if [[ -d "${restored}/hollow" ]]; then
    pass "${label}: empty directory survived"
  else
    fail "${label}: empty directory lost"
  fi
}

"${GO_CLI}" -C "${JAVA_REPO}" restore HEAD --to "${WORK}/go-restores-java" \
  > /dev/null
verify_restore "go restores java" "${WORK}/go-restores-java"
"${JAVA_CLI}" -C "${GO_REPO}" restore HEAD --to "${WORK}/java-restores-go" \
  > /dev/null
verify_restore "java restores go" "${WORK}/java-restores-go"

# --- fsck accepts both repositories and rejects a corrupted one. ------------

for repo in "${JAVA_REPO}" "${GO_REPO}"; do
  if "${FSCK}" "${repo}" > /dev/null; then
    pass "fsck passes $(basename "${repo}")"
  else
    fail "fsck rejected $(basename "${repo}")"
  fi
done

readonly CORRUPT="${WORK}/corrupt"
cp -R "${GO_REPO}" "${CORRUPT}"
victim="$(find "${CORRUPT}/.snapvault/objects" -type f | head -1)"
readonly victim
printf '\x00\x00\x00\x00' \
  | dd of="${victim}" bs=1 count=4 conv=notrunc status=none
if "${FSCK}" "${CORRUPT}" > /dev/null; then
  fail "fsck accepted a corrupted object"
else
  pass "fsck rejects a corrupted object"
fi

# ----------------------------------------------------------------------------

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} interop check(s) failed" >&2
  exit 1
fi
echo "all interop checks passed"
