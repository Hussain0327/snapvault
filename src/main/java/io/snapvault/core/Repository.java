package io.snapvault.core;

import io.snapvault.hash.Sha256;
import io.snapvault.model.Commit;
import io.snapvault.model.CommitInfo;
import io.snapvault.model.EntryKind;
import io.snapvault.model.Tree;
import io.snapvault.model.TreeEntry;
import io.snapvault.store.FileObjectStore;
import io.snapvault.store.ObjectStore;
import io.snapvault.store.ObjectType;
import io.snapvault.store.StoredObject;

import java.io.IOException;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.AtomicMoveNotSupportedException;
import java.nio.file.DirectoryStream;
import java.nio.file.Files;
import java.nio.file.LinkOption;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.attribute.BasicFileAttributes;
import java.time.Clock;
import java.time.Instant;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.Deque;
import java.util.HashSet;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.Optional;
import java.util.Set;
import java.util.TreeMap;

/** The high-level SnapVault repository API used by the CLI and tests. */
public final class Repository {
    public static final String METADATA_DIRECTORY = ".snapvault";
    private static final String FORMAT = "snapvault 1";
    private static final String DEFAULT_REF = "refs/heads/main";

    private final Path root;
    private final Path metadata;
    private final ObjectStore objectStore;
    private final Clock clock;

    private Repository(Path root, Path metadata, ObjectStore objectStore, Clock clock) {
        this.root = root;
        this.metadata = metadata;
        this.objectStore = objectStore;
        this.clock = clock;
    }

    /** Initializes a repository in an existing or new ordinary directory. */
    public static Repository init(Path directory) throws IOException {
        Objects.requireNonNull(directory, "directory");
        Path requested = directory.toAbsolutePath().normalize();
        Files.createDirectories(requested);
        Path root = requested.toRealPath();
        Path metadata = root.resolve(METADATA_DIRECTORY);
        if (Files.exists(metadata, LinkOption.NOFOLLOW_LINKS)) {
            throw new IOException("SnapVault is already initialized at " + root);
        }

        Files.createDirectory(metadata);
        Files.createDirectories(metadata.resolve("objects"));
        Files.createDirectories(metadata.resolve("refs/heads"));
        Files.writeString(metadata.resolve("format"), FORMAT + System.lineSeparator());
        Files.writeString(metadata.resolve("HEAD"), "ref: " + DEFAULT_REF + System.lineSeparator());
        return openAt(root, Clock.systemUTC());
    }

    /** Finds a repository at or above {@code start}, so commands also work in subdirectories. */
    public static Repository open(Path start) throws IOException {
        Objects.requireNonNull(start, "start");
        Path requested = start.toAbsolutePath().normalize();
        if (!Files.exists(requested, LinkOption.NOFOLLOW_LINKS)) {
            throw new IOException("Path does not exist: " + requested);
        }
        Path current = Files.isDirectory(requested) ? requested.toRealPath() : requested.toRealPath().getParent();
        while (current != null) {
            if (Files.isDirectory(current.resolve(METADATA_DIRECTORY), LinkOption.NOFOLLOW_LINKS)) {
                return openAt(current, Clock.systemUTC());
            }
            current = current.getParent();
        }
        throw new IOException("Not inside a SnapVault repository (run 'snapvault init' first)");
    }

    /** Test seam for deterministic commit timestamps. */
    public Repository withClock(Clock replacement) {
        return new Repository(root, metadata, objectStore, Objects.requireNonNull(replacement));
    }

    private static Repository openAt(Path root, Clock clock) throws IOException {
        Path realRoot = root.toRealPath();
        Path metadata = realRoot.resolve(METADATA_DIRECTORY);
        String format = Files.readString(metadata.resolve("format")).strip();
        if (!FORMAT.equals(format)) {
            throw new IOException("Unsupported SnapVault repository format: " + format);
        }
        validateHead(metadata);
        ObjectStore store = new FileObjectStore(metadata.resolve("objects"));
        return new Repository(realRoot, metadata, store, clock);
    }

    private static void validateHead(Path metadata) throws IOException {
        String head = Files.readString(metadata.resolve("HEAD")).strip();
        if (!head.startsWith("ref: ")) {
            throw new IOException("Detached or malformed HEAD is not supported");
        }
        resolveRefPath(metadata, head.substring(5));
    }

    public Path root() {
        return root;
    }

    public Path metadata() {
        return metadata;
    }

    /** Creates an immutable snapshot commit and advances the current branch atomically. */
    public String snapshot(String message) throws IOException {
        String normalizedMessage = Objects.requireNonNullElse(message, "Snapshot").strip();
        if (normalizedMessage.isEmpty()) {
            throw new IllegalArgumentException("Snapshot message cannot be empty");
        }

        try (RepositoryLock repositoryLock = RepositoryLock.acquire(metadata.resolve("lock"))) {
            repositoryLock.ensureHeld();
            String treeId = writeTree(root);
            Optional<String> parent = head();
            Commit commit = new Commit(
                    treeId,
                    parent.map(List::of).orElseGet(List::of),
                    Instant.now(clock),
                    normalizedMessage);
            String commitId = objectStore.put(ObjectType.COMMIT, commit.encode());
            writeCurrentRef(commitId);
            return commitId;
        }
    }

    /** Returns the current commit id, or empty before the first snapshot. */
    public Optional<String> head() throws IOException {
        Path ref = currentRefPath();
        if (!Files.exists(ref)) {
            return Optional.empty();
        }
        String objectId = Files.readString(ref).strip();
        try {
            Sha256.requireObjectId(objectId);
        } catch (IllegalArgumentException exception) {
            throw new IOException("Current ref contains an invalid object id", exception);
        }
        return Optional.of(objectId);
    }

    public Commit readCommit(String objectId) throws IOException {
        StoredObject object = objectStore.get(objectId);
        if (object.type() != ObjectType.COMMIT) {
            throw new IOException("Object is not a commit: " + objectId);
        }
        return Commit.decode(object.payload());
    }

    public Tree readTree(String objectId) throws IOException {
        StoredObject object = objectStore.get(objectId);
        if (object.type() != ObjectType.TREE) {
            throw new IOException("Object is not a tree: " + objectId);
        }
        return Tree.decode(object.payload());
    }

    /** Resolves HEAD, HEAD~N, a full commit id, or an unambiguous 7+ character prefix. */
    public String resolveCommit(String revision) throws IOException {
        if (revision == null || revision.isBlank()) {
            throw new IllegalArgumentException("Snapshot revision cannot be empty");
        }
        String spec = revision.strip();
        int generations = 0;
        int tilde = spec.lastIndexOf('~');
        if (tilde >= 0) {
            String suffix = spec.substring(tilde + 1);
            if (suffix.isEmpty() || !suffix.chars().allMatch(Character::isDigit)) {
                throw new IOException("Invalid ancestor expression: " + revision);
            }
            try {
                generations = Integer.parseInt(suffix);
            } catch (NumberFormatException exception) {
                throw new IOException("Ancestor count is too large: " + suffix, exception);
            }
            spec = spec.substring(0, tilde);
        }

        String objectId;
        if (spec.equals("HEAD") || spec.equals("@")) {
            objectId = head().orElseThrow(() -> new IOException("No snapshots exist yet"));
        } else if (spec.length() == Sha256.HEX_LENGTH) {
            objectId = spec.toLowerCase(java.util.Locale.ROOT);
            try {
                Sha256.requireObjectId(objectId);
            } catch (IllegalArgumentException exception) {
                throw new IOException("Invalid snapshot id: " + revision, exception);
            }
            if (!objectStore.contains(objectId)) {
                throw new IOException("Unknown snapshot: " + revision);
            }
        } else {
            if (spec.length() < 7) {
                throw new IOException("Snapshot prefixes must contain at least 7 hex characters");
            }
            List<String> commitMatches = new ArrayList<>();
            for (String candidate : objectStore.findByPrefix(spec)) {
                try {
                    readCommit(candidate);
                    commitMatches.add(candidate);
                } catch (IOException exception) {
                    // A matching blob or tree is not a matching snapshot.
                }
            }
            if (commitMatches.isEmpty()) {
                throw new IOException("Unknown snapshot: " + revision);
            }
            if (commitMatches.size() > 1) {
                throw new IOException("Ambiguous snapshot prefix: " + revision);
            }
            objectId = commitMatches.getFirst();
        }

        readCommit(objectId);
        for (int index = 0; index < generations; index++) {
            Commit commit = readCommit(objectId);
            if (commit.parents().isEmpty()) {
                throw new IOException(revision + " walks beyond the beginning of history");
            }
            objectId = commit.parents().getFirst();
        }
        return objectId;
    }

    /** Walks the commit graph depth-first, suppressing duplicate ancestors. */
    public List<CommitInfo> history(String startRevision, int limit) throws IOException {
        if (limit < 1) {
            throw new IllegalArgumentException("History limit must be positive");
        }
        String start = resolveCommit(startRevision);
        Deque<String> pending = new ArrayDeque<>();
        Set<String> visited = new LinkedHashSet<>();
        List<CommitInfo> result = new ArrayList<>();
        pending.push(start);

        while (!pending.isEmpty() && result.size() < limit) {
            String objectId = pending.pop();
            if (!visited.add(objectId)) {
                continue;
            }
            Commit commit = readCommit(objectId);
            result.add(new CommitInfo(objectId, commit));
            List<String> parents = commit.parents();
            for (int index = parents.size() - 1; index >= 0; index--) {
                pending.push(parents.get(index));
            }
        }
        return List.copyOf(result);
    }

    /** Compares two stored snapshots. */
    public List<FileChange> diff(String fromRevision, String toRevision) throws IOException {
        Commit before = readCommit(resolveCommit(fromRevision));
        Commit after = readCommit(resolveCommit(toRevision));
        return diffTrees(before.treeId(), after.treeId());
    }

    /** Compares one stored snapshot to the live working directory. */
    public List<FileChange> diffWorking(String fromRevision) throws IOException {
        Commit before = readCommit(resolveCommit(fromRevision));
        try (RepositoryLock repositoryLock = RepositoryLock.acquire(metadata.resolve("lock"))) {
            repositoryLock.ensureHeld();
            return diffTrees(before.treeId(), writeTree(root));
        }
    }

    /** Compares HEAD to the working directory, treating an unborn HEAD as an empty tree. */
    public List<FileChange> diffWorkingFromHead() throws IOException {
        try (RepositoryLock repositoryLock = RepositoryLock.acquire(metadata.resolve("lock"))) {
            repositoryLock.ensureHeld();
            String beforeTree;
            Optional<String> current = head();
            if (current.isPresent()) {
                beforeTree = readCommit(current.get()).treeId();
            } else {
                beforeTree = objectStore.put(ObjectType.TREE, new Tree(List.of()).encode());
            }
            return diffTrees(beforeTree, writeTree(root));
        }
    }

    private List<FileChange> diffTrees(String beforeTreeId, String afterTreeId) throws IOException {
        Map<String, TreeEntry> before = flatten(beforeTreeId);
        Map<String, TreeEntry> after = flatten(afterTreeId);
        Set<String> allPaths = new java.util.TreeSet<>();
        allPaths.addAll(before.keySet());
        allPaths.addAll(after.keySet());

        List<FileChange> changes = new ArrayList<>();
        for (String path : allPaths) {
            TreeEntry oldEntry = before.get(path);
            TreeEntry newEntry = after.get(path);
            if (oldEntry == null) {
                changes.add(new FileChange(ChangeType.ADDED, path, null, newEntry));
            } else if (newEntry == null) {
                changes.add(new FileChange(ChangeType.DELETED, path, oldEntry, null));
            } else if (oldEntry.kind() != newEntry.kind()) {
                changes.add(new FileChange(ChangeType.TYPE_CHANGED, path, oldEntry, newEntry));
            } else if (!oldEntry.objectId().equals(newEntry.objectId())
                    || oldEntry.executable() != newEntry.executable()) {
                changes.add(new FileChange(ChangeType.MODIFIED, path, oldEntry, newEntry));
            }
        }
        return List.copyOf(changes);
    }

    private Map<String, TreeEntry> flatten(String treeId) throws IOException {
        Map<String, TreeEntry> entries = new TreeMap<>();
        flatten(treeId, "", entries, new HashSet<>());
        return entries;
    }

    private void flatten(
            String treeId,
            String prefix,
            Map<String, TreeEntry> flattened,
            Set<String> ancestors)
            throws IOException {
        if (!ancestors.add(treeId)) {
            throw new IOException("Tree graph contains a cycle at " + treeId);
        }
        try {
            for (TreeEntry entry : readTree(treeId).entries()) {
                String path = prefix.isEmpty() ? entry.name() : prefix + "/" + entry.name();
                if (entry.kind() == EntryKind.DIRECTORY) {
                    flatten(entry.objectId(), path, flattened, ancestors);
                } else {
                    flattened.put(path, entry);
                }
            }
        } finally {
            ancestors.remove(treeId);
        }
    }

    /**
     * Restores a snapshot into the repository root or a separate target directory.
     * HEAD is intentionally not moved: restore changes files, while snapshot changes history.
     */
    public void restore(String revision, Path requestedTarget, boolean force) throws IOException {
        String commitId = resolveCommit(revision);
        Commit commit = readCommit(commitId);
        verifyTree(commit.treeId(), new HashSet<>(), new HashSet<>(), new HashSet<>());

        Path target = requestedTarget == null
                ? root
                : requestedTarget.toAbsolutePath().normalize();
        if (Files.isSymbolicLink(target)) {
            throw new IOException("Refusing to restore through a symbolic-link target: " + target);
        }
        target = canonicalizeTarget(target);
        boolean inPlace = target.equals(root);
        validateRestoreTarget(target, inPlace);

        try (RepositoryLock repositoryLock = RepositoryLock.acquire(metadata.resolve("lock"))) {
            repositoryLock.ensureHeld();
            if (inPlace) {
                if (!force && isWorkingTreeDirty()) {
                    throw new IOException(
                            "Working directory has unsnapshotted changes; rerun restore with --force");
                }
                clearDirectory(root, metadata);
            } else {
                prepareExternalTarget(target, force);
            }
            materializeTree(commit.treeId(), target);
        }
    }

    private boolean isWorkingTreeDirty() throws IOException {
        String workingTree = writeTree(root);
        Optional<String> current = head();
        if (current.isEmpty()) {
            String emptyTree = objectStore.put(ObjectType.TREE, new Tree(List.of()).encode());
            return !workingTree.equals(emptyTree);
        }
        return !workingTree.equals(readCommit(current.get()).treeId());
    }

    private void validateRestoreTarget(Path target, boolean inPlace) throws IOException {
        if (inPlace) {
            return;
        }
        if (target.getParent() == null) {
            throw new IOException("Refusing to restore over a filesystem root");
        }
        Path userHome = Path.of(System.getProperty("user.home")).toAbsolutePath().normalize();
        if (target.equals(userHome)) {
            throw new IOException("Refusing to restore over the user home directory");
        }
        if (target.startsWith(metadata) || root.startsWith(target)) {
            throw new IOException("Restore target would overwrite the SnapVault repository");
        }
    }

    private static Path canonicalizeTarget(Path target) throws IOException {
        Deque<Path> missing = new ArrayDeque<>();
        Path existing = target;
        while (existing != null && !Files.exists(existing, LinkOption.NOFOLLOW_LINKS)) {
            missing.push(existing.getFileName());
            existing = existing.getParent();
        }
        if (existing == null) {
            throw new IOException("Restore target has no existing filesystem ancestor: " + target);
        }
        Path canonical = existing.toRealPath();
        while (!missing.isEmpty()) {
            canonical = canonical.resolve(missing.pop());
        }
        return canonical.normalize();
    }

    private static void prepareExternalTarget(Path target, boolean force) throws IOException {
        if (!Files.exists(target, LinkOption.NOFOLLOW_LINKS)) {
            Files.createDirectories(target);
            return;
        }
        if (!Files.isDirectory(target, LinkOption.NOFOLLOW_LINKS)) {
            throw new IOException("Restore target is not a directory: " + target);
        }
        boolean hasChildren;
        try (DirectoryStream<Path> children = Files.newDirectoryStream(target)) {
            hasChildren = children.iterator().hasNext();
        }
        if (hasChildren) {
            if (!force) {
                throw new IOException("Restore target is not empty; rerun with --force");
            }
            clearDirectory(target, null);
        }
    }

    private void verifyTree(
            String treeId,
            Set<String> verifiedTrees,
            Set<String> ancestors,
            Set<String> blobs)
            throws IOException {
        if (verifiedTrees.contains(treeId)) {
            return;
        }
        if (!ancestors.add(treeId)) {
            throw new IOException("Tree graph contains a cycle at " + treeId);
        }
        try {
            Tree tree = readTree(treeId);
            for (TreeEntry entry : tree.entries()) {
                if (entry.kind() == EntryKind.DIRECTORY) {
                    verifyTree(entry.objectId(), verifiedTrees, ancestors, blobs);
                } else if (blobs.add(entry.objectId())) {
                    objectStore.copyPayload(
                            entry.objectId(), ObjectType.BLOB, OutputStream.nullOutputStream());
                }
            }
            verifiedTrees.add(treeId);
        } finally {
            ancestors.remove(treeId);
        }
    }

    private void materializeTree(String treeId, Path directory) throws IOException {
        Files.createDirectories(directory);
        for (TreeEntry entry : readTree(treeId).entries()) {
            Path destination = directory.resolve(entry.name()).normalize();
            if (!destination.getParent().equals(directory)) {
                throw new IOException("Unsafe path in snapshot: " + entry.name());
            }
            switch (entry.kind()) {
                case DIRECTORY -> materializeTree(entry.objectId(), destination);
                case FILE -> restoreFile(entry, destination);
                case SYMLINK -> restoreSymlink(entry, destination);
            }
        }
    }

    private void restoreFile(TreeEntry entry, Path destination) throws IOException {
        Path temporary = Files.createTempFile(destination.getParent(), ".snapvault-restore-", ".tmp");
        try {
            try (OutputStream output = Files.newOutputStream(temporary)) {
                objectStore.copyPayload(entry.objectId(), ObjectType.BLOB, output);
            }
            moveReplacing(temporary, destination);
            temporary = null;
            if (!destination.toFile().setExecutable(entry.executable(), false)
                    && Files.isExecutable(destination) != entry.executable()) {
                throw new IOException("Could not restore executable bit for " + destination);
            }
        } finally {
            if (temporary != null) {
                Files.deleteIfExists(temporary);
            }
        }
    }

    private void restoreSymlink(TreeEntry entry, Path destination) throws IOException {
        StoredObject object = objectStore.get(entry.objectId());
        if (object.type() != ObjectType.BLOB) {
            throw new IOException("Symlink target is not stored as a blob");
        }
        byte[] targetBytes = object.payload();
        String target = new String(targetBytes, StandardCharsets.UTF_8);
        if (!java.util.Arrays.equals(target.getBytes(StandardCharsets.UTF_8), targetBytes)) {
            throw new IOException("Symlink target is not valid UTF-8");
        }
        Files.createSymbolicLink(destination, Path.of(target));
    }

    private String writeTree(Path directory) throws IOException {
        List<Path> children = new ArrayList<>();
        try (DirectoryStream<Path> stream = Files.newDirectoryStream(directory)) {
            for (Path child : stream) {
                if (directory.equals(root) && child.getFileName().toString().equals(METADATA_DIRECTORY)) {
                    continue;
                }
                children.add(child);
            }
        }
        children.sort(Comparator.comparing(path -> path.getFileName().toString()));

        List<TreeEntry> entries = new ArrayList<>();
        for (Path child : children) {
            BasicFileAttributes before = Files.readAttributes(
                    child, BasicFileAttributes.class, LinkOption.NOFOLLOW_LINKS);
            String name = child.getFileName().toString();
            if (before.isSymbolicLink()) {
                byte[] target = Files.readSymbolicLink(child)
                        .toString()
                        .getBytes(StandardCharsets.UTF_8);
                String objectId = objectStore.put(ObjectType.BLOB, target);
                entries.add(new TreeEntry(name, EntryKind.SYMLINK, objectId, false));
            } else if (before.isDirectory()) {
                String objectId = writeTree(child);
                entries.add(new TreeEntry(name, EntryKind.DIRECTORY, objectId, false));
            } else if (before.isRegularFile()) {
                String objectId = objectStore.putBlob(child);
                BasicFileAttributes after = Files.readAttributes(
                        child, BasicFileAttributes.class, LinkOption.NOFOLLOW_LINKS);
                if (!after.isRegularFile()
                        || before.size() != after.size()
                        || !before.lastModifiedTime().equals(after.lastModifiedTime())) {
                    throw new IOException("File changed while snapshotting: " + child);
                }
                entries.add(new TreeEntry(name, EntryKind.FILE, objectId, Files.isExecutable(child)));
            } else {
                throw new IOException("Unsupported filesystem entry: " + child);
            }
        }

        return objectStore.put(ObjectType.TREE, new Tree(entries).encode());
    }

    private static void clearDirectory(Path directory, Path preservedChild) throws IOException {
        List<Path> children = new ArrayList<>();
        try (DirectoryStream<Path> stream = Files.newDirectoryStream(directory)) {
            for (Path child : stream) {
                if (preservedChild != null && child.equals(preservedChild)) {
                    continue;
                }
                children.add(child);
            }
        }
        for (Path child : children) {
            deleteRecursively(child);
        }
    }

    private static void deleteRecursively(Path path) throws IOException {
        BasicFileAttributes attributes = Files.readAttributes(
                path, BasicFileAttributes.class, LinkOption.NOFOLLOW_LINKS);
        if (attributes.isDirectory() && !attributes.isSymbolicLink()) {
            clearDirectory(path, null);
        }
        Files.delete(path);
    }

    private Path currentRefPath() throws IOException {
        String head = Files.readString(metadata.resolve("HEAD")).strip();
        if (!head.startsWith("ref: ")) {
            throw new IOException("Detached or malformed HEAD is not supported");
        }
        return resolveRefPath(metadata, head.substring(5));
    }

    private static Path resolveRefPath(Path metadata, String refName) throws IOException {
        if (!refName.startsWith("refs/") || refName.contains("..") || refName.indexOf('\\') >= 0) {
            throw new IOException("Unsafe HEAD ref: " + refName);
        }
        Path ref = metadata.resolve(refName).normalize();
        if (!ref.startsWith(metadata.resolve("refs"))) {
            throw new IOException("HEAD ref escapes repository metadata");
        }
        return ref;
    }

    private void writeCurrentRef(String objectId) throws IOException {
        Sha256.requireObjectId(objectId);
        Path ref = currentRefPath();
        Files.createDirectories(ref.getParent());
        Path temporary = Files.createTempFile(ref.getParent(), ".ref-", ".tmp");
        try {
            Files.writeString(temporary, objectId + System.lineSeparator());
            moveReplacing(temporary, ref);
            temporary = null;
        } finally {
            if (temporary != null) {
                Files.deleteIfExists(temporary);
            }
        }
    }

    private static void moveReplacing(Path source, Path destination) throws IOException {
        try {
            Files.move(
                    source,
                    destination,
                    StandardCopyOption.ATOMIC_MOVE,
                    StandardCopyOption.REPLACE_EXISTING);
        } catch (AtomicMoveNotSupportedException exception) {
            Files.move(source, destination, StandardCopyOption.REPLACE_EXISTING);
        }
    }

    public long objectCount() throws IOException {
        return objectStore.count();
    }
}
