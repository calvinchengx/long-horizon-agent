package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/model/claudecodetest"
)

// End-to-end tests of LHA_LEAD_ENGINE=claude_code and LHA_MODEL_BACKEND=claude_code through the
// built lha binary, the real local sandbox and the real dispatcher (gates, record_decision). The
// fake claude is this test binary (claudecodetest). When uv is on PATH the Python lha runs the
// same mission against the same fake, and the argv/MCP config/prompt each implementation gave
// claude, the tool results the bridge returned, the report and the committed workspace must
// match.

func TestMain(m *testing.M) {
	claudecodetest.Main() // this binary doubles as the fake claude
	// Runs persist to the mission store (and memory): never the developer's per-user one.
	dir, err := os.MkdirTemp("", "lha-cli-store-")
	if err != nil {
		panic(err)
	}
	packageXDG = dir
	os.Setenv("XDG_DATA_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var (
	bearerRE = regexp.MustCompile(`Bearer [A-Za-z0-9_-]+`)
	portRE   = regexp.MustCompile(`127\.0\.0\.1:\d+`)
	hexRE    = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	// Known gap (not the claude_code port's): the Go execution tools' JSON-Schema properties are
	// plain maps, so the prompt lists their keys sorted where Python keeps declaration order.
	toolArgsRE = regexp.MustCompile(`\(args: \{[^\n]*\}\)`)
)

// normalizeCall masks what legitimately differs between two runs: the bridge's token and port,
// commit shas, mission ids and the temp directory.
func normalizeCall(c claudecodetest.Call, dir string) claudecodetest.Call {
	norm := func(s string) string {
		s = strings.ReplaceAll(s, dir, "<DIR>")
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			s = strings.ReplaceAll(s, real, "<DIR>")
		}
		s = bearerRE.ReplaceAllString(s, "Bearer <TOKEN>")
		s = portRE.ReplaceAllString(s, "127.0.0.1:<PORT>")
		s = missionIDRE.ReplaceAllString(s, "mission_<ID>")
		s = toolArgsRE.ReplaceAllString(s, "(args: <SCHEMA>)")
		return hexRE.ReplaceAllString(s, "<SHA>")
	}
	out := claudecodetest.Call{Stdin: norm(c.Stdin), Cwd: norm(c.Cwd)}
	for _, a := range c.Argv {
		out.Argv = append(out.Argv, norm(a))
	}
	for _, x := range c.Texts {
		out.Texts = append(out.Texts, norm(x))
	}
	return out
}

func compareCalls(t *testing.T, goCalls, pyCalls []claudecodetest.Call, goDir, pyDir string) {
	t.Helper()
	if len(goCalls) != len(pyCalls) {
		t.Fatalf("claude runs: go %d, python %d", len(goCalls), len(pyCalls))
	}
	for i := range goCalls {
		g, p := normalizeCall(goCalls[i], goDir), normalizeCall(pyCalls[i], pyDir)
		if !reflect.DeepEqual(g.Argv, p.Argv) {
			for j := 0; j < max(len(g.Argv), len(p.Argv)); j++ {
				var ga, pa string
				if j < len(g.Argv) {
					ga = g.Argv[j]
				}
				if j < len(p.Argv) {
					pa = p.Argv[j]
				}
				if ga != pa {
					t.Errorf("run %d argv[%d] differs:\ngo: %q\npy: %q", i, j, ga, pa)
				}
			}
		}
		if g.Stdin != p.Stdin {
			t.Errorf("run %d prompt differs:\ngo: %s\npy: %s", i, g.Stdin, p.Stdin)
		}
		if g.Cwd != p.Cwd {
			t.Errorf("run %d cwd differs: go %s, python %s", i, g.Cwd, p.Cwd)
		}
		if !reflect.DeepEqual(g.Texts, p.Texts) {
			t.Errorf("run %d tool results differ:\ngo: %q\npy: %q", i, g.Texts, p.Texts)
		}
	}
}

// engineCalls: record a decision, write hello.txt, try an irreversible command (gated: refused
// without a human gate), read the file back with a real command, fail a path rule, then verify.
var engineCalls = []any{
	[]any{"record_decision", map[string]any{"decision": "greet in English", "rationale": "the audience",
		"alternatives_rejected": "French", "affected": []any{"hello.txt"}}},
	[]any{"write_file", map[string]any{"path": "hello.txt", "content": "hello"}},
	[]any{"run_command", map[string]any{"argv": []any{"git", "push", "origin", "main"}}},
	[]any{"run_command", map[string]any{"argv": []any{"cat", "hello.txt"}}},
	[]any{"write_file", map[string]any{"path": ".lha/checklist.json", "content": "{}"}},
	[]any{"verify", map[string]any{}},
}

func TestE2EClaudeCodeEngineMatchesPython(t *testing.T) {
	bin := lhaBinary(t)
	fake, _ := claudecodetest.Install(t)
	calls, _ := json.Marshal(engineCalls)
	args := []string{"run-local", "--title", "Greeter", "--item", "write hello.txt",
		"--no-default-checks", "--check", helloCheck, "--workdir", "ws"}
	envFor := func(dir string) []string {
		return processEnv("LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_LEAD_ENGINE=claude_code",
			"LHA_CLAUDE_CODE_BIN="+fake, "LHA_CLAUDE_CODE_MAX_BUDGET_USD=1.5", "LHA_MODEL_NAME=sonnet",
			claudecodetest.EnvFake+"=1", "FAKE_CLAUDE_MODE=mcp", "FAKE_CLAUDE_CALLS="+string(calls),
			"FAKE_CLAUDE_LOG="+filepath.Join(dir, "claude.log"))
	}

	goDir := t.TempDir()
	goRun := runProcess(t, goDir, envFor(goDir), bin, args...)
	if goRun.code != 0 || !strings.Contains(goRun.stdout, ": complete\nitems 1/1  cycles 1  cost $0.4200\n") {
		t.Fatalf("go: %+v", goRun)
	}
	workdir := filepath.Join(goDir, "ws")
	if data, err := os.ReadFile(filepath.Join(workdir, "hello.txt")); err != nil || string(data) != "hello" {
		t.Fatalf("hello.txt: %q %v", data, err)
	}
	goCalls := claudecodetest.Calls(t, filepath.Join(goDir, "claude.log"))
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
	// The session's reported cost reached the persistent ledger (the meter's hook), and the cycle
	// reached memory.
	goStore := storeDigest(t, bin, filepath.Join(goDir, ".lha-xdg", "lha", "lha.sqlite3"), missionIDRE.FindString(goRun.stdout))
	if !strings.Contains(goStore, "DONE") || !strings.Contains(goStore, " lead ") || !strings.Contains(goStore, "$0.4200") ||
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
	compareCalls(t, goCalls, claudecodetest.Calls(t, filepath.Join(pyDir, "claude.log")), goDir, pyDir)
	if pyStore := storeDigest(t, bin, filepath.Join(pyDir, "lha.sqlite3"), missionIDRE.FindString(pyRun.stdout)); pyStore != goStore {
		t.Errorf("stores differ:\ngo:\n%s\npy:\n%s", goStore, pyStore)
	}
}

// TestE2EClaudeCodeNativeEngineMatchesPython: LHA_CLAUDE_CODE_TOOLS=native keeps Claude Code's own
// tools (with the deny list) and bridges only record_decision and verify. The session does
// nothing useful here, so the item blocks after three failed attempts in both implementations.
func TestE2EClaudeCodeNativeEngineMatchesPython(t *testing.T) {
	bin := lhaBinary(t)
	fake, _ := claudecodetest.Install(t)
	calls, _ := json.Marshal([]any{engineCalls[0], engineCalls[1], engineCalls[5]})
	args := []string{"run-local", "--title", "Greeter", "--item", "write hello.txt",
		"--no-default-checks", "--check", helloCheck, "--workdir", "ws"}
	envFor := func(dir string) []string {
		return processEnv("LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true",
			"LHA_LEAD_ENGINE=claude_code", "LHA_CLAUDE_CODE_TOOLS=native", "LHA_MAX_REPLANS=0",
			"LHA_CLAUDE_CODE_BIN="+fake, claudecodetest.EnvFake+"=1", "FAKE_CLAUDE_MODE=mcp",
			"FAKE_CLAUDE_CALLS="+string(calls), "FAKE_CLAUDE_LOG="+filepath.Join(dir, "claude.log"))
	}
	goDir := t.TempDir()
	goRun := runProcess(t, goDir, envFor(goDir), bin, args...)
	if goRun.code != 1 || !strings.Contains(goRun.stdout, ": deadlocked: blocked: 01\nitems 0/1  cycles 3  cost $1.2600\n") {
		t.Fatalf("go: %+v", goRun)
	}
	goCalls := claudecodetest.Calls(t, filepath.Join(goDir, "claude.log"))
	if len(goCalls) != 3 || !strings.HasPrefix(goCalls[0].Texts[1], "unknown tool: 'write_file'") {
		t.Fatalf("calls: %+v", goCalls)
	}
	if _, ok := argValue(goCalls[0].Argv, "--tools"); ok {
		t.Fatalf("argv: %q", goCalls[0].Argv)
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
	compareWorkspaces(t, snapshot(t, filepath.Join(goDir, "ws")), snapshot(t, filepath.Join(pyDir, "ws")))
	compareCalls(t, goCalls, claudecodetest.Calls(t, filepath.Join(pyDir, "claude.log")), goDir, pyDir)
}

// TestE2EClaudeCodeModelBackendMatchesPython: with the built-in loop on the claude_code model
// backend each lead turn is one tool-less claude -p call; the argv and the flattened prompt must
// match Python's, and so must the capped (error_max_budget_usd) and failing runs.
func TestE2EClaudeCodeModelBackendMatchesPython(t *testing.T) {
	bin := lhaBinary(t)
	fake, _ := claudecodetest.Install(t)
	withPython := pythonAvailable(t)
	for _, c := range []struct {
		name, mode, reply string
		code              int
	}{
		{"write-then-done", "text", `{"tool": "write_file", "arguments": {"path": "hello.txt", "content": "hello"}}`, 1},
		{"done", "text", `{"done": true, "summary": "ok"}`, 1},
		{"expired-login", "auth", "", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := []string{"run-local", "--title", "Greeter", "--item", "write hello.txt",
				"--no-default-checks", "--check", helloCheck, "--workdir", "ws"}
			envFor := func(dir string) []string {
				return processEnv("LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_MODEL_BACKEND=claude_code",
					"LHA_CLAUDE_CODE_BIN="+fake, "LHA_MAX_TURNS_PER_CYCLE=2", "LHA_MAX_REPLANS=0",
					"LHA_BUDGET_USD_CEILING=100", claudecodetest.EnvFake+"=1", "FAKE_CLAUDE_MODE="+c.mode,
					"FAKE_CLAUDE_REPLY="+c.reply, "FAKE_CLAUDE_LOG="+filepath.Join(dir, "claude.log"))
			}
			goDir := t.TempDir()
			goRun := runProcess(t, goDir, envFor(goDir), bin, args...)
			goCalls := claudecodetest.Calls(t, filepath.Join(goDir, "claude.log"))
			if len(goCalls) == 0 {
				t.Fatalf("claude never ran: %+v", goRun)
			}
			if tools, _ := argValue(goCalls[0].Argv, "--tools"); tools != "" || !strings.Contains(goCalls[0].Argv[len(goCalls[0].Argv)-1], "Available tools:") {
				t.Fatalf("argv: %q", goCalls[0].Argv)
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
			if goRun.code == 0 || strings.Contains(goRun.stdout, "\nitems ") {
				if g, p := report(t, goRun.stdout), report(t, pyRun.stdout); g != p {
					t.Errorf("reports differ:\ngo: %s\npy: %s", g, p)
				}
				compareWorkspaces(t, snapshot(t, filepath.Join(goDir, "ws")), snapshot(t, filepath.Join(pyDir, "ws")))
			} else if want := "claude -p failed: Failed to authenticate: OAuth session expired\n"; !strings.HasSuffix(goRun.stderr, "error: "+want) ||
				!strings.HasSuffix(pyRun.stderr, "ClaudeCodeError: "+want) { // Python's CLI prints a traceback
				t.Errorf("errors differ:\ngo: %q\npy: %q", goRun.stderr, pyRun.stderr)
			}
			compareCalls(t, goCalls, claudecodetest.Calls(t, filepath.Join(pyDir, "claude.log")), goDir, pyDir)
		})
	}
}

func argValue(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}
