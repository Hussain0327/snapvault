package io.snapvault.store;

import java.io.IOException;
import java.io.OutputStream;
import java.nio.file.Path;
import java.util.List;

/** A content-addressed store for typed, immutable objects. */
public interface ObjectStore {
    String put(ObjectType type, byte[] payload) throws IOException;

    String putBlob(Path source) throws IOException;

    StoredObject get(String objectId) throws IOException;

    void copyPayload(String objectId, ObjectType expectedType, OutputStream destination)
            throws IOException;

    boolean contains(String objectId);

    List<String> findByPrefix(String prefix) throws IOException;

    long count() throws IOException;
}
