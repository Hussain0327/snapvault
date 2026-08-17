#ifndef SNAPVAULT_CPP_SRC_SHA256_H_
#define SNAPVAULT_CPP_SRC_SHA256_H_

#include <array>
#include <cstddef>
#include <cstdint>

namespace snapvault {

// Incremental SHA-256 (FIPS 180-4). Feed bytes with Update and call Finish
// once; the instance must not be reused afterwards.
class Sha256 {
 public:
  Sha256();

  void Update(const uint8_t* data, size_t length);
  std::array<uint8_t, 32> Finish();

 private:
  void ProcessBlock(const uint8_t* block);

  std::array<uint32_t, 8> state_;
  std::array<uint8_t, 64> buffer_;
  size_t buffered_ = 0;
  uint64_t total_bytes_ = 0;
};

// Convenience one-shot digest.
std::array<uint8_t, 32> Sha256Of(const uint8_t* data, size_t length);

}  // namespace snapvault

#endif  // SNAPVAULT_CPP_SRC_SHA256_H_
