package io.snapvault.core;

import java.io.IOException;
import java.nio.channels.FileChannel;
import java.nio.channels.FileLock;
import java.nio.channels.OverlappingFileLockException;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;

/** A process-level lock that serializes working-tree scans and ref updates. */
final class RepositoryLock implements AutoCloseable {
    private final FileChannel channel;
    private final FileLock lock;

    private RepositoryLock(FileChannel channel, FileLock lock) {
        this.channel = channel;
        this.lock = lock;
    }

    static RepositoryLock acquire(Path lockFile) throws IOException {
        FileChannel channel = FileChannel.open(
                lockFile, StandardOpenOption.CREATE, StandardOpenOption.WRITE);
        try {
            FileLock lock;
            try {
                lock = channel.tryLock();
            } catch (OverlappingFileLockException exception) {
                lock = null;
            }
            if (lock == null) {
                throw new IOException("Another SnapVault operation is already running");
            }
            return new RepositoryLock(channel, lock);
        } catch (IOException exception) {
            channel.close();
            throw exception;
        }
    }

    void ensureHeld() throws IOException {
        if (!lock.isValid()) {
            throw new IOException("SnapVault repository lock was lost");
        }
    }

    @Override
    public void close() throws IOException {
        try {
            lock.release();
        } finally {
            channel.close();
        }
    }
}
