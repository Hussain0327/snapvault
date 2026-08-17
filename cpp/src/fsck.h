#ifndef SNAPVAULT_CPP_SRC_FSCK_H_
#define SNAPVAULT_CPP_SRC_FSCK_H_

#include <filesystem>
#include <ostream>

namespace snapvault {

// Verifies the SnapVault repository rooted at root: repository layout,
// HEAD and refs, and every object reachable from every ref (inflated,
// parsed, and SHA-256-checked). Unreachable objects and an interrupted
// restore marker are warnings; everything else is an error. Returns the
// process exit code: 0 when no errors were found, 1 otherwise.
int RunFsck(const std::filesystem::path& root, std::ostream& out);

}  // namespace snapvault

#endif  // SNAPVAULT_CPP_SRC_FSCK_H_
