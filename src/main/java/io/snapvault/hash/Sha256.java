package io.snapvault.hash;

import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.HexFormat;

/** Utilities for SnapVault's SHA-256 object identifiers. */
public final class Sha256 {
    public static final int BYTE_LENGTH = 32;
    public static final int HEX_LENGTH = BYTE_LENGTH * 2;

    private Sha256() {
    }

    public static MessageDigest newDigest() {
        try {
            return MessageDigest.getInstance("SHA-256");
        } catch (NoSuchAlgorithmException exception) {
            throw new IllegalStateException("This Java runtime does not provide SHA-256", exception);
        }
    }

    public static String hex(byte[] digest) {
        return HexFormat.of().formatHex(digest);
    }

    public static byte[] bytes(String objectId) {
        requireObjectId(objectId);
        return HexFormat.of().parseHex(objectId);
    }

    public static void requireObjectId(String objectId) {
        if (objectId == null || objectId.length() != HEX_LENGTH) {
            throw new IllegalArgumentException("Object id must be 64 hexadecimal characters");
        }
        for (int index = 0; index < objectId.length(); index++) {
            char character = objectId.charAt(index);
            boolean digit = character >= '0' && character <= '9';
            boolean lowercaseHex = character >= 'a' && character <= 'f';
            if (!digit && !lowercaseHex) {
                throw new IllegalArgumentException("Object id must use lowercase hexadecimal characters");
            }
        }
    }
}
