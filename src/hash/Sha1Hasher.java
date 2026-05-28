package hash;

import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;


public class Sha1Hasher implements Hasher {

    @Override
    public String hash(byte[] contents) {
        try {
            MessageDigest md = MessageDigest.getInstance("SHA-1");
            byte[] raw = md.digest(contents);

            StringBuilder sb = new StringBuilder();
            for (byte b : raw) {
                sb.append(String.format("%02x", b));
            }
            return sb.toString();
        } catch (NoSuchAlgorithmException e) {
            throw new RuntimeException("SHA-1 not available", e);
        }

    }
}
