package io.snapvault.store;

import io.snapvault.hash.Sha256;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.AtomicMoveNotSupportedException;
import java.nio.file.DirectoryStream;
import java.nio.file.FileAlreadyExistsException;
import java.nio.file.Files;
import java.nio.file.LinkOption;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.security.MessageDigest;
import java.security.DigestInputStream;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.Locale;
import java.util.Objects;
import java.util.zip.DeflaterOutputStream;
import java.util.zip.InflaterInputStream;

/**
 * A Git-style filesystem object database.
 *
 * <p>Each object id is the SHA-256 digest of {@code "type size\\0" + payload}. The canonical
 * envelope is zlib-compressed on disk and sharded by the first two hex digits of the id.</p>
 */
public final class FileObjectStore implements ObjectStore {
    private static final int BUFFER_SIZE = 64 * 1024;
    private static final int MAX_HEADER_BYTES = 128;

    /**
     * Ceiling on an object read whole into memory. Far above any legitimate tree or commit,
     * and far below what a deliberately or accidentally corrupt header could otherwise make
     * this process allocate. Blobs are streamed and are not subject to it.
     */
    private static final long MAX_INLINE_PAYLOAD = 256L * 1024 * 1024;

    private final Path objectsDirectory;

    public FileObjectStore(Path objectsDirectory) throws IOException {
        this.objectsDirectory = Objects.requireNonNull(objectsDirectory, "objectsDirectory")
                .toAbsolutePath()
                .normalize();
        Files.createDirectories(this.objectsDirectory);
    }

    @Override
    public String put(ObjectType type, byte[] payload) throws IOException {
        Objects.requireNonNull(type, "type");
        Objects.requireNonNull(payload, "payload");
        return writeObject(type, payload.length, new ByteArrayInputStream(payload));
    }

    @Override
    public String putBlob(Path source) throws IOException {
        Objects.requireNonNull(source, "source");
        long expectedSize = Files.size(source);
        try (InputStream input = Files.newInputStream(source)) {
            return writeObject(ObjectType.BLOB, expectedSize, input);
        }
    }

    private String writeObject(ObjectType type, long payloadSize, InputStream payload)
            throws IOException {
        if (payloadSize < 0) {
            throw new IllegalArgumentException("Payload size cannot be negative");
        }

        Files.createDirectories(objectsDirectory);
        Path temporary = Files.createTempFile(objectsDirectory, "tmp-", ".object");
        MessageDigest digest = Sha256.newDigest();
        byte[] header = ObjectId.header(type, payloadSize);
        long copied = 0;

        try {
            try (OutputStream fileOutput = Files.newOutputStream(temporary);
                    DeflaterOutputStream compressed = new DeflaterOutputStream(fileOutput)) {
                digest.update(header);
                compressed.write(header);

                byte[] buffer = new byte[BUFFER_SIZE];
                int read;
                while ((read = payload.read(buffer)) != -1) {
                    digest.update(buffer, 0, read);
                    compressed.write(buffer, 0, read);
                    copied += read;
                }
            }

            if (copied != payloadSize) {
                throw new IOException(
                        "File changed while it was being stored (expected "
                                + payloadSize
                                + " bytes, read "
                                + copied
                                + ")");
            }

            String objectId = Sha256.hex(digest.digest());
            Path destination = pathFor(objectId);
            if (Files.exists(destination, LinkOption.NOFOLLOW_LINKS)) {
                if (!Files.isRegularFile(destination, LinkOption.NOFOLLOW_LINKS)) {
                    throw new IOException("Object path is not a regular file: " + destination);
                }
                return objectId;
            }

            Files.createDirectories(destination.getParent());
            moveWithoutReplacing(temporary, destination);
            temporary = null;
            return objectId;
        } finally {
            if (temporary != null) {
                Files.deleteIfExists(temporary);
            }
        }
    }

    private static void moveWithoutReplacing(Path source, Path destination) throws IOException {
        try {
            Files.move(source, destination, StandardCopyOption.ATOMIC_MOVE);
        } catch (AtomicMoveNotSupportedException exception) {
            try {
                Files.move(source, destination);
            } catch (FileAlreadyExistsException ignored) {
                Files.deleteIfExists(source);
            }
        } catch (FileAlreadyExistsException ignored) {
            Files.deleteIfExists(source);
        }
    }

    @Override
    public StoredObject get(String objectId) throws IOException {
        ByteArrayOutputStream payload = new ByteArrayOutputStream();
        ObjectType type = copyVerified(objectId, null, payload, MAX_INLINE_PAYLOAD);
        return new StoredObject(type, payload.toByteArray());
    }

    @Override
    public void copyPayload(
            String objectId, ObjectType expectedType, OutputStream destination) throws IOException {
        Objects.requireNonNull(expectedType, "expectedType");
        Objects.requireNonNull(destination, "destination");
        copyVerified(objectId, expectedType, destination, Long.MAX_VALUE);
    }

    private ObjectType copyVerified(
            String objectId,
            ObjectType expectedType,
            OutputStream destination,
            long maximumPayloadSize)
            throws IOException {
        Path objectPath = pathFor(objectId);
        if (!Files.isRegularFile(objectPath, LinkOption.NOFOLLOW_LINKS)) {
            throw new IOException("Object does not exist: " + objectId);
        }

        MessageDigest digest = Sha256.newDigest();
        Header parsed;
        try (InputStream fileInput = Files.newInputStream(objectPath);
                InflaterInputStream decompressed = new InflaterInputStream(fileInput);
                DigestInputStream canonical = new DigestInputStream(decompressed, digest)) {
            parsed = readHeader(canonical);
            if (parsed.payloadSize() > maximumPayloadSize) {
                throw new IOException(
                        "Object "
                                + objectId
                                + " declares an implausible payload size: "
                                + parsed.payloadSize());
            }
            if (expectedType != null && parsed.type() != expectedType) {
                throw new IOException(
                        "Object "
                                + objectId
                                + " is "
                                + parsed.type().token()
                                + ", expected "
                                + expectedType.token());
            }

            copyExactly(canonical, destination, parsed.payloadSize());
            if (canonical.read() != -1) {
                throw new IOException("Object has trailing data: " + objectId);
            }
        } catch (java.util.zip.ZipException exception) {
            throw new IOException("Object is corrupt: " + objectId, exception);
        }

        String actualId = Sha256.hex(digest.digest());
        if (!actualId.equals(objectId)) {
            throw new IOException(
                    "Object failed its SHA-256 integrity check: "
                            + objectId
                            + " (actual "
                            + actualId
                            + ")");
        }

        return parsed.type();
    }

    private static Header readHeader(InputStream input) throws IOException {
        ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        for (int index = 0; index < MAX_HEADER_BYTES; index++) {
            int next = input.read();
            if (next == -1) {
                throw new IOException("Truncated object header");
            }
            if (next == 0) {
                String header = bytes.toString(StandardCharsets.US_ASCII);
                int separator = header.indexOf(' ');
                if (separator <= 0 || separator == header.length() - 1) {
                    throw new IOException("Malformed object header");
                }
                ObjectType type = ObjectType.fromToken(header.substring(0, separator));
                try {
                    long size = Long.parseLong(header.substring(separator + 1));
                    if (size < 0) {
                        throw new IOException("Negative object size");
                    }
                    return new Header(type, size);
                } catch (NumberFormatException exception) {
                    throw new IOException("Malformed object size", exception);
                }
            }
            bytes.write(next);
        }
        throw new IOException("Object header is too long");
    }

    private static void copyExactly(InputStream source, OutputStream destination, long bytes)
            throws IOException {
        byte[] buffer = new byte[BUFFER_SIZE];
        long remaining = bytes;
        while (remaining > 0) {
            int requested = (int) Math.min(buffer.length, remaining);
            int read = source.read(buffer, 0, requested);
            if (read == -1) {
                throw new IOException("Truncated object payload");
            }
            destination.write(buffer, 0, read);
            remaining -= read;
        }
    }

    @Override
    public boolean contains(String objectId) {
        try {
            return Files.isRegularFile(pathFor(objectId), LinkOption.NOFOLLOW_LINKS);
        } catch (IllegalArgumentException exception) {
            return false;
        }
    }

    @Override
    public List<String> findByPrefix(String prefix) throws IOException {
        Objects.requireNonNull(prefix, "prefix");
        String normalized = prefix.toLowerCase(Locale.ROOT);
        if (normalized.length() < 2 || normalized.length() > Sha256.HEX_LENGTH) {
            throw new IllegalArgumentException("Object prefix must contain 2 to 64 hex characters");
        }
        for (int index = 0; index < normalized.length(); index++) {
            char character = normalized.charAt(index);
            if (!((character >= '0' && character <= '9')
                    || (character >= 'a' && character <= 'f'))) {
                throw new IllegalArgumentException("Object prefix must be hexadecimal");
            }
        }

        String directoryPrefix = normalized.substring(0, 2);
        String filePrefix = normalized.substring(2);
        Path shard = objectsDirectory.resolve(directoryPrefix);
        if (!Files.isDirectory(shard)) {
            return List.of();
        }

        List<String> matches = new ArrayList<>();
        try (DirectoryStream<Path> files = Files.newDirectoryStream(shard)) {
            for (Path file : files) {
                String name = file.getFileName().toString();
                if (name.length() == Sha256.HEX_LENGTH - 2
                        && name.startsWith(filePrefix)
                        && Files.isRegularFile(file, LinkOption.NOFOLLOW_LINKS)) {
                    matches.add(directoryPrefix + name);
                }
            }
        }
        matches.sort(Comparator.naturalOrder());
        return List.copyOf(matches);
    }

    @Override
    public long count() throws IOException {
        if (!Files.isDirectory(objectsDirectory)) {
            return 0;
        }
        long total = 0;
        try (DirectoryStream<Path> shards = Files.newDirectoryStream(objectsDirectory)) {
            for (Path shard : shards) {
                String shardName = shard.getFileName().toString();
                if (shardName.length() != 2 || !Files.isDirectory(shard)) {
                    continue;
                }
                try (DirectoryStream<Path> files = Files.newDirectoryStream(shard)) {
                    for (Path file : files) {
                        if (file.getFileName().toString().length() == Sha256.HEX_LENGTH - 2
                                && Files.isRegularFile(file, LinkOption.NOFOLLOW_LINKS)) {
                            total++;
                        }
                    }
                }
            }
        }
        return total;
    }

    private Path pathFor(String objectId) {
        Sha256.requireObjectId(objectId);
        return objectsDirectory.resolve(objectId.substring(0, 2)).resolve(objectId.substring(2));
    }

    private record Header(ObjectType type, long payloadSize) {
    }
}
