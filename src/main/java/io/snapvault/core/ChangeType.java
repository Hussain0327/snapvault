package io.snapvault.core;

/** The kind of change between two directory trees. */
public enum ChangeType {
    ADDED('A'),
    MODIFIED('M'),
    DELETED('D'),
    TYPE_CHANGED('T');

    private final char status;

    ChangeType(char status) {
        this.status = status;
    }

    public char status() {
        return status;
    }
}
