package execution

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
)

// DockerCLI runs one `docker` command (args exclude the binary). It returns the command's exit
// code; err is non-nil only when the command could not be run at all (binary missing, ctx
// done). Tests inject a fake.
type DockerCLI interface {
	Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

// ExecDockerCLI runs the real docker binary (Binary, default "docker"), with the harness's own
// environment (DOCKER_HOST, contexts): it is the operator's tool, not agent code.
type ExecDockerCLI struct {
	Binary string
}

// Run implements DockerCLI.
func (c ExecDockerCLI) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	bin := c.Binary
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// DockerError is a failed docker command (python: docker.errors.APIError / DockerException).
type DockerError struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *DockerError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = "exit status " + itoa(e.ExitCode)
	}
	cmd := "docker"
	if len(e.Args) > 0 {
		cmd += " " + e.Args[0]
	}
	return cmd + " failed: " + msg
}

// PyTypeName names the Python exception type.
func (e *DockerError) PyTypeName() string { return "APIError" }

func itoa(n int) string { return pyval.Repr(n) }

// dockerOutput runs args and returns stdout, stderr and the exit code.
func dockerOutput(ctx context.Context, cli DockerCLI, stdin io.Reader, args ...string) (string, string, int, error) {
	var out, errBuf bytes.Buffer
	code, err := cli.Run(ctx, args, stdin, &out, &errBuf)
	return out.String(), errBuf.String(), code, err
}

// dockerMust runs args and returns stdout, or a *DockerError for a non-zero exit.
func dockerMust(ctx context.Context, cli DockerCLI, stdin io.Reader, args ...string) (string, error) {
	out, stderr, code, err := dockerOutput(ctx, cli, stdin, args...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", &DockerError{Args: args, ExitCode: code, Stderr: stderr}
	}
	return out, nil
}
