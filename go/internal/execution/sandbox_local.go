package execution

import (
	"context"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// LocalSandbox runs commands directly on the host in a working directory
// (python/src/lha/execution/sandbox_local.py).
//
// For development, CI and the offline/$0 path. NOT isolated: commands run as the host user with
// host network access, so it must only run trusted code. Run paths obtain sandboxes through
// BuildSandbox, which refuses "local" unless the operator explicitly opts in
// (allow_unsafe_local).
//
// What it DOES enforce: file IO is confined to the workdir (no absolute paths, .. or symlink
// escapes) and children get a minimal environment with no host secrets (RunProc). What it does
// NOT enforce: network egress and filesystem access by arbitrary commands — use the Docker
// sandbox for that. Snapshots are the workdir's git HEAD.
type LocalSandbox struct{}

var _ contracts.Sandbox = (*LocalSandbox)(nil)

// NewLocalSandbox returns the host-local sandbox (callers normally go through BuildSandbox).
func NewLocalSandbox() *LocalSandbox { return &LocalSandbox{} }

// Name is "local".
func (*LocalSandbox) Name() string { return "local" }

// Open creates workdir if needed and returns a session rooted there (snapshotID, a git sha, is
// honoured by the caller via git checkout).
func (*LocalSandbox) Open(_ context.Context, workdir, _ string) (contracts.SandboxSession, error) {
	if err := mkdirParents(workdir); err != nil {
		return nil, err
	}
	return &LocalSandboxSession{workdir: workdir}, nil
}

// Snapshot returns the workdir's HEAD sha ("no-commit" when there is none).
func (*LocalSandbox) Snapshot(ctx context.Context, session contracts.SandboxSession) (contracts.Snapshot, error) {
	sha, err := state.HeadSHA(ctx, session.Workdir())
	if err != nil {
		return contracts.Snapshot{}, err
	}
	if sha == "" {
		sha = "no-commit"
	}
	return contracts.Snapshot{SnapshotID: sha, Kind: "local"}, nil
}

// LocalSandboxSession is a host-local working directory.
type LocalSandboxSession struct {
	workdir string
}

var _ contracts.SandboxSession = (*LocalSandboxSession)(nil)

// NewLocalSandboxSession returns a session rooted at workdir (which must exist).
func NewLocalSandboxSession(workdir string) *LocalSandboxSession {
	return &LocalSandboxSession{workdir: workdir}
}

// Workdir is the workspace root (a host path).
func (s *LocalSandboxSession) Workdir() string { return s.workdir }

// Exec runs argv via RunProc with HOME = the workdir; opts.Cwd may be relative to the workdir or
// absolute inside it, never outside (*PathEscapeError).
func (s *LocalSandboxSession) Exec(ctx context.Context, argv []string, opts contracts.ExecOptions) (contracts.ExecResult, error) {
	cwd, err := s.containedCwd(opts.Cwd)
	if err != nil {
		return contracts.ExecResult{}, err
	}
	return RunProc(ctx, argv, cwd, ProcOptions{TimeoutS: opts.TimeoutS, Env: opts.Env, Home: s.workdir})
}

func (s *LocalSandboxSession) containedCwd(cwd string) (string, error) {
	if cwd == "" {
		return s.workdir, nil
	}
	root, err := resolvePath(s.workdir)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(cwd, "/") {
		resolved, err := resolvePath(cwd)
		if err != nil {
			return "", err
		}
		if !IsRelativeTo(resolved, root) {
			return "", escapeErr("cwd escapes the workspace: " + contracts.PyRepr(cwd))
		}
		return resolved, nil
	}
	return ResolveWithin(root, cwd)
}

// WriteFile writes content to relpath inside the workdir (WriteTextWithin).
func (s *LocalSandboxSession) WriteFile(_ context.Context, relpath, content string) error {
	_, err := WriteTextWithin(s.workdir, relpath, content)
	return err
}

// ReadFile reads relpath inside the workdir (ReadTextWithin).
func (s *LocalSandboxSession) ReadFile(_ context.Context, relpath string) (string, error) {
	return ReadTextWithin(s.workdir, relpath)
}

// Close is a no-op.
func (s *LocalSandboxSession) Close(context.Context) error { return nil }
