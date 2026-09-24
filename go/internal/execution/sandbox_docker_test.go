package execution

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

func asErr[T error](err error, target *T) bool { return errors.As(err, target) }

// fakeDocker stands in for the docker CLI (no daemon needed).
type fakeDocker struct {
	mu          sync.Mutex
	calls       [][]string
	stdout      string // for `exec ... timeout ...`
	stderr      string
	exitCode    int
	realpath    string // what `realpath` prints ("" = echo the target)
	archives    map[string][]byte
	proxyLog    string
	proxyStatus string
	failSandbox bool
	block       bool // `exec ... timeout` blocks until ctx is done
}

func (f *fakeDocker) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.mu.Unlock()
	switch args[0] {
	case "run":
		if slices.Contains(args, "--name") {
			return 0, write(stdout, "proxyid\n")
		}
		if f.failSandbox {
			stderr.Write([]byte("Unable to find image: image not found\n"))
			return 125, nil
		}
		return 0, write(stdout, "cid\n")
	case "exec":
		if i := slices.Index(args, "timeout"); i >= 0 {
			if f.block {
				<-ctx.Done()
				return -1, ctx.Err()
			}
			stdout.Write([]byte(f.stdout))
			stderr.Write([]byte(f.stderr))
			return f.exitCode, nil
		}
		switch args[2] {
		case "realpath":
			out := f.realpath
			if out == "" {
				out = args[len(args)-1]
			}
			return 0, write(stdout, out+"\n")
		case "cat":
			return 0, write(stdout, "content")
		}
		return 0, nil
	case "cp":
		data, _ := io.ReadAll(stdin)
		f.mu.Lock()
		if f.archives == nil {
			f.archives = map[string][]byte{}
		}
		f.archives[args[2]] = data
		f.mu.Unlock()
		return 0, nil
	case "logs":
		log := f.proxyLog
		if log == "" {
			log = "INFO lha-egress-proxy listening on 0.0.0.0:3128\n"
		}
		return 0, write(stderr, log)
	case "inspect":
		status := f.proxyStatus
		if status == "" {
			status = "running"
		}
		return 0, write(stdout, status+"\n")
	case "commit":
		return 0, write(stdout, "sha256:abc\n")
	}
	return 0, nil
}

func write(w io.Writer, s string) error { _, err := w.Write([]byte(s)); return err }

func (f *fakeDocker) find(prefix ...string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			out = append(out, c)
		}
	}
	return out
}

func flagValues(args []string, flag string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestDockerContainerIsHardened(t *testing.T) {
	tmp := t.TempDir()
	os.Mkdir(filepath.Join(tmp, ".git"), 0o755)
	os.Mkdir(filepath.Join(tmp, ".lha"), 0o755)
	cli := &fakeDocker{}
	sb, err := NewDockerSandbox(DockerOptions{CLI: cli})
	if err != nil {
		t.Fatal(err)
	}
	s, err := sb.Open(context.Background(), tmp, "")
	if err != nil {
		t.Fatal(err)
	}
	host := Realpath(tmp)
	if contracts.HostRoot(s) != host || s.Workdir() != "/workspace" {
		t.Fatal(contracts.HostRoot(s), s.Workdir())
	}
	args := cli.find("run")[0]
	if !slices.Equal(args[len(args)-3:], []string{DockerDefaultImage, "sleep", "infinity"}) {
		t.Fatal(args)
	}
	checks := map[string][]string{
		"--network":      {"none"},
		"--cap-drop":     {"ALL"},
		"--security-opt": {"no-new-privileges:true"},
		"--memory":       {"2g"},
		"--memory-swap":  {"2g"},
		"--pids-limit":   {"512"},
		"--cpus":         {"2"},
		"--tmpfs":        {"/tmp:rw,exec,nosuid,nodev,size=1g"},
		"--workdir":      {"/workspace"},
		"-v": {host + ":/workspace:rw", host + "/.git:/workspace/.git:ro",
			host + "/.lha:/workspace/.lha:ro"},
		"-e": {"HOME=/workspace", "LANG=C.UTF-8"},
	}
	for flag, want := range checks {
		if got := flagValues(args, flag); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", flag, got, want)
		}
	}
	if !slices.Contains(args, "--read-only") {
		t.Fatal("root not read-only")
	}
	if user := flagValues(args, "--user")[0]; user == "" || strings.HasPrefix(user, "0:") {
		t.Fatal(user)
	}
	networked, _ := NewDockerSandbox(DockerOptions{CLI: cli, Network: true})
	if got := flagValues(networked.RunArgs("img", tmp, "", nil), "--network"); got[0] != "bridge" {
		t.Fatal(got)
	}
	if snap, err := sb.Snapshot(context.Background(), s); err != nil || snap.SnapshotID != "sha256:abc" || snap.Kind != "docker" {
		t.Fatal(snap, err)
	}
}

func TestDockerExecWrapsTimeoutEnvAndBoundsOutput(t *testing.T) {
	cli := &fakeDocker{stdout: strings.Repeat("a", 5000) + "END", stderr: "err"}
	s := NewDockerSandboxSession(cli, "c1", "", "", 1000, nil)
	res, err := s.Exec(context.Background(), []string{"pytest", "-q"}, contracts.ExecOptions{TimeoutS: 30, Env: map[string]string{"FOO": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	args := cli.find("exec")[0]
	i := slices.Index(args, "c1")
	if !slices.Equal(args[i+1:], []string{"timeout", "-k", "5", "30s", "pytest", "-q"}) {
		t.Fatal(args)
	}
	env := flagValues(args[:i], "-e")
	if !slices.Contains(env, "HOME=/workspace") || !slices.Contains(env, "FOO=1") || flagValues(args, "-w")[0] != "/workspace" {
		t.Fatal(env)
	}
	if !res.OK() || len(res.Stdout) >= 1200 || !strings.HasSuffix(res.Stdout, "END") || res.Stderr != "err" {
		t.Fatal(res)
	}
}

type ticks struct {
	mu  sync.Mutex
	out []time.Duration
}

func (tk *ticks) clock() time.Time {
	tk.mu.Lock()
	defer tk.mu.Unlock()
	d := tk.out[0]
	if len(tk.out) > 1 {
		tk.out = tk.out[1:]
	}
	return time.Unix(0, 0).Add(d)
}

func TestDockerExecExitCodes(t *testing.T) {
	ctx := context.Background()
	run := func(code int, timeoutS int, elapsed time.Duration) contracts.ExecResult {
		cli := &fakeDocker{stdout: "out", exitCode: code}
		tk := &ticks{out: []time.Duration{0, elapsed}}
		res, err := NewDockerSandboxSession(cli, "c1", "", "", 0, tk.clock).Exec(ctx, []string{"prog"}, contracts.ExecOptions{TimeoutS: timeoutS})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// The deadline (1s) has passed when the exit is read.
	if res := run(124, 1, 5*time.Second); !res.TimedOut || res.OK() || res.ExitCode != -1 || res.Stderr != "\n[timed out after 1s]" {
		t.Fatal(res)
	}
	if res := run(124, 2, 10*time.Second); !res.TimedOut || res.ExitCode != -1 {
		t.Fatal(res)
	}
	if res := run(124, 60, time.Second); res.TimedOut || res.ExitCode != 124 {
		t.Fatal(res)
	}
	if res := run(137, 60, time.Second); res.TimedOut || res.ExitCode != 137 ||
		res.Stderr != "\n[killed by SIGKILL (exit 137), e.g. out of memory]" {
		t.Fatal(res)
	}
	if res := run(3, 5, 0); res.ExitCode != 3 || res.TimedOut {
		t.Fatal(res)
	}
	s := NewDockerSandboxSession(&fakeDocker{}, "c1", "", "", 0, nil)
	if _, err := s.Exec(ctx, []string{"true"}, contracts.ExecOptions{TimeoutS: -1}); err == nil {
		t.Fatal("bad timeout accepted")
	}
	if _, err := s.Exec(ctx, []string{"true"}, contracts.ExecOptions{Cwd: "/etc"}); !isEscape(err) ||
		err.Error() != "path escapes the workspace: '../etc'" {
		t.Fatal(err)
	}
	cli := &fakeDocker{}
	s = NewDockerSandboxSession(cli, "c1", "", "", 0, nil)
	if _, err := s.Exec(ctx, []string{"true"}, contracts.ExecOptions{Cwd: "/workspace/src"}); err != nil {
		t.Fatal(err)
	}
	if w := flagValues(cli.find("exec")[0], "-w")[0]; w != "/workspace/src" {
		t.Fatal(w)
	}
}

func TestDockerHostDeadline(t *testing.T) {
	defer func(g time.Duration) { dockerHostGrace = g }(dockerHostGrace)
	dockerHostGrace = 0
	cli := &fakeDocker{block: true}
	s := NewDockerSandboxSession(cli, "c1", "", "", 0, nil)
	res, err := s.Exec(context.Background(), []string{"sleep", "999"}, contracts.ExecOptions{TimeoutS: 1})
	if err != nil || !res.TimedOut || res.ExitCode != -1 || res.Stderr != "[timed out after 1s (host deadline)]" || res.Stdout != "" {
		t.Fatal(res, err)
	}
	// Cancelling the caller's context is an error, not a timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Exec(ctx, []string{"sleep", "999"}, contracts.ExecOptions{TimeoutS: 1}); err == nil {
		t.Fatal("cancelled exec returned no error")
	}
}

func TestDockerFileIOIsContained(t *testing.T) {
	ctx := context.Background()
	cli := &fakeDocker{}
	s := NewDockerSandboxSession(cli, "c1", "", "", 0, nil)
	if _, err := s.ReadFile(ctx, "../etc/passwd"); !isEscape(err) {
		t.Fatal(err)
	}
	if err := s.WriteFile(ctx, "/etc/cron.d/x", "boom"); !isEscape(err) {
		t.Fatal(err)
	}
	if err := s.WriteFile(ctx, "src/a.py", "x = 1"); err != nil {
		t.Fatal(err)
	}
	data, ok := cli.archives["c1:/workspace/src"]
	if !ok {
		t.Fatal(cli.archives)
	}
	tr := tar.NewReader(bytes.NewReader(data))
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "a.py" || hdr.Mode != 0o644 {
		t.Fatal(hdr, err)
	}
	body, _ := io.ReadAll(tr)
	if string(body) != "x = 1" {
		t.Fatal(string(body))
	}
	if len(cli.find("exec", "c1", "mkdir", "-p", "--", "/workspace/src")) != 1 {
		t.Fatal(cli.calls)
	}
	if got, err := s.ReadFile(ctx, "src/a.py"); err != nil || got != "content" {
		t.Fatal(got, err)
	}
	cli.realpath = "/etc/shadow" // a symlink inside the workspace pointing out
	if _, err := s.ReadFile(ctx, "innocent_link"); !isEscape(err) {
		t.Fatal(err)
	}
}

func TestEgressHostsRouteTheSandboxThroughAnAllowListProxy(t *testing.T) {
	tmp := t.TempDir()
	cli := &fakeDocker{}
	sb, err := NewDockerSandbox(DockerOptions{Image: "my/sandbox:1", CLI: cli, EgressHosts: []string{"pypi.org", ".golang.org"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := sb.Open(context.Background(), tmp, "")
	if err != nil {
		t.Fatal(err)
	}
	netCreate := cli.find("network", "create")[0]
	network := netCreate[len(netCreate)-1]
	if !strings.HasPrefix(network, "lha-egress-") || !slices.Contains(netCreate, "--internal") {
		t.Fatal(netCreate)
	}
	runs := cli.find("run")
	proxyArgs, sandboxArgs := runs[0], runs[1]
	proxyName := flagValues(proxyArgs, "--name")[0]
	i := slices.Index(proxyArgs, DefaultProxyImage)
	if i < 0 || proxyArgs[i+1] != "python" || proxyArgs[i+2] != "-c" || !strings.Contains(proxyArgs[i+3], "class EgressProxy") {
		t.Fatal(proxyArgs)
	}
	if !slices.Contains(flagValues(proxyArgs, "-e"), "LHA_PROXY_ALLOW=pypi.org,.golang.org") ||
		flagValues(proxyArgs, "--network")[0] != "bridge" || flagValues(proxyArgs, "--cap-drop")[0] != "ALL" ||
		!slices.Contains(proxyArgs, "--read-only") {
		t.Fatal(proxyArgs)
	}
	if len(cli.find("network", "connect", network, proxyName)) != 1 {
		t.Fatal(cli.calls)
	}
	if !slices.Contains(sandboxArgs, "my/sandbox:1") || !slices.Equal(flagValues(sandboxArgs, "--network"), []string{network}) {
		t.Fatal(sandboxArgs)
	}
	env := flagValues(sandboxArgs, "-e")
	url := "http://" + proxyName + ":3128"
	for _, v := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if !slices.Contains(env, v+"="+url) {
			t.Fatal(env)
		}
	}
	if !slices.Contains(env, "NO_PROXY=localhost,127.0.0.1") || !slices.Contains(sandboxArgs, "--read-only") {
		t.Fatal(env)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cli.find("rm", "-f", "cid")) != 1 || len(cli.find("rm", "-f", proxyName)) != 1 ||
		len(cli.find("network", "rm", network)) != 1 {
		t.Fatal(cli.calls)
	}
}

func TestEmptyEgressListKeepsNetworkNone(t *testing.T) {
	cli := &fakeDocker{}
	sb, _ := NewDockerSandbox(DockerOptions{CLI: cli, EgressHosts: []string{"", " "}})
	if _, err := sb.Open(context.Background(), t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	if len(cli.find("network")) != 0 || len(cli.find("run")) != 1 {
		t.Fatal(cli.calls)
	}
	args := cli.find("run")[0]
	if flagValues(args, "--network")[0] != "none" {
		t.Fatal(args)
	}
	for _, e := range flagValues(args, "-e") {
		if strings.Contains(strings.ToUpper(e), "PROXY") {
			t.Fatal(e)
		}
	}
}

func TestEgressHostsAreValidatedAndExclusive(t *testing.T) {
	_, err := NewDockerSandbox(DockerOptions{CLI: &fakeDocker{}, Network: true, EgressHosts: []string{"pypi.org"}})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatal(err)
	}
	_, err = NewDockerSandbox(DockerOptions{CLI: &fakeDocker{}, EgressHosts: []string{"10.0.0.1"}})
	if err == nil || !strings.Contains(err.Error(), "invalid egress allow-list entry") {
		t.Fatal(err)
	}
}

func TestFailedOpenTearsDownProxyAndNetwork(t *testing.T) {
	cli := &fakeDocker{failSandbox: true}
	sb, _ := NewDockerSandbox(DockerOptions{CLI: cli, EgressHosts: []string{"pypi.org"}})
	_, err := sb.Open(context.Background(), t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "image not found") {
		t.Fatal(err)
	}
	if len(cli.find("network", "rm")) != 1 || len(cli.find("rm", "-f")) != 1 {
		t.Fatal(cli.calls)
	}
}

func TestProxyThatDiesAtStartupFailsOpenAndCleansUp(t *testing.T) {
	cli := &fakeDocker{proxyLog: "SyntaxError: boom", proxyStatus: "exited"}
	sb, _ := NewDockerSandbox(DockerOptions{CLI: cli, EgressHosts: []string{"pypi.org"}, Sleep: func(time.Duration) {}})
	_, err := sb.Open(context.Background(), t.TempDir(), "")
	if err == nil || err.Error() != "egress proxy exited during startup:\nSyntaxError: boom" {
		t.Fatal(err)
	}
	if len(cli.find("network", "rm")) != 1 || len(cli.find("rm", "-f")) != 1 || len(cli.find("run")) != 1 {
		t.Fatal(cli.calls)
	}
	cli = &fakeDocker{proxyLog: "starting"}
	tk := &ticks{out: []time.Duration{0, 0, 61 * time.Second}}
	sb, _ = NewDockerSandbox(DockerOptions{CLI: cli, EgressHosts: []string{"pypi.org"}, Sleep: func(time.Duration) {}, Clock: tk.clock})
	if _, err := sb.Open(context.Background(), t.TempDir(), ""); err == nil || err.Error() != "egress proxy not ready after 60.0s" {
		t.Fatal(err)
	}
}
