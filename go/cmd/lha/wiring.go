package main

import (
	"context"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
)

// The execution layer (sandboxes, tools, the allow-list dispatcher, the web tools and the human
// gate) links into the CLI here, and only here.
//
// openToolbox opens the lead's sandbox session and dispatcher for a run. Its contract is
// agent.ToolboxOpener: refuse an unsafe local sandbox and a lethal-trifecta run before touching
// the workspace; build every lead tool (mutating allowed), the web tools under the egress policy
// when LHA_WEB_ALLOW_HOSTS is set (unless req.AllowEgress is false), record_decision bound to
// req.Anchor (its RecordDecision method), and the console y/N gate when req.ApproveInteractive.
// Until the execution package is linked, every run stops with agent.ErrExecutionNotLinked.
var openToolbox agent.ToolboxOpener = func(context.Context, agent.ToolboxRequest) (agent.Toolbox, error) {
	return nil, agent.ErrExecutionNotLinked
}

// validateWebTools validates the web settings up front, before any planning spend (python: the
// web_tools(settings) half of preflight_run_tools: LHA_WEB_ALLOW_PORTS, LHA_WEB_CREDENTIALS
// bindings, the search provider). The run-level Rule of Two is checked separately
// (agent.CheckRunRuleOfTwo). The execution layer replaces this with its own validation; until
// then openToolbox refuses every run, so nothing runs unvalidated.
var validateWebTools = func(*config.Settings) error { return nil }

// preflightRunTools is python's preflight_run_tools: the Rule of Two, then the web settings.
func preflightRunTools(settings *config.Settings) error {
	if err := agent.CheckRunRuleOfTwo(settings); err != nil {
		return err
	}
	return validateWebTools(settings)
}
