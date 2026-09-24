package contracts

import "context"

// ExecResult is the real result of running a command in a sandbox session.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timed_out"`
}

// OK reports a zero exit code without a timeout.
func (r ExecResult) OK() bool { return r.ExitCode == 0 && !r.TimedOut }

// Snapshot is a handle to a persisted sandbox state.
type Snapshot struct {
	SnapshotID string `json:"snapshot_id"`
	Kind       string `json:"kind"`
}

// ExecOptions are the optional arguments of SandboxSession.Exec.
type ExecOptions struct {
	TimeoutS int               // 0 means the default (600s)
	Cwd      string            // "" means the session workdir
	Env      map[string]string // merged over the session environment
}

// DefaultExecTimeoutS mirrors the Python default for SandboxSession.exec.
const DefaultExecTimeoutS = 600

// SandboxSession is a live workspace rooted at Workdir.
type SandboxSession interface {
	Workdir() string
	// Exec runs argv (no shell) and captures its real output and exit code.
	Exec(ctx context.Context, argv []string, opts ExecOptions) (ExecResult, error)
	WriteFile(ctx context.Context, relpath, content string) error
	ReadFile(ctx context.Context, relpath string) (string, error)
	Close(ctx context.Context) error
}

// HostWorkdirSession is implemented by sessions whose Workdir is not a path on this machine (the
// Docker sandbox mounts the host workspace at /workspace): HostWorkdir is the host directory.
type HostWorkdirSession interface {
	HostWorkdir() string
}

// HostRoot is the workspace as a path on THIS machine, for code that reads it without the
// sandbox (harness integrity, the list_files / grep tools, the dispatcher's path checks). A
// session that mounts a host directory implements HostWorkdirSession; otherwise Workdir is
// already a host path (python: contracts.sandbox.host_root).
func HostRoot(s SandboxSession) string {
	if h, ok := s.(HostWorkdirSession); ok {
		if dir := h.HostWorkdir(); dir != "" {
			return dir
		}
	}
	return s.Workdir()
}

// Sandbox opens sandbox sessions.
type Sandbox interface {
	Name() string
	Open(ctx context.Context, workdir string, snapshotID string) (SandboxSession, error)
	Snapshot(ctx context.Context, session SandboxSession) (Snapshot, error)
}
