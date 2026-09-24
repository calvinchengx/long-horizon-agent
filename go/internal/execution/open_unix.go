//go:build unix

package execution

import (
	"io/fs"
	"os"
	"syscall"
)

// openNoFollow opens name with O_NOFOLLOW | O_NONBLOCK (a FIFO must not block the open; blocking
// mode is restored by verifyFD before any IO). Errors are *fs.PathError carrying the errno.
func openNoFollow(name string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(name, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, uint32(perm))
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func setBlocking(f *os.File) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) { setErr = syscall.SetNonblock(int(fd), false) }); err != nil {
		return err
	}
	return setErr
}
