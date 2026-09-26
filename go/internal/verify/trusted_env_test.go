package verify

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Ported from the environment tests in python/tests/unit/test_trusted_runner.py.

func trustedRepo(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := state.InitRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("1"), 0o644)
	if _, err := state.CommitAll(ctx, dir, "base"); err != nil {
		t.Fatal(err)
	}
	commit, err := CandidateCommit(ctx, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	return dir, commit
}

func envOf(output string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

func TestTrustedCheckEnvIsScrubbed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh and env")
	}
	secrets := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-leak", "LHA_ANTHROPIC_API_KEY": "sk-lha-leak",
		"AWS_SECRET_ACCESS_KEY": "aws-leak", "GITHUB_TOKEN": "ghp-leak", "MY_SERVICE_SECRET": "svc-leak",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	t.Setenv("GOPROXY", "https://proxy.example")
	dir, commit := trustedRepo(t)
	res, err := NewCommandTrustedRunner().Run(context.Background(),
		contracts.Check{Name: "e2e", Command: []string{"env"}, Gating: true, Where: "trusted"}, dir, commit)
	if err != nil || !res.Passed {
		t.Fatalf("%+v %v", res, err)
	}
	env := envOf(res.OutputTail)
	for k, v := range secrets {
		if _, ok := env[k]; ok || strings.Contains(res.OutputTail, v) {
			t.Errorf("%s leaked into the trusted check", k)
		}
	}
	if _, ok := env["GOPROXY"]; ok {
		t.Error("GOPROXY passed without being allow-listed")
	}
	if env["PATH"] == "" || env["LHA_CHECK_NAME"] != "e2e" || env["LHA_CHECK_COMMIT"] != commit {
		t.Errorf("env: %v", env)
	}
	home := env["HOME"]
	userHome, _ := os.UserHomeDir()
	if home == userHome || !strings.HasPrefix(filepath.Base(home), "lha-trusted-home-") ||
		filepath.Dir(env["TMPDIR"]) != home {
		t.Errorf("HOME=%q TMPDIR=%q", home, env["TMPDIR"])
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("temporary home left behind: %v", err)
	}
}

func TestTrustedCheckAllowListedNamesPass(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh and env")
	}
	t.Setenv("GOPROXY", "https://proxy.example")
	t.Setenv("CI_HANDOFF_TOKEN", "tok")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-leak")
	dir, commit := trustedRepo(t)
	r, err := NewCommandTrustedRunnerWithEnv([]string{"GOPROXY", "CI_HANDOFF_TOKEN", "NOT_SET_ANYWHERE_X"})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := r.Run(context.Background(), contracts.Check{Name: "e2e", Command: []string{"env"}, Where: "trusted"}, dir, commit)
	env := envOf(res.OutputTail)
	if env["GOPROXY"] != "https://proxy.example" || env["CI_HANDOFF_TOKEN"] != "tok" {
		t.Errorf("env: %v", env)
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Error("ANTHROPIC_API_KEY leaked")
	}
	if _, ok := env["NOT_SET_ANYWHERE_X"]; ok {
		t.Error("an unset name was invented")
	}
	// The home starts empty (but for tmp) and is writable.
	script := `test "$(ls -A "$HOME")" = tmp && touch "$HOME/x" "$TMPDIR/y" && echo ok`
	res, _ = r.Run(context.Background(), contracts.Check{Name: "e2e", Command: []string{"sh", "-c", script}, Where: "trusted"}, dir, commit)
	if !res.Passed || res.OutputTail != "ok" {
		t.Fatalf("%+v", res)
	}
}

func TestEnvAllowListRefusesLHAAndInvalidNames(t *testing.T) {
	for _, name := range []string{"LHA_ANTHROPIC_API_KEY", "lha_x", "BAD-NAME", "", "1X", "FOO\n"} {
		if _, err := NewCommandTrustedRunnerWithEnv([]string{name}); err == nil ||
			!strings.HasPrefix(err.Error(), "LHA_TRUSTED_CHECK_ENV: ") {
			t.Errorf("%q: %v", name, err)
		}
	}
	_, err := ValidateEnvAllowList([]string{"LHA_POSTGRES_DSN"})
	if err == nil || err.Error() != "LHA_TRUSTED_CHECK_ENV: 'LHA_POSTGRES_DSN' cannot be passed to trusted checks (LHA_* settings hold LHA's own credentials)" {
		t.Error(err)
	}
	// A runner built by hand with a bad name fails the check instead of running it.
	dir, commit := trustedRepo(t)
	r := NewCommandTrustedRunner()
	r.EnvAllow = []string{"LHA_X"}
	res, _ := r.Run(context.Background(), contracts.Check{Name: "e2e", Command: []string{"true"}, Where: "trusted"}, dir, commit)
	if res.Passed || !strings.Contains(res.OutputTail, "LHA_TRUSTED_CHECK_ENV") {
		t.Fatalf("%+v", res)
	}
}

func TestTrustedEnvDefaultsAndOrder(t *testing.T) {
	base := map[string]string{"PATH": "/p", "LC_ALL": "C", "HOME": "/real", "FOO": "1", "SECRET_KEY": "s"}
	got := TrustedEnv("/h", "/h/tmp", []string{"FOO"}, map[string]string{"LHA_CHECK_NAME": "n"}, base)
	want := map[string]string{
		"PATH": "/p", "LC_ALL": "C", "LANG": "C.UTF-8", "HOME": "/h", "TMPDIR": "/h/tmp",
		"GIT_TERMINAL_PROMPT": "0", "FOO": "1", "LHA_CHECK_NAME": "n",
	}
	if runtime.GOOS == "windows" {
		want["USERPROFILE"], want["TEMP"], want["TMP"] = "/h", "/h/tmp", "/h/tmp"
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	if TrustedEnv("/h", "/t", nil, nil, map[string]string{})["PATH"] == "" {
		t.Error("no default PATH")
	}
}
