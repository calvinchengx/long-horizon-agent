//go:build !unix

package pyfmt

import "syscall"

// ErrnoTypeName is the OSError subclass Python raises for errno (PEP 3151), else "OSError".
// Windows system errors mostly reach ExcTypeName through the fs.Err* sentinels instead.
func ErrnoTypeName(errno syscall.Errno) string {
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
