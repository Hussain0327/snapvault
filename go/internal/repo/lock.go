package repo

import (
	"errors"
	"os"
	"syscall"
)

// repoLock is a process-level advisory lock that serializes working-tree
// scans and ref updates.
type repoLock struct {
	file *os.File
}

func acquireLock(path string) (*repoLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another SnapVault operation is already running")
	}
	return &repoLock{file: f}, nil
}

func (l *repoLock) close() error {
	defer l.file.Close()
	return syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
}
