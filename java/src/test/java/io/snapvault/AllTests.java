package io.snapvault;

import io.airlift.compress.zstd.ZstdOutputStream;
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
import io.snapvault.store.GoldenDeltaVectors;
import io.snapvault.store.ObjectId;
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
import java.util.Arrays;
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
        run("container full objects round trip through zlib", this::containerFullZlibRoundTrips);
        run("container full objects round trip through zstd", this::containerFullZstdRoundTrips);
        run("a delta reconstructs the FORMAT.md worked example", this::deltaWorkedExampleReconstructsTarget);
        run("a delta copy instruction cannot read past the base", this::deltaRejectsOutOfBoundsCopy);
        run("a delta stream cannot end mid-instruction", this::deltaRejectsTruncatedInstructions);
        run("the reserved delta opcode 0x00 is rejected", this::deltaRejectsReservedOpcodeZero);
        run("a delta's declared source size must match its base", this::deltaRejectsSourceSizeMismatch);
        run("a delta's reconstructed output must match its declared target size",
                this::deltaRejectsTargetSizeMismatch);
        run("multi-byte varints in a delta header decode correctly", this::deltaHandlesMultiByteVarintSizes);
        run("a delta copy size of zero means 65536 bytes", this::deltaZeroSizeCopyMeans65536Bytes);
        run("two deltas that base off each other are rejected as a cycle", this::deltaCycleIsRejected);
        run("a delta chain deeper than 32 hops is rejected", this::deltaChainDepthCapIsEnforced);
        run("legacy and container objects coexist in a v2 repository",
                this::mixedLegacyAndContainerObjectsInV2Repo);
        run("a repository bumped to format 2 keeps working", this::formatTwoRepositoryOpens);
        run("a malformed v2 container is rejected as corrupt", this::malformedContainerIsRejected);
        run("a container object in a format 1 store is rejected",
                this::containerObjectRejectedInFormatOneStore);
        run("a repository opened at format 1 rejects container objects",
                this::repositoryOpenedAtFormatOneRejectsContainerObjects);
        run("a corrupt zstd container yields a corrupt-object error, not a crash",
                this::corruptZstdContainerYieldsCorruptObjectError);
        run("a delta against a legacy base uses its raw header bytes",
                this::deltaAgainstLegacyBaseUsesRawHeaderBytes);
        run("a zstd container rejects multi-frame, skippable, and trailing-garbage streams",
                this::zstdContainerRejectsMultiFrameSkippableAndTrailingGarbage);
        run("the shared v2 delta golden vectors all apply to their targets",
                this::deltaGoldenVectorsApplyToTarget);
        run("the shared v2 delta reject vectors are all refused",
                this::deltaGoldenVectorsRejectMalformed);
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

    private void containerFullZlibRoundTrips() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-container-full-zlib-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);
            byte[] payload = "container payload, stored whole".getBytes(StandardCharsets.UTF_8);
            String objectId = ObjectId.of(ObjectType.BLOB, payload);
            writeFullContainer(
                    objectPath(objects, objectId), CODEC_ZLIB, canonicalBytes(ObjectType.BLOB, payload));

            StoredObject restored = store.get(objectId);
            assertEquals(ObjectType.BLOB, restored.type(), "container/full/zlib object type");
            assertArrayEquals(payload, restored.payload(), "container/full/zlib object payload");
        } finally {
            deleteTree(temporary);
        }
    }

    private void containerFullZstdRoundTrips() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-container-full-zstd-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);
            byte[] payload = "container payload, stored whole via zstd".getBytes(StandardCharsets.UTF_8);
            String objectId = ObjectId.of(ObjectType.BLOB, payload);
            writeFullContainer(
                    objectPath(objects, objectId), CODEC_ZSTD, canonicalBytes(ObjectType.BLOB, payload));

            StoredObject restored = store.get(objectId);
            assertEquals(ObjectType.BLOB, restored.type(), "container/full/zstd object type");
            assertArrayEquals(payload, restored.payload(), "container/full/zstd object payload");
        } finally {
            deleteTree(temporary);
        }
    }

    private void deltaWorkedExampleReconstructsTarget() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-delta-worked-example-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);

            // The exact base, target, and instruction bytes from FORMAT.md's worked example.
            byte[] basePayload = "hello world\n".getBytes(StandardCharsets.UTF_8);
            String baseId = store.put(ObjectType.BLOB, basePayload);
            assertEquals(
                    "0bd69098bd9b9cc5934a610ab65da429b525361147faa7b5b922919e9a23143d",
                    baseId,
                    "the worked example's base id is a golden value from FORMAT.md");

            byte[] instructions = {
                0x14, 0x15, 0x08, 0x62, 0x6c, 0x6f, 0x62, 0x20, 0x31, 0x33, 0x00, (byte) 0x91, 0x08,
                0x0b, 0x02, 0x73, 0x0a,
            };
            String targetId = ObjectId.of(ObjectType.BLOB, "hello worlds\n".getBytes(StandardCharsets.UTF_8));
            writeDeltaContainer(objectPath(objects, targetId), CODEC_ZLIB, baseId, instructions);

            StoredObject restored = store.get(targetId);
            assertEquals(ObjectType.BLOB, restored.type(), "worked-example delta target type");
            assertEquals(
                    "hello worlds\n",
                    new String(restored.payload(), StandardCharsets.UTF_8),
                    "worked-example delta target payload");
        } finally {
            deleteTree(temporary);
        }
    }

    private void deltaRejectsOutOfBoundsCopy() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-delta-out-of-bounds-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);
            byte[] basePayload = "hello world\n".getBytes(StandardCharsets.UTF_8);
            String baseId = store.put(ObjectType.BLOB, basePayload);
            long baseEnvelopeSize = canonicalBytes(ObjectType.BLOB, basePayload).length;

            // Copies 20 bytes from a base whose canonical bytes are far shorter than that.
            byte[] instructions = deltaInstructions(baseEnvelopeSize, 20, copyOp1(0, 20));
            String targetId = ObjectId.of(ObjectType.BLOB, "irrelevant, never reached".getBytes(StandardCharsets.UTF_8));
            writeDeltaContainer(objectPath(objects, targetId), CODEC_ZLIB, baseId, instructions);

            assertThrows(
                    IOException.class,
                    () -> store.get(targetId),
                    "a copy reaching past the base must be rejected");
        } finally {
            deleteTree(temporary);
        }
    }

    private void deltaRejectsTruncatedInstructions() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-delta-truncated-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);
            byte[] basePayload = "hello world\n".getBytes(StandardCharsets.UTF_8);
            String baseId = store.put(ObjectType.BLOB, basePayload);
            long baseEnvelopeSize = canonicalBytes(ObjectType.BLOB, basePayload).length;

            // An insert opcode claiming 5 literal bytes, with only 2 actually present.
            byte[] whole = deltaInstructions(baseEnvelopeSize, 5, insertOp(new byte[] {'a', 'b', 'c', 'd', 'e'}));
            byte[] truncated = Arrays.copyOf(whole, whole.length - 3);
            String targetId = ObjectId.of(ObjectType.BLOB, "irrelevant, never reached".getBytes(StandardCharsets.UTF_8));
            writeDeltaContainer(objectPath(objects, targetId), CODEC_ZLIB, baseId, truncated);

            assertThrows(
                    IOException.class,
                    () -> store.get(targetId),
                    "a delta stream that ends mid-instruction must be rejected");
        } finally {
            deleteTree(temporary);
        }
    }

    private void deltaRejectsReservedOpcodeZero() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-delta-opcode-zero-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);
            byte[] basePayload = "hello world\n".getBytes(StandardCharsets.UTF_8);
            String baseId = store.put(ObjectType.BLOB, basePayload);
            long baseEnvelopeSize = canonicalBytes(ObjectType.BLOB, basePayload).length;

            byte[] instructions = deltaInstructions(baseEnvelopeSize, 0, new byte[] {0x00});
            String targetId = ObjectId.of(ObjectType.BLOB, "irrelevant, never reached".getBytes(StandardCharsets.UTF_8));
            writeDeltaContainer(objectPath(objects, targetId), CODEC_ZLIB, baseId, instructions);

            assertThrows(
                    IOException.class,
                    () -> store.get(targetId),
                    "opcode 0x00 must always be rejected");
        } finally {
            deleteTree(temporary);
        }
    }

    private void deltaRejectsSourceSizeMismatch() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-delta-src-size-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);
            byte[] basePayload = "hello world\n".getBytes(StandardCharsets.UTF_8);
            String baseId = store.put(ObjectType.BLOB, basePayload);

            // Declares a source size that does not match the base's actual canonical size.
            byte[] instructions = deltaInstructions(5, 0);
            String targetId = ObjectId.of(ObjectType.BLOB, "irrelevant, never reached".getBytes(StandardCharsets.UTF_8));
            writeDeltaContainer(objectPath(objects, targetId), CODEC_ZLIB, baseId, instructions);

            assertThrows(
                    IOException.class,
                    () -> store.get(targetId),
                    "a mismatched srcSize must be rejected before any instruction runs");
        } finally {
            deleteTree(temporary);
        }
    }

    private void deltaRejectsTargetSizeMismatch() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-delta-tgt-size-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);
            byte[] basePayload = "hello world\n".getBytes(StandardCharsets.UTF_8);
            String baseId = store.put(ObjectType.BLOB, basePayload);
            long baseEnvelopeSize = canonicalBytes(ObjectType.BLOB, basePayload).length;

            // Declares tgtSize=5 but the instructions only ever produce 3 bytes of output.
            byte[] instructions = deltaInstructions(baseEnvelopeSize, 5, insertOp(new byte[] {'a', 'b', 'c'}));
            String targetId = ObjectId.of(ObjectType.BLOB, "irrelevant, never reached".getBytes(StandardCharsets.UTF_8));
            writeDeltaContainer(objectPath(objects, targetId), CODEC_ZLIB, baseId, instructions);

            assertThrows(
                    IOException.class,
                    () -> store.get(targetId),
                    "output that stops short of tgtSize must be rejected");
        } finally {
            deleteTree(temporary);
        }
    }

    private void deltaHandlesMultiByteVarintSizes() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-delta-multibyte-varint-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);

            // 200 bytes of payload makes both srcSize and tgtSize (209) require a two-byte varint.
            byte[] basePayload = "x".repeat(200).getBytes(StandardCharsets.UTF_8);
            String baseId = store.put(ObjectType.BLOB, basePayload);
            byte[] baseEnvelope = canonicalBytes(ObjectType.BLOB, basePayload);

            byte[] targetPayload = ("x".repeat(199) + "y").getBytes(StandardCharsets.UTF_8);
            String targetId = ObjectId.of(ObjectType.BLOB, targetPayload);

            // Copy everything but the base's last payload byte, then insert the replacement.
            byte[] instructions = deltaInstructions(
                    baseEnvelope.length,
                    baseEnvelope.length,
                    copyOp1(0, baseEnvelope.length - 1),
                    insertOp(new byte[] {'y'}));
            writeDeltaContainer(objectPath(objects, targetId), CODEC_ZLIB, baseId, instructions);

            StoredObject restored = store.get(targetId);
            assertArrayEquals(targetPayload, restored.payload(), "multi-byte-varint delta payload");
        } finally {
            deleteTree(temporary);
        }
    }

    private void deltaZeroSizeCopyMeans65536Bytes() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-delta-zero-size-copy-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);

            byte[] basePayload = "x".repeat(65536).getBytes(StandardCharsets.UTF_8);
            String baseId = store.put(ObjectType.BLOB, basePayload);
            byte[] baseEnvelope = canonicalBytes(ObjectType.BLOB, basePayload);
            int baseHeaderLength = baseEnvelope.length - basePayload.length;

            // Target reuses the whole 65536-byte base payload, addressed via the size-omitted
            // "0 means 65536" rule, plus one appended byte the base does not have.
            byte[] targetPayload = new byte[basePayload.length + 1];
            System.arraycopy(basePayload, 0, targetPayload, 0, basePayload.length);
            targetPayload[targetPayload.length - 1] = 'y';
            String targetId = ObjectId.of(ObjectType.BLOB, targetPayload);
            byte[] targetHeader = canonicalHeader(ObjectType.BLOB, targetPayload.length);

            byte[] instructions = deltaInstructions(
                    baseEnvelope.length,
                    targetHeader.length + targetPayload.length,
                    insertOp(targetHeader),
                    copyOpImplied65536(baseHeaderLength),
                    insertOp(new byte[] {'y'}));
            writeDeltaContainer(objectPath(objects, targetId), CODEC_ZLIB, baseId, instructions);

            StoredObject restored = store.get(targetId);
            assertArrayEquals(targetPayload, restored.payload(), "size-0-means-65536 delta payload");
        } finally {
            deleteTree(temporary);
        }
    }

    /**
     * Reads the base/delta/target triples shared with the Go and C++ suites out of
     * tests/golden/v2/delta/ (see that directory's MANIFEST.md) and asserts that applying each
     * delta to its base reproduces its target byte for byte, so all three languages' delta
     * decoders agree on the same fixtures.
     */
    private void deltaGoldenVectorsApplyToTarget() throws Exception {
        Path goldenDir = requireGoldenDeltaDir();
        for (String name : GOLDEN_DELTA_CASES) {
            byte[] base = readGoldenDeltaFixture(goldenDir, name, "base");
            byte[] delta = readGoldenDeltaFixture(goldenDir, name, "delta");
            byte[] target = readGoldenDeltaFixture(goldenDir, name, "target");
            byte[] got = GoldenDeltaVectors.apply(base, delta);
            assertArrayEquals(target, got, "golden vector " + name + " must apply to its target");
        }
    }

    /**
     * Reads the malformed base/delta pairs shared with the Go and C++ suites out of
     * tests/golden/v2/delta/reject/ (see that directory's MANIFEST.md) and asserts that applying
     * each one throws, with a message naming the specific defect rather than any IOException that
     * happened to fire.
     */
    private void deltaGoldenVectorsRejectMalformed() throws Exception {
        Path rejectDir = requireGoldenDeltaRejectDir();
        for (Map.Entry<String, String> entry : GOLDEN_DELTA_REJECT_CASES.entrySet()) {
            String name = entry.getKey();
            String wantSubstring = entry.getValue();
            byte[] base = readGoldenDeltaFixture(rejectDir, name, "base");
            byte[] delta = readGoldenDeltaFixture(rejectDir, name, "delta");
            IOException failure = captureFailure(() -> GoldenDeltaVectors.apply(base, delta));
            assertContains(
                    failure.getMessage(),
                    wantSubstring,
                    "reject vector " + name + " must be refused for the expected reason");
        }
    }

    private void deltaCycleIsRejected() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-delta-cycle-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);

            // Two ids that each name the other as their delta base. Neither payload need be
            // reachable; the cycle must be caught before any digest is even checked.
            String idA = "a".repeat(64);
            String idB = "b".repeat(64);
            byte[] instructions = deltaInstructions(0, 0);
            writeDeltaContainer(objectPath(objects, idA), CODEC_ZLIB, idB, instructions);
            writeDeltaContainer(objectPath(objects, idB), CODEC_ZLIB, idA, instructions);

            assertThrows(
                    IOException.class,
                    () -> store.get(idA),
                    "two deltas basing off each other must be rejected as a cycle");
        } finally {
            deleteTree(temporary);
        }
    }

    private void deltaChainDepthCapIsEnforced() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-delta-depth-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);

            // Level 0 is a legacy blob "a"; level k deltas against level k-1 by appending one more
            // "a", so level k's payload is "a" repeated k+1 times.
            byte[] previousPayload = "a".getBytes(StandardCharsets.UTF_8);
            String previousId = store.put(ObjectType.BLOB, previousPayload);
            byte[] previousEnvelope = canonicalBytes(ObjectType.BLOB, previousPayload);
            String depthThirtyTwoId = null;

            for (int level = 1; level <= 33; level++) {
                byte[] targetPayload = "a".repeat(level + 1).getBytes(StandardCharsets.UTF_8);
                byte[] targetHeader = canonicalHeader(ObjectType.BLOB, targetPayload.length);
                String targetId = ObjectId.of(ObjectType.BLOB, targetPayload);

                byte[] instructions = deltaInstructions(
                        previousEnvelope.length,
                        targetHeader.length + targetPayload.length,
                        insertOp(targetHeader),
                        copyOp1(previousEnvelope.length - previousPayload.length, previousPayload.length),
                        insertOp(new byte[] {'a'}));
                writeDeltaContainer(objectPath(objects, targetId), CODEC_ZLIB, previousId, instructions);

                if (level == 32) {
                    depthThirtyTwoId = targetId;
                }
                previousId = targetId;
                previousPayload = targetPayload;
                previousEnvelope = canonicalBytes(ObjectType.BLOB, targetPayload);
            }

            StoredObject atCap = store.get(depthThirtyTwoId);
            assertEquals(33, atCap.payload().length, "a chain exactly 32 deltas deep must still resolve");

            String depthThirtyThreeId = previousId;
            IOException failure = captureFailure(() -> store.get(depthThirtyThreeId));
            assertContains(failure.getMessage(), "depth", "the depth cap must name itself in the error");
        } finally {
            deleteTree(temporary);
        }
    }

    private void mixedLegacyAndContainerObjectsInV2Repo() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-mixed-v2-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);

            byte[] legacyPayload = "legacy content, written the v1 way".getBytes(StandardCharsets.UTF_8);
            String legacyId = store.put(ObjectType.BLOB, legacyPayload);

            byte[] containerPayload = "container content, written the v2 way".getBytes(StandardCharsets.UTF_8);
            String containerId = ObjectId.of(ObjectType.BLOB, containerPayload);
            writeFullContainer(
                    objectPath(objects, containerId),
                    CODEC_ZLIB,
                    canonicalBytes(ObjectType.BLOB, containerPayload));

            assertArrayEquals(legacyPayload, store.get(legacyId).payload(), "the legacy object still reads");
            assertArrayEquals(
                    containerPayload, store.get(containerId).payload(), "the container object reads too");
        } finally {
            deleteTree(temporary);
        }
    }

    private void formatTwoRepositoryOpens() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-format-two-test-");
        try {
            Path root = temporary.resolve("repo");
            Files.createDirectories(root);
            Files.writeString(root.resolve("a.txt"), "hello");
            Repository repository = Repository.init(root);
            String commitId = repository.snapshot("v1 snapshot");
            Files.writeString(repository.metadata().resolve("format"), "snapvault 2" + System.lineSeparator());

            Repository reopened = Repository.open(root);
            assertTrue(reopened.head().isPresent(), "a v2-format repository still sees its v1 history");
            Files.writeString(root.resolve("a.txt"), "changed");
            reopened.restore(commitId, null, true);
            assertEquals(
                    "hello",
                    Files.readString(root.resolve("a.txt")),
                    "a v2-format repository restores its legacy objects normally");

            Files.writeString(repository.metadata().resolve("format"), "snapvault 3" + System.lineSeparator());
            assertThrows(
                    IOException.class,
                    () -> Repository.open(root),
                    "an unrecognized future format must still be rejected");
        } finally {
            deleteTree(temporary);
        }
    }

    private void malformedContainerIsRejected() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-malformed-container-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);

            String partialMagicId = "1".repeat(64);
            Path partialMagicPath = objectPath(objects, partialMagicId);
            Files.createDirectories(partialMagicPath.getParent());
            Files.write(partialMagicPath, new byte[] {0x53, 0x56}); // "SV", truncated before "O2"
            assertThrows(
                    IOException.class,
                    () -> store.get(partialMagicId),
                    "a partial SVO2 magic must be rejected");

            String unknownKindId = "2".repeat(64);
            writeRawContainer(objectPath(objects, unknownKindId), 0x09, CODEC_ZLIB, new byte[0]);
            assertThrows(
                    IOException.class,
                    () -> store.get(unknownKindId),
                    "an unknown container kind byte must be rejected");

            String unknownCodecId = "3".repeat(64);
            writeRawContainer(objectPath(objects, unknownCodecId), 0x01, 0x09, new byte[0]);
            assertThrows(
                    IOException.class,
                    () -> store.get(unknownCodecId),
                    "an unknown container codec byte must be rejected");

            String emptyId = "4".repeat(64);
            Path emptyPath = objectPath(objects, emptyId);
            Files.createDirectories(emptyPath.getParent());
            Files.write(emptyPath, new byte[0]);
            assertThrows(IOException.class, () -> store.get(emptyId), "an empty object file must be rejected");
        } finally {
            deleteTree(temporary);
        }
    }

    /**
     * A container-form object is never legal in a format 1 repository (FORMAT.md, "Compatibility");
     * Go and C++ fsck already reject one, so Java must too rather than silently decoding it.
     */
    private void containerObjectRejectedInFormatOneStore() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-container-in-v1-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);
            store.setFormat(1);

            byte[] payload = "should never appear in a format 1 repository".getBytes(StandardCharsets.UTF_8);
            String id = ObjectId.of(ObjectType.BLOB, payload);
            writeFullContainer(objectPath(objects, id), CODEC_ZLIB, canonicalBytes(ObjectType.BLOB, payload));

            try {
                store.get(id);
                throw new AssertionError("a container object in a format 1 store must be rejected");
            } catch (IOException expected) {
                assertContains(expected.getMessage(), "format 1", "error should mention format 1");
            }
        } finally {
            deleteTree(temporary);
        }
    }

    /**
     * A repository's declared format governs every store built for it, so opening one at format 1
     * and finding a container-form object must fail the same way a directly constructed store does.
     */
    private void repositoryOpenedAtFormatOneRejectsContainerObjects() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-repo-format-one-container-test-");
        try {
            Path root = temporary.resolve("repo");
            Files.createDirectories(root);
            Files.writeString(root.resolve("a.txt"), "hello");
            Repository repository = Repository.init(root);
            repository.snapshot("v1 snapshot");

            Path objects = repository.metadata().resolve("objects");
            byte[] payload = "planted straight into a format 1 repository".getBytes(StandardCharsets.UTF_8);
            String id = ObjectId.of(ObjectType.BLOB, payload);
            writeFullContainer(objectPath(objects, id), CODEC_ZLIB, canonicalBytes(ObjectType.BLOB, payload));

            Repository reopened = Repository.open(root);
            try {
                // readTree fails on the format check inside the object store before it ever gets
                // to compare the (irrelevant here) declared type, so this exercises exactly the
                // path a real tree or blob lookup would take.
                reopened.readTree(id);
                throw new AssertionError("a container object planted in a format 1 repository must be rejected");
            } catch (IOException expected) {
                assertContains(expected.getMessage(), "format 1", "error should mention format 1");
            }
        } finally {
            deleteTree(temporary);
        }
    }

    /**
     * ZstdInputStream signals corrupt input with a RuntimeException (MalformedInputException), not
     * an IOException; the store must still surface the usual corrupt-object IOException rather than
     * letting it escape as an uncaught RuntimeException.
     */
    private void corruptZstdContainerYieldsCorruptObjectError() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-corrupt-zstd-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);

            String id = "5".repeat(64);
            writeRawContainer(objectPath(objects, id), 0x01, CODEC_ZSTD, new byte[] {0, 0, 0, 0});

            try {
                store.get(id);
                throw new AssertionError("a corrupt zstd container must be rejected");
            } catch (IOException expected) {
                assertContains(expected.getMessage(), "corrupt", "error should say the object is corrupt");
            }
        } finally {
            deleteTree(temporary);
        }
    }

    /**
     * A delta's base may itself be a legacy object whose header is not canonical (e.g. a leading
     * zero in its declared size); the delta must be applied against the header exactly as stored,
     * not a re-rendering built from the parsed type and integer size, since the id was computed
     * over -- and the digest already verified -- the raw bytes.
     */
    private void deltaAgainstLegacyBaseUsesRawHeaderBytes() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-legacy-base-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);

            // "blob 05\0hello": 13 raw bytes, with a non-canonical leading zero the decimal-size
            // envelope parser accepts anyway.
            byte[] baseCanonical = {
                'b', 'l', 'o', 'b', ' ', '0', '5', 0x00, 'h', 'e', 'l', 'l', 'o'
            };
            String baseId = Sha256.hex(Sha256.newDigest().digest(baseCanonical));
            Path basePath = objectPath(objects, baseId);
            Files.createDirectories(basePath.getParent());
            try (OutputStream out = Files.newOutputStream(basePath);
                    DeflaterOutputStream deflate = new DeflaterOutputStream(out)) {
                deflate.write(baseCanonical);
            }

            // "blob 6\0hello!": also 13 bytes, so a delta built against the raw 13-byte source
            // reconstructs it with one insert-header, one copy-payload, and one insert-suffix
            // instruction.
            byte[] targetPayload = "hello!".getBytes(StandardCharsets.UTF_8);
            byte[] targetCanonical = canonicalBytes(ObjectType.BLOB, targetPayload);
            String targetId = ObjectId.of(ObjectType.BLOB, targetPayload);

            byte[] instructions = deltaInstructions(
                    13,
                    13,
                    new byte[] {0x07, 'b', 'l', 'o', 'b', ' ', '6', 0x00},
                    new byte[] {(byte) 0x91, 0x08, 0x05},
                    new byte[] {0x01, '!'});
            writeDeltaContainer(objectPath(objects, targetId), CODEC_ZLIB, baseId, instructions);

            assertArrayEquals(targetPayload, store.get(targetId).payload(), "delta against a raw legacy header");
        } finally {
            deleteTree(temporary);
        }
    }

    /**
     * FORMAT.md requires a codec-zstd stream to be exactly one standard zstd frame with no
     * skippable frames. id is computed over the *single-frame* canonical bytes, so a rejection here
     * can only come from framing enforcement -- a second frame or trailing bytes would also fail on
     * a simple digest mismatch, which would mask whether framing itself is actually checked.
     */
    private void zstdContainerRejectsMultiFrameSkippableAndTrailingGarbage() throws Exception {
        Path temporary = Files.createTempDirectory("snapvault-zstd-framing-test-");
        try {
            Path objects = temporary.resolve("objects");
            FileObjectStore store = new FileObjectStore(objects);

            byte[] payload = "hello frames".getBytes(StandardCharsets.UTF_8);
            byte[] canonical = canonicalBytes(ObjectType.BLOB, payload);
            String id = ObjectId.of(ObjectType.BLOB, payload);
            byte[] frame = encode(CODEC_ZSTD, canonical);
            byte[] skippable = {
                0x50, 0x2a, 0x4d, 0x18, 0x04, 0x00, 0x00, 0x00, (byte) 0xde, (byte) 0xad,
                (byte) 0xbe, (byte) 0xef
            };

            Map<String, byte[]> cases = new HashMap<>();
            cases.put("twoFrames", concatBytes(frame, frame));
            cases.put("trailingGarbage", concatBytes(frame, new byte[] {0x01, 0x02, 0x03}));
            cases.put("skippableFirst", concatBytes(skippable, frame));

            for (Map.Entry<String, byte[]> testCase : cases.entrySet()) {
                Path objectFile = objectPath(objects, id);
                Files.deleteIfExists(objectFile);
                writeRawContainer(objectFile, 0x01, CODEC_ZSTD, testCase.getValue());
                try {
                    store.get(id);
                    throw new AssertionError(testCase.getKey() + ": expected a framing rejection");
                } catch (IOException expected) {
                    // expected: any corrupt-object rejection is acceptable here.
                }
            }
        } finally {
            deleteTree(temporary);
        }
    }

    private static byte[] concatBytes(byte[] first, byte[] second) {
        byte[] joined = new byte[first.length + second.length];
        System.arraycopy(first, 0, joined, 0, first.length);
        System.arraycopy(second, 0, joined, first.length, second.length);
        return joined;
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

    // --- Format v2 object container fixtures -------------------------------------------------
    //
    // These build raw "objects/aa/<62 hex>" files by hand, the same way filesystemErrorsExplain
    // Themselves and oversizedObjectsAreRejected already hand-build legacy object files: a v2
    // reader must accept exactly what FORMAT.md describes, so the fixtures are assembled from
    // that byte layout rather than through any writer.

    private static final int CODEC_ZLIB = 0x01;
    private static final int CODEC_ZSTD = 0x02;
    private static final byte[] SVO2_MAGIC = {0x53, 0x56, 0x4f, 0x32};

    // Shared cross-language v2 delta fixtures; see tests/golden/v2/delta/MANIFEST.md. Resolved
    // relative to the working directory `make test` runs this suite from (java/), the same way
    // go/internal/delta/golden_test.go resolves its goldenDir relative to its own package
    // directory.
    private static final Path GOLDEN_DELTA_DIR = Path.of("..", "tests", "golden", "v2", "delta");
    private static final List<String> GOLDEN_DELTA_CASES = List.of(
            "01-worked-example",
            "02-multi-byte-varint",
            "03-copy-65536",
            "04-insert-chain",
            "05-binary-content",
            "06-mixed-edits");

    // Shared cross-language v2 delta *negative* fixtures: malformed streams every decoder must
    // refuse. Kept in their own subdirectory of GOLDEN_DELTA_DIR (a matching .base and .delta,
    // deliberately no .target) so a reject case can never be mistaken for an accept case. The
    // expected value is a substring DeltaApplier's IOException message must contain, so this pins
    // *why* the stream is rejected and not just that it is.
    private static final Path GOLDEN_DELTA_REJECT_DIR = GOLDEN_DELTA_DIR.resolve("reject");
    private static final Map<String, String> GOLDEN_DELTA_REJECT_CASES =
            Map.ofEntries(
                    Map.entry("01-copy-past-end", "out of bounds"),
                    Map.entry("02-truncated-instruction", "ends mid-instruction"),
                    Map.entry("03-truncated-varint-header", "ends mid-instruction"),
                    Map.entry("04-reserved-opcode-zero", "reserved opcode 0x00"),
                    Map.entry("05-src-size-mismatch", "does not match base object size"),
                    Map.entry("06-tgt-size-mismatch", "delta stream produced"));

    private static Path requireGoldenDeltaDir() throws IOException {
        if (!Files.isDirectory(GOLDEN_DELTA_DIR)) {
            throw new IOException(
                    "golden delta fixtures not found at "
                            + GOLDEN_DELTA_DIR.toAbsolutePath()
                            + "; run tests via `make test` (or `make -C java test`) from the repository "
                            + "so tests/golden/v2/delta/ resolves, see that directory's MANIFEST.md");
        }
        return GOLDEN_DELTA_DIR;
    }

    private static Path requireGoldenDeltaRejectDir() throws IOException {
        if (!Files.isDirectory(GOLDEN_DELTA_REJECT_DIR)) {
            throw new IOException(
                    "golden delta reject fixtures not found at "
                            + GOLDEN_DELTA_REJECT_DIR.toAbsolutePath()
                            + "; run tests via `make test` (or `make -C java test`) from the repository "
                            + "so tests/golden/v2/delta/reject/ resolves, see that directory's MANIFEST.md");
        }
        return GOLDEN_DELTA_REJECT_DIR;
    }

    private static byte[] readGoldenDeltaFixture(Path goldenDir, String name, String extension)
            throws IOException {
        Path file = goldenDir.resolve(name + "." + extension);
        if (!Files.isRegularFile(file)) {
            throw new IOException("missing golden delta fixture: " + file.toAbsolutePath());
        }
        return Files.readAllBytes(file);
    }

    private static Path objectPath(Path objectsDirectory, String objectId) {
        return objectsDirectory.resolve(objectId.substring(0, 2)).resolve(objectId.substring(2));
    }

    private static byte[] canonicalHeader(ObjectType type, long payloadSize) {
        return (type.token() + " " + payloadSize + "\0").getBytes(StandardCharsets.US_ASCII);
    }

    private static byte[] canonicalBytes(ObjectType type, byte[] payload) {
        byte[] header = canonicalHeader(type, payload.length);
        byte[] envelope = new byte[header.length + payload.length];
        System.arraycopy(header, 0, envelope, 0, header.length);
        System.arraycopy(payload, 0, envelope, header.length, payload.length);
        return envelope;
    }

    private static byte[] encode(int codec, byte[] raw) throws IOException {
        ByteArrayOutputStream encoded = new ByteArrayOutputStream();
        if (codec == CODEC_ZSTD) {
            try (ZstdOutputStream zstd = new ZstdOutputStream(encoded)) {
                zstd.write(raw);
            }
        } else {
            try (DeflaterOutputStream deflate = new DeflaterOutputStream(encoded)) {
                deflate.write(raw);
            }
        }
        return encoded.toByteArray();
    }

    private static void writeFullContainer(Path objectFile, int codec, byte[] canonicalBytes)
            throws IOException {
        writeRawContainer(objectFile, 0x01, codec, encode(codec, canonicalBytes));
    }

    private static void writeDeltaContainer(
            Path objectFile, int codec, String baseObjectId, byte[] instructions) throws IOException {
        ByteArrayOutputStream body = new ByteArrayOutputStream();
        body.writeBytes(Sha256.bytes(baseObjectId));
        body.writeBytes(encode(codec, instructions));
        writeRawContainer(objectFile, 0x02, codec, body.toByteArray());
    }

    /** Writes a container with whatever kind/codec bytes and body the caller supplies, valid or not. */
    private static void writeRawContainer(Path objectFile, int kind, int codec, byte[] body)
            throws IOException {
        Files.createDirectories(objectFile.getParent());
        try (OutputStream out = Files.newOutputStream(objectFile)) {
            out.write(SVO2_MAGIC);
            out.write(kind);
            out.write(codec);
            out.write(body);
        }
    }

    /** Assembles a delta instruction stream: the srcSize/tgtSize varint header, then each op's bytes. */
    private static byte[] deltaInstructions(long sourceSize, long targetSize, byte[]... ops)
            throws IOException {
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        writeVarint(out, sourceSize);
        writeVarint(out, targetSize);
        for (byte[] op : ops) {
            out.writeBytes(op);
        }
        return out.toByteArray();
    }

    private static void writeVarint(ByteArrayOutputStream out, long value) {
        long remaining = value;
        while (true) {
            int sevenBits = (int) (remaining & 0x7f);
            remaining >>>= 7;
            if (remaining == 0) {
                out.write(sevenBits);
                return;
            }
            out.write(sevenBits | 0x80);
        }
    }

    /** An insert opcode (1..127) carrying its literal bytes. */
    private static byte[] insertOp(byte[] literal) {
        byte[] op = new byte[1 + literal.length];
        op[0] = (byte) literal.length;
        System.arraycopy(literal, 0, op, 1, literal.length);
        return op;
    }

    /** A copy opcode with a one-byte offset (0..255) and a one-byte size (1..255). */
    private static byte[] copyOp1(int offset, int size) {
        return new byte[] {(byte) 0x91, (byte) offset, (byte) size};
    }

    /** A copy opcode with a one-byte offset (0..255) and the size omitted, meaning 65536. */
    private static byte[] copyOpImplied65536(int offset) {
        return new byte[] {(byte) 0x81, (byte) offset};
    }

    private static IOException captureFailure(ThrowingRunnable action) throws Exception {
        try {
            action.run();
        } catch (IOException exception) {
            return exception;
        }
        throw new AssertionError("expected an IOException");
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
