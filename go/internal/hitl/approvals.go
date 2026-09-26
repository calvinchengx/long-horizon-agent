package hitl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

// Human approval of irreversible tool calls on the terminal (python/src/lha/hitl/approvals.py:
// TerminalApprover, console_gate). The dispatcher sends every command the classifier flags to
// the gate; with no gate such commands are denied. The approver shows the exact argv and the
// classifier's reason, asks y/N (default reject), rejects without asking when stdin is not a
// TTY, and rejects on timeout after reminders on the escalation ladder. Prompt text, gate events
// and webhook payloads are byte-identical to Python's.

// ReadLine reads one answer line: (line, true) — "" means end of input — or ("", false) when
// timeoutS seconds pass with no line (python: ReadLine returning None).
type ReadLine func(ctx context.Context, timeoutS float64) (line string, ok bool)

// RequestArgv is the exact argv of a gated command call ([] for non-command tools).
func RequestArgv(req contracts.GateRequest) []string {
	raw := req.Context["argv"]
	if raw == "" {
		return []string{}
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return []string{}
	}
	list, ok := value.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, len(list))
	for i, token := range list {
		if s, isStr := token.(string); isStr {
			out[i] = s
		} else {
			b, _ := json.Marshal(token)
			out[i] = string(b)
		}
	}
	return out
}

var shUnsafe = regexp.MustCompile(`[^A-Za-z0-9_@%+=:,./-]`)

// ShQuote is Python's shlex.quote.
func ShQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !shUnsafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ShJoin is Python's shlex.join.
func ShJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = ShQuote(a)
	}
	return strings.Join(quoted, " ")
}

// Describe is the one-line human description of the call: the shell-quoted argv, else the
// arguments.
func Describe(req contracts.GateRequest) string {
	if argv := RequestArgv(req); len(argv) > 0 {
		return ShJoin(argv)
	}
	return req.Context["arguments"]
}

// TerminalOptions configures a TerminalApprover. Zero values mean the defaults.
type TerminalOptions struct {
	TimeoutSeconds    float64   // default 3600
	EscalationSeconds []float64 // reminder offsets (only those inside the timeout are used)
	Stdin             io.Reader // default os.Stdin
	Out               io.Writer // default os.Stderr
	IsTTY             func() bool
	ReadLine          ReadLine
	Notify            Notify
	Clock             func() float64 // seconds, monotonic
}

// TerminalApprover asks on the terminal: exact argv + reason, y/N (default deny), timeout =>
// deny. Stdin that is not a TTY is never read: the call is denied at once. While waiting it walks
// the escalation ladder: at each offset it prints a reminder, keeps a gate_reminder event (the
// dispatcher commits it with the checkpoint) and notifies the webhook if one is configured; at
// the timeout the call is denied. Prompts are serialized.
type TerminalApprover struct {
	timeout  float64
	schedule []float64
	out      io.Writer
	isTTY    func() bool
	readLine ReadLine
	notify   Notify
	clock    func() float64

	prompt   sync.Mutex
	eventsMu sync.Mutex
	events   []contracts.EventRecord

	storeMu       sync.Mutex
	store         GateRecorder // the run's hitl_gates (BindStore); nil = not recorded
	storeFailures int
}

var (
	_ contracts.HITLGate     = (*TerminalApprover)(nil)
	_ contracts.EventDrainer = (*TerminalApprover)(nil)
)

// NewTerminalApprover builds a TerminalApprover.
func NewTerminalApprover(o TerminalOptions) *TerminalApprover {
	timeout := o.TimeoutSeconds
	if timeout == 0 {
		timeout = 3600
	}
	stdin := o.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	a := &TerminalApprover{
		timeout: timeout, schedule: EscalationSchedule(timeout, o.EscalationSeconds),
		out: o.Out, isTTY: o.IsTTY, readLine: o.ReadLine, notify: o.Notify, clock: o.Clock,
	}
	if a.out == nil {
		a.out = os.Stderr
	}
	if a.isTTY == nil {
		a.isTTY = func() bool { return IsTerminal(stdin) }
	}
	if a.readLine == nil {
		a.readLine = NewLineReader(stdin).Read
	}
	if a.clock == nil {
		start := time.Now()
		a.clock = func() float64 { return time.Since(start).Seconds() }
	}
	return a
}

// DrainEvents returns the gate_reminder events since the last drain.
func (a *TerminalApprover) DrainEvents() []contracts.EventRecord {
	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	events := a.events
	a.events = nil
	if events == nil {
		events = []contracts.EventRecord{}
	}
	return events
}

func denied(req contracts.GateRequest, by string, defaulted bool) contracts.GateResolution {
	return contracts.GateResolution{GateID: req.GateID, Decision: contracts.GateReject, ResolvedBy: by, Defaulted: defaulted}
}

// Request implements contracts.HITLGate. It returns ctx's error if ctx ends while waiting.
func (a *TerminalApprover) Request(ctx context.Context, req contracts.GateRequest) (contracts.GateResolution, error) {
	if !a.isTTY() {
		by := "non-interactive (stdin is not a TTY)"
		a.persist(ctx, req, "defaulted", "reject", by, 0)
		return denied(req, by, true), nil
	}
	a.prompt.Lock()
	defer a.prompt.Unlock()
	return a.ask(ctx, req)
}

func (a *TerminalApprover) say(text string) {
	_, _ = io.WriteString(a.out, text)
	if f, ok := a.out.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
}

func (a *TerminalApprover) send(req contracts.GateRequest, event string, extra ...Field) {
	if a.notify == nil {
		return
	}
	argv := RequestArgv(req)
	redacted := make([]string, len(argv))
	for i, t := range argv {
		redacted[i] = obs.RedactText(t)
	}
	payload := Payload{
		{"source", "lha"},
		{"kind", "tool_call"},
		{"event", event},
		{"gate_id", req.GateID},
		{"question", obs.RedactText(req.Question)},
		{"tool", req.Context["tool"]},
		{"argv", redacted},
		{"reason", req.Context["reason"]},
		{"default_action", "reject"},
	}
	a.notify(append(payload, extra...))
}

func (a *TerminalApprover) ask(ctx context.Context, req contracts.GateRequest) (contracts.GateResolution, error) {
	label := "args"
	if len(RequestArgv(req)) > 0 {
		label = "argv"
	}
	a.say("\n[lha] APPROVAL NEEDED - an irreversible command was flagged by the classifier\n" +
		"  tool:    " + req.Context["tool"] + "\n" +
		"  " + label + ":    " + Describe(req) + "\n" +
		"  reason:  " + req.Context["reason"] + "\n" +
		fmt.Sprintf("  timeout: %ds (default: reject)\n", int(a.timeout)) +
		"Allow this exact call? [y/N]: ")
	a.send(req, "opened")
	a.persist(ctx, req, "opened", "", "", 0)
	start := a.clock()
	sent := 0
	for {
		rung := NextRung(a.clock()-start, a.timeout, a.schedule, sent)
		line, ok := a.readLine(ctx, rung.WaitSeconds)
		if err := ctx.Err(); err != nil {
			return contracts.GateResolution{}, err
		}
		if !ok {
			if rung.Step == 0 {
				a.say("\n[lha] no answer before the timeout: rejected.\n")
				a.send(req, "defaulted", Field{"decision", "reject"})
				a.persist(ctx, req, "defaulted", "reject", "timeout", 0)
				return denied(req, "timeout", true), nil
			}
			sent = rung.Step
			remaining := max(0, int(a.timeout-(a.clock()-start)))
			a.say(fmt.Sprintf("\n[lha] reminder %d: still waiting for your answer "+
				"(rejected in %ds). Allow this exact call? [y/N]: ", sent, remaining))
			a.eventsMu.Lock()
			a.events = append(a.events, contracts.EventRecord{
				Kind: "gate_reminder",
				Payload: map[string]any{
					"gate_id":     req.GateID,
					"gate":        "tool_call",
					"step":        sent,
					"tool":        req.Context["tool"],
					"fingerprint": req.Context["fingerprint"],
				},
			})
			a.eventsMu.Unlock()
			a.send(req, "reminder", Field{"step", sent})
			a.persist(ctx, req, "reminder", "", "", sent)
			continue
		}
		if line == "" {
			a.say("\n[lha] end of input: rejected.\n")
			a.send(req, "defaulted", Field{"decision", "reject"})
			a.persist(ctx, req, "defaulted", "reject", "end of input", 0)
			return denied(req, "end of input", true), nil
		}
		answer := strings.ToLower(pyStrip(line))
		approved := answer == "y" || answer == "yes"
		decision := "reject"
		if approved {
			decision = "approve"
		}
		a.send(req, "resolved", Field{"decision", decision})
		a.persist(ctx, req, "resolved", decision, "terminal:"+terminalUser(), 0)
		if approved {
			return contracts.GateResolution{GateID: req.GateID, Decision: contracts.GateApprove, ResolvedBy: "human"}, nil
		}
		return denied(req, "human", false), nil
	}
}

func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return r == ' ' || (r >= '\t' && r <= '\r') || (r >= 0x1c && r <= 0x1f) || r == 0x85 || r == 0xa0 ||
			r == 0x1680 || (r >= 0x2000 && r <= 0x200a) || r == 0x2028 || r == 0x2029 || r == 0x202f ||
			r == 0x205f || r == 0x3000
	})
}

// LineReader reads lines from a stream with a timeout (python: select_readline). A background
// goroutine, started on the first read, reads whole lines; a line typed after a timeout is the
// answer to the next question, as with Python's buffered stdin.
type LineReader struct {
	r    io.Reader
	once sync.Once
	ch   chan string
}

// NewLineReader wraps r.
func NewLineReader(r io.Reader) *LineReader { return &LineReader{r: r, ch: make(chan string)} }

func (l *LineReader) start() {
	go func() {
		br := bufio.NewReader(l.r)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				l.ch <- line
			}
			if err != nil {
				close(l.ch) // end of input: every later read returns ""
				return
			}
		}
	}()
}

// Read implements ReadLine.
func (l *LineReader) Read(ctx context.Context, timeoutS float64) (string, bool) {
	l.once.Do(l.start)
	if timeoutS <= 0 {
		select {
		case line, open := <-l.ch:
			return lineOrEOF(line, open), true
		default:
			return "", false
		}
	}
	timer := time.NewTimer(time.Duration(timeoutS * float64(time.Second)))
	defer timer.Stop()
	select {
	case line, open := <-l.ch:
		return lineOrEOF(line, open), true
	case <-timer.C:
		return "", false
	case <-ctx.Done():
		return "", false
	}
}

func lineOrEOF(line string, open bool) string {
	if !open {
		return ""
	}
	return line
}

// SettingsNotifier is the webhook notifier from settings (nil when LHA_GATE_WEBHOOK_URL is
// unset).
func SettingsNotifier(settings *config.Settings) Notify {
	if settings.GateWebhookURL == nil {
		return nil
	}
	return WebhookNotifier(settings.GateWebhookURL.Value(), settings.GateWebhookTimeoutSeconds)
}

// ConsoleGate is the terminal approver for attended local runs (--approve-interactive): anything
// but y/yes rejects; no TTY rejects without asking; no answer within
// LHA_CONSOLE_APPROVAL_TIMEOUT_S rejects, after reminders at LHA_GATE_ESCALATION_SECONDS.
func ConsoleGate(settings *config.Settings) *TerminalApprover {
	steps := make([]float64, len(settings.GateEscalationSeconds))
	for i, s := range settings.GateEscalationSeconds {
		steps[i] = float64(s)
	}
	return NewTerminalApprover(TerminalOptions{
		TimeoutSeconds:    float64(settings.ConsoleApprovalTimeoutS),
		EscalationSeconds: steps,
		Notify:            SettingsNotifier(settings),
	})
}
