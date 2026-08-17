#ifndef SNAPVAULT_CPP_SRC_BYTES_H_
#define SNAPVAULT_CPP_SRC_BYTES_H_

#include <array>
#include <cstddef>
#include <cstdint>
#include <string>

namespace snapvault {

// Renders bytes as lowercase hexadecimal.
std::string ToHex(const uint8_t* data, size_t length);
std::string HexDigest(const std::array<uint8_t, 32>& digest);

// Reports whether id is exactly 64 lowercase hexadecimal characters.
bool IsValidObjectId(const std::string& id);

// Strict UTF-8 validation: rejects overlong forms, surrogates, and code
// points above U+10FFFF, matching the reference implementations.
bool IsValidUtf8(const uint8_t* data, size_t length);

// Orders strings by UTF-16 code unit, matching Java String order, which is
// the sort order format v1 requires for tree entries. Differs from byte
// order for names containing characters outside the BMP.
int CompareUtf16(const std::string& a, const std::string& b);

}  // namespace snapvault

#endif  // SNAPVAULT_CPP_SRC_BYTES_H_
