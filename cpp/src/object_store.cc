#include "src/object_store.h"

#include <zlib.h>

#include <cstdio>
#include <cstring>
#include <memory>

#include "src/bytes.h"
#include "src/sha256.h"

namespace snapvault {
namespace {

constexpr size_t kChunkSize = 64 * 1024;
constexpr size_t kMaxHeaderBytes = 128;

bool Fail(std::string* error, const std::string& message) {
  *error = message;
  return false;
}

struct FileCloser {
  void operator()(std::FILE* f) const { std::fclose(f); }
};

struct InflateEnder {
  void operator()(z_stream* stream) const { inflateEnd(stream); }
};

// Consumes inflated canonical bytes: parses the envelope header, counts the
// payload, and optionally captures it.
class EnvelopeSink {
 public:
  EnvelopeSink(const std::string& id, std::vector<uint8_t>* payload)
      : id_(id), payload_(payload) {}

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
  std::unique_ptr<std::FILE, FileCloser> file(
      std::fopen(path.c_str(), "rb"));
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
  }
  return true;
}

}  // namespace snapvault
