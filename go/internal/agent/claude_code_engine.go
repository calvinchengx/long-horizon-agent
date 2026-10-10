package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent/mcpbridge"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// The claude_code lead engine: one lead cycle is one claude -p session (python:
// lha.agent.claude_code_engine).
//
// With LHA_LEAD_ENGINE=claude_code the built-in turn loop is replaced, for the lead only, by
// Claude Code's own agentic loop. Everything around it is unchanged: LHA picks the item, recites
// the mission anchor, then (after the session) runs the deterministic checks and the item's
// witnesses, commits a verified result or rolls a failed attempt back, and replans blocked items.
// A fresh session each cycle is deliberate: the anchor in git is the memory, not the chat.
//
// Tools (LHA_CLAUDE_CODE_TOOLS):
//
//   - lha (default): Claude Code's built-in tools are switched off (--tools "") and LHA's tools
//     are served to it over MCP (mcpbridge). Every command runs in LHA's sandbox, irreversible
//     commands go to the human gate, and egress follows the allow-list.
//   - native: Claude Code uses its own Read/Edit/Bash on the host workdir. Nothing is isolated,
//     so it needs sandbox=local with allow_unsafe_local. A deny list keeps git history,
//     publishing and the web out of reach, but a prefix deny list is not a safety boundary.
//
// In both modes the session also gets a verify tool: it runs the mission's checks and the item's
// witnesses exactly as the harness will afterwards, so Claude Code can iterate to green before it
// stops. LHA still verifies again after the session; verify never marks anything done.
//
// Budget: the session is authorized up front with LHA_CLAUDE_CODE_MAX_BUDGET_USD as its worst
// case (also passed as --max-budget-usd), and the total_cost_usd Claude Code reports is recorded
// in the mission's ledger.

// NativeTools are Claude Code's built-in tools a native session may use.
var NativeTools = []string{"Read", "Edit", "MultiEdit", "Write", "Glob", "Grep", "LS", "Bash", "TodoWrite"}

// NativeDeny is denied in a native session: LHA owns git history, and publishing and the web need
// a human or the egress policy. Prefix rules only: sh -c 'git push' is not caught.
var NativeDeny = []string{
	"Bash(git commit:*)",
	"Bash(git push:*)",
	"Bash(git reset:*)",
	"Bash(git checkout:*)",
	"Bash(git switch:*)",
	"Bash(git rebase:*)",
	"Bash(git merge:*)",
	"Bash(git tag:*)",
	"Bash(git stash:*)",
	"Bash(gh:*)",
	"Bash(npm publish:*)",
	"Bash(pnpm publish:*)",
	"Bash(uv publish:*)",
	"Bash(twine upload:*)",
	"Bash(docker push:*)",
	"Bash(curl:*)",
	"Bash(wget:*)",
	"WebFetch",
	"WebSearch",
}

// Tools a native session still gets from LHA over MCP.
var nativeBridged = []string{"record_decision"}

const verifyDescription = "Run the mission's deterministic checks and this item's witnesses, exactly as the harness " +
	"will after this session. Returns PASSED or the failure report. Call it when you think the " +
	"item is done; it does not mark anything done."

// EngineDispatch runs one tool call through LHA's dispatcher.
type EngineDispatch func(ctx context.Context, call contracts.ToolCall) contracts.ToolResult

// EngineVerify runs the mission's checks and the item's witnesses (with harness integrity).
type EngineVerify func(ctx context.Context) (contracts.VerificationResult, error)

// Engine is a lead engine: a whole lead cycle runs as one external agent session (Claude Code or
// OpenCode). Both share EngineRun, EngineRequest, the MCP bridge and the loop's wiring.
type Engine interface {
	Name() string
	Native() bool
	// SessionEvent is the trace event the loop records when the session ends.
	SessionEvent() string
	Run(ctx context.Context, req EngineRequest) (EngineRun, error)
}

// EngineRun is what one session did, for the cycle's checkpoint and trace.
type EngineRun struct {
	Summary   string
	Turns     int
	ToolCalls int
	ToolsUsed []string
	// EditsUntracked: the session could change files without going through LHA's dispatcher
	// (native tools).
	EditsUntracked bool
	// Stopped is set when the session stopped early (time or spend cap); its work is still
	// verified.
	Stopped   string
	SessionID string
}

// VerificationText is the verify tool's reply for result.
func VerificationText(result contracts.VerificationResult) string {
	if result.AllGreen {
		return "PASSED: every check and witness is green."
	}
	if result.Verdict == contracts.VerdictUnverified {
		return "UNVERIFIED: this mission has no checks to run."
	}
	return "FAILED:\n" + result.FailureReport(0)
}

// ClaudeCodeEngineOptions configures a ClaudeCodeEngine; zero values take the Python defaults.
type ClaudeCodeEngineOptions struct {
	Binary       string  // "" => "claude"
	Model        string  // "" => model.ClaudeCodeDefaultModel
	Tools        string  // "" => "lha"; else "lha" | "native"
	MaxBudgetUSD float64 // <= 0 => 5.0
	TimeoutS     float64 // <= 0 => 3600
}

// ClaudeCodeEngine runs a lead cycle as one claude -p session.
type ClaudeCodeEngine struct {
	binary       string
	model        string
	tools        string
	maxBudgetUSD float64
	timeoutS     float64
	name         string
}

// NewClaudeCodeEngine builds an engine; an unknown tool mode is an error.
func NewClaudeCodeEngine(o ClaudeCodeEngineOptions) (*ClaudeCodeEngine, error) {
	e := &ClaudeCodeEngine{binary: o.Binary, model: o.Model, tools: o.Tools, maxBudgetUSD: o.MaxBudgetUSD, timeoutS: o.TimeoutS}
	if e.binary == "" {
		e.binary = "claude"
	}
	if e.model == "" {
		e.model = model.ClaudeCodeDefaultModel
	}
	if e.tools == "" {
		e.tools = "lha"
	}
	if e.tools != "lha" && e.tools != "native" {
		return nil, fmt.Errorf("unknown Claude Code tool mode: %s", contracts.PyRepr(e.tools))
	}
	if e.maxBudgetUSD <= 0 {
		e.maxBudgetUSD = 5.0
	}
	if e.timeoutS <= 0 {
		e.timeoutS = 3600.0
	}
	e.name = "claude_code_engine:" + e.model
	return e, nil
}

// Name is "claude_code_engine:<model>".
func (e *ClaudeCodeEngine) Name() string { return e.name }

// Native reports whether the session uses Claude Code's own tools.
func (e *ClaudeCodeEngine) Native() bool { return e.tools == "native" }

// SessionEvent is the trace event the loop records when the session ends.
func (e *ClaudeCodeEngine) SessionEvent() string { return "claude_code_session" }

// usedTools records the tools a session called, in order. A handler may still be running when a
// killed session is torn down, so it is locked.
type usedTools struct {
	mu    sync.Mutex
	names []string
}

func (u *usedTools) add(name string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.names = append(u.names, name)
	return len(u.names)
}

func (u *usedTools) list() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string{}, u.names...)
}

// bridgeTools are the specs bridged over MCP (all of them, or only bridged in native mode) plus
// verify. used collects the names of the tools called, in order; idPrefix tags this engine's call
// ids (claude: "cc", opencode: "oc") so two engines never mint the same id.
func bridgeTools(specs []contracts.ToolSpec, dispatch EngineDispatch, verify EngineVerify, cycleID string, used *usedTools, native bool, bridged []string, idPrefix string) []mcpbridge.Tool {
	handler := func(spec contracts.ToolSpec) mcpbridge.Handler {
		return func(ctx context.Context, arguments map[string]any) (string, bool, error) {
			n := used.add(spec.Name)
			call := contracts.ToolCall{ID: fmt.Sprintf("%s-%s-%d", cycleID, idPrefix, n), Name: spec.Name, Arguments: arguments}
			result := dispatch(ctx, call)
			if result.OK {
				return result.Content, false, nil
			}
			text := result.ErrorText()
			if text == "" {
				text = result.Content
			}
			return text, true, nil
		}
	}
	tools := []mcpbridge.Tool{}
	for _, s := range specs {
		if !native || contains(bridged, s.Name) {
			tools = append(tools, mcpbridge.NewTool(s.Name, s.Description, s.Parameters, handler(s)))
		}
	}
	tools = append(tools, mcpbridge.NewTool("verify", verifyDescription, map[string]any{},
		func(ctx context.Context, _ map[string]any) (string, bool, error) {
			result, err := verify(ctx)
			if err != nil {
				return "", true, err
			}
			return VerificationText(result), !result.AllGreen, nil
		}))
	return tools
}

// bridgeTools is the claude_code engine's bridge (all specs, or only nativeBridged in native mode).
func (e *ClaudeCodeEngine) bridgeTools(specs []contracts.ToolSpec, dispatch EngineDispatch, verify EngineVerify, cycleID string, used *usedTools) []mcpbridge.Tool {
	return bridgeTools(specs, dispatch, verify, cycleID, used, e.Native(), nativeBridged, "cc")
}

// Args is the claude argv (after the binary) for a session on bridge with system appended to
// Claude Code's own system prompt.
func (e *ClaudeCodeEngine) Args(bridge *mcpbridge.Bridge, system string) []string {
	return e.args(bridge, system, e.maxBudgetUSD)
}

func (e *ClaudeCodeEngine) args(bridge *mcpbridge.Bridge, system string, maxBudgetUSD float64) []string {
	args := append(model.ClaudeCodeBaseArgs(e.model, maxBudgetUSD),
		"--strict-mcp-config",
		"--mcp-config",
		bridge.MCPConfig(),
		"--append-system-prompt",
		system,
	)
	if e.Native() {
		args = append(args, "--allowedTools")
		args = append(args, NativeTools...)
		args = append(args, bridge.AllowedTools()...)
		args = append(args, "--disallowedTools")
		args = append(args, NativeDeny...)
	} else {
		args = append(args, "--tools", "", "--allowedTools")
		args = append(args, bridge.AllowedTools()...)
	}
	return args
}

// EngineRequest is one session's inputs.
type EngineRequest struct {
	Messages []contracts.ModelMessage // system + task
	Cwd      string
	CycleID  string
	Specs    []contracts.ToolSpec
	Dispatch EngineDispatch
	Verify   EngineVerify
	// Meter meters the session when it is a *governor.MeteredModel (else it runs unmetered).
	Meter contracts.ModelProvider
	// OnProgress sees each turn and tool call as the session makes it (stream-json).
	OnProgress func(model.Progress)
}

// Run is one session on req.Messages in req.Cwd, metered through req.Meter. A refused budget
// returns a *governor.BudgetExceeded before claude runs. A timeout or a spend/turn cap
// (error_max_*) ends the session early with EngineRun.Stopped set; any other claude failure is
// returned as an error.
func (e *ClaudeCodeEngine) Run(ctx context.Context, req EngineRequest) (EngineRun, error) {
	systems, prompts := []string{}, []string{}
	for _, m := range req.Messages {
		if m.Role == "system" {
			systems = append(systems, m.Content)
		} else {
			prompts = append(prompts, m.Content)
		}
	}
	system, prompt := strings.Join(systems, "\n\n"), strings.Join(prompts, "\n\n")
	used := &usedTools{}
	tools := e.bridgeTools(req.Specs, req.Dispatch, req.Verify, req.CycleID, used)

	// The session's cap is what is left of the budget when that is less than the configured cap.
	budget := e.maxBudgetUSD
	if mm, ok := req.Meter.(*governor.MeteredModel); ok {
		budget = model.CallBudgetUSD(budget, mm.RemainingUSD())
	}
	var run EngineRun
	err := governor.RunExternal(ctx, req.Meter, budget, func(ctx context.Context) (contracts.Usage, error) {
		bridge := mcpbridge.New(tools)
		if err := bridge.Start(ctx); err != nil {
			return contracts.Usage{}, err
		}
		var onProgress func(*model.SessionProgress)
		if req.OnProgress != nil {
			onProgress = func(p *model.SessionProgress) { req.OnProgress(p) }
		}
		result, err := model.RunClaude(ctx, model.ClaudeCodeRun{
			Args: e.args(bridge, system, budget), Prompt: prompt, Binary: e.binary, Cwd: req.Cwd,
			TimeoutS: e.timeoutS, Provider: e.name, FallbackModel: e.model, OnProgress: onProgress,
		})
		_ = bridge.Close()
		if err != nil {
			// The work so far is still in the workdir: verify it rather than drop it.
			var timeout *model.ClaudeCodeTimeoutError
			if errors.As(err, &timeout) {
				// Charged what the session spent (its cap when a model has no price).
				run = e.stopped(err.Error(), used)
				if timeout.Progress != nil {
					return timeout.Progress.Usage(e.name, e.model), nil
				}
				return contracts.Usage{Provider: e.name}, nil
			}
			var failed *model.ClaudeCodeError
			if errors.As(err, &failed) && strings.HasPrefix(failed.Subtype, "error_max") {
				run = e.stopped(err.Error(), used)
				if failed.Usage != nil {
					return *failed.Usage, nil
				}
				return contracts.Usage{Provider: e.name}, nil
			}
			return contracts.Usage{}, err
		}
		sessionID := ""
		if result.SessionID != nil {
			sessionID = *result.SessionID
		}
		names := used.list()
		run = EngineRun{
			Summary:        result.Text,
			Turns:          result.NumTurns,
			ToolCalls:      len(names),
			ToolsUsed:      names,
			EditsUntracked: e.Native(),
			SessionID:      sessionID,
		}
		return result.Usage, nil
	})
	if err != nil {
		return EngineRun{}, err
	}
	return run, nil
}

func (e *ClaudeCodeEngine) stopped(reason string, used *usedTools) EngineRun {
	names := used.list()
	return EngineRun{
		ToolCalls:      len(names),
		ToolsUsed:      names,
		EditsUntracked: e.Native(),
		Stopped:        reason,
	}
}

// LeadEngine is the claude_code / opencode lead engine from settings, or nil for the built-in
// loop (python: lha.agent.assembly.lead_engine).
//
// Native CLI tools act on the host with no sandbox, so they need sandbox=local and
// allow_unsafe_local, and they cannot run under a guarded dispatcher (the orchestrator's ownership
// guard only sees calls that go through LHA's tools).
func LeadEngine(settings *config.Settings, guarded bool) (Engine, error) {
	if settings.LeadEngine == "opencode" {
		if settings.OpenCodeTools == "native" {
			if err := requireNativeEngine(settings, guarded, "LHA_OPENCODE_TOOLS", "OpenCode"); err != nil {
				return nil, err
			}
		}
		return NewOpenCodeEngine(OpenCodeEngineOptions{
			Binary:       settings.OpenCodeBin,
			Model:        settings.OpenCodeModel,
			Agent:        settings.OpenCodeAgent,
			Tools:        settings.OpenCodeTools,
			Standalone:   model.Bool(settings.OpenCodeStandalone),
			MaxBudgetUSD: settings.OpenCodeMaxBudgetUSD,
			TimeoutS:     settings.OpenCodeTimeoutS,
		})
	}
	if settings.LeadEngine != "claude_code" {
		return nil, nil
	}
	if settings.ClaudeCodeTools == "native" {
		if settings.Sandbox != "local" || !settings.AllowUnsafeLocal {
			return nil, errors.New("LHA_CLAUDE_CODE_TOOLS=native runs Claude Code's own tools on the host with no " +
				"isolation: it needs LHA_SANDBOX=local and LHA_ALLOW_UNSAFE_LOCAL=true " +
				"(or use the default LHA_CLAUDE_CODE_TOOLS=lha)")
		}
		if guarded {
			return nil, errors.New("LHA_CLAUDE_CODE_TOOLS=native bypasses the orchestrator's file-ownership guard; " +
				"use LHA_CLAUDE_CODE_TOOLS=lha with the multi-agent organization")
		}
	}
	name := model.ClaudeCodeDefaultModel
	if settings.ModelBackend == "claude_code" && settings.ModelName != model.DefaultSettingsModelName() {
		name = settings.ModelName
	}
	return NewClaudeCodeEngine(ClaudeCodeEngineOptions{
		Binary:       settings.ClaudeCodeBin,
		Model:        name,
		Tools:        settings.ClaudeCodeTools,
		MaxBudgetUSD: settings.ClaudeCodeMaxBudgetUSD,
		TimeoutS:     settings.ClaudeCodeTimeoutS,
	})
}

// engineSummaryCap bounds the session summary kept for the cycle (python: run.summary[:2000]).
const engineSummaryCap = 2000

// requireNativeEngine refuses a native-mode engine that runs its own tools on the host: it needs
// sandbox=local with allow_unsafe_local, and it cannot run under a guarded dispatcher (python:
// _require_native_engine).
func requireNativeEngine(settings *config.Settings, guarded bool, varName, engineLabel string) error {
	if settings.Sandbox != "local" || !settings.AllowUnsafeLocal {
		return errors.New(varName + "=native runs " + engineLabel + "'s own tools on the host with no " +
			"isolation: it needs LHA_SANDBOX=local and LHA_ALLOW_UNSAFE_LOCAL=true " +
			"(or use the default " + varName + "=lha)")
	}
	if guarded {
		return errors.New(varName + "=native bypasses the orchestrator's file-ownership guard; " +
			"use " + varName + "=lha with the multi-agent organization")
	}
	return nil
}
