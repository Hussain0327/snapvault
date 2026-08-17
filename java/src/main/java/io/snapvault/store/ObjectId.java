package io.snapvault.store;

import io.snapvault.hash.Sha256;

import java.io.IOException;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;

/**
 * Computes SnapVault object ids without storing anything.
 *
 * <p>An object id is the SHA-256 digest of the canonical envelope {@code "<type> <size>\0"}
 * followed by the payload. Computing it separately lets a caller address content it has not
 * decided to keep, which is what makes a working-tree diff a read-only operation.</p>
 */
public final class ObjectId {
    private static final int BUFFER_SIZE = 64 * 1024;

    private ObjectId() {
    }

    /** Returns the id an in-memory payload would have if it were stored. */
    public static String of(ObjectType type, byte[] payload) {
        MessageDigest digest = Sha256.newDigest();
        digest.update(header(type, payload.length));
        digest.update(payload);
        return Sha256.hex(digest.digest());
    }

    /** Returns the blob id of a file, streaming it so its size is not bounded by heap size. */
    public static String ofBlob(Path source) throws IOException {
        long expectedSize = Files.size(source);
        MessageDigest digest = Sha256.newDigest();
        digest.update(header(ObjectType.BLOB, expectedSize));

        long read = 0;
        byte[] buffer = new byte[BUFFER_SIZE];
        try (InputStream input = Files.newInputStream(source)) {
            int chunk;
            while ((chunk = input.read(buffer)) != -1) {
                digest.update(buffer, 0, chunk);
                read += chunk;
            }
        }
        if (read != expectedSize) {
            throw new IOException(
                    "File changed while it was being read (expected "
                            + expectedSize
                            + " bytes, read "
                            + read
                            + ")");
        }
        return Sha256.hex(digest.digest());
    }

    static byte[] header(ObjectType type, long payloadSize) {
        return (type.token() + " " + payloadSize + "\0").getBytes(StandardCharsets.US_ASCII);
    }
}
