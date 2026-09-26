//go:build unix

package durable

import (
	"errors"
	"os"
	"syscall"
)

// flockFile takes an exclusive, non-blocking BSD flock on f: the same lock Python's
// fcntl.flock(fd, LOCK_EX | LOCK_NB) takes, so a Go and a Python process exclude each other on
// the same lock file. It reports (false, nil) when another holder has it.
func flockFile(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

func funlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
