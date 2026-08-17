#include "src/bytes.h"

namespace snapvault {
namespace {

constexpr char kHexDigits[] = "0123456789abcdef";

// Decodes one UTF-8 sequence starting at data[pos], advancing pos. Invalid
// input decodes to U+FFFD and advances one byte, matching how the Go and
// Java implementations treat malformed names.
uint32_t DecodeCodePoint(const uint8_t* data, size_t length, size_t* pos) {
  const uint8_t first = data[*pos];
  int continuation = 0;
  uint32_t code_point = 0;
  uint32_t minimum = 0;
  if (first < 0x80) {
    ++*pos;
    return first;
  } else if ((first & 0xe0) == 0xc0) {
    continuation = 1;
    code_point = first & 0x1f;
    minimum = 0x80;
  } else if ((first & 0xf0) == 0xe0) {
    continuation = 2;
    code_point = first & 0x0f;
    minimum = 0x800;
  } else if ((first & 0xf8) == 0xf0) {
    continuation = 3;
    code_point = first & 0x07;
    minimum = 0x10000;
  } else {
    ++*pos;
    return 0xfffd;
  }
  if (*pos + continuation >= length) {
    ++*pos;
    return 0xfffd;
  }
  for (int i = 1; i <= continuation; ++i) {
    const uint8_t byte = data[*pos + i];
    if ((byte & 0xc0) != 0x80) {
      ++*pos;
      return 0xfffd;
    }
    code_point = code_point << 6 | (byte & 0x3f);
  }
  if (code_point < minimum || code_point > 0x10ffff ||
      (code_point >= 0xd800 && code_point <= 0xdfff)) {
    ++*pos;
    return 0xfffd;
  }
  *pos += 1 + continuation;
  return code_point;
}

// Yields a string's UTF-16 code units one at a time. A code point above the
// BMP yields its high surrogate, then its low surrogate on the next call.
class Utf16Units {
 public:
  explicit Utf16Units(const std::string& s) : s_(s) {}

  bool Next(uint32_t* unit) {
    if (pending_ != 0) {
      *unit = pending_;
      pending_ = 0;
      return true;
    }
    if (pos_ >= s_.size()) {
      return false;
    }
    const uint32_t code_point = DecodeCodePoint(
        reinterpret_cast<const uint8_t*>(s_.data()), s_.size(), &pos_);
    if (code_point >= 0x10000) {
      pending_ = 0xdc00 + ((code_point - 0x10000) & 0x3ff);
      *unit = 0xd800 + ((code_point - 0x10000) >> 10);
      return true;
    }
    *unit = code_point;
    return true;
  }

 private:
  const std::string& s_;
  size_t pos_ = 0;
  uint32_t pending_ = 0;
};

}  // namespace

std::string ToHex(const uint8_t* data, size_t length) {
  std::string hex;
  hex.reserve(length * 2);
  for (size_t i = 0; i < length; ++i) {
    hex.push_back(kHexDigits[data[i] >> 4]);
    hex.push_back(kHexDigits[data[i] & 0x0f]);
  }
  return hex;
}

std::string HexDigest(const std::array<uint8_t, 32>& digest) {
  return ToHex(digest.data(), digest.size());
}

bool IsValidObjectId(const std::string& id) {
  if (id.size() != 64) {
    return false;
  }
  for (const char c : id) {
    if ((c < '0' || c > '9') && (c < 'a' || c > 'f')) {
      return false;
    }
  }
  return true;
}

bool IsValidUtf8(const uint8_t* data, size_t length) {
  size_t pos = 0;
  while (pos < length) {
    const size_t before = pos;
    const uint32_t code_point = DecodeCodePoint(data, length, &pos);
    // A genuine U+FFFD advances three bytes; a decode failure advances one.
    if (code_point == 0xfffd && pos - before != 3) {
      return false;
    }
  }
  return true;
}

int CompareUtf16(const std::string& a, const std::string& b) {
  Utf16Units units_a(a);
  Utf16Units units_b(b);
  while (true) {
    uint32_t unit_a = 0;
    uint32_t unit_b = 0;
    const bool has_a = units_a.Next(&unit_a);
    const bool has_b = units_b.Next(&unit_b);
    if (!has_a && !has_b) {
      return 0;
    }
    if (!has_a) {
      return -1;
    }
    if (!has_b) {
      return 1;
    }
    if (unit_a != unit_b) {
      return unit_a < unit_b ? -1 : 1;
    }
  }
}

}  // namespace snapvault
