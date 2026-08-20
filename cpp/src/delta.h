#ifndef SNAPVAULT_CPP_SRC_DELTA_H_
#define SNAPVAULT_CPP_SRC_DELTA_H_

#include <cstdint>
#include <string>
#include <vector>

namespace snapvault {

// Applies a Git pack-style delta (see docs/FORMAT.md "Delta instruction
// format") to base, producing target. base must be the referenced object's
// exact canonical bytes.
//
// Validates strictly: the delta's declared source size must equal
// base.size(); every copy instruction is bounds-checked against base; every
// varint and literal run must be fully present, not truncated; opcode 0x00
// is rejected; and the reconstructed output must be exactly the delta's
// declared target size once the instruction stream is exhausted. The
// reconstructed target is additionally capped at max_target_size bytes,
// checked before and during reconstruction so a hostile tgtSize or a run of
// copy/insert instructions cannot force unbounded allocation.
//
// Returns false with *error set on any violation; target is left empty.
bool ApplyDelta(const std::vector<uint8_t>& base,
                 const std::vector<uint8_t>& delta, uint64_t max_target_size,
                 std::vector<uint8_t>* target, std::string* error);

}  // namespace snapvault

#endif  // SNAPVAULT_CPP_SRC_DELTA_H_
