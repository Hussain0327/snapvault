package io.snapvault.model;

import io.snapvault.hash.Sha256;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.EOFException;
import java.io.IOException;
import java.time.DateTimeException;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Objects;

/** A snapshot commit linking a root tree to zero or more parent commits. */
public record Commit(String treeId, List<String> parents, Instant timestamp, String message) {
    private static final int MAGIC = 0x53564331; // SVC1
    private static final int MAX_PARENTS = 64;
    private static final int MAX_MESSAGE_BYTES = 4 * 1024 * 1024;

    public Commit {
        Sha256.requireObjectId(Objects.requireNonNull(treeId, "treeId"));
        parents = List.copyOf(Objects.requireNonNull(parents, "parents"));
        if (parents.size() > MAX_PARENTS) {
            throw new IllegalArgumentException("A commit cannot have more than 64 parents");
        }
        for (String parent : parents) {
            Sha256.requireObjectId(parent);
        }
        Objects.requireNonNull(timestamp, "timestamp");
        message = Objects.requireNonNull(message, "message");
        if (message.indexOf('\0') >= 0) {
            throw new IllegalArgumentException("Commit message cannot contain NUL");
        }
        if (message.getBytes(java.nio.charset.StandardCharsets.UTF_8).length > MAX_MESSAGE_BYTES) {
            throw new IllegalArgumentException("Commit message is too large");
        }
    }

    public byte[] encode() throws IOException {
        ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        try (DataOutputStream output = new DataOutputStream(bytes)) {
            output.writeInt(MAGIC);
            output.write(Sha256.bytes(treeId));
            output.writeInt(parents.size());
            for (String parent : parents) {
                output.write(Sha256.bytes(parent));
            }
            output.writeLong(timestamp.getEpochSecond());
            output.writeInt(timestamp.getNano());
            Tree.writeString(output, message);
        }
        return bytes.toByteArray();
    }

    public static Commit decode(byte[] encoded) throws IOException {
        Objects.requireNonNull(encoded, "encoded");
        try (ByteArrayInputStream bytes = new ByteArrayInputStream(encoded);
                DataInputStream input = new DataInputStream(bytes)) {
            if (input.readInt() != MAGIC) {
                throw new IOException("Invalid commit object signature");
            }

            String treeId = readObjectId(input, "tree");
            int parentCount = input.readInt();
            if (parentCount < 0 || parentCount > MAX_PARENTS) {
                throw new IOException("Invalid commit parent count: " + parentCount);
            }
            List<String> parents = new ArrayList<>(parentCount);
            for (int index = 0; index < parentCount; index++) {
                parents.add(readObjectId(input, "parent"));
            }

            Instant timestamp;
            try {
                long epochSecond = input.readLong();
                int nanosecond = input.readInt();
                if (nanosecond < 0 || nanosecond > 999_999_999) {
                    throw new IOException("Invalid commit nanosecond: " + nanosecond);
                }
                timestamp = Instant.ofEpochSecond(epochSecond, nanosecond);
            } catch (DateTimeException exception) {
                throw new IOException("Invalid commit timestamp", exception);
            }
            String message = Tree.readString(input, MAX_MESSAGE_BYTES);
            if (bytes.available() != 0) {
                throw new IOException("Commit object has trailing data");
            }

            try {
                return new Commit(treeId, parents, timestamp, message);
            } catch (IllegalArgumentException exception) {
                throw new IOException("Invalid commit object", exception);
            }
        } catch (EOFException exception) {
            throw new IOException("Truncated commit object", exception);
        }
    }

    private static String readObjectId(DataInputStream input, String description) throws IOException {
        byte[] objectId = input.readNBytes(Sha256.BYTE_LENGTH);
        if (objectId.length != Sha256.BYTE_LENGTH) {
            throw new EOFException("Truncated " + description + " object id");
        }
        return Sha256.hex(objectId);
    }
}
