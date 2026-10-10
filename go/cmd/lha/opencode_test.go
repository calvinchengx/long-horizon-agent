package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/model/opencodetest"
)

// End-to-end tests of LHA_LEAD_ENGINE=opencode and LHA_MODEL_BACKEND=opencode through the built lha
// binary, the real local sandbox and the real dispatcher (gates, record_decision). The fake
// opencode is this test binary (opencodetest). When uv is on PATH the Python lha runs the same
// mission against the same fake, and the argv, the injected config (incl. the system prompt and the
// MCP server entry), the prompt and the committed workspace must match.

// normalizeOpenCodeValue applies norm to every string in a decoded JSON value (the injected config
// is a nested map).
func normalizeOpenCodeValue(v any, norm func(string) string) any {
	switch x := v.(type) {
	case string:
		return norm(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalizeOpenCodeValue(e, norm)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalizeOpenCodeValue(e, norm)
		}
		return out
	}
	return v
}

// normalizeOpenCodeCall masks what legitimately differs between two runs: the bridge's token and
// port, commit shas, mission ids and the temp directory (in the config's system prompt too).
func normalizeOpenCodeCall(c opencodetest.Call, dir string) opencodetest.Call {
	norm := func(s string) string {
		s = strings.ReplaceAll(s, dir, "<DIR>")
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			s = strings.ReplaceAll(s, real, "<DIR>")
		}
		s = bearerRE.ReplaceAllString(s, "Bearer <TOKEN>")
		s = portRE.ReplaceAllString(s, "127.0.0.1:<PORT>")
		s = missionIDRE.ReplaceAllString(s, "mission_<ID>")
		return hexRE.ReplaceAllString(s, "<SHA>")
	}
	out := opencodetest.Call{Stdin: norm(c.Stdin), Cwd: norm(c.Cwd), ProjectDisabled: c.ProjectDisabled}
	for _, a := range c.Argv {
		out.Argv = append(out.Argv, norm(a))
	}
	if config, ok := normalizeOpenCodeValue(c.Config, norm).(map[string]any); ok {
		out.Config = config
	}
	return out
}

func compareOpenCodeCalls(t *testing.T, goCalls, pyCalls []opencodetest.Call, goDir, pyDir string) {
	t.Helper()
	if len(goCalls) != len(pyCalls) {
		t.Fatalf("opencode runs: go %d, python %d", len(goCalls), len(pyCalls))
	}
	for i := range goCalls {
		g, p := normalizeOpenCodeCall(goCalls[i], goDir), normalizeOpenCodeCall(pyCalls[i], pyDir)
		if !reflect.DeepEqual(g.Argv, p.Argv) {
			t.Errorf("run %d argv differs:\ngo: %q\npy: %q", i, g.Argv, p.Argv)
		}
		if g.Stdin != p.Stdin {
			t.Errorf("run %d prompt differs:\ngo: %s\npy: %s", i, g.Stdin, p.Stdin)
		}
		if g.Cwd != p.Cwd {
			t.Errorf("run %d cwd differs: go %s, python %s", i, g.Cwd, p.Cwd)
		}
		if g.ProjectDisabled != p.ProjectDisabled {
			t.Errorf("run %d OPENCODE_DISABLE_PROJECT_CONFIG differs: go %q, python %q", i, g.ProjectDisabled, p.ProjectDisabled)
		}
		if !reflect.DeepEqual(g.Config, p.Config) {
			t.Errorf("run %d injected config differs:\ngo: %v\npy: %v", i, g.Config, p.Config)
		}
	}
}

func TestE2EOpenCodeEngineMatchesPython(t *testing.T) {
	bin := lhaBinary(t)
	fake, _ := opencodetest.Install(t)
	calls, _ := json.Marshal(engineCalls)
	args := []string{"run-local", "--title", "Greeter", "--item", "write hello.txt",
		"--no-default-checks", "--check", helloCheck, "--workdir", "ws"}
	envFor := func(dir string) []string {
		return processEnv("LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_LEAD_ENGINE=opencode",
			"LHA_OPENCODE_BIN="+fake, "LHA_OPENCODE_MAX_BUDGET_USD=1.5", "LHA_OPENCODE_MODEL=sonnet",
			opencodetest.EnvFake+"=1", "FAKE_OPENCODE_MODE=mcp", "FAKE_OPENCODE_CALLS="+string(calls),
			"FAKE_OPENCODE_LOG="+filepath.Join(dir, "opencode.log"))
	}

	goDir := t.TempDir()
	goRun := runProcess(t, goDir, envFor(goDir), bin, args...)
	if goRun.code != 0 || !strings.Contains(goRun.stdout, ": complete\nitems 1/1  cycles 1  cost $1.4700\n") {
		t.Fatalf("go: %+v", goRun)
	}
	workdir := filepath.Join(goDir, "ws")
	if data, err := os.ReadFile(filepath.Join(workdir, "hello.txt")); err != nil || string(data) != "hello" {
		t.Fatalf("hello.txt: %q %v", data, err)
	}
	goCalls := opencodetest.Calls(t, filepath.Join(goDir, "opencode.log"))
	if len(goCalls) != 1 {
		t.Fatalf("sessions: %d", len(goCalls))
	}
	texts := goCalls[0].Texts
	if len(texts) != 6 || !strings.HasPrefix(texts[0], "recorded decision") || !strings.Contains(texts[3], "--- stdout ---\nhello\n") ||
		!strings.Contains(texts[2], "git push") || !strings.Contains(texts[4], ".lha") ||
		texts[5] != "PASSED: every check and witness is green." {
		t.Fatalf("tool results: %q", texts)
	}
	if model, _ := argValue(goCalls[0].Argv, "--model"); model != "sonnet" {
		t.Fatalf("argv: %q", goCalls[0].Argv)
	}
	approvals := eventsOfKind(t, workdir, "tool_approval")
	if len(approvals) != 1 || approvals[0]["decision"] != "reject" || approvals[0]["tool"] != "run_command" {
		t.Fatalf("tool_approval events: %v", approvals)
	}
	if r := runProcess(t, goDir, processEnv(), bin, "decisions", "--workdir", "ws", "--verify"); r.stdout != "decision chain OK: 1 record(s), 1 chained\n" {
		t.Fatalf("decisions: %+v", r)
	}
	goWS := snapshot(t, workdir)
	goStore := storeDigest(t, bin, filepath.Join(goDir, ".lha-xdg", "lha", "lha.sqlite3"), missionIDRE.FindString(goRun.stdout))
	if !strings.Contains(goStore, "DONE") || !strings.Contains(goStore, " lead ") || !strings.Contains(goStore, "$1.4700") ||
		!strings.Contains(goStore, "cycle_outcome 1") {
		t.Fatalf("store:\n%s", goStore)
	}
	if !pythonAvailable(t) {
		return
	}
	pyDir := t.TempDir()
	pyRun := runPythonLHA(t, pyDir, envFor(pyDir), args...)
	if pyRun.code != goRun.code {
		t.Fatalf("exit codes differ: go %d, python %d\n%s%s", goRun.code, pyRun.code, pyRun.stdout, pyRun.stderr)
	}
	if g, p := report(t, goRun.stdout), report(t, pyRun.stdout); g != p {
		t.Errorf("reports differ:\ngo: %s\npy: %s", g, p)
	}
	compareWorkspaces(t, goWS, snapshot(t, filepath.Join(pyDir, "ws")))
	compareOpenCodeCalls(t, goCalls, opencodetest.Calls(t, filepath.Join(pyDir, "opencode.log")), goDir, pyDir)
	if pyStore := storeDigest(t, bin, filepath.Join(pyDir, "lha.sqlite3"), missionIDRE.FindString(pyRun.stdout)); pyStore != goStore {
		t.Errorf("stores differ:\ngo:\n%s\npy:\n%s", goStore, pyStore)
	}
}

// TestE2EOpenCodeModelBackendMatchesPython: with the built-in loop on the opencode model backend
// each lead turn is one tool-less opencode run call; the argv, the injected config and the
// flattened prompt must match Python's.
func TestE2EOpenCodeModelBackendMatchesPython(t *testing.T) {
	bin := lhaBinary(t)
	fake, _ := opencodetest.Install(t)
	withPython := pythonAvailable(t)
	for _, c := range []struct {
		name, mode, reply string
		code              int
	}{
		{"write-then-done", "text", `{"tool": "write_file", "arguments": {"path": "hello.txt", "content": "hello"}}`, 1},
		{"done", "text", `{"done": true, "summary": "ok"}`, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := []string{"run-local", "--title", "Greeter", "--item", "write hello.txt",
				"--no-default-checks", "--check", helloCheck, "--workdir", "ws"}
			envFor := func(dir string) []string {
				return processEnv("LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_MODEL_BACKEND=opencode",
					"LHA_OPENCODE_BIN="+fake, "LHA_MAX_TURNS_PER_CYCLE=2", "LHA_MAX_REPLANS=0",
					"LHA_BUDGET_USD_CEILING=100", opencodetest.EnvFake+"=1", "FAKE_OPENCODE_MODE="+c.mode,
					"FAKE_OPENCODE_REPLY="+c.reply, "FAKE_OPENCODE_LOG="+filepath.Join(dir, "opencode.log"))
			}
			goDir := t.TempDir()
			goRun := runProcess(t, goDir, envFor(goDir), bin, args...)
			goCalls := opencodetest.Calls(t, filepath.Join(goDir, "opencode.log"))
			if len(goCalls) == 0 {
				t.Fatalf("opencode never ran: %+v", goRun)
			}
			argv := goCalls[0].Argv
			if strings.Join(argv[:5], " ") != "run --format json --auto --agent" {
				t.Fatalf("argv: %q", argv)
			}
			if got, _ := argValue(argv, "--agent"); got != "lha-model" {
				t.Fatalf("agent: %q", got)
			}
			agents, _ := goCalls[0].Config["agents"].(map[string]any)
			if _, ok := agents["lha-model"]; !ok || goCalls[0].ProjectDisabled != "true" {
				t.Fatalf("config: %v", goCalls[0].Config)
			}
			if _, ok := goCalls[0].Config["mcp"]; ok {
				t.Fatalf("a plain text turn must have no tools: %v", goCalls[0].Config)
			}
			if !withPython {
				return
			}
			pyDir := t.TempDir()
			pyRun := runPythonLHA(t, pyDir, envFor(pyDir), args...)
			if pyRun.code != goRun.code {
				t.Fatalf("exit codes differ: go %d, python %d\ngo: %s%s\npy: %s%s", goRun.code, pyRun.code,
					goRun.stdout, goRun.stderr, pyRun.stdout, pyRun.stderr)
			}
			if g, p := report(t, goRun.stdout), report(t, pyRun.stdout); g != p {
				t.Errorf("reports differ:\ngo: %s\npy: %s", g, p)
			}
			compareOpenCodeCalls(t, goCalls, opencodetest.Calls(t, filepath.Join(pyDir, "opencode.log")), goDir, pyDir)
		})
	}
}
