//go:build windows

package lockfile

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// On Windows we use LockFileEx with LOCKFILE_EXCLUSIVE_LOCK and
// LOCKFILE_FAIL_IMMEDIATELY for non-blocking try-lock semantics that
// match flock(LOCK_EX|LOCK_NB) on Unix.
const (
	lockExclusive = windows.LOCKFILE_EXCLUSIVE_LOCK
	lockNonBlock  = windows.LOCKFILE_FAIL_IMMEDIATELY
	lockBytesLow  = ^uint32(0)
	lockBytesHigh = ^uint32(0)
)

func tryLock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), lockExclusive|lockNonBlock,
		0, lockBytesLow, lockBytesHigh, ol)
}

func unlock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0,
		lockBytesLow, lockBytesHigh, ol)
}

func isWouldBlock(err error) bool {
	// ERROR_LOCK_VIOLATION is what LockFileEx returns when another
	// process holds the lock and we asked not to wait.
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_IO_PENDING)
}
