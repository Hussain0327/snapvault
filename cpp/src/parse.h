#ifndef SNAPVAULT_CPP_SRC_PARSE_H_
#define SNAPVAULT_CPP_SRC_PARSE_H_

#include <cstdint>
#include <string>
#include <vector>

namespace snapvault {

// The tree entry kinds defined by format v1.
constexpr uint8_t kKindFile = 1;
constexpr uint8_t kKindDirectory = 2;
constexpr uint8_t kKindSymlink = 3;

struct TreeEntry {
  std::string name;
  uint8_t kind = 0;
  bool executable = false;
  std::string object_id;
};

struct Commit {
  std::string tree_id;
  std::vector<std::string> parents;
  int64_t seconds = 0;
  int32_t nanos = 0;
  std::string message;
};

// Parses a tree payload, enforcing the format-v1 invariants: magic, sane
// entry count, safe UTF-8 names in strictly increasing UTF-16 order, known
// kinds, executable only on regular files with the flag byte 0 or 1, and no
// trailing data. Returns false with *error set on any violation.
bool ParseTree(const std::vector<uint8_t>& payload,
               std::vector<TreeEntry>* entries, std::string* error);

// Parses a commit payload, enforcing magic, parent count, nanosecond range,
// UTF-8 message without NUL, and no trailing data.
bool ParseCommit(const std::vector<uint8_t>& payload, Commit* commit,
                 std::string* error);

}  // namespace snapvault

#endif  // SNAPVAULT_CPP_SRC_PARSE_H_
