package io.snapvault.core;

import io.snapvault.model.TreeEntry;

import java.util.Objects;

/** One leaf-path change between snapshots or between a snapshot and the working directory. */
public record FileChange(
        ChangeType type, String path, TreeEntry before, TreeEntry after) {
    public FileChange {
        Objects.requireNonNull(type, "type");
        Objects.requireNonNull(path, "path");
        if (path.isEmpty()) {
            throw new IllegalArgumentException("Change path cannot be empty");
        }
        if (before == null && after == null) {
            throw new IllegalArgumentException("A change needs a before or after entry");
        }
    }
}
