#include "src/object_store.h"

#include <zlib.h>
#include <zstd.h>

#include <algorithm>
#include <cstdio>
#include <cstring>
#include <memory>

#include "src/bytes.h"
#include "src/delta.h"
#include "src/sha256.h"

namespace snapvault {
namespace {

constexpr size_t kChunkSize = 64 * 1024;
constexpr size_t kMaxHeaderBytes = 128;
constexpr uint8_t kContainerMagic[4] = {'S', 'V', 'O', '2'};
constexpr uint8_t kKindFull = 0x01;
constexpr uint8_t kKindDelta = 0x02;
constexpr uint8_t kCodecZlib = 0x01;
constexpr uint8_t kCodecZstd = 0x02;

bool Fail(std::string* error, const std::string& message) {
  *error = message;
  return false;
}

struct FileCloser {
  void operator()(std::FILE* f) const { std::fclose(f); }
};

using UniqueFile = std::unique_ptr<std::FILE, FileCloser>;

struct InflateEnder {
  void operator()(z_stream* stream) const { inflateEnd(stream); }
};

struct ZstdDStreamDeleter {
  void operator()(ZSTD_DStream* stream) const { ZSTD_freeDStream(stream); }
};

// Consumes inflated canonical bytes: parses the envelope header, counts the
// payload, and optionally captures it. Shared by the legacy path (streamed
// chunk by chunk, so a blob can be verified without ever being fully
// buffered) and, in one shot, by the container path.
class EnvelopeSink {
 public:
  EnvelopeSink(const std::string& id, std::vector<uint8_t>* payload)
      : id_(id), payload_(payload) {}

  // The raw header bytes exactly as they appeared before the NUL separator,
  // whatever their formatting -- a caller that needs the object's full
  // canonical bytes (rather than just its payload) must use this instead of
  // re-rendering the header from info().type and info().payload_size, since
  // the object's id was computed over -- and the digest already verified --
  // these exact bytes, not a canonicalized re-encoding of them.
  const std::string& raw_header() const { return header_; }

  bool Consume(const uint8_t* data, size_t length, std::string* error) {
    size_t pos = 0;
    while (in_header_ && pos < length) {
      const uint8_t byte = data[pos++];
      if (byte == 0) {
        in_header_ = false;
        if (!ParseHeader(error)) {
          return false;
        }
        continue;
      }
      if (header_.size() >= kMaxHeaderBytes) {
        return Fail(error, "object header is too long: " + id_);
      }
      header_.push_back(static_cast<char>(byte));
    }
    const size_t body = length - pos;
    payload_seen_ += body;
    if (payload_seen_ > info_.payload_size) {
      return Fail(error, "object has trailing data: " + id_);
    }
    if (payload_ != nullptr) {
      payload_->insert(payload_->end(), data + pos, data + length);
    }
    return true;
  }

  bool Finish(std::string* error) const {
    if (in_header_) {
      return Fail(error, "truncated object header: " + id_);
    }
    if (payload_seen_ != info_.payload_size) {
      return Fail(error, "truncated object payload: " + id_);
    }
    return true;
  }

  const ObjectInfo& info() const { return info_; }

 private:
  bool ParseHeader(std::string* error) {
    const size_t separator = header_.find(' ');
    if (separator == std::string::npos || separator == 0 ||
        separator == header_.size() - 1) {
      return Fail(error, "malformed object header: " + id_);
    }
    info_.type = header_.substr(0, separator);
    if (info_.type != "blob" && info_.type != "tree" &&
        info_.type != "commit") {
      return Fail(error, "unknown object type: " + info_.type);
    }
    const std::string size_text = header_.substr(separator + 1);
    uint64_t size = 0;
    for (const char c : size_text) {
      if (c < '0' || c > '9') {
        return Fail(error, "malformed object size: " + id_);
      }
      if (size > (UINT64_MAX - static_cast<uint64_t>(c - '0')) / 10) {
        return Fail(error, "malformed object size: " + id_);
      }
      size = size * 10 + static_cast<uint64_t>(c - '0');
    }
    info_.payload_size = size;
    if (payload_ != nullptr) {
      if (size > kMaxInlinePayload) {
        return Fail(error, "object " + id_ +
                               " declares an implausible payload size: " +
                               std::to_string(size));
      }
      payload_->reserve(static_cast<size_t>(size));
    }
    return true;
  }

  const std::string& id_;
  std::vector<uint8_t>* payload_;
  std::string header_;
  bool in_header_ = true;
  uint64_t payload_seen_ = 0;
  ObjectInfo info_;
};

// Reads and verifies a legacy (v1) object: the whole file is one zlib
// stream of the canonical bytes. Behavior, including the "streamed and
// uncapped when payload is null" property, is unchanged from v1. When
// raw_header is non-null it receives the header exactly as read, for a
// caller that needs the object's full canonical bytes rather than just its
// payload (see EnvelopeSink::raw_header).
bool ReadLegacyObject(const std::filesystem::path& objects_dir,
                      const std::string& id, std::vector<uint8_t>* payload,
                      ObjectInfo* info, std::string* error,
                      std::string* raw_header = nullptr) {
  const std::filesystem::path path =
      objects_dir / id.substr(0, 2) / id.substr(2);
  UniqueFile file(std::fopen(path.c_str(), "rb"));
  if (file == nullptr) {
    return Fail(error, "object is unreadable: " + id);
  }

  z_stream stream;
  std::memset(&stream, 0, sizeof(stream));
  if (inflateInit(&stream) != Z_OK) {
    return Fail(error, "zlib initialization failed");
  }
  std::unique_ptr<z_stream, InflateEnder> stream_guard(&stream);

  Sha256 digest;
  EnvelopeSink sink(id, payload);
  std::vector<uint8_t> in(kChunkSize);
  std::vector<uint8_t> out(kChunkSize);
  int status = Z_OK;
  while (status != Z_STREAM_END) {
    if (stream.avail_in == 0) {
      const size_t read = std::fread(in.data(), 1, in.size(), file.get());
      if (read == 0) {
        if (std::ferror(file.get())) {
          return Fail(error, "object is unreadable: " + id);
        }
        return Fail(error, "object is corrupt: " + id);
      }
      stream.next_in = in.data();
      stream.avail_in = static_cast<uInt>(read);
    }
    stream.next_out = out.data();
    stream.avail_out = static_cast<uInt>(out.size());
    status = inflate(&stream, Z_NO_FLUSH);
    if (status != Z_OK && status != Z_STREAM_END) {
      return Fail(error, "object is corrupt: " + id);
    }
    const size_t produced = out.size() - stream.avail_out;
    if (produced > 0) {
      digest.Update(out.data(), produced);
      if (!sink.Consume(out.data(), produced, error)) {
        return false;
      }
    }
  }
  if (!sink.Finish(error)) {
    return false;
  }

  const std::string actual = HexDigest(digest.Finish());
  if (actual != id) {
    return Fail(error, "object failed its SHA-256 integrity check: " + id +
                           " (actual " + actual + ")");
  }
  if (info != nullptr) {
    *info = sink.info();
    info->form = ObjectForm::kLegacy;
    info->delta_depth = 0;
  }
  if (raw_header != nullptr) {
    *raw_header = sink.raw_header();
  }
  return true;
}

// Decompresses the remainder of `file` (from its current position to EOF)
// with zlib, appending to *out and failing once *out would exceed max_size.
// Like the legacy reader, trailing bytes after the logical end of the
// stream are not an error: nothing in this codebase checks for them.
bool InflateZlibBounded(std::FILE* file, uint64_t max_size,
                        std::vector<uint8_t>* out, std::string* error) {
  z_stream stream;
  std::memset(&stream, 0, sizeof(stream));
  if (inflateInit(&stream) != Z_OK) {
    return Fail(error, "zlib initialization failed");
  }
  std::unique_ptr<z_stream, InflateEnder> stream_guard(&stream);

  std::vector<uint8_t> in(kChunkSize);
  std::vector<uint8_t> chunk(kChunkSize);
  int status = Z_OK;
  while (status != Z_STREAM_END) {
    if (stream.avail_in == 0) {
      const size_t read = std::fread(in.data(), 1, in.size(), file);
      if (read == 0) {
        if (std::ferror(file)) {
          return Fail(error, "object is unreadable");
        }
        return Fail(error, "object is corrupt: truncated compressed "
                            "stream");
      }
      stream.next_in = in.data();
      stream.avail_in = static_cast<uInt>(read);
    }
    stream.next_out = chunk.data();
    stream.avail_out = static_cast<uInt>(chunk.size());
    status = inflate(&stream, Z_NO_FLUSH);
    if (status != Z_OK && status != Z_STREAM_END) {
      return Fail(error, "object is corrupt: invalid zlib stream");
    }
    const size_t produced = chunk.size() - stream.avail_out;
    if (produced > 0) {
      if (out->size() + produced > max_size) {
        return Fail(error, "object exceeds the maximum reconstructed size "
                            "of " + std::to_string(max_size) + " bytes");
      }
      out->insert(out->end(), chunk.begin(), chunk.begin() + produced);
    }
  }
  return true;
}

// Same contract as InflateZlibBounded, decoding a single zstd frame. Frame
// content size is never relied on; the loop is driven purely by
// ZSTD_decompressStream's return value and the byte cap. FORMAT.md requires
// exactly one standard zstd frame with no skippable frames, so this also
// rejects a leading skippable-frame magic outright and, once the one frame
// is fully decoded, rejects any byte left over -- whether still buffered or
// still unread in the file -- as more than one frame or trailing garbage.
bool InflateZstdBounded(std::FILE* file, uint64_t max_size,
                        std::vector<uint8_t>* out, std::string* error) {
  std::unique_ptr<ZSTD_DStream, ZstdDStreamDeleter> stream(
      ZSTD_createDStream());
  if (stream == nullptr || ZSTD_isError(ZSTD_initDStream(stream.get()))) {
    return Fail(error, "zstd initialization failed");
  }

  std::vector<uint8_t> in(kChunkSize);
  std::vector<uint8_t> chunk(kChunkSize);
  bool checked_magic = false;
  bool frame_complete = false;
  ZSTD_inBuffer in_buffer = {nullptr, 0, 0};
  while (!frame_complete) {
    if (in_buffer.pos >= in_buffer.size) {
      const size_t read = std::fread(in.data(), 1, in.size(), file);
      if (read == 0) {
        if (std::ferror(file)) {
          return Fail(error, "object is unreadable");
        }
        return Fail(error, "object is corrupt: truncated compressed stream");
      }
      if (!checked_magic) {
        checked_magic = true;
        // Skippable frame magic numbers are 0x184D2A50 through 0x184D2A5F,
        // stored little-endian.
        if (read >= 4 && in[0] >= 0x50 && in[0] <= 0x5f && in[1] == 0x2a &&
            in[2] == 0x4d && in[3] == 0x18) {
          return Fail(error, "object is corrupt: skippable zstd frames "
                              "are not allowed");
        }
      }
      in_buffer = ZSTD_inBuffer{in.data(), read, 0};
    }
    ZSTD_outBuffer out_buffer = {chunk.data(), chunk.size(), 0};
    const size_t result =
        ZSTD_decompressStream(stream.get(), &out_buffer, &in_buffer);
    if (ZSTD_isError(result)) {
      return Fail(error, std::string("object is corrupt: ") +
                             ZSTD_getErrorName(result));
    }
    if (out_buffer.pos > 0) {
      if (out->size() + out_buffer.pos > max_size) {
        return Fail(error, "object exceeds the maximum reconstructed "
                            "size of " + std::to_string(max_size) +
                               " bytes");
      }
      out->insert(out->end(), chunk.begin(), chunk.begin() + out_buffer.pos);
    }
    if (result == 0) {
      frame_complete = true;
    }
  }

  if (in_buffer.pos < in_buffer.size) {
    return Fail(error, "object is corrupt: codec-zstd stream carries more "
                        "than one frame");
  }
  uint8_t probe;
  if (std::fread(&probe, 1, 1, file) != 0) {
    return Fail(error, "object is corrupt: codec-zstd stream carries more "
                        "than one frame");
  }
  if (std::ferror(file)) {
    return Fail(error, "object is unreadable");
  }
  return true;
}

bool InflateBounded(uint8_t codec, std::FILE* file, uint64_t max_size,
                    std::vector<uint8_t>* out, std::string* error) {
  if (codec == kCodecZlib) {
    return InflateZlibBounded(file, max_size, out, error);
  }
  return InflateZstdBounded(file, max_size, out, error);
}

// Parses canonical[...] as "<type> <size>\0<payload>", verifies size and
// SHA-256 against id, and fills in info's type and payload_size. Used for
// container forms, whose canonical bytes are always fully materialized
// before verification (unlike the legacy path's streaming check).
bool ParseCanonicalEnvelope(const std::vector<uint8_t>& canonical,
                            const std::string& id, ObjectInfo* info,
                            std::string* error) {
  const size_t search_limit = std::min(canonical.size(), kMaxHeaderBytes + 1);
  size_t separator_nul = canonical.size();
  for (size_t i = 0; i < search_limit; ++i) {
    if (canonical[i] == 0) {
      separator_nul = i;
      break;
    }
  }
  if (separator_nul == canonical.size()) {
    return Fail(error, "malformed object header: " + id);
  }
  const std::string header(
      reinterpret_cast<const char*>(canonical.data()), separator_nul);
  const size_t space = header.find(' ');
  if (space == std::string::npos || space == 0 || space == header.size() - 1) {
    return Fail(error, "malformed object header: " + id);
  }
  const std::string type = header.substr(0, space);
  if (type != "blob" && type != "tree" && type != "commit") {
    return Fail(error, "unknown object type: " + type);
  }
  const std::string size_text = header.substr(space + 1);
  uint64_t size = 0;
  for (const char c : size_text) {
    if (c < '0' || c > '9') {
      return Fail(error, "malformed object size: " + id);
    }
    if (size > (UINT64_MAX - static_cast<uint64_t>(c - '0')) / 10) {
      return Fail(error, "malformed object size: " + id);
    }
    size = size * 10 + static_cast<uint64_t>(c - '0');
  }
  const uint64_t body_length = canonical.size() - (separator_nul + 1);
  if (body_length != size) {
    return Fail(error, "truncated object payload: " + id);
  }

  const std::string actual =
      HexDigest(Sha256Of(canonical.data(), canonical.size()));
  if (actual != id) {
    return Fail(error, "object failed its SHA-256 integrity check: " + id +
                           " (actual " + actual + ")");
  }
  info->type = type;
  info->payload_size = size;
  return true;
}

bool LoadCanonicalBytes(const std::filesystem::path& objects_dir,
                        const std::string& id,
                        std::vector<std::string>* chain_stack, int depth,
                        std::vector<uint8_t>* canonical, ObjectInfo* info,
                        std::string* error);

// Resolves id as a container's delta or full base, whichever it turns out
// to be: legacy objects are read (and thereby capped at kMaxInlinePayload,
// since payload is always requested here) through the same path top-level
// legacy reads use; container objects recurse into LoadCanonicalBytes.
bool LoadBaseCanonicalBytes(const std::filesystem::path& objects_dir,
                            const std::string& id,
                            std::vector<std::string>* chain_stack, int depth,
                            std::vector<uint8_t>* canonical, ObjectInfo* info,
                            std::string* error) {
  return LoadCanonicalBytes(objects_dir, id, chain_stack, depth, canonical,
                            info, error);
}

// Reads the first up-to-6 bytes of id's object file, enough to sniff its
// form and, for a container, read its kind and codec bytes.
bool PeekHeader(const std::filesystem::path& objects_dir,
                const std::string& id, uint8_t* header, size_t* header_len,
                std::string* error) {
  const std::filesystem::path path =
      objects_dir / id.substr(0, 2) / id.substr(2);
  UniqueFile file(std::fopen(path.c_str(), "rb"));
  if (file == nullptr) {
    return Fail(error, "object is unreadable: " + id);
  }
  *header_len = std::fread(header, 1, 6, file.get());
  return true;
}

bool LoadCanonicalBytes(const std::filesystem::path& objects_dir,
                        const std::string& id,
                        std::vector<std::string>* chain_stack, int depth,
                        std::vector<uint8_t>* canonical, ObjectInfo* info,
                        std::string* error) {
  if (depth > kMaxDeltaChainDepth) {
    return Fail(error, "delta chain depth exceeds the maximum of " +
                           std::to_string(kMaxDeltaChainDepth) +
                           " while resolving " + id);
  }
  if (std::find(chain_stack->begin(), chain_stack->end(), id) !=
      chain_stack->end()) {
    return Fail(error,
               "delta cycle detected: " + id + " is already being resolved "
               "on this chain");
  }
  if (!IsValidObjectId(id)) {
    return Fail(error, "invalid object id: " + id);
  }
  const std::filesystem::path path =
      objects_dir / id.substr(0, 2) / id.substr(2);
  std::error_code ec;
  if (!std::filesystem::is_regular_file(
          std::filesystem::symlink_status(path, ec))) {
    if (depth > 0) {
      return Fail(error, "delta base object does not exist: " + id);
    }
    return Fail(error, "object does not exist: " + id);
  }

  uint8_t header[6] = {};
  size_t header_len = 0;
  if (!PeekHeader(objects_dir, id, header, &header_len, error)) {
    return false;
  }
  if (header_len == 0) {
    return Fail(error, "object is corrupt: empty file: " + id);
  }

  if ((header[0] & 0x0F) == 0x08) {
    std::vector<uint8_t> payload;
    ObjectInfo legacy_info;
    std::string raw_header;
    if (!ReadLegacyObject(objects_dir, id, &payload, &legacy_info, error,
                          &raw_header)) {
      return false;
    }
    // Use the header exactly as stored, not a re-rendering from the parsed
    // type and integer size: the object's id (and the digest check
    // ReadLegacyObject already ran) covers these exact bytes, which a
    // non-canonical header (e.g. a leading zero in the size) would not
    // survive re-rendering unchanged.
    canonical->assign(raw_header.begin(), raw_header.end());
    canonical->push_back(0);
    canonical->insert(canonical->end(), payload.begin(), payload.end());
    *info = legacy_info;
    return true;
  }

  if (header_len < 4 || std::memcmp(header, kContainerMagic, 4) != 0) {
    return Fail(error, "object is corrupt: unrecognized object header: " +
                           id);
  }
  if (header_len < 6) {
    return Fail(error, "object is corrupt: truncated container header: " +
                           id);
  }
  const uint8_t kind = header[4];
  const uint8_t codec = header[5];
  if (kind != kKindFull && kind != kKindDelta) {
    return Fail(error, "object is corrupt: unknown container kind 0x" +
                           ToHex(&kind, 1) + ": " + id);
  }
  if (codec != kCodecZlib && codec != kCodecZstd) {
    return Fail(error, "object is corrupt: unknown container codec 0x" +
                           ToHex(&codec, 1) + ": " + id);
  }

  UniqueFile file(std::fopen(path.c_str(), "rb"));
  if (file == nullptr) {
    return Fail(error, "object is unreadable: " + id);
  }
  if (std::fseek(file.get(), 6, SEEK_SET) != 0) {
    return Fail(error, "object is unreadable: " + id);
  }

  if (kind == kKindFull) {
    std::vector<uint8_t> plain;
    if (!InflateBounded(codec, file.get(), kMaxInlinePayload, &plain,
                        error)) {
      return false;
    }
    *canonical = std::move(plain);
    info->form = ObjectForm::kContainerFull;
    info->delta_depth = 0;
    return ParseCanonicalEnvelope(*canonical, id, info, error);
  }

  uint8_t base_id_raw[32];
  if (std::fread(base_id_raw, 1, sizeof(base_id_raw), file.get()) !=
      sizeof(base_id_raw)) {
    return Fail(error, "object is corrupt: truncated delta base id: " + id);
  }
  const std::string base_id = ToHex(base_id_raw, sizeof(base_id_raw));

  std::vector<uint8_t> delta_instructions;
  if (!InflateBounded(codec, file.get(), kMaxInlinePayload,
                      &delta_instructions, error)) {
    return false;
  }

  chain_stack->push_back(id);
  std::vector<uint8_t> base_canonical;
  ObjectInfo base_info;
  const bool base_ok = LoadBaseCanonicalBytes(
      objects_dir, base_id, chain_stack, depth + 1, &base_canonical,
      &base_info, error);
  chain_stack->pop_back();
  if (!base_ok) {
    return false;
  }

  std::vector<uint8_t> target;
  std::string apply_error;
  if (!ApplyDelta(base_canonical, delta_instructions, kMaxInlinePayload,
                  &target, &apply_error)) {
    return Fail(error, "object " + id + " failed to reconstruct from its "
                        "delta: " + apply_error);
  }
  *canonical = std::move(target);
  info->form = ObjectForm::kContainerDelta;
  info->delta_depth = base_info.delta_depth + 1;
  return ParseCanonicalEnvelope(*canonical, id, info, error);
}

}  // namespace

bool ReadObject(const std::filesystem::path& objects_dir,
                const std::string& id, std::vector<uint8_t>* payload,
                ObjectInfo* info, std::string* error) {
  error->clear();
  if (payload != nullptr) {
    payload->clear();
  }
  if (!IsValidObjectId(id)) {
    return Fail(error, "invalid object id: " + id);
  }
  const std::filesystem::path path =
      objects_dir / id.substr(0, 2) / id.substr(2);
  std::error_code ec;
  if (!std::filesystem::is_regular_file(
          std::filesystem::symlink_status(path, ec))) {
    return Fail(error, "object does not exist: " + id);
  }

  uint8_t header[6] = {};
  size_t header_len = 0;
  if (!PeekHeader(objects_dir, id, header, &header_len, error)) {
    return false;
  }
  if (header_len == 0) {
    return Fail(error, "object is corrupt: empty file: " + id);
  }

  if ((header[0] & 0x0F) == 0x08) {
    // Legacy objects keep the exact v1 read path: unbounded streaming when
    // payload capture isn't requested, matching v1 behavior byte for byte.
    return ReadLegacyObject(objects_dir, id, payload, info, error);
  }

  if (header_len < 4 || std::memcmp(header, kContainerMagic, 4) != 0) {
    return Fail(error, "object is corrupt: unrecognized object header: " +
                           id);
  }

  std::vector<std::string> chain_stack;
  std::vector<uint8_t> canonical;
  ObjectInfo local_info;
  if (!LoadCanonicalBytes(objects_dir, id, &chain_stack, 0, &canonical,
                         &local_info, error)) {
    return false;
  }
  if (info != nullptr) {
    *info = local_info;
  }
  if (payload != nullptr) {
    const auto nul_it = std::find(canonical.begin(), canonical.end(), 0);
    payload->assign(nul_it + 1, canonical.end());
  }
  return true;
}

}  // namespace snapvault
