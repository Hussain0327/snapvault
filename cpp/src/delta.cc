#include "src/delta.h"

namespace snapvault {
namespace {

// Sets *error and returns false. When target is non-null it is cleared
// first, so every ApplyDelta failure path leaves *target empty as the
// header documents -- including mid-stream failures, where earlier
// instructions may already have written attacker-influenced bytes into it.
bool Fail(std::string* error, const std::string& message,
          std::vector<uint8_t>* target = nullptr) {
  if (target != nullptr) {
    target->clear();
  }
  *error = message;
  return false;
}

// Reads one little-endian base-128 varint starting at delta[*pos], advancing
// *pos past it. Rejects a stream that runs out of bytes before the
// continuation bit clears, and rejects a value that would not fit in 64
// bits (nine continuation bytes is already far past any size this decoder
// accepts, so this only guards against a hostile stream).
bool ReadVarint(const std::vector<uint8_t>& delta, size_t* pos,
                 uint64_t* value, std::string* error) {
  uint64_t result = 0;
  int shift = 0;
  while (true) {
    if (*pos >= delta.size()) {
      return Fail(error, "truncated delta: varint runs past end of stream");
    }
    const uint8_t byte = delta[(*pos)++];
    if (shift >= 64) {
      return Fail(error, "delta varint is too large");
    }
    result |= static_cast<uint64_t>(byte & 0x7f) << shift;
    if ((byte & 0x80) == 0) {
      break;
    }
    shift += 7;
  }
  *value = result;
  return true;
}

}  // namespace

bool ApplyDelta(const std::vector<uint8_t>& base,
                 const std::vector<uint8_t>& delta, uint64_t max_target_size,
                 std::vector<uint8_t>* target, std::string* error) {
  target->clear();
  error->clear();

  size_t pos = 0;
  uint64_t src_size = 0;
  if (!ReadVarint(delta, &pos, &src_size, error)) {
    target->clear();
    return false;
  }
  if (src_size != base.size()) {
    return Fail(error, "delta source size mismatch: base is " +
                           std::to_string(base.size()) + " bytes, delta "
                           "declares " + std::to_string(src_size),
                target);
  }
  uint64_t tgt_size = 0;
  if (!ReadVarint(delta, &pos, &tgt_size, error)) {
    target->clear();
    return false;
  }
  if (tgt_size > max_target_size) {
    return Fail(error, "delta target size " + std::to_string(tgt_size) +
                           " exceeds the maximum of " +
                           std::to_string(max_target_size) + " bytes",
                target);
  }
  target->reserve(static_cast<size_t>(tgt_size));

  while (pos < delta.size()) {
    const uint8_t opcode = delta[pos++];
    if (opcode == 0x00) {
      return Fail(error, "delta contains the reserved opcode 0x00", target);
    }
    if ((opcode & 0x80) == 0) {
      // Insert: the next `opcode` bytes are literals.
      const size_t count = opcode;
      if (delta.size() - pos < count) {
        return Fail(error, "truncated delta: insert runs past end of "
                            "stream", target);
      }
      if (target->size() + count > max_target_size) {
        return Fail(error, "delta target exceeds the maximum of " +
                               std::to_string(max_target_size) + " bytes",
                    target);
      }
      target->insert(target->end(), delta.begin() + pos,
                     delta.begin() + pos + count);
      pos += count;
      continue;
    }

    // Copy from source: bits 0-3 select which of 4 little-endian offset
    // bytes are present, bits 4-6 select which of 3 little-endian size
    // bytes are present; omitted bytes are zero.
    uint64_t offset = 0;
    uint64_t size = 0;
    for (int i = 0; i < 4; ++i) {
      if ((opcode & (1 << i)) != 0) {
        if (pos >= delta.size()) {
          return Fail(error, "truncated delta: copy offset runs past end "
                              "of stream", target);
        }
        offset |= static_cast<uint64_t>(delta[pos++]) << (8 * i);
      }
    }
    for (int i = 0; i < 3; ++i) {
      if ((opcode & (0x10 << i)) != 0) {
        if (pos >= delta.size()) {
          return Fail(error, "truncated delta: copy size runs past end of "
                              "stream", target);
        }
        size |= static_cast<uint64_t>(delta[pos++]) << (8 * i);
      }
    }
    if (size == 0) {
      size = 65536;
    }
    if (offset > base.size() || size > base.size() - offset) {
      return Fail(error, "delta copy is out of bounds: offset " +
                             std::to_string(offset) + " size " +
                             std::to_string(size) + " base is " +
                             std::to_string(base.size()) + " bytes",
                  target);
    }
    if (target->size() + size > max_target_size) {
      return Fail(error, "delta target exceeds the maximum of " +
                             std::to_string(max_target_size) + " bytes",
                  target);
    }
    target->insert(target->end(), base.begin() + offset,
                   base.begin() + offset + size);
  }

  if (target->size() != tgt_size) {
    return Fail(error, "delta target size mismatch: declared " +
                           std::to_string(tgt_size) + " bytes, "
                           "reconstructed " +
                           std::to_string(target->size()) + " bytes",
                target);
  }
  return true;
}

}  // namespace snapvault
