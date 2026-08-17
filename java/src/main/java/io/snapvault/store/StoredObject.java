package io.snapvault.store;

import java.util.Objects;

/** An object after its envelope has been decoded and integrity-checked. */
public record StoredObject(ObjectType type, byte[] payload) {
    public StoredObject {
        Objects.requireNonNull(type, "type");
        payload = Objects.requireNonNull(payload, "payload").clone();
    }

    @Override
    public byte[] payload() {
        return payload.clone();
    }
}
