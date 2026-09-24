//go:build darwin || linux

package hitl

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

// IsTerminal reports whether r is a terminal (python: stream.isatty(); /dev/null is not one).
func IsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok || f == nil {
		return false
	}
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(ioctlGetTermios), uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
