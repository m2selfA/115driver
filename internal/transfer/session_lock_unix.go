//go:build !windows

package transfer

import (
	"errors"
	"os"
	"syscall"
)

func tryPlatformFileLock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockPlatformFile(file *os.File) error {
	if file == nil {
		return nil
	}
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func isPlatformLockContention(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
