#include "src/parse.h"

#include <cstring>

#include "src/bytes.h"

namespace snapvault {
namespace {

constexpr uint32_t kTreeMagic = 0x53565431;    // "SVT1"
constexpr uint32_t kCommitMagic = 0x53564331;  // "SVC1"
constexpr int32_t kMaxTreeEntries = 1000000;
constexpr int32_t kMaxNameBytes = 1 << 20;
constexpr int32_t kMaxParents = 64;
constexpr int32_t kMaxMessageBytes = 4 << 20;

// Reads big-endian primitives from a payload, tracking exhaustion.
class Reader {
 public:
  explicit Reader(const std::vector<uint8_t>& payload) : payload_(payload) {}

  bool ReadInt32(int32_t* value) {
    if (payload_.size() - pos_ < 4) {
      return false;
    }
    *value = static_cast<int32_t>(
        static_cast<uint32_t>(payload_[pos_]) << 24 |
        static_cast<uint32_t>(payload_[pos_ + 1]) << 16 |
        static_cast<uint32_t>(payload_[pos_ + 2]) << 8 |
        static_cast<uint32_t>(payload_[pos_ + 3]));
    pos_ += 4;
    return true;
  }

  bool ReadInt64(int64_t* value) {
    int32_t high = 0;
    int32_t low = 0;
    if (!ReadInt32(&high) || !ReadInt32(&low)) {
      return false;
    }
    *value = static_cast<int64_t>(high) << 32 |
             static_cast<int64_t>(static_cast<uint32_t>(low));
    return true;
  }

  bool ReadByte(uint8_t* value) {
    if (pos_ >= payload_.size()) {
      return false;
    }
    *value = payload_[pos_++];
    return true;
  }

  bool ReadObjectId(std::string* id) {
    if (payload_.size() - pos_ < 32) {
      return false;
    }
    *id = ToHex(payload_.data() + pos_, 32);
    pos_ += 32;
    return true;
  }

  bool ReadBytes(int32_t length, std::string* value) {
    if (length < 0 ||
        payload_.size() - pos_ < static_cast<size_t>(length)) {
      return false;
    }
    value->assign(reinterpret_cast<const char*>(payload_.data() + pos_),
                  static_cast<size_t>(length));
    pos_ += static_cast<size_t>(length);
    return true;
  }

  size_t Remaining() const { return payload_.size() - pos_; }

 private:
  const std::vector<uint8_t>& payload_;
  size_t pos_ = 0;
};

bool Fail(std::string* error, const std::string& message) {
  *error = message;
  return false;
}

bool IsSafeName(const std::string& name) {
  if (name.empty() || name == "." || name == "..") {
    return false;
  }
  if (name.find_first_of("/\\") != std::string::npos ||
      name.find('\0') != std::string::npos) {
    return false;
  }
  return IsValidUtf8(reinterpret_cast<const uint8_t*>(name.data()),
                     name.size());
}

}  // namespace

bool ParseTree(const std::vector<uint8_t>& payload,
               std::vector<TreeEntry>* entries, std::string* error) {
  entries->clear();
  error->clear();
  Reader reader(payload);
  int32_t magic = 0;
  if (!reader.ReadInt32(&magic)) {
    return Fail(error, "truncated tree object");
  }
  if (static_cast<uint32_t>(magic) != kTreeMagic) {
    return Fail(error, "invalid tree object signature");
  }
  int32_t count = 0;
  if (!reader.ReadInt32(&count)) {
    return Fail(error, "truncated tree object");
  }
  if (count < 0 || count > kMaxTreeEntries) {
    return Fail(error,
                "invalid tree entry count: " + std::to_string(count));
  }

  for (int32_t i = 0; i < count; ++i) {
    TreeEntry entry;
    int32_t name_length = 0;
    if (!reader.ReadInt32(&name_length)) {
      return Fail(error, "truncated tree object");
    }
    if (name_length < 0 || name_length > kMaxNameBytes) {
      return Fail(error,
                  "invalid string length: " + std::to_string(name_length));
    }
    if (!reader.ReadBytes(name_length, &entry.name)) {
      return Fail(error, "truncated tree object");
    }
    if (!IsSafeName(entry.name)) {
      return Fail(error, "unsafe tree entry name: " + entry.name);
    }
    uint8_t executable = 0;
    if (!reader.ReadByte(&entry.kind) || !reader.ReadByte(&executable)) {
      return Fail(error, "truncated tree object");
    }
    if (entry.kind != kKindFile && entry.kind != kKindDirectory &&
        entry.kind != kKindSymlink) {
      return Fail(error, "unknown tree entry kind: " +
                             std::to_string(entry.kind));
    }
    if (executable > 1) {
      return Fail(error, "executable flag must be 0 or 1 for " + entry.name);
    }
    entry.executable = executable == 1;
    if (entry.executable && entry.kind != kKindFile) {
      return Fail(error, "only regular files can be executable: " +
                             entry.name);
    }
    if (!reader.ReadObjectId(&entry.object_id)) {
      return Fail(error, "truncated tree object");
    }
    if (!entries->empty() &&
        CompareUtf16(entries->back().name, entry.name) >= 0) {
      return Fail(error, "tree entries are not sorted and unique: \"" +
                             entries->back().name + "\" then \"" +
                             entry.name + "\"");
    }
    entries->push_back(std::move(entry));
  }
  if (reader.Remaining() != 0) {
    return Fail(error, "tree object has trailing data");
  }
  return true;
}

bool ParseCommit(const std::vector<uint8_t>& payload, Commit* commit,
                 std::string* error) {
  *commit = Commit();
  error->clear();
  Reader reader(payload);
  int32_t magic = 0;
  if (!reader.ReadInt32(&magic)) {
    return Fail(error, "truncated commit object");
  }
  if (static_cast<uint32_t>(magic) != kCommitMagic) {
    return Fail(error, "invalid commit object signature");
  }
  if (!reader.ReadObjectId(&commit->tree_id)) {
    return Fail(error, "truncated commit object");
  }
  int32_t parent_count = 0;
  if (!reader.ReadInt32(&parent_count)) {
    return Fail(error, "truncated commit object");
  }
  if (parent_count < 0 || parent_count > kMaxParents) {
    return Fail(error, "invalid commit parent count: " +
                           std::to_string(parent_count));
  }
  for (int32_t i = 0; i < parent_count; ++i) {
    std::string parent;
    if (!reader.ReadObjectId(&parent)) {
      return Fail(error, "truncated commit object");
    }
    commit->parents.push_back(std::move(parent));
  }
  if (!reader.ReadInt64(&commit->seconds)) {
    return Fail(error, "truncated commit object");
  }
  if (!reader.ReadInt32(&commit->nanos)) {
    return Fail(error, "truncated commit object");
  }
  if (commit->nanos < 0 || commit->nanos > 999999999) {
    return Fail(error, "invalid commit nanosecond: " +
                           std::to_string(commit->nanos));
  }
  int32_t message_length = 0;
  if (!reader.ReadInt32(&message_length)) {
    return Fail(error, "truncated commit object");
  }
  if (message_length < 0 || message_length > kMaxMessageBytes) {
    return Fail(error, "invalid string length: " +
                           std::to_string(message_length));
  }
  if (!reader.ReadBytes(message_length, &commit->message)) {
    return Fail(error, "truncated commit object");
  }
  if (commit->message.find('\0') != std::string::npos ||
      !IsValidUtf8(reinterpret_cast<const uint8_t*>(commit->message.data()),
                   commit->message.size())) {
    return Fail(error, "commit message is not valid NUL-free UTF-8");
  }
  if (reader.Remaining() != 0) {
    return Fail(error, "commit object has trailing data");
  }
  return true;
}

}  // namespace snapvault
