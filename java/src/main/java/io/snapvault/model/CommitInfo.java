package io.snapvault.model;

import io.snapvault.hash.Sha256;

import java.util.Objects;

/** A commit paired with the content address used to reach it. */
public record CommitInfo(String objectId, Commit commit) {
    public CommitInfo {
        Sha256.requireObjectId(objectId);
        Objects.requireNonNull(commit, "commit");
    }
}
