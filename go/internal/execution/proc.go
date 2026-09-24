package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
)

// A small subprocess runner shared by the sandbox and the verifier
// (python/src/lha/execution/proc.py). It never invokes a shell (argv only). Hardening:
//
//   - Minimal environment (ChildEnv): children never inherit the host environment, so API keys
//     and LHA_* settings cannot leak into agent-run code. Only an allowlist (PATH, locale, TMPDIR,
//     a few OS essentials) is copied, HOME is pointed at the workspace, and callers may add
//     explicit extras.
//   - Bounded output: stdout/stderr are drained concurrently into BoundedBuffers that keep the
//     head and tail and drop the middle.
//   - Validated, capped timeout: positive, capped at MaxTimeoutS.
//   - Whole-tree kill: POSIX children start in a new session; on timeout the whole process group
//     is SIGKILL-ed, not just the direct child.

// MaxTimeoutS caps every command timeout (seconds).
const MaxTimeoutS = 3600

// DefaultMaxOutputBytes is the per-stream output cap (head + tail kept).
const DefaultMaxOutputBytes = 1_000_000

const readChunk = 65_536

// inheritedEnv are the host variables a child may inherit; everything else is dropped.
var inheritedEnv = []string{
	"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TERM", "UV_CACHE_DIR",
	// Windows: subprocesses fail to start without these.
	"SYSTEMROOT", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "WINDIR",
}

const defaultPath = "/usr/local/bin:/usr/bin:/bin"

// ChildEnv builds the minimal environment for a child process rooted at home (the workspace).
// base is the source environment (nil = the process environment); only allowlisted names are
// copied from it. extra entries are the caller's explicit choice and are applied last.
func ChildEnv(home string, extra map[string]string, base map[string]string) map[string]string {
	if base == nil {
		base = environMap()
	}
	env := map[string]string{}
	for _, k := range inheritedEnv {
		if v, ok := base[k]; ok {
			env[k] = v
		}
	}
	if _, ok := env["PATH"]; !ok {
		env["PATH"] = defaultPath
	}
	if _, ok := env["LANG"]; !ok {
		env["LANG"] = "C.UTF-8"
	}
	env["HOME"] = home
	env["TMPDIR"] = base["TMPDIR"]
	if env["TMPDIR"] == "" {
		env["TMPDIR"] = gettempdir(base)
	}
	// Keep tool caches out of the workspace (HOME) so they are not committed with the agent's work.
	env["XDG_CACHE_HOME"] = base["XDG_CACHE_HOME"]
	if env["XDG_CACHE_HOME"] == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			userHome = "/"
		}
		env["XDG_CACHE_HOME"] = posixJoin(userHome, ".cache")
	}
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["PYTHONDONTWRITEBYTECODE"] = "1"
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// gettempdir is tempfile.gettempdir(): TMPDIR/TEMP/TMP, else the first usable standard dir.
func gettempdir(base map[string]string) string {
	for _, k := range []string{"TMPDIR", "TEMP", "TMP"} {
		if v := base[k]; v != "" {
			return v
		}
	}
	for _, d := range []string{"/tmp", "/var/tmp", "/usr/tmp"} {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return os.TempDir()
}

func environMap() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// ValidateTimeout returns a safe timeout: a positive int capped at MaxTimeoutS. A non-positive
// value is a ValueError-typed error.
func ValidateTimeout(timeoutS int) (int, error) {
	if timeoutS <= 0 {
		return 0, pyval.NewError("ValueError", fmt.Sprintf("timeout_s must be positive, got %d", timeoutS))
	}
	return min(timeoutS, MaxTimeoutS), nil
}

// BoundedBuffer accumulates bytes but keeps at most limit of them: the head and the most recent
// tail. It is safe for concurrent use.
type BoundedBuffer struct {
	mu        sync.Mutex
	headLimit int
	tailLimit int
	head      []byte
	tail      []byte
	Dropped   int
}

// NewBoundedBuffer returns a buffer keeping at most limit bytes.
func NewBoundedBuffer(limit int) *BoundedBuffer {
	return &BoundedBuffer{headLimit: limit / 2, tailLimit: limit - limit/2}
}

// Write implements io.Writer (it never fails).
func (b *BoundedBuffer) Write(chunk []byte) (int, error) {
	n := len(chunk)
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.headLimit - len(b.head); room > 0 {
		take := min(room, len(chunk))
		b.head = append(b.head, chunk[:take]...)
		chunk = chunk[take:]
	}
	if len(chunk) == 0 {
		return n, nil
	}
	b.tail = append(b.tail, chunk...)
	if excess := len(b.tail) - b.tailLimit; excess > 0 {
		b.tail = append(b.tail[:0:0], b.tail[excess:]...)
		b.Dropped += excess
	}
	return n, nil
}

// Text decodes the kept bytes (UTF-8, invalid bytes replaced), marking any dropped middle.
func (b *BoundedBuffer) Text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	head := pyval.DecodeUTF8Replace(b.head)
	tail := pyval.DecodeUTF8Replace(b.tail)
	if b.Dropped > 0 {
		return fmt.Sprintf("%s\n…[%d bytes truncated]…\n%s", head, b.Dropped, tail)
	}
	return head + tail
}

// ProcOptions are the optional arguments of RunProc.
type ProcOptions struct {
	TimeoutS       int               // 0 = the default (600)
	Env            map[string]string // extra child environment
	MaxOutputBytes int               // 0 = DefaultMaxOutputBytes
	Home           string            // "" = cwd
}

// RunProc runs argv in cwd and returns a real ExecResult (a non-zero exit is not an error). The
// child gets ChildEnv(home or cwd, extra=Env) — never the full host environment. An invalid
// timeout is a ValueError-typed error; a program that cannot be started is exit code 127 with
// the Python OSError text as stderr.
func RunProc(ctx context.Context, argv []string, cwd string, opts ProcOptions) (contracts.ExecResult, error) {
	timeoutS := opts.TimeoutS
	if timeoutS == 0 {
		timeoutS = contracts.DefaultExecTimeoutS
	}
	timeout, err := ValidateTimeout(timeoutS)
	if err != nil {
		return contracts.ExecResult{}, err
	}
	if len(argv) == 0 {
		return contracts.ExecResult{}, pyval.NewError("IndexError", "list index out of range")
	}
	limit := opts.MaxOutputBytes
	if limit == 0 {
		limit = DefaultMaxOutputBytes
	}
	home := opts.Home
	if home == "" {
		home = cwd
	}
	env := ChildEnv(home, opts.Env, nil)

	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		errno := syscall.ENOENT
		if err == nil {
			errno = syscall.ENOTDIR
		} else {
			var e syscall.Errno
			if errors.As(err, &e) {
				errno = e
			}
		}
		return contracts.ExecResult{ExitCode: 127, Stderr: pyval.ErrnoText(errno, cwd)}, nil
	}
	program, err := lookPath(argv[0], env["PATH"], cwd)
	if err != nil {
		return contracts.ExecResult{ExitCode: 127, Stderr: pyval.OSErrorText(err)}, nil
	}

	out, errBuf := NewBoundedBuffer(limit), NewBoundedBuffer(limit)
	cmd := exec.Command(program, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Dir = cwd
	cmd.Env = envList(env)
	cmd.SysProcAttr = newSessionAttr()
	outR, outW, err := os.Pipe()
	if err != nil {
		return contracts.ExecResult{}, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return contracts.ExecResult{}, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{outR, outW, errR, errW} {
			f.Close()
		}
		return contracts.ExecResult{ExitCode: 127, Stderr: startErrorText(err, argv[0])}, nil
	}
	outW.Close()
	errW.Close()
	var readers sync.WaitGroup
	for _, pair := range []struct {
		r *os.File
		w io.Writer
	}{{outR, out}, {errR, errBuf}} {
		readers.Add(1)
		go func(r *os.File, w io.Writer) {
			defer readers.Done()
			buf := make([]byte, readChunk)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					w.Write(buf[:n])
				}
				if err != nil {
					return
				}
			}
		}(pair.r, pair.w)
	}

	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	timedOut := false
	timer := time.NewTimer(time.Duration(timeout) * time.Second)
	defer timer.Stop()
	select {
	case <-waited:
	case <-timer.C:
		timedOut = true
		killTree(cmd.Process)
		<-waited
	case <-ctx.Done():
		killTree(cmd.Process)
		<-waited
	}
	// Reap stragglers that kept the group alive after the leader exited.
	killGroup(cmd.Process)
	done := make(chan struct{})
	go func() {
		readers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	outR.Close()
	errR.Close()

	stderr := errBuf.Text()
	if timedOut {
		stderr += fmt.Sprintf("\n[timed out after %ds]", timeout)
	}
	code := exitCode(cmd.ProcessState)
	if timedOut {
		code = -1
	}
	if ctx.Err() != nil && !timedOut {
		return contracts.ExecResult{ExitCode: code, Stdout: out.Text(), Stderr: stderr}, ctx.Err()
	}
	return contracts.ExecResult{ExitCode: code, Stdout: out.Text(), Stderr: stderr, TimedOut: timedOut}, nil
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, k := range pyval.SortedKeys(env) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// lookPath resolves file the way CPython's subprocess does on POSIX: a name containing a slash
// is used as-is (relative to cwd); otherwise each directory of the CHILD's PATH is tried. The
// error is the Python OSError (filename = the name as given).
func lookPath(file, pathEnv, cwd string) (string, error) {
	if strings.Contains(file, "/") {
		full := file
		if !strings.HasPrefix(file, "/") {
			full = posixJoin(cwd, file)
		}
		if err := checkExecutable(full); err != nil {
			return "", &fs.PathError{Op: "exec", Path: file, Err: err}
		}
		return full, nil
	}
	var firstErr error = syscall.ENOENT
	sawEACCES := false
	for _, dir := range strings.Split(pathEnv, ":") {
		if dir == "" {
			dir = "."
		}
		candidate := posixJoin(dir, file)
		if !strings.HasPrefix(candidate, "/") {
			candidate = posixJoin(cwd, candidate)
		}
		err := checkExecutable(candidate)
		if err == nil {
			return candidate, nil
		}
		if errors.Is(err, syscall.EACCES) {
			sawEACCES = true
		}
	}
	if sawEACCES {
		firstErr = syscall.EACCES
	}
	return "", &fs.PathError{Op: "exec", Path: file, Err: firstErr}
}

func checkExecutable(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) {
			return errno
		}
		return syscall.ENOENT
	}
	if st.IsDir() || st.Mode()&0o111 == 0 {
		return syscall.EACCES
	}
	return nil
}

func startErrorText(err error, name string) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return pyval.ErrnoText(errno, name)
	}
	return err.Error()
}
