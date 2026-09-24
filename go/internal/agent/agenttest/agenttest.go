// Package agenttest provides small fakes of the execution layer (a host "sandbox" session, a
// dispatcher with write_file/read_file tools, and a Toolbox bundling them) for tests of the agent
// loop, the runner and the CLI. They are test doubles, not a sandbox: commands run directly on
// the host in the workdir.
package agenttest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Session is a SandboxSession that runs argv on the host in Dir.
type Session struct {
	Dir string
}

// Workdir is the session's directory.
func (s *Session) Workdir() string { return s.Dir }

// Exec runs argv (no shell) and captures its output and exit code.
func (s *Session) Exec(ctx context.Context, argv []string, opts contracts.ExecOptions) (contracts.ExecResult, error) {
	if len(argv) == 0 {
		return contracts.ExecResult{}, errors.New("empty argv")
	}
	timeout := opts.TimeoutS
	if timeout <= 0 {
		timeout = contracts.DefaultExecTimeoutS
	}
	tctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, argv[0], argv[1:]...)
	cmd.Dir = s.Dir
	if opts.Cwd != "" {
		cmd.Dir = filepath.Join(s.Dir, opts.Cwd)
	}
	cmd.Env = os.Environ()
	for k, v := range opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := contracts.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if tctx.Err() == context.DeadlineExceeded {
		res.TimedOut, res.ExitCode = true, -1
		return res, nil
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return res, err
		}
		res.ExitCode = exitErr.ExitCode()
	}
	return res, nil
}

func (s *Session) path(rel string) (string, error) {
	if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
		return "", fmt.Errorf("path escapes the workspace: %s", rel)
	}
	return filepath.Join(s.Dir, rel), nil
}

// WriteFile writes content to relpath (creating parent directories).
func (s *Session) WriteFile(_ context.Context, relpath, content string) error {
	p, err := s.path(relpath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// ReadFile reads relpath.
func (s *Session) ReadFile(_ context.Context, relpath string) (string, error) {
	p, err := s.path(relpath)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	return string(data), err
}

// Close is a no-op.
func (s *Session) Close(context.Context) error { return nil }

// Dispatcher routes write_file / read_file to the session and records every call.
type Dispatcher struct {
	mu    sync.Mutex
	Calls []contracts.ToolCall
	// Events are returned (once) by DrainEvents, like a dispatcher that keeps gate events.
	Events []contracts.EventRecord
	// RecordDecision, when set, serves a record_decision tool (e.g. an anchor's RecordDecision).
	RecordDecision func(contracts.DecisionRecord) int
}

// Specs describes the two tools.
func (d *Dispatcher) Specs() []contracts.ToolSpec {
	return []contracts.ToolSpec{
		{Name: "read_file", Description: "Read a file.", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
		}, PathArgs: []string{}},
		{Name: "write_file", Description: "Write a file.", Mutating: true, Parameters: map[string]any{
			"type": "object", "properties": map[string]any{
				"content": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"},
			},
		}, PathArgs: []string{"path"}},
	}
}

// Dispatch runs the call against tctx.Session.
func (d *Dispatcher) Dispatch(ctx context.Context, call contracts.ToolCall, tctx contracts.ToolContext) contracts.ToolResult {
	d.mu.Lock()
	d.Calls = append(d.Calls, call)
	d.mu.Unlock()
	path, _ := call.Arguments["path"].(string)
	switch call.Name {
	case "write_file":
		content, _ := call.Arguments["content"].(string)
		if err := tctx.Session.WriteFile(ctx, path, content); err != nil {
			return contracts.Failure(err.Error())
		}
		return contracts.Success(fmt.Sprintf("wrote %d chars to %s", len(content), path))
	case "read_file":
		text, err := tctx.Session.ReadFile(ctx, path)
		if err != nil {
			return contracts.Failure(err.Error())
		}
		return contracts.Success(text)
	case "record_decision":
		if d.RecordDecision == nil {
			break
		}
		record := contracts.DecisionRecord{Affected: []string{}}
		record.Decision, _ = call.Arguments["decision"].(string)
		record.Rationale, _ = call.Arguments["rationale"].(string)
		record.AlternativesRejected, _ = call.Arguments["alternatives_rejected"].(string)
		if list, ok := call.Arguments["affected"].([]any); ok {
			for _, a := range list {
				if s, ok := a.(string); ok {
					record.Affected = append(record.Affected, s)
				}
			}
		}
		return contracts.Success(fmt.Sprintf("recorded decision #%d", d.RecordDecision(record)))
	}
	return contracts.Failure("unknown tool: " + call.Name)
}

// DrainEvents returns and clears Events.
func (d *Dispatcher) DrainEvents() []contracts.EventRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.Events
	d.Events = nil
	return out
}

// Toolbox bundles a Session and a Dispatcher (it satisfies agent.Toolbox).
type Toolbox struct {
	Sess   *Session
	Disp   *Dispatcher
	Closed bool
}

// NewToolbox returns a toolbox rooted at workdir.
func NewToolbox(workdir string) *Toolbox {
	return &Toolbox{Sess: &Session{Dir: workdir}, Disp: &Dispatcher{}}
}

// Session is the host session.
func (t *Toolbox) Session() contracts.SandboxSession { return t.Sess }

// Dispatcher is the fake dispatcher.
func (t *Toolbox) Dispatcher() contracts.ToolDispatcher { return t.Disp }

// Close marks the toolbox closed.
func (t *Toolbox) Close(context.Context) error {
	t.Closed = true
	return nil
}
