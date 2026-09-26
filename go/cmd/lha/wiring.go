package main

import (
	"context"
	"errors"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/tools"
	"github.com/calvinchengx/long-horizon-agent/go/internal/hitl"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
)

// The execution layer (sandboxes, tools, the allow-list dispatcher, the web tools and the human
// gate) links into the CLI here, and only here (python: lha.agent.assembly open_lead_sandbox +
// lead_dispatcher + with_decision_tool, and lha.cli.main._gate).

// consoleGate builds the --approve-interactive gate (tests replace it).
var consoleGate = func(settings *config.Settings) contracts.HITLGate { return hitl.ConsoleGate(settings) }

// toolbox is one run's lead sandbox session and dispatcher.
type toolbox struct {
	session    contracts.SandboxSession
	dispatcher contracts.ToolDispatcher
	gate       contracts.HITLGate
}

func (t *toolbox) Session() contracts.SandboxSession    { return t.session }
func (t *toolbox) Dispatcher() contracts.ToolDispatcher { return t.dispatcher }
func (t *toolbox) Close(ctx context.Context) error      { return t.session.Close(ctx) }

// BindGateStore points a gate that records its events (the terminal approver) at the run's
// mission store (python: bind_gate_store), so `lha gates` lists them.
func (t *toolbox) BindGateStore(store persistence.Store) {
	if binder, ok := t.gate.(interface{ BindStore(hitl.GateRecorder) }); ok {
		binder.BindStore(store)
	}
}

// openToolbox opens the lead's sandbox session and dispatcher for a run (agent.ToolboxOpener).
//
// It refuses an unsafe local sandbox (*execution.UnsafeSandboxError) and a lethal-trifecta run
// (*tools.RuleOfTwoViolation) before touching the workspace, builds the lead dispatcher (every
// tool, mutating allowed; the web tools under the egress policy when LHA_WEB_ALLOW_HOSTS is set,
// unless req.AllowEgress is false; irreversible commands go to the console y/N gate when
// req.ApproveInteractive, else they are refused), adds record_decision bound to req.Anchor, and
// only then opens the sandbox from settings: kind (LHA_SANDBOX), image (LHA_SANDBOX_IMAGE) and
// the sandbox egress allow-list (LHA_SANDBOX_EGRESS). Gate events (tool_approval, gate_reminder)
// reach the agent loop through the dispatcher's DrainEvents, which the record_decision wrapper
// forwards.
var openToolbox agent.ToolboxOpener = func(ctx context.Context, req agent.ToolboxRequest) (agent.Toolbox, error) {
	settings := req.Settings
	if settings == nil {
		return nil, errors.New("openToolbox: no settings")
	}
	// BuildSandbox refuses 'local' without the opt-in (and an unknown / e2b kind) without any IO.
	sandbox, err := execution.BuildSandbox(settings.Sandbox, execution.SandboxOptions{
		AllowUnsafeLocal: settings.AllowUnsafeLocal,
		Image:            settings.SandboxImage,
		EgressHosts:      settings.SandboxEgressHosts(),
		Memory:           settings.SandboxMemory,
		CPUs:             settings.SandboxCPUs,
		TmpSize:          settings.SandboxTmpSize,
	})
	if err != nil {
		return nil, err
	}
	var gate contracts.HITLGate // nil: irreversible commands are refused
	if req.ApproveInteractive {
		gate = consoleGate(settings)
	}
	dispatcher, err := tools.BuildRunDispatcher(settings, tools.RunDispatcherOptions{
		AllowMutating: true,
		DisableEgress: req.AllowEgress != nil && !*req.AllowEgress,
		Gate:          gate,
	})
	if err != nil {
		return nil, err
	}
	var lead contracts.ToolDispatcher = dispatcher
	if req.Anchor != nil {
		lead = tools.WithDecisionTool(dispatcher, req.Anchor)
	}
	session, err := sandbox.Open(ctx, req.Workdir, "")
	if err != nil {
		return nil, err
	}
	return &toolbox{session: session, dispatcher: lead, gate: gate}, nil
}

// validateWebTools validates the web settings up front, before any planning spend (python: the
// web_tools(settings) half of preflight_run_tools: LHA_WEB_ALLOW_PORTS, LHA_WEB_CREDENTIALS
// bindings, the search provider). The run-level Rule of Two is checked separately.
var validateWebTools = func(settings *config.Settings) error {
	_, err := tools.WebTools(settings, nil)
	return err
}

// preflightRunTools is python's preflight_run_tools: the Rule of Two, then the web settings.
func preflightRunTools(settings *config.Settings) error { return tools.PreflightRunTools(settings) }
