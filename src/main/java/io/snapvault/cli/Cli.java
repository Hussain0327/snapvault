package io.snapvault.cli;

import io.snapvault.core.FileChange;
import io.snapvault.core.Repository;
import io.snapvault.model.CommitInfo;
import io.snapvault.model.EntryKind;
import io.snapvault.model.TreeEntry;

import java.io.IOException;
import java.io.PrintStream;
import java.nio.file.AccessDeniedException;
import java.nio.file.FileSystemException;
import java.nio.file.NoSuchFileException;
import java.nio.file.Path;
import java.time.ZoneId;
import java.time.format.DateTimeFormatter;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.Optional;

/** Dependency-free command-line interface for SnapVault. */
public final class Cli {
    private static final int DEFAULT_LOG_LIMIT = 50;
    private static final DateTimeFormatter LOG_DATE = DateTimeFormatter.ISO_OFFSET_DATE_TIME;

    private final PrintStream out;
    private final PrintStream err;
    private final Path initialDirectory;

    public Cli(PrintStream out, PrintStream err, Path initialDirectory) {
        this.out = out;
        this.err = err;
        this.initialDirectory = initialDirectory.toAbsolutePath().normalize();
    }

    public int run(String... arguments) {
        try {
            return execute(List.of(arguments));
        } catch (UsageException exception) {
            err.println("error: " + exception.getMessage());
            err.println("Run 'snapvault help' for usage.");
            return 2;
        } catch (IOException | IllegalArgumentException exception) {
            err.println("error: " + describe(exception));
            return 1;
        }
    }

    private int execute(List<String> rawArguments) throws IOException, UsageException {
        List<String> arguments = new ArrayList<>(rawArguments);
        Path workingDirectory = initialDirectory;
        int index = 0;
        while (index < arguments.size() && arguments.get(index).equals("-C")) {
            if (index + 1 >= arguments.size()) {
                throw new UsageException("-C requires a directory");
            }
            workingDirectory = resolve(workingDirectory, arguments.get(index + 1));
            index += 2;
        }

        if (index >= arguments.size()) {
            printUsage(out);
            return 0;
        }

        String command = arguments.get(index++).toLowerCase(Locale.ROOT);
        List<String> commandArguments = arguments.subList(index, arguments.size());
        return switch (command) {
            case "init" -> init(workingDirectory, commandArguments);
            case "snapshot", "commit" -> snapshot(workingDirectory, commandArguments);
            case "log" -> log(workingDirectory, commandArguments);
            case "diff" -> diff(workingDirectory, commandArguments);
            case "restore" -> restore(workingDirectory, commandArguments);
            case "help", "--help", "-h" -> {
                if (!commandArguments.isEmpty()) {
                    throw new UsageException("help does not accept arguments");
                }
                printUsage(out);
                yield 0;
            }
            case "version", "--version" -> {
                if (!commandArguments.isEmpty()) {
                    throw new UsageException("version does not accept arguments");
                }
                out.println("SnapVault 1.0.0");
                yield 0;
            }
            default -> throw new UsageException("unknown command: " + command);
        };
    }

    private int init(Path workingDirectory, List<String> arguments)
            throws IOException, UsageException {
        if (arguments.size() > 1) {
            throw new UsageException("init accepts at most one directory");
        }
        Path directory = arguments.isEmpty()
                ? workingDirectory
                : resolve(workingDirectory, arguments.getFirst());
        Repository repository = Repository.init(directory);
        out.println("Initialized empty SnapVault repository in " + repository.metadata());
        return 0;
    }

    private int snapshot(Path workingDirectory, List<String> arguments)
            throws IOException, UsageException {
        String message = "Snapshot";
        for (int index = 0; index < arguments.size(); index++) {
            String argument = arguments.get(index);
            if (argument.equals("-m") || argument.equals("--message")) {
                if (++index >= arguments.size()) {
                    throw new UsageException(argument + " requires a message");
                }
                message = arguments.get(index);
            } else if (argument.startsWith("--message=")) {
                message = argument.substring("--message=".length());
            } else {
                throw new UsageException("unexpected snapshot argument: " + argument);
            }
        }

        Repository repository = Repository.open(workingDirectory);
        String commitId = repository.snapshot(message);
        out.println("Snapshot " + abbreviate(commitId) + " " + message.strip());
        return 0;
    }

    private int log(Path workingDirectory, List<String> arguments)
            throws IOException, UsageException {
        boolean oneline = false;
        int limit = DEFAULT_LOG_LIMIT;
        String revision = "HEAD";
        boolean revisionSet = false;

        for (int index = 0; index < arguments.size(); index++) {
            String argument = arguments.get(index);
            if (argument.equals("--oneline")) {
                oneline = true;
            } else if (argument.equals("--limit") || argument.equals("-n")) {
                if (++index >= arguments.size()) {
                    throw new UsageException(argument + " requires a number");
                }
                limit = positiveInteger(arguments.get(index), "log limit");
            } else if (argument.startsWith("--limit=")) {
                limit = positiveInteger(argument.substring("--limit=".length()), "log limit");
            } else if (argument.startsWith("-")) {
                throw new UsageException("unknown log option: " + argument);
            } else if (!revisionSet) {
                revision = argument;
                revisionSet = true;
            } else {
                throw new UsageException("log accepts at most one starting snapshot");
            }
        }

        Repository repository = Repository.open(workingDirectory);
        if (repository.head().isEmpty()) {
            out.println("No snapshots yet.");
            return 0;
        }

        List<CommitInfo> history = repository.history(revision, limit);
        for (CommitInfo info : history) {
            if (oneline) {
                out.println(abbreviate(info.objectId()) + " " + firstLine(info.commit().message()));
            } else {
                out.println("commit " + info.objectId());
                for (String parent : info.commit().parents()) {
                    out.println("Parent: " + parent);
                }
                out.println("Date:   " + LOG_DATE.format(
                        info.commit().timestamp().atZone(ZoneId.systemDefault())));
                out.println();
                for (String line : info.commit().message().lines().toList()) {
                    out.println("    " + line);
                }
                out.println();
            }
        }
        return 0;
    }

    private int diff(Path workingDirectory, List<String> arguments)
            throws IOException, UsageException {
        for (String argument : arguments) {
            if (argument.startsWith("-")) {
                throw new UsageException("unknown diff option: " + argument);
            }
        }
        if (arguments.size() > 2) {
            throw new UsageException("diff accepts at most two snapshot revisions");
        }

        Repository repository = Repository.open(workingDirectory);
        List<FileChange> changes = switch (arguments.size()) {
            case 0 -> repository.diffWorkingFromHead();
            case 1 -> repository.diffWorking(arguments.getFirst());
            case 2 -> repository.diff(arguments.get(0), arguments.get(1));
            default -> throw new AssertionError("unreachable");
        };

        if (changes.isEmpty()) {
            out.println("No changes.");
        } else {
            for (FileChange change : changes) {
                TreeEntry entry = change.after() == null ? change.before() : change.after();
                String suffix = entry.kind() == EntryKind.DIRECTORY ? "/" : "";
                out.println(change.type().status() + "\t" + printablePath(change.path()) + suffix);
            }
            out.println(changes.size() + (changes.size() == 1 ? " change" : " changes"));
        }
        return 0;
    }

    private int restore(Path workingDirectory, List<String> arguments)
            throws IOException, UsageException {
        boolean force = false;
        Path target = null;
        String revision = null;

        for (int index = 0; index < arguments.size(); index++) {
            String argument = arguments.get(index);
            if (argument.equals("--force") || argument.equals("-f")) {
                force = true;
            } else if (argument.equals("--to")) {
                if (++index >= arguments.size()) {
                    throw new UsageException("--to requires a directory");
                }
                target = resolve(workingDirectory, arguments.get(index));
            } else if (argument.startsWith("--to=")) {
                target = resolve(workingDirectory, argument.substring("--to=".length()));
            } else if (argument.startsWith("-")) {
                throw new UsageException("unknown restore option: " + argument);
            } else if (revision == null) {
                revision = argument;
            } else {
                throw new UsageException("restore requires exactly one snapshot revision");
            }
        }
        if (revision == null) {
            throw new UsageException("restore requires a snapshot revision");
        }

        Repository repository = Repository.open(workingDirectory);
        String resolved = repository.resolveCommit(revision);
        repository.restore(resolved, target, force);
        Path restoredTo = target == null ? repository.root() : target;
        out.println("Restored " + abbreviate(resolved) + " to " + restoredTo.toAbsolutePath().normalize());
        return 0;
    }

    /**
     * Describes a failure in terms a person can act on. {@link FileSystemException} and its
     * subtypes carry only the offending path as their message, which on its own prints as an
     * unexplained path with no indication of what went wrong.
     */
    private static String describe(Throwable exception) {
        if (exception instanceof AccessDeniedException denied) {
            return "permission denied: " + denied.getFile();
        }
        if (exception instanceof NoSuchFileException missing) {
            return "no such file: " + missing.getFile();
        }
        if (exception instanceof FileSystemException failure) {
            String reason = failure.getReason();
            return (reason == null ? "filesystem error" : reason.strip()) + ": " + failure.getFile();
        }
        String message = exception.getMessage();
        return message == null || message.isBlank() ? exception.toString() : message;
    }

    private static int positiveInteger(String value, String description) throws UsageException {
        try {
            int parsed = Integer.parseInt(value);
            if (parsed < 1) {
                throw new UsageException(description + " must be positive");
            }
            return parsed;
        } catch (NumberFormatException exception) {
            throw new UsageException(description + " must be a number");
        }
    }

    private static String firstLine(String value) {
        Optional<String> first = value.lines().findFirst();
        return first.orElse("");
    }

    private static String printablePath(String path) {
        return path.replace("\\", "\\\\")
                .replace("\t", "\\t")
                .replace("\r", "\\r")
                .replace("\n", "\\n");
    }

    private static String abbreviate(String objectId) {
        return objectId.substring(0, 12);
    }

    private static Path resolve(Path base, String value) {
        Path path = Path.of(value);
        return path.isAbsolute() ? path.normalize() : base.resolve(path).normalize();
    }

    private static void printUsage(PrintStream stream) {
        stream.println("SnapVault - Git-style snapshots for any directory");
        stream.println();
        stream.println("Usage:");
        stream.println("  snapvault init [directory]");
        stream.println("  snapvault [-C directory] snapshot [-m message]");
        stream.println("  snapvault [-C directory] log [revision] [--oneline] [--limit n]");
        stream.println("  snapvault [-C directory] diff [from [to]]");
        stream.println("  snapvault [-C directory] restore <revision> [--to directory] [--force]");
        stream.println();
        stream.println("Revisions can be HEAD, HEAD~N, a full SHA-256 id, or a 7+ character prefix.");
        stream.println("With no revisions, diff compares HEAD to the working directory.");
        stream.println("With one revision, diff compares that snapshot to the working directory.");
    }

    private static final class UsageException extends Exception {
        private static final long serialVersionUID = 1L;

        private UsageException(String message) {
            super(message);
        }
    }
}
