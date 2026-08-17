package io.snapvault.cli;

import java.nio.file.Path;

/** SnapVault process entry point. */
public final class Main {
    private Main() {
    }

    public static void main(String[] arguments) {
        Cli cli = new Cli(System.out, System.err, Path.of(System.getProperty("user.dir")));
        int exitCode = cli.run(arguments);
        if (exitCode != 0) {
            System.exit(exitCode);
        }
    }
}
