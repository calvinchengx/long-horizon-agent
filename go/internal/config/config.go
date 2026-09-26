// Package config is the Go mirror of python/src/lha/config.py: one Settings value populated from
// LHA_* environment variables and an optional .env file (environment variables win), with the same
// names, declaration order, defaults and validation, so one environment configures either
// implementation (and `lha config` prints the same lines from both).
package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/egressproxy"
)

// Redacted is what secret values display as.
const Redacted = "***"

// Secret is a string that never prints its value (python: SecretStr).
type Secret struct{ value string }

// NewSecret wraps a raw secret value.
func NewSecret(v string) *Secret { return &Secret{value: v} }

// Value returns the raw secret; read it only at the point of use.
func (s *Secret) Value() string {
	if s == nil {
		return ""
	}
	return s.value
}

func (s *Secret) String() string   { return "**********" }
func (s *Secret) GoString() string { return "**********" }

// Settings is the runtime configuration of an LHA deployment. Field tags name the env var suffix
// (`env`), the default (`default`; absent = optional, nil), allowed values (`choices`) and numeric
// bounds (`gt`, `ge`, `le`). Field order is python's declaration order (Redacted relies on it).
type Settings struct {
	// --- Model layer
	ModelBackend string `env:"model_backend" default:"stub" choices:"stub,ollama,openai_compat,claude,claude_code"`
	ModelName    string `env:"model_name" default:"stub-1"`

	OllamaBaseURL string  `env:"ollama_base_url" default:"http://localhost:11434"`
	OpenAIBaseURL *string `env:"openai_base_url"`
	OpenAIAPIKey  *Secret `env:"openai_api_key"`

	AnthropicAPIKey *Secret `env:"anthropic_api_key"`

	OpenAIPriceInPerMTok  *float64 `env:"openai_price_in_per_mtok"`
	OpenAIPriceOutPerMTok *float64 `env:"openai_price_out_per_mtok"`
	ClaudePriceInPerMTok  *float64 `env:"claude_price_in_per_mtok"`
	ClaudePriceOutPerMTok *float64 `env:"claude_price_out_per_mtok"`
	AllowUnpricedModels   bool     `env:"allow_unpriced_models" default:"false"`

	// --- Claude Code (claude -p)
	LeadEngine             string  `env:"lead_engine" default:"loop" choices:"loop,claude_code"`
	ClaudeCodeBin          string  `env:"claude_code_bin" default:"claude"`
	ClaudeCodeTools        string  `env:"claude_code_tools" default:"lha" choices:"lha,native"`
	ClaudeCodeMaxBudgetUSD float64 `env:"claude_code_max_budget_usd" default:"5.0" gt:"0"`
	ClaudeCodeTimeoutS     float64 `env:"claude_code_timeout_s" default:"3600.0" gt:"0"`

	// --- Durable control plane (Temporal)
	TemporalAddress   string `env:"temporal_address" default:"localhost:7233"`
	TemporalNamespace string `env:"temporal_namespace" default:"default"`
	TaskQueue         string `env:"task_queue" default:"lha-mission"`

	// --- Persistence
	PostgresDSN     *Secret `env:"postgres_dsn"`
	WorkspaceRoot   string  `env:"workspace_root" default:".lha/workspaces"`
	ObjectStoreRoot string  `env:"object_store_root" default:".lha/objects"`

	// --- Governor
	BudgetUSDCeiling float64 `env:"budget_usd_ceiling" default:"10.0"`
	MaxCycles        int     `env:"max_cycles" default:"1000"`
	MaxTurnsPerCycle int     `env:"max_turns_per_cycle" default:"8"`
	StallLimit       int     `env:"stall_limit" default:"5"`
	MaxReplans       int     `env:"max_replans" default:"20"`
	MaxSplitDepth    int     `env:"max_split_depth" default:"2"`
	ApprovalTimeoutS int     `env:"approval_timeout_s" default:"86400"`

	// --- Execution sandbox
	Sandbox          string `env:"sandbox" default:"docker" choices:"docker,e2b,local"`
	AllowUnsafeLocal bool   `env:"allow_unsafe_local" default:"false"`
	SandboxImage     string `env:"sandbox_image" default:"ghcr.io/astral-sh/uv:python3.12-bookworm-slim"`
	// Hosts the Docker sandbox may reach through its egress proxy, split by what a host lets
	// code in the sandbox do (egressproxy.SandboxAllowList): package-fetch download hosts only;
	// any other host except known push/upload hosts; push/upload hosts, acknowledged.
	SandboxEgress                string `env:"sandbox_egress" default:""`
	SandboxEgressExtraHosts      string `env:"sandbox_egress_extra_hosts" default:""`
	SandboxEgressAllowWriteHosts string `env:"sandbox_egress_allow_write_hosts" default:""`
	// Docker sandbox limits (memory incl. swap; CPUs) and the /tmp tmpfs size, which holds the
	// toolchain caches and counts against the memory limit.
	SandboxMemory  string  `env:"sandbox_memory" default:"2g"`
	SandboxCPUs    float64 `env:"sandbox_cpus" default:"2.0" gt:"0"`
	SandboxTmpSize string  `env:"sandbox_tmp_size" default:"1g"`
	WebAllowHosts  string  `env:"web_allow_hosts" default:""`
	TrustedChecks  string  `env:"trusted_checks" default:""`
	HarnessPaths   string  `env:"harness_paths" default:""`
	FlakyRetries   int     `env:"flaky_retries" default:"1" ge:"0" le:"5"`

	// --- Multi-agent coordination
	MaxParallelImplementers int `env:"max_parallel_implementers" default:"3"`

	// --- Observability. ``envalias`` names are also read WITHOUT the LHA_ prefix (the standard
	// OpenTelemetry variables); ``noprefix`` fields are read ONLY by their alias (python:
	// validation_alias=AliasChoices(...)). When both spellings are set the LHA_ one wins.
	OTelExporterOTLPEndpoint *string `env:"otel_exporter_otlp_endpoint" envalias:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	OTelSDKDisabled          bool    `env:"otel_sdk_disabled" default:"false" envalias:"OTEL_SDK_DISABLED" noprefix:"true"`
	OTelServiceName          string  `env:"otel_service_name" default:"lha"`
	OTelExportTimeoutS       int     `env:"otel_export_timeout_s" default:"5"`
	LangfuseHost             *string `env:"langfuse_host"`
	LangfusePublicKey        *string `env:"langfuse_public_key"`
	LangfuseSecretKey        *Secret `env:"langfuse_secret_key"`

	// --- Persistence backend + tiered memory
	SQLitePathSetting        string `env:"sqlite_path" default:""`
	PostgresFallbackToSQLite bool   `env:"postgres_fallback_to_sqlite" default:"true"`
	MemoryEnabled            bool   `env:"memory_enabled" default:"true"`
	MemoryPromptBudgetChars  int    `env:"memory_prompt_budget_chars" default:"4000"`
	MemoryEpisodicK          int    `env:"memory_episodic_k" default:"4"`
	MemorySemanticK          int    `env:"memory_semantic_k" default:"4"`
	MemorySkillsK            int    `env:"memory_skills_k" default:"2"`
	MemoryEmbedder           string `env:"memory_embedder" default:"hash" choices:"hash,sentence_transformers,ollama,none"`
	MemoryEmbeddingModel     string `env:"memory_embedding_model" default:""`
	MemoryRerank             string `env:"memory_rerank" default:"none" choices:"none,cross_encoder"`
	MemoryConsolidateEvery   int    `env:"memory_consolidate_every" default:"5"`
	MemoryConsolidation      string `env:"memory_consolidation" default:"extractive" choices:"extractive,model"`
	MemoryIndexRepoFiles     bool   `env:"memory_index_repo_files" default:"true"`
	MemoryMaxRepoFiles       int    `env:"memory_max_repo_files" default:"400"`

	// --- Web tools
	WebAllowPorts       string  `env:"web_allow_ports" default:""`
	WebCredentials      *Secret `env:"web_credentials"`
	WebSearchProvider   *string `env:"web_search_provider" choices:"tavily,exa"`
	WebSearchAPIKey     *Secret `env:"web_search_api_key"`
	WebSearchEndpoint   *string `env:"web_search_endpoint"`
	WebTimeoutS         float64 `env:"web_timeout_s" default:"30.0" gt:"0"`
	WebMaxResponseBytes int     `env:"web_max_response_bytes" default:"2000000" gt:"0"`
	PrivateData         bool    `env:"private_data" default:"false"`

	// --- Model resilience
	FallbackModels     string  `env:"fallback_models" default:""`
	FallbackMaxRounds  int     `env:"fallback_max_rounds" default:"2" ge:"1"`
	ModelProbeTimeoutS float64 `env:"model_probe_timeout_s" default:"10.0" gt:"0"`

	// --- Human gates
	ConsoleApprovalTimeoutS   int     `env:"console_approval_timeout_s" default:"3600" ge:"1"`
	GateEscalationSeconds     []int   `env:"gate_escalation_seconds" default:"[900, 2700, 14400, 43200]"`
	DeadlockGateDefault       string  `env:"deadlock_gate_default" default:"abort" choices:"abort,impossible"`
	ImpossibleAfterFailures   int     `env:"impossible_after_failures" default:"3" ge:"1"`
	CyclePauseSeconds         int     `env:"cycle_pause_seconds" default:"0" ge:"0"`
	GateWebhookURL            *Secret `env:"gate_webhook_url"`
	GateWebhookTimeoutSeconds float64 `env:"gate_webhook_timeout_seconds" default:"5.0" gt:"0" le:"60"`
}

// Load reads settings from the process environment and ./.env (if present).
func Load() (*Settings, error) { return LoadFrom(os.Environ(), ".env") }

// LoadFrom reads settings from environ (KEY=VALUE pairs) layered over the dotenv file (optional).
func LoadFrom(environ []string, dotenv string) (*Settings, error) {
	values := map[string]string{}
	aliases := map[string]string{} // upper-cased unprefixed env name -> value
	if dotenv != "" {
		if err := readDotenv(dotenv, values); err != nil {
			return nil, err
		}
	}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(strings.ToUpper(k), "LHA_") {
			values[strings.ToLower(k[4:])] = v // environment variables win over .env
		}
		if ok {
			aliases[strings.ToUpper(k)] = v
		}
	}
	s := &Settings{}
	rv := reflect.ValueOf(s).Elem()
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		name := f.Tag.Get("env")
		envName := "LHA_" + strings.ToUpper(name)
		raw, set := values[name]
		if f.Tag.Get("noprefix") == "true" {
			raw, set = "", false
		}
		if alias := f.Tag.Get("envalias"); !set && alias != "" {
			if v, ok := aliases[alias]; ok {
				raw, set, envName = v, true, alias
			}
		}
		if !set {
			def, hasDef := f.Tag.Lookup("default")
			if !hasDef {
				continue // optional field stays nil
			}
			raw = def
		}
		if choices := f.Tag.Get("choices"); choices != "" && !contains(strings.Split(choices, ","), raw) {
			return nil, fmt.Errorf("%s: %q is not one of %s", envName, raw, choices)
		}
		if err := assign(rv.Field(i), raw); err != nil {
			return nil, fmt.Errorf("%s: %w", envName, err)
		}
		if err := checkBounds(f, rv.Field(i)); err != nil {
			return nil, fmt.Errorf("%s: %w", envName, err)
		}
	}
	// python: the _claude_code_lead_uses_claude_code model validator.
	if _, explicit := values["model_backend"]; s.LeadEngine == "claude_code" && !explicit {
		s.ModelBackend = "claude_code"
	}
	return s, nil
}

func assign(field reflect.Value, raw string) error {
	switch field.Interface().(type) {
	case string:
		field.SetString(raw)
	case *string:
		field.Set(reflect.ValueOf(&raw))
	case *Secret:
		field.Set(reflect.ValueOf(NewSecret(raw)))
	case bool:
		b, err := parseBool(raw)
		if err != nil {
			return err
		}
		field.SetBool(b)
	case int:
		n, err := parseInt(raw)
		if err != nil {
			return err
		}
		field.SetInt(int64(n))
	case float64:
		x, err := parseFloat(raw)
		if err != nil {
			return err
		}
		field.SetFloat(x)
	case *float64:
		x, err := parseFloat(raw)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(&x))
	case []int:
		list, err := parseIntList(raw)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(list))
	default:
		return fmt.Errorf("unsupported settings field type %s", field.Type())
	}
	return nil
}

// checkBounds enforces the gt/ge/le tags (python: Field(gt=..., ge=..., le=...)).
func checkBounds(f reflect.StructField, v reflect.Value) error {
	var x float64
	switch v.Kind() {
	case reflect.Int:
		x = float64(v.Int())
	case reflect.Float64:
		x = v.Float()
	default:
		return nil
	}
	for _, c := range []struct {
		tag, word string
		ok        func(x, bound float64) bool
	}{
		{"gt", "greater than", func(x, b float64) bool { return x > b }},
		{"ge", "greater than or equal to", func(x, b float64) bool { return x >= b }},
		{"le", "less than or equal to", func(x, b float64) bool { return x <= b }},
	} {
		raw, ok := f.Tag.Lookup(c.tag)
		if !ok {
			continue
		}
		bound, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("bad %s bound %q on %s", c.tag, raw, f.Name)
		}
		if !c.ok(x, bound) {
			return fmt.Errorf("input should be %s %s, got %s", c.word, raw, FormatPy(v.Interface()))
		}
	}
	return nil
}

// parseBool accepts what pydantic accepts for booleans.
func parseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "t", "yes", "y", "on":
		return true, nil
	case "0", "false", "f", "no", "n", "off":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q", raw)
}

// parseInt accepts what pydantic's lax int accepts from a string: surrounding whitespace, a sign,
// digit-group underscores, and floats with no fractional part ("5.0").
func parseInt(raw string) (int, error) {
	s := strings.ReplaceAll(strings.TrimSpace(raw), "_", "")
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	if x, err := strconv.ParseFloat(s, 64); err == nil && !strings.ContainsAny(s, "xXpP") &&
		x == math.Trunc(x) && !math.IsInf(x, 0) && math.Abs(x) < 1<<62 {
		return int(x), nil
	}
	return 0, fmt.Errorf("invalid integer %q", raw)
}

func parseFloat(raw string) (float64, error) {
	s := strings.ReplaceAll(strings.TrimSpace(raw), "_", "")
	if strings.ContainsAny(s, "xXpP") {
		return 0, fmt.Errorf("invalid number %q", raw)
	}
	x, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", raw)
	}
	return x, nil
}

// parseIntList decodes a JSON list of integers (pydantic-settings parses complex fields as JSON;
// elements are coerced like pydantic's lax int: bools, integral floats and numeric strings).
func parseIntList(raw string) ([]int, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var items []any
	if err := dec.Decode(&items); err != nil {
		return nil, fmt.Errorf("invalid JSON list of integers %q", raw)
	}
	if dec.More() {
		return nil, fmt.Errorf("invalid JSON list of integers %q", raw)
	}
	if items == nil {
		return nil, fmt.Errorf("invalid JSON list of integers %q", raw)
	}
	out := make([]int, 0, len(items))
	for i, item := range items {
		var n int
		var err error
		switch v := item.(type) {
		case json.Number:
			n, err = parseInt(v.String())
		case string:
			n, err = parseInt(v)
		case bool:
			if v {
				n = 1
			}
		default:
			err = fmt.Errorf("not an integer")
		}
		if err != nil {
			return nil, fmt.Errorf("item %d of %q is not an integer", i, raw)
		}
		out = append(out, n)
	}
	return out, nil
}

// readDotenv parses KEY=VALUE lines (comments, blank lines, optional `export `, quotes).
func readDotenv(path string, into map[string]string) error {
	fh, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer fh.Close()
	scanner := bufio.NewScanner(fh)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		} else if idx := strings.Index(v, " #"); idx >= 0 {
			v = strings.TrimSpace(v[:idx])
		}
		if strings.HasPrefix(strings.ToUpper(k), "LHA_") {
			into[strings.ToLower(k[4:])] = v
		}
	}
	return scanner.Err()
}

// Clone returns a copy whose pointer and slice fields are copied too, so a caller can override
// fields (Sandbox, AllowUnsafeLocal, WebAllowHosts, ...) per run without touching the original.
func (s *Settings) Clone() *Settings {
	c := *s
	cloneStr := func(p *string) *string {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	cloneF := func(p *float64) *float64 {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	cloneSecret := func(p *Secret) *Secret {
		if p == nil {
			return nil
		}
		return NewSecret(p.value)
	}
	c.OpenAIBaseURL = cloneStr(s.OpenAIBaseURL)
	c.OpenAIAPIKey = cloneSecret(s.OpenAIAPIKey)
	c.AnthropicAPIKey = cloneSecret(s.AnthropicAPIKey)
	c.OpenAIPriceInPerMTok = cloneF(s.OpenAIPriceInPerMTok)
	c.OpenAIPriceOutPerMTok = cloneF(s.OpenAIPriceOutPerMTok)
	c.ClaudePriceInPerMTok = cloneF(s.ClaudePriceInPerMTok)
	c.ClaudePriceOutPerMTok = cloneF(s.ClaudePriceOutPerMTok)
	c.PostgresDSN = cloneSecret(s.PostgresDSN)
	c.LangfuseHost = cloneStr(s.LangfuseHost)
	c.OTelExporterOTLPEndpoint = cloneStr(s.OTelExporterOTLPEndpoint)
	c.LangfusePublicKey = cloneStr(s.LangfusePublicKey)
	c.LangfuseSecretKey = cloneSecret(s.LangfuseSecretKey)
	c.WebCredentials = cloneSecret(s.WebCredentials)
	c.WebSearchProvider = cloneStr(s.WebSearchProvider)
	c.WebSearchAPIKey = cloneSecret(s.WebSearchAPIKey)
	c.WebSearchEndpoint = cloneStr(s.WebSearchEndpoint)
	if s.GateEscalationSeconds != nil {
		c.GateEscalationSeconds = append([]int{}, s.GateEscalationSeconds...)
	}
	c.GateWebhookURL = cloneSecret(s.GateWebhookURL)
	return &c
}

// SandboxEgressHosts is the Docker sandbox's egress allow-list from the three sandbox_egress*
// settings (python: sandbox_egress_hosts). It returns a ValueError-typed error for a malformed
// entry, a non-package-fetch host in LHA_SANDBOX_EGRESS or a known write host in
// LHA_SANDBOX_EGRESS_EXTRA_HOSTS.
func (s *Settings) SandboxEgressHosts() ([]string, error) {
	return egressproxy.SandboxAllowList(CSV(s.SandboxEgress), CSV(s.SandboxEgressExtraHosts),
		CSV(s.SandboxEgressAllowWriteHosts))
}

// SandboxEgressEntries is the three sandbox egress lists as written, de-duplicated (for
// messages: no validation).
func (s *Settings) SandboxEgressEntries() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range []string{s.SandboxEgress, s.SandboxEgressExtraHosts, s.SandboxEgressAllowWriteHosts} {
		for _, h := range CSV(v) {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// SandboxEgressEnabled reports whether a docker sandbox gets network through the egress proxy
// (any of the three lists set) (python: sandbox_egress_enabled).
func (s *Settings) SandboxEgressEnabled() bool {
	return s.Sandbox == "docker" && (len(CSV(s.SandboxEgress)) > 0 ||
		len(CSV(s.SandboxEgressExtraHosts)) > 0 || len(CSV(s.SandboxEgressAllowWriteHosts)) > 0)
}

// WebHosts is web_allow_hosts split (python: web_hosts).
func (s *Settings) WebHosts() []string { return CSV(s.WebAllowHosts) }

// WebPorts is web_allow_ports parsed as integers (python: web_ports).
func (s *Settings) WebPorts() ([]int, error) {
	out := []int{}
	for _, part := range CSV(s.WebAllowPorts) {
		n, err := pyInt(part)
		if err != nil {
			return nil, fmt.Errorf("LHA_WEB_ALLOW_PORTS must be integers: %w", err)
		}
		out = append(out, n)
	}
	return out, nil
}

// pyInt mirrors python int(str) (sign, underscores between digits), with its error text.
func pyInt(s string) (int, error) {
	body := strings.TrimLeft(s, "+-")
	ok := len(s)-len(body) <= 1 && body != "" && body[0] != '_' && body[len(body)-1] != '_' &&
		!strings.Contains(body, "__")
	if ok {
		if n, err := strconv.Atoi(strings.ReplaceAll(s, "_", "")); err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("invalid literal for int() with base 10: %s", contracts.PyRepr(s))
}

// FallbackModelEntries is fallback_models split (python: fallback_model_entries).
func (s *Settings) FallbackModelEntries() []string { return CSV(s.FallbackModels) }

// HarnessGlobs is harness_paths split (python: harness_globs).
func (s *Settings) HarnessGlobs() []string { return CSV(s.HarnessPaths) }

// TrustedCheckCommands parses trusted_checks (python: trusted_check_commands).
func (s *Settings) TrustedCheckCommands() (map[string][]string, error) {
	out := map[string][]string{}
	if strings.TrimSpace(s.TrustedChecks) == "" {
		return out, nil
	}
	var parsed any
	dec := json.NewDecoder(strings.NewReader(s.TrustedChecks))
	err := dec.Decode(&parsed)
	if err == nil && dec.More() {
		err = fmt.Errorf("extra data after the JSON value")
	}
	if err != nil {
		return nil, fmt.Errorf("LHA_TRUSTED_CHECKS is not valid JSON: %v", err)
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("LHA_TRUSTED_CHECKS must be a JSON object of name -> argv list")
	}
	// Validate in document order (python dicts keep it), so the first bad entry is the one named.
	for _, name := range objectKeyOrder(s.TrustedChecks) {
		argv := obj[name]
		list, ok := argv.([]any)
		if ok && len(list) > 0 {
			strs := make([]string, 0, len(list))
			for _, a := range list {
				str, isStr := a.(string)
				if !isStr {
					ok = false
					break
				}
				strs = append(strs, str)
			}
			if ok {
				out[name] = strs
				continue
			}
		}
		return nil, fmt.Errorf("LHA_TRUSTED_CHECKS[%s] must be a non-empty list of strings", contracts.PyRepr(name))
	}
	return out, nil
}

// objectKeyOrder lists a JSON object's top-level keys by first occurrence (the input is known to
// be a valid object).
func objectKeyOrder(raw string) []string {
	dec := json.NewDecoder(strings.NewReader(raw))
	keys := []string{}
	seen := map[string]bool{}
	if _, err := dec.Token(); err != nil { // '{'
		return keys
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		key, _ := tok.(string)
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			break
		}
	}
	return keys
}

// Redacted returns every setting keyed by its snake_case name with secrets masked, in declaration
// order (python: Settings.redacted()). Unset optional values are nil.
func (s *Settings) Redacted() []KV {
	out := []KV{}
	rv := reflect.ValueOf(s).Elem()
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Tag.Get("env")
		var value any
		switch v := rv.Field(i).Interface().(type) {
		case *Secret:
			if v != nil {
				value = Redacted
			}
		case *string:
			if v != nil {
				value = *v
			}
		case *float64:
			if v != nil {
				value = *v
			}
		case []int:
			value = append([]int{}, v...)
		default:
			value = v
		}
		out = append(out, KV{Key: name, Value: value})
	}
	return out
}

// KV is one displayed setting.
type KV struct {
	Key   string
	Value any
}

// FormatPy renders a Redacted() value the way python's str() does inside an f-string, so
// `lha config` prints byte-identical lines from both implementations.
func FormatPy(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return pyFloat(x)
	case *float64:
		if x == nil {
			return "None"
		}
		return pyFloat(*x)
	case *string:
		if x == nil {
			return "None"
		}
		return *x
	case []int:
		parts := make([]string, len(x))
		for i, n := range x {
			parts[i] = strconv.Itoa(n)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprint(v)
}

// pyFloat is python's repr(float): shortest round-trip digits, fixed notation for decimal
// exponents in [-4, 16), scientific ("1e-05", "1.5e+16") otherwise.
func pyFloat(x float64) string {
	switch {
	case math.IsNaN(x):
		return "nan"
	case math.IsInf(x, 1):
		return "inf"
	case math.IsInf(x, -1):
		return "-inf"
	}
	sci := strconv.FormatFloat(x, 'e', -1, 64)
	exp, _ := strconv.Atoi(sci[strings.LastIndexByte(sci, 'e')+1:])
	if exp < -4 || exp >= 16 {
		return sci // Go already writes a signed, at-least-two-digit exponent, like python
	}
	fixed := strconv.FormatFloat(x, 'f', -1, 64)
	if !strings.Contains(fixed, ".") {
		fixed += ".0"
	}
	return fixed
}

// SQLiteFile is the SQLite file name inside the per-user data directory.
const SQLiteFile = "lha.sqlite3"

// DefaultSQLitePath is the per-user SQLite store used when LHA_SQLITE_PATH is unset (python:
// persistence.store.default_sqlite_path): $XDG_DATA_HOME/lha/lha.sqlite3 when XDG_DATA_HOME is
// set and absolute, else ~/Library/Application Support/lha/lha.sqlite3 on macOS,
// %LOCALAPPDATA%/lha/lha.sqlite3 on Windows and ~/.local/share/lha/lha.sqlite3 elsewhere.
func DefaultSQLitePath() string {
	var base string
	xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	switch {
	case xdg != "" && filepath.IsAbs(xdg):
		base = pyPath(xdg)
	case runtime.GOOS == "darwin":
		base = filepath.Join(homeDir(), "Library", "Application Support")
	case runtime.GOOS == "windows" && os.Getenv("LOCALAPPDATA") != "":
		base = pyPath(os.Getenv("LOCALAPPDATA"))
	default:
		base = filepath.Join(homeDir(), ".local", "share")
	}
	return pyJoin(base, "lha", SQLiteFile)
}

// SQLitePath is the SQLite store's absolute path (python: configured_sqlite_path, which is also
// resolve_sqlite_path without a workdir): sqlite_path expanded and resolved, or
// DefaultSQLitePath() when empty. (Python additionally warns once about a relative path.)
func (s *Settings) SQLitePath() string {
	configured := strings.TrimSpace(s.SQLitePathSetting)
	if configured == "" {
		return DefaultSQLitePath()
	}
	return resolvePath(expandUser(configured))
}

// DescribeStore says where the store (no workdir) reads and writes, for humans (python:
// persistence.store.describe_store).
func (s *Settings) DescribeStore() string {
	if s.PostgresDSN.Value() != "" {
		fallback := ""
		if s.PostgresFallbackToSQLite {
			fallback = " (falls back to SQLite at " + s.SQLitePath() + ")"
		}
		return "postgres (LHA_POSTGRES_DSN)" + fallback
	}
	return "sqlite " + s.SQLitePath()
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "~"
}

// pyPath normalizes like pathlib.PurePosixPath: collapses repeated separators and "." parts and
// drops a trailing separator, but keeps ".." (unlike filepath.Clean).
func pyPath(p string) string {
	if runtime.GOOS == "windows" {
		return filepath.Clean(p)
	}
	abs := strings.HasPrefix(p, "/")
	parts := []string{}
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	joined := strings.Join(parts, "/")
	if abs {
		return "/" + joined
	}
	if joined == "" {
		return "."
	}
	return joined
}

func pyJoin(base string, parts ...string) string {
	return pyPath(strings.Join(append([]string{base}, parts...), string(filepath.Separator)))
}

// expandUser mirrors pathlib's Path.expanduser for "~" and "~user" prefixes.
func expandUser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	head, rest, _ := strings.Cut(p, "/")
	var home string
	if head == "~" {
		home = homeDir()
	} else if u, err := user.Lookup(head[1:]); err == nil {
		home = u.HomeDir
	} else {
		return p
	}
	if rest == "" {
		return home
	}
	return home + "/" + rest
}

// resolvePath mirrors pathlib's Path.resolve(strict=False): absolute, ".." collapsed and symlinks
// resolved for the longest existing prefix.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	existing, rest := abs, ""
	for {
		if real, err := filepath.EvalSymlinks(existing); err == nil {
			if rest == "" {
				return real
			}
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return abs
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// CSV splits a comma-separated setting (python: config._csv), dropping blanks.
func CSV(value string) []string {
	out := []string{}
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
