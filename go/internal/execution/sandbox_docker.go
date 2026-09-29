package execution

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/egressproxy"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Docker sandbox — real isolation behind the same Sandbox interface
// (python/src/lha/execution/sandbox_docker.py), driving the `docker` CLI.
//
// The host workdir is bind-mounted at /workspace (so the harness's git anchor / verifier see the
// agent's edits), and commands run inside a locked-down container:
//
//   - --network none by default: this is where egress default-deny is actually ENFORCED for
//     run_command. Opt in with Network (full network), or — better — give EgressHosts (an
//     operator allow-list such as pypi.org, files.pythonhosted.org): each session then gets its
//     own --internal network with no route out, plus a proxy container (egressproxy) that is the
//     only way out and forwards only to the allow-listed hosts. The sandbox gets HTTP(S)_PROXY
//     pointing at it; code that ignores the proxy has no route at all.
//   - resource limits (--memory, --pids-limit, --cpus), --cap-drop ALL, no-new-privileges, a
//     non-root user (the host uid:gid on POSIX so files stay owned by the operator, else
//     nobody), and a read-only root filesystem with a tmpfs /tmp;
//   - the harness-owned .git and .lha dirs are re-mounted read-only, so code in the container
//     cannot plant git hooks or rewrite the mission state;
//   - every exec is wrapped in coreutils `timeout` (plus a host-side deadline) and its output is
//     streamed into a bounded buffer;
//   - file IO paths are validated lexically and re-checked with `realpath` inside the container.
//
// Snapshots commit the container to an image, whose id the durable workflow stores.

const (
	dockerWorkdir       = "/workspace"
	dockerContainerPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	dockerTimeoutExit   = 124 // coreutils `timeout` exit status when the deadline hit
	dockerKilledExit    = 137 // 128 + SIGKILL (from `timeout -k`)
	defaultHostGrace    = 30 * time.Second
	dockerPollInterval  = 100 * time.Millisecond
	proxyReadyTimeoutS  = 60.0
	egressPrefix        = "lha-egress-"
)

// dockerHostGrace is added to the command timeout for the host-side deadline (tests shorten it).
var dockerHostGrace = defaultHostGrace

// DockerDefaultImage is the DockerSandbox default image (the factory passes DefaultDockerImage).
const DockerDefaultImage = "python:3.12-slim"

// DefaultProxyImage runs the egress proxy (the stdlib-only Python source).
const DefaultProxyImage = "python:3.12-alpine"

// ProxyPort is the egress proxy's port inside the per-session network.
const ProxyPort = 3128

// DockerOptions configures a DockerSandbox. Zero values mean the Python defaults.
type DockerOptions struct {
	Image       string   // default DockerDefaultImage
	Network     bool     // unrestricted default bridge (exclusive with EgressHosts)
	EgressHosts []string // allow-list routed through a per-session proxy
	ProxyImage  string   // default DefaultProxyImage
	// ProxyCommand overrides the proxy container's command (default: python -c <PythonSource>),
	// e.g. a Go binary serving egressproxy.ServeEnv. It must log egressproxy.ReadyMessage.
	ProxyCommand   []string
	MemLimit       string  // default "2g"
	PidsLimit      int     // default 512
	CPUs           float64 // default 2.0
	TmpSize        string  // size of the /tmp tmpfs (toolchain caches); default "1g"
	User           string  // default the host uid:gid (POSIX) or 65534:65534
	WritableRoot   bool    // default false: a read-only root filesystem
	MaxOutputBytes int     // default DefaultMaxOutputBytes
	CLI            DockerCLI
	// Clock / Sleep let tests drive the exec deadline and the proxy readiness poll.
	Clock func() time.Time
	Sleep func(time.Duration)
}

// DockerSandbox opens hardened containers. No Network and no EgressHosts (the default) means no
// network at all.
type DockerSandbox struct {
	opts        DockerOptions
	egressHosts []string
	nanoCPUs    int64
}

var _ contracts.Sandbox = (*DockerSandbox)(nil)

func defaultDockerUser() string {
	if runtime.GOOS == "windows" {
		return "65534:65534"
	}
	return strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
}

// NewDockerSandbox validates opts (a ValueError-typed error for Network together with
// EgressHosts, or a malformed allow-list) and returns the sandbox.
func NewDockerSandbox(opts DockerOptions) (*DockerSandbox, error) {
	var hosts []string
	for _, h := range opts.EgressHosts {
		if s := pystr.Strip(h); s != "" {
			hosts = append(hosts, s)
		}
	}
	if opts.Network && len(hosts) > 0 {
		return nil, pyval.NewError("ValueError",
			"network=True (unrestricted network) and egress_hosts (allow-list proxy) are mutually exclusive")
	}
	if len(hosts) > 0 {
		if _, err := egressproxy.ParseAllowList(hosts); err != nil {
			return nil, err
		}
	}
	if opts.Image == "" {
		opts.Image = DockerDefaultImage
	}
	if opts.ProxyImage == "" {
		opts.ProxyImage = DefaultProxyImage
	}
	if opts.MemLimit == "" {
		opts.MemLimit = "2g"
	}
	if opts.PidsLimit == 0 {
		opts.PidsLimit = 512
	}
	if opts.CPUs == 0 {
		opts.CPUs = 2.0
	}
	if opts.TmpSize == "" {
		opts.TmpSize = "1g"
	}
	if opts.User == "" {
		opts.User = defaultDockerUser()
	}
	if opts.MaxOutputBytes == 0 {
		opts.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if opts.CLI == nil {
		opts.CLI = ExecDockerCLI{}
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	return &DockerSandbox{opts: opts, egressHosts: hosts, nanoCPUs: int64(opts.CPUs * 1_000_000_000)}, nil
}

// Name is "docker".
func (*DockerSandbox) Name() string { return "docker" }

// Image is the configured image.
func (s *DockerSandbox) Image() string { return s.opts.Image }

// EgressHosts is the (stripped, non-empty) egress allow-list.
func (s *DockerSandbox) EgressHosts() []string { return append([]string(nil), s.egressHosts...) }

// protectedMounts are the read-only binds over the writable work tree for everything git trusts
// in it: .git and .lha whether a directory OR a file. In a linked worktree .git is a
// "gitdir: <path>" FILE, and a writable one would let sandboxed code repoint the host's next
// git add/commit at a repository it planted. The git dir it names normally lies outside the
// mount (the parent repository's .git/worktrees/<name>); should it (or its common dir) lie inside
// the work tree, that directory is bound read-only too.
func protectedMounts(host string) []string {
	var args []string
	for _, name := range ProtectedDirs { // sorted: .git, .lha
		if _, err := os.Lstat(posixJoin(host, name)); err == nil { // a dangling symlink fails the run: closed
			args = append(args, "-v", posixJoin(host, name)+":"+dockerWorkdir+"/"+name+":ro")
		}
	}
	for _, inner := range state.GitDirsInTree(host) {
		rel, err := filepath.Rel(host, inner)
		if err != nil {
			continue
		}
		args = append(args, "-v", inner+":"+dockerWorkdir+"/"+filepath.ToSlash(rel)+":ro")
	}
	return args
}

// RunArgs is the full `docker run` argument list for a session container (exposed for
// inspection / tests). With egressNetwork the container joins ONLY that (internal) network
// instead of --network none/bridge, and proxyEnv is added to its environment.
func (s *DockerSandbox) RunArgs(image, workdir, egressNetwork string, proxyEnv map[string]string) []string {
	host := Realpath(workdir)
	args := []string{"run", "-d", "--workdir", dockerWorkdir, "--user", s.opts.User}
	env := map[string]string{"HOME": dockerWorkdir, "LANG": "C.UTF-8"}
	for k, v := range proxyEnv {
		env[k] = v
	}
	for _, k := range pyval.SortedKeys(env) {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args, "-v", host+":"+dockerWorkdir+":rw")
	args = append(args, protectedMounts(host)...)
	switch {
	case egressNetwork != "":
		args = append(args, "--network", egressNetwork)
	case s.opts.Network:
		args = append(args, "--network", "bridge")
	default:
		args = append(args, "--network", "none")
	}
	args = append(args,
		"--memory", s.opts.MemLimit,
		"--memory-swap", s.opts.MemLimit,
		"--pids-limit", strconv.Itoa(s.opts.PidsLimit),
		"--cpus", strconv.FormatFloat(float64(s.nanoCPUs)/1e9, 'f', -1, 64),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		// A reaping PID 1: commands run via docker exec, so orphans (a timed-out command's killed
		// grandchildren) are re-parented to PID 1, and sleep never reaps them.
		"--init",
	)
	if !s.opts.WritableRoot {
		args = append(args, "--read-only")
	}
	// exec: Docker mounts tmpfs noexec by default, which breaks every toolchain that builds then
	// runs a binary there (go test). Code already runs from /workspace, so this adds nothing an
	// agent could not do anyway. Go/uv/pnpm caches live here too; the size counts against --memory.
	args = append(args, "--tmpfs", "/tmp:rw,exec,nosuid,nodev,size="+s.opts.TmpSize)
	args = append(args, "--label", OwnerLabel+"="+owner())
	return append(args, image, "sleep", "infinity")
}

// Open starts a container for workdir (or restores snapshotID, an image id).
func (s *DockerSandbox) Open(ctx context.Context, workdir, snapshotID string) (contracts.SandboxSession, error) {
	image := snapshotID
	if image == "" {
		image = s.opts.Image
	}
	SweepOrphans(ctx, s.opts.CLI)
	if err := mkdirParents(workdir); err != nil {
		return nil, err
	}
	host := Realpath(workdir)
	if len(s.egressHosts) == 0 {
		out, err := dockerMust(ctx, s.opts.CLI, nil, s.RunArgs(image, workdir, "", nil)...)
		if err != nil {
			return nil, err
		}
		return s.session(strings.TrimSpace(out), host, nil), nil
	}
	token := make([]byte, 6)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	gate := &egressGate{cli: s.opts.CLI, token: hex.EncodeToString(token), clock: s.opts.Clock, sleep: s.opts.Sleep}
	if err := gate.create(ctx, s.opts.ProxyImage, s.opts.ProxyCommand, s.egressHosts); err != nil {
		gate.teardown()
		return nil, err
	}
	out, err := dockerMust(ctx, s.opts.CLI, nil, s.RunArgs(image, workdir, gate.networkName(), gate.proxyEnv())...)
	if err != nil {
		gate.teardown()
		return nil, err
	}
	session := s.session(strings.TrimSpace(out), host, gate.teardown)
	session.egressLog = gate.logs
	return session, nil
}

func (s *DockerSandbox) session(containerID, host string, onClose func()) *DockerSandboxSession {
	return &DockerSandboxSession{
		cli:         s.opts.CLI,
		containerID: containerID,
		workdir:     dockerWorkdir,
		hostWorkdir: host,
		maxOutput:   s.opts.MaxOutputBytes,
		onClose:     onClose,
		clock:       s.opts.Clock,
	}
}

// Snapshot commits the container to an image and returns its id.
func (s *DockerSandbox) Snapshot(ctx context.Context, session contracts.SandboxSession) (contracts.Snapshot, error) {
	ds, ok := session.(*DockerSandboxSession)
	if !ok {
		return contracts.Snapshot{}, pyval.NewError("AssertionError", "not a docker session")
	}
	out, err := dockerMust(ctx, ds.cli, nil, "commit", ds.containerID)
	if err != nil {
		return contracts.Snapshot{}, err
	}
	return contracts.Snapshot{SnapshotID: strings.TrimSpace(out), Kind: "docker"}, nil
}

// DockerSandboxSession is one running container.
type DockerSandboxSession struct {
	cli         DockerCLI
	containerID string
	workdir     string
	hostWorkdir string
	maxOutput   int
	onClose     func() // tears down per-session egress resources (proxy + network)
	clock       func() time.Time
	egressLog   func(context.Context) string // the egress proxy's log (nil: no egress)
	egressSeen  egressproxy.ProxyLogCursor
}

// DrainEgressEvents is the sandbox_egress events for the proxy requests since the last drain
// (none without an egress allow-list) (python: drain_egress_events).
func (s *DockerSandboxSession) DrainEgressEvents(ctx context.Context) []contracts.EventRecord {
	if s.egressLog == nil {
		return nil
	}
	return s.egressSeen.Drain(s.egressLog(ctx))
}

var (
	_ contracts.SandboxSession     = (*DockerSandboxSession)(nil)
	_ contracts.HostWorkdirSession = (*DockerSandboxSession)(nil)
)

// NewDockerSandboxSession wraps an existing container (tests, or reattaching). workdir "" means
// /workspace; maxOutput 0 means DefaultMaxOutputBytes; clock nil means time.Now.
func NewDockerSandboxSession(cli DockerCLI, containerID, workdir, hostWorkdir string, maxOutput int, clock func() time.Time) *DockerSandboxSession {
	if workdir == "" {
		workdir = dockerWorkdir
	}
	if maxOutput == 0 {
		maxOutput = DefaultMaxOutputBytes
	}
	if clock == nil {
		clock = time.Now
	}
	return &DockerSandboxSession{cli: cli, containerID: containerID, workdir: workdir,
		hostWorkdir: hostWorkdir, maxOutput: maxOutput, clock: clock}
}

// Workdir is the mount point inside the container (/workspace).
func (s *DockerSandboxSession) Workdir() string { return s.workdir }

// HostWorkdir is the host directory bind-mounted at Workdir (contracts.HostRoot).
func (s *DockerSandboxSession) HostWorkdir() string { return s.hostWorkdir }

// ContainerID is the container's id.
func (s *DockerSandboxSession) ContainerID() string { return s.containerID }

// containerPath lexically contains relpath, then confirms it with realpath inside the container.
func (s *DockerSandboxSession) containerPath(ctx context.Context, relpath string) (string, error) {
	target, err := ContainedPosix(s.workdir, relpath)
	if err != nil {
		return "", err
	}
	out, stderr, code, err := dockerOutput(ctx, s.cli, nil, "exec", s.containerID, "realpath", "-m", "--", target)
	if err != nil {
		return "", err
	}
	if code == 0 {
		real := pystr.Strip(pyval.DecodeUTF8Replace([]byte(out + stderr)))
		if real != "" && real != s.workdir && !strings.HasPrefix(real, s.workdir+"/") {
			return "", escapeErr("path escapes the workspace: " + contracts.PyRepr(relpath))
		}
	}
	return target, nil
}

func (s *DockerSandboxSession) env(extra map[string]string) map[string]string {
	env := map[string]string{
		"PATH":                dockerContainerPath,
		"HOME":                s.workdir,
		"LANG":                "C.UTF-8",
		"LC_ALL":              "C.UTF-8",
		"TMPDIR":              "/tmp",
		"GIT_TERMINAL_PROMPT": "0",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// posixRelpath is posixpath.relpath(p, start) for absolute paths.
func posixRelpath(p, start string) string {
	split := func(x string) []string {
		var out []string
		for _, c := range strings.Split(posixNormpath(x), "/") {
			if c != "" {
				out = append(out, c)
			}
		}
		return out
	}
	a, b := split(start), split(p)
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	parts := make([]string, 0, len(a)-i+len(b)-i)
	for range a[i:] {
		parts = append(parts, "..")
	}
	parts = append(parts, b[i:]...)
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}

// Exec runs argv in the container wrapped in `timeout -k 5 <T>s`, with a host-side deadline of
// T + 30s. Exit 124/137 is a timeout only if the deadline actually passed (otherwise it is the
// program's own status; 137 is typically an OOM kill).
func (s *DockerSandboxSession) Exec(ctx context.Context, argv []string, opts contracts.ExecOptions) (contracts.ExecResult, error) {
	timeoutS := opts.TimeoutS
	if timeoutS == 0 {
		timeoutS = contracts.DefaultExecTimeoutS
	}
	timeout, err := ValidateTimeout(timeoutS)
	if err != nil {
		return contracts.ExecResult{}, err
	}
	workdir := s.workdir
	if opts.Cwd != "" {
		rel := opts.Cwd
		if strings.HasPrefix(opts.Cwd, "/") {
			rel = posixRelpath(posixNormpath(opts.Cwd), s.workdir)
		}
		if workdir, err = ContainedPosix(s.workdir, rel); err != nil {
			return contracts.ExecResult{}, err
		}
	}
	args := []string{"exec", "-w", workdir}
	env := s.env(opts.Env)
	for _, k := range pyval.SortedKeys(env) {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args, s.containerID, "timeout", "-k", "5", strconv.Itoa(timeout)+"s")
	args = append(args, argv...)

	hostCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second+dockerHostGrace)
	defer cancel()
	started := s.clock()
	out, errBuf := NewBoundedBuffer(s.maxOutput), NewBoundedBuffer(s.maxOutput)
	code, err := s.cli.Run(hostCtx, args, nil, out, errBuf)
	if err != nil {
		if hostCtx.Err() != nil && ctx.Err() == nil {
			return contracts.ExecResult{ExitCode: -1, TimedOut: true,
				Stderr: fmt.Sprintf("[timed out after %ds (host deadline)]", timeout)}, nil
		}
		return contracts.ExecResult{}, err
	}
	deadlineHit := s.clock().Sub(started) >= time.Duration(timeout)*time.Second
	timedOut := deadlineHit && (code == dockerTimeoutExit || code == dockerKilledExit)
	note := ""
	if timedOut {
		note = fmt.Sprintf("\n[timed out after %ds]", timeout)
	} else if code == dockerKilledExit {
		note = "\n[killed by SIGKILL (exit 137), e.g. out of memory]"
	}
	if timedOut {
		code = -1
	}
	return contracts.ExecResult{ExitCode: code, Stdout: out.Text(), Stderr: errBuf.Text() + note, TimedOut: timedOut}, nil
}

// WriteFile writes content at relpath (contained) via a tar stream into the container.
func (s *DockerSandboxSession) WriteFile(ctx context.Context, relpath, content string) error {
	target, err := s.containerPath(ctx, relpath)
	if err != nil {
		return err
	}
	parent, name := path.Split(target)
	parent = strings.TrimRight(parent, "/")
	if parent == "" {
		parent = "/"
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	data := []byte(content)
	if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0o644,
		Typeflag: tar.TypeReg, Format: tar.FormatPAX, ModTime: time.Unix(0, 0)}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if _, _, _, err := dockerOutput(ctx, s.cli, nil, "exec", s.containerID, "mkdir", "-p", "--", parent); err != nil {
		return err
	}
	_, err = dockerMust(ctx, s.cli, &archive, "cp", "-", s.containerID+":"+parent)
	return err
}

// ReadFile reads relpath (contained) from the container (invalid UTF-8 replaced).
func (s *DockerSandboxSession) ReadFile(ctx context.Context, relpath string) (string, error) {
	target, err := s.containerPath(ctx, relpath)
	if err != nil {
		return "", err
	}
	out, stderr, code, err := dockerOutput(ctx, s.cli, nil, "exec", s.containerID, "cat", "--", target)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", pyval.NewError("OSError", "cannot read "+contracts.PyRepr(relpath)+" in container")
	}
	return pyval.DecodeUTF8Replace([]byte(out + stderr)), nil
}

// Close stops and removes the container, then tears down the session's egress resources.
func (s *DockerSandboxSession) Close(ctx context.Context) error {
	defer func() {
		if s.onClose != nil {
			onClose := s.onClose
			s.onClose = nil
			onClose()
		}
	}()
	if _, err := dockerMust(ctx, s.cli, nil, "stop", "-t", "5", s.containerID); err != nil {
		return err
	}
	_, err := dockerMust(ctx, s.cli, nil, "rm", "-f", s.containerID)
	return err
}

// egressGate is the per-session egress resources: an --internal network and the proxy
// container. teardown is best-effort and idempotent.
type egressGate struct {
	cli   DockerCLI
	token string
	clock func() time.Time
	sleep func(time.Duration)
}

func (g *egressGate) networkName() string { return egressPrefix + g.token }
func (g *egressGate) proxyName() string   { return egressPrefix + "proxy-" + g.token }
func (g *egressGate) proxyURL() string {
	return "http://" + g.proxyName() + ":" + strconv.Itoa(ProxyPort)
}

func (g *egressGate) proxyEnv() map[string]string {
	url := g.proxyURL()
	noProxy := "localhost,127.0.0.1"
	return map[string]string{
		"HTTP_PROXY": url, "HTTPS_PROXY": url, "http_proxy": url, "https_proxy": url,
		"NO_PROXY": noProxy, "no_proxy": noProxy,
	}
}

func (g *egressGate) create(ctx context.Context, proxyImage string, command, egressHosts []string) error {
	if _, err := dockerMust(ctx, g.cli, nil, "network", "create", "--driver", "bridge", "--internal",
		"--label", "lha.egress=network", "--label", OwnerLabel+"="+owner(), g.networkName()); err != nil {
		return err
	}
	if len(command) == 0 {
		command = []string{"python", "-c", egressproxy.PythonSource}
	}
	env := map[string]string{
		"LHA_PROXY_ALLOW":         strings.Join(egressHosts, ","),
		"LHA_PROXY_PORT":          strconv.Itoa(ProxyPort),
		"PYTHONDONTWRITEBYTECODE": "1",
		"PYTHONUNBUFFERED":        "1",
	}
	keys := pyval.SortedKeys(env)
	sort.Strings(keys)
	args := []string{"run", "-d", "--name", g.proxyName(),
		// Default bridge = the way out; the internal network is joined below.
		"--network", "bridge"}
	for _, k := range keys {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args,
		"--label", "lha.egress=proxy",
		"--label", OwnerLabel+"="+owner(),
		"--user", "65534:65534",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--read-only",
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=16m",
		"--memory", "256m",
		"--pids-limit", "128",
		proxyImage)
	args = append(args, command...)
	if _, err := dockerMust(ctx, g.cli, nil, args...); err != nil {
		return err
	}
	if _, err := dockerMust(ctx, g.cli, nil, "network", "connect", g.networkName(), g.proxyName()); err != nil {
		return err
	}
	return g.waitReady(ctx)
}

func (g *egressGate) waitReady(ctx context.Context) error {
	deadline := g.clock().Add(time.Duration(proxyReadyTimeoutS * float64(time.Second)))
	for {
		out, stderr, _, err := dockerOutput(ctx, g.cli, nil, "logs", g.proxyName())
		if err != nil {
			return err
		}
		logs := pyval.DecodeUTF8Replace([]byte(out + stderr))
		if strings.Contains(logs, egressproxy.ReadyMessage) {
			return nil
		}
		status, err := dockerMust(ctx, g.cli, nil, "inspect", "-f", "{{.State.Status}}", g.proxyName())
		if err != nil {
			return err
		}
		if st := strings.TrimSpace(status); st == "exited" || st == "dead" {
			return pyval.NewError("RuntimeError", "egress proxy exited during startup:\n"+pyval.Tail(logs, 2000))
		}
		if !g.clock().Before(deadline) {
			return pyval.NewError("RuntimeError",
				"egress proxy not ready after "+pyval.FloatRepr(proxyReadyTimeoutS)+"s")
		}
		g.sleep(dockerPollInterval)
	}
}

// logs is the proxy container's log so far ("" once it is gone).
func (g *egressGate) logs(ctx context.Context) string {
	out, stderr, code, err := dockerOutput(ctx, g.cli, nil, "logs", g.proxyName())
	if err != nil || code != 0 {
		return ""
	}
	return pyval.DecodeUTF8Replace([]byte(out + stderr))
}

func (g *egressGate) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, _, _, _ = dockerOutput(ctx, g.cli, nil, "rm", "-f", g.proxyName())
	_, _, _, _ = dockerOutput(ctx, g.cli, nil, "network", "rm", g.networkName())
}

// OwnerLabel marks every container and network a session creates with the process that opened
// it ("host:pid"; python: sandbox_docker.OWNER_LABEL), so a later Open can remove what a killed
// process left behind.
const OwnerLabel = "lha.owner"

func owner() string {
	host, _ := os.Hostname()
	return host + ":" + strconv.Itoa(os.Getpid())
}

// OwnerIsGone reports whether owner ("host:pid") is a process on this host that no longer
// exists. Another host's owner, a malformed label, or a process that exists (even another
// user's) is never gone; a reused pid keeps an orphan until that process ends too.
func OwnerIsGone(owner string) bool {
	i := strings.LastIndex(owner, ":")
	if i < 0 {
		return false
	}
	host, _ := os.Hostname()
	pid, err := strconv.Atoi(owner[i+1:])
	if owner[:i] != host || err != nil || pid <= 0 || strings.ContainsAny(owner[i+1:], "+-") {
		return false
	}
	return processGone(pid)
}

// SweepOrphans removes the containers, then the networks, whose lha.owner has died (best
// effort; python: sweep_orphans) and returns the names removed. Only resources this code
// labelled are considered, so containers of other projects, and of LHA processes still running,
// are never touched.
func SweepOrphans(ctx context.Context, cli DockerCLI) []string {
	var removed []string
	sweep := func(list []string, remove ...string) {
		out, err := dockerMust(ctx, cli, nil, list...)
		if err != nil {
			return
		}
		for _, line := range strings.Split(out, "\n") {
			name, label, ok := strings.Cut(strings.TrimSpace(line), "\t")
			if !ok || !OwnerIsGone(label) {
				continue
			}
			if _, err := dockerMust(ctx, cli, nil, append(remove, name)...); err == nil {
				removed = append(removed, name)
			}
		}
	}
	format := "{{.Names}}\t{{.Label \"" + OwnerLabel + "\"}}"
	sweep([]string{"ps", "-a", "--filter", "label=" + OwnerLabel, "--format", format}, "rm", "-f")
	format = "{{.Name}}\t{{.Label \"" + OwnerLabel + "\"}}"
	sweep([]string{"network", "ls", "--filter", "label=" + OwnerLabel, "--format", format}, "network", "rm")
	return removed
}
