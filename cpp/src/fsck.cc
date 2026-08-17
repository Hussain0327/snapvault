#include "src/fsck.h"

#include <cctype>
#include <deque>
#include <fstream>
#include <map>
#include <set>
#include <sstream>
#include <string>
#include <vector>

#include "src/bytes.h"
#include "src/object_store.h"
#include "src/parse.h"

namespace snapvault {
namespace {

std::string Trim(const std::string& s) {
  size_t begin = 0;
  size_t end = s.size();
  while (begin < end && std::isspace(static_cast<unsigned char>(s[begin]))) {
    ++begin;
  }
  while (end > begin && std::isspace(static_cast<unsigned char>(s[end - 1]))) {
    --end;
  }
  return s.substr(begin, end - begin);
}

bool ReadTextFile(const std::filesystem::path& path, std::string* content) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    return false;
  }
  std::ostringstream buffer;
  buffer << in.rdbuf();
  *content = buffer.str();
  return true;
}

// Walks one repository and accumulates findings.
class Checker {
 public:
  Checker(const std::filesystem::path& root, std::ostream& out)
      : metadata_(root / ".snapvault"),
        objects_(metadata_ / "objects"),
        out_(out) {}

  int Run() {
    if (CheckLayout()) {
      CheckRefs();
      CheckUnreachable();
    }
    out_ << "checked " << checked_.size() << " objects: " << errors_
         << " errors, " << warnings_ << " warnings\n";
    return errors_ == 0 ? 0 : 1;
  }

 private:
  void Error(const std::string& message) {
    out_ << "error: " << message << "\n";
    ++errors_;
  }

  void Warning(const std::string& message) {
    out_ << "warning: " << message << "\n";
    ++warnings_;
  }

  bool CheckLayout() {
    std::error_code ec;
    if (!std::filesystem::is_directory(metadata_, ec)) {
      Error("not a SnapVault repository (no .snapvault directory): " +
            metadata_.parent_path().string());
      return false;
    }
    std::string format;
    if (!ReadTextFile(metadata_ / "format", &format)) {
      Error("repository format file is missing or unreadable");
    } else if (Trim(format) != "snapvault 1") {
      Error("unsupported repository format: " + Trim(format));
    }
    std::string head;
    if (!ReadTextFile(metadata_ / "HEAD", &head)) {
      Error("HEAD is missing or unreadable");
    } else {
      const std::string trimmed = Trim(head);
      if (trimmed.rfind("ref: refs/", 0) != 0 ||
          trimmed.find("..") != std::string::npos ||
          trimmed.find('\\') != std::string::npos) {
        Error("detached or malformed HEAD: " + trimmed);
      }
    }
    if (std::filesystem::exists(metadata_ / "restore-in-progress", ec)) {
      Warning(
          "an interrupted restore is recorded in restore-in-progress; "
          "rerun that restore to finish it");
    }
    return true;
  }

  void CheckRefs() {
    const std::filesystem::path refs = metadata_ / "refs";
    std::error_code ec;
    if (!std::filesystem::is_directory(refs, ec)) {
      Error("refs directory is missing");
      return;
    }
    bool any_ref = false;
    for (auto it = std::filesystem::recursive_directory_iterator(refs, ec);
         it != std::filesystem::recursive_directory_iterator();
         it.increment(ec)) {
      if (ec) {
        Error("refs directory is unreadable: " + ec.message());
        return;
      }
      if (!it->is_regular_file(ec)) {
        continue;
      }
      any_ref = true;
      const std::filesystem::path ref = it->path();
      std::string content;
      if (!ReadTextFile(ref, &content)) {
        Error("ref is unreadable: " + ref.string());
        continue;
      }
      const std::string id = Trim(content);
      if (!IsValidObjectId(id)) {
        Error("ref does not name an object id: " + ref.string());
        continue;
      }
      CheckCommitGraph(id);
    }
    if (!any_ref) {
      // A freshly initialized repository has no snapshots yet; that is
      // healthy, not a finding.
      out_ << "note: no refs found; repository has no snapshots\n";
    }
  }

  void CheckCommitGraph(const std::string& start) {
    std::deque<std::string> pending{start};
    while (!pending.empty()) {
      const std::string id = pending.front();
      pending.pop_front();
      reachable_.insert(id);
      if (!visited_commits_.insert(id).second) {
        continue;
      }
      std::vector<uint8_t> payload;
      if (!ReadVerified(id, &payload)) {
        continue;
      }
      if (types_[id] != "commit") {
        Error("object is not a commit: " + id);
        continue;
      }
      Commit commit;
      std::string parse_error;
      if (!ParseCommit(payload, &commit, &parse_error)) {
        Error("commit " + id + ": " + parse_error);
        continue;
      }
      std::set<std::string> stack;
      CheckTree(commit.tree_id, &stack);
      for (const std::string& parent : commit.parents) {
        pending.push_back(parent);
      }
    }
  }

  void CheckTree(const std::string& id, std::set<std::string>* stack) {
    reachable_.insert(id);
    if (ok_trees_.count(id) != 0) {
      return;
    }
    if (!stack->insert(id).second) {
      Error("tree graph contains a cycle at " + id);
      return;
    }
    std::vector<uint8_t> payload;
    if (ReadVerified(id, &payload)) {
      if (types_[id] != "tree") {
        Error("object is not a tree: " + id);
      } else {
        std::vector<TreeEntry> entries;
        std::string parse_error;
        if (!ParseTree(payload, &entries, &parse_error)) {
          Error("tree " + id + ": " + parse_error);
        } else {
          for (const TreeEntry& entry : entries) {
            if (entry.kind == kKindDirectory) {
              CheckTree(entry.object_id, stack);
            } else {
              CheckBlob(entry.object_id);
            }
          }
          ok_trees_.insert(id);
        }
      }
    }
    stack->erase(id);
  }

  void CheckBlob(const std::string& id) {
    reachable_.insert(id);
    if (!ReadVerified(id, nullptr)) {
      return;
    }
    if (types_[id] != "blob") {
      Error("object is not a blob: " + id);
    }
  }

  // Reads an object once, verifying envelope and digest; results are cached
  // so shared subtrees and blobs are checked a single time.
  bool ReadVerified(const std::string& id, std::vector<uint8_t>* payload) {
    auto cached = checked_.find(id);
    if (cached != checked_.end()) {
      if (!cached->second) {
        return false;
      }
      if (payload == nullptr) {
        return true;
      }
      // Payload-bearing objects (trees, commits) are revisited only along
      // error paths; reread rather than cache every payload.
      ObjectInfo info;
      std::string error;
      return ReadObject(objects_, id, payload, &info, &error);
    }
    ObjectInfo info;
    std::string error;
    const bool ok = ReadObject(objects_, id, payload, &info, &error);
    checked_[id] = ok;
    if (ok) {
      types_[id] = info.type;
    } else {
      Error(error);
    }
    return ok;
  }

  void CheckUnreachable() {
    std::error_code ec;
    if (!std::filesystem::is_directory(objects_, ec)) {
      Error("object database is missing");
      return;
    }
    for (const auto& shard : std::filesystem::directory_iterator(objects_)) {
      const std::string shard_name = shard.path().filename().string();
      if (!shard.is_directory()) {
        Warning("unexpected file in object database: " +
                shard.path().string());
        continue;
      }
      if (shard_name.size() != 2) {
        Warning("unexpected directory in object database: " +
                shard.path().string());
        continue;
      }
      for (const auto& file : std::filesystem::directory_iterator(shard)) {
        const std::string name = file.path().filename().string();
        const std::string id = shard_name + name;
        if (!file.is_regular_file() || !IsValidObjectId(id)) {
          Warning("unexpected file in object database: " +
                  file.path().string());
          continue;
        }
        if (reachable_.count(id) == 0) {
          Warning("unreachable object: " + id);
        }
      }
    }
  }

  const std::filesystem::path metadata_;
  const std::filesystem::path objects_;
  std::ostream& out_;
  int errors_ = 0;
  int warnings_ = 0;
  std::set<std::string> reachable_;
  std::set<std::string> visited_commits_;
  std::set<std::string> ok_trees_;
  std::map<std::string, bool> checked_;
  std::map<std::string, std::string> types_;
};

}  // namespace

int RunFsck(const std::filesystem::path& root, std::ostream& out) {
  Checker checker(root, out);
  return checker.Run();
}

}  // namespace snapvault
