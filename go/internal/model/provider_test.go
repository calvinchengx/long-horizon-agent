package model

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

func settingsFrom(t *testing.T, env ...string) *config.Settings {
	t.Helper()
	s, err := config.LoadFrom(env, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSecretValue(t *testing.T) {
	if SecretValue(config.NewSecret("abc")) != "abc" || SecretValue(nil) != "" || SecretValue(config.NewSecret("")) != "" {
		t.Error("SecretValue")
	}
}

func TestBuildProviderBackends(t *testing.T) {
	p, err := BuildProvider(settingsFrom(t, "LHA_MODEL_BACKEND=claude", "LHA_MODEL_NAME=claude-sonnet-4-6", "LHA_ANTHROPIC_API_KEY=sk-x"), "", nil)
	if err != nil || p.Name() != "claude:claude-sonnet-4-6" {
		t.Fatalf("claude: %v %v", p, err)
	}
	if _, ok := p.(io.Closer); !ok {
		t.Error("HTTP providers are io.Closers")
	}

	ollama, err := BuildProvider(settingsFrom(t, "LHA_MODEL_BACKEND=ollama", "LHA_MODEL_NAME=llama3", "LHA_OLLAMA_BASE_URL=http://gpu:11434//"), "", nil)
	if err != nil || ollama.Name() != "ollama:llama3" {
		t.Fatalf("ollama: %v %v", ollama, err)
	}
	if c, err := ollama.EstimateCostUSD(contracts.Usage{InputTokens: 1000}); c != 0 || err != nil {
		t.Errorf("local => genuinely $0: %v %v", c, err)
	}
	o := ollama.(*OpenAICompatModel)
	if o.baseURL != "http://gpu:11434/v1" || o.apiKey != "ollama" || o.DefaultMaxTokens() != 8192 {
		t.Errorf("ollama = %+v", o)
	}

	stub, err := BuildProvider(settingsFrom(t), "", nil)
	if err != nil || stub.Name() != "stub:stub-1" {
		t.Fatalf("default is the stub: %v %v", stub, err)
	}
	if _, ok := stub.(*StubModel); !ok {
		t.Errorf("stub = %T", stub)
	}
	if routed, _ := BuildProvider(settingsFrom(t), "planner", nil); routed.Name() != "stub:planner" {
		t.Errorf("name override = %q", routed.Name())
	}
}

func TestBuildProviderNilSettingsLoadsEnvironment(t *testing.T) {
	t.Setenv("LHA_MODEL_BACKEND", "stub")
	t.Setenv("LHA_MODEL_NAME", "from-env")
	t.Chdir(t.TempDir()) // no .env
	p, err := BuildProvider(nil, "", nil)
	if err != nil || p.Name() != "stub:from-env" {
		t.Fatalf("%v %v", p, err)
	}
	t.Setenv("LHA_MODEL_BACKEND", "nope")
	if _, err := BuildProvider(nil, "", nil); err == nil {
		t.Error("invalid settings must error")
	}
}

func TestBuildProviderErrors(t *testing.T) {
	cases := []struct {
		settings *config.Settings
		want     string
	}{
		{settingsFrom(t, "LHA_MODEL_BACKEND=openai_compat"), "LHA_OPENAI_BASE_URL is required for the 'openai_compat' backend."},
		{settingsFrom(t, "LHA_MODEL_BACKEND=openai_compat", "LHA_OPENAI_BASE_URL="), "LHA_OPENAI_BASE_URL is required for the 'openai_compat' backend."},
		{settingsFrom(t, "LHA_MODEL_BACKEND=claude"), "LHA_ANTHROPIC_API_KEY is required for the 'claude' backend."},
		{settingsFrom(t, "LHA_MODEL_BACKEND=claude", "LHA_ANTHROPIC_API_KEY="), "LHA_ANTHROPIC_API_KEY is required for the 'claude' backend."},
		{&config.Settings{ModelBackend: "bedrock"}, "Unknown model backend: 'bedrock'"},
		{settingsFrom(t, "LHA_MODEL_BACKEND=openai_compat", "LHA_OPENAI_BASE_URL=http://x", "LHA_OPENAI_PRICE_IN_PER_MTOK=1"),
			"configure both price_in_per_mtok and price_out_per_mtok, or neither"},
	}
	for _, c := range cases {
		p, err := BuildProvider(c.settings, "", nil)
		if err == nil || err.Error() != c.want || p != nil {
			t.Errorf("BuildProvider(%s) = %v, %v; want %q", c.settings.ModelBackend, p, err, c.want)
		}
	}
	// An unpriced Claude model is refused (never silently $0), and p is a true nil.
	p, err := BuildProvider(settingsFrom(t, "LHA_MODEL_BACKEND=claude", "LHA_MODEL_NAME=claude-custom", "LHA_ANTHROPIC_API_KEY=k"), "", nil)
	if !errors.Is(err, contracts.ErrUnknownPrice) || p != nil {
		t.Errorf("unpriced claude = %v, %v", p, err)
	}
}

func TestOpenAIPriceEnvVarsAreHonoured(t *testing.T) {
	s := settingsFrom(t, "LHA_MODEL_BACKEND=openai_compat", "LHA_MODEL_NAME=m",
		"LHA_OPENAI_BASE_URL=http://example.invalid/v1", "LHA_OPENAI_PRICE_IN_PER_MTOK=1.0",
		"LHA_OPENAI_PRICE_OUT_PER_MTOK=2.0", "LHA_OPENAI_API_KEY=sk-o")
	p, err := BuildProvider(s, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	cost, err := p.EstimateCostUSD(contracts.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "cost", cost, 3.0)
	if p.(*OpenAICompatModel).apiKey != "sk-o" {
		t.Error("api key not passed")
	}
}

func TestClaudeExplicitPriceSettingsApply(t *testing.T) {
	env := []string{"LHA_MODEL_BACKEND=claude", "LHA_MODEL_NAME=claude-custom-model", "LHA_ANTHROPIC_API_KEY=sk-test",
		"LHA_CLAUDE_PRICE_IN_PER_MTOK=4.0", "LHA_CLAUDE_PRICE_OUT_PER_MTOK=8.0"}
	p, err := BuildProvider(settingsFrom(t, env...), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	cost, err := p.EstimateCostUSD(contracts.Usage{InputTokens: 1_000_000, Model: "claude-custom-model"})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "cost", cost, 4.0)
	// Explicit prices describe the configured model only, not a per-role override.
	if _, err := BuildProvider(settingsFrom(t, env...), "claude-other-custom", nil); !errors.Is(err, contracts.ErrUnknownPrice) {
		t.Errorf("per-role override must not inherit explicit prices: %v", err)
	}
	routed, err := BuildProvider(settingsFrom(t, env...), "claude-haiku-4-5", nil)
	if err != nil || routed.Name() != "claude:claude-haiku-4-5" {
		t.Errorf("routed = %v, %v", routed, err)
	}
}

func TestBuildProviderSharesTheCallersClient(t *testing.T) {
	shared := &http.Client{}
	s := settingsFrom(t, "LHA_MODEL_BACKEND=claude", "LHA_MODEL_NAME=claude-sonnet-4-6", "LHA_ANTHROPIC_API_KEY=k")
	p, _ := BuildProvider(s, "", shared)
	c := p.(*ClaudeModel)
	if c.http.client != shared || c.http.owned {
		t.Error("the shared client must be borrowed, not owned")
	}
}
