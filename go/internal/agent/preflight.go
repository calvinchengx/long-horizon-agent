package agent

import (
	"errors"
	"sort"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
)

// The run-level Rule of Two (python: lha.execution.tools.toolset.check_run_rule_of_two). Web
// tools give a run UNTRUSTED_CONTENT and EXTERNAL_COMMS; it holds PRIVATE_DATA when
// LHA_PRIVATE_DATA=true or the sandbox is local. All three together fail closed before anything
// runs. Validating the rest of the web settings (ports, credentials, search provider) belongs to
// the execution layer that builds the web tools.

// RuleOfTwoViolation is a run that would hold the lethal trifecta.
type RuleOfTwoViolation struct{ Message string }

func (e *RuleOfTwoViolation) Error() string { return e.Message }

// Unwrap lets errors.Is(err, safety.ErrRuleOfTwoViolation) match.
func (e *RuleOfTwoViolation) Unwrap() error { return safety.ErrRuleOfTwoViolation }

// EgressHosts is the normalized web egress allow-list (invalid hosts dropped), sorted.
func EgressHosts(settings *config.Settings) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range settings.WebHosts() {
		if h := safety.NormalizeHost(raw); h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// WebEnabled reports whether the web tools are registered (a non-empty allow-list).
func WebEnabled(settings *config.Settings) bool { return len(EgressHosts(settings)) > 0 }

// CheckRunRuleOfTwo fails closed if the run would hold untrusted content + private data +
// external comms.
func CheckRunRuleOfTwo(settings *config.Settings) error {
	caps := []safety.Capability{}
	if WebEnabled(settings) {
		caps = append(caps, safety.UntrustedContent, safety.ExternalComms)
	}
	if settings.PrivateData || settings.Sandbox == "local" {
		caps = append(caps, safety.PrivateData)
	}
	err := safety.CheckRuleOfTwo(caps...)
	if err == nil {
		return nil
	}
	if !errors.Is(err, safety.ErrRuleOfTwoViolation) {
		return err
	}
	why := "LHA_PRIVATE_DATA=true declares the workspace holds secrets or customer data"
	if settings.Sandbox == "local" {
		why = "LHA_SANDBOX=local runs the agent's shell on this host (host files, credentials " +
			"and network)"
	}
	return &RuleOfTwoViolation{Message: "refusing to start: web tools are enabled (egress allow-list: " +
		strings.Join(EgressHosts(settings), ", ") + "), which brings untrusted content and " +
		"external comms, and " + why + ". " + err.Error() + ". Use a docker/e2b sandbox without private data, " +
		"or clear the allow-list (LHA_WEB_ALLOW_HOSTS / --allow-host)."}
}

// WithAllowHosts is settings with hosts added to the egress allow-list (per-run --allow-host);
// settings itself is not modified.
func WithAllowHosts(settings *config.Settings, hosts []string) *config.Settings {
	extra := []string{}
	for _, h := range hosts {
		if h = pyfmt.PyStrip(h); h != "" {
			extra = append(extra, h)
		}
	}
	if len(extra) == 0 {
		return settings
	}
	merged := []string{}
	seen := map[string]bool{}
	for _, h := range append(settings.WebHosts(), extra...) {
		if !seen[h] {
			seen[h] = true
			merged = append(merged, h)
		}
	}
	out := settings.Clone()
	out.WebAllowHosts = strings.Join(merged, ",")
	return out
}
