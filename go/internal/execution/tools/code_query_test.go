package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
)

// Ported from python/tests/unit/test_code_query.py.

type querySession struct {
	contracts.SandboxSession
	res  contracts.ExecResult
	argv []string
}

func (s *querySession) Exec(_ context.Context, argv []string, _ contracts.ExecOptions) (contracts.ExecResult, error) {
	s.argv = argv
	return s.res, nil
}

func queryCtx(res contracts.ExecResult) (contracts.ToolContext, *querySession) {
	s := &querySession{res: res}
	return contracts.ToolContext{MissionID: "m", Session: s}, s
}

func TestCodeQueryReturnsTheAnswer(t *testing.T) {
	tctx, s := queryCtx(contracts.ExecResult{Stdout: "<callers of='f'/>"})
	res := CodeQueryTool{TokenBudget: 1500, TimeoutS: 5}.Run(context.Background(), map[string]any{"kind": "callers", "target": "f"}, tctx)
	if !res.OK || res.Content != "<callers of='f'/>" || strings.Join(s.argv, " ") != "ripwire . --callers=f" {
		t.Fatalf("%+v %v", res, s.argv)
	}
	bad := CodeQueryTool{}.Run(context.Background(), map[string]any{"kind": "callers", "target": ""}, tctx)
	if bad.OK || !strings.Contains(*bad.Error, "empty") {
		t.Fatalf("%+v", bad)
	}
}

func TestCodeQueryFailuresAreToolErrors(t *testing.T) {
	for want, res := range map[string]contracts.ExecResult{
		"needs ripwire":    {ExitCode: 127, Stderr: "sh: ripwire: not found"},
		"symbol not found": {ExitCode: 1, Stderr: "ripwire: --callers symbol not found: f"},
		"timed out":        {ExitCode: 124, TimedOut: true},
	} {
		tctx, _ := queryCtx(res)
		out := CodeQueryTool{TimeoutS: 5}.Run(context.Background(), map[string]any{"kind": "callers", "target": "f"}, tctx)
		if out.OK || !strings.Contains(*out.Error, want) {
			t.Errorf("%s: %+v", want, out)
		}
	}
}

func TestTheSettingRegistersCodeQuery(t *testing.T) {
	has := func(env ...string) bool {
		s, err := config.LoadFrom(env, "")
		if err != nil {
			t.Fatal(err)
		}
		list, err := RunTools(s, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range list {
			if tool.Spec().Name == "code_query" {
				spec := tool.Spec()
				if spec.Mutating || spec.Egress || spec.UntrustedInput {
					t.Fatal("code_query must be read-only with no egress")
				}
				return true
			}
		}
		return false
	}
	if has() || !has("LHA_CODE_QUERY=true") {
		t.Fatal("code_query registration does not follow LHA_CODE_QUERY")
	}
}

func TestRealRipwireAnswersCallers(t *testing.T) {
	if _, err := exec.LookPath("ripwire"); err != nil {
		t.Skip("needs ripwire on PATH")
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "lib.py"), []byte("def helper():\n    return 1\n\n\ndef caller():\n    return helper()\n"), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	session := execution.NewLocalSandboxSession(dir)
	tctx := contracts.ToolContext{MissionID: "m", Session: session}
	found := CodeQueryTool{TimeoutS: 30}.Run(context.Background(), map[string]any{"kind": "callers", "target": "helper"}, tctx)
	if !found.OK || !strings.Contains(found.Content, `n="caller"`) {
		t.Fatalf("%+v", found)
	}
	missing := CodeQueryTool{TimeoutS: 30}.Run(context.Background(), map[string]any{"kind": "callers", "target": "nope_xyz"}, tctx)
	if missing.OK || !strings.Contains(*missing.Error, "not found") {
		t.Fatalf("%+v", missing)
	}
}
