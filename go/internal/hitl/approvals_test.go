package hitl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

type scenario struct {
	Name       string            `json:"name"`
	Timeout    float64           `json:"timeout"`
	Escalation []float64         `json:"escalation"`
	TTY        bool              `json:"tty"`
	Answers    []*string         `json:"answers"` // nil = timeout
	GateID     string            `json:"gate_id"`
	Question   string            `json:"question"`
	Context    map[string]string `json:"context"`
}

func s(v string) *string { return &v }

var pushContext = map[string]string{
	"tool":        "run_command",
	"arguments":   "{'argv': ['git', 'push', 'origin', 'main']}",
	"reason":      "git push (outward-facing / rewrites history)",
	"fingerprint": "0a28dfb603a983d592b50925e728af39",
	"argv":        `["git", "push", "origin", "main"]`,
}

var scenarios = []scenario{
	{Name: "reminders then yes", Timeout: 60, Escalation: []float64{20, 10, 10, 0, 90}, TTY: true,
		Answers: []*string{nil, nil, s("  YES \n")}, GateID: "m:tool:p1", Question: "Allow 'run_command'? git push", Context: pushContext},
	{Name: "no", Timeout: 3600, Escalation: []float64{900, 2700, 14400, 43200}, TTY: true,
		Answers: []*string{s("n\n")}, GateID: "m:tool:p2", Question: "Allow 'run_command'? git push", Context: pushContext},
	{Name: "timeout", Timeout: 30.9, Escalation: []float64{10}, TTY: true,
		Answers: []*string{nil, nil}, GateID: "m:tool:p3", Question: "q", Context: pushContext},
	{Name: "eof", Timeout: 100, TTY: true, Answers: []*string{s("")}, GateID: "m:tool:p4", Question: "q",
		Context: map[string]string{"tool": "fetch_url", "arguments": "{'url': 'https://docs.example.com/'}",
			"reason": "egress under the lethal trifecta", "fingerprint": "ff"}},
	{Name: "not a tty", Timeout: 100, TTY: false, GateID: "m:tool:p5", Question: "q", Context: pushContext},
	{Name: "quoting, secrets and unicode", Timeout: 100, TTY: true, Answers: []*string{s("y")}, GateID: "m:tool:p6",
		Question: "Allow? Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123",
		Context: map[string]string{"tool": "run_command", "arguments": "…", "reason": "upload \"é\" </x>&\x01\t",
			"fingerprint": "ab", "argv": `["curl", "-H", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123", "it's here", "", "a=b,c:d/e.f@g%h+i-j_k", "héllo", "sk-abcdefghijklmnopqrstuvwx"]`}},
}

type outcome struct {
	Out        string           `json:"out"`
	Resolution map[string]any   `json:"resolution"`
	Events     []map[string]any `json:"events"`
	Webhook    []string         `json:"webhook"`
}

func runGo(t *testing.T, sc scenario) outcome {
	t.Helper()
	answers := append([]*string{}, sc.Answers...)
	now := 0.0
	var out bytes.Buffer
	var bodies []string
	approver := NewTerminalApprover(TerminalOptions{
		TimeoutSeconds: sc.Timeout, EscalationSeconds: sc.Escalation, Out: &out,
		IsTTY: func() bool { return sc.TTY },
		ReadLine: func(_ context.Context, timeout float64) (string, bool) {
			a := answers[0]
			answers = answers[1:]
			if a == nil {
				now += timeout
				return "", false
			}
			return *a, true
		},
		Notify: func(p Payload) string { bodies = append(bodies, string(p.JSON())); return WebhookSent },
		Clock:  func() float64 { return now },
	})
	req := contracts.GateRequest{GateID: sc.GateID, Question: sc.Question, Context: sc.Context}
	res, err := approver.Request(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	events := []map[string]any{}
	for _, e := range approver.DrainEvents() {
		events = append(events, map[string]any{"kind": e.Kind, "payload": e.Payload})
	}
	if bodies == nil {
		bodies = []string{}
	}
	return outcome{Out: out.String(), Resolution: map[string]any{
		"gate_id": res.GateID, "decision": string(res.Decision), "resolved_by": res.ResolvedBy, "defaulted": res.Defaulted,
	}, Events: events, Webhook: bodies}
}

func normalize(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestTerminalApproverMatchesPython runs every scenario through Python's TerminalApprover (when
// uv is on PATH) and requires byte-identical prompts, resolutions, events and webhook bodies.
func TestTerminalApproverMatchesPython(t *testing.T) {
	var goOut []outcome
	for _, sc := range scenarios {
		goOut = append(goOut, runGo(t, sc))
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH: the Python side-by-side comparison is skipped (golden tests still run)")
	}
	_, file, _, _ := runtime.Caller(0)
	here := filepath.Dir(file)
	spec, _ := json.Marshal(scenarios)
	specPath := filepath.Join(t.TempDir(), "scenarios.json")
	if err := os.WriteFile(specPath, spec, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("uv", "run", "--quiet", "--project", filepath.Join(here, "..", "..", "..", "python"),
		"python", filepath.Join(here, "testdata", "terminal_approver.py"), specPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, stderr.String())
	}
	var pyOut []any
	if err := json.Unmarshal(raw, &pyOut); err != nil {
		t.Fatalf("python output: %v\n%s", err, raw)
	}
	for i, sc := range scenarios {
		g, p := normalize(t, goOut[i]), pyOut[i]
		if !reflect.DeepEqual(g, p) {
			gj, _ := json.MarshalIndent(g, "", " ")
			pj, _ := json.MarshalIndent(p, "", " ")
			t.Errorf("%s:\ngo: %s\npy: %s", sc.Name, gj, pj)
		}
	}
}

func TestTerminalApproverGolden(t *testing.T) {
	o := runGo(t, scenarios[0])
	want := "\n[lha] APPROVAL NEEDED - an irreversible command was flagged by the classifier\n" +
		"  tool:    run_command\n" +
		"  argv:    git push origin main\n" +
		"  reason:  git push (outward-facing / rewrites history)\n" +
		"  timeout: 60s (default: reject)\n" +
		"Allow this exact call? [y/N]: " +
		"\n[lha] reminder 1: still waiting for your answer (rejected in 50s). Allow this exact call? [y/N]: " +
		"\n[lha] reminder 2: still waiting for your answer (rejected in 40s). Allow this exact call? [y/N]: "
	if o.Out != want {
		t.Fatalf("prompt:\n%q\nwant\n%q", o.Out, want)
	}
	if o.Resolution["decision"] != "approve" || o.Resolution["resolved_by"] != "human" || len(o.Events) != 2 {
		t.Fatalf("%+v", o)
	}
	if o.Webhook[0] != `{"source":"lha","kind":"tool_call","event":"opened","gate_id":"m:tool:p1","question":"Allow 'run_command'? git push","tool":"run_command","argv":["git","push","origin","main"],"reason":"git push (outward-facing / rewrites history)","default_action":"reject"}` {
		t.Fatalf("webhook: %s", o.Webhook[0])
	}
	if last := o.Webhook[len(o.Webhook)-1]; !strings.HasSuffix(last, `"default_action":"reject","decision":"approve"}`) {
		t.Fatalf("resolved webhook: %s", last)
	}
	if o := runGo(t, scenarios[4]); o.Out != "" || o.Resolution["resolved_by"] != "non-interactive (stdin is not a TTY)" || len(o.Webhook) != 0 {
		t.Fatalf("non-tty: %+v", o)
	}
	if o := runGo(t, scenarios[2]); !strings.HasSuffix(o.Out, "\n[lha] no answer before the timeout: rejected.\n") ||
		o.Resolution["resolved_by"] != "timeout" || o.Resolution["defaulted"] != true {
		t.Fatalf("timeout: %+v", o)
	}
	secret := runGo(t, scenarios[5])
	for _, body := range secret.Webhook {
		if strings.Contains(body, "abcdefghijklmnopqrstuvwxyz0123") || strings.Contains(body, "sk-abcdefghijkl") {
			t.Fatalf("secret in webhook body: %s", body)
		}
	}
}

func TestEscalationLadder(t *testing.T) {
	if got := EscalationSchedule(60, []float64{20, 10, 10, 0, -1, 60, 90}); !reflect.DeepEqual(got, []float64{10, 20}) {
		t.Fatal(got)
	}
	sched := []float64{10, 20}
	for _, c := range []struct {
		elapsed float64
		sent    int
		want    Rung
	}{{0, 0, Rung{10, 1}}, {15, 1, Rung{5, 2}}, {25, 2, Rung{35, 0}}, {70, 2, Rung{0, 0}}, {12, 0, Rung{0, 1}}} {
		if got := NextRung(c.elapsed, 60, sched, c.sent); got != c.want {
			t.Errorf("%+v: %+v", c, got)
		}
	}
}

func TestShQuoteMatchesPython(t *testing.T) {
	for in, want := range map[string]string{"": "''", "abc": "abc", "a b": "'a b'", "it's": `'it'"'"'s'`,
		"a=b,c:d/e.f@g%h+i-j_k": "a=b,c:d/e.f@g%h+i-j_k", "héllo": "'héllo'", "$HOME": "'$HOME'"} {
		if got := ShQuote(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}

func TestLineReader(t *testing.T) {
	r, w := io.Pipe()
	lr := NewLineReader(r)
	if _, ok := lr.Read(context.Background(), 0); ok {
		t.Fatal("poll with nothing typed must time out")
	}
	if _, ok := lr.Read(context.Background(), 0.02); ok {
		t.Fatal("expected a timeout")
	}
	go func() { _, _ = w.Write([]byte("yes\npartial")); _ = w.Close() }()
	if line, ok := lr.Read(context.Background(), 5); !ok || line != "yes\n" {
		t.Fatalf("%q %v", line, ok)
	}
	if line, ok := lr.Read(context.Background(), 5); !ok || line != "partial" {
		t.Fatalf("%q %v", line, ok)
	}
	for range 2 {
		if line, ok := lr.Read(context.Background(), 5); !ok || line != "" {
			t.Fatalf("eof: %q %v", line, ok)
		}
	}
}

func TestRequestReturnsContextErrorWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	approver := NewTerminalApprover(TerminalOptions{Out: io.Discard, IsTTY: func() bool { return true },
		ReadLine: func(context.Context, float64) (string, bool) { cancel(); return "", false }})
	if _, err := approver.Request(ctx, contracts.GateRequest{Context: pushContext}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestIsTerminal(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	if IsTerminal(devnull) || IsTerminal(strings.NewReader("")) {
		t.Fatal("/dev/null and a reader are not terminals")
	}
}

func TestPostWebhook(t *testing.T) {
	var got []byte
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		contentType = r.Header.Get("Content-Type")
		switch r.URL.Path {
		case "/fail":
			w.WriteHeader(500)
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		}
	}))
	defer srv.Close()
	payload := Payload{{"source", "lha"}, {"argv", []string{"é", "<&>"}}, {"step", 2}}
	if out := PostWebhook(srv.URL+"/ok", payload, time.Second, nil); out != WebhookSent {
		t.Fatal(out)
	}
	if string(got) != `{"source":"lha","argv":["é","<&>"],"step":2}` || contentType != "application/json" {
		t.Fatalf("%s %s", got, contentType)
	}
	if out := PostWebhook(srv.URL+"/fail", payload, time.Second, nil); out != "failed: HTTP 500" {
		t.Fatal(out)
	}
	if out := PostWebhook(srv.URL+"/redirect", payload, time.Second, nil); out != "failed: HTTP 302" {
		t.Fatal(out)
	}
	if out := PostWebhook("", payload, time.Second, nil); out != WebhookOff {
		t.Fatal(out)
	}
	if out := PostWebhook("ftp://x/y", payload, time.Second, nil); out != "failed: UnsupportedProtocol" {
		t.Fatal(out)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	if out := PostWebhook(url+"/?token=s3cret", payload, time.Second, nil); out != "failed: ConnectError" {
		t.Fatal(out)
	}
}

func TestConsoleGateFromSettings(t *testing.T) {
	settings := &config.Settings{ConsoleApprovalTimeoutS: 120, GateEscalationSeconds: []int{30, 500}}
	g := ConsoleGate(settings)
	if g.timeout != 120 || !reflect.DeepEqual(g.schedule, []float64{30}) || g.notify != nil {
		t.Fatalf("%+v", g)
	}
	settings.GateWebhookURL = config.NewSecret("http://127.0.0.1:1/hook")
	settings.GateWebhookTimeoutSeconds = 0.5
	if ConsoleGate(settings).notify == nil {
		t.Fatal("webhook notifier not configured")
	}
}
