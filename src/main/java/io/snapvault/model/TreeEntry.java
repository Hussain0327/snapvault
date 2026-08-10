package io.snapvault.model;

import io.snapvault.hash.Sha256;

import java.util.Objects;

/** A single immutable entry in a directory tree object. */
public record TreeEntry(String name, EntryKind kind, String objectId, boolean executable) {
    public TreeEntry {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(kind, "kind");
        Objects.requireNonNull(objectId, "objectId");
        if (name.isEmpty()
                || name.equals(".")
                || name.equals("..")
                || name.indexOf('/') >= 0
                || name.indexOf('\\') >= 0
                || name.indexOf('\0') >= 0) {
            throw new IllegalArgumentException("Unsafe tree entry name: " + name);
        }
        if (kind != EntryKind.FILE && executable) {
            throw new IllegalArgumentException("Only regular files can be executable");
        }
        Sha256.requireObjectId(objectId);
    }
}
