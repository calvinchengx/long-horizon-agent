package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agent/agenttest"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// cleanEnv removes every ambient LHA_* variable (restored after the test), sets env, and runs
// the test in an empty directory (no .env).
func cleanEnv(t *testing.T, env ...string) string {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(strings.ToUpper(k), "LHA_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

type result struct {
	stdout, stderr string
	code           int
}

func runCLI(t *testing.T, lead contracts.ModelProvider, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	c := &cli{stdout: &out, stderr: &errOut, ctx: context.Background(), leadModel: lead}
	code := c.run(args)
	return result{out.String(), errOut.String(), code}
}

// linkFakeToolbox replaces the execution layer with the agenttest fake for one test.
func linkFakeToolbox(t *testing.T) *[]*agenttest.Toolbox {
	t.Helper()
	boxes := &[]*agenttest.Toolbox{}
	saved := openToolbox
	t.Cleanup(func() { openToolbox = saved })
	openToolbox = func(_ context.Context, req agent.ToolboxRequest) (agent.Toolbox, error) {
		if req.Settings.Sandbox == "local" && !req.Settings.AllowUnsafeLocal {
			t.Fatal("the CLI must refuse an unsafe local sandbox before opening the toolbox")
		}
		box := agenttest.NewToolbox(req.Workdir)
		box.Disp.RecordDecision = req.Anchor.RecordDecision
		*boxes = append(*boxes, box)
		return box, nil
	}
	return boxes
}

func TestVersionHelpAndUnknownCommands(t *testing.T) {
	cleanEnv(t)
	if r := runCLI(t, nil, "version"); r.code != 0 || r.stdout != "lha 0.1.0\n" {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil); r.code != 2 || !strings.Contains(r.stdout, "Usage: lha") {
		t.Fatalf("no args: %+v", r)
	}
	if r := runCLI(t, nil, "--help"); r.code != 0 {
		t.Fatalf("--help: %+v", r)
	}
	if r := runCLI(t, nil, "bogus"); r.code != 2 || !strings.Contains(r.stderr, "No such command 'bogus'") {
		t.Fatalf("bogus: %+v", r)
	}
	if r := runCLI(t, nil, "run-local", "--max"); r.code != 2 {
		t.Fatalf("unknown option: %+v", r)
	}
	for _, name := range notPorted {
		r := runCLI(t, nil, name, "--anything")
		if r.code != 2 || !strings.Contains(r.stderr, "not yet available in the Go implementation; use the Python lha") {
			t.Fatalf("%s: %+v", name, r)
		}
	}
}

func TestConfigPrintsEverySettingRedacted(t *testing.T) {
	cleanEnv(t, "LHA_ANTHROPIC_API_KEY=sk-ant-secret", "XDG_DATA_HOME=/data")
	r := runCLI(t, nil, "config")
	if r.code != 0 || strings.Contains(r.stdout, "sk-ant-secret") || !strings.Contains(r.stdout, "anthropic_api_key = ***\n") ||
		!strings.Contains(r.stdout, "openai_api_key = None\n") || !strings.Contains(r.stdout, "budget_usd_ceiling = 10.0\n") ||
		!strings.HasSuffix(r.stdout, "mission store = sqlite /data/lha/lha.sqlite3\n") {
		t.Fatalf("%+v", r)
	}
}

func TestRunLocalUsageErrors(t *testing.T) {
	cleanEnv(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"run-local"}, "error: give either --item (repeatable) or --checklist FILE\n"},
		{[]string{"run-local", "--item", "a", "--checklist", "x.md"}, "error: give either --item (repeatable) or --checklist FILE\n"},
		{[]string{"run-local", "--item", "a", "--no-default-checks"}, "error: --no-default-checks requires at least one non-empty --check\n"},
		{[]string{"run-local", "--item", "a", "--no-default-checks", "--check", "  "}, "error: --no-default-checks requires at least one non-empty --check\n"},
		{[]string{"run-local", "--item", "a", "--check", `echo "open`}, "error: invalid --check value: No closing quotation\n"},
		{[]string{"run-local", "--item", "a", "--sandbox", "chroot"}, "error: unknown --sandbox 'chroot'; expected one of local, docker, e2b\n"},
		{[]string{"run-local", "--item", "a", "--sandbox", "local"}, "error: the 'local' sandbox runs agent commands directly on the host with no isolation or network restriction; use sandbox='docker' (or 'e2b'), or opt in explicitly with allow_unsafe_local=True (LHA_ALLOW_UNSAFE_LOCAL=true) (or pass --unsafe-local)\n"},
		{[]string{"mission"}, "error: give --task (to plan) or --checklist FILE (to import a checklist)\n"},
		{[]string{"run-local", "--checklist", "missing.md"}, "error: cannot import checklist 'missing.md': "},
		{[]string{"decisions", "--workdir", "nowhere"}, "error: no mission anchor at 'nowhere' (expected a .lha/ directory)\n"},
	} {
		r := runCLI(t, nil, c.args...)
		if r.code != 2 || !strings.HasPrefix(r.stderr, c.want) {
			t.Errorf("%v: code %d stderr %q, want %q", c.args, r.code, r.stderr, c.want)
		}
	}
}

func TestRuleOfTwoIsRefusedUpFront(t *testing.T) {
	cleanEnv(t)
	r := runCLI(t, nil, "run-local", "--item", "a", "--sandbox", "local", "--unsafe-local", "--allow-host", "Docs.Example.com")
	if r.code != 2 || !strings.HasPrefix(r.stderr, "error: refusing to start: web tools are enabled (egress allow-list: docs.example.com)") {
		t.Fatalf("%+v", r)
	}
}

func TestSandboxEgressIsCheckedUpFront(t *testing.T) {
	cleanEnv(t)
	t.Setenv("LHA_SANDBOX_EGRESS", "github.com")
	r := runCLI(t, nil, "run-local", "--item", "a", "--sandbox", "docker")
	if r.code != 2 || !strings.HasPrefix(r.stderr, "error: invalid sandbox egress settings: LHA_SANDBOX_EGRESS entry github.com is not a package-fetch host") {
		t.Fatalf("%+v", r)
	}
	t.Setenv("LHA_SANDBOX_EGRESS", "pypi.org")
	t.Setenv("LHA_PRIVATE_DATA", "true")
	r = runCLI(t, nil, "run-local", "--item", "a", "--sandbox", "docker")
	if r.code != 2 || !strings.HasPrefix(r.stderr, "error: refusing to start: the sandbox can reach the network (sandbox egress: pypi.org)") {
		t.Fatalf("%+v", r)
	}
}

var reportRE = regexp.MustCompile(`^mission (mission_[0-9a-f]{12}): (.+)\nitems (\d+)/(\d+)  cycles (\d+)  cost \$(\d+\.\d{4})\nhead ([0-9a-f]{40}|\(none\))\n$`)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestRunLocalEndToEnd runs `lha run-local` with the stub model (echo mode: every reply is an
// invalid action, so the cycle ends after max_turns and the checks decide) against a temp git
// workdir through a fake toolbox.
func TestRunLocalEndToEndWithTheStubModel(t *testing.T) {
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub", "LHA_MAX_TURNS_PER_CYCLE=2")
	boxes := linkFakeToolbox(t)
	workdir := filepath.Join(dir, "ws")
	r := runCLI(t, nil, "run-local", "--item", "say hello", "--title", "Greeter", "--workdir", workdir,
		"--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", "true")
	m := reportRE.FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil || m[2] != "complete" || m[3] != "1" || m[4] != "1" || m[5] != "1" || m[6] != "0.0000" {
		t.Fatalf("%+v", r)
	}
	if len(*boxes) != 1 || !(*boxes)[0].Closed {
		t.Fatal("toolbox not opened once and closed")
	}
	if head := git(t, workdir, "rev-parse", "HEAD"); head != m[7] {
		t.Fatalf("head %s != reported %s", head, m[7])
	}
	if log := git(t, workdir, "log", "--format=%s"); !strings.HasPrefix(log, "lha: complete 01 (say hello)\n") {
		t.Fatalf("log: %s", log)
	}
}

func scriptedLead() *model.StubModel {
	return model.NewStub([]contracts.TurnResult{
		{ToolCalls: []contracts.ToolCall{{ID: "d1", Name: "record_decision", Arguments: map[string]any{
			"decision": "greet in English", "rationale": "the audience", "alternatives_rejected": "French",
			"affected": []any{"hello.txt"},
		}}}, StopReason: contracts.Str("tool_use")},
		{Text: `{"tool": "write_file", "arguments": {"path": "hello.txt", "content": "hello"}}`},
		{Text: `{"done": true, "summary": "wrote hello.txt"}`},
	})
}

func TestRunLocalEndToEndToolsDecisionsAndDecisionsCommand(t *testing.T) {
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub")
	linkFakeToolbox(t)
	workdir := filepath.Join(dir, "ws")
	r := runCLI(t, scriptedLead(), "run-local", "--item", "write hello.txt", "--workdir", workdir,
		"--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", `sh -c 'test "$(cat hello.txt)" = hello'`)
	if r.code != 0 || !strings.Contains(r.stdout, ": complete\n") {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "decisions", "--workdir", workdir); r.code != 0 ||
		r.stdout != "1. [c1] greet in English\n   why: the audience\n   rejected: French\n   affects: hello.txt\n" {
		t.Fatalf("decisions: %+v", r)
	}
	if r := runCLI(t, nil, "decisions", "--workdir", workdir, "--verify"); r.code != 0 ||
		r.stdout != "decision chain OK: 1 record(s), 1 chained\n" {
		t.Fatalf("verify: %+v", r)
	}
	// Tamper with the committed log: --verify exits 1, the listing refuses (exit 1).
	path := filepath.Join(workdir, ".lha", "decisions.ndjson")
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(data, []byte("the audience"), []byte("a whim"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, workdir, "commit", "-qam", "tamper")
	if r := runCLI(t, nil, "decisions", "--workdir", workdir, "--verify"); r.code != 1 || !strings.HasPrefix(r.stdout, "decision chain BROKEN: ") {
		t.Fatalf("tampered verify: %+v", r)
	}
	if r := runCLI(t, nil, "decisions", "--workdir", workdir); r.code != 1 ||
		!strings.HasPrefix(r.stderr, "error: .lha/decisions.ndjson failed verification (") {
		t.Fatalf("tampered listing: %+v", r)
	}
}

func TestRunLocalDeadlockExitsOne(t *testing.T) {
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub", "LHA_MAX_TURNS_PER_CYCLE=1")
	linkFakeToolbox(t)
	r := runCLI(t, nil, "run-local", "--item", "impossible", "--workdir", filepath.Join(dir, "ws"),
		"--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", "false")
	m := reportRE.FindStringSubmatch(r.stdout)
	if r.code != 1 || m == nil || m[2] != "deadlocked: blocked: 01" || m[3] != "0" || m[5] != "3" {
		t.Fatalf("%+v", r)
	}
}

func TestMissionPlansThenRuns(t *testing.T) {
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub", "LHA_MAX_TURNS_PER_CYCLE=1")
	linkFakeToolbox(t)
	workdir := filepath.Join(dir, "ws")
	// The echo stub cannot plan, so the Planner falls back to one item: the task itself.
	r := runCLI(t, nil, "mission", "--task", "build the thing", "--workdir", workdir,
		"--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", "true")
	if r.code != 0 || !strings.Contains(r.stdout, ": complete\nitems 1/1") {
		t.Fatalf("%+v", r)
	}
	var cl contracts.Checklist
	if err := json.Unmarshal([]byte(git(t, workdir, "show", "HEAD:.lha/checklist.json")), &cl); err != nil ||
		cl.Items[0].Description != "build the thing" {
		t.Fatalf("%+v %v", cl, err)
	}
}

func TestRunLocalFromAMarkdownChecklist(t *testing.T) {
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub", "LHA_MAX_TURNS_PER_CYCLE=1")
	linkFakeToolbox(t)
	roadmap := filepath.Join(dir, "roadmap.md")
	body := "# Roadmap title\n\nThe description.\n\n## Phase 1\n\n- [ ] first (witness: cmd:true)\n\n## Phase 2\n\n- [ ] second\n"
	if err := os.WriteFile(roadmap, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(dir, "ws")
	r := runCLI(t, nil, "run-local", "--checklist", roadmap, "--workdir", workdir, "--reference", "docs/ref.md",
		"--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", "true")
	if r.code != 0 || !strings.Contains(r.stdout, ": complete\nitems 2/2  cycles 2") {
		t.Fatalf("%+v", r)
	}
	var spec contracts.MissionSpec
	if err := json.Unmarshal([]byte(git(t, workdir, "show", "HEAD:.lha/mission.json")), &spec); err != nil ||
		spec.Title != "Roadmap title" || spec.Description != "The description." || len(spec.References) != 1 {
		t.Fatalf("%+v %v", spec, err)
	}
}

func TestBudgetRefusalExitsThree(t *testing.T) {
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub", "LHA_BUDGET_USD_CEILING=0.5")
	linkFakeToolbox(t)
	r := runCLI(t, pricey{}, "mission", "--task", "t", "--workdir", filepath.Join(dir, "ws"),
		"--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", "true")
	if r.code != 3 || !strings.HasPrefix(r.stderr, "error: budget governor refused model call: ") {
		t.Fatalf("%+v", r)
	}
}

// pricey costs $1 per call.
type pricey struct{}

func (pricey) Name() string { return "fake:pricey" }
func (pricey) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	return contracts.TurnResult{Text: "[]"}, nil
}
func (pricey) EstimateCostUSD(contracts.Usage) (float64, error) { return 1.0, nil }

func TestShlexSplitMatchesPython(t *testing.T) {
	for in, want := range map[string][]string{
		`uv run pytest -q`:           {"uv", "run", "pytest", "-q"},
		`sh -c 'a "b" c'`:            {"sh", "-c", `a "b" c`},
		`echo "a\"b\\c\d" x\ y ''`:   {"echo", `a"b\c\d`, "x y", ""},
		"  ":                         {},
		`a'b'"c"`:                    {"abc"},
		"tab\tsep\nline":             {"tab", "sep", "line"},
		`"it's" 'say "hi"' \'quoted`: {"it's", `say "hi"`, "'quoted"},
	} {
		got, err := shlexSplit(in)
		if err != nil || strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Errorf("%q -> %q %v, want %q", in, got, err, want)
		}
	}
	for in, msg := range map[string]string{`"open`: "No closing quotation", `'open`: "No closing quotation", `trail\`: "No escaped character"} {
		if _, err := shlexSplit(in); err == nil || err.Error() != msg {
			t.Errorf("%q: %v", in, err)
		}
	}
}

// TestPythonReadsTheGoMissionAnchor: a mission anchor produced by the Go runner (through the Go
// CLI) is read back by the Python reference implementation.
func TestPythonReadsTheGoMissionAnchor(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping the cross-implementation check")
	}
	if testing.Short() {
		t.Skip("cross-implementation check is slow")
	}
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub")
	linkFakeToolbox(t)
	workdir := filepath.Join(dir, "ws")
	r := runCLI(t, scriptedLead(), "run-local", "--item", "write hello.txt", "--item", "then idle", "--title", "Cross",
		"--description", "Go writes, Python reads.", "--workdir", workdir, "--reference", "docs/ref.md",
		"--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", "test -f hello.txt")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	_, file, _, _ := runtime.Caller(0)
	project := filepath.Join(filepath.Dir(file), "..", "..", "..", "python")
	script := `
import asyncio, json, sys
from lha.contracts.state import EventRecord
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
a = GitMissionAnchor(sys.argv[1])
async def main():
    check = await a.verify_decisions()
    snap = await a.read_situational_awareness()
    mission = await a.read_mission()
    checklist = await a.read_checklist()
    decisions = await a.read_decisions()
    events = git_ops.show_at_head(sys.argv[1], ".lha/events.ndjson")
    kinds = [EventRecord.model_validate_json(ln).kind for ln in events.split("\n") if ln]
    print(json.dumps({
        "chain_ok": check.ok, "checked": check.checked, "legacy": check.legacy,
        "title": mission.title, "anchor": mission.render_anchor(),
        "statuses": [(i.id, i.status, i.verified_by) for i in checklist.items],
        "complete": snap.is_complete, "decisions": [d.model_dump() for d in decisions],
        "event_kinds": kinds,
    }))
asyncio.run(main())
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "uv", "run", "--quiet", "--project", project, "python", "-c", script, workdir)
	cmd.Dir = project
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, stderr.String())
	}
	var got struct {
		ChainOK    bool                       `json:"chain_ok"`
		Checked    int                        `json:"checked"`
		Legacy     int                        `json:"legacy"`
		Title      string                     `json:"title"`
		Anchor     string                     `json:"anchor"`
		Statuses   [][]any                    `json:"statuses"`
		Complete   bool                       `json:"complete"`
		Decisions  []contracts.DecisionRecord `json:"decisions"`
		EventKinds []string                   `json:"event_kinds"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !got.ChainOK || got.Checked != 1 || got.Legacy != 0 || got.Title != "Cross" || !got.Complete {
		t.Fatalf("%s", out)
	}
	if got.Anchor != "Mission: Cross\n\nGo writes, Python reads.\n\nReference material (vendored, read-only; consult it instead of guessing):\n- docs/ref.md" {
		t.Fatalf("anchor: %q", got.Anchor)
	}
	statuses, _ := json.Marshal(got.Statuses)
	if string(statuses) != `[["01","done",["test"]],["02","done",["test"]]]` {
		t.Fatalf("statuses: %s", statuses)
	}
	if len(got.Decisions) != 1 || got.Decisions[0].Decision != "greet in English" || got.Decisions[0].CycleID != "c1" ||
		strings.Join(got.Decisions[0].Affected, ",") != "hello.txt" {
		t.Fatalf("decisions: %+v", got.Decisions)
	}
	if strings.Join(got.EventKinds, ",") != "cycle,cycle" {
		t.Fatalf("events: %v", got.EventKinds)
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestEgressProxyCommand(t *testing.T) {
	cleanEnv(t, "LHA_PROXY_ALLOW=.example.org, docs.example.com", "LHA_PROXY_PORT=0", "LHA_PROXY_BIND=127.0.0.1")
	if r := runCLI(t, nil, "--help"); strings.Contains(r.stdout, "egress-proxy") {
		t.Fatal("egress-proxy is a hidden command")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var stderr syncBuffer
	c := &cli{stdout: io.Discard, stderr: &stderr, ctx: ctx}
	done := make(chan int)
	go func() { done <- c.run([]string{"egress-proxy"}) }()
	ready := regexp.MustCompile(`INFO lha-egress-proxy listening on 127\.0\.0\.1:(\d+) allow=\.example\.org,docs\.example\.com\n`)
	deadline := time.Now().Add(5 * time.Second)
	for !ready.MatchString(stderr.String()) {
		if time.Now().After(deadline) {
			t.Fatalf("proxy not ready: %q", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	port := ready.FindStringSubmatch(stderr.String())[1]
	conn, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "CONNECT evil.example.net:443 HTTP/1.1\r\nHost: evil.example.net:443\r\n\r\n")
	status, _ := bufio.NewReader(conn).ReadString('\n')
	conn.Close()
	if !strings.HasPrefix(status, "HTTP/1.1 403") {
		t.Fatalf("a host outside the allow-list must be refused: %q", status)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	cleanEnv(t, "LHA_PROXY_PORT=nope")
	if r := runCLI(t, nil, "egress-proxy"); r.code != 2 || r.stderr != "error: invalid literal for int() with base 10: 'nope'\n" {
		t.Fatalf("%+v", r)
	}
}
