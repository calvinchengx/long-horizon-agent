package execution

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

func TestChildEnvIsAllowlisted(t *testing.T) {
	base := map[string]string{
		"PATH": "/bin", "LANG": "en_US.UTF-8", "ANTHROPIC_API_KEY": "sk-secret",
		"LHA_DATABASE_URL": "postgres://secret", "AWS_SECRET_ACCESS_KEY": "x", "HOME": "/Users/me",
	}
	env := ChildEnv("/work", map[string]string{"EXTRA": "1"}, base)
	if env["HOME"] != "/work" || env["PATH"] != "/bin" || env["EXTRA"] != "1" || env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatal(env)
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "LHA_DATABASE_URL", "AWS_SECRET_ACCESS_KEY"} {
		if _, ok := env[k]; ok {
			t.Fatal(k)
		}
	}
	if env := ChildEnv("/w", nil, map[string]string{}); env["PATH"] != defaultPath || env["LANG"] != "C.UTF-8" {
		t.Fatal(env)
	}
}

func TestRunProcDoesNotLeakHostSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-leak")
	t.Setenv("LHA_ANTHROPIC_API_KEY", "sk-leak2")
	tmp := t.TempDir()
	res, err := RunProc(context.Background(), []string{"env"}, tmp, ProcOptions{})
	if err != nil || !res.OK() {
		t.Fatal(res, err)
	}
	if strings.Contains(res.Stdout, "sk-leak") || !strings.Contains(res.Stdout, "HOME="+tmp) {
		t.Fatal(res.Stdout)
	}
}

func TestValidateTimeout(t *testing.T) {
	for _, bad := range []int{0, -5} {
		if _, err := ValidateTimeout(bad); err == nil {
			t.Error(bad)
		}
	}
	if _, err := ValidateTimeout(-5); err.Error() != "timeout_s must be positive, got -5" {
		t.Fatal(err)
	}
	if n, _ := ValidateTimeout(10); n != 10 {
		t.Fatal(n)
	}
	if n, _ := ValidateTimeout(1 << 30); n != MaxTimeoutS {
		t.Fatal(n)
	}
}

func TestBoundedBufferKeepsHeadAndTail(t *testing.T) {
	buf := NewBoundedBuffer(100)
	for i := 0; i < 100; i++ {
		buf.Write([]byte(strings.Repeat("x", 10)))
	}
	buf.Write([]byte("END"))
	text := buf.Text()
	if !strings.Contains(text, "truncated") || !strings.HasSuffix(text, "END") || len(text) >= 200 {
		t.Fatal(text)
	}
	want := strings.Repeat("x", 50) + "\n…[903 bytes truncated]…\n" + strings.Repeat("x", 47) + "END"
	if text != want {
		t.Fatalf("%q", text)
	}
	b := NewBoundedBuffer(100)
	b.Write([]byte("a\xe2\x82b\xff"))
	if b.Text() != "a�b�" {
		t.Fatalf("%q", b.Text())
	}
}

func TestRunProcOutputIsBounded(t *testing.T) {
	script := `i=0; while [ $i -lt 2000 ]; do printf '%1000s' y; i=$((i+1)); done; echo TAIL`
	res, err := RunProc(context.Background(), []string{"sh", "-c", script}, t.TempDir(), ProcOptions{MaxOutputBytes: 10_000})
	if err != nil || !res.OK() {
		t.Fatal(res, err)
	}
	if len(res.Stdout) >= 11_000 || !strings.HasSuffix(strings.TrimRight(res.Stdout, "\n"), "TAIL") {
		t.Fatal(len(res.Stdout))
	}
}

func TestRunProcExitCodesAndMissingProgram(t *testing.T) {
	tmp := t.TempDir()
	res, err := RunProc(context.Background(), []string{"sh", "-c", "echo out; echo err >&2; exit 3"}, tmp, ProcOptions{})
	if err != nil || res.ExitCode != 3 || res.Stdout != "out\n" || res.Stderr != "err\n" || res.OK() {
		t.Fatal(res, err)
	}
	res, _ = RunProc(context.Background(), []string{"definitely-not-a-program-xyz"}, tmp, ProcOptions{})
	if res.ExitCode != 127 || res.Stderr != "[Errno 2] No such file or directory: 'definitely-not-a-program-xyz'" {
		t.Fatal(res)
	}
	res, _ = RunProc(context.Background(), []string{"true"}, filepath.Join(tmp, "missing"), ProcOptions{})
	if res.ExitCode != 127 || res.Stderr != "[Errno 2] No such file or directory: '"+filepath.Join(tmp, "missing")+"'" {
		t.Fatal(res)
	}
	res, _ = RunProc(context.Background(), []string{"sh", "-c", "kill -TERM $$"}, tmp, ProcOptions{})
	if res.ExitCode != -int(syscall.SIGTERM) {
		t.Fatal(res)
	}
}

func TestLocalSandboxExecFileIOAndSnapshot(t *testing.T) {
	tmp := t.TempDir()
	sb := NewLocalSandbox()
	s := openLocal(t, tmp)
	ctx := context.Background()
	if err := s.WriteFile(ctx, "sub/hello.txt", "hi"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadFile(ctx, "sub/hello.txt"); err != nil || got != "hi" {
		t.Fatal(got, err)
	}
	ok, _ := s.Exec(ctx, []string{"sh", "-c", "echo ok"}, contracts.ExecOptions{})
	if !ok.OK() || !strings.Contains(ok.Stdout, "ok") {
		t.Fatal(ok)
	}
	sub, _ := s.Exec(ctx, []string{"pwd"}, contracts.ExecOptions{Cwd: "sub"})
	if strings.TrimSpace(sub.Stdout) != Realpath(filepath.Join(tmp, "sub")) {
		t.Fatal(sub)
	}
	if snap, err := sb.Snapshot(ctx, s); err != nil || snap.SnapshotID != "no-commit" {
		t.Fatal(snap, err)
	}
	if err := state.InitRepo(ctx, tmp); err != nil {
		t.Fatal(err)
	}
	sha, err := state.CommitAll(ctx, tmp, "init")
	if err != nil {
		t.Fatal(err)
	}
	if snap, err := sb.Snapshot(ctx, s); err != nil || snap.SnapshotID != sha || snap.Kind != "local" {
		t.Fatal(snap, err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFactory(t *testing.T) {
	_, err := BuildSandbox("local", SandboxOptions{})
	var unsafe *UnsafeSandboxError
	if err == nil || !asErr(err, &unsafe) {
		t.Fatal(err)
	}
	if sb, err := BuildSandbox(" Local ", SandboxOptions{AllowUnsafeLocal: true}); err != nil || sb.Name() != "local" {
		t.Fatal(sb, err)
	}
	if _, err := BuildSandbox("chroot", SandboxOptions{}); err == nil ||
		err.Error() != "unknown sandbox kind 'chroot'; expected one of ('local', 'docker', 'e2b')" {
		t.Fatal(err)
	}
	if _, err := BuildSandbox("e2b", SandboxOptions{}); err == nil || !strings.Contains(err.Error(), "not supported in Go yet") {
		t.Fatal(err)
	}
	sb, err := BuildSandbox("docker", SandboxOptions{DockerCLI: &fakeDocker{}})
	if err != nil || sb.(*DockerSandbox).Image() != DefaultDockerImage {
		t.Fatal(sb, err)
	}
	sb, _ = BuildSandbox("docker", SandboxOptions{Image: "custom:1", EgressHosts: []string{"pypi.org"}, DockerCLI: &fakeDocker{}})
	if d := sb.(*DockerSandbox); d.Image() != "custom:1" || d.EgressHosts()[0] != "pypi.org" {
		t.Fatal(d)
	}
	tmp := t.TempDir()
	if _, err := OpenSandbox(context.Background(), "local", tmp, "", SandboxOptions{}); err == nil {
		t.Fatal("opened without opt-in")
	}
	s, err := OpenSandbox(context.Background(), "local", tmp, "", SandboxOptions{AllowUnsafeLocal: true})
	if err != nil || s.Workdir() != tmp {
		t.Fatal(s, err)
	}
}
