package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent/mcpbridge"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// The opencode lead engine: one lead cycle is one opencode run session (python:
// lha.agent.opencode_engine).
//
// With LHA_LEAD_ENGINE=opencode the built-in turn loop is replaced, for the lead only, by
// OpenCode's own agentic loop. Everything around it is unchanged: LHA picks the item, recites the
// mission anchor, then (after the session) runs the deterministic checks and the item's witnesses,
// commits a verified result or rolls a failed attempt back, and replans blocked items. A fresh
// session each cycle is deliberate: the anchor in git is the memory, not the chat.
//
// Tools (LHA_OPENCODE_TOOLS):
//
//   - lha (default): OpenCode's own tools are denied by an injected agent, and LHA's tools are
//     served to it over MCP (mcpbridge) as lha_<tool>. Every command runs in LHA's sandbox,
//     irreversible commands go to the human gate, and egress follows the allow-list.
//   - native: OpenCode uses its own read/edit/shell on the host workdir. Nothing is isolated, so
//     it needs sandbox=local with allow_unsafe_local. A deny list keeps git history, publishing
//     and the web out of reach, but a prefix deny list is not a safety boundary.
//
// In both modes the session also gets a verify tool: it runs the mission's checks and the item's
// witnesses exactly as the harness will afterwards, so OpenCode can iterate to green before it
// stops. LHA still verifies again after the session; verify never marks anything done.
//
// Budget: the session is authorized up front with LHA_OPENCODE_MAX_BUDGET_USD as its worst case,
// or what is left of the budget when that is less. OpenCode has no spend-cap flag, so the engine
// kills a session that reaches its cap while streaming; the cost OpenCode reports per step is
// recorded in the mission's ledger, and a killed session is charged what it streamed
// (OpenCodeSessionProgress.SpentUSD), or its whole cap when its spend could not be seen.

// OpenCodeNativeDeny is denied in a native session: LHA owns git history, and publishing and the
// web need a human or the egress policy. Prefix rules only: sh -c 'git push' is not caught.
var OpenCodeNativeDeny = buildOpenCodeNativeDeny()

func buildOpenCodeNativeDeny() []map[string]string {
	patterns := []string{
		"git commit *", "git push *", "git reset *", "git checkout *", "git switch *",
		"git rebase *", "git merge *", "git tag *", "git stash *", "gh *",
		"npm publish *", "pnpm publish *", "uv publish *", "twine upload *", "docker push *",
		"curl *", "wget *",
	}
	rules := make([]map[string]string, 0, len(patterns)+4)
	for _, pattern := range patterns {
		rules = append(rules, map[string]string{"action": "shell", "resource": pattern, "effect": "deny"})
	}
	return append(rules,
		map[string]string{"action": "webfetch", "resource": "*", "effect": "deny"},
		map[string]string{"action": "websearch", "resource": "*", "effect": "deny"},
		map[string]string{"action": "subagent", "resource": "*", "effect": "deny"},
		map[string]string{"action": "question", "resource": "*", "effect": "deny"},
	)
}

// OpenCodeEngineOptions configures an OpenCodeEngine; zero values take the Python defaults
// (Standalone nil => true).
type OpenCodeEngineOptions struct {
	Binary       string  // "" => "opencode"
	Model        string  // "" => model.OpenCodeDefaultModel
	Agent        string  // "" => model.OpenCodeAgentDefault
	Tools        string  // "" => "lha"; else "lha" | "native"
	Standalone   *bool   // nil => true
	MaxBudgetUSD float64 // <= 0 => 5.0
	TimeoutS     float64 // <= 0 => 3600
}

// OpenCodeEngine runs a lead cycle as one opencode run session.
type OpenCodeEngine struct {
	binary       string
	model        string
	agent        string
	tools        string
	standalone   bool
	maxBudgetUSD float64
	timeoutS     float64
	name         string
}

var _ Engine = (*OpenCodeEngine)(nil)

// NewOpenCodeEngine builds an engine; an unknown tool mode is an error.
func NewOpenCodeEngine(o OpenCodeEngineOptions) (*OpenCodeEngine, error) {
	standalone := true
	if o.Standalone != nil {
		standalone = *o.Standalone
	}
	e := &OpenCodeEngine{
		binary: o.Binary, model: o.Model, agent: o.Agent, tools: o.Tools, standalone: standalone,
		maxBudgetUSD: o.MaxBudgetUSD, timeoutS: o.TimeoutS,
	}
	if e.binary == "" {
		e.binary = "opencode"
	}
	if e.agent == "" {
		e.agent = model.OpenCodeAgentDefault
	}
	if e.tools == "" {
		e.tools = "lha"
	}
	if e.tools != "lha" && e.tools != "native" {
		return nil, fmt.Errorf("unknown OpenCode tool mode: %s", contracts.PyRepr(e.tools))
	}
	if e.maxBudgetUSD <= 0 {
		e.maxBudgetUSD = 5.0
	}
	if e.timeoutS <= 0 {
		e.timeoutS = 3600.0
	}
	name := e.model
	if name == "" {
		name = "default"
	}
	e.name = "opencode_engine:" + name
	return e, nil
}

// Name is "opencode_engine:<model>".
func (e *OpenCodeEngine) Name() string { return e.name }

// Native reports whether the session uses OpenCode's own tools.
func (e *OpenCodeEngine) Native() bool { return e.tools == "native" }

// SessionEvent is the trace event the loop records when the session ends.
func (e *OpenCodeEngine) SessionEvent() string { return "opencode_session" }

// permissions is the injected agent's permission list (python: _permissions). LHA mode denies
// every action but the lha_* tools; native mode denies git history, publishing and the web.
func (e *OpenCodeEngine) permissions() []map[string]string {
	if e.Native() {
		rules := make([]map[string]string, len(OpenCodeNativeDeny))
		for i, rule := range OpenCodeNativeDeny {
			copied := make(map[string]string, len(rule))
			for k, v := range rule {
				copied[k] = v
			}
			rules[i] = copied
		}
		return rules
	}
	return []map[string]string{
		{"action": "*", "resource": "*", "effect": "deny"},
		{"action": "lha_*", "resource": "*", "effect": "allow"},
	}
}

// config is the per-session OpenCode config: an agent, and LHA's tools over MCP (python: _config).
func (e *OpenCodeEngine) config(system string, bridge *mcpbridge.Bridge) map[string]any {
	return map[string]any{
		"mcp": map[string]any{"servers": map[string]any{bridge.Name: bridge.OpenCodeServer()}},
		"agents": map[string]any{
			e.agent: map[string]any{
				"description": "LHA lead engine session",
				"mode":        "primary",
				"system":      system,
				"permissions": e.permissions(),
			},
		},
	}
}

// Run is one session on req.Messages in req.Cwd, metered through req.Meter. A refused budget
// returns a *governor.BudgetExceeded before opencode runs. A timeout or a spend cap
// (error_max_*) ends the session early with EngineRun.Stopped set; any other opencode failure is
// returned as an error.
func (e *OpenCodeEngine) Run(ctx context.Context, req EngineRequest) (EngineRun, error) {
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
	tools := bridgeTools(req.Specs, req.Dispatch, req.Verify, req.CycleID, used, e.Native(), nativeBridged, "oc")

	// The session's cap is what is left of the budget when that is less than the configured cap.
	budget := e.maxBudgetUSD
	if mm, ok := req.Meter.(*governor.MeteredModel); ok {
		budget = model.CallBudgetUSD(budget, mm.RemainingUSD())
	}
	cap := budget
	var run EngineRun
	err := governor.RunExternal(ctx, req.Meter, budget, func(ctx context.Context) (contracts.Usage, error) {
		bridge := mcpbridge.New(tools)
		if err := bridge.Start(ctx); err != nil {
			return contracts.Usage{}, err
		}
		dir, cfgPath, err := model.WriteOpenCodeConfig(e.config(system, bridge))
		if err != nil {
			_ = bridge.Close()
			return contracts.Usage{}, err
		}
		var onProgress func(*model.OpenCodeSessionProgress)
		if req.OnProgress != nil {
			onProgress = func(p *model.OpenCodeSessionProgress) { req.OnProgress(p) }
		}
		result, runErr := model.RunOpenCode(ctx, model.OpenCodeRun{
			Args: model.OpenCodeBaseArgs(e.model, e.agent, e.standalone), Prompt: prompt,
			Binary: e.binary, Cwd: req.Cwd, TimeoutS: e.timeoutS, Provider: e.name,
			FallbackModel: e.model, MaxCostUSD: &cap, Env: model.OpenCodeInjectedEnv(cfgPath),
			OnProgress: onProgress,
		})
		_ = os.RemoveAll(dir)
		_ = bridge.Close()
		if runErr != nil {
			// The work so far is still in the workdir: verify it rather than drop it.
			var timeout *model.OpenCodeTimeoutError
			if errors.As(runErr, &timeout) {
				// Charged what the session spent (its cap when a model has no price).
				run = e.stopped(runErr.Error(), used)
				if timeout.Progress != nil {
					return timeout.Progress.Usage(e.name, e.model), nil
				}
				return contracts.Usage{Provider: e.name}, nil
			}
			var failed *model.OpenCodeError
			if errors.As(runErr, &failed) && strings.HasPrefix(failed.Subtype, "error_max") {
				run = e.stopped(runErr.Error(), used)
				if failed.Usage != nil {
					return *failed.Usage, nil
				}
				return contracts.Usage{Provider: e.name}, nil
			}
			return contracts.Usage{}, runErr
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

func (e *OpenCodeEngine) stopped(reason string, used *usedTools) EngineRun {
	names := used.list()
	return EngineRun{
		ToolCalls:      len(names),
		ToolsUsed:      names,
		EditsUntracked: e.Native(),
		Stopped:        reason,
	}
}
