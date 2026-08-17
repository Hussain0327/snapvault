package io.snapvault.model;

import io.snapvault.hash.Sha256;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.EOFException;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashSet;
import java.util.List;
import java.util.Objects;
import java.util.Set;

/** A canonical, sorted directory listing stored as a tree object. */
public final class Tree {
    private static final int MAGIC = 0x53565431; // SVT1
    private static final int MAX_ENTRIES = 1_000_000;
    private static final int MAX_NAME_BYTES = 1_048_576;

    private final List<TreeEntry> entries;

    public Tree(List<TreeEntry> entries) {
        Objects.requireNonNull(entries, "entries");
        List<TreeEntry> sorted = new ArrayList<>(entries);
        sorted.sort(Comparator.comparing(TreeEntry::name));

        Set<String> names = new HashSet<>();
        for (TreeEntry entry : sorted) {
            if (!names.add(entry.name())) {
                throw new IllegalArgumentException("Duplicate tree entry: " + entry.name());
            }
        }
        this.entries = List.copyOf(sorted);
    }

    public List<TreeEntry> entries() {
        return entries;
    }

    public byte[] encode() throws IOException {
        ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        try (DataOutputStream output = new DataOutputStream(bytes)) {
            output.writeInt(MAGIC);
            output.writeInt(entries.size());
            for (TreeEntry entry : entries) {
                writeString(output, entry.name());
                output.writeByte(entry.kind().code());
                output.writeBoolean(entry.executable());
                output.write(Sha256.bytes(entry.objectId()));
            }
        }
        return bytes.toByteArray();
    }

    public static Tree decode(byte[] encoded) throws IOException {
        Objects.requireNonNull(encoded, "encoded");
        try (ByteArrayInputStream bytes = new ByteArrayInputStream(encoded);
                DataInputStream input = new DataInputStream(bytes)) {
            if (input.readInt() != MAGIC) {
                throw new IOException("Invalid tree object signature");
            }
            int count = input.readInt();
            if (count < 0 || count > MAX_ENTRIES) {
                throw new IOException("Invalid tree entry count: " + count);
            }

            List<TreeEntry> entries = new ArrayList<>(count);
            try {
                for (int index = 0; index < count; index++) {
                    String name = readString(input, MAX_NAME_BYTES);
                    EntryKind kind = EntryKind.fromCode(input.readUnsignedByte());
                    boolean executable = input.readBoolean();
                    byte[] objectId = input.readNBytes(Sha256.BYTE_LENGTH);
                    if (objectId.length != Sha256.BYTE_LENGTH) {
                        throw new EOFException("Truncated tree entry object id");
                    }
                    entries.add(new TreeEntry(name, kind, Sha256.hex(objectId), executable));
                }
            } catch (IllegalArgumentException exception) {
                throw new IOException("Invalid tree entry", exception);
            }

            if (bytes.available() != 0) {
                throw new IOException("Tree object has trailing data");
            }
            try {
                return new Tree(entries);
            } catch (IllegalArgumentException exception) {
                throw new IOException("Invalid tree object", exception);
            }
        } catch (EOFException exception) {
            throw new IOException("Truncated tree object", exception);
        }
    }

    static void writeString(DataOutputStream output, String value) throws IOException {
        byte[] bytes = value.getBytes(StandardCharsets.UTF_8);
        output.writeInt(bytes.length);
        output.write(bytes);
    }

    static String readString(DataInputStream input, int maximumBytes) throws IOException {
        int length = input.readInt();
        if (length < 0 || length > maximumBytes) {
            throw new IOException("Invalid string length: " + length);
        }
        byte[] bytes = input.readNBytes(length);
        if (bytes.length != length) {
            throw new EOFException("Truncated string");
        }
        String value = new String(bytes, StandardCharsets.UTF_8);
        if (!java.util.Arrays.equals(value.getBytes(StandardCharsets.UTF_8), bytes)) {
            throw new IOException("String is not valid UTF-8");
        }
        return value;
    }
}
