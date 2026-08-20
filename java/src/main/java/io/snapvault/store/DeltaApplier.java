package io.snapvault.store;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.util.Arrays;

/**
 * Applies a SnapVault v2 delta instruction stream (Git's pack delta wire format) to a base
 * object's canonical bytes, reconstructing the target object's canonical bytes.
 *
 * <p>See docs/FORMAT.md, "Delta instruction format", for the byte-exact specification this class
 * implements: a little-endian base-128 varint header ({@code srcSize}, {@code tgtSize}) followed
 * by insert and copy opcodes that run to the end of the stream. Output length is the only
 * terminator, so every structural rule short of that is enforced as each opcode is read.</p>
 */
final class DeltaApplier {
    private DeltaApplier() {
    }

    /**
     * Reconstructs the target canonical bytes described by {@code instructions} against
     * {@code base}. {@code srcSize} must match {@code base.length}; every copy must stay in
     * bounds; the final output length must equal {@code tgtSize}; and {@code tgtSize} itself may
     * not exceed {@code maxOutputSize}, checked before any output buffer is allocated.
     */
    static byte[] apply(byte[] base, byte[] instructions, long maxOutputSize) throws IOException {
        Cursor cursor = new Cursor(instructions);
        long sourceSize = cursor.readVarint();
        long targetSize = cursor.readVarint();
        if (sourceSize != base.length) {
            throw new IOException(
                    "delta source size " + sourceSize + " does not match base object size " + base.length);
        }
        if (targetSize > maxOutputSize) {
            throw new IOException("delta target size " + targetSize + " exceeds the maximum object size");
        }

        ByteArrayOutputStream output = new ByteArrayOutputStream((int) Math.min(targetSize, base.length + 1));
        while (cursor.hasRemaining()) {
            int opcode = cursor.readByte();
            if (opcode == 0x00) {
                throw new IOException("delta stream contains the reserved opcode 0x00");
            }
            if ((opcode & 0x80) == 0) {
                output.writeBytes(cursor.readBytes(opcode));
            } else {
                copyFromBase(base, opcode, cursor, output);
            }
            if (output.size() > targetSize) {
                throw new IOException("delta stream produced more output than its declared target size");
            }
        }

        if (output.size() != targetSize) {
            throw new IOException("delta stream produced " + output.size() + " bytes, expected " + targetSize);
        }
        return output.toByteArray();
    }

    /**
     * Reads a copy opcode's offset and size bytes (present bytes little-endian, lowest first,
     * offset-then-size; omitted bytes are zero) and appends the copied slice of {@code base}.
     */
    private static void copyFromBase(byte[] base, int opcode, Cursor cursor, ByteArrayOutputStream output)
            throws IOException {
        long offset = 0;
        for (int shift = 0, bit = 0x01; bit <= 0x08; shift += 8, bit <<= 1) {
            if ((opcode & bit) != 0) {
                offset |= ((long) cursor.readByte()) << shift;
            }
        }
        long size = 0;
        for (int shift = 0, bit = 0x10; bit <= 0x40; shift += 8, bit <<= 1) {
            if ((opcode & bit) != 0) {
                size |= ((long) cursor.readByte()) << shift;
            }
        }
        if (size == 0) {
            size = 65536;
        }
        if (offset + size > base.length) {
            throw new IOException("delta copy instruction is out of bounds: offset=" + offset + " size=" + size);
        }
        output.write(base, (int) offset, (int) size);
    }

    /** A forward-only reader over the instruction bytes that turns short reads into clear errors. */
    private static final class Cursor {
        private final byte[] data;
        private int position;

        Cursor(byte[] data) {
            this.data = data;
        }

        boolean hasRemaining() {
            return position < data.length;
        }

        int readByte() throws IOException {
            if (position >= data.length) {
                throw new IOException("delta stream ends mid-instruction");
            }
            return data[position++] & 0xff;
        }

        byte[] readBytes(int count) throws IOException {
            if (position + count > data.length) {
                throw new IOException("delta stream ends mid-instruction");
            }
            byte[] slice = Arrays.copyOfRange(data, position, position + count);
            position += count;
            return slice;
        }

        long readVarint() throws IOException {
            long value = 0;
            int shift = 0;
            while (true) {
                int next = readByte();
                value |= ((long) (next & 0x7f)) << shift;
                if ((next & 0x80) == 0) {
                    return value;
                }
                shift += 7;
                if (shift > 63) {
                    throw new IOException("delta varint is too long");
                }
            }
        }
    }
}
