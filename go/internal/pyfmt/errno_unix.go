//go:build unix

package pyfmt

import "syscall"

// ErrnoTypeName is the OSError subclass Python raises for errno (PEP 3151), else "OSError".
func ErrnoTypeName(errno syscall.Errno) string {
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
	case syscall.ECONNRESET:
		return "ConnectionResetError"
	case syscall.ECONNABORTED:
		return "ConnectionAbortedError"
	case syscall.EPIPE, syscall.ESHUTDOWN:
		return "BrokenPipeError"
	case syscall.EINTR:
		return "InterruptedError"
	case syscall.ECHILD:
		return "ChildProcessError"
	case syscall.ESRCH:
		return "ProcessLookupError"
	case syscall.EAGAIN, syscall.EALREADY, syscall.EINPROGRESS:
		return "BlockingIOError"
	}
	return "OSError"
}
