package execution

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// The Docker sandbox against a real Docker daemon (opt-in, like the Python integration tests):
// LHA_IT_DOCKER=1 go test ./internal/execution/ -run Docker_IT
// It pulls python:3.12-slim (and python:3.12-alpine for the egress proxy) on first use.

func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("LHA_IT_DOCKER") != "1" {
		t.Skip("set LHA_IT_DOCKER=1")
	}
}

func openDocker(t *testing.T, opts DockerOptions) contracts.SandboxSession {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	sb, err := NewDockerSandbox(opts)
	if err != nil {
		t.Fatal(err)
	}
	s, err := sb.Open(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func TestDocker_IT_ExitCodesTimeoutsAndIsolation(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	s := openDocker(t, DockerOptions{})
	run := func(timeoutS int, argv ...string) contracts.ExecResult {
		t.Helper()
		res, err := s.Exec(ctx, argv, contracts.ExecOptions{TimeoutS: timeoutS})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if ok := run(0, "sh", "-c", "echo hello"); !ok.OK() || strings.TrimSpace(ok.Stdout) != "hello" {
		t.Fatal(ok)
	}
	if failed := run(0, "sh", "-c", "echo oops >&2; exit 3"); failed.ExitCode != 3 || failed.TimedOut || !strings.Contains(failed.Stderr, "oops") {
		t.Fatal(failed)
	}
	if r := run(60, "sh", "-c", "exit 124"); r.ExitCode != 124 || r.TimedOut {
		t.Fatal(r)
	}
	if r := run(2, "sleep", "30"); !r.TimedOut || r.ExitCode != -1 {
		t.Fatal(r)
	}
	// The output stream ends immediately, but the process keeps running before it exits 5.
	if r := run(30, "sh", "-c", "exec >&- 2>&-; sleep 2; exit 5"); r.ExitCode != 5 || r.TimedOut {
		t.Fatal(r)
	}
	if r := run(0, "sh", "-c", "echo x > .git/hooks/pre-commit"); r.OK() {
		t.Fatal("harness dir writable", r)
	}
	if r := run(0, "sh", "-c", "echo x > scratch.txt"); !r.OK() {
		t.Fatal(r)
	}
	probe := "import socket\ntry:\n    socket.create_connection(('1.1.1.1', 53), timeout=3)\nexcept OSError:\n    raise SystemExit(7)\n"
	if r := run(30, "python", "-c", probe); r.ExitCode != 7 {
		t.Fatal("network reachable", r)
	}
	if err := s.WriteFile(ctx, "src/a.py", "x = 'é'\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadFile(ctx, "src/a.py"); err != nil || got != "x = 'é'\n" {
		t.Fatal(got, err)
	}
	host := contracts.HostRoot(s)
	if data, err := os.ReadFile(host + "/src/a.py"); err != nil || string(data) != "x = 'é'\n" {
		t.Fatal(string(data), err)
	}
	if r := run(0, "ln", "-s", "/etc", "innocent_link"); !r.OK() {
		t.Fatal(r)
	}
	if _, err := s.ReadFile(ctx, "innocent_link/passwd"); !isEscape(err) {
		t.Fatal(err)
	}
}

func TestDocker_IT_OutOfMemoryIsNotATimeout(t *testing.T) {
	requireDocker(t)
	s := openDocker(t, DockerOptions{MemLimit: "64m"})
	res, err := s.Exec(context.Background(), []string{"python", "-c", "b = bytearray(512 * 1024 * 1024); print(len(b))"},
		contracts.ExecOptions{TimeoutS: 60})
	if err != nil || res.ExitCode != 137 || res.TimedOut || !strings.Contains(res.Stderr, "SIGKILL") {
		t.Fatal(res, err)
	}
}

func egressResources(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, args := range [][]string{
		{"network", "ls", "--filter", "name=lha-egress-", "--format", "{{.Name}}"},
		{"ps", "-a", "--filter", "name=lha-egress-", "--format", "{{.Names}}"},
	} {
		raw, err := exec.Command("docker", args...).Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range strings.Fields(string(raw)) {
			if strings.HasPrefix(name, "lha-egress-") {
				out[name] = true
			}
		}
	}
	return out
}

func TestDocker_IT_EgressAllowList(t *testing.T) {
	requireDocker(t)
	before := egressResources(t)
	dir := t.TempDir()
	sb, err := NewDockerSandbox(DockerOptions{Image: DefaultDockerImage, EgressHosts: []string{"pypi.org", "files.pythonhosted.org"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := sb.Open(ctx, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	created := 0
	for name := range egressResources(t) {
		if !before[name] {
			created++
		}
	}
	if created < 2 {
		_ = s.Close(ctx)
		t.Fatal("expected a per-session proxy + network")
	}
	allowed, err := s.Exec(ctx, []string{"python", "-m", "pip", "download", "--no-deps", "--dest", "/tmp/dl", "six==1.16.0"},
		contracts.ExecOptions{TimeoutS: 180})
	denied, err2 := s.Exec(ctx, []string{"python", "-c",
		"import urllib.request\ntry:\n    urllib.request.urlopen('https://example.com', timeout=10)\nexcept Exception as e:\n    print(e); raise SystemExit(9)"},
		contracts.ExecOptions{TimeoutS: 60})
	if cerr := s.Close(ctx); cerr != nil {
		t.Error(cerr)
	}
	for name := range egressResources(t) {
		if !before[name] {
			t.Errorf("egress resource leaked: %s", name)
		}
	}
	if err != nil || !allowed.OK() {
		t.Fatal("allowed registry unreachable:", allowed, err)
	}
	if err2 != nil || denied.ExitCode != 9 || !strings.Contains(denied.Stdout, "403") {
		t.Fatal("non-allow-listed host reachable:", denied, err2)
	}
}
