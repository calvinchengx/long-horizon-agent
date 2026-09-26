package tools

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
)

// Run tool assembly: the web tools, their egress policy and the run-level Rule of Two
// (python/src/lha/execution/tools/toolset.py). Every run path builds its dispatcher with
// BuildRunDispatcher, so the web tools and the Rule of Two are wired identically everywhere:
//
//   - Web tools are opt-in. fetch_url is registered only when LHA_WEB_ALLOW_HOSTS (plus any
//     per-run --allow-host) is non-empty; web_search additionally needs LHA_WEB_SEARCH_PROVIDER
//     and LHA_WEB_SEARCH_API_KEY.
//   - fetch_url gets an EgressPolicy over exactly the allow-list, the public-address resolver
//     check, per-hop redirect re-checks, the configured size/timeout limits and a
//     CredentialBroker built from LHA_WEB_CREDENTIALS whose secrets are bound to allow-listed
//     hosts only.
//   - Rule of Two (fail closed): web tools give the run UNTRUSTED_CONTENT and EXTERNAL_COMMS. The
//     run holds PRIVATE_DATA when LHA_PRIVATE_DATA=true or the sandbox is local. All three
//     together are refused before anything runs — even if a human gate is configured.

// DefaultLocalTools is the non-egress tool set (file IO + shell) every mission gets by default.
// record_decision is not in it: it is bound to a mission's decision sink (WithDecisionTool).
func DefaultLocalTools() []contracts.Tool {
	return []contracts.Tool{ReadFileTool{}, WriteFileTool{}, ListFilesTool{}, GrepTool{}, ShellTool{}}
}

// WebConfigError reports inconsistent web/egress settings (e.g. a credential bound to a host
// outside the allow-list).
type WebConfigError struct{ Msg string }

func (e *WebConfigError) Error() string { return e.Msg }

// PyTypeName names the Python exception type.
func (e *WebConfigError) PyTypeName() string { return "WebConfigError" }

// RuleOfTwoViolation is the run-level refusal; it wraps safety.ErrRuleOfTwoViolation.
type RuleOfTwoViolation struct{ Msg string }

func (e *RuleOfTwoViolation) Error() string { return e.Msg }

// Unwrap exposes safety.ErrRuleOfTwoViolation to errors.Is.
func (e *RuleOfTwoViolation) Unwrap() error { return safety.ErrRuleOfTwoViolation }

// PyTypeName names the Python exception type.
func (e *RuleOfTwoViolation) PyTypeName() string { return "RuleOfTwoViolation" }

// WebIO are the network seams of the web tools (tests inject a fake resolver + transport).
type WebIO struct {
	Resolver  safety.Resolver   // nil = safety.SystemResolver
	Transport http.RoundTripper // nil = the default public-only transport
}

// DefaultWebIO is the seam every run path uses; tests replace it (never real network in tests).
var DefaultWebIO = &WebIO{}

// EgressHosts is the normalized egress allow-list (invalid hosts dropped), sorted.
func EgressHosts(s *config.Settings) []string {
	set := map[string]bool{}
	for _, raw := range s.WebHosts() {
		if h := safety.NormalizeHost(raw); h != "" {
			set[h] = true
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// WebEnabled reports whether web tools are registered (the egress allow-list is non-empty).
func WebEnabled(s *config.Settings) bool { return len(EgressHosts(s)) > 0 }

// WithAllowHosts returns a copy of s with hosts added to the egress allow-list (per-run
// --allow-host); s itself is returned when there is nothing to add.
func WithAllowHosts(s *config.Settings, hosts []string) *config.Settings {
	var extra []string
	for _, h := range hosts {
		if t := strings.TrimSpace(h); t != "" {
			extra = append(extra, t)
		}
	}
	if len(extra) == 0 {
		return s
	}
	seen := map[string]bool{}
	var merged []string
	for _, h := range append(s.WebHosts(), extra...) {
		if !seen[h] {
			seen[h] = true
			merged = append(merged, h)
		}
	}
	out := *s
	out.WebAllowHosts = strings.Join(merged, ",")
	return &out
}

// RunCapabilities is the Rule-of-Two capability set of a run under s (web tools enabled iff
// WebEnabled), sorted.
func RunCapabilities(s *config.Settings) []safety.Capability {
	return runCapabilities(s, WebEnabled(s))
}

func runCapabilities(s *config.Settings, web bool) []safety.Capability {
	var caps []safety.Capability
	if s.PrivateData || s.Sandbox == "local" {
		caps = append(caps, safety.PrivateData)
	}
	if web || s.SandboxEgressEnabled() {
		caps = append(caps, safety.ExternalComms, safety.UntrustedContent)
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
	return caps
}

// CheckRunRuleOfTwo fails closed (*RuleOfTwoViolation) if the run would hold untrusted content +
// private data + external comms. Web tools and sandbox egress each bring untrusted content and
// external comms.
func CheckRunRuleOfTwo(s *config.Settings) error {
	if err := safety.CheckRuleOfTwo(RunCapabilities(s)...); err != nil {
		why := "LHA_PRIVATE_DATA=true declares the workspace holds secrets or customer data"
		if s.Sandbox == "local" {
			why = "LHA_SANDBOX=local runs the agent's shell on this host (host files, credentials " +
				"and network)"
		}
		var web, sandbox []string
		if WebEnabled(s) {
			web = EgressHosts(s)
		}
		if s.SandboxEgressEnabled() {
			sandbox = s.SandboxEgressEntries()
		}
		return &RuleOfTwoViolation{Msg: safety.RunRefusal(web, sandbox, why, err)}
	}
	return nil
}

type orderedEntry struct {
	key   string
	value json.RawMessage
}

// decodeOrderedObject decodes a top-level JSON object keeping key order (last value wins, at the
// first key's position, like a Python dict).
func decodeOrderedObject(raw string) ([]orderedEntry, bool, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, false, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		// Not an object: still validate the whole document.
		if _, err := decodePyJSON(raw); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	var entries []orderedEntry
	index := map[string]int{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false, err
		}
		key, _ := keyTok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false, err
		}
		if i, ok := index[key]; ok {
			entries[i].value = value
			continue
		}
		index[key] = len(entries)
		entries = append(entries, orderedEntry{key, value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, false, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false, errors.New("Extra data")
	}
	return entries, true, nil
}

// NewCredentialBroker builds the broker from LHA_WEB_CREDENTIALS; each binding must be
// allow-listed. It returns the broker and {placeholder: hosts} (safe to show the model: no
// secrets). Errors are *WebConfigError.
func NewCredentialBroker(s *config.Settings, hosts []string) (*safety.CredentialBroker, map[string][]string, error) {
	broker := safety.NewCredentialBroker()
	shown := map[string][]string{}
	if s.WebCredentials == nil {
		return broker, shown, nil
	}
	raw := s.WebCredentials.Value()
	if strings.TrimSpace(raw) == "" {
		return broker, shown, nil
	}
	entries, isObject, err := decodeOrderedObject(raw)
	if err != nil {
		// Only the message: the secret-bearing document never lands in a trace.
		return nil, nil, &WebConfigError{Msg: "LHA_WEB_CREDENTIALS is not valid JSON: " + pyJSONMsg(raw, err)}
	}
	if !isObject {
		return nil, nil, &WebConfigError{Msg: "LHA_WEB_CREDENTIALS must be a JSON object"}
	}
	allowed := map[string]bool{}
	for _, h := range hosts {
		allowed[h] = true
	}
	for _, e := range entries {
		name := contracts.PyRepr(e.key)
		var entry map[string]any
		if err := json.Unmarshal(e.value, &entry); err != nil || entry == nil {
			return nil, nil, &WebConfigError{Msg: "credential " + name + " must be an object"}
		}
		value, ok := entry["value"].(string)
		if !ok || value == "" {
			return nil, nil, &WebConfigError{Msg: "credential " + name + " needs a non-empty 'value'"}
		}
		list, ok := entry["hosts"].([]any)
		if !ok || len(list) == 0 {
			return nil, nil, &WebConfigError{Msg: "credential " + name + " needs a non-empty 'hosts' list"}
		}
		normalized := map[string]bool{}
		for _, h := range list {
			hs, isStr := h.(string)
			if !isStr {
				return nil, nil, &WebConfigError{Msg: "credential " + name + " needs a non-empty 'hosts' list"}
			}
			normalized[safety.NormalizeHost(hs)] = true
		}
		var outside []any
		var bound []string
		for h := range normalized {
			if !allowed[h] {
				if h == "" {
					h = "<invalid>"
				}
				outside = append(outside, h)
			}
			bound = append(bound, h)
		}
		if len(outside) > 0 {
			sort.Slice(outside, func(i, j int) bool { return outside[i].(string) < outside[j].(string) })
			return nil, nil, &WebConfigError{Msg: "credential " + name +
				" is bound to hosts outside the egress allow-list: " + pyval.Repr(outside)}
		}
		sort.Strings(bound)
		if err := broker.Register(e.key, value, bound...); err != nil {
			return nil, nil, pyval.NewError("ValueError", err.Error())
		}
		shown[e.key] = bound
	}
	return broker, shown, nil
}

// WebTools are the web tools for s (none when the allow-list is empty).
func WebTools(s *config.Settings, webIO *WebIO) ([]contracts.Tool, error) {
	hosts := EgressHosts(s)
	if len(hosts) == 0 {
		return nil, nil
	}
	if webIO == nil {
		webIO = DefaultWebIO
	}
	broker, placeholders, err := NewCredentialBroker(s, hosts)
	if err != nil {
		return nil, err
	}
	ports, err := s.WebPorts()
	if err != nil {
		return nil, pyval.NewError("ValueError", err.Error())
	}
	tools := []contracts.Tool{NewFetchURLTool(FetchURLOptions{
		Policy:           &safety.EgressPolicy{AllowHosts: hosts, AllowPorts: ports},
		Broker:           broker,
		Resolver:         webIO.Resolver,
		Transport:        webIO.Transport,
		TimeoutS:         s.WebTimeoutS,
		MaxResponseBytes: s.WebMaxResponseBytes,
		Placeholders:     placeholders,
	})}
	key := s.WebSearchAPIKey.Value()
	if s.WebSearchProvider != nil && *s.WebSearchProvider != "" && key != "" {
		endpoint := ""
		if s.WebSearchEndpoint != nil {
			endpoint = *s.WebSearchEndpoint
		}
		search, err := NewWebSearchTool(WebSearchOptions{
			APIKey:           key,
			Provider:         *s.WebSearchProvider,
			Endpoint:         endpoint,
			Resolver:         webIO.Resolver,
			Transport:        webIO.Transport,
			TimeoutS:         s.WebTimeoutS,
			MaxResponseBytes: s.WebMaxResponseBytes,
		})
		if err != nil {
			return nil, &WebConfigError{Msg: "invalid LHA_WEB_SEARCH_ENDPOINT: " + err.Error()}
		}
		tools = append(tools, search)
	}
	return tools, nil
}

// PreflightRunTools validates a run's tool configuration up front (before any planning spend):
// *RuleOfTwoViolation (lethal trifecta), a ValueError naming a host in the wrong sandbox egress
// list, or *WebConfigError (bad web settings).
func PreflightRunTools(s *config.Settings) error {
	if err := CheckRunRuleOfTwo(s); err != nil {
		return err
	}
	if s.SandboxEgressEnabled() {
		if _, err := s.SandboxEgressHosts(); err != nil {
			return err
		}
	}
	_, err := WebTools(s, nil)
	return err
}

// RunTools are the default local tools, plus the web tools when web and the allow-list is set.
func RunTools(s *config.Settings, web bool, webIO *WebIO) ([]contracts.Tool, error) {
	tools := DefaultLocalTools()
	if !web {
		return tools, nil
	}
	extra, err := WebTools(s, webIO)
	if err != nil {
		return nil, err
	}
	return append(tools, extra...), nil
}

// RunDispatcherOptions are BuildRunDispatcher's options.
type RunDispatcherOptions struct {
	AllowMutating bool
	// DisableEgress forces the web tools off for this dispatcher (e.g. a role without egress);
	// otherwise they are on iff the allow-list is non-empty.
	DisableEgress bool
	Gate          contracts.HITLGate
	IO            *WebIO
}

// BuildRunDispatcher is the dispatcher for one agent of a run: local tools (+ web tools when
// enabled). The run-level Rule of Two is checked with the RUN's capabilities (a run's agents share
// briefs, so untrusted content read by one reaches the others).
func BuildRunDispatcher(s *config.Settings, opts RunDispatcherOptions) (*execution.AllowListDispatcher, error) {
	if err := CheckRunRuleOfTwo(s); err != nil {
		return nil, err
	}
	useWeb := WebEnabled(s) && !opts.DisableEgress
	tools, err := RunTools(s, useWeb, opts.IO)
	if err != nil {
		return nil, err
	}
	var declared []safety.Capability
	for _, c := range RunCapabilities(s) {
		if c == safety.PrivateData {
			declared = append(declared, c)
		}
	}
	return execution.ForTools(tools, execution.DispatcherOptions{
		AllowMutating: opts.AllowMutating,
		AllowEgress:   useWeb,
		Gate:          opts.Gate,
		Capabilities:  declared,
	})
}
