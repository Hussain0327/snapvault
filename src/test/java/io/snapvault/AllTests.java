package io.snapvault;

import io.snapvault.cli.Cli;
import io.snapvault.core.ChangeType;
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
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.LinkOption;
import java.nio.file.Path;
import java.time.Clock;
import java.time.Instant;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

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
        run("diff reports live and snapshot changes", this::diffReportsChanges);
        run("restore protects dirty work and preserves history", this::restoreIsSafe);
        run("restore validates objects before changing files", this::restorePreflightsObjects);
        run("symlinks survive a snapshot round trip", this::symlinkRoundTrip);
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
            assertContains(output, "Restored ", "restore output");
            assertEquals("", stderr.toString(StandardCharsets.UTF_8), "successful CLI should not use stderr");
        } finally {
            deleteTree(temporary);
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
