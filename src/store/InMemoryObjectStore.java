package store;

import hash.Hasher;

import java.util.HashMap;
import java.util.Map;

public class InMemoryObjectStore implements ObjectStore {
    private final Map<String, byte[]> objects;
    private final Hasher hasher;

    public InMemoryObjectStore(Hasher hasher) {
        this.hasher = hasher;
        this.objects = new HashMap<>();
    }

    @Override
    public String store(byte[] contents) {
        String fingerprint = hasher.hash(contents);
        objects.put(fingerprint, contents);
        return fingerprint;
    }

    @Override
    public byte[] read(String fingerprint) {
        return objects.get(fingerprint);
    }

    @Override
    public boolean has(String fingerprint) {
        return objects.containsKey(fingerprint);
    }
}