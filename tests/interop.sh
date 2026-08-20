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

# ==============================================================================
# Format v2: delta compression, zstd containers, upgrade, and repack. These
# checks prove Go (the only v2 writer), Java (a v2 reader that keeps writing
# legacy objects), and snapvault-fsck all agree on the new on-disk forms.
# ==============================================================================

# Prints the object id a blob file would get if stored: the hex SHA-256 of
# the canonical envelope "blob <size>\0<content>", matching object.ID in Go.
function blob_id_of() {
  local file="$1"
  local size
  size="$(wc -c < "${file}" | tr -d ' ')"
  { printf 'blob %s\0' "${size}"; cat "${file}"; } | shasum -a 256 \
    | awk '{print $1}'
}

# Echoes id's on-disk object file path under repo's sharded object database.
function object_path() {
  local repo="$1"
  local id="$2"
  echo "${repo}/.snapvault/objects/${id:0:2}/${id:2}"
}

# Writes 30 slightly-different versions of a ~50 KB text file into repo,
# snapshotting after each one with the given CLI. Mirrors the Go repack
# package's own fixture so the same real delta savings show up here.
# Records one "<abbreviated commit id> <blob id>" line per version to
# versions_file, so a later check can target a specific historical version's
# object precisely instead of guessing at the object database's layout.
function build_versioned_document() {
  local cli="$1"
  local repo="$2"
  local versions_file="$3"
  local lines=()
  local i
  for ((i = 0; i < 1200; i++)); do
    lines[i]="the quick brown fox jumps over the lazy dog"
  done

  : > "${versions_file}"
  local seed=1
  local v k idx output commit_abbrev
  for ((v = 1; v <= 30; v++)); do
    for ((k = 0; k < 5; k++)); do
      seed=$(( (seed * 1103515245 + 12345) & 0x7fffffff ))
      idx=$((seed % 1200))
      lines[idx]="line ${idx} changed in version ${v} - ${seed}"
    done
    printf '%s\n' "${lines[@]}" > "${repo}/doc.txt"
    output="$("${cli}" -C "${repo}" snapshot -m "version ${v}")"
    commit_abbrev="$(printf '%s\n' "${output}" | awk '{print $2}')"
    printf '%s %s\n' "${commit_abbrev}" "$(blob_id_of "${repo}/doc.txt")" \
      >> "${versions_file}"
  done
}

# Sums the actual on-disk size of every object file in repo, in bytes.
function total_object_bytes() {
  local repo="$1"
  find "${repo}/.snapvault/objects" -type f -exec cat {} + | wc -c | tr -d ' '
}

# Sniffs one object file's on-disk form: "legacy" (zlib, still valid in a
# format 2 repository), "full" (container/full), or "delta" (container/delta).
# See docs/FORMAT.md's v2 addendum for the SVO2 envelope this reads.
function object_form() {
  local file="$1"
  case "$(head -c 5 "${file}" | od -An -tx1 | tr -d ' \n')" in
    53564f3201) echo full ;;
    53564f3202) echo delta ;;
    *) echo legacy ;;
  esac
}

# Echoes "<legacy count> <full count> <delta count>" for every object file in
# repo's object database.
function count_object_forms() {
  local repo="$1"
  local legacy=0 full=0 delta=0 file
  while IFS= read -r file; do
    case "$(object_form "${file}")" in
      legacy) legacy=$((legacy + 1)) ;;
      full) full=$((full + 1)) ;;
      delta) delta=$((delta + 1)) ;;
    esac
  done < <(find "${repo}/.snapvault/objects" -type f)
  echo "${legacy} ${full} ${delta}"
}

# Scans versions_file (as written by build_versioned_document) and prints
# the first "<commit id> <blob id>" pair whose blob is currently stored as a
# container/delta object in repo, so a caller can restore exactly the
# revision that depends on it. Fails if no version qualifies.
function find_delta_version() {
  local versions_file="$1"
  local repo="$2"
  local commit_abbrev blob_id
  while read -r commit_abbrev blob_id; do
    if [[ "$(object_form "$(object_path "${repo}" "${blob_id}")")" \
        == "delta" ]]; then
      echo "${commit_abbrev} ${blob_id}"
      return 0
    fi
  done < "${versions_file}"
  return 1
}

# Asserts java and go print identical `log` output for repo, reporting
# failures with label (e.g. "over the repacked v2 repository").
function assert_logs_match() {
  local label="$1"
  local repo="$2"
  if diff <("${JAVA_CLI}" -C "${repo}" log) \
          <("${GO_CLI}" -C "${repo}" log) > /dev/null; then
    pass "java and go print identical logs ${label}"
  else
    fail "log output differs ${label}"
  fi
}

# --- upgrade + repack a many-versions fixture: real delta savings, then
#     cross-implementation agreement on the result. --------------------------

readonly V2_REPO="${WORK}/v2-repack"
readonly V2_VERSIONS="${WORK}/v2-versions.txt"
mkdir -p "${V2_REPO}"
"${GO_CLI}" init "${V2_REPO}" > /dev/null
build_versioned_document "${GO_CLI}" "${V2_REPO}" "${V2_VERSIONS}"
"${GO_CLI}" -C "${V2_REPO}" upgrade > /dev/null

before_bytes="$(total_object_bytes "${V2_REPO}")"
readonly before_bytes
"${GO_CLI}" -C "${V2_REPO}" repack > /dev/null
after_bytes="$(total_object_bytes "${V2_REPO}")"
readonly after_bytes

shrink_detail="${before_bytes} -> ${after_bytes} bytes"
if [[ "${after_bytes}" -le $((before_bytes / 2)) ]]; then
  pass "repack shrinks a 30-version fixture by at least 50% (${shrink_detail})"
else
  fail "repack shrink of a 30-version fixture is under 50% (${shrink_detail})"
fi

if [[ "$("${JAVA_CLI}" -C "${V2_REPO}" diff)" == "No changes." ]]; then
  pass "java diff is clean over the Go-repacked v2 repository"
else
  fail "java diff over the Go-repacked v2 repository"
fi
assert_logs_match "over the repacked v2 repository" "${V2_REPO}"
"${JAVA_CLI}" -C "${V2_REPO}" restore HEAD \
  --to "${WORK}/java-restores-v2" > /dev/null
if diff -r -x .snapvault "${V2_REPO}" "${WORK}/java-restores-v2" \
    > /dev/null; then
  pass "java restores the repacked v2 repository to a matching tree"
else
  fail "java restore of the repacked v2 repository differs from the tree"
fi
if "${FSCK}" "${V2_REPO}" > /dev/null; then
  pass "fsck passes the repacked v2 repository"
else
  fail "fsck rejected the repacked v2 repository"
fi

# --- Java snapshots into the same v2 repository — legal, since it keeps
#     writing legacy objects — leaving a mix of legacy, container-full, and
#     container-delta objects that Go must read identically. -----------------

printf 'one more version, written by java\n' >> "${V2_REPO}/doc.txt"
"${JAVA_CLI}" -C "${V2_REPO}" snapshot -m 'java snapshot into a v2 repo' \
  > /dev/null

if [[ "$("${GO_CLI}" -C "${V2_REPO}" diff)" == "No changes." ]]; then
  pass "go diff is clean after a java snapshot into a v2 repository"
else
  fail "go diff after a java snapshot into a v2 repository"
fi
assert_logs_match "after a java snapshot into a v2 repository" "${V2_REPO}"
"${GO_CLI}" -C "${V2_REPO}" restore HEAD \
  --to "${WORK}/go-restores-mixed" > /dev/null
if diff -r -x .snapvault "${V2_REPO}" "${WORK}/go-restores-mixed" \
    > /dev/null; then
  pass "go restores the mixed legacy/container repository to a match"
else
  fail "go restore of the mixed legacy/container repository differs"
fi
if "${FSCK}" "${V2_REPO}" > /dev/null; then
  pass "fsck passes the mixed legacy/container-full/container-delta repo"
else
  fail "fsck rejected the mixed legacy/container-full/container-delta repo"
fi

read -r legacy_count full_count delta_count \
  < <(count_object_forms "${V2_REPO}")
form_detail="legacy=${legacy_count} full=${full_count} delta=${delta_count}"
if [[ "${legacy_count}" -gt 0 ]] \
    && [[ "${full_count}" -gt 0 ]] \
    && [[ "${delta_count}" -gt 0 ]]; then
  pass "the mixed repository holds all three object forms (${form_detail})"
else
  fail "the mixed repository is missing an object form (${form_detail})"
fi

# --- Corruption rejection: a mangled container-delta object must fail fsck
#     and be refused by restore, in both implementations. --------------------

readonly V2_CORRUPT="${WORK}/v2-corrupt"
cp -R "${V2_REPO}" "${V2_CORRUPT}"
delta_line=""
if ! delta_line="$(find_delta_version "${V2_VERSIONS}" "${V2_CORRUPT}")"; then
  fail "no historical version is stored as a container-delta object"
else
  # delta_commit is the one snapshot whose own tree references delta_blob,
  # so restoring exactly that revision is guaranteed to read it (restoring
  # HEAD would not: HEAD's own blob may land on a different object).
  read -r delta_commit delta_blob <<< "${delta_line}"
  delta_victim="$(object_path "${V2_CORRUPT}" "${delta_blob}")"
  # Overwrite the first 4 bytes of the zstd codec stream (past the 4-byte
  # magic, 1-byte kind, 1-byte codec, and 32-byte base id) so the frame no
  # longer decodes, without disturbing the header the form sniff depends on.
  printf '\xff\xff\xff\xff' \
    | dd of="${delta_victim}" bs=1 seek=38 count=4 conv=notrunc status=none
  if "${FSCK}" "${V2_CORRUPT}" > /dev/null; then
    fail "fsck accepted a repository with a corrupted delta object"
  else
    pass "fsck rejects a corrupted delta object"
  fi
  if "${GO_CLI}" -C "${V2_CORRUPT}" restore "${delta_commit}" \
      --to "${WORK}/go-restore-corrupt" > /dev/null 2>&1; then
    fail "go restore succeeded over a corrupted delta object"
  else
    pass "go restore refuses a corrupted delta object"
  fi
  if "${JAVA_CLI}" -C "${V2_CORRUPT}" restore "${delta_commit}" \
      --to "${WORK}/java-restore-corrupt" > /dev/null 2>&1; then
    fail "java restore succeeded over a corrupted delta object"
  else
    pass "java restore refuses a corrupted delta object"
  fi
fi

# --- A repository claiming format 3 is rejected by all three
#     implementations. --------------------------------------------------------

readonly V3_REPO="${WORK}/v3-unsupported"
cp -R "${V2_REPO}" "${V3_REPO}"
printf 'snapvault 3\n' > "${V3_REPO}/.snapvault/format"

if "${GO_CLI}" -C "${V3_REPO}" log > /dev/null 2>&1; then
  fail "go accepted a format 3 repository"
else
  pass "go rejects a format 3 repository"
fi
if "${JAVA_CLI}" -C "${V3_REPO}" log > /dev/null 2>&1; then
  fail "java accepted a format 3 repository"
else
  pass "java rejects a format 3 repository"
fi
if "${FSCK}" "${V3_REPO}" > /dev/null; then
  fail "fsck accepted a format 3 repository"
else
  pass "fsck rejects a format 3 repository"
fi

# ----------------------------------------------------------------------------

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} interop check(s) failed" >&2
  exit 1
fi
echo "all interop checks passed"
