// Package execution is the execution plane: sandboxes, workspace path containment, process
// execution and the tool dispatcher (python/src/lha/execution). Concrete tools live in the
// tools subpackage; the Docker sandbox's egress proxy in egressproxy.
//
// Observable strings (tool results, error messages, gate questions, event payloads) match the
// Python implementation byte for byte where the inputs are the same; see the package-level docs
// of each file for the documented exceptions (Go maps have no insertion order, Go's regexp is
// RE2, and OS/transport error texts are reproduced only for the common errnos).
package execution

import (
	"context"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
)

// SandboxKinds are the sandbox kinds BuildSandbox accepts.
var SandboxKinds = []string{"local", "docker", "e2b"}

// DefaultDockerImage matches Settings.SandboxImage's default: Python 3.12 + uv (see
// sandbox/Dockerfile for Go/Node).
const DefaultDockerImage = "ghcr.io/astral-sh/uv:python3.12-bookworm-slim"

// UnsafeSandboxError is returned when the unisolated local sandbox is requested without an
// explicit opt-in.
type UnsafeSandboxError struct{ Msg string }

func (e *UnsafeSandboxError) Error() string { return e.Msg }

// PyTypeName names the Python exception type.
func (e *UnsafeSandboxError) PyTypeName() string { return "UnsafeSandboxError" }

// E2BUnsupportedError is returned for the "e2b" sandbox: the Go implementation has no E2B
// client (E2B ships SDKs for Python and JavaScript only).
type E2BUnsupportedError struct{}

func (E2BUnsupportedError) Error() string {
	return "the 'e2b' sandbox is not supported in Go yet (E2B has no Go SDK); use " +
		"sandbox='docker', or run this mission with the Python implementation"
}

// PyTypeName names the exception type.
func (E2BUnsupportedError) PyTypeName() string { return "NotImplementedError" }

// SandboxOptions are BuildSandbox's options.
type SandboxOptions struct {
	// AllowUnsafeLocal is required to get "local"; otherwise *UnsafeSandboxError.
	AllowUnsafeLocal bool
	// Network (docker only): false (default) runs containers with --network none.
	Network bool
	// EgressHosts (docker only): a non-empty allow-list routes the sandbox's egress through a
	// per-session proxy that reaches only those hosts (".example.org" = domain + subdomains).
	// Mutually exclusive with Network.
	EgressHosts []string
	// Image is the docker image (default DefaultDockerImage).
	Image string
	// Memory, CPUs and TmpSize (docker only) are the container's limits and its /tmp tmpfs
	// size; zero values keep the defaults.
	Memory  string
	CPUs    float64
	TmpSize string
	// Template is the E2B template (unused: E2B is not supported in Go).
	Template string
	// DockerCLI overrides the docker command runner (tests).
	DockerCLI DockerCLI
}

// BuildSandbox returns the Sandbox for kind ("local" | "docker" | "e2b"). It returns a
// ValueError-typed error for an unknown kind or for Network with EgressHosts, and
// E2BUnsupportedError for "e2b".
func BuildSandbox(kind string, opts SandboxOptions) (contracts.Sandbox, error) {
	switch pystr.Lower(pystr.Strip(kind)) {
	case "local":
		if !opts.AllowUnsafeLocal {
			return nil, &UnsafeSandboxError{Msg: "the 'local' sandbox runs agent commands directly on the host with no isolation " +
				"or network restriction; use sandbox='docker' (or 'e2b'), or opt in explicitly " +
				"with allow_unsafe_local=True (LHA_ALLOW_UNSAFE_LOCAL=true)"}
		}
		return NewLocalSandbox(), nil
	case "docker":
		image := opts.Image
		if image == "" {
			image = DefaultDockerImage
		}
		return NewDockerSandbox(DockerOptions{Image: image, Network: opts.Network,
			EgressHosts: opts.EgressHosts, MemLimit: opts.Memory, CPUs: opts.CPUs,
			TmpSize: opts.TmpSize, CLI: opts.DockerCLI})
	case "e2b":
		return nil, E2BUnsupportedError{}
	}
	return nil, pyval.NewError("ValueError", "unknown sandbox kind "+contracts.PyRepr(kind)+
		"; expected one of ('local', 'docker', 'e2b')")
}

// OpenSandbox is BuildSandbox then Open(workdir, snapshotID) in one call.
func OpenSandbox(ctx context.Context, kind, workdir, snapshotID string, opts SandboxOptions) (contracts.SandboxSession, error) {
	sandbox, err := BuildSandbox(kind, opts)
	if err != nil {
		return nil, err
	}
	return sandbox.Open(ctx, workdir, snapshotID)
}
