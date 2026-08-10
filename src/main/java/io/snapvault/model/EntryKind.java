package io.snapvault.model;

import java.io.IOException;

/** The filesystem node represented by a tree entry. */
public enum EntryKind {
    FILE(1),
    DIRECTORY(2),
    SYMLINK(3);

    private final int code;

    EntryKind(int code) {
        this.code = code;
    }

    public int code() {
        return code;
    }

    public static EntryKind fromCode(int code) throws IOException {
        for (EntryKind kind : values()) {
            if (kind.code == code) {
                return kind;
            }
        }
        throw new IOException("Unknown tree entry kind: " + code);
    }
}
