package io.snapvault.store;

import io.airlift.compress.MalformedInputException;
import io.airlift.compress.zstd.ZstdInputStream;
import io.snapvault.hash.Sha256;

import java.io.BufferedInputStream;
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
import java.util.Arrays;
import java.util.Comparator;
import java.util.HashSet;
import java.util.List;
import java.util.Locale;
import java.util.Objects;
import java.util.Set;
import java.util.zip.DeflaterOutputStream;
import java.util.zip.InflaterInputStream;

/**
 * A Git-style filesystem object database.
 *
 * <p>Each object id is the SHA-256 digest of {@code "type size\\0" + payload}. The canonical
 * envelope is either zlib-compressed on disk (the legacy, format-1 form) or wrapped in a format-2
 * "SVO2" container that additionally allows zstd and delta-against-a-base storage; either way the
 * store is sharded by the first two hex digits of the id. See docs/FORMAT.md, "Format v2", for
 * the container byte layout that {@link #readContainerEnvelope} and {@link DeltaApplier}
 * implement.</p>
 */
public final class FileObjectStore implements ObjectStore {
    private static final int BUFFER_SIZE = 64 * 1024;
    private static final int MAX_HEADER_BYTES = 128;

    /**
     * Ceiling on an object read whole into memory. Far above any legitimate tree or commit,
     * and far below what a deliberately or accidentally corrupt header could otherwise make
     * this process allocate. Legacy blobs read through {@link #copyPayload} are streamed and are
     * not subject to it; every v2 container object is, per FORMAT.md's caps.
     */
    private static final long MAX_INLINE_PAYLOAD = 256L * 1024 * 1024;

    private static final byte[] CONTAINER_MAGIC = {0x53, 0x56, 0x4f, 0x32}; // "SVO2"
    private static final int KIND_FULL = 0x01;
    private static final int KIND_DELTA = 0x02;
    private static final int CODEC_ZLIB = 0x01;
    private static final int CODEC_ZSTD = 0x02;
    private static final int MAX_DELTA_DEPTH = 32;

    private final Path objectsDirectory;

    // Which repository format this store serves: 1 or 2, matching Repository's ".snapvault/format"
    // values. A container-form object is never legal in a format 1 repository (FORMAT.md,
    // "Compatibility"), so copyVerified/envelopeOf consult this before decoding one. Defaults to 2
    // (container-permissive) so a store built without an explicit format -- every existing caller
    // of this constructor, container-object tests included -- keeps behaving exactly as before;
    // {@link Repository} is the one caller that knows a store might actually serve a format 1
    // repository and calls {@link #setFormat} to say so.
    private int format = 2;

    public FileObjectStore(Path objectsDirectory) throws IOException {
        this.objectsDirectory = Objects.requireNonNull(objectsDirectory, "objectsDirectory")
                .toAbsolutePath()
                .normalize();
        Files.createDirectories(this.objectsDirectory);
    }

    /**
     * Sets which repository format this store serves (1 or 2). Reads are unaffected except that a
     * container-form object is rejected as corrupt while the format is 1, matching the rule that
     * format v2 containers never legitimately appear in a format 1 repository.
     */
    public void setFormat(int format) {
        this.format = format;
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

        // Sniff which of the two v2-legal on-disk forms this is (FORMAT.md, "Format v2"): a zlib
        // stream's first byte (the CMF byte) always has 0x08 in its low nibble, which an SVO2
        // container's magic can never produce. Legacy objects fall straight through to the
        // original streaming decode below, unchanged, so v1 repositories and large legacy blobs
        // keep behaving exactly as before; only the new container branch buffers in memory, and
        // only up to the format's own 256 MiB cap.
        if (firstByte(objectPath, objectId) != Byte.toUnsignedInt(CONTAINER_MAGIC[0])) {
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

        requireContainerFormatSupported(objectId);
        Set<String> chainStack = new HashSet<>();
        chainStack.add(objectId);
        byte[] envelope = readContainerEnvelope(objectId, objectPath, chainStack, 0);
        return finishFromEnvelope(
                objectId, envelope, expectedType, destination, maximumPayloadSize);
    }

    /**
     * Rejects a container-form object outright while this store's format is 1: FORMAT.md,
     * "Compatibility", requires exactly that -- Go's {@code loadCanonical} and C++ fsck's
     * {@code CheckObject} enforce the identical rule.
     */
    private void requireContainerFormatSupported(String objectId) throws IOException {
        if (format != 2) {
            throw new IOException(
                    "Object is corrupt: " + objectId + ": container-form object in a format 1 repository");
        }
    }

    /**
     * Splits a verified envelope (header + payload, as reconstructed by the v2 container path)
     * back into its type and payload, reusing {@link #readHeader} so both forms agree on what a
     * well-formed header looks like.
     */
    private static ObjectType finishFromEnvelope(
            String objectId,
            byte[] envelope,
            ObjectType expectedType,
            OutputStream destination,
            long maximumPayloadSize)
            throws IOException {
        ByteArrayInputStream stream = new ByteArrayInputStream(envelope);
        Header parsed = readHeader(stream);
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
        if (stream.available() != parsed.payloadSize()) {
            throw new IOException("Object is corrupt: " + objectId);
        }
        destination.write(envelope, envelope.length - stream.available(), stream.available());
        return parsed.type();
    }

    private static int firstByte(Path objectPath, String objectId) throws IOException {
        try (InputStream input = Files.newInputStream(objectPath)) {
            int first = input.read();
            if (first == -1) {
                throw new IOException("Object is corrupt: " + objectId);
            }
            return first;
        }
    }

    /**
     * Resolves any stored object -- legacy or v2 container, full or delta -- to its verified
     * canonical envelope bytes ({@code "<type> <size>\0"} + payload), recursively following delta
     * base chains through this same store. {@code chainStack} holds every id already on the
     * current chain (FORMAT.md's "delta cycle" check) and {@code depth} counts delta hops taken
     * so far from the object this call tree was originally asked to resolve, so that a chain
     * requiring a 33rd hop is rejected before it is walked (a full object is depth 0).
     */
    private byte[] envelopeOf(String objectId, Set<String> chainStack, int depth)
            throws IOException {
        Path objectPath = pathFor(objectId);
        if (!Files.isRegularFile(objectPath, LinkOption.NOFOLLOW_LINKS)) {
            throw new IOException("Object does not exist: " + objectId);
        }
        if (firstByte(objectPath, objectId) != Byte.toUnsignedInt(CONTAINER_MAGIC[0])) {
            return readLegacyEnvelope(objectPath, objectId);
        }
        requireContainerFormatSupported(objectId);
        return readContainerEnvelope(objectId, objectPath, chainStack, depth);
    }

    /**
     * Decodes a legacy zlib object into its verified canonical envelope bytes (header included),
     * bounded to {@link #MAX_INLINE_PAYLOAD}. Used only to resolve a delta's base object; the
     * top-level {@link #copyVerified} legacy branch streams directly to its destination instead,
     * so restoring a large legacy blob never buffers the whole thing in memory.
     */
    private byte[] readLegacyEnvelope(Path objectPath, String objectId) throws IOException {
        MessageDigest digest = Sha256.newDigest();
        Header parsed;
        ByteArrayOutputStream headerBuffer = new ByteArrayOutputStream();
        ByteArrayOutputStream payloadBuffer = new ByteArrayOutputStream();
        try (InputStream fileInput = Files.newInputStream(objectPath);
                InflaterInputStream decompressed = new InflaterInputStream(fileInput);
                DigestInputStream canonical = new DigestInputStream(decompressed, digest)) {
            parsed = readHeader(canonical, headerBuffer);
            if (parsed.payloadSize() > MAX_INLINE_PAYLOAD) {
                throw new IOException(
                        "Object "
                                + objectId
                                + " declares an implausible payload size: "
                                + parsed.payloadSize());
            }
            copyExactly(canonical, payloadBuffer, parsed.payloadSize());
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
        // Use the header exactly as read (headerBuffer, NUL included), not a re-rendering built
        // from parsed.type() and parsed.payloadSize(): the object's id -- and the digest check just
        // above -- covers these exact bytes, which a non-canonical header (e.g. a leading zero in
        // the size) would not survive re-rendering unchanged. A delta based on this object addresses
        // offsets into the same bytes its id was computed over, nothing else.
        return concat(headerBuffer.toByteArray(), payloadBuffer.toByteArray());
    }

    /**
     * Parses an SVO2 container (magic already confirmed by the caller) and returns its verified
     * canonical envelope bytes, applying a delta against its base when {@code kind} says to.
     */
    private byte[] readContainerEnvelope(
            String objectId, Path objectPath, Set<String> chainStack, int depth) throws IOException {
        try (InputStream raw = new BufferedInputStream(Files.newInputStream(objectPath))) {
            byte[] magic = readExactly(raw, CONTAINER_MAGIC.length, objectId);
            if (!Arrays.equals(magic, CONTAINER_MAGIC)) {
                throw new IOException("Object is corrupt: " + objectId);
            }
            int kind = readRequiredByte(raw, objectId);
            int codec = readRequiredByte(raw, objectId);
            if (kind != KIND_FULL && kind != KIND_DELTA) {
                throw new IOException("Object is corrupt: " + objectId);
            }
            if (codec != CODEC_ZLIB && codec != CODEC_ZSTD) {
                throw new IOException("Object is corrupt: " + objectId);
            }

            if (kind == KIND_FULL) {
                byte[] envelope = decodeCodecStream(raw, codec, objectId);
                verifyEnvelopeDigest(objectId, envelope);
                return envelope;
            }

            byte[] baseIdBytes = readExactly(raw, Sha256.BYTE_LENGTH, objectId);
            String baseObjectId = Sha256.hex(baseIdBytes);
            byte[] instructions = decodeCodecStream(raw, codec, objectId);

            if (depth + 1 > MAX_DELTA_DEPTH) {
                throw new IOException(
                        "Object "
                                + objectId
                                + " exceeds the maximum delta chain depth of "
                                + MAX_DELTA_DEPTH);
            }
            if (!chainStack.add(baseObjectId)) {
                throw new IOException("Delta cycle detected while resolving object: " + objectId);
            }
            byte[] base = envelopeOf(baseObjectId, chainStack, depth + 1);

            byte[] target;
            try {
                target = DeltaApplier.apply(base, instructions, MAX_INLINE_PAYLOAD);
            } catch (IOException exception) {
                throw new IOException(
                        "Object " + objectId + " has a corrupt delta: " + exception.getMessage(),
                        exception);
            }
            verifyEnvelopeDigest(objectId, target);
            return target;
        }
    }

    private static byte[] decodeCodecStream(InputStream raw, int codec, String objectId) throws IOException {
        if (codec == CODEC_ZSTD) {
            // FORMAT.md requires exactly one standard zstd frame, no skippable frames:
            // ZstdInputStream instead silently concatenates a second valid frame and, unlike a
            // truncated/garbled one, does not error on a few trailing bytes it fails to recognize
            // as a frame, so framing is validated statically (via the frame and block headers,
            // never a decompressed size) before any byte reaches the decoder.
            byte[] compressed = raw.readAllBytes();
            int frameLength = zstdSingleFrameLength(compressed, objectId);
            if (frameLength != compressed.length) {
                throw new IOException(
                        "Object is corrupt: " + objectId + ": codec-zstd stream carries more than one frame");
            }
            try (InputStream decoder = new ZstdInputStream(new ByteArrayInputStream(compressed))) {
                return drainCodecStream(decoder, objectId);
            } catch (MalformedInputException exception) {
                // Thrown by ZstdInputStream on bad input; it extends RuntimeException, not
                // IOException, so without catching it here it would otherwise escape as an
                // uncaught crash instead of the usual corrupt-object error every other on-disk
                // form here produces.
                throw new IOException("Object is corrupt: " + objectId, exception);
            }
        }
        try (InputStream decoder = new InflaterInputStream(raw)) {
            return drainCodecStream(decoder, objectId);
        } catch (java.util.zip.ZipException exception) {
            throw new IOException("Object is corrupt: " + objectId, exception);
        }
    }

    private static byte[] drainCodecStream(InputStream decoder, String objectId) throws IOException {
        ByteArrayOutputStream buffer = new ByteArrayOutputStream();
        byte[] chunk = new byte[BUFFER_SIZE];
        long total = 0;
        int read;
        while ((read = decoder.read(chunk)) != -1) {
            total += read;
            if (total > MAX_INLINE_PAYLOAD) {
                throw new IOException("Object " + objectId + " decodes beyond the maximum allowed size");
            }
            buffer.write(chunk, 0, read);
        }
        return buffer.toByteArray();
    }

    /**
     * Returns the byte length of the single standard zstd frame {@code raw} is expected to hold
     * entirely (FORMAT.md, "zstd streams": exactly one frame, no skippable frames). Walks the frame
     * header and every data block's header -- each of which states its own stored content length --
     * so the frame's exact end is known without decompressing anything and without ever trusting
     * the optional frame content size field. The caller rejects {@code raw} as carrying more than
     * one frame (or trailing garbage) whenever the returned length is shorter than
     * {@code raw.length}.
     */
    private static int zstdSingleFrameLength(byte[] raw, String objectId) throws IOException {
        if (raw.length < 5) {
            throw new IOException("Object is corrupt: " + objectId + ": truncated zstd frame header");
        }
        boolean skippable =
                (raw[0] & 0xf0) == 0x50 && raw[1] == 0x2a && raw[2] == 0x4d && raw[3] == 0x18;
        if (skippable) {
            throw new IOException("Object is corrupt: " + objectId + ": skippable zstd frames are not allowed");
        }
        boolean standardMagic =
                raw[0] == 0x28 && raw[1] == (byte) 0xb5 && raw[2] == 0x2f && raw[3] == (byte) 0xfd;
        if (!standardMagic) {
            throw new IOException("Object is corrupt: " + objectId + ": invalid zstd frame magic");
        }

        int frameHeaderDescriptor = raw[4] & 0xff;
        if ((frameHeaderDescriptor & 0x08) != 0) {
            throw new IOException(
                    "Object is corrupt: " + objectId + ": reserved bit set on zstd frame header");
        }
        boolean singleSegment = (frameHeaderDescriptor & 0x20) != 0;
        boolean hasChecksum = (frameHeaderDescriptor & 0x04) != 0;
        int dictionaryIdFlag = frameHeaderDescriptor & 0x03;
        int frameContentSizeFlag = (frameHeaderDescriptor >> 6) & 0x03;

        int offset = 5;
        if (!singleSegment) {
            offset += 1; // Window_Descriptor.
        }
        offset += switch (dictionaryIdFlag) {
            case 0 -> 0;
            case 1 -> 1;
            case 2 -> 2;
            default -> 4;
        };
        offset += frameContentSizeFlag == 0 ? (singleSegment ? 1 : 0) : 1 << frameContentSizeFlag;
        if (offset > raw.length) {
            throw new IOException("Object is corrupt: " + objectId + ": truncated zstd frame header");
        }

        boolean lastBlock = false;
        while (!lastBlock) {
            if (offset + 3 > raw.length) {
                throw new IOException("Object is corrupt: " + objectId + ": truncated zstd block header");
            }
            int blockHeader =
                    (raw[offset] & 0xff) | ((raw[offset + 1] & 0xff) << 8) | ((raw[offset + 2] & 0xff) << 16);
            lastBlock = (blockHeader & 1) != 0;
            int blockType = (blockHeader >> 1) & 0x3;
            int blockSize = blockHeader >>> 3;
            offset += 3;
            switch (blockType) {
                case 0, 2 -> offset += blockSize; // Raw_Block, Compressed_Block.
                case 1 -> offset += 1; // RLE_Block.
                default -> throw new IOException(
                        "Object is corrupt: " + objectId + ": reserved zstd block type");
            }
            if (offset > raw.length) {
                throw new IOException("Object is corrupt: " + objectId + ": truncated zstd block");
            }
        }
        if (hasChecksum) {
            offset += 4;
        }
        if (offset > raw.length) {
            throw new IOException("Object is corrupt: " + objectId + ": truncated zstd checksum");
        }
        return offset;
    }

    private static void verifyEnvelopeDigest(String objectId, byte[] envelope) throws IOException {
        MessageDigest digest = Sha256.newDigest();
        digest.update(envelope);
        String actualId = Sha256.hex(digest.digest());
        if (!actualId.equals(objectId)) {
            throw new IOException(
                    "Object failed its SHA-256 integrity check: " + objectId + " (actual " + actualId + ")");
        }
    }

    private static byte[] readExactly(InputStream input, int length, String objectId) throws IOException {
        byte[] buffer = new byte[length];
        int total = 0;
        while (total < length) {
            int read = input.read(buffer, total, length - total);
            if (read == -1) {
                throw new IOException("Object is corrupt: " + objectId);
            }
            total += read;
        }
        return buffer;
    }

    private static int readRequiredByte(InputStream input, String objectId) throws IOException {
        int value = input.read();
        if (value == -1) {
            throw new IOException("Object is corrupt: " + objectId);
        }
        return value;
    }

    private static byte[] concat(byte[] first, byte[] second) {
        byte[] joined = new byte[first.length + second.length];
        System.arraycopy(first, 0, joined, 0, first.length);
        System.arraycopy(second, 0, joined, first.length, second.length);
        return joined;
    }

    private static Header readHeader(InputStream input) throws IOException {
        return readHeader(input, null);
    }

    /**
     * Reads and parses a canonical envelope header. When {@code rawHeader} is non-null, the exact
     * bytes consumed -- including the terminating NUL -- are also written there, for a caller that
     * needs the header exactly as stored rather than a re-rendering from the parsed type and size
     * (see {@link #readLegacyEnvelope}).
     */
    private static Header readHeader(InputStream input, ByteArrayOutputStream rawHeader)
            throws IOException {
        ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        for (int index = 0; index < MAX_HEADER_BYTES; index++) {
            int next = input.read();
            if (next == -1) {
                throw new IOException("Truncated object header");
            }
            if (rawHeader != null) {
                rawHeader.write(next);
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
