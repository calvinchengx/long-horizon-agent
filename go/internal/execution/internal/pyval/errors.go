package pyval

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// Error is an error carrying a Python exception type name; Error() is str(exc).
type Error struct {
	Type string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// PyTypeName names the Python exception type.
func (e *Error) PyTypeName() string { return e.Type }

// Errorf-style constructors for the exception types the execution layer raises.
func NewError(typ, msg string) error { return &Error{Type: typ, Msg: msg} }

type pyTyped interface{ PyTypeName() string }

// ExcTypeName is type(exc).__name__ for err (pyfmt.ExcTypeName: the PyTypeName() of the first
// error in the chain that has one, the Python OSError subclass for an errno, ..., else
// "RuntimeError").
func ExcTypeName(err error) string { return pyfmt.ExcTypeName(err) }

var osErrorTypes = map[string]bool{
	"OSError": true, "FileNotFoundError": true, "PermissionError": true, "FileExistsError": true,
	"IsADirectoryError": true, "NotADirectoryError": true, "PathEscapeError": true,
	"TimeoutError": true, "ConnectionRefusedError": true, "InterruptedError": true,
}

// IsOSError reports whether err is (a subclass of) Python's OSError.
func IsOSError(err error) bool { return osErrorTypes[ExcTypeName(err)] }

// ExcText is f"{type(exc).__name__}: {exc}" (with OSError text for errno errors).
func ExcText(err error) string { return ExcTypeName(err) + ": " + OSErrorText(err) }

// OSErrorText is str(exc) of the Python OSError an errno-carrying Go error corresponds to
// ("[Errno 2] No such file or directory: '/path'"); other errors are returned as Error().
func OSErrorText(err error) string {
	var typed pyTyped
	if errors.As(err, &typed) {
		return err.Error()
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var syscallErr *os.SyscallError
	switch {
	case errors.As(err, &pathErr):
		return ErrnoText(errno, pathErr.Path)
	case errors.As(err, &linkErr):
		return ErrnoText(errno, linkErr.Old) + " -> " + contracts.PyRepr(linkErr.New)
	case errors.As(err, &syscallErr):
		return ErrnoText(errno, "")
	}
	return ErrnoText(errno, "")
}

// ErrnoText is str(OSError(errno, strerror, filename)).
func ErrnoText(errno syscall.Errno, filename string) string {
	text := "[Errno " + itoa(int(errno)) + "] " + Strerror(errno)
	if filename != "" {
		text += ": " + contracts.PyRepr(filename)
	}
	return text
}

// NewOSError returns an errno error Python would raise for filename (TypeName and OSErrorText
// render it as the matching OSError subclass).
func NewOSError(errno syscall.Errno, op, filename string) error {
	return &fs.PathError{Op: op, Path: filename, Err: errno}
}

// Strerror is C strerror(errno) (the text Python's OSError shows).
func Strerror(errno syscall.Errno) string {
	if s, ok := strerrorTable[errno]; ok {
		return s
	}
	s := errno.Error()
	if s == "" {
		return "Unknown error " + itoa(int(errno))
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
