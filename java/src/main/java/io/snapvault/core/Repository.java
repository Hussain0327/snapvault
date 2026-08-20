package io.snapvault.core;

import io.snapvault.hash.Sha256;
import io.snapvault.model.Commit;
import io.snapvault.model.CommitInfo;
import io.snapvault.model.EntryKind;
import io.snapvault.model.Tree;
import io.snapvault.model.TreeEntry;
import io.snapvault.store.FileObjectStore;
import io.snapvault.store.ObjectId;
import io.snapvault.store.ObjectStore;
import io.snapvault.store.ObjectType;
import io.snapvault.store.StoredObject;

import java.io.IOException;
import java.io.OutputStream;
import java.io.UncheckedIOException;
import java.nio.ByteBuffer;
import java.nio.channels.FileChannel;
import java.nio.charset.StandardCharsets;
import java.nio.file.AtomicMoveNotSupportedException;
import java.nio.file.DirectoryStream;
import java.nio.file.Files;
import java.nio.file.LinkOption;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.StandardOpenOption;
import java.nio.file.attribute.BasicFileAttributes;
import java.nio.file.attribute.FileTime;
import java.text.Normalizer;
import java.time.Clock;
import java.time.Instant;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.Deque;
import java.util.HashMap;
import java.util.HashSet;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.NavigableMap;
import java.util.Objects;
import java.util.Optional;
import java.util.Set;
import java.util.TreeMap;
import java.util.TreeSet;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;

/** The high-level SnapVault repository API used by the CLI and tests. */
public final class Repository {
    public static final String METADATA_DIRECTORY = ".snapvault";

    // This CLI's writer only ever writes format 1; format 2 repositories are read-compatible
    // (legacy zlib objects are always legal there) and differ only in the object read path, so
    // no second write format is needed here.
    private static final String FORMAT = "snapvault 1";
    private static final Set<String> SUPPORTED_FORMATS = Set.of("snapvault 1", "snapvault 2");
    private static final String DEFAULT_REF = "refs/heads/main";
    private static final String RESTORE_MARKER = "restore-in-progress";

    private final Path root;
    private final Path metadata;
    private final ObjectStore objectStore;
    private final Clock clock;
    private final int workers;

    private Repository(
            Path root, Path metadata, ObjectStore objectStore, Clock clock, int workers) {
        this.root = root;
        this.metadata = metadata;
        this.objectStore = objectStore;
        this.clock = clock;
        this.workers = workers;
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
        return new Repository(
                root, metadata, objectStore, Objects.requireNonNull(replacement), workers);
    }

    /**
     * Bounds the hashing worker pool. {@code 0} restores the per-CPU default. The object ids a
     * snapshot produces do not depend on the worker count.
     */
    public Repository withWorkers(int workerCount) {
        if (workerCount < 0) {
            throw new IllegalArgumentException("Worker count cannot be negative");
        }
        return new Repository(root, metadata, objectStore, clock, workerCount);
    }

    private static Repository openAt(Path root, Clock clock) throws IOException {
        Path realRoot = root.toRealPath();
        Path metadata = realRoot.resolve(METADATA_DIRECTORY);
        String format = Files.readString(metadata.resolve("format")).strip();
        if (!SUPPORTED_FORMATS.contains(format)) {
            throw new IOException("Unsupported SnapVault repository format: " + format);
        }
        validateHead(metadata);
        FileObjectStore store = new FileObjectStore(metadata.resolve("objects"));
        // format is one of SUPPORTED_FORMATS ("snapvault 1" or "snapvault 2"), so its last
        // character is always the version digit; the store needs it to reject a container-form
        // object it might find in a format 1 repository (FORMAT.md, "Compatibility").
        store.setFormat(format.charAt(format.length() - 1) - '0');
        return new Repository(realRoot, metadata, store, clock, 0);
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
            requireCompleteWorkingTree();
            List<PendingEntry> files = new ArrayList<>();
            String treeId = scan(root, "", storingSink(), new TreeMap<>(), files);
            DirCache.write(cachePath(), toCacheEntries(files));
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

    /**
     * Resolves HEAD, a full commit id, or an unambiguous 7+ character prefix, optionally followed
     * by one or more ancestor steps. {@code ~} means one generation and {@code ~N} means N, and
     * repeated steps accumulate, so {@code HEAD~1~1} and {@code HEAD~2} name the same snapshot.
     */
    public String resolveCommit(String revision) throws IOException {
        if (revision == null || revision.isBlank()) {
            throw new IllegalArgumentException("Snapshot revision cannot be empty");
        }
        String spec = revision.strip();
        long generations = 0;
        int tilde;
        while ((tilde = spec.lastIndexOf('~')) >= 0) {
            String suffix = spec.substring(tilde + 1);
            long step;
            if (suffix.isEmpty()) {
                step = 1;
            } else if (suffix.chars().allMatch(Character::isDigit)) {
                try {
                    step = Long.parseLong(suffix);
                } catch (NumberFormatException exception) {
                    throw new IOException("Ancestor count is too large: " + suffix, exception);
                }
            } else {
                throw new IOException("Invalid ancestor expression: " + revision);
            }
            generations += step;
            if (generations < 0) {
                throw new IOException("Ancestor count is too large: " + revision);
            }
            spec = spec.substring(0, tilde);
        }
        if (spec.isEmpty()) {
            throw new IOException("Revision names no starting snapshot: " + revision);
        }

        String objectId;
        if (spec.equals("HEAD") || spec.equals("@")) {
            objectId = head().orElseThrow(() -> new IOException("No snapshots exist yet"));
        } else if (spec.length() == Sha256.HEX_LENGTH) {
            objectId = spec.toLowerCase(Locale.ROOT);
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
        for (long index = 0; index < generations; index++) {
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

    /** Compares one stored snapshot to the live working directory without writing objects. */
    public List<FileChange> diffWorking(String fromRevision) throws IOException {
        Commit before = readCommit(resolveCommit(fromRevision));
        try (RepositoryLock repositoryLock = RepositoryLock.acquire(metadata.resolve("lock"))) {
            repositoryLock.ensureHeld();
            requireCompleteWorkingTree();
            return compare(flatten(before.treeId()), scanWorkingTree());
        }
    }

    /** Compares HEAD to the working directory, treating an unborn HEAD as an empty tree. */
    public List<FileChange> diffWorkingFromHead() throws IOException {
        try (RepositoryLock repositoryLock = RepositoryLock.acquire(metadata.resolve("lock"))) {
            repositoryLock.ensureHeld();
            requireCompleteWorkingTree();
            Optional<String> current = head();
            NavigableMap<String, TreeEntry> before = current.isPresent()
                    ? flatten(readCommit(current.get()).treeId())
                    : new TreeMap<>();
            return compare(before, scanWorkingTree());
        }
    }

    private List<FileChange> diffTrees(String beforeTreeId, String afterTreeId) throws IOException {
        return compare(flatten(beforeTreeId), flatten(afterTreeId));
    }

    /**
     * Compares two flattened trees. Both sides contain every file, symbolic link, and empty
     * directory, so an empty set of changes always means the two trees are byte-for-byte identical.
     */
    private static List<FileChange> compare(
            NavigableMap<String, TreeEntry> before, NavigableMap<String, TreeEntry> after) {
        Set<String> allPaths = new TreeSet<>();
        allPaths.addAll(before.keySet());
        allPaths.addAll(after.keySet());

        List<FileChange> changes = new ArrayList<>();
        for (String path : allPaths) {
            TreeEntry oldEntry = before.get(path);
            TreeEntry newEntry = after.get(path);
            if (oldEntry == null) {
                if (!hasDescendants(newEntry, path, before)) {
                    changes.add(new FileChange(ChangeType.ADDED, path, null, newEntry));
                }
            } else if (newEntry == null) {
                if (!hasDescendants(oldEntry, path, after)) {
                    changes.add(new FileChange(ChangeType.DELETED, path, oldEntry, null));
                }
            } else if (oldEntry.kind() != newEntry.kind()) {
                changes.add(new FileChange(ChangeType.TYPE_CHANGED, path, oldEntry, newEntry));
            } else if (!oldEntry.objectId().equals(newEntry.objectId())
                    || oldEntry.executable() != newEntry.executable()) {
                changes.add(new FileChange(ChangeType.MODIFIED, path, oldEntry, newEntry));
            }
        }
        return List.copyOf(changes);
    }

    /**
     * Reports whether a directory that is empty on one side holds entries on the other side. Those
     * entries already describe the difference, so reporting the directory itself would be noise.
     */
    private static boolean hasDescendants(
            TreeEntry entry, String path, NavigableMap<String, TreeEntry> otherSide) {
        if (entry.kind() != EntryKind.DIRECTORY) {
            return false;
        }
        String prefix = path + "/";
        Map.Entry<String, TreeEntry> nearest = otherSide.ceilingEntry(prefix);
        return nearest != null && nearest.getKey().startsWith(prefix);
    }

    private NavigableMap<String, TreeEntry> flatten(String treeId) throws IOException {
        NavigableMap<String, TreeEntry> entries = new TreeMap<>();
        flatten(treeId, "", entries, new HashSet<>());
        return entries;
    }

    /**
     * Collects the leaves of a tree: every file, every symbolic link, and every directory that
     * contains nothing, because an empty directory is itself part of the snapshotted state.
     */
    private void flatten(
            String treeId,
            String prefix,
            NavigableMap<String, TreeEntry> flattened,
            Set<String> ancestors)
            throws IOException {
        if (!ancestors.add(treeId)) {
            throw new IOException("Tree graph contains a cycle at " + treeId);
        }
        try {
            for (TreeEntry entry : readTree(treeId).entries()) {
                String path = prefix.isEmpty() ? entry.name() : prefix + "/" + entry.name();
                if (entry.kind() == EntryKind.DIRECTORY) {
                    int leavesBefore = flattened.size();
                    flatten(entry.objectId(), path, flattened, ancestors);
                    if (flattened.size() == leavesBefore) {
                        flattened.put(path, entry);
                    }
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
            } else {
                openExternalTarget(target, force);
            }
            verifyNamesAreRepresentable(commit.treeId(), target);

            beginRestore(commitId, target);
            clearDirectory(target, inPlace ? metadata : null);
            materializeTree(commit.treeId(), target);
            if (inPlace) {
                Files.deleteIfExists(cachePath());
            }
            endRestore();
        }
    }

    /** A restore that began but never finished, so the tree it targeted is incomplete. */
    public record InterruptedRestore(String commitId, Path target) {
    }

    /** Returns the restore that was interrupted, if one left a target half-written. */
    public Optional<InterruptedRestore> interruptedRestore() throws IOException {
        Path marker = metadata.resolve(RESTORE_MARKER);
        if (!Files.exists(marker, LinkOption.NOFOLLOW_LINKS)) {
            return Optional.empty();
        }
        List<String> lines = Files.readAllLines(marker, StandardCharsets.UTF_8);
        if (lines.size() < 2) {
            throw new IOException("A restore was interrupted and its marker is unreadable: " + marker);
        }
        return Optional.of(
                new InterruptedRestore(lines.get(0).strip(), Path.of(lines.get(1).strip())));
    }

    /**
     * Records a restore before it removes anything, and forces the record to disk. A crash between
     * clearing and materializing is otherwise indistinguishable from an empty directory.
     */
    private void beginRestore(String commitId, Path target) throws IOException {
        byte[] content = (commitId + System.lineSeparator() + target + System.lineSeparator())
                .getBytes(StandardCharsets.UTF_8);
        try (FileChannel channel = FileChannel.open(
                metadata.resolve(RESTORE_MARKER),
                StandardOpenOption.CREATE,
                StandardOpenOption.WRITE,
                StandardOpenOption.TRUNCATE_EXISTING)) {
            channel.write(ByteBuffer.wrap(content));
            channel.force(true);
        }
    }

    private void endRestore() throws IOException {
        Files.deleteIfExists(metadata.resolve(RESTORE_MARKER));
    }

    /**
     * Refuses work that a half-restored working tree would silently corrupt. Only an interrupted
     * in-place restore leaves this repository's own files incomplete; an external target does not.
     */
    private void requireCompleteWorkingTree() throws IOException {
        Optional<InterruptedRestore> interrupted = interruptedRestore();
        if (interrupted.isEmpty() || !interrupted.get().target().equals(root)) {
            return;
        }
        String commitId = interrupted.get().commitId();
        throw new IOException(
                "A restore of "
                        + commitId
                        + " was interrupted, so the working directory is incomplete; finish it with"
                        + " 'snapvault restore "
                        + commitId
                        + " --force'");
    }

    /**
     * Refuses, before anything is removed, a snapshot holding sibling names this filesystem cannot
     * keep apart. Names differing only in case or Unicode composition are distinct in a snapshot
     * but one file here, so materializing them would silently discard one.
     */
    private void verifyNamesAreRepresentable(String treeId, Path target) throws IOException {
        if (distinguishesNameCase(target)) {
            return;
        }
        verifyNamesAreRepresentable(treeId, "", new HashSet<>());
    }

    private void verifyNamesAreRepresentable(String treeId, String prefix, Set<String> checked)
            throws IOException {
        if (!checked.add(treeId)) {
            return;
        }
        Map<String, String> byFoldedName = new HashMap<>();
        for (TreeEntry entry : readTree(treeId).entries()) {
            String folded =
                    Normalizer.normalize(entry.name(), Normalizer.Form.NFC).toLowerCase(Locale.ROOT);
            String clashing = byFoldedName.put(folded, entry.name());
            if (clashing != null) {
                throw new IOException(
                        "This filesystem cannot keep \""
                                + clashing
                                + "\" and \""
                                + entry.name()
                                + "\" apart in "
                                + (prefix.isEmpty() ? "the snapshot root" : prefix)
                                + "; restore on a case-sensitive filesystem instead");
            }
            if (entry.kind() == EntryKind.DIRECTORY) {
                String path = prefix.isEmpty() ? entry.name() : prefix + "/" + entry.name();
                verifyNamesAreRepresentable(entry.objectId(), path, checked);
            }
        }
    }

    /** Probes whether {@code directory} keeps names that differ only by case apart. */
    private static boolean distinguishesNameCase(Path directory) throws IOException {
        Path probe = Files.createTempFile(directory, "snapvault-probe-", ".tmp");
        try {
            String upper = probe.getFileName().toString().toUpperCase(Locale.ROOT);
            return !Files.exists(directory.resolve(upper), LinkOption.NOFOLLOW_LINKS);
        } finally {
            Files.deleteIfExists(probe);
        }
    }

    private boolean isWorkingTreeDirty() throws IOException {
        String workingTree = scan(root, "", hashingSink(), new TreeMap<>(), new ArrayList<>());
        Optional<String> current = head();
        if (current.isEmpty()) {
            return !workingTree.equals(ObjectId.of(ObjectType.TREE, new Tree(List.of()).encode()));
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
        if (target.startsWith(root)) {
            throw new IOException(
                    "Restore target is inside the repository and would be captured by the next"
                            + " snapshot; choose a directory outside " + root);
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

    private static void openExternalTarget(Path target, boolean force) throws IOException {
        if (!Files.exists(target, LinkOption.NOFOLLOW_LINKS)) {
            Files.createDirectories(target);
            return;
        }
        if (!Files.isDirectory(target, LinkOption.NOFOLLOW_LINKS)) {
            throw new IOException("Restore target is not a directory: " + target);
        }
        if (force) {
            return;
        }
        try (DirectoryStream<Path> children = Files.newDirectoryStream(target)) {
            if (children.iterator().hasNext()) {
                throw new IOException("Restore target is not empty; rerun with --force");
            }
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
            if (Files.exists(destination, LinkOption.NOFOLLOW_LINKS)) {
                throw new IOException(
                        "Two entries in this snapshot resolve to the same file: " + destination);
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

    /** Reads the working tree without writing anything, so diffing never grows the object store. */
    private NavigableMap<String, TreeEntry> scanWorkingTree() throws IOException {
        NavigableMap<String, TreeEntry> leaves = new TreeMap<>();
        scan(root, "", hashingSink(), leaves, new ArrayList<>());
        return leaves;
    }

    /**
     * Walks {@code directory}, hashes regular files on a bounded worker pool, feeds what it finds
     * to {@code sink}, records every leaf in {@code leaves}, and returns the id of the tree object
     * describing it. {@code files} receives the regular-file entries so snapshot can persist a
     * working-tree cache.
     */
    private String scan(
            Path directory,
            String prefix,
            TreeSink sink,
            NavigableMap<String, TreeEntry> leaves,
            List<PendingEntry> files)
            throws IOException {
        PendingDir pending = walk(directory, prefix, sink, files);
        hashFiles(files, sink);
        return assemble(pending, sink, leaves);
    }

    private PendingDir walk(
            Path directory, String prefix, TreeSink sink, List<PendingEntry> files)
            throws IOException {
        List<Path> children = new ArrayList<>();
        try (DirectoryStream<Path> stream = Files.newDirectoryStream(directory)) {
            for (Path child : stream) {
                if (child.equals(metadata) || isRepositoryMetadata(child)) {
                    continue;
                }
                children.add(child);
            }
        }
        children.sort(Comparator.comparing(path -> path.getFileName().toString()));

        PendingDir result = new PendingDir();
        for (Path child : children) {
            BasicFileAttributes before = Files.readAttributes(
                    child, BasicFileAttributes.class, LinkOption.NOFOLLOW_LINKS);
            String name = child.getFileName().toString();
            String path = prefix.isEmpty() ? name : prefix + "/" + name;
            PendingEntry entry = new PendingEntry();
            entry.name = name;
            entry.relPath = path;
            entry.absPath = child;
            if (before.isSymbolicLink()) {
                byte[] target = Files.readSymbolicLink(child)
                        .toString()
                        .getBytes(StandardCharsets.UTF_8);
                entry.kind = EntryKind.SYMLINK;
                entry.objectId = sink.symlinkTarget(target);
            } else if (before.isDirectory()) {
                entry.kind = EntryKind.DIRECTORY;
                entry.child = walk(child, path, sink, files);
            } else if (before.isRegularFile()) {
                long[] identity = fileIdentity(child);
                entry.kind = EntryKind.FILE;
                entry.executable = Files.isExecutable(child);
                entry.size = before.size();
                entry.mtime = before.lastModifiedTime();
                entry.mtimeNano = entry.mtime.to(TimeUnit.NANOSECONDS);
                entry.dev = identity[0];
                entry.ino = identity[1];
                files.add(entry);
            } else {
                throw new IOException("Unsupported filesystem entry: " + child);
            }
            result.entries.add(entry);
        }
        return result;
    }

    private void hashFiles(List<PendingEntry> files, TreeSink sink) throws IOException {
        if (files.isEmpty()) {
            return;
        }
        DirCache cache = DirCache.load(cachePath());
        int n = workerCount(files.size());
        if (n == 1) {
            for (PendingEntry file : files) {
                hashOne(file, sink, cache);
            }
            return;
        }
        ExecutorService pool = Executors.newFixedThreadPool(n);
        try {
            List<Future<?>> futures = new ArrayList<>(files.size());
            for (PendingEntry file : files) {
                futures.add(pool.submit(() -> {
                    try {
                        hashOne(file, sink, cache);
                    } catch (IOException exception) {
                        throw new UncheckedIOException(exception);
                    }
                }));
            }
            IOException first = null;
            for (Future<?> future : futures) {
                try {
                    future.get();
                } catch (InterruptedException exception) {
                    Thread.currentThread().interrupt();
                    throw new IOException("Snapshot interrupted", exception);
                } catch (ExecutionException exception) {
                    Throwable cause = exception.getCause();
                    IOException io = cause instanceof UncheckedIOException uio
                            ? uio.getCause()
                            : new IOException(cause);
                    if (first == null) {
                        first = io;
                    }
                }
            }
            if (first != null) {
                throw first;
            }
        } finally {
            pool.shutdownNow();
        }
    }

    private void hashOne(PendingEntry entry, TreeSink sink, DirCache cache) throws IOException {
        String cached = cache.lookup(
                entry.relPath,
                entry.size,
                entry.mtimeNano,
                entry.dev,
                entry.ino,
                objectStore::contains);
        if (cached != null) {
            BasicFileAttributes after = Files.readAttributes(
                    entry.absPath, BasicFileAttributes.class, LinkOption.NOFOLLOW_LINKS);
            if (sameStat(entry, after)) {
                entry.objectId = cached;
                return;
            }
        }
        String objectId = sink.blob(entry.absPath);
        BasicFileAttributes after = Files.readAttributes(
                entry.absPath, BasicFileAttributes.class, LinkOption.NOFOLLOW_LINKS);
        if (!sameStat(entry, after)) {
            throw new IOException("File changed while snapshotting: " + entry.absPath);
        }
        entry.objectId = objectId;
    }

    private String assemble(
            PendingDir dir, TreeSink sink, NavigableMap<String, TreeEntry> leaves)
            throws IOException {
        List<TreeEntry> entries = new ArrayList<>();
        for (PendingEntry pending : dir.entries) {
            TreeEntry entry;
            if (pending.kind == EntryKind.DIRECTORY) {
                int leavesBefore = leaves.size();
                String objectId = assemble(pending.child, sink, leaves);
                entry = new TreeEntry(pending.name, EntryKind.DIRECTORY, objectId, false);
                if (leaves.size() == leavesBefore) {
                    leaves.put(pending.relPath, entry);
                }
            } else {
                entry = new TreeEntry(
                        pending.name, pending.kind, pending.objectId, pending.executable);
                leaves.put(pending.relPath, entry);
            }
            entries.add(entry);
        }
        return sink.tree(new Tree(entries));
    }

    private Path cachePath() {
        return metadata.resolve(DirCache.FILE_NAME);
    }

    private int workerCount(int fileCount) {
        int n = workers > 0 ? workers : Runtime.getRuntime().availableProcessors();
        return Math.max(1, Math.min(n, fileCount));
    }

    private static List<DirCache.Entry> toCacheEntries(List<PendingEntry> files) {
        List<DirCache.Entry> entries = new ArrayList<>(files.size());
        for (PendingEntry file : files) {
            entries.add(new DirCache.Entry(
                    file.relPath,
                    file.size,
                    file.mtimeNano,
                    file.dev,
                    file.ino,
                    file.objectId));
        }
        return entries;
    }

    private static long[] fileIdentity(Path path) {
        try {
            Map<String, Object> attributes =
                    Files.readAttributes(path, "unix:dev,ino", LinkOption.NOFOLLOW_LINKS);
            Number dev = (Number) attributes.get("dev");
            Number ino = (Number) attributes.get("ino");
            return new long[] {
                dev == null ? 0L : dev.longValue(), ino == null ? 0L : ino.longValue()
            };
        } catch (UnsupportedOperationException | IOException ignored) {
            return new long[] {0L, 0L};
        }
    }

    private static boolean sameStat(PendingEntry entry, BasicFileAttributes after) {
        if (!after.isRegularFile()
                || after.size() != entry.size
                || !after.lastModifiedTime().equals(entry.mtime)) {
            return false;
        }
        long[] identity = fileIdentity(entry.absPath);
        if (entry.dev != 0 && identity[0] != 0 && entry.dev != identity[0]) {
            return false;
        }
        if (entry.ino != 0 && identity[1] != 0 && entry.ino != identity[1]) {
            return false;
        }
        return true;
    }

    private static final class PendingDir {
        private final List<PendingEntry> entries = new ArrayList<>();
    }

    private static final class PendingEntry {
        private String name;
        private EntryKind kind;
        private boolean executable;
        private String objectId;
        private PendingDir child;
        private String relPath;
        private Path absPath;
        private long size;
        private FileTime mtime;
        private long mtimeNano;
        private long dev;
        private long ino;
    }

    /** Reports whether {@code child} is the metadata directory of a SnapVault repository. */
    private static boolean isRepositoryMetadata(Path child) {
        return child.getFileName().toString().equals(METADATA_DIRECTORY)
                && Files.isRegularFile(child.resolve("format"), LinkOption.NOFOLLOW_LINKS);
    }

    /** Persists everything a scan discovers, for {@code snapshot}. */
    private TreeSink storingSink() {
        return new TreeSink() {
            @Override
            public String blob(Path file) throws IOException {
                return objectStore.putBlob(file);
            }

            @Override
            public String symlinkTarget(byte[] target) throws IOException {
                return objectStore.put(ObjectType.BLOB, target);
            }

            @Override
            public String tree(Tree tree) throws IOException {
                return objectStore.put(ObjectType.TREE, tree.encode());
            }
        };
    }

    /**
     * Addresses everything a scan discovers without persisting it, for {@code diff} and the
     * dirty-working-tree check. Ids match {@link #storingSink} exactly, because both hash the same
     * canonical envelope.
     */
    private static TreeSink hashingSink() {
        return new TreeSink() {
            @Override
            public String blob(Path file) throws IOException {
                return ObjectId.ofBlob(file);
            }

            @Override
            public String symlinkTarget(byte[] target) {
                return ObjectId.of(ObjectType.BLOB, target);
            }

            @Override
            public String tree(Tree tree) throws IOException {
                return ObjectId.of(ObjectType.TREE, tree.encode());
            }
        };
    }

    /** Where a working-tree scan sends the content it finds. */
    private interface TreeSink {
        String blob(Path file) throws IOException;

        String symlinkTarget(byte[] target) throws IOException;

        String tree(Tree tree) throws IOException;
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
