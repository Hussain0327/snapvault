// Unit tests for snapvault-fsck, using golden vectors produced by the Java
// reference implementation so all three languages agree byte for byte.

#include <zlib.h>
#include <zstd.h>

#include <atomic>
#include <cstdint>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <sstream>
#include <string>
#include <vector>

#include "src/bytes.h"
#include "src/delta.h"
#include "src/fsck.h"
#include "src/object_store.h"
#include "src/parse.h"
#include "src/sha256.h"

namespace {

int failures = 0;

#define EXPECT(cond)                                                     \
  do {                                                                   \
    if (!(cond)) {                                                       \
      std::cerr << "FAIL " << __FILE__ << ":" << __LINE__ << ": " #cond  \
                << "\n";                                                 \
      ++failures;                                                        \
    }                                                                    \
  } while (0)

std::vector<uint8_t> FromHex(const std::string& hex) {
  std::vector<uint8_t> out;
  for (size_t i = 0; i + 1 < hex.size(); i += 2) {
    out.push_back(static_cast<uint8_t>(
        std::stoi(hex.substr(i, 2), nullptr, 16)));
  }
  return out;
}

// Golden tree from GoldenVectors.java: entries sorted in UTF-16 code-unit
// order (Z-dir, a.txt, link, U+10000, U+FFFD).
const char kGoldenTreeHex[] =
    "5356543100000005000000055a2d6469720200bccaa8985496fdc553fa99487f038d3c"
    "1c5cb5aebbe47d7d9f12bc758820106a00000005612e74787401010bd69098bd9b9cc5"
    "934a610ab65da429b525361147faa7b5b922919e9a23143d000000046c696e6b03000b"
    "d69098bd9b9cc5934a610ab65da429b525361147faa7b5b922919e9a23143d00000004"
    "f090808001000bd69098bd9b9cc5934a610ab65da429b525361147faa7b5b922919e9a"
    "23143d00000003efbfbd01000bd69098bd9b9cc5934a610ab65da429b525361147faa7"
    "b5b922919e9a23143d";

const char kGoldenRootCommitHex[] =
    "5356433179dc49597b4cc697ddad8ca36f36561ae631fa8f4fa20a391ad505022ad834"
    "2000000000000000006553f100075bcd150000001046697273740a0a426f6479206c69"
    "6e65";

void TestSha256Vectors() {
  EXPECT(snapvault::HexDigest(snapvault::Sha256Of(nullptr, 0)) ==
         "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  const std::string abc = "abc";
  EXPECT(snapvault::HexDigest(snapvault::Sha256Of(
             reinterpret_cast<const uint8_t*>(abc.data()), abc.size())) ==
         "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  const std::string two_blocks =
      "abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq";
  EXPECT(snapvault::HexDigest(snapvault::Sha256Of(
             reinterpret_cast<const uint8_t*>(two_blocks.data()),
             two_blocks.size())) ==
         "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1");
}

void TestSha256IncrementalMatchesOneShot() {
  std::string input;
  for (int i = 0; i < 1000; ++i) input += "chunked hashing ";
  snapvault::Sha256 incremental;
  size_t offset = 0;
  size_t step = 1;
  while (offset < input.size()) {
    size_t take = std::min(step, input.size() - offset);
    incremental.Update(
        reinterpret_cast<const uint8_t*>(input.data()) + offset, take);
    offset += take;
    step = step * 3 + 1;  // uneven chunks cross block boundaries
  }
  EXPECT(snapvault::HexDigest(incremental.Finish()) ==
         snapvault::HexDigest(snapvault::Sha256Of(
             reinterpret_cast<const uint8_t*>(input.data()), input.size())));
}

void TestCompareUtf16MatchesJavaStringOrder() {
  // U+10000 (f0 90 80 80) sorts before U+FFFD (ef bf bd) in UTF-16 code-unit
  // order even though its UTF-8 bytes are greater.
  const std::string supplementary = "\xf0\x90\x80\x80";
  const std::string replacement = "\xef\xbf\xbd";
  EXPECT(snapvault::CompareUtf16(supplementary, replacement) < 0);
  EXPECT(supplementary.compare(replacement) > 0);
  EXPECT(snapvault::CompareUtf16("a", "b") < 0);
  EXPECT(snapvault::CompareUtf16("b", "a") > 0);
  EXPECT(snapvault::CompareUtf16("same", "same") == 0);
  EXPECT(snapvault::CompareUtf16("Z-dir", "a.txt") < 0);
}

void TestParseGoldenTree() {
  std::vector<snapvault::TreeEntry> entries;
  std::string error;
  EXPECT(snapvault::ParseTree(FromHex(kGoldenTreeHex), &entries, &error));
  EXPECT(error.empty());
  EXPECT(entries.size() == 5);
  if (entries.size() == 5) {
    EXPECT(entries[0].name == "Z-dir");
    EXPECT(entries[0].kind == snapvault::kKindDirectory);
    EXPECT(entries[1].name == "a.txt");
    EXPECT(entries[1].executable);
    EXPECT(entries[2].name == "link");
    EXPECT(entries[2].kind == snapvault::kKindSymlink);
    EXPECT(entries[3].name == "\xf0\x90\x80\x80");
    EXPECT(entries[4].name == "\xef\xbf\xbd");
    EXPECT(entries[1].object_id ==
           "0bd69098bd9b9cc5934a610ab65da429b525361147faa7b5b922919e9a23143d");
  }
}

void TestParseTreeRejectsSpecViolations() {
  std::vector<snapvault::TreeEntry> entries;
  std::string error;

  auto bad_magic = FromHex(kGoldenTreeHex);
  bad_magic[0] = 'X';
  EXPECT(!snapvault::ParseTree(bad_magic, &entries, &error));

  auto truncated = FromHex(kGoldenTreeHex);
  truncated.pop_back();
  EXPECT(!snapvault::ParseTree(truncated, &entries, &error));

  auto trailing = FromHex(kGoldenTreeHex);
  trailing.push_back(0);
  EXPECT(!snapvault::ParseTree(trailing, &entries, &error));

  // Swapping the last two entries yields UTF-8 byte order, which violates
  // the spec's UTF-16 ordering; fsck must flag it. The U+10000 entry spans
  // 42 bytes (4-byte name) and the final U+FFFD entry 41 bytes.
  auto golden = FromHex(kGoldenTreeHex);
  const size_t fffd_size = 4 + 3 + 2 + 32;
  const size_t supplementary_size = 4 + 4 + 2 + 32;
  const size_t split = golden.size() - fffd_size - supplementary_size;
  std::vector<uint8_t> unsorted(golden.begin(), golden.begin() + split);
  unsorted.insert(unsorted.end(), golden.end() - fffd_size, golden.end());
  unsorted.insert(unsorted.end(), golden.begin() + split,
                  golden.end() - fffd_size);
  EXPECT(unsorted.size() == golden.size());
  EXPECT(!snapvault::ParseTree(unsorted, &entries, &error));

  auto executable_two = FromHex(kGoldenTreeHex);
  // a.txt's executable byte follows magic(4) count(4) entry0(4+5+2+32) name
  // length(4) name(5) kind(1).
  const size_t exec_offset = 4 + 4 + (4 + 5 + 2 + 32) + 4 + 5 + 1;
  EXPECT(executable_two[exec_offset] == 1);
  executable_two[exec_offset] = 2;
  EXPECT(!snapvault::ParseTree(executable_two, &entries, &error));

  auto executable_dir = FromHex(kGoldenTreeHex);
  const size_t dir_exec_offset = 4 + 4 + 4 + 5 + 1;
  EXPECT(executable_dir[dir_exec_offset] == 0);
  executable_dir[dir_exec_offset] = 1;
  EXPECT(!snapvault::ParseTree(executable_dir, &entries, &error));

  auto bad_kind = FromHex(kGoldenTreeHex);
  const size_t kind_offset = 4 + 4 + 4 + 5;
  EXPECT(bad_kind[kind_offset] == snapvault::kKindDirectory);
  bad_kind[kind_offset] = 4;
  EXPECT(!snapvault::ParseTree(bad_kind, &entries, &error));
}

void TestParseGoldenCommit() {
  snapvault::Commit commit;
  std::string error;
  EXPECT(snapvault::ParseCommit(
      FromHex(kGoldenRootCommitHex), &commit, &error));
  EXPECT(commit.tree_id ==
         "79dc49597b4cc697ddad8ca36f36561ae631fa8f4fa20a391ad505022ad83420");
  EXPECT(commit.parents.empty());
  EXPECT(commit.seconds == 1700000000);
  EXPECT(commit.nanos == 123456789);
  EXPECT(commit.message == "First\n\nBody line");
}

void TestParseCommitRejectsCorruptPayloads() {
  snapvault::Commit commit;
  std::string error;

  auto bad_magic = FromHex(kGoldenRootCommitHex);
  bad_magic[0] = 'X';
  EXPECT(!snapvault::ParseCommit(bad_magic, &commit, &error));

  auto truncated = FromHex(kGoldenRootCommitHex);
  truncated.pop_back();
  EXPECT(!snapvault::ParseCommit(truncated, &commit, &error));

  auto trailing = FromHex(kGoldenRootCommitHex);
  trailing.push_back(0);
  EXPECT(!snapvault::ParseCommit(trailing, &commit, &error));

  auto bad_nano = FromHex(kGoldenRootCommitHex);
  // Nanoseconds sit after magic(4) tree(32) parent count(4) seconds(8).
  bad_nano[48] = 0x7f;
  bad_nano[49] = 0xff;
  bad_nano[50] = 0xff;
  bad_nano[51] = 0xff;
  EXPECT(!snapvault::ParseCommit(bad_nano, &commit, &error));
}

void TestIsValidObjectId() {
  EXPECT(snapvault::IsValidObjectId(
      "0bd69098bd9b9cc5934a610ab65da429b525361147faa7b5b922919e9a23143d"));
  EXPECT(!snapvault::IsValidObjectId("abc"));
  EXPECT(!snapvault::IsValidObjectId(
      "0BD69098BD9B9CC5934A610AB65DA429B525361147FAA7B5B922919E9A23143D"));
  EXPECT(!snapvault::IsValidObjectId(std::string(64, 'g')));
}

// ---------------------------------------------------------------------------
// ApplyDelta (docs/FORMAT.md v2 "Delta instruction format").

std::vector<uint8_t> Bytes(std::initializer_list<int> values) {
  std::vector<uint8_t> out;
  out.reserve(values.size());
  for (const int v : values) {
    out.push_back(static_cast<uint8_t>(v));
  }
  return out;
}

std::vector<uint8_t> StringBytes(const std::string& s) {
  return std::vector<uint8_t>(s.begin(), s.end());
}

// LEB128 base-128 varint, matching the delta wire format's srcSize/tgtSize
// encoding. Only used by tests to build fixtures; the production decoder
// lives in delta.cc.
std::vector<uint8_t> EncodeVarint(uint64_t value) {
  std::vector<uint8_t> out;
  do {
    uint8_t byte = value & 0x7f;
    value >>= 7;
    if (value != 0) {
      byte |= 0x80;
    }
    out.push_back(byte);
  } while (value != 0);
  return out;
}

void Append(std::vector<uint8_t>* dst, const std::vector<uint8_t>& src) {
  dst->insert(dst->end(), src.begin(), src.end());
}

// The worked example from docs/FORMAT.md, reproduced byte for byte so every
// language's decoder agrees on it.
void TestApplyDeltaWorkedExample() {
  const std::vector<uint8_t> base =
      StringBytes(std::string("blob 12\0hello world\n", 20));
  EXPECT(base.size() == 20);
  const std::vector<uint8_t> want_target =
      StringBytes(std::string("blob 13\0hello worlds\n", 21));
  EXPECT(want_target.size() == 21);
  const std::vector<uint8_t> delta =
      Bytes({0x14, 0x15, 0x08, 0x62, 0x6c, 0x6f, 0x62, 0x20, 0x31, 0x33, 0x00,
             0x91, 0x08, 0x0b, 0x02, 0x73, 0x0a});

  std::vector<uint8_t> target;
  std::string error;
  EXPECT(snapvault::ApplyDelta(base, delta, 1 << 20, &target, &error));
  EXPECT(error.empty());
  EXPECT(target == want_target);
}

void TestApplyDeltaMultiByteVarintAndCopy() {
  // A base long enough (200 bytes) that its size needs a two-byte varint,
  // reconstructed target is the base with " done" appended, requiring one
  // copy instruction whose size field also needs more than the trivial
  // one-byte opcode-embedded case to express (200 > 127).
  std::string base_text;
  for (int i = 0; i < 20; ++i) base_text += "0123456789";
  EXPECT(base_text.size() == 200);
  const std::vector<uint8_t> base = StringBytes(base_text);

  std::vector<uint8_t> delta;
  Append(&delta, EncodeVarint(200));  // srcSize, two-byte varint (0xC8, 0x01)
  EXPECT(EncodeVarint(200) == Bytes({0xc8, 0x01}));
  Append(&delta, EncodeVarint(205));  // tgtSize
  // Copy the entire 200-byte base: offset omitted (0), one size byte (200).
  delta.push_back(0x80 | 0x10);
  delta.push_back(200);
  // Insert the trailing literal.
  delta.push_back(5);
  Append(&delta, StringBytes(" done"));

  std::vector<uint8_t> target;
  std::string error;
  EXPECT(snapvault::ApplyDelta(base, delta, 1 << 20, &target, &error));
  EXPECT(error.empty());
  EXPECT(target == StringBytes(base_text + " done"));
}

void TestApplyDeltaSizeZeroMeans65536() {
  const std::vector<uint8_t> base(65536, 0x5a);
  std::vector<uint8_t> delta;
  Append(&delta, EncodeVarint(65536));
  Append(&delta, EncodeVarint(65536));
  delta.push_back(0x80);  // copy, offset and size both omitted (0 -> 65536)

  std::vector<uint8_t> target;
  std::string error;
  EXPECT(snapvault::ApplyDelta(base, delta, 1 << 20, &target, &error));
  EXPECT(error.empty());
  EXPECT(target == base);
}

void TestApplyDeltaRejectsSrcSizeMismatch() {
  const std::vector<uint8_t> base(10, 'x');
  std::vector<uint8_t> delta;
  Append(&delta, EncodeVarint(11));  // wrong: base is 10 bytes
  Append(&delta, EncodeVarint(0));
  std::vector<uint8_t> target;
  std::string error;
  EXPECT(!snapvault::ApplyDelta(base, delta, 1 << 20, &target, &error));
  EXPECT(!error.empty());
  EXPECT(target.empty());
}

void TestApplyDeltaRejectsTgtSizeMismatch() {
  const std::vector<uint8_t> base(10, 'x');
  std::vector<uint8_t> delta;
  Append(&delta, EncodeVarint(10));
  Append(&delta, EncodeVarint(5));  // claims 5 bytes
  delta.push_back(3);               // but inserts 3 literal bytes only
  Append(&delta, StringBytes("abc"));
  std::vector<uint8_t> target;
  std::string error;
  EXPECT(!snapvault::ApplyDelta(base, delta, 1 << 20, &target, &error));
}

void TestApplyDeltaRejectsOutOfBoundsCopy() {
  const std::vector<uint8_t> base(10, 'x');
  std::vector<uint8_t> delta;
  Append(&delta, EncodeVarint(10));
  Append(&delta, EncodeVarint(10));
  // offset=5, size=10 -> reads past the end of a 10-byte base.
  delta.push_back(0x80 | 0x01 | 0x10);
  delta.push_back(5);
  delta.push_back(10);
  std::vector<uint8_t> target;
  std::string error;
  EXPECT(!snapvault::ApplyDelta(base, delta, 1 << 20, &target, &error));
}

void TestApplyDeltaRejectsTruncatedVarint() {
  std::vector<uint8_t> delta = {0x80};  // continuation bit set, then EOF
  std::vector<uint8_t> target;
  std::string error;
  EXPECT(!snapvault::ApplyDelta({}, delta, 1 << 20, &target, &error));
}

void TestApplyDeltaRejectsTruncatedLiteral() {
  const std::vector<uint8_t> base;
  std::vector<uint8_t> delta;
  Append(&delta, EncodeVarint(0));
  Append(&delta, EncodeVarint(5));
  delta.push_back(5);  // insert 5 bytes, but only 2 follow
  Append(&delta, StringBytes("ab"));
  std::vector<uint8_t> target;
  std::string error;
  EXPECT(!snapvault::ApplyDelta(base, delta, 1 << 20, &target, &error));
}

void TestApplyDeltaRejectsTruncatedCopyFields() {
  const std::vector<uint8_t> base(10, 'x');
  std::vector<uint8_t> delta;
  Append(&delta, EncodeVarint(10));
  Append(&delta, EncodeVarint(1));
  delta.push_back(0x81);  // copy, one offset byte declared, but stream ends
  std::vector<uint8_t> target;
  std::string error;
  EXPECT(!snapvault::ApplyDelta(base, delta, 1 << 20, &target, &error));
}

void TestApplyDeltaRejectsOpcodeZero() {
  std::vector<uint8_t> delta;
  Append(&delta, EncodeVarint(0));
  Append(&delta, EncodeVarint(0));
  delta.push_back(0x00);
  std::vector<uint8_t> target;
  std::string error;
  EXPECT(!snapvault::ApplyDelta({}, delta, 1 << 20, &target, &error));
}

void TestApplyDeltaEnforcesOutputCap() {
  const std::vector<uint8_t> base(10, 'x');
  std::vector<uint8_t> delta;
  Append(&delta, EncodeVarint(10));
  Append(&delta, EncodeVarint(1000));  // declared target exceeds the cap
  delta.push_back(0x80 | 0x10);
  delta.push_back(0);  // size byte 0 in a 3-byte-capable field: still 0
  std::vector<uint8_t> target;
  std::string error;
  EXPECT(!snapvault::ApplyDelta(base, delta, 100, &target, &error));
  EXPECT(target.empty());
}

// delta.h documents that target is left empty on failure. Earlier
// instructions in the stream can write real output before a later
// instruction fails, so this drives that case directly: one valid insert
// (which lands 3 bytes in target) followed by an out-of-bounds copy, and
// asserts target does not retain the insert's bytes.
void TestApplyDeltaClearsPartialOutputOnMidStreamFailure() {
  const std::vector<uint8_t> base(10, 'x');
  std::vector<uint8_t> delta;
  Append(&delta, EncodeVarint(10));  // srcSize
  Append(&delta, EncodeVarint(13));  // tgtSize
  delta.push_back(3);                // insert 3 literal bytes
  Append(&delta, StringBytes("abc"));
  // offset=5, size=10 -> reads past the end of a 10-byte base.
  delta.push_back(0x80 | 0x01 | 0x10);
  delta.push_back(5);
  delta.push_back(10);

  std::vector<uint8_t> target;
  std::string error;
  EXPECT(!snapvault::ApplyDelta(base, delta, 1 << 20, &target, &error));
  EXPECT(!error.empty());
  EXPECT(target.empty());
}

// Cross-language delta golden vectors, shared verbatim with the Go and Java
// suites. See tests/golden/v2/delta/MANIFEST.md for what each case covers
// and how to regenerate them. SNAPVAULT_GOLDEN_DELTA_DIR is the fixture
// directory's absolute path, set by CMakeLists.txt from
// CMAKE_CURRENT_SOURCE_DIR so this test finds the fixtures regardless of
// ctest's working directory.
#ifndef SNAPVAULT_GOLDEN_DELTA_DIR
#error "SNAPVAULT_GOLDEN_DELTA_DIR must be defined by CMakeLists.txt"
#endif

const char* const kGoldenDeltaCases[] = {
    "01-worked-example", "02-multi-byte-varint", "03-copy-65536",
    "04-insert-chain",   "05-binary-content",     "06-mixed-edits",
};

std::vector<uint8_t> ReadGoldenDeltaFile(const std::string& stem,
                                         const std::string& extension) {
  const std::filesystem::path path =
      std::filesystem::path(SNAPVAULT_GOLDEN_DELTA_DIR) /
      (stem + "." + extension);
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    std::cerr << "FAIL: cannot open golden delta fixture " << path
              << " (see tests/golden/v2/delta/MANIFEST.md)\n";
    ++failures;
    return {};
  }
  std::ostringstream buffer;
  buffer << in.rdbuf();
  const std::string contents = buffer.str();
  return std::vector<uint8_t>(contents.begin(), contents.end());
}

void TestGoldenDeltaVectorsApplyToTarget() {
  for (const char* name : kGoldenDeltaCases) {
    const std::vector<uint8_t> base = ReadGoldenDeltaFile(name, "base");
    const std::vector<uint8_t> delta = ReadGoldenDeltaFile(name, "delta");
    const std::vector<uint8_t> want_target =
        ReadGoldenDeltaFile(name, "target");

    std::vector<uint8_t> target;
    std::string error;
    if (!snapvault::ApplyDelta(base, delta, 1 << 20, &target, &error)) {
      std::cerr << "FAIL: ApplyDelta(" << name << ".base, " << name
                << ".delta) returned an error: " << error << "\n";
      ++failures;
      continue;
    }
    EXPECT(target == want_target);
  }
}

// Shared cross-language *negative* delta fixtures: malformed streams every
// decoder must refuse. Live under kGoldenDeltaCases' directory in a reject/
// subdirectory (a matching .base and .delta, deliberately no .target) so a
// reject case can never be mistaken for an accept case; see
// tests/golden/v2/delta/MANIFEST.md. want_substring is text ApplyDelta's
// error must contain, pinning *why* the stream is rejected and not just
// that it is.
struct GoldenDeltaRejectCase {
  const char* name;
  const char* want_substring;
};

const GoldenDeltaRejectCase kGoldenDeltaRejectCases[] = {
    {"reject/01-copy-past-end", "out of bounds"},
    {"reject/02-truncated-instruction",
     "truncated delta: insert runs past end of stream"},
    {"reject/03-truncated-varint-header",
     "truncated delta: varint runs past end of stream"},
    {"reject/04-reserved-opcode-zero", "reserved opcode 0x00"},
    {"reject/05-src-size-mismatch", "delta source size mismatch"},
    {"reject/06-tgt-size-mismatch", "delta target size mismatch"},
};

void TestGoldenDeltaVectorsRejectMalformed() {
  for (const GoldenDeltaRejectCase& c : kGoldenDeltaRejectCases) {
    const std::vector<uint8_t> base = ReadGoldenDeltaFile(c.name, "base");
    const std::vector<uint8_t> delta = ReadGoldenDeltaFile(c.name, "delta");

    std::vector<uint8_t> target;
    std::string error;
    if (snapvault::ApplyDelta(base, delta, 1 << 20, &target, &error)) {
      std::cerr << "FAIL: ApplyDelta(" << c.name << ".base, " << c.name
                << ".delta) unexpectedly succeeded\n";
      ++failures;
      continue;
    }
    if (error.find(c.want_substring) == std::string::npos) {
      std::cerr << "FAIL: ApplyDelta(" << c.name << ") error \"" << error
                << "\" does not contain \"" << c.want_substring << "\"\n";
      ++failures;
      continue;
    }
    EXPECT(target.empty());
  }
}

// ---------------------------------------------------------------------------
// object_store / fsck v2 container support. Fixtures are handcrafted here
// (zlib via the linked zlib, zstd via the linked libzstd) rather than
// produced by a CLI, since neither the Go nor Java writer speaks v2 yet.

namespace fs = std::filesystem;

// A counter, not a process id, so parallel test processes never collide
// while still giving every fixture directory a distinct name.
std::string NextTempSuffix() {
  static std::atomic<int> counter{0};
  return std::to_string(counter.fetch_add(1));
}

std::vector<uint8_t> CanonicalBytes(const std::string& type,
                                    const std::vector<uint8_t>& payload) {
  const std::string header = type + " " + std::to_string(payload.size());
  std::vector<uint8_t> canonical(header.begin(), header.end());
  canonical.push_back(0);
  canonical.insert(canonical.end(), payload.begin(), payload.end());
  return canonical;
}

std::string ObjectId(const std::vector<uint8_t>& canonical) {
  return snapvault::HexDigest(
      snapvault::Sha256Of(canonical.data(), canonical.size()));
}

std::vector<uint8_t> ZlibCompress(const std::vector<uint8_t>& data) {
  uLongf bound = compressBound(static_cast<uLong>(data.size()));
  std::vector<uint8_t> out(bound);
  const int rc = compress2(out.data(), &bound,
                           data.empty() ? reinterpret_cast<const Bytef*>("")
                                        : data.data(),
                           static_cast<uLong>(data.size()),
                           Z_DEFAULT_COMPRESSION);
  EXPECT(rc == Z_OK);
  out.resize(bound);
  return out;
}

std::vector<uint8_t> ZstdCompress(const std::vector<uint8_t>& data) {
  const size_t bound = ZSTD_compressBound(data.size());
  std::vector<uint8_t> out(bound);
  const size_t written = ZSTD_compress(
      out.data(), bound, data.empty() ? "" : reinterpret_cast<const char*>(
                                                  data.data()),
      data.size(), 1);
  EXPECT(!ZSTD_isError(written));
  out.resize(written);
  return out;
}

void WriteFile(const fs::path& path, const std::vector<uint8_t>& bytes) {
  fs::create_directories(path.parent_path());
  std::ofstream out(path, std::ios::binary);
  out.write(reinterpret_cast<const char*>(bytes.data()),
            static_cast<std::streamsize>(bytes.size()));
}

void WriteTextFile(const fs::path& path, const std::string& text) {
  fs::create_directories(path.parent_path());
  std::ofstream out(path, std::ios::binary);
  out << text;
}

fs::path ShardedPath(const fs::path& objects_dir, const std::string& id) {
  return objects_dir / id.substr(0, 2) / id.substr(2);
}

void WriteLegacyObject(const fs::path& objects_dir, const std::string& id,
                       const std::vector<uint8_t>& canonical) {
  WriteFile(ShardedPath(objects_dir, id), ZlibCompress(canonical));
}

void WriteContainerFull(const fs::path& objects_dir, const std::string& id,
                        const std::vector<uint8_t>& canonical,
                        uint8_t codec) {
  std::vector<uint8_t> body =
      codec == 0x01 ? ZlibCompress(canonical) : ZstdCompress(canonical);
  std::vector<uint8_t> file = {'S', 'V', 'O', '2', 0x01, codec};
  file.insert(file.end(), body.begin(), body.end());
  WriteFile(ShardedPath(objects_dir, id), file);
}

void WriteContainerDelta(const fs::path& objects_dir, const std::string& id,
                         const std::string& base_id,
                         const std::vector<uint8_t>& delta_instructions,
                         uint8_t codec) {
  std::vector<uint8_t> body = codec == 0x01
                                  ? ZlibCompress(delta_instructions)
                                  : ZstdCompress(delta_instructions);
  std::vector<uint8_t> file = {'S', 'V', 'O', '2', 0x02, codec};
  const std::vector<uint8_t> raw_base_id = FromHex(base_id);
  EXPECT(raw_base_id.size() == 32);
  file.insert(file.end(), raw_base_id.begin(), raw_base_id.end());
  file.insert(file.end(), body.begin(), body.end());
  WriteFile(ShardedPath(objects_dir, id), file);
}

void PutInt32Be(std::vector<uint8_t>* out, int32_t value) {
  const auto unsigned_value = static_cast<uint32_t>(value);
  out->push_back(static_cast<uint8_t>(unsigned_value >> 24));
  out->push_back(static_cast<uint8_t>(unsigned_value >> 16));
  out->push_back(static_cast<uint8_t>(unsigned_value >> 8));
  out->push_back(static_cast<uint8_t>(unsigned_value));
}

void PutInt64Be(std::vector<uint8_t>* out, int64_t value) {
  PutInt32Be(out, static_cast<int32_t>(static_cast<uint64_t>(value) >> 32));
  PutInt32Be(out, static_cast<int32_t>(value));
}

void PutObjectId(std::vector<uint8_t>* out, const std::string& id) {
  const std::vector<uint8_t> raw = FromHex(id);
  EXPECT(raw.size() == 32);
  out->insert(out->end(), raw.begin(), raw.end());
}

// A synthetic 64-hex-character id, used only to name links in a chain fixture
// whose objects never need to resolve to real content (e.g. a depth-cap
// probe, where recursion stops before the id is ever looked up).
std::string HexId(int value) {
  std::ostringstream hex;
  hex << std::hex << std::setfill('0') << std::setw(64) << value;
  return hex.str();
}

struct FixtureTreeEntry {
  std::string name;
  uint8_t kind;
  bool executable;
  std::string object_id;
};

std::vector<uint8_t> BuildTreePayload(
    const std::vector<FixtureTreeEntry>& entries) {
  std::vector<uint8_t> out;
  PutInt32Be(&out, static_cast<int32_t>(0x53565431));
  PutInt32Be(&out, static_cast<int32_t>(entries.size()));
  for (const FixtureTreeEntry& entry : entries) {
    PutInt32Be(&out, static_cast<int32_t>(entry.name.size()));
    out.insert(out.end(), entry.name.begin(), entry.name.end());
    out.push_back(entry.kind);
    out.push_back(entry.executable ? 1 : 0);
    PutObjectId(&out, entry.object_id);
  }
  return out;
}

std::vector<uint8_t> BuildCommitPayload(const std::string& tree_id,
                                        const std::string& message) {
  std::vector<uint8_t> out;
  PutInt32Be(&out, static_cast<int32_t>(0x53564331));
  PutObjectId(&out, tree_id);
  PutInt32Be(&out, 0);  // no parents
  PutInt64Be(&out, 1700000000);
  PutInt32Be(&out, 0);
  PutInt32Be(&out, static_cast<int32_t>(message.size()));
  out.insert(out.end(), message.begin(), message.end());
  return out;
}

// Lays out a repository skeleton (format, HEAD, empty refs dir) under root
// and returns its objects directory.
fs::path InitFixtureRepo(const fs::path& root, const std::string& format) {
  const fs::path metadata = root / ".snapvault";
  WriteTextFile(metadata / "format", format + "\n");
  WriteTextFile(metadata / "HEAD", "ref: refs/heads/main\n");
  fs::create_directories(metadata / "refs" / "heads");
  fs::create_directories(metadata / "objects");
  return metadata / "objects";
}

void WriteMainRef(const fs::path& root, const std::string& commit_id) {
  WriteTextFile(root / ".snapvault" / "refs" / "heads" / "main",
               commit_id + "\n");
}

// Writes a one-file tree plus a commit pointing at it, and updates
// refs/heads/main to the new commit. Both tree and commit are stored as
// ordinary legacy objects, matching what every writer produces today.
void WriteFixtureSnapshot(const fs::path& root, const fs::path& objects_dir,
                          const std::string& file_name,
                          const std::string& blob_id) {
  const std::vector<uint8_t> tree_payload = BuildTreePayload(
      {{file_name, snapvault::kKindFile, false, blob_id}});
  const std::vector<uint8_t> tree_canonical =
      CanonicalBytes("tree", tree_payload);
  const std::string tree_id = ObjectId(tree_canonical);
  WriteLegacyObject(objects_dir, tree_id, tree_canonical);

  const std::vector<uint8_t> commit_payload =
      BuildCommitPayload(tree_id, "fixture snapshot");
  const std::vector<uint8_t> commit_canonical =
      CanonicalBytes("commit", commit_payload);
  const std::string commit_id = ObjectId(commit_canonical);
  WriteLegacyObject(objects_dir, commit_id, commit_canonical);

  WriteMainRef(root, commit_id);
}

// The docs/FORMAT.md worked example, as canonical bytes and the exact
// 17-byte delta.
std::vector<uint8_t> WorkedExampleBaseCanonical() {
  return CanonicalBytes(
      "blob", StringBytes(std::string("hello world\n", 12)));
}

std::vector<uint8_t> WorkedExampleTargetCanonical() {
  return CanonicalBytes(
      "blob", StringBytes(std::string("hello worlds\n", 13)));
}

std::vector<uint8_t> WorkedExampleDeltaInstructions() {
  return Bytes({0x14, 0x15, 0x08, 0x62, 0x6c, 0x6f, 0x62, 0x20, 0x31, 0x33,
                0x00, 0x91, 0x08, 0x0b, 0x02, 0x73, 0x0a});
}

void TestReadObjectLegacyUnchanged() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-legacy-" + NextTempSuffix());
  fs::remove_all(root);
  const std::vector<uint8_t> canonical =
      CanonicalBytes("blob", StringBytes("hello\n"));
  const std::string id = ObjectId(canonical);
  WriteLegacyObject(root, id, canonical);

  std::vector<uint8_t> payload;
  snapvault::ObjectInfo info;
  std::string error;
  EXPECT(snapvault::ReadObject(root, id, &payload, &info, &error));
  EXPECT(error.empty());
  EXPECT(info.type == "blob");
  EXPECT(info.form == snapvault::ObjectForm::kLegacy);
  EXPECT(info.delta_depth == 0);
  EXPECT(payload == StringBytes("hello\n"));
  fs::remove_all(root);
}

void TestReadObjectContainerFull(uint8_t codec) {
  const fs::path root =
      fs::temp_directory_path() /
      ("sv-full-" + std::to_string(codec) + "-" + NextTempSuffix());
  fs::remove_all(root);
  const std::vector<uint8_t> canonical =
      CanonicalBytes("blob", StringBytes("container full payload\n"));
  const std::string id = ObjectId(canonical);
  WriteContainerFull(root, id, canonical, codec);

  std::vector<uint8_t> payload;
  snapvault::ObjectInfo info;
  std::string error;
  EXPECT(snapvault::ReadObject(root, id, &payload, &info, &error));
  EXPECT(error.empty());
  EXPECT(info.type == "blob");
  EXPECT(info.form == snapvault::ObjectForm::kContainerFull);
  EXPECT(info.delta_depth == 0);
  EXPECT(payload == StringBytes("container full payload\n"));
  fs::remove_all(root);
}

void TestReadObjectContainerFullZlib() { TestReadObjectContainerFull(0x01); }
void TestReadObjectContainerFullZstd() { TestReadObjectContainerFull(0x02); }

void TestReadObjectContainerDelta(uint8_t codec) {
  const fs::path root =
      fs::temp_directory_path() /
      ("sv-delta-" + std::to_string(codec) + "-" +
       NextTempSuffix());
  fs::remove_all(root);
  const std::vector<uint8_t> base_canonical = WorkedExampleBaseCanonical();
  const std::string base_id = ObjectId(base_canonical);
  WriteContainerFull(root, base_id, base_canonical, codec);

  const std::vector<uint8_t> target_canonical =
      WorkedExampleTargetCanonical();
  const std::string target_id = ObjectId(target_canonical);
  WriteContainerDelta(root, target_id, base_id,
                      WorkedExampleDeltaInstructions(), codec);

  std::vector<uint8_t> payload;
  snapvault::ObjectInfo info;
  std::string error;
  EXPECT(snapvault::ReadObject(root, target_id, &payload, &info, &error));
  EXPECT(error.empty());
  EXPECT(info.type == "blob");
  EXPECT(info.form == snapvault::ObjectForm::kContainerDelta);
  EXPECT(info.delta_depth == 1);
  EXPECT(payload == StringBytes(std::string("hello worlds\n", 13)));
  fs::remove_all(root);
}

void TestReadObjectContainerDeltaZlib() {
  TestReadObjectContainerDelta(0x01);
}
void TestReadObjectContainerDeltaZstd() {
  TestReadObjectContainerDelta(0x02);
}

void TestReadObjectRejectsUnknownMagic() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-badmagic-" + NextTempSuffix());
  fs::remove_all(root);
  const std::string id(64, '0');
  WriteFile(ShardedPath(root, id), Bytes({'X', 'X', 'X', 'X', 0x01, 0x01}));
  std::vector<uint8_t> payload;
  snapvault::ObjectInfo info;
  std::string error;
  EXPECT(!snapvault::ReadObject(root, id, &payload, &info, &error));
  EXPECT(!error.empty());
  fs::remove_all(root);
}

void TestReadObjectRejectsUnknownKindAndCodec() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-badkind-" + NextTempSuffix());
  fs::remove_all(root);
  const std::string bad_kind_id(64, '1');
  WriteFile(ShardedPath(root, bad_kind_id),
           Bytes({'S', 'V', 'O', '2', 0x03, 0x01}));
  std::vector<uint8_t> payload;
  snapvault::ObjectInfo info;
  std::string error;
  EXPECT(!snapvault::ReadObject(root, bad_kind_id, &payload, &info, &error));

  const std::string bad_codec_id(64, '2');
  WriteFile(ShardedPath(root, bad_codec_id),
           Bytes({'S', 'V', 'O', '2', 0x01, 0x09}));
  EXPECT(
      !snapvault::ReadObject(root, bad_codec_id, &payload, &info, &error));
  fs::remove_all(root);
}

// FORMAT.md requires a codec-zstd stream to be exactly one standard zstd
// frame with no skippable frames. id is computed over the *single-frame*
// canonical bytes, so a rejection here can only come from framing
// enforcement -- a second frame or trailing bytes would also fail on a
// simple digest mismatch, which would mask whether framing itself is
// actually checked.
void TestReadObjectRejectsMultiFrameZstd(
    const std::vector<uint8_t>& body_suffix, const std::string& name) {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-zstdframing-" + name + "-" + NextTempSuffix());
  fs::remove_all(root);
  const std::vector<uint8_t> canonical =
      CanonicalBytes("blob", StringBytes("hello frames"));
  const std::string id = ObjectId(canonical);
  const std::vector<uint8_t> frame = ZstdCompress(canonical);

  std::vector<uint8_t> file = {'S', 'V', 'O', '2', 0x01, 0x02};
  Append(&file, frame);
  Append(&file, body_suffix);
  WriteFile(ShardedPath(root, id), file);

  std::vector<uint8_t> payload;
  snapvault::ObjectInfo info;
  std::string error;
  EXPECT(!snapvault::ReadObject(root, id, &payload, &info, &error));
  fs::remove_all(root);
}

void TestReadObjectRejectsMultiFrameZstdTwoFrames() {
  const std::vector<uint8_t> canonical =
      CanonicalBytes("blob", StringBytes("hello frames"));
  TestReadObjectRejectsMultiFrameZstd(ZstdCompress(canonical), "twoframes");
}

void TestReadObjectRejectsMultiFrameZstdTrailingGarbage() {
  TestReadObjectRejectsMultiFrameZstd(Bytes({0x01, 0x02, 0x03}), "trailing");
}

void TestReadObjectRejectsSkippableZstdFramePrefix() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-zstdskippable-" + NextTempSuffix());
  fs::remove_all(root);
  const std::vector<uint8_t> canonical =
      CanonicalBytes("blob", StringBytes("hello frames"));
  const std::string id = ObjectId(canonical);

  std::vector<uint8_t> file = {'S', 'V', 'O', '2', 0x01, 0x02};
  // Skippable frame magic 0x184D2A50 (little-endian), 4-byte length 4, and
  // 4 bytes of arbitrary skip payload, followed by the real frame.
  Append(&file, Bytes({0x50, 0x2a, 0x4d, 0x18, 0x04, 0x00, 0x00, 0x00, 0xde,
                       0xad, 0xbe, 0xef}));
  Append(&file, ZstdCompress(canonical));
  WriteFile(ShardedPath(root, id), file);

  std::vector<uint8_t> payload;
  snapvault::ObjectInfo info;
  std::string error;
  EXPECT(!snapvault::ReadObject(root, id, &payload, &info, &error));
  fs::remove_all(root);
}

void TestReadObjectRejectsDigestMismatch() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-baddigest-" + NextTempSuffix());
  fs::remove_all(root);
  const std::vector<uint8_t> canonical =
      CanonicalBytes("blob", StringBytes("real content\n"));
  const std::string real_id = ObjectId(canonical);
  // Store it under a different, unrelated id so the digest check fails.
  const std::string wrong_id(64, '3');
  EXPECT(wrong_id != real_id);
  WriteContainerFull(root, wrong_id, canonical, 0x01);

  std::vector<uint8_t> payload;
  snapvault::ObjectInfo info;
  std::string error;
  EXPECT(!snapvault::ReadObject(root, wrong_id, &payload, &info, &error));
  EXPECT(error.find("integrity") != std::string::npos);
  fs::remove_all(root);
}

void TestReadObjectRejectsMissingDeltaBase() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-missingbase-" + NextTempSuffix());
  fs::remove_all(root);
  const std::string missing_base_id(64, '4');
  const std::vector<uint8_t> target_canonical =
      WorkedExampleTargetCanonical();
  const std::string target_id = ObjectId(target_canonical);
  WriteContainerDelta(root, target_id, missing_base_id,
                      WorkedExampleDeltaInstructions(), 0x01);

  std::vector<uint8_t> payload;
  snapvault::ObjectInfo info;
  std::string error;
  EXPECT(!snapvault::ReadObject(root, target_id, &payload, &info, &error));
  EXPECT(error.find("does not exist") != std::string::npos);
  fs::remove_all(root);
}

void TestReadObjectRejectsDeltaCycle() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-cycle-" + NextTempSuffix());
  fs::remove_all(root);
  const std::string id_a(64, '5');
  const std::string id_b(64, '6');
  // A's base is B and B's base is A: neither can ever bottom out at a full
  // object.
  WriteContainerDelta(root, id_a, id_b, Bytes({0x00, 0x00}), 0x01);
  WriteContainerDelta(root, id_b, id_a, Bytes({0x00, 0x00}), 0x01);

  std::vector<uint8_t> payload;
  snapvault::ObjectInfo info;
  std::string error;
  EXPECT(!snapvault::ReadObject(root, id_a, &payload, &info, &error));
  EXPECT(error.find("cycle") != std::string::npos);
  fs::remove_all(root);
}

void TestReadObjectRejectsDeltaChainTooDeep() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-deepchain-" + NextTempSuffix());
  fs::remove_all(root);
  // A chain of 33 deltas (depths 0..32 all present, the 33rd cannot be
  // resolved) exceeds the depth-32 cap without needing any of them to be a
  // valid, appliable diff: the depth check happens before the base is ever
  // opened.
  std::vector<std::string> ids;
  for (int i = 0; i < 34; ++i) {
    ids.push_back(HexId(i));
  }
  for (int i = 0; i < 33; ++i) {
    WriteContainerDelta(root, ids[i], ids[i + 1], Bytes({0x00, 0x00}), 0x01);
  }

  std::vector<uint8_t> payload;
  snapvault::ObjectInfo info;
  std::string error;
  EXPECT(!snapvault::ReadObject(root, ids[0], &payload, &info, &error));
  EXPECT(error.find("depth") != std::string::npos);
  fs::remove_all(root);
}

void TestFsckAcceptsFormatOneAndTwo() {
  for (const std::string& format : {"snapvault 1", "snapvault 2"}) {
    const fs::path root =
        fs::temp_directory_path() / ("sv-fmt-" + NextTempSuffix());
    fs::remove_all(root);
    InitFixtureRepo(root, format);
    std::ostringstream out;
    const int status = snapvault::RunFsck(root, out);
    EXPECT(status == 0);
    EXPECT(out.str().find("0 errors") != std::string::npos);
    fs::remove_all(root);
  }
}

void TestFsckRejectsUnsupportedFormat() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-fmt3-" + NextTempSuffix());
  fs::remove_all(root);
  InitFixtureRepo(root, "snapvault 3");
  std::ostringstream out;
  const int status = snapvault::RunFsck(root, out);
  EXPECT(status == 1);
  EXPECT(out.str().find("unsupported repository format") !=
         std::string::npos);
  fs::remove_all(root);
}

void TestFsckAcceptsMixedLegacyAndContainerInV2Repo() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-mixed-" + NextTempSuffix());
  fs::remove_all(root);
  const fs::path objects_dir = InitFixtureRepo(root, "snapvault 2");

  const std::vector<uint8_t> base_canonical = WorkedExampleBaseCanonical();
  const std::string base_id = ObjectId(base_canonical);
  WriteContainerFull(objects_dir, base_id, base_canonical, 0x02);  // zstd

  const std::vector<uint8_t> target_canonical =
      WorkedExampleTargetCanonical();
  const std::string target_id = ObjectId(target_canonical);
  WriteContainerDelta(objects_dir, target_id, base_id,
                      WorkedExampleDeltaInstructions(), 0x01);  // zlib

  WriteFixtureSnapshot(root, objects_dir, "doc.txt", target_id);

  std::ostringstream out;
  const int status = snapvault::RunFsck(root, out);
  EXPECT(status == 0);
  EXPECT(out.str().find("0 errors") != std::string::npos);
  fs::remove_all(root);
}

void TestFsckFlagsContainerObjectInV1Repo() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-v1container-" + NextTempSuffix());
  fs::remove_all(root);
  const fs::path objects_dir = InitFixtureRepo(root, "snapvault 1");

  const std::vector<uint8_t> blob_canonical =
      CanonicalBytes("blob", StringBytes("v2 object in a v1 repo\n"));
  const std::string blob_id = ObjectId(blob_canonical);
  WriteContainerFull(objects_dir, blob_id, blob_canonical, 0x01);

  WriteFixtureSnapshot(root, objects_dir, "file.txt", blob_id);

  std::ostringstream out;
  const int status = snapvault::RunFsck(root, out);
  EXPECT(status == 1);
  EXPECT(out.str().find("format 1") != std::string::npos);
  fs::remove_all(root);
}

void TestFsckFlagsMissingDeltaBase() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-fsckmissingbase-" + NextTempSuffix());
  fs::remove_all(root);
  const fs::path objects_dir = InitFixtureRepo(root, "snapvault 2");

  const std::string missing_base_id(64, '7');
  const std::vector<uint8_t> target_canonical =
      WorkedExampleTargetCanonical();
  const std::string target_id = ObjectId(target_canonical);
  WriteContainerDelta(objects_dir, target_id, missing_base_id,
                      WorkedExampleDeltaInstructions(), 0x01);

  WriteFixtureSnapshot(root, objects_dir, "file.txt", target_id);

  std::ostringstream out;
  const int status = snapvault::RunFsck(root, out);
  EXPECT(status == 1);
  EXPECT(out.str().find("does not exist") != std::string::npos);
  fs::remove_all(root);
}

void TestFsckFlagsDeltaChainDepthExceeded() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-fsckdeep-" + NextTempSuffix());
  fs::remove_all(root);
  const fs::path objects_dir = InitFixtureRepo(root, "snapvault 2");

  std::vector<std::string> ids;
  for (int i = 0; i < 34; ++i) {
    ids.push_back(HexId(i));
  }
  for (int i = 0; i < 33; ++i) {
    WriteContainerDelta(objects_dir, ids[i], ids[i + 1], Bytes({0x00, 0x00}),
                        0x01);
  }

  WriteFixtureSnapshot(root, objects_dir, "file.txt", ids[0]);

  std::ostringstream out;
  const int status = snapvault::RunFsck(root, out);
  EXPECT(status == 1);
  EXPECT(out.str().find("depth") != std::string::npos);
  fs::remove_all(root);
}

// A tree graph nested well past fsck's kMaxTreeDepth (1000): kDepth real
// tree objects, each with a single "child" directory entry pointing at the
// next, with the deepest one pointing at an id that is never resolved
// because the depth cap trips first. Before fsck bounded tree recursion,
// walking a tree graph this deep risked exhausting the C call stack instead
// of reporting an error; this asserts it reports the error cleanly.
void TestFsckFlagsExcessiveTreeDepth() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-fscktreedeep-" + NextTempSuffix());
  fs::remove_all(root);
  const fs::path objects_dir = InitFixtureRepo(root, "snapvault 1");

  constexpr int kDepth = 1002;  // fsck's cap (1000) plus a margin of 2.
  std::string child_id = HexId(0);  // never resolved; the cap trips first.
  std::string tree_id;
  for (int i = 0; i < kDepth; ++i) {
    const std::vector<uint8_t> payload = BuildTreePayload(
        {{"child", snapvault::kKindDirectory, false, child_id}});
    const std::vector<uint8_t> canonical = CanonicalBytes("tree", payload);
    tree_id = ObjectId(canonical);
    WriteLegacyObject(objects_dir, tree_id, canonical);
    child_id = tree_id;
  }

  const std::vector<uint8_t> commit_payload =
      BuildCommitPayload(tree_id, "pathologically deep tree fixture");
  const std::vector<uint8_t> commit_canonical =
      CanonicalBytes("commit", commit_payload);
  const std::string commit_id = ObjectId(commit_canonical);
  WriteLegacyObject(objects_dir, commit_id, commit_canonical);
  WriteMainRef(root, commit_id);

  std::ostringstream out;
  const int status = snapvault::RunFsck(root, out);
  EXPECT(status == 1);
  EXPECT(out.str().find("depth") != std::string::npos);
  fs::remove_all(root);
}

// A legacy object's header is accepted verbatim (non-canonical decimal sizes
// included, e.g. a leading zero) by the envelope parser; ReadObject must
// apply a delta against those exact raw header bytes, not a re-rendering
// built from the parsed type and integer size, since the id was computed
// over -- and the digest already verified -- the raw bytes.
void TestReadObjectDeltaAgainstLegacyBaseUsesRawHeaderBytes() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-legacybase-" + NextTempSuffix());
  fs::remove_all(root);

  // "blob 05\0hello": 13 raw bytes, with a non-canonical leading zero the
  // decimal-size envelope parser accepts anyway.
  const std::vector<uint8_t> base_canonical =
      Bytes({'b', 'l', 'o', 'b', ' ', '0', '5', 0x00, 'h', 'e', 'l', 'l', 'o'});
  EXPECT(base_canonical.size() == 13);
  const std::string base_id = ObjectId(base_canonical);
  WriteLegacyObject(root, base_id, base_canonical);

  // "blob 6\0hello!": also 13 bytes, so a delta built against the raw
  // 13-byte source reconstructs it with one insert-header, one copy-payload,
  // and one insert-suffix instruction.
  const std::vector<uint8_t> target_canonical =
      CanonicalBytes("blob", StringBytes("hello!"));
  EXPECT(target_canonical.size() == 13);
  const std::string target_id = ObjectId(target_canonical);

  std::vector<uint8_t> instructions;
  Append(&instructions, EncodeVarint(13));  // srcSize
  Append(&instructions, EncodeVarint(13));  // tgtSize
  Append(&instructions,
        Bytes({0x07, 'b', 'l', 'o', 'b', ' ', '6', 0x00}));  // insert "blob 6\0"
  Append(&instructions, Bytes({0x91, 0x08, 0x05}));          // copy(offset=8, size=5) "hello"
  Append(&instructions, Bytes({0x01, '!'}));                 // insert "!"
  WriteContainerDelta(root, target_id, base_id, instructions, 0x01);

  std::vector<uint8_t> payload;
  snapvault::ObjectInfo info;
  std::string error;
  EXPECT(snapvault::ReadObject(root, target_id, &payload, &info, &error));
  EXPECT(error.empty());
  EXPECT(payload == StringBytes("hello!"));
  fs::remove_all(root);
}

void TestFsckFlagsDigestMismatch() {
  const fs::path root = fs::temp_directory_path() /
                        ("sv-fsckbaddigest-" + NextTempSuffix());
  fs::remove_all(root);
  const fs::path objects_dir = InitFixtureRepo(root, "snapvault 2");

  const std::vector<uint8_t> canonical =
      CanonicalBytes("blob", StringBytes("digest mismatch fixture\n"));
  const std::string real_id = ObjectId(canonical);
  std::vector<uint8_t> corrupted = canonical;
  corrupted.back() ^= 0xff;  // still a valid container, wrong content
  WriteContainerFull(objects_dir, real_id, corrupted, 0x01);

  WriteFixtureSnapshot(root, objects_dir, "file.txt", real_id);

  std::ostringstream out;
  const int status = snapvault::RunFsck(root, out);
  EXPECT(status == 1);
  EXPECT(out.str().find("integrity") != std::string::npos);
  fs::remove_all(root);
}

}  // namespace

int main() {
  TestSha256Vectors();
  TestSha256IncrementalMatchesOneShot();
  TestCompareUtf16MatchesJavaStringOrder();
  TestParseGoldenTree();
  TestParseTreeRejectsSpecViolations();
  TestParseGoldenCommit();
  TestParseCommitRejectsCorruptPayloads();
  TestIsValidObjectId();
  TestApplyDeltaWorkedExample();
  TestApplyDeltaMultiByteVarintAndCopy();
  TestApplyDeltaSizeZeroMeans65536();
  TestApplyDeltaRejectsSrcSizeMismatch();
  TestApplyDeltaRejectsTgtSizeMismatch();
  TestApplyDeltaRejectsOutOfBoundsCopy();
  TestApplyDeltaRejectsTruncatedVarint();
  TestApplyDeltaRejectsTruncatedLiteral();
  TestApplyDeltaRejectsTruncatedCopyFields();
  TestApplyDeltaRejectsOpcodeZero();
  TestApplyDeltaEnforcesOutputCap();
  TestApplyDeltaClearsPartialOutputOnMidStreamFailure();
  TestGoldenDeltaVectorsApplyToTarget();
  TestGoldenDeltaVectorsRejectMalformed();
  TestReadObjectLegacyUnchanged();
  TestReadObjectContainerFullZlib();
  TestReadObjectContainerFullZstd();
  TestReadObjectContainerDeltaZlib();
  TestReadObjectContainerDeltaZstd();
  TestReadObjectRejectsUnknownMagic();
  TestReadObjectRejectsUnknownKindAndCodec();
  TestReadObjectRejectsDigestMismatch();
  TestReadObjectRejectsMultiFrameZstdTwoFrames();
  TestReadObjectRejectsMultiFrameZstdTrailingGarbage();
  TestReadObjectRejectsSkippableZstdFramePrefix();
  TestReadObjectRejectsMissingDeltaBase();
  TestReadObjectRejectsDeltaCycle();
  TestReadObjectRejectsDeltaChainTooDeep();
  TestReadObjectDeltaAgainstLegacyBaseUsesRawHeaderBytes();
  TestFsckAcceptsFormatOneAndTwo();
  TestFsckRejectsUnsupportedFormat();
  TestFsckAcceptsMixedLegacyAndContainerInV2Repo();
  TestFsckFlagsContainerObjectInV1Repo();
  TestFsckFlagsMissingDeltaBase();
  TestFsckFlagsDeltaChainDepthExceeded();
  TestFsckFlagsExcessiveTreeDepth();
  TestFsckFlagsDigestMismatch();
  if (failures != 0) {
    std::cerr << failures << " test expectation(s) failed\n";
    return EXIT_FAILURE;
  }
  std::cout << "all unit tests passed\n";
  return EXIT_SUCCESS;
}
