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
