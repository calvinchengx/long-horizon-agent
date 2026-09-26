package org

import (
	"context"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/tools"
)

// Assembling the organization's agents from settings, the same way for every role (python:
// lha.agent.assembly open_lead_sandbox / lead_dispatcher / lead_verifier).

// OpenLeadSandbox opens the sandbox from settings for workdir: kind (LHA_SANDBOX), image and the
// sandbox egress allow-list. A 'local' sandbox without the opt-in is an
// *execution.UnsafeSandboxError.
func OpenLeadSandbox(ctx context.Context, settings *config.Settings, workdir string) (contracts.SandboxSession, error) {
	// The egress allow-list is split by what a host accepts; a host in the wrong list is refused.
	egressHosts, err := settings.SandboxEgressHosts()
	if err != nil {
		return nil, err
	}
	sandbox, err := execution.BuildSandbox(settings.Sandbox, execution.SandboxOptions{
		AllowUnsafeLocal: settings.AllowUnsafeLocal,
		Image:            settings.SandboxImage,
		EgressHosts:      egressHosts,
		Memory:           settings.SandboxMemory,
		CPUs:             settings.SandboxCPUs,
		TmpSize:          settings.SandboxTmpSize,
	})
	if err != nil {
		return nil, err
	}
	return sandbox.Open(ctx, workdir, "")
}

// LeadDispatcher is every lead tool, mutating allowed; egress only for the web tools'
// allow-list (allowEgress nil => web tools iff LHA_WEB_ALLOW_HOSTS is set; false drops them).
// Irreversible commands go to gate (nil: refused). A lethal-trifecta run is a
// *tools.RuleOfTwoViolation.
func LeadDispatcher(settings *config.Settings, gate contracts.HITLGate, allowEgress *bool) (contracts.ToolDispatcher, error) {
	return RunDispatcher(settings, true, gate, allowEgress)
}

// RunDispatcher is one agent's dispatcher (python: build_run_dispatcher): the local tools, plus
// the web tools under the egress policy unless allowEgress is false.
func RunDispatcher(settings *config.Settings, allowMutating bool, gate contracts.HITLGate, allowEgress *bool) (contracts.ToolDispatcher, error) {
	var hitlGate contracts.HITLGate
	if gate != nil {
		hitlGate = gate
	}
	return tools.BuildRunDispatcher(settings, tools.RunDispatcherOptions{
		AllowMutating: allowMutating,
		DisableEgress: allowEgress != nil && !*allowEgress,
		Gate:          hitlGate,
	})
}

// LeadVerifier is the lead's verifier for a workdir (python: lead_verifier): sandbox checks in the
// sandbox, operator trusted: checks on the host with the LHA_TRUSTED_CHECK_ENV allow-list, and the
// LHA_FLAKY_RETRIES re-run/quarantine wrapper. Implementer worktrees and branch integration use
// it exactly as the Lead does.
func LeadVerifier(workdir string, settings *config.Settings) contracts.Verifier {
	return agent.LeadVerifier(workdir, settings)
}
