package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent/agenttest"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// Ported from python/tests/unit/test_agent_loop.py and test_large_missions.py: a scripted stub
// model drives the REAL loop, verifier, git anchor and checkpoint; the execution layer is the
// agenttest fake (host session + write_file/read_file dispatcher).

var fileHasHello = contracts.Check{Name: "out_has_hello", Gating: true, Where: "sandbox",
	Command: []string{"sh", "-c", `test "$(cat out.txt)" = hello`}}

var passCheck = contracts.Check{Name: "always", Gating: true, Where: "sandbox", Command: []string{"true"}}

func failCheck(script string) contracts.Check {
	return contracts.Check{Name: "fail", Gating: true, Where: "sandbox", Command: []string{"sh", "-c", script}}
}

func text(s string) contracts.TurnResult { return contracts.TurnResult{Text: s} }

var done = text(`{"done": true, "summary": "ok"}`)

func writeTurn(path, content string) contracts.TurnResult {
	args, _ := json.Marshal(map[string]string{"path": path, "content": content})
	return text(`{"tool": "write_file", "arguments": ` + string(args) + `}`)
}

func nativeWrite(id, path, content string) contracts.TurnResult {
	return contracts.TurnResult{ToolCalls: []contracts.ToolCall{{ID: id, Name: "write_file",
		Arguments: map[string]any{"path": path, "content": content}}}, StopReason: contracts.Str("tool_use")}
}

// recordingModel is a scripted stub that records every message list it was sent.
type recordingModel struct {
	*model.StubModel
	mu    sync.Mutex
	calls [][]contracts.ModelMessage
	// before runs before each turn (e.g. to tamper with the workspace like an agent would).
	before func()
}

func newRecording(script ...contracts.TurnResult) *recordingModel {
	return &recordingModel{StubModel: model.NewStub(script)}
}

func (m *recordingModel) Complete(ctx context.Context, msgs []contracts.ModelMessage, tools []map[string]any, maxTokens int) (contracts.TurnResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, append([]contracts.ModelMessage{}, msgs...))
	m.mu.Unlock()
	if m.before != nil {
		m.before()
	}
	return m.StubModel.Complete(ctx, msgs, tools, maxTokens)
}

type fixture struct {
	dir    string
	anchor *state.GitMissionAnchor
	tctx   contracts.ToolContext
	disp   *agenttest.Dispatcher
}

func setup(t *testing.T, items ...contracts.ChecklistItem) *fixture {
	t.Helper()
	dir := t.TempDir()
	return setupIn(t, dir, items...)
}

func setupIn(t *testing.T, dir string, items ...contracts.ChecklistItem) *fixture {
	t.Helper()
	anchor := state.NewGitMissionAnchor(dir)
	if _, err := anchor.Initialize(context.Background(), "T", "D", contracts.Checklist{Items: items, SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	box := agenttest.NewToolbox(dir)
	return &fixture{dir: dir, anchor: anchor, disp: box.Disp,
		tctx: contracts.ToolContext{MissionID: "m1", Session: box.Session()}}
}

func (f *fixture) loop(m contracts.ModelProvider, mutate func(*LoopOptions)) *AgentLoop {
	opts := DefaultLoopOptions()
	opts.Model = m
	opts.Dispatcher = f.disp
	opts.Verifier = &verify.DeterministicVerifier{DefaultTimeoutS: 60, OutputTail: verify.OutputTailChars}
	opts.Anchor = f.anchor
	if mutate != nil {
		mutate(&opts)
	}
	return NewAgentLoop(opts)
}

func (f *fixture) run(t *testing.T, l *AgentLoop, cycleID string, checks ...contracts.Check) CycleOutcome {
	t.Helper()
	out, err := l.RunCycle(context.Background(), f.tctx, "m1", cycleID, "", checks)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *fixture) item(t *testing.T, id string) contracts.ChecklistItem {
	t.Helper()
	cl, err := f.anchor.ReadChecklist(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	it := cl.Get(id)
	if it == nil {
		t.Fatalf("no item %s", id)
	}
	return *it
}

func (f *fixture) git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := state.RunGit(context.Background(), f.dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func item(id, desc string) contracts.ChecklistItem { return contracts.NewChecklistItem(id, desc) }

func withWitnesses(it contracts.ChecklistItem, w ...string) contracts.ChecklistItem {
	it.Witnesses = w
	return it
}

func TestParseActionVariants(t *testing.T) {
	a := ParseAction(`{"tool": "read_file", "arguments": {"path": "x"}}`, nil, nil)
	if a.Tool != "read_file" || a.Arguments["path"] != "x" || a.Done || !a.IsValid() {
		t.Fatalf("tool: %+v", a)
	}
	d := ParseAction(`{"done": true, "summary": "ok"}`, nil, nil)
	if !d.Done || d.Summary != "ok" {
		t.Fatalf("done: %+v", d)
	}
	n := ParseAction("ignored", []contracts.ToolCall{{ID: "1", Name: "grep", Arguments: map[string]any{"pattern": "x"}}}, nil)
	if n.Tool != "grep" || n.Arguments["pattern"] != "x" {
		t.Fatalf("native: %+v", n)
	}
	// str(summary) of a non-string, like Python.
	if s := ParseAction(`{"done": true, "summary": [1, "a", null, 2.50]}`, nil, nil).Summary; s != "[1, 'a', None, 2.5]" {
		t.Fatalf("summary repr: %q", s)
	}
}

func TestParseActionUnparseableIsNeverDone(t *testing.T) {
	prose := ParseAction("I am finished.", nil, nil)
	if prose.Done || prose.IsValid() || prose.Tool != "" {
		t.Fatalf("prose: %+v", prose)
	}
	noAction := ParseAction(`{"summary": "hi"}`, nil, nil)
	if noAction.Done || noAction.IsValid() {
		t.Fatalf("no action: %+v", noAction)
	}
	truncated := ParseAction(`{"done": true}`, nil, contracts.Str("max_tokens"))
	if truncated.Done || !strings.Contains(truncated.Error, "truncated") {
		t.Fatalf("truncated: %+v", truncated)
	}
	if ParseAction(`{"done": "yes"}`, nil, nil).Done {
		t.Fatal(`"done": "yes" is not done`)
	}
}

func TestLoopUsesToolThenCompletes(t *testing.T) {
	f := setup(t, item("01", "write a file"))
	out := f.run(t, f.loop(model.NewStub([]contracts.TurnResult{writeTurn("out.txt", "hello"), done}), nil), "c1", fileHasHello)
	if !out.Advanced || !out.Verified || out.Verdict != "passed" || !out.IsComplete || out.ToolCalls != 1 {
		t.Fatalf("outcome: %+v", out)
	}
	data, _ := os.ReadFile(filepath.Join(f.dir, "out.txt"))
	if string(data) != "hello" {
		t.Fatalf("out.txt = %q", data)
	}
	if !strings.Contains(f.git(t, "log", "--oneline"), "lha: complete 01 (write a file)") {
		t.Fatal("no completion commit")
	}
	it := f.item(t, "01")
	if it.Status != "done" || strings.Join(it.VerifiedBy, ",") != "out_has_hello" {
		t.Fatalf("item: %+v", it)
	}
}

func TestZeroChecksIsUnverifiedNeverDone(t *testing.T) {
	f := setup(t, item("01", "x"))
	out := f.run(t, f.loop(model.NewStub([]contracts.TurnResult{done}), nil), "c1")
	if out.Verified || out.Verdict != "unverified" || out.IsComplete {
		t.Fatalf("outcome: %+v", out)
	}
	it := f.item(t, "01")
	if it.Status != "in_progress" || !strings.Contains(it.LastFailure, "UNVERIFIED") {
		t.Fatalf("item: %+v", it)
	}
}

func TestAdvisoryOnlyChecksAreUnverified(t *testing.T) {
	f := setup(t, item("01", "x"))
	adv := contracts.Check{Name: "adv", Command: []string{"true"}, Gating: false, Where: "sandbox"}
	out := f.run(t, f.loop(model.NewStub([]contracts.TurnResult{done}), nil), "c1", adv)
	if out.Verdict != "unverified" || out.IsComplete {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestLoopBlocksOnFailedVerification(t *testing.T) {
	f := setup(t, item("01", "x"))
	l := f.loop(model.NewStub([]contracts.TurnResult{done}), func(o *LoopOptions) { o.MaxTurns = 1 })
	out := f.run(t, l, "c1", failCheck("echo boom; exit 1"))
	if !out.Advanced || out.Verified || out.IsComplete {
		t.Fatalf("outcome: %+v", out)
	}
	if !strings.Contains(f.git(t, "log", "--oneline"), "lha: attempt 01") {
		t.Fatal("no attempt commit")
	}
	if !strings.Contains(f.item(t, "01").LastFailure, "boom") {
		t.Fatal("check output did not reach the next attempt")
	}
}

func TestRepeatedFailureBlocksItemAndIndependentItemsProceed(t *testing.T) {
	f := setup(t, item("01", "impossible"), contracts.NewChecklistItem("02", "depends on 01", "01"), item("03", "independent"))
	l := f.loop(model.NewStub([]contracts.TurnResult{done}), func(o *LoopOptions) {
		o.MaxTurns, o.MaxConsecutiveFailures = 1, 2
	})
	fail := failCheck("exit 1")
	first := f.run(t, l, "c1", fail)
	second := f.run(t, l, "c2", fail)
	if first.ItemID != "01" || second.ItemID != "01" || !second.ItemBlocked || second.IsDeadlocked {
		t.Fatalf("first %+v second %+v", first, second)
	}
	if third := f.run(t, l, "c3", fail); third.ItemID != "03" {
		t.Fatalf("blocked item was re-picked: %+v", third)
	}
	f.run(t, l, "c4", fail)
	idle := f.run(t, l, "c5", fail)
	if idle.ItemID != "" || idle.Advanced || !idle.IsDeadlocked || idle.IsComplete || !strings.Contains(idle.Reason, "blocked") {
		t.Fatalf("idle: %+v", idle)
	}
}

func lastUser(msgs []contracts.ModelMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

func TestUnparseableReplyGetsCorrectiveTurn(t *testing.T) {
	f := setup(t, item("01", "x"))
	m := newRecording(text("Sure, I'll do that now."),
		contracts.TurnResult{Text: `{"done": true`, StopReason: contracts.Str("max_tokens")},
		writeTurn("out.txt", "hello"), done)
	out := f.run(t, f.loop(m, nil), "c1", fileHasHello)
	if !out.Verified || out.Turns != 4 {
		t.Fatalf("outcome: %+v", out)
	}
	if !strings.Contains(lastUser(m.calls[1]), "not a valid action") {
		t.Fatal("no corrective turn")
	}
	if !strings.Contains(lastUser(m.calls[2]), "truncated") {
		t.Fatal("truncation not explained")
	}
}

func TestFailedDoneIsFedBackAndFixedWithinCycle(t *testing.T) {
	f := setup(t, item("01", "x"))
	m := newRecording(writeTurn("out.txt", "nope"), done, writeTurn("out.txt", "hello"), done)
	out := f.run(t, f.loop(m, nil), "c1", fileHasHello)
	if !out.Verified || out.Turns != 4 {
		t.Fatalf("outcome: %+v", out)
	}
	last := m.calls[2][len(m.calls[2])-1].Content
	if !strings.Contains(last, "VERIFICATION FAILED") {
		t.Fatalf("not fed back: %q", last)
	}
}

func TestNativeToolCallsAllExecutedAndPaired(t *testing.T) {
	f := setup(t, item("01", "x"))
	m := newRecording(contracts.TurnResult{Text: "writing two files", ToolCalls: []contracts.ToolCall{
		{ID: "t1", Name: "write_file", Arguments: map[string]any{"path": "a.txt", "content": "1"}},
		{ID: "t2", Name: "write_file", Arguments: map[string]any{"path": "out.txt", "content": "hello"}},
	}}, done)
	out := f.run(t, f.loop(m, nil), "c1", fileHasHello)
	if out.ToolCalls != 2 || !out.Verified {
		t.Fatalf("outcome: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	msgs := m.calls[1]
	assistant, first, second := msgs[len(msgs)-3], msgs[len(msgs)-2], msgs[len(msgs)-1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 2 || assistant.ToolCalls[0].ID != "t1" ||
		assistant.ToolCalls[1].ID != "t2" || assistant.Content != "writing two files" {
		t.Fatalf("assistant: %+v", assistant)
	}
	if first.Role != "tool" || *first.ToolCallID != "t1" || second.Role != "tool" || *second.ToolCallID != "t2" {
		t.Fatalf("pairing: %+v %+v", first, second)
	}
}

func TestNativeToolCallsWithoutIDsGetCycleIDs(t *testing.T) {
	f := setup(t, item("01", "x"))
	m := newRecording(nativeWrite("", "out.txt", "hello"), done)
	f.run(t, f.loop(m, nil), "c7", fileHasHello)
	if got := f.disp.Calls[0].ID; got != "c7-1-0" {
		t.Fatalf("id = %q", got)
	}
}

func tamper(dir string, files map[string]*string) func() {
	return func() {
		for rel, content := range files {
			p := filepath.Join(dir, rel)
			if content == nil {
				_ = os.Remove(p)
			} else {
				_ = os.WriteFile(p, []byte(*content), 0o644)
			}
		}
	}
}

func TestAgentCannotWriteItsOwnVerdict(t *testing.T) {
	f := setup(t, item("01", "x"))
	forgedItem := item("01", "x")
	forgedItem.Status = "done"
	forged, _ := json.Marshal(contracts.Checklist{Items: []contracts.ChecklistItem{forgedItem}, SchemaVersion: 1})
	evil := `{"title": "evil"}`
	fs := string(forged)
	m := newRecording(done)
	m.before = tamper(f.dir, map[string]*string{".lha/checklist.json": &fs, ".lha/mission.json": &evil})
	out := f.run(t, f.loop(m, nil), "c1", failCheck("exit 1"))
	if out.Verified || out.IsComplete {
		t.Fatalf("outcome: %+v", out)
	}
	if f.item(t, "01").Status == "done" {
		t.Fatal("forged verdict committed")
	}
	data, _ := os.ReadFile(filepath.Join(f.dir, ".lha", "checklist.json"))
	var onDisk contracts.Checklist
	if err := json.Unmarshal(data, &onDisk); err != nil || onDisk.Items[0].Status == "done" {
		t.Fatalf("forged working-tree file survived: %s", data)
	}
	mission, err := f.anchor.ReadMission(context.Background())
	if err != nil || mission == nil || mission.Title != "T" {
		t.Fatalf("mission: %+v %v", mission, err)
	}
}

func setupWithTests(t *testing.T, it contracts.ChecklistItem) *fixture {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tests", "test_core.py"), []byte("assert 1 == 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return setupIn(t, dir, it)
}

func strp(s string) *string { return &s }

func TestHarnessTamperingFailsAndIsReverted(t *testing.T) {
	f := setupWithTests(t, item("01", "x"))
	m := newRecording(done)
	m.before = tamper(f.dir, map[string]*string{"tests/test_core.py": strp("assert True\n"), "tests/test_new.py": strp("x = 1\n")})
	out := f.run(t, f.loop(m, nil), "c1", passCheck)
	if out.Verified || out.Verdict != "failed" {
		t.Fatalf("outcome: %+v", out)
	}
	it := f.item(t, "01")
	if !strings.Contains(it.LastFailure, "harness_integrity") || !strings.Contains(it.LastFailure, "tests/test_core.py") {
		t.Fatalf("failure: %s", it.LastFailure)
	}
	data, _ := os.ReadFile(filepath.Join(f.dir, "tests", "test_core.py"))
	if string(data) != "assert 1 == 2\n" {
		t.Fatalf("not reverted: %q", data)
	}
	// New tests are not a violation, but the failed attempt is rolled back as a whole: its new
	// file is kept on the attempt ref, not in the checkout.
	if _, err := os.Stat(filepath.Join(f.dir, "tests", "test_new.py")); !os.IsNotExist(err) {
		t.Fatal("the failed attempt's new file is still in the checkout")
	}
	if saved := f.git(t, "show", "refs/lha/attempts/m1/c1:tests/test_new.py"); saved != "x = 1" {
		t.Fatalf("attempt ref: %q", saved)
	}
}

func TestHarnessDeletionDetected(t *testing.T) {
	f := setupWithTests(t, item("01", "x"))
	m := newRecording(done)
	m.before = tamper(f.dir, map[string]*string{"tests/test_core.py": nil})
	if out := f.run(t, f.loop(m, nil), "c1", passCheck); out.Verified {
		t.Fatalf("outcome: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "tests", "test_core.py")); err != nil {
		t.Fatal("not restored from HEAD")
	}
}

func TestHarnessEditsAllowedWhenItemOptsIn(t *testing.T) {
	it := item("01", "fix test")
	it.AllowHarnessEdits = true
	f := setupWithTests(t, it)
	m := newRecording(done)
	m.before = tamper(f.dir, map[string]*string{"tests/test_core.py": strp("assert True\n")})
	if out := f.run(t, f.loop(m, nil), "c1", passCheck); !out.Verified {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestMissionAnchorSurvivesManyCycles(t *testing.T) {
	dir := t.TempDir()
	anchor := state.NewGitMissionAnchor(dir)
	if _, err := anchor.Initialize(context.Background(), "Build the Frobnicator", "It must frobnicate widgets.",
		contracts.Checklist{Items: []contracts.ChecklistItem{item("01", "a")}, SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	box := agenttest.NewToolbox(dir)
	f := &fixture{dir: dir, anchor: anchor, disp: box.Disp, tctx: contracts.ToolContext{MissionID: "m1", Session: box.Session()}}
	m := newRecording(done)
	l := f.loop(m, func(o *LoopOptions) { o.MaxTurns, o.MaxConsecutiveFailures = 1, 10 })
	for _, c := range []string{"c0", "c1", "c2"} {
		f.run(t, l, c, failCheck("exit 1"))
	}
	system := m.calls[len(m.calls)-1][0].Content
	if !strings.Contains(system, "Build the Frobnicator") || !strings.Contains(system, "It must frobnicate widgets.") {
		t.Fatalf("system: %s", system)
	}
	progress, _ := os.ReadFile(filepath.Join(dir, ".lha", "progress.md"))
	for _, want := range []string{"Build the Frobnicator", "c0 [01]", "c2 [01]"} {
		if !strings.Contains(string(progress), want) {
			t.Fatalf("progress lacks %q:\n%s", want, progress)
		}
	}
}

// --- witnesses, protected paths, replanning, rollback (test_large_missions.py) --------------

func TestItemWitnessGatesOnTopOfMissionChecks(t *testing.T) {
	f := setup(t, withWitnesses(item("01", "needs its witness"), "cmd:exit 3"))
	l := f.loop(model.NewStub([]contracts.TurnResult{done, done}), func(o *LoopOptions) { o.MaxTurns = 2 })
	out := f.run(t, l, "c1", passCheck)
	if out.Verified || out.Verdict != "failed" || !strings.Contains(f.item(t, "01").LastFailure, "cmd:exit 3 FAILED") {
		t.Fatalf("outcome: %+v / %s", out, f.item(t, "01").LastFailure)
	}
}

func TestPassingWitnessMarksItemDoneAndIsRecorded(t *testing.T) {
	f := setup(t, withWitnesses(item("01", "ok"), "cmd:true"))
	out := f.run(t, f.loop(model.NewStub([]contracts.TurnResult{done}), nil), "c1", passCheck)
	if !out.Verified || strings.Join(f.item(t, "01").VerifiedBy, ",") != "always,cmd:true" {
		t.Fatalf("outcome: %+v %+v", out, f.item(t, "01"))
	}
}

func TestWitnessAloneCanGateAnItem(t *testing.T) {
	f := setup(t, withWitnesses(item("01", "ok"), "cmd:true"))
	if out := f.run(t, f.loop(model.NewStub([]contracts.TurnResult{done}), nil), "c1"); !out.Verified {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestUnusableWitnessFailsNeverSkips(t *testing.T) {
	for _, w := range []string{"bogus:x", "trusted:e2e"} {
		f := setup(t, withWitnesses(item("01", "x"), w))
		out := f.run(t, f.loop(model.NewStub([]contracts.TurnResult{done, done}), nil), "c1", passCheck)
		if out.Verified || !strings.Contains(f.item(t, "01").LastFailure, "invalid witness") {
			t.Fatalf("%s: %+v", w, out)
		}
	}
}

func TestTrustedWitnessRunsOutsideTheSandbox(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "-trusted-ran"
	t.Cleanup(func() { os.Remove(marker) })
	f := setupIn(t, dir, withWitnesses(item("01", "x"), "trusted:e2e"))
	l := f.loop(model.NewStub([]contracts.TurnResult{nativeWrite("w", "hello.txt", "hi"), done}), func(o *LoopOptions) {
		o.Verifier = verify.NewTrustedAwareVerifier(verify.NewDeterministicVerifier(), verify.NewCommandTrustedRunner(), dir)
		o.TrustedChecks = map[string][]string{"e2e": {"sh", "-c", `test -f hello.txt && touch "` + marker + `"`}}
	})
	out := f.run(t, l, "c1", passCheck)
	if _, err := os.Stat(marker); !out.Verified || err != nil {
		t.Fatalf("outcome: %+v marker: %v", out, err)
	}
}

func TestTrustedCheckIsRefusedByThePlainSandboxVerifier(t *testing.T) {
	res, err := verify.NewDeterministicVerifier().Verify(context.Background(), &agenttest.Session{Dir: t.TempDir()},
		[]contracts.Check{{Name: "e2e", Command: []string{"true"}, Gating: true, Where: "trusted"}})
	if err != nil || res.AllGreen || !strings.Contains(res.Results[0].OutputTail, "needs a trusted runner") {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestProtectedGlobBlocksEditingTheGateDefinition(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("e2e:\n\t./run-e2e\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := setupIn(t, dir, item("01", "x"))
	l := f.loop(model.NewStub([]contracts.TurnResult{nativeWrite("w", "Makefile", "e2e:\n\ttrue\n"), done}), func(o *LoopOptions) {
		o.MaxTurns = 2
		o.HarnessGlobs = []string{"Makefile"}
	})
	if out := f.run(t, l, "c1", passCheck); out.Verified {
		t.Fatalf("outcome: %+v", out)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "Makefile")); string(data) != "e2e:\n\t./run-e2e\n" {
		t.Fatalf("not reverted: %q", data)
	}
}

func TestBlockedItemIsSplitInsteadOfDeadlocking(t *testing.T) {
	f := setup(t, withWitnesses(item("01", "coarse"), "cmd:exit 1"), contracts.NewChecklistItem("02", "after", "01"))
	plan := text(`[{"description": "part A"}, {"description": "part B"}]`)
	m := model.NewStub([]contracts.TurnResult{done, done, plan}) // the lead's model also replans
	l := f.loop(m, func(o *LoopOptions) {
		o.MaxTurns, o.MaxConsecutiveFailures, o.MaxReplans = 2, 1, 5
		o.Replanner = agents.NewReplanner(m)
	})
	out := f.run(t, l, "c1", passCheck)
	if !out.ItemSplit || out.ItemBlocked || out.IsDeadlocked {
		t.Fatalf("outcome: %+v", out)
	}
	cl, _ := f.anchor.ReadChecklist(context.Background())
	got := []string{}
	for _, it := range cl.Items {
		got = append(got, it.ID+":"+it.Status)
	}
	if strings.Join(got, " ") != "01:split 02:todo 01.1:todo 01.2:todo" {
		t.Fatalf("items: %v", got)
	}
	if w := cl.Get("01.2").Witnesses; len(w) != 1 || w[0] != "cmd:exit 1" {
		t.Fatalf("witnesses: %v", w)
	}
	if d := cl.Get("02").DependsOn; len(d) != 1 || d[0] != "01.2" {
		t.Fatalf("depends_on: %v", d)
	}
	if !strings.Contains(f.git(t, "log", "--oneline", "-1"), "lha: split 01") {
		t.Fatal("no split commit")
	}
}

type failingSplitter struct{}

func (failingSplitter) Split(context.Context, string, contracts.ChecklistItem) ([]contracts.ChecklistItem, error) {
	panic("the replanner must not be called")
}

func TestReplanningRespectsTheBudgetAndDepth(t *testing.T) {
	f := setup(t, withWitnesses(item("01.1.1", "deep"), "cmd:exit 1"))
	l := f.loop(model.NewStub([]contracts.TurnResult{done, done}), func(o *LoopOptions) {
		o.MaxTurns, o.MaxConsecutiveFailures, o.MaxReplans, o.MaxSplitDepth = 2, 1, 5, 2
		o.Replanner = failingSplitter{}
	})
	if out := f.run(t, l, "c1", passCheck); !out.ItemBlocked || !out.IsDeadlocked {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestUnusableSplitLeavesTheItemBlocked(t *testing.T) {
	f := setup(t, withWitnesses(item("01", "x"), "cmd:exit 1"))
	m := model.NewStub([]contracts.TurnResult{done, done, text("no idea")})
	l := f.loop(m, func(o *LoopOptions) {
		o.MaxTurns, o.MaxConsecutiveFailures, o.MaxReplans = 2, 1, 5
		o.Replanner = agents.NewReplanner(m)
	})
	if out := f.run(t, l, "c1", passCheck); !out.ItemBlocked || out.ItemSplit {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestFailedAttemptIsRolledBackAndKeptOnARef(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "lib.py"), []byte("GOOD = 1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("cache/\n"), 0o644)
	f := setupIn(t, dir, withWitnesses(item("01", "x"), "cmd:exit 1"))
	_ = os.MkdirAll(filepath.Join(dir, "cache"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "cache", "keep.bin"), []byte("ignored build output"), 0o644)
	l := f.loop(model.NewStub([]contracts.TurnResult{nativeWrite("w", "lib.py", "BROKEN = (\n"), nativeWrite("w2", "new.py", "x = 1\n")}),
		func(o *LoopOptions) { o.MaxTurns = 2 })
	out, err := l.RunCycle(context.Background(), f.tctx, "m1", "c1", "", []contracts.Check{passCheck})
	if err != nil || out.Verified {
		t.Fatalf("outcome: %+v %v", out, err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "lib.py")); string(data) != "GOOD = 1\n" {
		t.Fatalf("lib.py = %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.py")); !os.IsNotExist(err) {
		t.Fatal("new.py survived the rollback")
	}
	if _, err := os.Stat(filepath.Join(dir, "cache", "keep.bin")); err != nil {
		t.Fatal("ignored files must be left alone")
	}
	ref := "refs/lha/attempts/m1/c1"
	if got := f.git(t, "show", ref+":lib.py"); got != "BROKEN = (" {
		t.Fatalf("ref content %q", got)
	}
	if strings.Contains(f.git(t, "show", "--name-only", "--format=", "HEAD"), "lib.py") {
		t.Fatal("failed code was committed")
	}
	last := f.item(t, "01").LastFailure
	if !strings.Contains(last, "rolled back to the last verified state (kept at "+ref+")") || !strings.Contains(last, "lib.py, new.py") {
		t.Fatalf("failure: %s", last)
	}
}

func TestPassingAttemptIsCommittedNotRolledBack(t *testing.T) {
	f := setup(t, withWitnesses(item("01", "x"), "cmd:test -f new.py"))
	l := f.loop(model.NewStub([]contracts.TurnResult{nativeWrite("w", "new.py", "x = 1\n"), done}), func(o *LoopOptions) { o.MaxTurns = 2 })
	if out := f.run(t, l, "c1", passCheck); !out.Verified {
		t.Fatalf("outcome: %+v", out)
	}
	if !strings.Contains(f.git(t, "show", "--name-only", "--format=", "HEAD"), "new.py") {
		t.Fatal("new.py not committed")
	}
	if refs := f.git(t, "for-each-ref", "refs/lha/attempts"); refs != "" {
		t.Fatalf("refs: %s", refs)
	}
}

func TestFailedAttemptWithoutCodeChangesCreatesNoRef(t *testing.T) {
	f := setup(t, withWitnesses(item("01", "x"), "cmd:exit 1"))
	if out := f.run(t, f.loop(model.NewStub([]contracts.TurnResult{done, done}), func(o *LoopOptions) { o.MaxTurns = 2 }), "c1", passCheck); out.Verified {
		t.Fatalf("outcome: %+v", out)
	}
	if refs := f.git(t, "for-each-ref", "refs/lha/attempts"); refs != "" {
		t.Fatalf("refs: %s", refs)
	}
	if strings.Contains(f.item(t, "01").LastFailure, "rolled back") {
		t.Fatal("nothing was rolled back")
	}
}

func TestGateEventsAndCycleEventAreCommitted(t *testing.T) {
	f := setup(t, item("01", "x"))
	f.disp.Events = []contracts.EventRecord{{Kind: "gate_resolved", Payload: contracts.Payload("decision", "reject")}}
	f.run(t, f.loop(model.NewStub([]contracts.TurnResult{done}), nil), "c9", passCheck)
	events := f.git(t, "show", "HEAD:.lha/events.ndjson")
	lines := strings.Split(events, "\n")
	if len(lines) != 2 {
		t.Fatalf("events: %s", events)
	}
	var gate, cycle map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &gate)
	_ = json.Unmarshal([]byte(lines[1]), &cycle)
	if gate["kind"] != "gate_resolved" || gate["cycle_id"] != "c9" || cycle["kind"] != "cycle" {
		t.Fatalf("gate %v cycle %v", gate, cycle)
	}
	payload := cycle["payload"].(map[string]any)
	if payload["item_id"] != "01" || payload["verdict"] != "passed" || payload["status"] != "done" {
		t.Fatalf("payload %v", payload)
	}
}

func TestRecordedDecisionIsChainedByTheCheckpoint(t *testing.T) {
	f := setup(t, item("01", "x"))
	f.anchor.RecordDecision(contracts.DecisionRecord{Decision: "use sqlite", Rationale: "simple", Affected: []string{}})
	f.run(t, f.loop(model.NewStub([]contracts.TurnResult{done}), nil), "c3", passCheck)
	records, err := f.anchor.ReadDecisions(context.Background())
	if err != nil || len(records) != 1 || records[0].CycleID != "c3" {
		t.Fatalf("%+v %v", records, err)
	}
}

// python: test_large_missions.py::test_a_split_item_does_not_starve_independent_items.
func TestASplitItemDoesNotStarveIndependentItems(t *testing.T) {
	f := setup(t, withWitnesses(item("01", "hard"), "cmd:exit 1"), item("02", "easy, independent"))
	plan := text(`[{"description": "part A"}, {"description": "part B"}]`)
	m := model.NewStub([]contracts.TurnResult{done, done, plan, done, done})
	l := f.loop(m, func(o *LoopOptions) {
		o.MaxTurns, o.MaxConsecutiveFailures, o.MaxReplans = 2, 1, 5
		o.Replanner = agents.NewReplanner(m)
	})
	first := f.run(t, l, "c1", passCheck)
	second := f.run(t, l, "c2", passCheck)
	if first.ItemID != "01" || !first.ItemSplit {
		t.Fatalf("first: %+v", first)
	}
	if second.ItemID != "02" || !second.Verified {
		t.Fatalf("second worked %q, want the independent item 02: %+v", second.ItemID, second)
	}
}

func TestRunningOutOfTurnsIsRecorded(t *testing.T) {
	f := setup(t, item("01", "x"))
	rec := obs.NewTraceRecorder(nil)
	kinds := func(cycleID string) (found []obs.TraceEvent) {
		for _, e := range rec.Events() {
			if e.CycleID == cycleID && e.Kind == "turns_exhausted" {
				found = append(found, e)
			}
		}
		return found
	}
	withRec := func(o *LoopOptions) { o.MaxTurns, o.Recorder = 2, rec }
	f.run(t, f.loop(model.NewStub([]contracts.TurnResult{writeTurn("out.txt", "a"), writeTurn("out.txt", "b")}), withRec), "c1", fileHasHello)
	got := kinds("c1")
	if len(got) != 1 {
		t.Fatalf("events: %+v", rec.Events())
	}
	if n, _ := got[0].Data.Get("max_turns"); n != 2 {
		t.Fatal(got[0].Data)
	}
	if n, _ := got[0].Data.Get("tool_calls"); n != 2 {
		t.Fatal(got[0].Data)
	}
	f.run(t, f.loop(model.NewStub([]contracts.TurnResult{writeTurn("out.txt", "hello"), done}), withRec), "c2", fileHasHello)
	if len(kinds("c2")) != 0 {
		t.Fatal("a cycle that finished was reported as out of turns")
	}
}

func TestAFailedToolCallRecordsWhy(t *testing.T) {
	f := setup(t, item("01", "x"))
	rec := obs.NewTraceRecorder(nil)
	read := text(`{"tool": "read_file", "arguments": {"path": "missing-token=sk-live-abcdefghijklmnop.txt"}}`)
	f.run(t, f.loop(model.NewStub([]contracts.TurnResult{read, writeTurn("out.txt", "hello")}),
		func(o *LoopOptions) { o.MaxTurns, o.Recorder = 2, rec }), "c1", fileHasHello)
	var calls []obs.TraceEvent
	for _, e := range rec.Events() {
		if e.Kind == "tool_call" {
			calls = append(calls, e)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("events: %+v", rec.Events())
	}
	errText, _ := calls[0].Data.Get("error")
	if ok, _ := calls[0].Data.Get("ok"); ok != false || !strings.Contains(errText.(string), "missing") ||
		strings.Contains(errText.(string), "sk-live-abcdefghijklmnop") {
		t.Fatal(calls[0].Data)
	}
	if _, has := calls[1].Data.Get("error"); has {
		t.Fatal(calls[1].Data)
	}
	long := ToolErrorDetail(contracts.ToolResult{OK: false, Error: contracts.Str("e"), Content: strings.Repeat("é", 1000) + "END"})
	if n := len([]rune(long)); n != ToolErrorTail || !strings.HasSuffix(long, "END") {
		t.Fatal(n)
	}
}
