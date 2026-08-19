package io.snapvault.core;

import io.snapvault.hash.Sha256;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.EOFException;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.AtomicMoveNotSupportedException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.function.Predicate;

/**
 * Optional working-tree cache used to skip hashing unchanged regular files.
 *
 * <p>The file is not part of object identity. A missing or corrupt cache is treated as empty.
 * Java and Go write the same big-endian {@code SVDC} layout so either CLI can reuse the other's
 * cache after a snapshot.
 */
public final class DirCache {
    public static final String FILE_NAME = "cache";

    private static final int MAGIC = 0x53564443; // SVDC
    private static final int VERSION = 1;
    private static final int MAX_ENTRIES = 1_000_000;
    private static final int MAX_PATH_BYTES = 1_048_576;

    private final long writtenAt;
    private final Map<String, Entry> byPath;

    public record Entry(
            String path, long size, long mtimeNano, long dev, long ino, String objectId) {}

    private DirCache(long writtenAt, Map<String, Entry> byPath) {
        this.writtenAt = writtenAt;
        this.byPath = Map.copyOf(byPath);
    }

    /** Loads {@code file}, or an empty cache if it is missing or not a valid SVDC file. */
    public static DirCache load(Path file) {
        Objects.requireNonNull(file, "file");
        try {
            if (!Files.isRegularFile(file)) {
                return empty();
            }
            return decode(Files.readAllBytes(file));
        } catch (IOException ignored) {
            return empty();
        }
    }

    public static DirCache empty() {
        return new DirCache(0L, Map.of());
    }

    /**
     * Returns a cached blob id when the path, size, mtime, and device/inode still match, the blob
     * still exists, and the mtime is older than the cache write time (racy-clean).
     */
    public String lookup(
            String path,
            long size,
            long mtimeNano,
            long dev,
            long ino,
            Predicate<String> contains) {
        Entry entry = byPath.get(path);
        if (entry == null) {
            return null;
        }
        if (entry.size != size || entry.mtimeNano != mtimeNano) {
            return null;
        }
        if (entry.dev != 0 && dev != 0 && entry.dev != dev) {
            return null;
        }
        if (entry.ino != 0 && ino != 0 && entry.ino != ino) {
            return null;
        }
        if (mtimeNano >= writtenAt) {
            return null;
        }
        if (contains == null || !contains.test(entry.objectId)) {
            return null;
        }
        return entry.objectId;
    }

    /** Atomically replaces the cache file with {@code entries} stamped at the current time. */
    public static void write(Path destination, List<Entry> entries) throws IOException {
        Objects.requireNonNull(destination, "destination");
        Instant now = Instant.now();
        long writtenAt = now.getEpochSecond() * 1_000_000_000L + now.getNano();
        byte[] raw = encode(writtenAt, entries);
        Path parent = destination.getParent();
        Path temporary = Files.createTempFile(parent, ".cache-", ".tmp");
        try {
            Files.write(temporary, raw);
            try {
                Files.move(
                        temporary,
                        destination,
                        StandardCopyOption.ATOMIC_MOVE,
                        StandardCopyOption.REPLACE_EXISTING);
            } catch (AtomicMoveNotSupportedException exception) {
                Files.move(temporary, destination, StandardCopyOption.REPLACE_EXISTING);
            }
            temporary = null;
        } finally {
            if (temporary != null) {
                Files.deleteIfExists(temporary);
            }
        }
    }

    public static byte[] encode(long writtenAt, List<Entry> entries) throws IOException {
        List<Entry> sorted = new ArrayList<>(entries);
        sorted.sort(Comparator.comparing(Entry::path));
        ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        try (DataOutputStream output = new DataOutputStream(bytes)) {
            output.writeInt(MAGIC);
            output.writeInt(VERSION);
            output.writeLong(writtenAt);
            output.writeInt(sorted.size());
            for (Entry entry : sorted) {
                byte[] path = entry.path.getBytes(StandardCharsets.UTF_8);
                output.writeInt(path.length);
                output.write(path);
                output.writeLong(entry.size);
                output.writeLong(entry.mtimeNano);
                output.writeLong(entry.dev);
                output.writeLong(entry.ino);
                output.write(Sha256.bytes(entry.objectId));
            }
        }
        return bytes.toByteArray();
    }

    public static DirCache decode(byte[] raw) throws IOException {
        Objects.requireNonNull(raw, "raw");
        ByteArrayInputStream bytes = new ByteArrayInputStream(raw);
        try (DataInputStream input = new DataInputStream(bytes)) {
            if (input.readInt() != MAGIC) {
                throw new IOException("Invalid working-tree cache signature");
            }
            if (input.readInt() != VERSION) {
                throw new IOException("Unsupported working-tree cache version");
            }
            long writtenAt = input.readLong();
            int count = input.readInt();
            if (count < 0 || count > MAX_ENTRIES) {
                throw new IOException("Invalid working-tree cache entry count: " + count);
            }
            Map<String, Entry> byPath = new HashMap<>();
            for (int index = 0; index < count; index++) {
                int pathLen = input.readInt();
                if (pathLen < 0 || pathLen > MAX_PATH_BYTES) {
                    throw new IOException("Invalid working-tree cache path length");
                }
                byte[] pathBytes = input.readNBytes(pathLen);
                if (pathBytes.length != pathLen) {
                    throw new EOFException("Truncated working-tree cache path");
                }
                String path = new String(pathBytes, StandardCharsets.UTF_8);
                long size = input.readLong();
                long mtimeNano = input.readLong();
                long dev = input.readLong();
                long ino = input.readLong();
                byte[] objectId = input.readNBytes(Sha256.BYTE_LENGTH);
                if (objectId.length != Sha256.BYTE_LENGTH) {
                    throw new EOFException("Truncated working-tree cache object id");
                }
                byPath.put(
                        path,
                        new Entry(path, size, mtimeNano, dev, ino, Sha256.hex(objectId)));
            }
            if (bytes.available() != 0) {
                throw new IOException("Trailing garbage in working-tree cache");
            }
            return new DirCache(writtenAt, byPath);
        }
    }
}
