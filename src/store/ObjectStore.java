package store;

public interface ObjectStore {
    String store(byte[] contents);
    byte[] read(String fingerprint);
    boolean has(String fingerprint);
}
