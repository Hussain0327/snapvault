package io.snapvault;

import io.snapvault.cli.Cli;
import io.snapvault.core.ChangeType;
import io.snapvault.core.DirCache;
import io.snapvault.core.FileChange;
import io.snapvault.core.Repository;
import io.snapvault.hash.Sha256;
import io.snapvault.model.Commit;
import io.snapvault.model.EntryKind;
import io.snapvault.model.Tree;
import io.snapvault.model.TreeEntry;
import io.snapvault.store.FileObjectStore;
import io.snapvault.store.ObjectType;
import io.snapvault.store.StoredObject;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.OutputStream;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.LinkOption;
import java.nio.file.Path;
import java.nio.file.attribute.FileTime;
import java.nio.file.attribute.PosixFilePermission;
import java.time.Clock;
import java.time.Instant;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashMap;
import java.util.HexFormat;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.zip.DeflaterOutputStream;

/** Minimal dependency-free test runner used by {@code make test}. */
public final class AllTests {
    private int passed;

    private AllTests() {
    }

    public static void main(String[] arguments) throws Exception {
        new AllTests().runAll();
    }

    private void runAll() throws Exception {
        run("object store deduplicates and verifies SHA-256", this::objectStoreDeduplicates);
        run("snapshots form a parent-linked commit graph", this::snapshotsFormCommitGraph);
        run(
                "working-tree cache skips hashing unchanged files",
                this::workingTreeCacheSkipsUnchangedFiles);
        run("working-tree cache golden bytes round-trip", this::workingTreeCacheGoldenRoundTrips);
        run("worker count does not change snapshot ids", this::workerCountDoesNotChangeSnapshotIds);
        run("diff reports live and snapshot changes", this::diffReportsChanges);
        run("restore protects dirty work and preserves history", this::restoreIsSafe);
        run("restore validates objects before changing files", this::restorePreflightsObjects);
        run("symlinks survive a snapshot round trip", this::symlinkRoundTrip);
        run("diff sees empty directories", this::diffSeesEmptyDirectories);
        run("working-tree diff writes no objects", this::workingTreeDiffWritesNoObjects);
        run("chained ancestor revisions resolve", this::chainedAncestorRevisionsResolve);
        run("filesystem errors explain themselves", this::filesystemErrorsExplainThemselves);
        run("restore refuses names the filesystem cannot represent", this::restoreRefusesUnrepresentableNames);
        run("oversized objects are rejected before allocating", this::oversizedObjectsAreRejected);
        run("nested repositories are not captured", this::nestedRepositoriesAreNotCaptured);
        run("restore refuses targets inside the repository", this::restoreRefusesInternalTargets);
        run("an interrupted restore blocks snapshot and diff", this::interruptedRestoreBlocksWork);
        run("CLI supports init snapshot log diff and restore", this::cliEndToEnd);
        System.out.println();
        System.out.println(passed + " tests passed");
    }

    private void run(String name, ThrowingRunnable test) throws Exception {
        test.run();
        passed++;
        System.out.println("PASS " + name);
    }

    private void objectStoreDeduplicates() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-store-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);
            byte[] payload = "identical content".getBytes(StandardCharsets.UTF_8);

            String first = store.put(ObjectType.BLOB, payload);
            String second = store.put(ObjectType.BLOB, payload);
            assertEquals(first, second, "identical content must have one id");
            assertEquals(1L, store.count(), "duplicate writes must create one object");

            var digest = Sha256.newDigest();
            digest.update(("blob " + payload.length + "\0").getBytes(StandardCharsets.US_ASCII));
            digest.update(payload);
            assertEquals(Sha256.hex(digest.digest()), first, "id must hash the canonical envelope");

            StoredObject restored = store.get(first);
            assertEquals(ObjectType.BLOB, restored.type(), "stored object type");
            assertArrayEquals(payload, restored.payload(), "stored object payload");

            Path objectPath = objects.resolve(first.substring(0, 2)).resolve(first.substring(2));
            Files.write(objectPath, "corrupt".getBytes(StandardCharsets.UTF_8));
            assertThrows(IOException.class, () -> store.get(first), "corruption must be detected");
        } finally {
            deleteTree(temporary);
        }
    }

    private void snapshotsFormCommitGraph() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-graph-test-");
        try {
            Path root = temporary.resolve("ordinary-directory");
            Files.createDirectories(root.resolve("nested"));
            Files.writeString(root.resolve("alpha.txt"), "same bytes");
            Files.writeString(root.resolve("nested/beta.txt"), "same bytes");

            Repository repository = Repository.init(root).withClock(fixed("2026-08-10T12:00:00Z"));
            String first = repository.snapshot("baseline");
            Commit firstCommit = repository.readCommit(first);
            Map<String, TreeEntry> firstFiles = flatten(repository, firstCommit.treeId());

            assertEquals(
                    firstFiles.get("alpha.txt").objectId(),
                    firstFiles.get("nested/beta.txt").objectId(),
                    "equal files in different paths must share a blob");
            assertEquals(List.of(), firstCommit.parents(), "first snapshot has no parent");

            long objectsAfterFirst = repository.objectCount();
            repository = repository.withClock(fixed("2026-08-10T12:01:00Z"));
            String second = repository.snapshot("unchanged checkpoint");
            Commit secondCommit = repository.readCommit(second);

            assertEquals(List.of(first), secondCommit.parents(), "second snapshot points to first");
            assertEquals(firstCommit.treeId(), secondCommit.treeId(), "unchanged tree is deduplicated");
            assertEquals(
                    objectsAfterFirst + 1,
                    repository.objectCount(),
                    "an unchanged snapshot writes only a new commit");
            assertEquals(second, repository.head().orElseThrow(), "HEAD advances atomically");
            assertEquals(first, repository.resolveCommit("HEAD~1"), "HEAD~1 resolves through parent");
            assertEquals(
                    List.of(second, first),
                    repository.history("HEAD", 10).stream().map(info -> info.objectId()).toList(),
                    "history follows the graph in parent order");
        } finally {
            deleteTree(temporary);
        }
    }

    private void workingTreeCacheSkipsUnchangedFiles() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-cache-test-");
        try {
            Path root = temporary.resolve("work");
            Files.createDirectories(root);
            Path file = root.resolve("a.txt");
            Files.writeString(file, "hello");
            Files.setLastModifiedTime(file, FileTime.from(Instant.now().minusSeconds(2)));

            Repository repository = Repository.init(root).withClock(fixed("2026-08-10T12:00:00Z"));
            repository.snapshot("first");
            assertTrue(
                    Files.isRegularFile(root.resolve(".snapvault").resolve(DirCache.FILE_NAME)),
                    "snapshot must write a working-tree cache");

            Files.setPosixFilePermissions(file, Set.of());
            repository = repository.withClock(fixed("2026-08-10T12:01:00Z"));
            repository.snapshot("second");

            Files.setPosixFilePermissions(
                    file,
                    Set.of(PosixFilePermission.OWNER_READ, PosixFilePermission.OWNER_WRITE));
            Files.writeString(file, "changed");
            String third = repository.withClock(fixed("2026-08-10T12:02:00Z")).snapshot("third");
            Commit thirdCommit = repository.readCommit(third);
            String second = repository.resolveCommit("HEAD~1");
            assertTrue(
                    !repository.readCommit(second).treeId().equals(thirdCommit.treeId()),
                    "content change must produce a new tree");

            Path cache = root.resolve(".snapvault").resolve(DirCache.FILE_NAME);
            Files.write(cache, "corrupt".getBytes(StandardCharsets.UTF_8));
            repository.withClock(fixed("2026-08-10T12:03:00Z")).snapshot("after corrupt cache");

            Files.writeString(file, "dirty");
            repository.restore("HEAD", null, true);
            assertFalse(
                    Files.exists(root.resolve(".snapvault").resolve(DirCache.FILE_NAME)),
                    "in-place restore must delete the cache");
        } finally {
            deleteTree(temporary);
        }
    }

    private void workingTreeCacheGoldenRoundTrips() throws Exception {
        String hex = "535644430000000117979cfe362a00000000000100000005612e747874"
                + "000000000000000317940f7f9163800000000000000000010000000000000002"
                + "abababababababababababababababababababababababababababababababab";
        byte[] raw = HexFormat.of().parseHex(hex);
        byte[] encoded = DirCache.encode(
                1_700_000_000_000_000_000L,
                List.of(new DirCache.Entry(
                        "a.txt",
                        3,
                        1_699_000_000_000_000_000L,
                        1,
                        2,
                        "abababababababababababababababababababababababababababababababab")));
        assertArrayEquals(raw, encoded, "Java encode must match the shared SVDC golden");
        DirCache cache = DirCache.decode(raw);
        assertEquals(
                "abababababababababababababababababababababababababababababababab",
                cache.lookup("a.txt", 3, 1_699_000_000_000_000_000L, 1, 2, id -> true),
                "golden cache must look up the recorded blob id");
    }

    private void workerCountDoesNotChangeSnapshotIds() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-workers-test-");
        try {
            Path oneRoot = temporary.resolve("one");
            Path eightRoot = temporary.resolve("eight");
            Files.createDirectories(oneRoot.resolve("nested"));
            Files.createDirectories(eightRoot.resolve("nested"));
            Files.writeString(oneRoot.resolve("a.txt"), "alpha");
            Files.writeString(eightRoot.resolve("a.txt"), "alpha");
            Files.writeString(oneRoot.resolve("nested/b.txt"), "beta");
            Files.writeString(eightRoot.resolve("nested/b.txt"), "beta");

            Repository one = Repository.init(oneRoot)
                    .withClock(fixed("2026-08-10T12:00:00Z"))
                    .withWorkers(1);
            Repository eight = Repository.init(eightRoot)
                    .withClock(fixed("2026-08-10T12:00:00Z"))
                    .withWorkers(8);
            String oneId = one.snapshot("same");
            String eightId = eight.snapshot("same");
            assertEquals(
                    one.readCommit(oneId).treeId(),
                    eight.readCommit(eightId).treeId(),
                    "tree id must not depend on the hashing worker count");
        } finally {
            deleteTree(temporary);
        }
    }

    private void diffReportsChanges() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-diff-test-");
        try {
            Path root = temporary.resolve("files");
            Files.createDirectories(root);
            Files.writeString(root.resolve("modify.txt"), "before");
            Files.writeString(root.resolve("delete.txt"), "remove me");

            Repository repository = Repository.init(root);
            String first = repository.snapshot("before changes");
            Files.writeString(root.resolve("modify.txt"), "after");
            Files.delete(root.resolve("delete.txt"));
            Files.writeString(root.resolve("add.txt"), "new");

            assertChangeTypes(
                    repository.diffWorking("HEAD"),
                    Map.of(
                            "add.txt", ChangeType.ADDED,
                            "delete.txt", ChangeType.DELETED,
                            "modify.txt", ChangeType.MODIFIED));

            String second = repository.snapshot("after changes");
            assertChangeTypes(
                    repository.diff(first, second),
                    Map.of(
                            "add.txt", ChangeType.ADDED,
                            "delete.txt", ChangeType.DELETED,
                            "modify.txt", ChangeType.MODIFIED));
            assertEquals(List.of(), repository.diffWorkingFromHead(), "HEAD should now be clean");
        } finally {
            deleteTree(temporary);
        }
    }

    private void restoreIsSafe() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-restore-test-");
        try {
            Path root = temporary.resolve("source");
            Files.createDirectories(root);
            Files.writeString(root.resolve("document.txt"), "version one");
            Repository repository = Repository.init(root);
            String first = repository.snapshot("version one");

            Files.writeString(root.resolve("document.txt"), "version two");
            Files.writeString(root.resolve("new.txt"), "created later");
            String second = repository.snapshot("version two");
            Files.writeString(root.resolve("document.txt"), "unsnapshotted work");

            assertThrows(
                    IOException.class,
                    () -> repository.restore(first, null, false),
                    "in-place restore must protect dirty work");
            assertEquals(
                    "unsnapshotted work",
                    Files.readString(root.resolve("document.txt")),
                    "refused restore must not touch files");

            repository.restore(first, null, true);
            assertEquals("version one", Files.readString(root.resolve("document.txt")), "old file restored");
            assertFalse(Files.exists(root.resolve("new.txt")), "later file removed during exact restore");
            assertTrue(Files.isDirectory(root.resolve(".snapvault")), "repository metadata survives");
            assertEquals(second, repository.head().orElseThrow(), "restore does not rewrite history");

            Path external = temporary.resolve("exported-copy");
            repository.restore(second, external, false);
            assertEquals("version two", Files.readString(external.resolve("document.txt")), "external restore");
            assertEquals("created later", Files.readString(external.resolve("new.txt")), "external tree complete");

            Files.writeString(external.resolve("sentinel.txt"), "keep unless forced");
            assertThrows(
                    IOException.class,
                    () -> repository.restore(first, external, false),
                    "non-empty external target needs force");
            assertTrue(Files.exists(external.resolve("sentinel.txt")), "refused external restore is intact");
            repository.restore(first, external, true);
            assertFalse(Files.exists(external.resolve("sentinel.txt")), "forced restore replaces target tree");
        } finally {
            deleteTree(temporary);
        }
    }

    private void restorePreflightsObjects() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-corruption-test-");
        try {
            Path root = temporary.resolve("source");
            Files.createDirectories(root);
            Files.writeString(root.resolve("important.txt"), "snapshotted");
            Repository repository = Repository.init(root);
            String commitId = repository.snapshot("safe copy");
            String treeId = repository.readCommit(commitId).treeId();
            String blobId = flatten(repository, treeId).get("important.txt").objectId();

            Files.writeString(root.resolve("important.txt"), "live sentinel");
            Path objectPath = repository.metadata()
                    .resolve("objects")
                    .resolve(blobId.substring(0, 2))
                    .resolve(blobId.substring(2));
            Files.writeString(objectPath, "damaged object");

            assertThrows(
                    IOException.class,
                    () -> repository.restore(commitId, null, true),
                    "restore must fail before clearing when an object is corrupt");
            assertEquals(
                    "live sentinel",
                    Files.readString(root.resolve("important.txt")),
                    "preflight failure must leave live data untouched");
        } finally {
            deleteTree(temporary);
        }
    }

    private void symlinkRoundTrip() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-symlink-test-");
        try {
            Path root = temporary.resolve("source");
            Files.createDirectories(root);
            Path targetFile = root.resolve("target.txt");
            Files.writeString(targetFile, "target");
            boolean executableBitsSupported = targetFile.toFile().setExecutable(true, false)
                    && Files.isExecutable(targetFile);
            Path link = root.resolve("shortcut");
            try {
                Files.createSymbolicLink(link, Path.of("target.txt"));
            } catch (UnsupportedOperationException | IOException exception) {
                System.out.println("SKIP symlink creation is unavailable on this filesystem");
                return;
            }

            Repository repository = Repository.init(root);
            repository.snapshot("with symlink");
            Files.delete(link);
            targetFile.toFile().setExecutable(false, false);
            repository.restore("HEAD", null, true);
            assertTrue(Files.isSymbolicLink(link), "restored entry is a symlink");
            assertEquals(Path.of("target.txt"), Files.readSymbolicLink(link), "symlink target round trips");
            if (executableBitsSupported) {
                assertTrue(Files.isExecutable(targetFile), "executable bit round trips");
            }
        } finally {
            deleteTree(temporary);
        }
    }

    private void cliEndToEnd() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-cli-test-");
        try {
            ByteArrayOutputStream stdout = new ByteArrayOutputStream();
            ByteArrayOutputStream stderr = new ByteArrayOutputStream();
            try (PrintStream out = new PrintStream(stdout, true, StandardCharsets.UTF_8);
                    PrintStream err = new PrintStream(stderr, true, StandardCharsets.UTF_8)) {
                Cli cli = new Cli(out, err, temporary);
                assertEquals(0, cli.run("init", "notes"), "CLI init exit code");
                Path root = temporary.resolve("notes");
                Files.writeString(root.resolve("todo.txt"), "first draft");
                assertEquals(
                        0,
                        cli.run("-C", "notes", "snapshot", "-m", "initial notes"),
                        "CLI snapshot exit code");
                assertEquals(0, cli.run("-C", "notes", "log", "--oneline"), "CLI log exit code");

                Files.writeString(root.resolve("todo.txt"), "edited");
                Files.createDirectory(root.resolve("scratch"));
                assertEquals(0, cli.run("-C", "notes", "diff"), "CLI diff exit code");
                assertEquals(
                        0,
                        cli.run("-C", "notes", "restore", "HEAD", "--force"),
                        "CLI restore exit code");
                assertEquals("first draft", Files.readString(root.resolve("todo.txt")), "CLI restored file");
            }

            String output = stdout.toString(StandardCharsets.UTF_8);
            assertContains(output, "Initialized empty SnapVault repository", "init output");
            assertContains(output, "Snapshot ", "snapshot output");
            assertContains(output, "initial notes", "log output");
            assertContains(output, "M\ttodo.txt", "diff output");
            assertContains(output, "A\tscratch/", "directories are marked in diff output");
            assertContains(output, "Restored ", "restore output");
            assertEquals("", stderr.toString(StandardCharsets.UTF_8), "successful CLI should not use stderr");
        } finally {
            deleteTree(temporary);
        }
    }

    private void diffSeesEmptyDirectories() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-empty-directory-test-");
        try {
            Path root = temporary.resolve("files");
            Files.createDirectories(root);
            Files.writeString(root.resolve("keep.txt"), "content");
            Repository repository = Repository.init(root);
            repository.snapshot("baseline");

            Path scratch = root.resolve("scratch");
            Files.createDirectory(scratch);
            List<FileChange> added = repository.diffWorkingFromHead();
            assertChangeTypes(added, Map.of("scratch", ChangeType.ADDED));
            assertEquals(
                    EntryKind.DIRECTORY,
                    added.getFirst().after().kind(),
                    "an empty directory is reported as a directory");

            repository.snapshot("with an empty directory");
            assertEquals(List.of(), repository.diffWorkingFromHead(), "the snapshot clears the change");
            repository.restore("HEAD", null, false);

            Files.delete(scratch);
            assertChangeTypes(repository.diffWorkingFromHead(), Map.of("scratch", ChangeType.DELETED));

            Files.createDirectory(scratch);
            Files.writeString(scratch.resolve("later.txt"), "a file appears");
            assertChangeTypes(
                    repository.diffWorkingFromHead(), Map.of("scratch/later.txt", ChangeType.ADDED));
        } finally {
            deleteTree(temporary);
        }
    }

    private void workingTreeDiffWritesNoObjects() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-read-only-diff-test-");
        try {
            Path root = temporary.resolve("files");
            Files.createDirectories(root);
            Files.writeString(root.resolve("tracked.txt"), "content");
            Repository repository = Repository.init(root);
            repository.snapshot("baseline");
            Files.writeString(root.resolve("untracked.txt"), "not snapshotted yet");

            long before = repository.objectCount();
            repository.diffWorkingFromHead();
            repository.diffWorking("HEAD");
            assertEquals(before, repository.objectCount(), "diff must not write objects");

            assertThrows(
                    IOException.class,
                    () -> repository.restore("HEAD", null, false),
                    "restore must still refuse a dirty working tree");
            assertEquals(
                    before, repository.objectCount(), "a refused restore must not write objects");
        } finally {
            deleteTree(temporary);
        }
    }

    private void chainedAncestorRevisionsResolve() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-revision-test-");
        try {
            Path root = temporary.resolve("files");
            Files.createDirectories(root);
            Repository repository = Repository.init(root);
            Files.writeString(root.resolve("one.txt"), "one");
            repository.snapshot("one");
            Files.writeString(root.resolve("two.txt"), "two");
            repository.snapshot("two");
            Files.writeString(root.resolve("three.txt"), "three");
            repository.snapshot("three");

            assertEquals(
                    repository.resolveCommit("HEAD~1"),
                    repository.resolveCommit("HEAD~"),
                    "a bare tilde means one generation");
            assertEquals(
                    repository.resolveCommit("HEAD~2"),
                    repository.resolveCommit("HEAD~1~1"),
                    "chained tildes accumulate");
        } finally {
            deleteTree(temporary);
        }
    }

    private void filesystemErrorsExplainThemselves() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-error-message-test-");
        try {
            Path root = temporary.resolve("files");
            Files.createDirectories(root);
            Path unreadable = root.resolve("locked.txt");
            Files.writeString(unreadable, "secret");
            if (!unreadable.toFile().setReadable(false, false) || Files.isReadable(unreadable)) {
                System.out.println("SKIP unreadable files cannot be simulated for this user");
                return;
            }

            ByteArrayOutputStream stderr = new ByteArrayOutputStream();
            try (PrintStream out =
                            new PrintStream(OutputStream.nullOutputStream(), true, StandardCharsets.UTF_8);
                    PrintStream err = new PrintStream(stderr, true, StandardCharsets.UTF_8)) {
                Cli cli = new Cli(out, err, root);
                assertEquals(0, cli.run("init"), "CLI init exit code");
                assertEquals(1, cli.run("snapshot", "-m", "locked"), "CLI snapshot must fail");
            }
            assertContains(
                    stderr.toString(StandardCharsets.UTF_8),
                    "permission denied",
                    "an unreadable file must explain why the snapshot failed");
        } finally {
            deleteTree(temporary);
        }
    }

    private void restoreRefusesUnrepresentableNames() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-case-clash-test-");
        try {
            Path root = temporary.resolve("source");
            Files.createDirectories(root);
            Files.writeString(root.resolve("ordinary.txt"), "content");
            Repository repository = Repository.init(root);
            repository.snapshot("baseline");

            // A snapshot authored on a case-sensitive filesystem can legitimately hold two sibling
            // names that differ only by case. Build one directly, the way such a repository arrives.
            FileObjectStore store = new FileObjectStore(repository.metadata().resolve("objects"));
            String lower = store.put(ObjectType.BLOB, "lower".getBytes(StandardCharsets.UTF_8));
            String upper = store.put(ObjectType.BLOB, "UPPER".getBytes(StandardCharsets.UTF_8));
            Tree tree = new Tree(List.of(
                    new TreeEntry("clash.txt", EntryKind.FILE, lower, false),
                    new TreeEntry("CLASH.txt", EntryKind.FILE, upper, false)));
            Commit commit = new Commit(
                    store.put(ObjectType.TREE, tree.encode()),
                    List.of(),
                    Instant.parse("2026-08-12T00:00:00Z"),
                    "case clash");
            String commitId = store.put(ObjectType.COMMIT, commit.encode());
            Path external = temporary.resolve("export");

            if (isCaseSensitive(temporary)) {
                repository.restore(commitId, external, false);
                assertEquals("lower", Files.readString(external.resolve("clash.txt")), "lower entry");
                assertEquals("UPPER", Files.readString(external.resolve("CLASH.txt")), "upper entry");
            } else {
                assertThrows(
                        IOException.class,
                        () -> repository.restore(commitId, external, false),
                        "this filesystem cannot represent both entries");
                assertFalse(
                        Files.exists(external.resolve("CLASH.txt")),
                        "the refusal must come before anything is written");
            }
        } finally {
            deleteTree(temporary);
        }
    }

    private void oversizedObjectsAreRejected() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-oversize-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);
            String objectId = "aa" + "0".repeat(61) + "1";
            Path shard = objects.resolve(objectId.substring(0, 2));
            Files.createDirectories(shard);

            byte[] header =
                    ("tree " + (300L * 1024 * 1024) + "\0").getBytes(StandardCharsets.US_ASCII);
            try (OutputStream file = Files.newOutputStream(shard.resolve(objectId.substring(2)));
                    DeflaterOutputStream compressed = new DeflaterOutputStream(file)) {
                compressed.write(header);
                compressed.write(new byte[1024]);
            }

            IOException failure = null;
            try {
                store.get(objectId);
            } catch (IOException exception) {
                failure = exception;
            }
            assertTrue(failure != null, "an implausible declared payload size must fail");
            assertContains(
                    failure.getMessage(),
                    "implausible",
                    "the header must be rejected before the payload is buffered");
        } finally {
            deleteTree(temporary);
        }
    }

    private void nestedRepositoriesAreNotCaptured() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-nested-test-");
        try {
            Path root = temporary.resolve("outer");
            Files.createDirectories(root);
            Files.writeString(root.resolve("outer.txt"), "outer content");
            Repository outer = Repository.init(root);
            outer.snapshot("baseline");

            Path inner = root.resolve("inner");
            Files.createDirectories(inner);
            Files.writeString(inner.resolve("note.txt"), "inner content");
            Repository.init(inner);

            assertChangeTypes(
                    outer.diffWorkingFromHead(), Map.of("inner/note.txt", ChangeType.ADDED));
        } finally {
            deleteTree(temporary);
        }
    }

    private void restoreRefusesInternalTargets() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-internal-target-test-");
        try {
            Path root = temporary.resolve("source");
            Files.createDirectories(root.resolve("sub"));
            Files.writeString(root.resolve("a.txt"), "a");
            Files.writeString(root.resolve("sub/b.txt"), "b");
            Repository repository = Repository.init(root);
            String commitId = repository.snapshot("baseline");

            assertThrows(
                    IOException.class,
                    () -> repository.restore(commitId, root.resolve("sub"), true),
                    "restore must refuse a target inside the repository");
            assertEquals(
                    "b", Files.readString(root.resolve("sub/b.txt")), "the refusal must not touch files");
        } finally {
            deleteTree(temporary);
        }
    }

    private void interruptedRestoreBlocksWork() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-interrupted-test-");
        try {
            Path root = temporary.resolve("source");
            Files.createDirectories(root);
            Files.writeString(root.resolve("a.txt"), "a");
            Repository repository = Repository.init(root);
            String commitId = repository.snapshot("baseline");

            Path marker = repository.metadata().resolve("restore-in-progress");
            Files.writeString(
                    marker,
                    commitId + System.lineSeparator() + repository.root() + System.lineSeparator());

            assertThrows(
                    IOException.class,
                    () -> repository.snapshot("after a crash"),
                    "snapshot must refuse an incomplete working tree");
            assertThrows(
                    IOException.class,
                    repository::diffWorkingFromHead,
                    "diff must refuse an incomplete working tree");
            assertEquals(commitId, repository.head().orElseThrow(), "history stays readable");

            repository.restore(commitId, null, true);
            assertFalse(Files.exists(marker), "a completed restore clears the marker");
            repository.snapshot("recovered");
        } finally {
            deleteTree(temporary);
        }
    }

    private static boolean isCaseSensitive(Path directory) throws IOException {
        Path probe = Files.createTempFile(directory, "case-probe-", ".tmp");
        try {
            String upper = probe.getFileName().toString().toUpperCase(Locale.ROOT);
            return !Files.exists(directory.resolve(upper), LinkOption.NOFOLLOW_LINKS);
        } finally {
            Files.deleteIfExists(probe);
        }
    }

    private static Map<String, TreeEntry> flatten(Repository repository, String treeId)
            throws IOException {
        Map<String, TreeEntry> result = new HashMap<>();
        flatten(repository, treeId, "", result);
        return result;
    }

    private static void flatten(
            Repository repository,
            String treeId,
            String prefix,
            Map<String, TreeEntry> result)
            throws IOException {
        Tree tree = repository.readTree(treeId);
        for (TreeEntry entry : tree.entries()) {
            String path = prefix.isEmpty() ? entry.name() : prefix + "/" + entry.name();
            if (entry.kind() == EntryKind.DIRECTORY) {
                flatten(repository, entry.objectId(), path, result);
            } else {
                result.put(path, entry);
            }
        }
    }

    private static void assertChangeTypes(List<FileChange> changes, Map<String, ChangeType> expected) {
        Map<String, ChangeType> actual = new HashMap<>();
        for (FileChange change : changes) {
            actual.put(change.path(), change.type());
        }
        assertEquals(expected, actual, "change set");
    }

    private static Clock fixed(String instant) {
        return Clock.fixed(Instant.parse(instant), ZoneOffset.UTC);
    }

    private static void deleteTree(Path root) throws IOException {
        if (!Files.exists(root, LinkOption.NOFOLLOW_LINKS)) {
            return;
        }
        List<Path> paths = new ArrayList<>();
        try (var stream = Files.walk(root)) {
            stream.sorted(Comparator.reverseOrder()).forEach(paths::add);
        }
        IOException failure = null;
        for (Path path : paths) {
            try {
                Files.deleteIfExists(path);
            } catch (IOException exception) {
                if (failure == null) {
                    failure = exception;
                } else {
                    failure.addSuppressed(exception);
                }
            }
        }
        if (failure != null) {
            throw failure;
        }
    }

    private static void assertThrows(
            Class<? extends Throwable> expected, ThrowingRunnable action, String message)
            throws Exception {
        try {
            action.run();
        } catch (Throwable throwable) {
            if (expected.isInstance(throwable)) {
                return;
            }
            throw new AssertionError(
                    message + ": expected " + expected.getName() + ", got " + throwable, throwable);
        }
        throw new AssertionError(message + ": expected " + expected.getName());
    }

    private static void assertTrue(boolean condition, String message) {
        if (!condition) {
            throw new AssertionError(message);
        }
    }

    private static void assertFalse(boolean condition, String message) {
        assertTrue(!condition, message);
    }

    private static void assertContains(String text, String expected, String message) {
        if (!text.contains(expected)) {
            throw new AssertionError(message + ": expected to find <" + expected + "> in <" + text + ">");
        }
    }

    private static void assertArrayEquals(byte[] expected, byte[] actual, String message) {
        if (!java.util.Arrays.equals(expected, actual)) {
            throw new AssertionError(message);
        }
    }

    private static void assertEquals(Object expected, Object actual, String message) {
        if (!java.util.Objects.equals(expected, actual)) {
            throw new AssertionError(
                    message + ": expected <" + expected + "> but was <" + actual + ">");
        }
    }

    @FunctionalInterface
    private interface ThrowingRunnable {
        void run() throws Exception;
    }
}
