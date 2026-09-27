package pyfmt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode"
)

// Python messages embed exception class names (f"{type(exc).__name__}: {exc}"): "ConnectTimeout:
// ...", "FileNotFoundError: ...", "implementer failed: RuntimeError: ...". Go errors have no such
// class, so ExcTypeName names a Go error after the exception Python raises in the same situation.

// modulePath prefixes the LHA packages, whose error types are named after the Python classes they
// port (BudgetExceeded, OwnershipConflictError, HTTPStatusError, ...).
const modulePath = "github.com/calvinchengx/long-horizon-agent/go/"

// FallbackExcType is the name of an error nothing more specific is known about: a Go error built
// with errors.New / fmt.Errorf (or a third-party type) corresponds to the RuntimeError the Python
// code raises for a generic failure.
const FallbackExcType = "RuntimeError"

// ExcTypeName is Python's type(exc).__name__ for err. In order:
//
//   - the PyTypeName() of the first error in the chain that has one (errors that port a Python
//     class name themselves);
//   - an HTTP client failure (*url.Error): httpx's ConnectTimeout, ConnectError, ReadTimeout,
//     RemoteProtocolError, UnsupportedProtocol or ReadError;
//   - context.DeadlineExceeded -> TimeoutError, context.Canceled -> CancelledError;
//   - a missing executable -> FileNotFoundError, a failed command -> CalledProcessError;
//   - an errno (os / net failures) -> the OSError subclass Python maps it to (FileNotFoundError,
//     PermissionError, FileExistsError, IsADirectoryError, NotADirectoryError, TimeoutError,
//     ConnectionRefusedError, ConnectionResetError, BrokenPipeError, InterruptedError, ...), else
//     OSError; fs.ErrNotExist / ErrExist / ErrPermission without an errno likewise;
//   - JSON text errors -> JSONDecodeError, number parsing -> ValueError, io.EOF -> EOFError;
//   - an LHA error type (declared in this module) -> its Go type name, which mirrors the Python
//     class;
//   - anything else -> FallbackExcType ("RuntimeError").
func ExcTypeName(err error) string {
	if err == nil {
		return "NoneType"
	}
	var typed interface{ PyTypeName() string }
	if errors.As(err, &typed) {
		return typed.PyTypeName()
	}
	namersMu.RLock()
	registered := namers
	namersMu.RUnlock()
	for _, namer := range registered {
		if name := namer(err); name != "" {
			return name
		}
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return HTTPErrorTypeName(err)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "TimeoutError"
	case errors.Is(err, context.Canceled):
		return "CancelledError"
	case errors.Is(err, exec.ErrNotFound):
		return "FileNotFoundError"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return ErrnoTypeName(errno)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "FileNotFoundError"
	case errors.Is(err, fs.ErrExist):
		return "FileExistsError"
	case errors.Is(err, fs.ErrPermission):
		return "PermissionError"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "EOFError"
	}
	var exitErr *exec.ExitError
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var numErr *strconv.NumError
	switch {
	case errors.As(err, &exitErr):
		return "CalledProcessError"
	case errors.As(err, &syntaxErr):
		return "JSONDecodeError"
	case errors.As(err, &typeErr), errors.As(err, &numErr):
		return "ValueError"
	}
	if name := lhaTypeName(err); name != "" {
		return name
	}
	return FallbackExcType
}

var (
	namersMu sync.RWMutex
	namers   []func(error) string
)

// RegisterExcNamer adds a namer ExcTypeName consults right after PyTypeName(): a package whose
// errors come from a third-party driver (the Postgres store: psycopg's class names) registers one
// in init. A namer returns "" for errors it does not know.
func RegisterExcNamer(namer func(error) string) {
	namersMu.Lock()
	defer namersMu.Unlock()
	namers = append(namers, namer)
}

// ExcText is f"{type(exc).__name__}: {exc}".
func ExcText(err error) string { return ExcTypeName(err) + ": " + err.Error() }

// HTTPErrorTypeName names an HTTP client failure after the httpx exception Python would see: a
// failed or timed-out dial is ConnectError / ConnectTimeout, a timeout after connecting is
// ReadTimeout, a malformed response RemoteProtocolError, a non-http(s) URL UnsupportedProtocol,
// anything else on the wire ReadError.
func HTTPErrorTypeName(err error) string {
	var op *net.OpError
	dial := errors.As(err, &op) && op.Op == "dial"
	timeout := errors.Is(err, context.DeadlineExceeded)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		timeout = true
	}
	text := err.Error()
	switch {
	case dial && timeout:
		return "ConnectTimeout"
	case dial:
		return "ConnectError"
	case timeout:
		return "ReadTimeout"
	case strings.Contains(text, "unsupported protocol scheme"):
		return "UnsupportedProtocol"
	case strings.Contains(text, "malformed HTTP"):
		return "RemoteProtocolError"
	case strings.Contains(text, "tls:") || strings.Contains(text, "x509:"):
		return "ConnectError"
	}
	return "ReadError"
}

// lhaTypeName is the Go type name of an error declared in this module ("" otherwise).
func lhaTypeName(err error) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		t := reflect.TypeOf(e)
		for t != nil && t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t == nil || t.Name() == "" || !strings.HasPrefix(t.PkgPath()+"/", modulePath) {
			continue
		}
		if r := []rune(t.Name()); unicode.IsUpper(r[0]) {
			return t.Name()
		}
	}
	return ""
}
