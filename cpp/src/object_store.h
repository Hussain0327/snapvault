#ifndef SNAPVAULT_CPP_SRC_OBJECT_STORE_H_
#define SNAPVAULT_CPP_SRC_OBJECT_STORE_H_

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

namespace snapvault {

struct ObjectInfo {
  std::string type;  // "blob", "tree", or "commit"
  uint64_t payload_size = 0;
};

// Maximum declared size of an object captured whole into memory. Blobs are
// streamed and not subject to it.
constexpr uint64_t kMaxInlinePayload = 256ull << 20;

// Reads and fully verifies one stored object: inflates the zlib stream,
// parses the "<type> <size>\0" envelope, checks the declared length against
// the actual payload, rejects trailing data, and recomputes the SHA-256 of
// the canonical bytes against the id. When payload is non-null the payload
// bytes are captured (subject to kMaxInlinePayload); otherwise they are
// discarded after hashing, so blob verification is not bounded by memory.
// Returns false with *error set on any failure.
bool ReadObject(const std::filesystem::path& objects_dir,
                const std::string& id, std::vector<uint8_t>* payload,
                ObjectInfo* info, std::string* error);

}  // namespace snapvault

#endif  // SNAPVAULT_CPP_SRC_OBJECT_STORE_H_
