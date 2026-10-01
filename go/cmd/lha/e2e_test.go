package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/hitl"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// End-to-end tests of the linked execution layer: the real local sandbox and the real tools,
// through the real openToolbox. When `uv` is on PATH the same inputs are run through the Python
// implementation side by side and the observable results (exit code, report, checkpoint commits,
// committed anchor files) must match. Docker runs are gated on LHA_IT_DOCKER=1.

// repoRoot is the repository root (this file is go/cmd/lha/e2e_test.go).
var repoRoot = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}()

var (
	buildOnce sync.Once
	builtLHA  string
	buildErr  error
)

// lhaBinary builds ./cmd/lha once per test run.
func lhaBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lha-e2e-bin-")
		if err != nil {
			buildErr = err
			return
		}
		builtLHA = filepath.Join(dir, "lha")
		cmd := exec.Command("go", "build", "-o", builtLHA, ".")
		cmd.Dir = filepath.Join(repoRoot, "go", "cmd", "lha")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = &buildFailure{string(out)}
		}
	})
	if buildErr != nil {
		t.Fatalf("go build ./cmd/lha: %v", buildErr)
	}
	return builtLHA
}

type buildFailure struct{ out string }

func (b *buildFailure) Error() string { return b.out }

// processEnv is the ambient environment without LHA_* variables, plus env.
func processEnv(env ...string) []string {
	out := []string{}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.HasPrefix(strings.ToUpper(k), "LHA_") {
			out = append(out, kv)
		}
	}
	return append(out, env...)
}

// packageXDG is the package's temporary XDG_DATA_HOME (TestMain).
var packageXDG string

// isolateStore gives a process run in dir its own mission store and memory (under dir, outside
// the workspace) unless the test chose one: the Go and Python runs a parity test compares must
// not recall each other's memory.
func isolateStore(dir string, env []string) []string {
	xdg := ""
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == "XDG_DATA_HOME" {
			xdg = v
		}
	}
	if env == nil || (xdg != "" && xdg != packageXDG) {
		return env
	}
	return append(append([]string{}, env...), "XDG_DATA_HOME="+filepath.Join(dir, ".lha-xdg"))
}

func runProcess(t *testing.T, dir string, env []string, name string, args ...string) result {
	t.Helper()
	env = isolateStore(dir, env)
	var out, errOut bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, &out, &errOut
	code := 0
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s: %v", name, err)
		}
		code = exit.ExitCode()
	}
	return result{out.String(), errOut.String(), code}
}

// pythonAvailable reports whether the Python implementation can run side by side.
func pythonAvailable(t *testing.T) bool {
	t.Helper()
	if _, err := exec.LookPath("uv"); err != nil {
		t.Log("uv not on PATH: skipping the Python side-by-side comparison")
		return false
	}
	return true
}

// pythonEnv is env with the Python mission store in dir (never the user's store).
func pythonEnv(dir string, env []string) []string {
	return append(append([]string{}, env...), "LHA_SQLITE_PATH="+filepath.Join(dir, "lha.sqlite3"))
}

func runPythonLHA(t *testing.T, dir string, env []string, args ...string) result {
	t.Helper()
	return runProcess(t, dir, pythonEnv(dir, env), "uv", append([]string{"run", "--quiet", "--project",
		filepath.Join(repoRoot, "python"), "lha"}, args...)...)
}

var reportLinesRE = regexp.MustCompile(`(?m)^mission mission_[0-9a-f]{12}: .+\nitems \d+/\d+  cycles \d+  cost \$\d+\.\d{4}\nhead (?:[0-9a-f]{40}|\(none\))$`)

var (
	missionIDRE = regexp.MustCompile(`mission_[0-9a-f]{12}`)
	shaRE       = regexp.MustCompile(`[0-9a-f]{40}`)
)

// report is the CLI's three-line report with the mission id and head sha masked (the Python CLI
// also logs to stdout, so the report is located rather than compared whole).
func report(t *testing.T, stdout string) string {
	t.Helper()
	m := reportLinesRE.FindString(stdout)
	if m == "" {
		t.Fatalf("no report in stdout:\n%s", stdout)
	}
	return shaRE.ReplaceAllString(missionIDRE.ReplaceAllString(m, "mission_X"), "SHA")
}

// workspace is what a run leaves behind: checkpoint commits, tracked files and the committed
// anchor files, compared as raw bytes (event durations masked).
type workspace struct {
	Commits []string
	Files   []string
	Anchor  map[string]string
}

var anchorFiles = []string{"checklist.json", "decisions.ndjson", "events.ndjson", "mission.json", "progress.md"}

func snapshot(t *testing.T, workdir string) workspace {
	t.Helper()
	w := workspace{
		Commits: strings.Split(git(t, workdir, "log", "--format=%s%n%b"), "\n"),
		Files:   strings.Split(git(t, workdir, "ls-files"), "\n"),
		Anchor:  map[string]string{},
	}
	for _, f := range anchorFiles {
		body := git(t, workdir, "show", "HEAD:.lha/"+f)
		if strings.HasSuffix(f, ".ndjson") {
			body = normalizeNDJSON(t, body)
		}
		w.Anchor[f] = body
	}
	return w
}

// durationRE matches a check's measured duration in an event line; with the commit shas a review
// event names (its base..head range), the only run-to-run noise.
var durationRE = regexp.MustCompile(`"duration_s":-?[0-9][0-9.e+-]*`)

// normalizeNDJSON is the log's raw bytes (not parsed: key order, float formatting and escaping
// are compared) with blank lines dropped and measured durations and commit shas masked.
func normalizeNDJSON(t *testing.T, body string) string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			t.Fatalf("bad ndjson line %q", line)
		}
		line = durationRE.ReplaceAllString(line, `"duration_s":0.0`)
		lines = append(lines, shaRE.ReplaceAllString(line, "SHA"))
	}
	return strings.Join(lines, "\n")
}

func compareWorkspaces(t *testing.T, goW, pyW workspace) {
	t.Helper()
	if strings.Join(goW.Commits, "\n") != strings.Join(pyW.Commits, "\n") {
		t.Errorf("commits differ:\ngo: %q\npy: %q", goW.Commits, pyW.Commits)
	}
	if strings.Join(goW.Files, "\n") != strings.Join(pyW.Files, "\n") {
		t.Errorf("tracked files differ:\ngo: %q\npy: %q", goW.Files, pyW.Files)
	}
	for _, f := range anchorFiles {
		if goW.Anchor[f] != pyW.Anchor[f] {
			t.Errorf(".lha/%s differs:\ngo: %s\npy: %s", f, goW.Anchor[f], pyW.Anchor[f])
		}
	}
}

// TestE2ERunLocalBinaryMatchesPython builds lha and runs `lha run-local` with the stub model on
// the real local sandbox (LHA_SANDBOX=local, LHA_ALLOW_UNSAFE_LOCAL=true) in a temp workdir.
func TestE2ERunLocalBinaryMatchesPython(t *testing.T) {
	bin := lhaBinary(t)
	withPython := pythonAvailable(t)
	env := processEnv("LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_MODEL_BACKEND=stub",
		"LHA_MAX_TURNS_PER_CYCLE=2")
	for _, c := range []struct {
		name, check, want string
		code              int
	}{
		{"complete", "true", "complete", 0},
		{"deadlock", "false", "deadlocked: blocked: 01", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := []string{"run-local", "--title", "Greeter", "--item", "say hello",
				"--no-default-checks", "--check", c.check, "--workdir", "ws"}
			goDir := t.TempDir()
			goRun := runProcess(t, goDir, env, bin, args...)
			if goRun.code != c.code || !strings.Contains(goRun.stdout, ": "+c.want+"\n") {
				t.Fatalf("go: %+v", goRun)
			}
			goWS := snapshot(t, filepath.Join(goDir, "ws"))
			if head := git(t, filepath.Join(goDir, "ws"), "rev-parse", "HEAD"); !strings.Contains(goRun.stdout, "head "+head+"\n") && c.code == 0 {
				t.Fatalf("reported head is not HEAD %s: %s", head, goRun.stdout)
			}
			if !withPython {
				return
			}
			pyDir := t.TempDir()
			pyRun := runPythonLHA(t, pyDir, env, args...)
			if pyRun.code != goRun.code {
				t.Fatalf("exit codes differ: go %d, python %d\n%s%s", goRun.code, pyRun.code, pyRun.stdout, pyRun.stderr)
			}
			if g, p := report(t, goRun.stdout), report(t, pyRun.stdout); g != p {
				t.Errorf("reports differ:\ngo: %s\npy: %s", g, p)
			}
			compareWorkspaces(t, goWS, snapshot(t, filepath.Join(pyDir, "ws")))
		})
	}
}

// TestE2ERefusalsMatchPython: an unsafe local sandbox and a Rule-of-Two run are refused before
// the workspace is touched, with Python's message and exit code.
func TestE2ERefusalsMatchPython(t *testing.T) {
	bin := lhaBinary(t)
	withPython := pythonAvailable(t)
	for _, c := range []struct {
		name string
		env  []string
		args []string
	}{
		{"unsafe-local", []string{"LHA_SANDBOX=local"}, nil},
		{"rule-of-two", []string{"LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_WEB_ALLOW_HOSTS=docs.example.com"}, nil},
		{"bad-web-settings", []string{"LHA_SANDBOX=docker", "LHA_WEB_ALLOW_HOSTS=docs.example.com",
			`LHA_WEB_CREDENTIALS={"tok": {"value": "s3cret", "hosts": ["evil.example.org"]}}`}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := processEnv(append([]string{"LHA_MODEL_BACKEND=stub"}, c.env...)...)
			args := []string{"run-local", "--item", "a", "--no-default-checks", "--check", "true", "--workdir", "ws"}
			dir := t.TempDir()
			goRun := runProcess(t, dir, env, bin, args...)
			if goRun.code != 2 || !strings.HasPrefix(goRun.stderr, "error: ") {
				t.Fatalf("go: %+v", goRun)
			}
			if _, err := os.Stat(filepath.Join(dir, "ws")); !os.IsNotExist(err) {
				t.Fatal("the workspace was touched before the refusal")
			}
			if strings.Contains(goRun.stderr, "s3cret") {
				t.Fatal("a credential leaked into the error")
			}
			if !withPython {
				return
			}
			pyRun := runPythonLHA(t, t.TempDir(), env, args...)
			if pyRun.code != goRun.code || pyRun.stderr != goRun.stderr {
				t.Errorf("go %d %q\npy %d %q", goRun.code, goRun.stderr, pyRun.code, pyRun.stderr)
			}
		})
	}
}

// scriptedTurns is the lead's script for the scripted runs: record a decision, write hello.txt,
// try an irreversible command (gated), read the file back with a real command, then finish.
const scriptedTurns = `[
 {"tool_calls": [{"id": "d1", "name": "record_decision", "arguments": {"decision": "greet in English",
   "rationale": "the audience", "alternatives_rejected": "French", "affected": ["hello.txt"]}}],
  "stop_reason": "tool_use"},
 {"text": "{\"tool\": \"write_file\", \"arguments\": {\"path\": \"hello.txt\", \"content\": \"hello\"}}"},
 {"tool_calls": [{"id": "p1", "name": "run_command", "arguments": {"argv": ["git", "push", "origin", "main"]}}],
  "stop_reason": "tool_use"},
 {"tool_calls": [{"id": "c1", "name": "run_command", "arguments": {"argv": ["cat", "hello.txt"]}}],
  "stop_reason": "tool_use"},
 {"text": "{\"done\": true, \"summary\": \"wrote hello.txt\"}"}
]`

const helloCheck = `sh -c 'test "$(cat hello.txt)" = hello'`

func scriptedModel(t *testing.T) *model.StubModel {
	t.Helper()
	var turns []contracts.TurnResult
	if err := json.Unmarshal([]byte(scriptedTurns), &turns); err != nil {
		t.Fatal(err)
	}
	return model.NewStub(turns)
}

func eventsOfKind(t *testing.T, workdir, kind string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(git(t, workdir, "show", "HEAD:.lha/events.ndjson"), "\n") {
		var ev struct {
			Kind    string         `json:"kind"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Kind == kind {
			out = append(out, ev.Payload)
		}
	}
	return out
}

// TestE2EScriptedRunMatchesPython: the model writes a file with the real tools in the real local
// sandbox, an irreversible command is refused (no gate) and audited, and the check passes — the
// mission completes in both implementations with the same commits and anchor.
func TestE2EScriptedRunMatchesPython(t *testing.T) {
	env := []string{"LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_MODEL_BACKEND=stub"}
	dir := cleanEnv(t, env...)
	workdir := filepath.Join(dir, "ws")
	goRun := runCLI(t, scriptedModel(t), "run-local", "--title", "Greeter", "--item", "write hello.txt",
		"--no-default-checks", "--check", helloCheck, "--workdir", workdir)
	if goRun.code != 0 || !strings.Contains(goRun.stdout, ": complete\nitems 1/1  cycles 1") {
		t.Fatalf("go: %+v", goRun)
	}
	if data, err := os.ReadFile(filepath.Join(workdir, "hello.txt")); err != nil || string(data) != "hello" {
		t.Fatalf("hello.txt: %q %v", data, err)
	}
	approvals := eventsOfKind(t, workdir, "tool_approval")
	if len(approvals) != 1 || approvals[0]["decision"] != "reject" ||
		approvals[0]["resolved_by"] != "no human gate configured" || approvals[0]["tool"] != "run_command" {
		t.Fatalf("tool_approval events: %v", approvals)
	}
	if r := runCLI(t, nil, "decisions", "--workdir", workdir, "--verify"); r.stdout != "decision chain OK: 1 record(s), 1 chained\n" {
		t.Fatalf("decisions: %+v", r)
	}
	goWS := snapshot(t, workdir)
	if !pythonAvailable(t) {
		return
	}
	pyDir := t.TempDir()
	spec, _ := json.Marshal(map[string]any{
		"workdir": filepath.Join(pyDir, "ws"), "title": "Greeter", "items": []string{"write hello.txt"},
		"checks": [][]string{{"sh", "-c", `test "$(cat hello.txt)" = hello`}}, "script": json.RawMessage(scriptedTurns),
	})
	specPath := filepath.Join(pyDir, "spec.json")
	if err := os.WriteFile(specPath, spec, 0o644); err != nil {
		t.Fatal(err)
	}
	pyRun := runProcess(t, pyDir, pythonEnv(pyDir, processEnv(env...)), "uv", "run", "--quiet", "--project",
		filepath.Join(repoRoot, "python"), "python", filepath.Join(repoRoot, "go", "cmd", "lha", "testdata", "scripted_run.py"), specPath)
	if pyRun.code != goRun.code {
		t.Fatalf("exit codes differ: go %d, python %d\n%s%s", goRun.code, pyRun.code, pyRun.stdout, pyRun.stderr)
	}
	if g, p := report(t, goRun.stdout), report(t, pyRun.stdout); g != p {
		t.Errorf("reports differ:\ngo: %s\npy: %s", g, p)
	}
	compareWorkspaces(t, goWS, snapshot(t, filepath.Join(pyDir, "ws")))
}

// TestE2EConsoleGateEventsReachTheCheckpoint: with --approve-interactive the gated command goes
// to the console gate; its reminder and the human's answer are committed with the checkpoint
// (the gate's events flow through the record_decision wrapper's DrainEvents).
func TestE2EConsoleGateEventsReachTheCheckpoint(t *testing.T) {
	dir := cleanEnv(t, "LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_MODEL_BACKEND=stub")
	var prompts bytes.Buffer
	answers := []struct {
		line string
		ok   bool
	}{{"", false}, {"no\n", true}}
	saved := consoleGate
	t.Cleanup(func() { consoleGate = saved })
	consoleGate = func(settings *config.Settings) contracts.HITLGate {
		return hitl.NewTerminalApprover(hitl.TerminalOptions{
			TimeoutSeconds: float64(settings.ConsoleApprovalTimeoutS), EscalationSeconds: []float64{900},
			Out: &prompts, IsTTY: func() bool { return true },
			ReadLine: func(context.Context, float64) (string, bool) {
				a := answers[0]
				answers = answers[1:]
				return a.line, a.ok
			},
		})
	}
	workdir := filepath.Join(dir, "ws")
	r := runCLI(t, scriptedModel(t), "run-local", "--item", "write hello.txt", "--approve-interactive",
		"--no-default-checks", "--check", helloCheck, "--workdir", workdir)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(prompts.String(), "  argv:    git push origin main\n") ||
		!strings.Contains(prompts.String(), "[lha] reminder 1: still waiting") {
		t.Fatalf("prompts: %s", prompts.String())
	}
	reminders := eventsOfKind(t, workdir, "gate_reminder")
	approvals := eventsOfKind(t, workdir, "tool_approval")
	if len(reminders) != 1 || reminders[0]["step"] != float64(1) || reminders[0]["gate"] != "tool_call" {
		t.Fatalf("gate_reminder events: %v", reminders)
	}
	if len(approvals) != 1 || approvals[0]["resolved_by"] != "human" || approvals[0]["defaulted"] != false {
		t.Fatalf("tool_approval events: %v", approvals)
	}
}

// TestE2EDockerScriptedRun runs the scripted mission in the Docker sandbox (LHA_IT_DOCKER=1).
func TestE2EDockerScriptedRun(t *testing.T) {
	if os.Getenv("LHA_IT_DOCKER") != "1" {
		t.Skip("set LHA_IT_DOCKER=1 to run the Docker end-to-end test")
	}
	dir := cleanEnv(t, "LHA_SANDBOX=docker", "LHA_MODEL_BACKEND=stub")
	workdir := filepath.Join(dir, "ws")
	r := runCLI(t, scriptedModel(t), "run-local", "--item", "write hello.txt",
		"--no-default-checks", "--check", helloCheck, "--workdir", workdir)
	if r.code != 0 || !strings.Contains(r.stdout, ": complete\n") {
		t.Fatalf("%+v", r)
	}
	if data, err := os.ReadFile(filepath.Join(workdir, "hello.txt")); err != nil || string(data) != "hello" {
		t.Fatalf("hello.txt: %q %v", data, err)
	}
}
