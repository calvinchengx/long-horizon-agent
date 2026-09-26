package agents

import (
	"net/http"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// The org chart as data (python: lha.agents.roles): one RoleSpec per specialized agent — its
// model tier (Opus for high-leverage judgment, Sonnet for mid, Haiku for cheap search), system
// prompt and tool policy (can it mutate files? reach the network?).

// ModelTier is a role's model tier.
type ModelTier string

// Model tiers.
const (
	TierOpus   ModelTier = "opus"
	TierSonnet ModelTier = "sonnet"
	TierHaiku  ModelTier = "haiku"
)

// RoleSpec is one role of the organization.
type RoleSpec struct {
	Name          string
	Tier          ModelTier
	SystemPrompt  string
	AllowMutating bool
	AllowEgress   bool
	MaxTurns      int
}

// Roles is the org chart (python: ROLES).
var Roles = map[string]RoleSpec{
	"planner": {Name: "planner", Tier: TierOpus, MaxTurns: 8, SystemPrompt: PlannerSystemPrompt},
	"lead": {Name: "lead", Tier: TierOpus, MaxTurns: 8, AllowMutating: true, SystemPrompt: "You are the Lead Engineer and the SOLE writer to the integration line. Keep design " +
		"decisions coherent. Use tools to read, edit, run, and verify. Mark an item done only " +
		"when the deterministic checks are green."},
	"researcher": {Name: "researcher", Tier: TierHaiku, MaxTurns: 8, AllowEgress: true, SystemPrompt: "You are a Researcher. Investigate read-only: search the codebase, read docs, and " +
		"(if permitted) the web. Return a concise ~1-2K-token brief. Never write code."},
	"reviewer": {Name: "reviewer", Tier: TierOpus, MaxTurns: 8, SystemPrompt: "You are an independent Reviewer with NO shared context with the author. Review the " +
		"diff adversarially for correctness, security, and scope. Return blocking vs advisory " +
		"findings anchored to evidence."},
	"implementer": {Name: "implementer", Tier: TierSonnet, MaxTurns: 8, AllowMutating: true, SystemPrompt: "You are an Implementer working a single file-disjoint slice in your own worktree. " +
		"Write only files in your assigned write-set; if you need a foreign file, stop and " +
		"request a lease instead of writing it."},
}

var claudeByTier = map[ModelTier]string{
	TierOpus:   "claude-opus-4-8",
	TierSonnet: "claude-sonnet-4-6",
	TierHaiku:  "claude-haiku-4-5-20251001",
}

// ClaudeModelFor maps a role's model tier to a concrete Claude model id.
func ClaudeModelFor(tier ModelTier) string { return claudeByTier[tier] }

// ModelForRole is a model tuned to roleName's tier on the Claude backend, or the configured
// model on every other backend (python: lha.agents.router.model_for_role). Pass one shared
// client for every role so all providers reuse one connection pool (the caller owns it).
func ModelForRole(roleName string, settings *config.Settings, client *http.Client) (contracts.ModelProvider, error) {
	if settings == nil {
		s, err := config.Load()
		if err != nil {
			return nil, err
		}
		settings = s
	}
	if role, ok := Roles[roleName]; ok && settings.ModelBackend == "claude" {
		return model.BuildProvider(settings, ClaudeModelFor(role.Tier), client)
	}
	return model.BuildProvider(settings, "", client)
}
