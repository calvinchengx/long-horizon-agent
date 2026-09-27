package pyfmt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
)

type portedError struct{}

func (portedError) Error() string      { return "x" }
func (portedError) PyTypeName() string { return "BudgetExceeded" }

type OwnError struct{}

func (*OwnError) Error() string { return "own" }

func TestExcTypeNameMapsCommonGoErrorsToPythonClasses(t *testing.T) {
	_, notFound := os.Open("/definitely/not/here")
	_, missingExe := exec.LookPath("definitely-not-a-binary-lha")
	var syntax *json.SyntaxError
	syntaxErr := json.Unmarshal([]byte("{"), &map[string]any{})
	errors.As(syntaxErr, &syntax)
	_, numErr := strconv.Atoi("x")
	dial := &url.Error{Op: "Get", URL: "http://h", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}
	for _, c := range []struct {
		err  error
		want string
	}{
		{portedError{}, "BudgetExceeded"},
		{fmt.Errorf("wrapped: %w", portedError{}), "BudgetExceeded"},
		{notFound, "FileNotFoundError"},
		{&fs.PathError{Op: "open", Path: "p", Err: syscall.EACCES}, "PermissionError"},
		{&fs.PathError{Op: "open", Path: "p", Err: syscall.EEXIST}, "FileExistsError"},
		{missingExe, "FileNotFoundError"},
		{context.DeadlineExceeded, "TimeoutError"},
		{context.Canceled, "CancelledError"},
		{syntaxErr, "JSONDecodeError"},
		{numErr, "ValueError"},
		{dial, "ConnectError"},
		{&url.Error{Op: "Get", URL: "ftp://h", Err: errors.New("unsupported protocol scheme \"ftp\"")}, "UnsupportedProtocol"},
		{&url.Error{Op: "Post", URL: "http://h", Err: context.DeadlineExceeded}, "ReadTimeout"},
		{&OwnError{}, "OwnError"}, // an LHA type: its name mirrors the Python class
		{errors.New("plain"), FallbackExcType},
		{&json.MarshalerError{}, FallbackExcType}, // a stdlib type with no mapping
	} {
		if got := ExcTypeName(c.err); got != c.want {
			t.Errorf("ExcTypeName(%T %v) = %q, want %q", c.err, c.err, got, c.want)
		}
	}
	if ExcText(errors.New("boom")) != "RuntimeError: boom" {
		t.Fatal(ExcText(errors.New("boom")))
	}
}

func TestRegisteredNamer(t *testing.T) {
	sentinel := errors.New("driver failure")
	RegisterExcNamer(func(err error) string {
		if errors.Is(err, sentinel) {
			return "OperationalError"
		}
		return ""
	})
	if got := ExcTypeName(fmt.Errorf("x: %w", sentinel)); got != "OperationalError" {
		t.Fatal(got)
	}
	if got := ExcTypeName(errors.New("other")); got != FallbackExcType {
		t.Fatal(got)
	}
}
