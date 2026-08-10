package io.snapvault.store;

import java.io.IOException;

/** The typed objects persisted in a SnapVault object database. */
public enum ObjectType {
    BLOB("blob"),
    TREE("tree"),
    COMMIT("commit");

    private final String token;

    ObjectType(String token) {
        this.token = token;
    }

    public String token() {
        return token;
    }

    public static ObjectType fromToken(String token) throws IOException {
        for (ObjectType type : values()) {
            if (type.token.equals(token)) {
                return type;
            }
        }
        throw new IOException("Unknown object type: " + token);
    }
}
