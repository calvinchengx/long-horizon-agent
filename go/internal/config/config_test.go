package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsMatchPython(t *testing.T) {
	s, err := LoadFrom(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.ModelBackend != "stub" || s.ModelName != "stub-1" || s.Sandbox != "docker" ||
		s.TaskQueue != "lha-mission" || s.BudgetUSDCeiling != 10.0 || s.MaxCycles != 1000 ||
		s.MaxTurnsPerCycle != 8 || s.StallLimit != 5 || s.AllowUnsafeLocal || s.OpenAIBaseURL != nil {
		t.Fatalf("unexpected defaults: %+v", s)
	}
}

func TestEnvOverridesDotenvAndSecretsAreHidden(t *testing.T) {
	dir := t.TempDir()
	dotenv := filepath.Join(dir, ".env")
	body := "# comment\nLHA_MODEL_NAME=from-dotenv\nexport LHA_STALL_LIMIT=7\n" +
		"LHA_ANTHROPIC_API_KEY=\"sk-ant-secret\"\nOTHER=ignored\n"
	if err := os.WriteFile(dotenv, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadFrom([]string{"LHA_MODEL_NAME=from-env", "lha_allow_unsafe_local=YES"}, dotenv)
	if err != nil {
		t.Fatal(err)
	}
	if s.ModelName != "from-env" || s.StallLimit != 7 || !s.AllowUnsafeLocal {
		t.Fatalf("precedence wrong: %+v", s)
	}
	if s.AnthropicAPIKey.Value() != "sk-ant-secret" {
		t.Fatal("secret value lost")
	}
	if strings.Contains(fmt.Sprintf("%v %+v", s.AnthropicAPIKey, s), "sk-ant-secret") {
		t.Fatal("secret leaked through formatting")
	}
	for _, kv := range s.Redacted() {
		if kv.Key == "anthropic_api_key" && kv.Value != Redacted {
			t.Fatalf("anthropic_api_key shown as %v", kv.Value)
		}
		if kv.Key == "openai_api_key" && kv.Value != nil {
			t.Fatalf("unset secret should display as null, got %v", kv.Value)
		}
	}
}

func TestValidation(t *testing.T) {
	for _, env := range []string{"LHA_MODEL_BACKEND=gpt", "LHA_SANDBOX=chroot", "LHA_MAX_CYCLES=many", "LHA_ALLOW_UNSAFE_LOCAL=maybe"} {
		if _, err := LoadFrom([]string{env}, ""); err == nil {
			t.Errorf("%s should be rejected", env)
		}
	}
}
