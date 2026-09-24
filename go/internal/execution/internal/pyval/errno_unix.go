//go:build unix

package pyval

import (
	"runtime"
	"syscall"
)

// strerrorTable holds the C library's strerror texts for the errnos file and process operations
// raise (glibc and Darwin agree on these except where noted).
var strerrorTable = func() map[syscall.Errno]string {
	t := map[syscall.Errno]string{
		syscall.EPERM:        "Operation not permitted",
		syscall.ENOENT:       "No such file or directory",
		syscall.EIO:          "Input/output error",
		syscall.E2BIG:        "Argument list too long",
		syscall.ENOEXEC:      "Exec format error",
		syscall.EBADF:        "Bad file descriptor",
		syscall.EACCES:       "Permission denied",
		syscall.EEXIST:       "File exists",
		syscall.ENOTDIR:      "Not a directory",
		syscall.EISDIR:       "Is a directory",
		syscall.EINVAL:       "Invalid argument",
		syscall.EMFILE:       "Too many open files",
		syscall.ETXTBSY:      "Text file busy",
		syscall.ENOSPC:       "No space left on device",
		syscall.EROFS:        "Read-only file system",
		syscall.ENAMETOOLONG: "File name too long",
		syscall.ELOOP:        "Too many levels of symbolic links",
		syscall.ECONNREFUSED: "Connection refused",
		syscall.EHOSTUNREACH: "No route to host",
		syscall.ENETUNREACH:  "Network is unreachable",
	}
	if runtime.GOOS == "darwin" {
		t[syscall.ENXIO] = "Device not configured"
		t[syscall.EBUSY] = "Resource busy"
		t[syscall.ETIMEDOUT] = "Operation timed out"
	} else {
		t[syscall.ENXIO] = "No such device or address"
		t[syscall.EBUSY] = "Device or resource busy"
		t[syscall.ETIMEDOUT] = "Connection timed out"
	}
	return t
}()

func errnoType(errno syscall.Errno) string {
	switch errno {
	case syscall.ENOENT:
		return "FileNotFoundError"
	case syscall.EACCES, syscall.EPERM:
		return "PermissionError"
	case syscall.EEXIST:
		return "FileExistsError"
	case syscall.EISDIR:
		return "IsADirectoryError"
	case syscall.ENOTDIR:
		return "NotADirectoryError"
	case syscall.ETIMEDOUT:
		return "TimeoutError"
	case syscall.ECONNREFUSED:
		return "ConnectionRefusedError"
	}
	return "OSError"
}
