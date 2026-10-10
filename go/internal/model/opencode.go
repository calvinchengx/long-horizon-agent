package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// OpenCode (opencode run) as a model backend and lead engine (python: lha.model.opencode).
//
// opencode run executes one headless OpenCode session and streams newline-delimited JSON events
// (--format json): each model step (step_start / step_finish), every tool call (tool_use) and the
// assistant text (text). step_finish carries the step's token usage and its cost in USD, so spend
// is read from what OpenCode itself reports.
//
// Two uses, sharing the helpers here:
//
//   - OpenCodeModel (LHA_MODEL_BACKEND=opencode): one Complete is one opencode run call with an
//     agent whose every tool is denied, so it is a plain text turn. The conversation is flattened
//     into the prompt and LHA's tools are described in the system prompt, so the lead replies with
//     the JSON actions the agent prompt asks for and LHA executes them.
//   - agent.OpenCodeEngine (LHA_LEAD_ENGINE=opencode): a whole lead cycle is one opencode run
//     session with LHA's tools served over MCP (agent/mcpbridge).
//
// Cost: the ledger records the session's cost (Usage.ReportedCostUSD). The --format json stream
// reports usage only for steps that end in a tool call, so the final assistant turn is missing; the
// exact totals come from opencode session export (SessionCost), falling back to the streamed step
// sum. Before a call runs, its worst case is LHA_OPENCODE_MAX_BUDGET_USD, or what is left of the
// budget when that is less (CallBudgetUSD). OpenCode has no spend-cap flag, so the engine kills a
// session that reaches its cap while streaming; whatever it left in the workdir is still verified.

// OpenCodeDefaultModel is the --model value meaning "whatever OpenCode would pick" (no flag is
// passed).
const OpenCodeDefaultModel = ""

// OpenCodeAgentDefault is OpenCode's default agent (the lead engine's LHA_OPENCODE_AGENT).
const OpenCodeAgentDefault = "lha"

// openCodeModelAgentDefault is the agent a tool-less model turn uses (python: "lha-model").
const openCodeModelAgentDefault = "lha-model"

// openCodeStreamLineLimit is the longest stream-json line read (a tool result can be large).
const openCodeStreamLineLimit = 64 << 20

const openCodeStderrTail = 2000

// openCodeSessionExportTimeoutS is the longest `opencode session export` may take when reading a
// finished session's totals (python: _SESSION_EXPORT_TIMEOUT_S).
const openCodeSessionExportTimeoutS = 30.0

// openCodeTransientMarkers are transient failures worth another try (rate limit, overload,
// server errors, timeouts).
var openCodeTransientMarkers = []string{"rate limit", "rate_limit", "overloaded", "529", "timeout", "timed out"}

// openCodeNestedEnvPrefix marks the environment variables a child opencode would use to attach to
// the parent's session or config; every OPENCODE_* name is dropped.
const openCodeNestedEnvPrefix = "OPENCODE_"

// OpenCodeError is opencode run failing: it could not start, exited non-zero, or streamed an error.
type OpenCodeError struct {
	Message string
	// IsRetryable marks a transient failure (Retryable honours it).
	IsRetryable bool
	// Subtype is a short reason tag (error_max_budget_usd when LHA killed the session at its cap).
	Subtype string
	// Usage is what the failed run still spent, when it got far enough to report it.
	Usage *contracts.Usage
}

func (e *OpenCodeError) Error() string { return e.Message }

// Retryable reports whether the failure is transient (python: the retryable attribute).
func (e *OpenCodeError) Retryable() bool { return e.IsRetryable }

// OpenCodeTimeoutError is an opencode run session killed after its timeout (python:
// OpenCodeTimeout). It is retryable, like any timeout, and carries the session's progress until
// the kill.
type OpenCodeTimeoutError struct {
	Message  string
	Progress *OpenCodeSessionProgress
}

func (e *OpenCodeTimeoutError) Error() string { return e.Message }

// Retryable is always true: a timeout is transient.
func (e *OpenCodeTimeoutError) Retryable() bool { return true }

// OpenCodeResult is the parsed result of one opencode run session.
type OpenCodeResult struct {
	Text       string
	Usage      contracts.Usage
	SessionID  *string
	NumTurns   int
	StopReason *string
	Raw        []*pyfmt.OrderedMap
}

// OpenCodeChildEnv is the environment for a child opencode: ours, minus every OPENCODE_* marker,
// plus extra. Dropping them keeps a session started by LHA from attaching to (or inheriting the
// config of) the OpenCode that may be running LHA itself; the caller adds back only the ones it
// wants (python: child_env).
func OpenCodeChildEnv(extra map[string]string) []string {
	env := []string{}
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, openCodeNestedEnvPrefix) {
			continue
		}
		if _, overridden := extra[key]; overridden {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// OpenCodeBaseArgs are the flags every LHA opencode run call shares: streamed JSON, an agent,
// auto-approval, and (when standalone) a private server. The --model flag is passed only for a
// concrete model (python: base_args).
func OpenCodeBaseArgs(model, agent string, standalone bool) []string {
	args := []string{"run", "--format", "json", "--auto", "--agent", agent}
	if standalone {
		args = append(args, "--standalone")
	}
	if model != "" && model != OpenCodeDefaultModel {
		args = append(args, "--model", model)
	}
	return args
}

// OpenCodeRun is one opencode run invocation (RunOpenCode's inputs).
type OpenCodeRun struct {
	Args          []string
	Prompt        string // sent on stdin, never argv, so its size is not limited by the OS
	Binary        string
	Cwd           string // "" = this process's working directory
	TimeoutS      float64
	Provider      string
	FallbackModel string
	// Env is added back on top of the child environment (OPENCODE_CONFIG, ...).
	Env map[string]string
	// MaxCostUSD, when set, is the session's spend cap: a step that reaches it kills the session.
	MaxCostUSD *float64
	// OnProgress is called with the session's progress whenever a turn begins or a tool is called.
	OnProgress func(*OpenCodeSessionProgress)
}

// openCodeTransient reports whether a streamed error text is worth another try.
func openCodeTransient(text string) bool {
	lowered := strings.ToLower(text)
	for _, marker := range openCodeTransientMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// RunOpenCode runs Binary Args with Prompt on stdin and parses its JSON event stream. OnProgress
// sees each turn and tool call as it happens. A run past TimeoutS is killed and returns a
// *OpenCodeTimeoutError (retryable) carrying its progress; a run past MaxCostUSD (its reported
// spend) is killed and returns a *OpenCodeError with subtype error_max_budget_usd.
func RunOpenCode(ctx context.Context, r OpenCodeRun) (OpenCodeResult, error) {
	cmd := exec.Command(r.Binary, r.Args...)
	cmd.Dir = r.Cwd
	cmd.Env = OpenCodeChildEnv(r.Env)
	cmd.Stdin = strings.NewReader(r.Prompt)
	stream := &openCodeStream{
		progress: &OpenCodeSessionProgress{}, onProgress: r.OnProgress,
		maxCostUSD: r.MaxCostUSD, cmd: cmd,
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stream, &stderr
	// A grandchild holding the pipes open must not keep us waiting after the kill.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return OpenCodeResult{}, &OpenCodeError{Message: fmt.Sprintf(
			"cannot run %s: %s. Install OpenCode or set LHA_OPENCODE_BIN.",
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
		return OpenCodeResult{}, &OpenCodeTimeoutError{
			Message:  fmt.Sprintf("opencode run did not finish within %.0fs", r.TimeoutS),
			Progress: stream.progress,
		}
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return OpenCodeResult{}, ctx.Err()
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
		return OpenCodeResult{}, &OpenCodeError{Message: fmt.Sprintf("opencode run failed: %v", waitErr)}
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.overBudget {
		capped := 0.0
		if r.MaxCostUSD != nil {
			capped = *r.MaxCostUSD
		}
		usage := stream.progress.Usage(r.Provider, r.FallbackModel)
		return OpenCodeResult{}, &OpenCodeError{
			Message: fmt.Sprintf("opencode run failed: error_max_budget_usd (reached its $%.4f cap)", capped),
			Subtype: "error_max_budget_usd",
			Usage:   &usage,
		}
	}
	if len(stream.raw) == 0 {
		errText := pyfmt.Tail(strings.ToValidUTF8(stderr.String(), "\uFFFD"), openCodeStderrTail)
		if errText == "" {
			errText = stream.other
		}
		if errText == "" {
			errText = "no output"
		}
		return OpenCodeResult{}, &OpenCodeError{Message: fmt.Sprintf("opencode run exited %d: %s", code, errText)}
	}
	if stream.errorLine != "" {
		return OpenCodeResult{}, &OpenCodeError{
			Message: "opencode run failed: " + stream.errorLine, IsRetryable: openCodeTransient(stream.errorLine),
		}
	}
	var sessionID *string
	if stream.progress.SessionID != "" {
		s := stream.progress.SessionID
		sessionID = &s
	}
	// The stream omits the final assistant turn's usage, so read the session's exact totals.
	usage := stream.progress.Usage(r.Provider, r.FallbackModel)
	if stream.progress.SessionID != "" {
		if cost, tokens, ok := SessionCost(ctx, r.Binary, stream.progress.SessionID, openCodeSessionExportTimeoutS); ok {
			usage = exportedUsage(cost, tokens, r.Provider, r.FallbackModel)
		}
	}
	return OpenCodeResult{
		Text:       strings.Join(stream.texts, "\n"),
		Usage:      usage,
		SessionID:  sessionID,
		NumTurns:   stream.progress.Turns,
		StopReason: stream.stopReason,
		Raw:        stream.raw,
	}, nil
}

// SessionCost reads the exact cost and token totals of a finished session from
// `opencode session export` (python: session_cost). The --format json stream reports usage only on
// the step_finish of a step that ended in a tool call, so a session's final assistant turn is
// missing from the streamed sum; the export carries the session's totals. ok is false when the
// export cannot be read (no such session, a non-zero exit, timed out, or no cost field).
func SessionCost(ctx context.Context, binary, sessionID string, timeoutS float64) (cost float64, tokens *pyfmt.OrderedMap, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutS*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "session", "export", sessionID)
	cmd.Env = OpenCodeChildEnv(nil)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return 0, nil, false
	}
	decoded, err := pyfmt.DecodeOrdered(out)
	if err != nil {
		return 0, nil, false
	}
	data, ok := decoded.(*pyfmt.OrderedMap)
	if !ok {
		return 0, nil, false
	}
	info, ok := omGet(data, "info").(*pyfmt.OrderedMap)
	if !ok {
		return 0, nil, false
	}
	// python: isinstance(cost, int | float) and not isinstance(cost, bool)
	costValue := omGet(info, "cost")
	if _, isBool := costValue.(bool); isBool {
		return 0, nil, false
	}
	c, _, numeric := pyNumber(costValue)
	if !numeric {
		return 0, nil, false
	}
	tokens, _ = omGet(info, "tokens").(*pyfmt.OrderedMap)
	return c, tokens, true
}

// exportedUsage is an exported session's totals as one Usage (reasoning counts as billed output;
// python: exported_usage).
func exportedUsage(cost float64, tokens *pyfmt.OrderedMap, provider, fallbackModel string) contracts.Usage {
	cache, _ := omGet(tokens, "cache").(*pyfmt.OrderedMap)
	rounded := math.Round(cost*1e6) / 1e6
	return contracts.Usage{
		InputTokens:              pyInt(omGet(tokens, "input")),
		OutputTokens:             pyInt(omGet(tokens, "output")) + pyInt(omGet(tokens, "reasoning")),
		CacheReadInputTokens:     pyInt(omGet(cache, "read")),
		CacheCreationInputTokens: pyInt(omGet(cache, "write")),
		Model:                    fallbackModel,
		Provider:                 provider,
		ReportedCostUSD:          &rounded,
	}
}

// openCodeStream is opencode run's stdout: it splits event lines as they arrive, follows the
// session's progress and enforces the spend cap.
type openCodeStream struct {
	mu         sync.Mutex
	pending    []byte
	skipping   bool // inside a line longer than openCodeStreamLineLimit
	progress   *OpenCodeSessionProgress
	onProgress func(*OpenCodeSessionProgress)
	maxCostUSD *float64
	// cmd is the running process, so a step that reaches the cap can kill it. It is set before
	// Start, so the copy goroutine that calls Write sees a started process.
	cmd        *exec.Cmd
	overBudget bool
	texts      []string
	stopReason *string
	errorLine  string
	other      string // the last output that was not a JSON event
	raw        []*pyfmt.OrderedMap
}

func (c *openCodeStream) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rest := b
	for len(rest) > 0 {
		if c.overBudget { // the session was killed: drop whatever is still in the pipe
			break
		}
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			if !c.skipping {
				c.pending = append(c.pending, rest...)
				if len(c.pending) > openCodeStreamLineLimit {
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
func (c *openCodeStream) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.overBudget { // the session was killed at its cap: drop a partial last line
		c.pending = nil
		return
	}
	if len(c.pending) > 0 && !c.skipping {
		c.line(c.pending)
	}
	c.pending = nil
}

func (c *openCodeStream) line(raw []byte) {
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
	c.raw = append(c.raw, event)
	part, _ := omGet(event, "part").(*pyfmt.OrderedMap)
	switch omGet(event, "type") {
	case "text":
		if text, ok := omGet(part, "text").(string); ok {
			c.texts = append(c.texts, text)
		}
	case "error":
		value := omGet(event, "error")
		if !ccTruthy(value) { // python: event.get("error") or event
			value = event
		}
		if dumped, err := pyJSONDumps(value); err == nil {
			c.errorLine = pyfmt.Tail(dumped, 500)
		}
	case "step_finish":
		if reason, ok := omGet(part, "reason").(string); ok && reason != "" {
			c.stopReason = &reason
		}
	}
	changed := c.progress.Observe(event)
	if changed && c.onProgress != nil {
		func() { // observing a session must never fail it
			defer func() {
				if p := recover(); p != nil {
					slog.Default().Warn("session_progress_failed", "error", fmt.Sprint(p))
				}
			}()
			c.onProgress(c.progress)
		}()
	}
	if c.maxCostUSD != nil {
		if spent, ok := c.progress.SpentUSD(); ok && spent >= *c.maxCostUSD {
			c.overBudget = true
			if c.cmd != nil && c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
		}
	}
}

// OpenCodeOptions configures an OpenCodeModel. Zero values take the Python defaults; Standalone
// nil means true (OpenCode runs against a private server).
type OpenCodeOptions struct {
	ModelName    string  // "" => OpenCodeDefaultModel
	Binary       string  // "" => "opencode"
	Agent        string  // "" => "lha-model"
	Cwd          string  // "" => this process's working directory
	Standalone   *bool   // nil => true
	MaxBudgetUSD float64 // <= 0 => 5.0
	TimeoutS     float64 // <= 0 => 3600
	Price        *ModelPrice
	MaxRetries   *int // nil => 3
	// RetryBaseDelayS is the backoff base (nil => 2.0).
	RetryBaseDelayS *float64
	Sleep           SleepFunc
}

// OpenCodeModel is a ModelProvider that runs each turn as one tool-less opencode run call.
type OpenCodeModel struct {
	name         string
	model        string
	binary       string
	agent        string
	cwd          string
	standalone   bool
	maxBudgetUSD float64
	timeoutS     float64
	price        *ModelPrice
	retry        RetryPolicy
}

var _ contracts.ModelProvider = (*OpenCodeModel)(nil)

// NewOpenCode builds an OpenCodeModel.
func NewOpenCode(o OpenCodeOptions) *OpenCodeModel {
	standalone := true
	if o.Standalone != nil {
		standalone = *o.Standalone
	}
	m := &OpenCodeModel{
		model: o.ModelName, binary: o.Binary, agent: o.Agent, cwd: o.Cwd, standalone: standalone,
		maxBudgetUSD: o.MaxBudgetUSD, timeoutS: o.TimeoutS, price: o.Price, retry: DefaultRetryPolicy(),
	}
	if m.binary == "" {
		m.binary = "opencode"
	}
	if m.agent == "" {
		m.agent = openCodeModelAgentDefault
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
	name := m.model
	if name == "" {
		name = "default"
	}
	m.name = "opencode:" + name
	return m
}

// Name is "opencode:<model>".
func (m *OpenCodeModel) Name() string { return m.name }

// turnConfig is an agent whose every tool is denied: a plain text turn, no MCP server (python:
// OpenCodeModel._config).
func (m *OpenCodeModel) turnConfig(system string) map[string]any {
	return map[string]any{
		"agents": map[string]any{
			m.agent: map[string]any{
				"description": "LHA model turn (no tools)",
				"mode":        "primary",
				"system":      system,
				"permissions": []any{map[string]any{"action": "*", "resource": "*", "effect": "deny"}},
			},
		},
	}
}

// Complete runs one opencode run turn. tools is ignored: native tool calling needs MCP (the
// opencode lead engine); here the tools are described in the system prompt and the reply is a JSON
// action. maxTokens is ignored too (opencode run has no such flag).
func (m *OpenCodeModel) Complete(ctx context.Context, messages []contracts.ModelMessage, _ []map[string]any, _ int) (contracts.TurnResult, error) {
	systems := []string{}
	for _, msg := range messages {
		if msg.Role == "system" {
			systems = append(systems, msg.Content)
		}
	}
	system := strings.Join(systems, "\n\n")
	run := func(ctx context.Context) (OpenCodeResult, error) {
		dir, cfgPath, err := WriteOpenCodeConfig(m.turnConfig(system))
		if err != nil {
			return OpenCodeResult{}, err
		}
		defer os.RemoveAll(dir)
		return RunOpenCode(ctx, OpenCodeRun{
			Args: OpenCodeBaseArgs(m.model, m.agent, m.standalone), Prompt: RenderTranscript(messages),
			Binary: m.binary, Cwd: m.cwd, TimeoutS: m.timeoutS, Provider: m.name,
			FallbackModel: m.model, MaxCostUSD: &m.maxBudgetUSD, Env: OpenCodeInjectedEnv(cfgPath),
		})
	}
	result, err := WithRetries(ctx, m.retry, run)
	if err != nil {
		return contracts.TurnResult{}, err
	}
	return contracts.TurnResult{
		Text: result.Text, Usage: result.Usage, StopReason: result.StopReason, SessionID: result.SessionID,
	}, nil
}

// BudgetCapped is this model with its per-call cap lowered to what is left of the budget (see
// CallBudgetUSD); the metered wrapper asks for it before each call (python: budget_capped).
func (m *OpenCodeModel) BudgetCapped(remainingUSD float64) contracts.ModelProvider {
	capped := *m
	capped.maxBudgetUSD = CallBudgetUSD(m.maxBudgetUSD, remainingUSD)
	return &capped
}

// EstimateCostUSD is the cost OpenCode reported; before a call, its worst case: the token price
// when the model is in the price table (or configured), capped by LHA_OPENCODE_MAX_BUDGET_USD, and
// otherwise that cap. It never returns an unknown-price error.
func (m *OpenCodeModel) EstimateCostUSD(usage contracts.Usage) (float64, error) {
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

// HealthCheck runs opencode --version, without spending tokens (python:
// OpenCodeModel.health_check). A parked mission resumes only when this is healthy, so a missing CLI
// is DOWN.
func (m *OpenCodeModel) HealthCheck(ctx context.Context, timeoutS float64) (bool, string) {
	if _, err := exec.LookPath(m.binary); err != nil {
		return false, fmt.Sprintf("%s: %s not found on PATH", m.name, contracts.PyRepr(m.binary))
	}
	out, timedOut, err := runOpenCodeCLI(ctx, m.binary, []string{"--version"}, timeoutS)
	if timedOut {
		return false, m.name + ": opencode --version timed out"
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, fmt.Sprintf("%s: opencode --version exited %d", m.name, exitErr.ExitCode())
		}
		return false, fmt.Sprintf("%s: %v", m.name, err)
	}
	return true, m.name + ": " + pyfmt.PyStrip(strings.ToValidUTF8(string(out), "\uFFFD"))
}

// runOpenCodeCLI runs a short, prompt-less opencode command and returns its combined output.
func runOpenCodeCLI(ctx context.Context, binary string, args []string, timeoutS float64) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutS*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = OpenCodeChildEnv(nil)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return out, ctx.Err() == context.DeadlineExceeded, err
}

// WriteOpenCodeConfig writes the injected OpenCode config as JSON in a fresh temporary directory
// and returns it and the file's path (python: _config_file). The caller removes the directory.
func WriteOpenCodeConfig(config map[string]any) (dir, path string, err error) {
	dir, err = os.MkdirTemp("", "lha-opencode-")
	if err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, "opencode.json")
	data, err := json.Marshal(config)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return dir, path, nil
}

// OpenCodeInjectedEnv is the environment that points a child opencode at our config and nothing
// else (python: _injected_env).
func OpenCodeInjectedEnv(cfgPath string) map[string]string {
	return map[string]string{"OPENCODE_CONFIG": cfgPath, "OPENCODE_DISABLE_PROJECT_CONFIG": "true"}
}
