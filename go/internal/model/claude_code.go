package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// Claude Code (claude -p) as a model backend (python: lha.model.claude_code).
//
// claude -p runs one headless Claude Code session and prints a JSON result: the final text, token
// usage per model, the session id and total_cost_usd. Running LHA through it means a Claude
// Pro/Max login works without an API key, with Claude Code's own model defaults.
//
// Two uses, sharing the helpers here:
//
//   - ClaudeCodeModel (LHA_MODEL_BACKEND=claude_code): one Complete is one claude -p call with
//     every built-in tool switched off (--tools ""), so it is a plain text turn. The conversation
//     is flattened into the prompt and LHA's tools are described in the system prompt, so the
//     lead replies with the JSON actions the agent prompt asks for and LHA executes them.
//   - agent.ClaudeCodeEngine (LHA_LEAD_ENGINE=claude_code): a whole lead cycle is one claude -p
//     session with LHA's tools served over MCP (agent/mcpbridge).
//
// Progress: the CLI runs with --output-format stream-json, so each model turn arrives as it
// happens. SessionProgress follows the turns, the tools called and the tokens spent so far,
// priced with the price table; a session killed at its timeout returns a
// *ClaudeCodeTimeoutError carrying that progress, so it is charged what it spent rather than its
// whole cap.
//
// Cost: the ledger records the total_cost_usd Claude Code reports (Usage.ReportedCostUSD). On a
// subscription that is the API-equivalent cost, not a bill, but the budget ceiling still applies
// to it. Before a call runs, its worst case is LHA_CLAUDE_CODE_MAX_BUDGET_USD, which is also
// passed to the CLI as --max-budget-usd.
//
// Failures: a result with is_error returns a *ClaudeCodeError; rate limits, overload and 5xx are
// marked retryable (IsRetryable), authentication and usage errors are not.

// ClaudeCodeDefaultModel is the --model value meaning "whatever Claude Code would pick" (no flag
// is passed).
const ClaudeCodeDefaultModel = "default"

// Transient API failures worth another try (rate limit, overload, server errors, timeouts).
var claudeCodeTransientMarkers = []string{"rate limit", "rate_limit", "overloaded", "529", "timeout", "timed out"}

const claudeCodeStderrTail = 2000

// Environment variables that make a child claude think it runs nested inside Claude Code.
var claudeCodeNestedEnv = []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"}

// ClaudeCodeError is claude -p failing: it could not start, exited non-zero, or reported
// is_error.
type ClaudeCodeError struct {
	Message string
	// IsRetryable marks a transient failure (IsRetryable honours it).
	IsRetryable bool
	// Subtype is the result's subtype (error_max_budget_usd, error_max_turns, ...), "" if none.
	Subtype string
	// Usage is what the failed run still spent, when it got far enough to report it.
	Usage *contracts.Usage
}

func (e *ClaudeCodeError) Error() string { return e.Message }

// Retryable reports whether the failure is transient (python: the retryable attribute).
func (e *ClaudeCodeError) Retryable() bool { return e.IsRetryable }

// ClaudeCodeTimeoutError is a claude -p run killed after its timeout (python: ClaudeCodeTimeout).
// It is retryable, like any timeout, and carries the session's progress until the kill.
type ClaudeCodeTimeoutError struct {
	Message  string
	Progress *SessionProgress
}

func (e *ClaudeCodeTimeoutError) Error() string { return e.Message }

// Retryable is always true: a timeout is transient.
func (e *ClaudeCodeTimeoutError) Retryable() bool { return true }

// ClaudeCodeResult is the parsed result of one claude -p run (the last line of its stream-json
// output).
type ClaudeCodeResult struct {
	Text       string
	Usage      contracts.Usage
	SessionID  *string
	NumTurns   int
	StopReason *string
	Raw        *pyfmt.OrderedMap
}

// pyNumber classifies a decoded JSON value as Python's isinstance(v, int | float) would (bool is
// an int in Python). isInt is false for a float.
func pyNumber(v any) (f float64, isInt, ok bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true, true
		}
		return 0, true, true
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return float64(n), true, true
			}
			g, _ := strconv.ParseFloat(s, 64)
			return g, true, true
		}
		g, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false, false
		}
		return g, false, true
	case float64:
		return x, x == math.Trunc(x), true
	case int:
		return float64(x), true, true
	}
	return 0, false, false
}

// pyInt is python's _int: int(value) for a number, else 0.
func pyInt(v any) int {
	if n, ok := v.(json.Number); ok && !strings.ContainsAny(n.String(), ".eE") {
		if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
			return int(i)
		}
	}
	f, _, ok := pyNumber(v)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return int(math.Trunc(f))
}

// ccTruthy is Python's bool(value) for a decoded JSON value.
func ccTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case *pyfmt.OrderedMap:
		return len(x.Keys) > 0
	}
	if f, _, ok := pyNumber(v); ok {
		return f != 0
	}
	return true
}

func omGet(m *pyfmt.OrderedMap, key string) any {
	if m == nil {
		return nil
	}
	return m.Values[key]
}

// servingModel is the model that produced most of the output (Claude Code may also use a small
// model); the first one wins a tie, as Python's max does.
func servingModel(modelUsage any, fallback string) string {
	m, ok := modelUsage.(*pyfmt.OrderedMap)
	if !ok || len(m.Keys) == 0 {
		return fallback
	}
	best, bestOut := m.Keys[0], -1
	for _, k := range m.Keys {
		out := 0
		if per, ok := m.Values[k].(*pyfmt.OrderedMap); ok {
			out = pyInt(omGet(per, "outputTokens"))
		}
		if bestOut == -1 || out > bestOut {
			best, bestOut = k, out
		}
	}
	return best
}

func claudeCodeTransient(data *pyfmt.OrderedMap, text string) bool {
	// Python: isinstance(status, int), so a JSON float such as 429.0 does not count.
	if status, isInt, ok := pyNumber(omGet(data, "api_error_status")); ok && isInt {
		if status == 408 || status == 409 || status == 429 || status >= 500 {
			return true
		}
	}
	lowered := strings.ToLower(text)
	for _, marker := range claudeCodeTransientMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// ParseClaudeCodeResult parses a claude -p result object (its last stream-json line); an error result
// returns a *ClaudeCodeError.
func ParseClaudeCodeResult(stdout, provider, fallbackModel string) (ClaudeCodeResult, error) {
	decoded, err := pyfmt.DecodeOrdered([]byte(stdout))
	if err != nil {
		return ClaudeCodeResult{}, &ClaudeCodeError{Message: "claude -p printed no JSON result: " + contracts.PyRepr(pyfmt.Tail(stdout, 500))}
	}
	data, ok := decoded.(*pyfmt.OrderedMap)
	if !ok {
		return ClaudeCodeResult{}, &ClaudeCodeError{Message: "claude -p printed an unexpected result: " + contracts.PyRepr(pyfmt.Tail(stdout, 500))}
	}
	text, _ := omGet(data, "result").(string)
	usage := claudeCodeUsage(data, provider, fallbackModel)
	subtype, hasSubtype := data.Values["subtype"]
	subtypeStr, subtypeIsStr := subtype.(string)
	isSuccess := !hasSubtype || subtype == nil || (subtypeIsStr && subtypeStr == "success")
	if ccTruthy(omGet(data, "is_error")) || !isSuccess {
		reason := text
		if reason == "" {
			if ccTruthy(subtype) {
				reason = pyfmt.PyStr(subtype)
			} else {
				reason = "error"
			}
		}
		e := &ClaudeCodeError{
			Message:     "claude -p failed: " + pyfmt.Head(reason, 500),
			IsRetryable: claudeCodeTransient(data, reason),
			Usage:       &usage,
		}
		if subtypeIsStr {
			e.Subtype = subtypeStr
		}
		return ClaudeCodeResult{}, e
	}
	r := ClaudeCodeResult{Text: text, Usage: usage, NumTurns: pyInt(omGet(data, "num_turns")), Raw: data}
	if s, ok := omGet(data, "session_id").(string); ok {
		r.SessionID = &s
	}
	if s, ok := omGet(data, "stop_reason").(string); ok {
		r.StopReason = &s
	}
	return r, nil
}

func claudeCodeUsage(data *pyfmt.OrderedMap, provider, fallbackModel string) contracts.Usage {
	usage, _ := omGet(data, "usage").(*pyfmt.OrderedMap)
	u := contracts.Usage{
		InputTokens:              pyInt(omGet(usage, "input_tokens")),
		OutputTokens:             pyInt(omGet(usage, "output_tokens")),
		CacheReadInputTokens:     pyInt(omGet(usage, "cache_read_input_tokens")),
		CacheCreationInputTokens: pyInt(omGet(usage, "cache_creation_input_tokens")),
		Model:                    servingModel(omGet(data, "modelUsage"), fallbackModel),
		Provider:                 provider,
	}
	if breakdown, ok := omGet(usage, "cache_creation").(*pyfmt.OrderedMap); ok {
		u.CacheCreation1hInputTokens = pyInt(omGet(breakdown, "ephemeral_1h_input_tokens"))
	}
	if cost, _, ok := pyNumber(omGet(data, "total_cost_usd")); ok {
		u.ReportedCostUSD = &cost
	}
	return u
}

// ClaudeCodeChildEnv is the environment for a child claude: ours, minus the nested-session
// markers, plus extra.
func ClaudeCodeChildEnv(extra map[string]string) []string {
	env := []string{}
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		nested := false
		for _, n := range claudeCodeNestedEnv {
			if key == n {
				nested = true
			}
		}
		if _, overridden := extra[key]; !nested && !overridden {
			env = append(env, kv)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// MinCallBudgetUSD is the least budget a claude -p call is started with; below it the call keeps its
// configured cap, so the governor refuses it rather than starting one that can do nothing.
const MinCallBudgetUSD = 0.01

// CallBudgetUSD is a claude -p call's spend cap: configured, or what is left of the budget if less
// (python: call_budget_usd). The cap is both --max-budget-usd and the worst case the governor
// reserves, so a call never reserves more than the mission can still spend.
func CallBudgetUSD(configured, remaining float64) float64 {
	if remaining < MinCallBudgetUSD {
		return configured
	}
	return math.Min(configured, remaining)
}

// ClaudeCodeBaseArgs are the flags every LHA claude -p call shares: streamed JSON, no saved
// session, a spend cap.
func ClaudeCodeBaseArgs(model string, maxBudgetUSD float64) []string {
	args := []string{
		"-p",
		"--output-format",
		"stream-json",
		"--verbose", // stream-json requires it with -p
		"--no-session-persistence",
		"--disable-slash-commands",
		"--max-budget-usd",
		fmt.Sprintf("%.4f", maxBudgetUSD),
	}
	if model != "" && model != ClaudeCodeDefaultModel {
		args = append(args, "--model", model)
	}
	return args
}

// ClaudeCodeRun is one claude -p invocation (RunClaude's inputs).
type ClaudeCodeRun struct {
	Args          []string
	Prompt        string // sent on stdin, never argv, so its size is not limited by the OS
	Binary        string
	Cwd           string // "" = this process's working directory
	TimeoutS      float64
	Provider      string
	FallbackModel string
	// OnProgress is called with the session's progress whenever a turn begins or a tool is called.
	OnProgress func(*SessionProgress)
}

// claudeStreamLineLimit is the longest stream-json line read (a tool result can be large).
const claudeStreamLineLimit = 64 << 20

// pyOSErrorText renders a failed exec as Python's OSError str() ("[Errno 2] No such file or
// directory: 'claude'").
func pyOSErrorText(binary string, err error) string {
	var errno syscall.Errno
	switch {
	case errors.As(err, &errno):
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		errno = syscall.ENOENT
	case errors.Is(err, fs.ErrPermission):
		errno = syscall.EACCES
	default:
		return err.Error()
	}
	msg := errno.Error()
	if msg != "" {
		msg = strings.ToUpper(msg[:1]) + msg[1:]
	}
	return fmt.Sprintf("[Errno %d] %s: %s", int(errno), msg, contracts.PyRepr(binary))
}

// RunClaude runs Binary Args with Prompt on stdin and parses its streamed result. OnProgress sees
// each turn and tool call as it happens. A run past TimeoutS is killed and returns a
// *ClaudeCodeTimeoutError (retryable) carrying its progress.
func RunClaude(ctx context.Context, r ClaudeCodeRun) (ClaudeCodeResult, error) {
	cmd := exec.Command(r.Binary, r.Args...)
	cmd.Dir = r.Cwd
	cmd.Env = ClaudeCodeChildEnv(nil)
	cmd.Stdin = strings.NewReader(r.Prompt)
	stream := &claudeStream{progress: &SessionProgress{}, onProgress: r.OnProgress}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stream, &stderr
	// A grandchild holding the pipes open must not keep us waiting after the kill.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return ClaudeCodeResult{}, &ClaudeCodeError{Message: fmt.Sprintf(
			"cannot run %s: %s. Install Claude Code or set LHA_CLAUDE_CODE_BIN.",
			contracts.PyRepr(r.Binary), pyOSErrorText(r.Binary, err))}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(time.Duration(r.TimeoutS * float64(time.Second)))
	defer timer.Stop()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-done
		stream.mu.Lock()
		defer stream.mu.Unlock()
		return ClaudeCodeResult{}, &ClaudeCodeTimeoutError{
			Message:  fmt.Sprintf("claude -p did not finish within %.0fs", r.TimeoutS),
			Progress: stream.progress,
		}
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return ClaudeCodeResult{}, ctx.Err()
	}
	stream.flush()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code = exitErr.ExitCode()
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = -int(ws.Signal()) // Python's returncode for a signal
		}
	} else if waitErr != nil && !errors.Is(waitErr, exec.ErrWaitDelay) {
		return ClaudeCodeResult{}, &ClaudeCodeError{Message: fmt.Sprintf("claude -p failed: %v", waitErr)}
	}
	if stream.result == "" {
		errText := pyfmt.Tail(strings.ToValidUTF8(stderr.String(), "\uFFFD"), claudeCodeStderrTail)
		if errText == "" {
			errText = stream.other
		}
		return ClaudeCodeResult{}, &ClaudeCodeError{Message: fmt.Sprintf("claude -p exited %d: %s", code, errText)}
	}
	return ParseClaudeCodeResult(stream.result, r.Provider, r.FallbackModel)
}

// claudeStream is claude -p's stdout: it splits stream-json lines as they arrive, follows the
// session's progress and keeps the result line.
type claudeStream struct {
	mu         sync.Mutex
	pending    []byte
	skipping   bool // inside a line longer than claudeStreamLineLimit
	progress   *SessionProgress
	onProgress func(*SessionProgress)
	result     string // the "result" event's line
	other      string // the last output that was not a stream-json event
}

func (c *claudeStream) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rest := b
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			if !c.skipping {
				c.pending = append(c.pending, rest...)
				if len(c.pending) > claudeStreamLineLimit {
					c.pending, c.skipping = nil, true
				}
			}
			break
		}
		if !c.skipping {
			c.line(append(c.pending, rest[:i]...))
		}
		c.pending, c.skipping = nil, false
		rest = rest[i+1:]
	}
	return len(b), nil
}

// flush handles a last line without a newline.
func (c *claudeStream) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) > 0 && !c.skipping {
		c.line(c.pending)
	}
	c.pending = nil
}

func (c *claudeStream) line(raw []byte) {
	line := pyfmt.PyStrip(strings.ToValidUTF8(string(raw), "\uFFFD"))
	if line == "" {
		return
	}
	decoded, err := pyfmt.DecodeOrdered([]byte(line))
	event, ok := decoded.(*pyfmt.OrderedMap)
	if err != nil || !ok {
		c.other = pyfmt.Tail(line, 500)
		return
	}
	if omGet(event, "type") == "result" {
		c.result = line
		return
	}
	if c.progress.Observe(event) && c.onProgress != nil {
		defer func() { // observing a session must never fail it
			if p := recover(); p != nil {
				slog.Default().Warn("session_progress_failed", "error", fmt.Sprint(p))
			}
		}()
		c.onProgress(c.progress)
	}
}

// RenderTranscript is the non-system messages as one prompt, ending with a request for the next
// reply.
func RenderTranscript(messages []contracts.ModelMessage) string {
	parts := []string{}
	for _, m := range messages {
		switch m.Role {
		case "system":
			continue
		case "assistant":
			body := m.Content
			for _, call := range m.ToolCalls {
				name, _ := pyJSONDumps(call.Name)
				args, _ := pyJSONDumps(call.Arguments)
				action := `{"tool": ` + name + `, "arguments": ` + args + `}`
				body = pyfmt.PyStrip(body + "\n" + action)
			}
			parts = append(parts, "<assistant>\n"+body+"\n</assistant>")
		case "tool":
			id := ""
			if m.ToolCallID != nil {
				id = *m.ToolCallID
			}
			parts = append(parts, "<tool_result id="+contracts.PyRepr(id)+">\n"+m.Content+"\n</tool_result>")
		default:
			parts = append(parts, "<user>\n"+m.Content+"\n</user>")
		}
	}
	if len(parts) == 1 && strings.HasPrefix(parts[0], "<user>") {
		return messages[len(messages)-1].Content // a single user turn needs no transcript framing
	}
	parts = append(parts, "Write the assistant's next reply only.")
	return strings.Join(parts, "\n\n")
}

// ClaudeCodeOptions configures a ClaudeCodeModel. Zero values take the Python defaults.
type ClaudeCodeOptions struct {
	ModelName    string  // "" => ClaudeCodeDefaultModel
	Binary       string  // "" => "claude"
	Cwd          string  // "" => this process's working directory
	MaxBudgetUSD float64 // <= 0 => 5.0
	TimeoutS     float64 // <= 0 => 3600
	Price        *ModelPrice
	MaxRetries   *int // nil => 3
	// RetryBaseDelayS is the backoff base (nil => 2.0).
	RetryBaseDelayS *float64
	Sleep           SleepFunc
}

// ClaudeCodeModel is a ModelProvider that runs each turn as one tool-less claude -p call.
type ClaudeCodeModel struct {
	name         string
	model        string
	binary       string
	cwd          string
	maxBudgetUSD float64
	timeoutS     float64
	price        *ModelPrice
	retry        RetryPolicy
}

var _ contracts.ModelProvider = (*ClaudeCodeModel)(nil)

// NewClaudeCode builds a ClaudeCodeModel.
func NewClaudeCode(o ClaudeCodeOptions) *ClaudeCodeModel {
	m := &ClaudeCodeModel{
		model: o.ModelName, binary: o.Binary, cwd: o.Cwd, maxBudgetUSD: o.MaxBudgetUSD,
		timeoutS: o.TimeoutS, price: o.Price, retry: DefaultRetryPolicy(),
	}
	if m.model == "" {
		m.model = ClaudeCodeDefaultModel
	}
	if m.binary == "" {
		m.binary = "claude"
	}
	if m.maxBudgetUSD <= 0 {
		m.maxBudgetUSD = 5.0
	}
	if m.timeoutS <= 0 {
		m.timeoutS = 3600.0
	}
	if o.MaxRetries != nil {
		m.retry.MaxRetries = *o.MaxRetries
	}
	m.retry.BaseDelaySeconds = 2.0
	if o.RetryBaseDelayS != nil {
		m.retry.BaseDelaySeconds = *o.RetryBaseDelayS
	}
	m.retry.Sleep = o.Sleep
	m.name = "claude_code:" + m.model
	return m
}

// Name is "claude_code:<model>".
func (m *ClaudeCodeModel) Name() string { return m.name }

// Complete runs one claude -p turn. tools is ignored: native tool calling needs MCP (the
// claude_code lead engine); here the tools are described in the system prompt and the reply is a
// JSON action. maxTokens is ignored too (claude -p has no such flag).
func (m *ClaudeCodeModel) Complete(ctx context.Context, messages []contracts.ModelMessage, _ []map[string]any, _ int) (contracts.TurnResult, error) {
	systems := []string{}
	for _, msg := range messages {
		if msg.Role == "system" {
			systems = append(systems, msg.Content)
		}
	}
	system := strings.Join(systems, "\n\n")
	args := append(ClaudeCodeBaseArgs(m.model, m.maxBudgetUSD), "--tools", "", "--strict-mcp-config")
	if system != "" {
		args = append(args, "--system-prompt", system)
	}
	run := ClaudeCodeRun{
		Args: args, Prompt: RenderTranscript(messages), Binary: m.binary, Cwd: m.cwd,
		TimeoutS: m.timeoutS, Provider: m.name, FallbackModel: m.model,
	}
	result, err := WithRetries(ctx, m.retry, func(ctx context.Context) (ClaudeCodeResult, error) {
		return RunClaude(ctx, run)
	})
	if err != nil {
		return contracts.TurnResult{}, err
	}
	return contracts.TurnResult{
		Text: result.Text, Usage: result.Usage, StopReason: result.StopReason, SessionID: result.SessionID,
	}, nil
}

// BudgetCapped is this model with its per-call cap lowered to what is left of the budget (see
// CallBudgetUSD); the metered wrapper asks for it before each call (python: budget_capped).
func (m *ClaudeCodeModel) BudgetCapped(remainingUSD float64) contracts.ModelProvider {
	capped := *m
	capped.maxBudgetUSD = CallBudgetUSD(m.maxBudgetUSD, remainingUSD)
	return &capped
}

// EstimateCostUSD is the cost Claude Code reported; before a call, its worst case: the token
// price when the model is in the price table (or configured), capped by --max-budget-usd, and
// otherwise that cap. It never returns an unknown-price error.
func (m *ClaudeCodeModel) EstimateCostUSD(usage contracts.Usage) (float64, error) {
	if usage.ReportedCostUSD != nil {
		return *usage.ReportedCostUSD, nil
	}
	price := m.price
	if price == nil {
		model := usage.Model
		if model == "" {
			model = m.model
		}
		price = LookupClaudePrice(model)
	}
	if price == nil {
		return m.maxBudgetUSD, nil
	}
	return math.Min(price.Cost(usage), m.maxBudgetUSD), nil
}

// HealthCheck runs claude --version, then claude auth status, without spending tokens (python:
// ClaudeCodeModel.health_check). A parked mission resumes only when this is healthy, so a
// logged-out CLI must be DOWN. With ANTHROPIC_API_KEY set the CLI authenticates with the key and the
// login is not checked; an auth status that is not JSON (older CLIs) is judged by --version alone.
func (m *ClaudeCodeModel) HealthCheck(ctx context.Context, timeoutS float64) (ok bool, detail string) {
	if _, err := exec.LookPath(m.binary); err != nil {
		return false, fmt.Sprintf("%s: %s not found on PATH", m.name, contracts.PyRepr(m.binary))
	}
	out, timedOut, err := runClaudeCLI(ctx, m.binary, []string{"--version"}, timeoutS)
	if timedOut {
		return false, m.name + ": claude --version timed out"
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, fmt.Sprintf("%s: claude --version exited %d", m.name, exitErr.ExitCode())
		}
		return false, fmt.Sprintf("%s: %v", m.name, err)
	}
	version := pyfmt.PyStrip(strings.ToValidUTF8(string(out), "�"))
	for _, kv := range ClaudeCodeChildEnv(nil) {
		if key, value, _ := strings.Cut(kv, "="); key == "ANTHROPIC_API_KEY" && value != "" {
			return true, m.name + ": " + version + " (API key)"
		}
	}
	status, timedOut, _ := runClaudeCLI(ctx, m.binary, []string{"auth", "status"}, timeoutS)
	var parsed map[string]any
	if !timedOut && json.Unmarshal(status, &parsed) == nil {
		if loggedIn, isBool := parsed["loggedIn"].(bool); isBool && !loggedIn {
			return false, m.name + ": not logged in; run `claude auth login` (or set ANTHROPIC_API_KEY)"
		}
	}
	return true, m.name + ": " + version
}

// runClaudeCLI runs a short, prompt-less claude command and returns its combined output.
func runClaudeCLI(ctx context.Context, binary string, args []string, timeoutS float64) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutS*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = ClaudeCodeChildEnv(nil)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return out, ctx.Err() == context.DeadlineExceeded, err
}

// DefaultSettingsModelName is LHA_MODEL_NAME's default (the stub's "stub-1"; python:
// Settings.model_fields["model_name"].default). A claude_code model left at it is Claude Code's
// own choice.
func DefaultSettingsModelName() string {
	f, _ := reflect.TypeOf(config.Settings{}).FieldByName("ModelName")
	return f.Tag.Get("default")
}
