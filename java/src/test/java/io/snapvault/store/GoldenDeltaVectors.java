package io.snapvault.store;

import java.io.IOException;

/**
 * Test-only bridge that exposes package-private {@link DeltaApplier#apply} to the cross-language
 * golden vector suite in {@code io.snapvault.AllTests}, which lives outside this package.
 *
 * <p>See tests/golden/v2/delta/MANIFEST.md for the fixtures this drives: raw {@code base} and
 * {@code delta} byte streams (the same {@code srcSize}/{@code tgtSize} varint header plus
 * insert/copy opcodes {@link DeltaApplier} decodes from a v2 container), shared verbatim with the
 * Go and C++ suites so all three decoders are checked against the same bytes.</p>
 */
public final class GoldenDeltaVectors {
    /** Matches {@link FileObjectStore}'s cap on a delta's reconstructed output. */
    private static final long MAX_OUTPUT_SIZE = 256L * 1024 * 1024;

    private GoldenDeltaVectors() {
    }

    /** Applies {@code delta} to {@code base}, exactly as a v2 container object's delta would be. */
    public static byte[] apply(byte[] base, byte[] delta) throws IOException {
        return DeltaApplier.apply(base, delta, MAX_OUTPUT_SIZE);
    }
}
