// snapvault-fsck verifies the integrity of a SnapVault repository written
// by any conforming implementation: it walks every ref, inflates every
// reachable object, recomputes every SHA-256, and validates tree and commit
// payloads against docs/FORMAT.md. It never writes.

#include <filesystem>
#include <iostream>
#include <string>

#include "src/fsck.h"

namespace {

void PrintUsage(std::ostream& out) {
  out << "usage: snapvault-fsck <repository-directory>\n"
      << "       snapvault-fsck --store <checkpoint-store>\n"
      << "\n"
      << "Verifies every object reachable from every ref in the SnapVault\n"
      << "repository at <repository-directory>, or in a checkpoint store\n"
      << "kept outside the folder it protects (snapvault mcp). Exits 0 when\n"
      << "the repository is intact, 1 when problems were found.\n";
}

}  // namespace

int main(int argc, char** argv) {
  if (argc == 2 && std::string(argv[1]) == "--help") {
    PrintUsage(std::cout);
    return 0;
  }
  if (argc == 3 && std::string(argv[1]) == "--store") {
    return snapvault::RunFsckStore(std::filesystem::path(argv[2]), std::cout);
  }
  if (argc != 2) {
    PrintUsage(std::cerr);
    return 2;
  }
  return snapvault::RunFsck(std::filesystem::path(argv[1]), std::cout);
}
