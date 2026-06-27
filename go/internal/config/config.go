// Package config is the Go mirror of python/src/lha/config.py: one Settings value populated from
// LHA_* environment variables and an optional .env file (environment variables win), with the same
// names, defaults and validation, so one environment configures either implementation.
package config

import (
	"bufio"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
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

// Settings is the runtime configuration of an LHA deployment. Field tags name the env var suffix.
type Settings struct {
	ModelBackend string `env:"model_backend" default:"stub" choices:"stub,ollama,openai_compat,claude"`
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

	TemporalAddress   string `env:"temporal_address" default:"localhost:7233"`
	TemporalNamespace string `env:"temporal_namespace" default:"default"`
	TaskQueue         string `env:"task_queue" default:"lha-mission"`

	PostgresDSN     *Secret `env:"postgres_dsn"`
	WorkspaceRoot   string  `env:"workspace_root" default:".lha/workspaces"`
	ObjectStoreRoot string  `env:"object_store_root" default:".lha/objects"`

	BudgetUSDCeiling float64 `env:"budget_usd_ceiling" default:"10.0"`
	MaxCycles        int     `env:"max_cycles" default:"1000"`
	MaxTurnsPerCycle int     `env:"max_turns_per_cycle" default:"8"`
	StallLimit       int     `env:"stall_limit" default:"5"`
	MaxReplans       int     `env:"max_replans" default:"20"`
	MaxSplitDepth    int     `env:"max_split_depth" default:"2"`
	ApprovalTimeoutS int     `env:"approval_timeout_s" default:"86400"`

	Sandbox          string `env:"sandbox" default:"docker" choices:"docker,e2b,local"`
	AllowUnsafeLocal bool   `env:"allow_unsafe_local" default:"false"`
	SandboxImage     string `env:"sandbox_image" default:"ghcr.io/astral-sh/uv:python3.12-bookworm-slim"`
	SandboxEgress    string `env:"sandbox_egress" default:""`
	WebAllowHosts    string `env:"web_allow_hosts" default:""`
	TrustedChecks    string `env:"trusted_checks" default:""`
	HarnessPaths     string `env:"harness_paths" default:""`

	LangfuseHost      *string `env:"langfuse_host"`
	LangfusePublicKey *string `env:"langfuse_public_key"`
	LangfuseSecretKey *Secret `env:"langfuse_secret_key"`
}

// Load reads settings from the process environment and ./.env (if present).
func Load() (*Settings, error) { return LoadFrom(os.Environ(), ".env") }

// LoadFrom reads settings from environ (KEY=VALUE pairs) layered over the dotenv file (optional).
func LoadFrom(environ []string, dotenv string) (*Settings, error) {
	values := map[string]string{}
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
	}
	s := &Settings{}
	rv := reflect.ValueOf(s).Elem()
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		name := f.Tag.Get("env")
		raw, set := values[name]
		if !set {
			def, hasDef := f.Tag.Lookup("default")
			if !hasDef {
				continue // optional field stays nil
			}
			raw = def
		}
		if choices := f.Tag.Get("choices"); choices != "" && !contains(strings.Split(choices, ","), raw) {
			return nil, fmt.Errorf("LHA_%s: %q is not one of %s", strings.ToUpper(name), raw, choices)
		}
		if err := assign(rv.Field(i), raw); err != nil {
			return nil, fmt.Errorf("LHA_%s: %w", strings.ToUpper(name), err)
		}
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
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid integer %q", raw)
		}
		field.SetInt(int64(n))
	case float64:
		x, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return fmt.Errorf("invalid number %q", raw)
		}
		field.SetFloat(x)
	case *float64:
		x, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return fmt.Errorf("invalid number %q", raw)
		}
		field.Set(reflect.ValueOf(&x))
	default:
		return fmt.Errorf("unsupported settings field type %s", field.Type())
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

// Redacted returns every setting keyed by its snake_case name with secrets masked, in declaration
// order (python: Settings.redacted()).
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
