//go:build !unix

package pyval

import "syscall"

var strerrorTable = map[syscall.Errno]string{}

func errnoType(errno syscall.Errno) string {
	switch errno {
	case syscall.ENOENT:
		return "FileNotFoundError"
	case syscall.EACCES, syscall.EPERM:
		return "PermissionError"
	case syscall.EEXIST:
		return "FileExistsError"
	}
	return "OSError"
}
