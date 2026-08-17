// Unit tests for snapvault-fsck, using golden vectors produced by the Java
// reference implementation so all three languages agree byte for byte.

#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <string>
#include <vector>

#include "src/bytes.h"
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
  if (failures != 0) {
    std::cerr << failures << " test expectation(s) failed\n";
    return EXIT_FAILURE;
  }
  std::cout << "all unit tests passed\n";
  return EXIT_SUCCESS;
}
