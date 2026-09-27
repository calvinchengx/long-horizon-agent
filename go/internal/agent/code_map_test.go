package agent

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Ported from python/tests/unit/test_code_map.py.

type fakeSession struct {
	contracts.SandboxSession
	res  contracts.ExecResult
	err  error
	argv []string
}

func (f *fakeSession) Exec(_ context.Context, argv []string, _ contracts.ExecOptions) (contracts.ExecResult, error) {
	f.argv = argv
	return f.res, f.err
}

func TestCodeMapRendersASuccessfulRun(t *testing.T) {
	s := &fakeSession{res: contracts.ExecResult{Stdout: "<ctx task='x'/>"}}
	text, info := (&RipwireCodeMap{TokenBudget: 900, TimeoutS: 5}).Render(context.Background(), s, item("01", "add farewell()"))
	if text != CodeMapHeader+"\n<ctx task='x'/>" || s.argv[3] != "--token-budget=900" || info.Value("ok") != true {
		t.Fatalf("%q %v %v", text, s.argv, info)
	}
}

func TestCodeMapFailuresMeanNoMap(t *testing.T) {
	for name, s := range map[string]*fakeSession{
		"missing": {res: contracts.ExecResult{ExitCode: 127, Stderr: "ripwire: not found"}},
		"timeout": {res: contracts.ExecResult{Stdout: "partial", TimedOut: true}},
		"broken":  {err: errors.New("sandbox gone")},
	} {
		text, info := (&RipwireCodeMap{TokenBudget: 900, TimeoutS: 5}).Render(context.Background(), s, item("01", "x"))
		if text != "" || info.Value("ok") != false || info.Value("error") == "" {
			t.Errorf("%s: %q %v", name, text, info)
		}
	}
}

func TestTheLeadSeesTheCodeMap(t *testing.T) {
	if _, err := exec.LookPath("ripwire"); err != nil {
		t.Skip("needs ripwire on PATH")
	}
	f := setup(t, item("01", "add farewell() next to greet()"))
	m := newRecording(done)
	l := f.loop(m, func(o *LoopOptions) { o.MaxTurns = 1; o.CodeMap = &RipwireCodeMap{TokenBudget: 800, TimeoutS: 30} })
	f.run(t, l, "c1", passCheck)
	user := m.calls[0][1].Content
	if !strings.Contains(user, CodeMapHeader) || strings.Index(user, CodeMapHeader) > strings.Index(user, "Recent commits:") {
		t.Fatalf("no code map before the recent commits:\n%s", user)
	}
}

type scriptedSession struct {
	contracts.SandboxSession
	trace, task contracts.ExecResult
	calls       [][]string
	envs        []map[string]string
}

func (s *scriptedSession) Exec(_ context.Context, argv []string, opts contracts.ExecOptions) (contracts.ExecResult, error) {
	s.calls = append(s.calls, argv)
	s.envs = append(s.envs, opts.Env)
	if argv[0] == "sh" {
		return s.trace, nil
	}
	return s.task, nil
}

func failingItem() contracts.ChecklistItem {
	it := item("02", "mask gitlab tokens")
	it.Witnesses = []string{"pytest:tests/test_redact.py::test_gitlab"}
	it.LastFailure = strings.Repeat("x", 5000) + "\ntests/test_redact.py:6: AssertionError"
	return it
}

func TestARetryMapsFromTheFailureReport(t *testing.T) {
	s := &scriptedSession{trace: contracts.ExecResult{Stdout: `<ctx><d p="src/redact.py:3"/></ctx>`}}
	it := failingItem()
	text, info := (&RipwireCodeMap{TokenBudget: 900, TimeoutS: 5}).Render(context.Background(), s, it)
	if !strings.Contains(text, `p="src/redact.py:3"`) || info.Value("mode") != "trace" || len(s.calls) != 1 {
		t.Fatalf("%q %v %v", text, info, s.calls)
	}
	if s.envs[0]["LHA_TRACE"] != it.LastFailure[len(it.LastFailure)-TraceChars:] || s.envs[0]["LHA_TRACE_BUDGET"] != "900" {
		t.Fatalf("env %v", s.envs[0]["LHA_TRACE_BUDGET"])
	}
	if strings.Contains(strings.Join(s.calls[0], " "), "AssertionError") {
		t.Fatal("the report is on the command line")
	}
}

func TestATraceThatFindsNothingFallsBackToTheTaskQuery(t *testing.T) {
	s := &scriptedSession{trace: contracts.ExecResult{Stdout: "<ctx/>"}, task: contracts.ExecResult{Stdout: `<ctx><d p="src/a.py:1"/></ctx>`}}
	text, info := (&RipwireCodeMap{TokenBudget: 900, TimeoutS: 5}).Render(context.Background(), s, failingItem())
	if len(s.calls) != 2 || s.calls[1][0] != "ripwire" || info.Value("mode") != "task" || info.Value("fell_back") != true ||
		!strings.Contains(text, `p="src/a.py:1"`) || !strings.Contains(s.calls[1][2], "Acceptance checks: pytest:") {
		t.Fatalf("%q %v %v", text, info, s.calls)
	}
}
