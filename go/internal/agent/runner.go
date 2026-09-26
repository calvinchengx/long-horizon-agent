package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// A local (non-Temporal) mission runner (python: lha.agent.runner). It drives the AgentLoop
// cycle by cycle until the mission completes, deadlocks (nothing actionable but items still open
// — never reported as complete), the budget governor refuses the next step or a model call
// (*governor.BudgetExceeded), or a loop is detected. Every model call (planner + lead) goes
// through ONE CostMeter so a single ledger/governor sees all spend.

// DecisionChainStop prefixes StoppedReason when the committed decision log fails hash-chain
// verification.
const DecisionChainStop = "decision log failed verification"

// ErrExecutionNotLinked is returned when a run has no toolbox opener (RunOptions.OpenToolbox is
// nil). The CLI links the execution layer in cmd/lha/wiring.go.
var ErrExecutionNotLinked = errors.New("execution layer not linked")

// Toolbox is the execution layer of one run: the lead's sandbox session and its tool dispatcher
// (python: open_lead_sandbox + lead_dispatcher + with_decision_tool). The execution package
// satisfies it; tests use in-package fakes.
type Toolbox interface {
	// Session is the open sandbox session every tool call and sandbox check runs in.
	Session() contracts.SandboxSession
	// Dispatcher is the lead's dispatcher: every lead tool, mutating allowed, the web tools under
	// the egress policy, and record_decision bound to the request's Anchor. A dispatcher that
	// keeps human-gate events implements EventDrainer.
	Dispatcher() contracts.ToolDispatcher
	// Close closes the sandbox session (and anything else the toolbox owns).
	Close(ctx context.Context) error
}

// ToolboxRequest is what a ToolboxOpener gets.
type ToolboxRequest struct {
	Settings *config.Settings // sandbox kind/image/egress, web settings, Rule of Two inputs
	Workdir  string
	// Anchor is the run's mission anchor (record_decision queues decisions on it). It is not yet
	// initialized when the toolbox opens (Python opens the sandbox first too).
	Anchor *state.GitMissionAnchor
	// AllowEgress: nil => web tools iff LHA_WEB_ALLOW_HOSTS is set; false drops them.
	AllowEgress *bool
	// ApproveInteractive routes irreversible commands to a console y/N gate (else refused).
	ApproveInteractive bool
}

// ToolboxOpener opens the run's Toolbox. It must refuse an unsafe local sandbox (without
// AllowUnsafeLocal) and a lethal-trifecta run before touching the workspace.
type ToolboxOpener func(ctx context.Context, req ToolboxRequest) (Toolbox, error)

// MissionSummary is the terminal summary of a locally-run mission. StoppedReason is "complete"
// ONLY when every item is verified done; otherwise e.g. "deadlocked: <reason>", "governor:
// <reason>", "loop on item <id>", "decision log failed verification: <why>" or "max_cycles".
type MissionSummary struct {
	MissionID     string
	Completed     bool
	Cycles        int
	ItemsDone     int
	ItemsTotal    int
	TotalUSD      float64
	HeadSHA       string
	StoppedReason string
	TraceJSONL    string
}

// BuildMeter is a fresh ledger + governor from settings (unpriced cost denied unless opted in).
func BuildMeter(settings *config.Settings) *governor.CostMeter {
	gov := governor.NewBudgetGovernor(settings.BudgetUSDCeiling, settings.MaxCycles, settings.AllowUnpricedModels)
	return governor.NewCostMeter(governor.NewCostLedger(), gov)
}

// NewID mints a fresh random id like "mission_3f9a1c0b2d4e" (python: lha.ids.new_id).
func NewID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// CloseProvider closes a provider that owns resources (an HTTP client); no-op otherwise.
func CloseProvider(ctx context.Context, p contracts.ModelProvider) error {
	switch c := p.(type) {
	case nil:
		return nil
	case interface{ Close(context.Context) error }:
		return c.Close(ctx)
	case io.Closer:
		return c.Close()
	}
	return nil
}

// RunOptions are the inputs of RunMissionLocal.
type RunOptions struct {
	Workdir     string
	Title       string
	Description string
	Checklist   contracts.Checklist
	// Checks are the gating verification checks (nil => verify.DefaultPythonChecks()).
	Checks   []contracts.Check
	Settings *config.Settings // nil => config.Load()
	// AnchorText is optional extra context recited every cycle.
	AnchorText  string
	AllowEgress *bool
	// Meter shares one budget/ledger with other callers (e.g. the Planner); nil => BuildMeter.
	Meter *governor.CostMeter
	// Model overrides the lead provider (wrapped with the meter either way; not closed here).
	Model contracts.ModelProvider
	// OpenToolbox opens the sandbox session + dispatcher (required).
	OpenToolbox        ToolboxOpener
	ApproveInteractive bool
	// References are vendored reference paths recited every cycle.
	References []string
	// Preflight validates the run's tool configuration (Rule of Two, web settings) before
	// anything is opened; nil skips it (the toolbox opener must then refuse unsafe runs).
	Preflight func(*config.Settings) error
	// Recorder collects the trace; nil => a fresh obs.TraceRecorder.
	Recorder *obs.TraceRecorder
}

func loadSettings(s *config.Settings) (*config.Settings, error) {
	if s != nil {
		return s, nil
	}
	return config.Load()
}

// LeadEngineError was returned for LHA_LEAD_ENGINE=claude_code before the Go port had the
// engine; nothing returns it now.
//
// Deprecated: LHA_LEAD_ENGINE=claude_code is supported (LeadEngine).
var LeadEngineError = errors.New("LHA_LEAD_ENGINE=claude_code is not available")

// LeadVerifier is the lead's verifier (python: lead_verifier): sandbox checks in the sandbox,
// operator trusted: checks on the trusted runner, and a failing gating check re-run up to
// LHA_FLAKY_RETRIES times with proven flakes quarantined (verify.FlakyRetryVerifier).
func LeadVerifier(workdir string, settings *config.Settings) *verify.FlakyRetryVerifier {
	return leadVerifierWith(workdir, settings, trustedRunnerFor(settings))
}

// trustedRunnerFor is the trusted-check runner with the operator's LHA_TRUSTED_CHECK_ENV
// allow-list. An invalid allow-list (already refused by BuildLeadLoop) falls back to the most
// restrictive runner, which passes no operator variables through.
func trustedRunnerFor(settings *config.Settings) *verify.CommandTrustedRunner {
	if names, err := settings.TrustedCheckEnvNames(); err == nil {
		if runner, err := verify.NewCommandTrustedRunnerWithEnv(names); err == nil {
			return runner
		}
	}
	return verify.NewCommandTrustedRunner()
}

func leadVerifierWith(workdir string, settings *config.Settings, runner *verify.CommandTrustedRunner) *verify.FlakyRetryVerifier {
	return verify.NewFlakyRetryVerifier(
		verify.NewTrustedAwareVerifier(verify.NewDeterministicVerifier(), runner, workdir),
		settings.FlakyRetries, workdir)
}

// BuildLeadLoop is the lead's AgentLoop with every capability wired from settings (python:
// build_lead_loop): the LeadVerifier (sandbox checks in the sandbox, trusted: checks on the
// host, flaky re-runs and quarantine), operator-protected harness globs, the trusted-check map,
// the replanner and the lead engine (the built-in turn loop, or LHA_LEAD_ENGINE=claude_code: see
// LeadEngine).
func BuildLeadLoop(settings *config.Settings, leadModel contracts.ModelProvider, anchor *state.GitMissionAnchor, dispatcher contracts.ToolDispatcher, recorder *obs.TraceRecorder) (*AgentLoop, error) {
	return buildLeadLoop(settings, leadModel, anchor, dispatcher, recorder, false)
}

// BuildGuardedLeadLoop is BuildLeadLoop for a dispatcher wrapped in the orchestrator's
// file-ownership guard (python: build_lead_loop(dispatcher=...)): a native claude_code session
// would bypass the guard, so it is refused.
func BuildGuardedLeadLoop(settings *config.Settings, leadModel contracts.ModelProvider, anchor *state.GitMissionAnchor, dispatcher contracts.ToolDispatcher, recorder *obs.TraceRecorder) (*AgentLoop, error) {
	return buildLeadLoop(settings, leadModel, anchor, dispatcher, recorder, true)
}

func buildLeadLoop(settings *config.Settings, leadModel contracts.ModelProvider, anchor *state.GitMissionAnchor, dispatcher contracts.ToolDispatcher, recorder *obs.TraceRecorder, guarded bool) (*AgentLoop, error) {
	engine, err := LeadEngine(settings, guarded)
	if err != nil {
		return nil, err
	}
	trusted, err := settings.TrustedCheckCommands()
	if err != nil {
		return nil, err
	}
	envAllow, err := settings.TrustedCheckEnvNames()
	if err != nil {
		return nil, err
	}
	runner, err := verify.NewCommandTrustedRunnerWithEnv(envAllow)
	if err != nil {
		return nil, err
	}
	opts := DefaultLoopOptions()
	opts.Model = leadModel
	opts.Dispatcher = dispatcher
	opts.Verifier = leadVerifierWith(anchor.Workdir(), settings, runner)
	opts.Anchor = anchor
	opts.Recorder = recorder
	opts.MaxTurns = settings.MaxTurnsPerCycle
	opts.TrustedChecks = trusted
	opts.HarnessGlobs = settings.HarnessGlobs()
	if settings.MaxReplans > 0 {
		opts.Replanner = agents.NewReplanner(leadModel)
	}
	opts.MaxReplans = settings.MaxReplans
	opts.MaxSplitDepth = settings.MaxSplitDepth
	opts.Engine = engine
	return NewAgentLoop(opts), nil
}

// RunMissionLocal initializes the anchor and runs cycles until done / deadlocked / over-budget /
// looping. A *governor.BudgetExceeded or a *state.DecisionChainError from a cycle ends the run
// with a StoppedReason (not an error); any other failure is returned as an error.
func RunMissionLocal(ctx context.Context, o RunOptions) (MissionSummary, error) {
	settings, err := loadSettings(o.Settings)
	if err != nil {
		return MissionSummary{}, err
	}
	if o.Preflight != nil {
		if err := o.Preflight(settings); err != nil {
			return MissionSummary{}, err
		}
	}
	if o.OpenToolbox == nil {
		return MissionSummary{}, ErrExecutionNotLinked
	}
	recorder := o.Recorder
	if recorder == nil {
		recorder = obs.NewTraceRecorder(nil)
	}
	meter := o.Meter
	if meter == nil {
		meter = BuildMeter(settings)
	}
	loopDetector := governor.NewLoopDetector(settings.StallLimit)
	checks := o.Checks
	if checks == nil {
		checks = verify.DefaultPythonChecks()
	}
	anchor := state.NewGitMissionAnchor(o.Workdir)
	toolbox, err := o.OpenToolbox(ctx, ToolboxRequest{
		Settings: settings, Workdir: o.Workdir, Anchor: anchor,
		AllowEgress: o.AllowEgress, ApproveInteractive: o.ApproveInteractive,
	})
	if err != nil {
		return MissionSummary{}, err
	}
	missionID := NewID("mission")
	summary, runErr := runCycles(ctx, o, settings, meter, loopDetector, checks, anchor, toolbox, recorder, missionID)
	closeErr := toolbox.Close(context.WithoutCancel(ctx))
	if runErr != nil {
		return MissionSummary{}, runErr
	}
	if closeErr != nil {
		return MissionSummary{}, closeErr
	}
	return summary, nil
}

func runCycles(ctx context.Context, o RunOptions, settings *config.Settings, meter *governor.CostMeter, detector *governor.LoopDetector, checks []contracts.Check, anchor *state.GitMissionAnchor, toolbox Toolbox, recorder *obs.TraceRecorder, missionID string) (MissionSummary, error) {
	lead := o.Model
	if lead == nil {
		built, err := model.BuildProvider(settings, "", nil)
		if err != nil {
			return MissionSummary{}, err
		}
		// A provider built here owns its HTTP client and is closed here; a caller's is not.
		defer CloseProvider(context.WithoutCancel(ctx), built)
		lead = built
	}
	if _, err := anchor.InitializeSpec(ctx, contracts.MissionSpec{
		Title: o.Title, Description: o.Description, References: o.References,
	}, o.Checklist); err != nil {
		return MissionSummary{}, err
	}
	if o.AllowEgress != nil && *o.AllowEgress && len(settings.WebHosts()) == 0 {
		return MissionSummary{}, errors.New("allow_egress needs LHA_WEB_ALLOW_HOSTS (the hosts fetch_url may read)")
	}
	loop, err := BuildLeadLoop(settings, meter.Wrap(lead, "lead"), anchor, toolbox.Dispatcher(), recorder)
	if err != nil {
		return MissionSummary{}, err
	}
	tctx := contracts.ToolContext{MissionID: missionID, Session: toolbox.Session()}

	cycles := 0
	lastHead := ""
	stopped := "max_cycles"
cycleLoop:
	for cycles < settings.MaxCycles {
		decision := meter.Governor.AuthorizeNext(meter.Ledger, cycles, nil)
		if !decision.Allow {
			recorder.Record("governor_block", missionID, "", obs.F("reason", decision.Reason))
			stopped = "governor: " + decision.Reason
			break
		}
		cycleID := fmt.Sprintf("c%d", cycles+1)
		meter.SetCycleID(cycleID)
		outcome, err := loop.RunCycle(ctx, tctx, missionID, cycleID, o.AnchorText, checks)
		if err != nil {
			var budget *governor.BudgetExceeded
			var chain *state.DecisionChainError
			switch {
			case errors.As(err, &budget):
				recorder.Record("governor_block", missionID, "", obs.F("reason", err.Error()))
				stopped = "governor: " + budget.Decision.Reason
			case errors.As(err, &chain):
				// Altered decision history: refuse to continue.
				recorder.Record("decision_chain_invalid", missionID, "", obs.F("reason", err.Error()))
				stopped = DecisionChainStop + ": " + err.Error()
			default:
				return MissionSummary{}, err
			}
			break cycleLoop
		}
		if outcome.Advanced {
			cycles++
		}
		if outcome.HeadSHA != "" {
			lastHead = outcome.HeadSHA
		}
		if outcome.IsComplete {
			stopped = "complete"
			break
		}
		if outcome.IsDeadlocked || !outcome.Advanced {
			reason := outcome.Reason
			if reason == "" {
				reason = "no actionable item"
			}
			recorder.Record("deadlocked", missionID, "", obs.F("reason", reason))
			stopped = "deadlocked: " + reason
			break
		}
		// Stop if the same item keeps failing verification (consecutive; reset on success).
		if detector.Observe(outcome.ItemID+":failed", !outcome.Verified) {
			recorder.Record("loop_detected", missionID, "", obs.F("item_id", outcome.ItemID))
			stopped = "loop on item " + outcome.ItemID
			break
		}
	}
	final, err := anchor.ReadChecklist(ctx)
	if err != nil {
		return MissionSummary{}, err
	}
	trace, err := recorder.ToJSONL()
	if err != nil {
		return MissionSummary{}, err
	}
	return MissionSummary{
		MissionID:     missionID,
		Completed:     final.IsComplete(),
		Cycles:        cycles,
		ItemsDone:     final.ItemsDone(),
		ItemsTotal:    final.ItemsTotal(),
		TotalUSD:      meter.Ledger.TotalUSD(),
		HeadSHA:       lastHead,
		StoppedReason: stopped,
		TraceJSONL:    trace,
	}, nil
}

// PlanOptions are the inputs of PlanAndRunLocal (the run options minus the checklist).
type PlanOptions struct {
	RunOptions
	// Task is the mission / task description the Planner decomposes.
	Task string
	// PlannerModel overrides the planner's provider (nil => model.BuildProvider(settings); a
	// Go-only hook for tests, like RunOptions.Model).
	PlannerModel contracts.ModelProvider
}

// PlanAndRunLocal is mission intake -> execution: the Planner decomposes Task into a checklist,
// then the mission runs. Planner and lead share one CostMeter; a *governor.BudgetExceeded from
// the planning call is returned as an error (nothing has run yet).
func PlanAndRunLocal(ctx context.Context, o PlanOptions) (MissionSummary, error) {
	settings, err := loadSettings(o.Settings)
	if err != nil {
		return MissionSummary{}, err
	}
	if o.Preflight != nil { // fail before the planning call spends anything
		if err := o.Preflight(settings); err != nil {
			return MissionSummary{}, err
		}
	}
	meter := o.Meter
	if meter == nil {
		meter = BuildMeter(settings)
	}
	plannerModel := o.PlannerModel
	if plannerModel == nil {
		built, err := model.BuildProvider(settings, "", nil)
		if err != nil {
			return MissionSummary{}, err
		}
		plannerModel = built
		defer CloseProvider(context.WithoutCancel(ctx), built)
	}
	checklist, err := agents.NewPlanner(meter.Wrap(plannerModel, "planner")).Plan(ctx, o.Title, o.Task, "")
	if err != nil {
		return MissionSummary{}, err
	}
	run := o.RunOptions
	run.Settings = settings
	run.Meter = meter
	run.Description = o.Task
	run.Checklist = checklist
	return RunMissionLocal(ctx, run)
}
