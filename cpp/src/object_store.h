#ifndef SNAPVAULT_CPP_SRC_OBJECT_STORE_H_
#define SNAPVAULT_CPP_SRC_OBJECT_STORE_H_

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

namespace snapvault {

// How an object file on disk was stored. Legacy is the v1 form (a bare zlib
// stream); the other two are the v2 "SVO2" container (see docs/FORMAT.md).
enum class ObjectForm {
  kLegacy,
  kContainerFull,
  kContainerDelta,
};

struct ObjectInfo {
  std::string type;  // "blob", "tree", or "commit"
  uint64_t payload_size = 0;
  ObjectForm form = ObjectForm::kLegacy;
  // 0 for legacy and container/full objects; for container/delta, one more
  // than the depth of the base it was reconstructed against.
  int delta_depth = 0;
};

// Maximum declared size of an object captured whole into memory. Blobs read
// through the legacy path are streamed and not subject to it. Container-form
// objects (v2) are always materialized in memory to be verified or, for
// deltas, applied, so this cap always applies to them: it bounds the
// reconstructed canonical bytes and, separately, the decompressed delta
// instruction stream, matching docs/FORMAT.md.
constexpr uint64_t kMaxInlinePayload = 256ull << 20;

// The deepest a delta chain may go before a container/delta object is
// resolved; a full object has depth 0.
constexpr int kMaxDeltaChainDepth = 32;

// Reads and fully verifies one stored object, whichever of the forms above
// it was written in: for legacy, inflates the zlib stream; for a v2
// container, decodes its codec (zlib or zstd) and, for a delta, recursively
// resolves and applies it against its base (following the chain up to
// kMaxDeltaChainDepth, and reporting a distinct error if an id is revisited
// on the chain currently being resolved). Either way, the resulting
// canonical bytes are parsed as "<type> <size>\0<payload>", the declared
// length is enforced, trailing data is rejected, and the SHA-256 of the
// canonical bytes is recomputed and compared with id. When payload is
// non-null the payload bytes are captured (subject to kMaxInlinePayload for
// container forms; legacy objects are capped only when payload is
// requested, exactly as in v1); otherwise, for legacy objects, they are
// discarded after hashing so legacy blob verification is not bounded by
// memory. Returns false with *error set on any failure.
bool ReadObject(const std::filesystem::path& objects_dir,
                const std::string& id, std::vector<uint8_t>* payload,
                ObjectInfo* info, std::string* error);

}  // namespace snapvault

#endif  // SNAPVAULT_CPP_SRC_OBJECT_STORE_H_
