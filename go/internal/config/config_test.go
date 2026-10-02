package config

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
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
		s.MaxTurnsPerCycle != 20 || s.StallLimit != 5 || s.AllowUnsafeLocal || s.OpenAIBaseURL != nil {
		t.Fatalf("unexpected defaults: %+v", s)
	}
}

func TestNewFieldDefaults(t *testing.T) {
	s, err := LoadFrom(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string][2]any{
		"LeadEngine":                 {s.LeadEngine, "loop"},
		"ClaudeCodeBin":              {s.ClaudeCodeBin, "claude"},
		"ClaudeCodeTools":            {s.ClaudeCodeTools, "lha"},
		"ClaudeCodeMaxBudgetUSD":     {s.ClaudeCodeMaxBudgetUSD, 5.0},
		"ClaudeCodeTimeoutS":         {s.ClaudeCodeTimeoutS, 3600.0},
		"MaxParallelImplementers":    {s.MaxParallelImplementers, 3},
		"SQLitePathSetting":          {s.SQLitePathSetting, ""},
		"PostgresFallbackToSQLite":   {s.PostgresFallbackToSQLite, true},
		"MemoryEnabled":              {s.MemoryEnabled, true},
		"MemoryPromptBudgetChars":    {s.MemoryPromptBudgetChars, 4000},
		"MemoryEpisodicK":            {s.MemoryEpisodicK, 4},
		"MemorySemanticK":            {s.MemorySemanticK, 4},
		"MemorySkillsK":              {s.MemorySkillsK, 2},
		"MemoryEmbedder":             {s.MemoryEmbedder, "hash"},
		"MemoryEmbeddingModel":       {s.MemoryEmbeddingModel, ""},
		"MemoryRerank":               {s.MemoryRerank, "none"},
		"MemoryConsolidateEvery":     {s.MemoryConsolidateEvery, 5},
		"MemoryConsolidation":        {s.MemoryConsolidation, "extractive"},
		"MemoryIndexRepoFiles":       {s.MemoryIndexRepoFiles, true},
		"MemoryMaxRepoFiles":         {s.MemoryMaxRepoFiles, 400},
		"WebAllowPorts":              {s.WebAllowPorts, ""},
		"WebTimeoutS":                {s.WebTimeoutS, 30.0},
		"WebMaxResponseBytes":        {s.WebMaxResponseBytes, 2000000},
		"PrivateData":                {s.PrivateData, false},
		"FallbackModels":             {s.FallbackModels, ""},
		"FallbackMaxRounds":          {s.FallbackMaxRounds, 2},
		"ModelTimeoutS":              {s.ModelTimeoutS, 120.0},
		"ModelProbeTimeoutS":         {s.ModelProbeTimeoutS, 10.0},
		"WorkerGuardIntervalS":       {s.WorkerGuardIntervalS, 30.0},
		"MutationCheck":              {s.MutationCheck, ""},
		"MutationTimeoutS":           {s.MutationTimeoutS, 1800},
		"WorkerDeployment":           {s.WorkerDeployment, ""},
		"WorkerBuildID":              {s.WorkerBuildID, ""},
		"WorkerVersioningBehavior":   {s.WorkerVersioningBehavior, "pinned"},
		"WorkerPromote":              {s.WorkerPromote, false},
		"ConsoleApprovalTimeoutS":    {s.ConsoleApprovalTimeoutS, 3600},
		"DeadlockGateDefault":        {s.DeadlockGateDefault, "abort"},
		"ImpossibleAfterFailures":    {s.ImpossibleAfterFailures, 3},
		"CyclePauseSeconds":          {s.CyclePauseSeconds, 0},
		"GateWebhookTimeoutSeconds":  {s.GateWebhookTimeoutSeconds, 5.0},
		"CodeMap":                    {s.CodeMap, "off"},
		"CodeMapTokenBudget":         {s.CodeMapTokenBudget, 2000},
		"CodeQuery":                  {s.CodeQuery, false},
		"CodeQueryTokenBudget":       {s.CodeQueryTokenBudget, 1500},
		"CodeMapTimeoutS":            {s.CodeMapTimeoutS, 60.0},
		"SystemOneBackend":           {s.SystemOneBackend, "off"},
		"SystemOneEndpoint":          {s.SystemOneEndpoint, "https://api.typesafe.ai/v1/systemone"},
		"SystemOneModel":             {s.SystemOneModel, "jev-1.13.0"},
		"SystemOneTimeoutS":          {s.SystemOneTimeoutS, 5.0},
		"SystemOneTriage":            {s.SystemOneTriage, true},
		"SystemOneTriageThreshold":   {s.SystemOneTriageThreshold, 0.9},
		"SystemOneTriageMinFailures": {s.SystemOneTriageMinFailures, 2},
		"SystemOneRerankMin":         {s.SystemOneRerankMin, 0.0},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %#v, want %#v", name, c[0], c[1])
		}
	}
	if !reflect.DeepEqual(s.GateEscalationSeconds, []int{900, 2700, 14400, 43200}) {
		t.Errorf("GateEscalationSeconds = %v", s.GateEscalationSeconds)
	}
	if s.WebCredentials != nil || s.WebSearchProvider != nil || s.WebSearchAPIKey != nil ||
		s.WebSearchEndpoint != nil || s.GateWebhookURL != nil || s.SystemOneAPIKey != nil ||
		s.SystemOnePriceInPerMTok != nil {
		t.Error("optional web/gate fields should default to nil")
	}
	keys := []string{}
	for _, kv := range s.Redacted() {
		keys = append(keys, kv.Key)
	}
	if keys[0] != "model_backend" || keys[len(keys)-1] != "gate_webhook_timeout_seconds" || len(keys) != 111 {
		t.Errorf("redacted keys (%d): %v", len(keys), keys)
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
	for _, env := range []string{
		"LHA_MODEL_BACKEND=gpt", "LHA_SANDBOX=chroot", "LHA_MAX_CYCLES=many", "LHA_ALLOW_UNSAFE_LOCAL=maybe",
		"LHA_LEAD_ENGINE=agent", "LHA_CLAUDE_CODE_TOOLS=all", "LHA_MEMORY_EMBEDDER=bert",
		"LHA_MEMORY_RERANK=llm", "LHA_MEMORY_CONSOLIDATION=magic", "LHA_WEB_SEARCH_PROVIDER=google",
		"LHA_DEADLOCK_GATE_DEFAULT=retry", "LHA_MAX_CYCLES=5.5",
	} {
		if _, err := LoadFrom([]string{env}, ""); err == nil {
			t.Errorf("%s should be rejected", env)
		}
	}
}

func TestNumericConstraints(t *testing.T) {
	bad := []string{
		"LHA_CLAUDE_CODE_MAX_BUDGET_USD=0", "LHA_CLAUDE_CODE_TIMEOUT_S=-1", "LHA_WEB_TIMEOUT_S=0",
		"LHA_WEB_MAX_RESPONSE_BYTES=0", "LHA_FALLBACK_MAX_ROUNDS=0", "LHA_MODEL_PROBE_TIMEOUT_S=0",
		"LHA_CONSOLE_APPROVAL_TIMEOUT_S=0", "LHA_IMPOSSIBLE_AFTER_FAILURES=0",
		"LHA_CYCLE_PAUSE_SECONDS=-1", "LHA_GATE_WEBHOOK_TIMEOUT_SECONDS=0",
		"LHA_GATE_WEBHOOK_TIMEOUT_SECONDS=60.5",
	}
	for _, env := range bad {
		_, err := LoadFrom([]string{env}, "")
		if err == nil {
			t.Errorf("%s should be rejected", env)
			continue
		}
		if name, _, _ := strings.Cut(env, "="); !strings.HasPrefix(err.Error(), name+":") {
			t.Errorf("%s: error %q should name the variable", env, err)
		}
	}
	good := []string{
		"LHA_GATE_WEBHOOK_TIMEOUT_SECONDS=60", "LHA_CYCLE_PAUSE_SECONDS=0", "LHA_FALLBACK_MAX_ROUNDS=1",
		"LHA_WEB_TIMEOUT_S=0.001", "LHA_MAX_CYCLES=1_000", "LHA_MAX_CYCLES= 5.0 ",
	}
	for _, env := range good {
		if _, err := LoadFrom([]string{env}, ""); err != nil {
			t.Errorf("%s should be accepted: %v", env, err)
		}
	}
}

func TestGateEscalationSecondsJSON(t *testing.T) {
	cases := map[string][]int{
		"[60, 120]":      {60, 120},
		" [1] ":          {1},
		"[]":             {},
		`[1, "2", 3.0]`:  {1, 2, 3},
		"[true, false]":  {1, 0},
		"[900,2700]\n  ": {900, 2700},
	}
	for raw, want := range cases {
		s, err := LoadFrom([]string{"LHA_GATE_ESCALATION_SECONDS=" + raw}, "")
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if !reflect.DeepEqual(s.GateEscalationSeconds, want) {
			t.Errorf("%q -> %v, want %v", raw, s.GateEscalationSeconds, want)
		}
	}
	for _, raw := range []string{"900,2700", "1", "[1.5]", `["a"]`, "[null]", "{}", "[1] [2]", "", "null"} {
		if _, err := LoadFrom([]string{"LHA_GATE_ESCALATION_SECONDS=" + raw}, ""); err == nil {
			t.Errorf("%q should be rejected", raw)
		}
	}
}

func TestLeadEngineValidator(t *testing.T) {
	s, err := LoadFrom([]string{"LHA_LEAD_ENGINE=claude_code"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.ModelBackend != "claude_code" {
		t.Errorf("lead_engine=claude_code alone should route model_backend to claude_code, got %s", s.ModelBackend)
	}
	s, err = LoadFrom([]string{"LHA_LEAD_ENGINE=claude_code", "LHA_MODEL_BACKEND=stub"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.ModelBackend != "stub" {
		t.Errorf("an explicit model_backend must win, got %s", s.ModelBackend)
	}
	dotenv := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(dotenv, []byte("LHA_MODEL_BACKEND=ollama\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = LoadFrom([]string{"LHA_LEAD_ENGINE=claude_code"}, dotenv)
	if err != nil {
		t.Fatal(err)
	}
	if s.ModelBackend != "ollama" {
		t.Errorf("a model_backend from .env must win, got %s", s.ModelBackend)
	}
}

func TestHelpers(t *testing.T) {
	s, err := LoadFrom([]string{
		"LHA_SANDBOX_EGRESS= pypi.org, ,proxy.golang.org", "LHA_WEB_ALLOW_HOSTS=a.com,b.com",
		"LHA_WEB_ALLOW_PORTS=8080, 8443", "LHA_FALLBACK_MODELS=claude:x,ollama:y", "LHA_HARNESS_PATHS=Makefile,e2e/**",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	egress, err := s.SandboxEgressHosts()
	if err != nil || !reflect.DeepEqual(egress, []string{"pypi.org", "proxy.golang.org"}) ||
		!reflect.DeepEqual(s.WebHosts(), []string{"a.com", "b.com"}) ||
		!reflect.DeepEqual(s.FallbackModelEntries(), []string{"claude:x", "ollama:y"}) ||
		!reflect.DeepEqual(s.HarnessGlobs(), []string{"Makefile", "e2e/**"}) {
		t.Fatalf("csv helpers wrong: %+v", s)
	}
	ports, err := s.WebPorts()
	if err != nil || !reflect.DeepEqual(ports, []int{8080, 8443}) {
		t.Fatalf("WebPorts = %v, %v", ports, err)
	}
	s.WebAllowPorts = "80,http"
	if _, err := s.WebPorts(); err == nil ||
		err.Error() != "LHA_WEB_ALLOW_PORTS must be integers: invalid literal for int() with base 10: 'http'" {
		t.Fatalf("WebPorts error = %v", err)
	}

	c := s.Clone()
	c.Sandbox = "local"
	c.GateEscalationSeconds[0] = 1
	if s.Sandbox != "docker" || s.GateEscalationSeconds[0] != 900 {
		t.Fatal("Clone must not share state with the original")
	}
}

func TestTrustedCheckCommands(t *testing.T) {
	s := &Settings{}
	if got, err := s.TrustedCheckCommands(); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	s.TrustedChecks = `{"e2e": ["make", "e2e"], "lint": ["ruff"]}`
	got, err := s.TrustedCheckCommands()
	if err != nil || !reflect.DeepEqual(got, map[string][]string{"e2e": {"make", "e2e"}, "lint": {"ruff"}}) {
		t.Fatalf("parsed: %v %v", got, err)
	}
	errs := map[string]string{
		"{nope":                          "LHA_TRUSTED_CHECKS is not valid JSON: ",
		`["make"]`:                       "LHA_TRUSTED_CHECKS must be a JSON object of name -> argv list",
		`{"ok": ["a"], "e2e": []}`:       "LHA_TRUSTED_CHECKS['e2e'] must be a non-empty list of strings",
		`{"b": [1], "a": "x"}`:           "LHA_TRUSTED_CHECKS['b'] must be a non-empty list of strings",
		`{"it's": "make"}`:               `LHA_TRUSTED_CHECKS["it's"] must be a non-empty list of strings`,
		`{"z": ["a"], "y": ["b", null]}`: "LHA_TRUSTED_CHECKS['y'] must be a non-empty list of strings",
	}
	for raw, want := range errs {
		s.TrustedChecks = raw
		_, err := s.TrustedCheckCommands()
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: got %v, want prefix %q", raw, err, want)
		}
	}
}

func TestTrustedCheckEnvNames(t *testing.T) {
	s := &Settings{TrustedCheckEnv: " GOFLAGS, GOPROXY ,"}
	if got, err := s.TrustedCheckEnvNames(); err != nil || !reflect.DeepEqual(got, []string{"GOFLAGS", "GOPROXY"}) {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := (&Settings{}).TrustedCheckEnvNames(); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	s.TrustedCheckEnv = "LHA_POSTGRES_DSN"
	if _, err := s.TrustedCheckEnvNames(); err == nil || !strings.Contains(err.Error(), "cannot be passed") {
		t.Fatal(err)
	}
}

func TestFormatPy(t *testing.T) {
	point1 := 0.1
	cases := []struct {
		in   any
		want string
	}{
		{nil, "None"}, {true, "True"}, {false, "False"}, {"", ""}, {"docker", "docker"},
		{0, "0"}, {-3, "-3"}, {86400, "86400"},
		{10.0, "10.0"}, {3600.0, "3600.0"}, {5.0, "5.0"}, {0.59, "0.59"}, {1e-05, "1e-05"},
		{0.0001, "0.0001"}, {1e16, "1e+16"}, {1.5e16, "1.5e+16"}, {1e15, "1000000000000000.0"},
		{123456789.125, "123456789.125"}, {point1 + 0.2, "0.30000000000000004"}, {-0.0, "0.0"},
		{math.Copysign(0, -1), "-0.0"}, {math.Inf(1), "inf"}, {math.NaN(), "nan"}, {2.5e-300, "2.5e-300"},
		{1e100, "1e+100"},
		{[]int{900, 2700, 14400, 43200}, "[900, 2700, 14400, 43200]"}, {[]int{}, "[]"},
		{Redacted, "***"},
	}
	for _, c := range cases {
		if got := FormatPy(c.in); got != c.want {
			t.Errorf("FormatPy(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDescribeStore(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	s, err := LoadFrom(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(xdg, "lha", "lha.sqlite3")
	if DefaultSQLitePath() != want || s.SQLitePath() != want {
		t.Fatalf("default path %q / %q, want %q", DefaultSQLitePath(), s.SQLitePath(), want)
	}
	if got := s.DescribeStore(); got != "sqlite "+want {
		t.Errorf("DescribeStore = %q", got)
	}

	s, _ = LoadFrom([]string{"LHA_POSTGRES_DSN=postgresql://u:p@h/db"}, "")
	if got := s.DescribeStore(); got != "postgres (LHA_POSTGRES_DSN) (falls back to SQLite at "+want+")" {
		t.Errorf("DescribeStore = %q", got)
	}
	s, _ = LoadFrom([]string{"LHA_POSTGRES_DSN=postgresql://u:p@h/db", "LHA_POSTGRES_FALLBACK_TO_SQLITE=false"}, "")
	if got := s.DescribeStore(); got != "postgres (LHA_POSTGRES_DSN)" {
		t.Errorf("DescribeStore = %q", got)
	}

	t.Setenv("XDG_DATA_HOME", "relative/dir")
	home, _ := os.UserHomeDir()
	wantDefault := filepath.Join(home, ".local", "share", "lha", "lha.sqlite3")
	if runtime.GOOS == "darwin" {
		wantDefault = filepath.Join(home, "Library", "Application Support", "lha", "lha.sqlite3")
	}
	if runtime.GOOS != "windows" && DefaultSQLitePath() != wantDefault {
		t.Errorf("relative XDG_DATA_HOME must be ignored: %q", DefaultSQLitePath())
	}

	s, _ = LoadFrom([]string{"LHA_SQLITE_PATH=~/x/lha.db"}, "")
	if runtime.GOOS != "windows" && s.SQLitePath() != resolvePath(filepath.Join(home, "x", "lha.db")) {
		t.Errorf("expanduser: %q", s.SQLitePath())
	}
	s, _ = LoadFrom([]string{"LHA_SQLITE_PATH=rel.db"}, "")
	cwd, _ := os.Getwd()
	if s.SQLitePath() != resolvePath(filepath.Join(cwd, "rel.db")) || !filepath.IsAbs(s.SQLitePath()) {
		t.Errorf("relative sqlite_path: %q", s.SQLitePath())
	}
}

// goConfigLines renders what `lha config` prints (python: cli.main.config).
func goConfigLines(s *Settings) string {
	var b strings.Builder
	for _, kv := range s.Redacted() {
		fmt.Fprintf(&b, "%s = %s\n", kv.Key, FormatPy(kv.Value))
	}
	fmt.Fprintf(&b, "mission store = %s\n", s.DescribeStore())
	return b.String()
}

const pyConfigScript = `
from lha.config import Settings
from lha.persistence.store import describe_store
s = Settings()
for k, v in s.redacted().items():
    print(f"{k} = {v}")
print(f"mission store = {describe_store(s)}")
`

// TestConfigOutputMatchesPython runs the Python reference under the same environment and compares
// the `lha config` lines byte for byte.
func TestConfigOutputMatchesPython(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-implementation test skipped in -short mode")
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("uv not on PATH")
	}
	pyProject, err := filepath.Abs(filepath.Join("..", "..", "..", "python"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pyProject, "pyproject.toml")); err != nil {
		t.Skipf("python project not found at %s", pyProject)
	}

	base := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		ku := strings.ToUpper(k)
		if strings.HasPrefix(ku, "LHA_") || ku == "XDG_DATA_HOME" || ku == "VIRTUAL_ENV" {
			continue
		}
		base = append(base, kv)
	}

	tmp := t.TempDir()
	cases := map[string][]string{
		"defaults": nil,
		"mixed": {
			"LHA_LEAD_ENGINE=claude_code", "lha_model_name=sonnet", "LHA_ANTHROPIC_API_KEY=sk-secret",
			"LHA_BUDGET_USD_CEILING=1e-5", "LHA_CLAUDE_CODE_TIMEOUT_S=1e16", "LHA_OPENAI_PRICE_IN_PER_MTOK=0.59",
			"LHA_GATE_ESCALATION_SECONDS=[1, \"2\", 3.0]", "LHA_WEB_SEARCH_PROVIDER=exa",
			"LHA_WEB_SEARCH_ENDPOINT=", "LHA_MAX_CYCLES=1_000", "LHA_MEMORY_ENABLED=off",
			"LHA_WEB_TIMEOUT_S=7", "LHA_GATE_WEBHOOK_URL=https://hooks.example/x", "LHA_SANDBOX_EGRESS=pypi.org",
			"LHA_TRUSTED_CHECK_ENV=GOFLAGS,GOPROXY",
			"LHA_SANDBOX_EGRESS_EXTRA_HOSTS=mirror.example", "LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS=github.com",
		},
		"postgres": {
			"LHA_POSTGRES_DSN=postgresql://u:p@h/db", "LHA_SQLITE_PATH=" + filepath.Join(tmp, "store", "lha.db"),
			"LHA_MODEL_BACKEND=ollama", "LHA_LEAD_ENGINE=claude_code", "LHA_MODEL_PROBE_TIMEOUT_S=123456789.125",
			"LHA_MEMORY_EMBEDDER=voyage", "LHA_VOYAGE_API_KEY=pa-secret", "LHA_VOYAGE_ENDPOINT=https://voyage.example/v1/embeddings",
		},
	}
	for name, lha := range cases {
		t.Run(name, func(t *testing.T) {
			xdg := filepath.Join(tmp, name, "data")
			cwd := filepath.Join(tmp, name, "cwd")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_DATA_HOME", xdg)
			s, err := LoadFrom(lha, "")
			if err != nil {
				t.Fatal(err)
			}
			want := goConfigLines(s)

			cmd := exec.Command(uv, "run", "--quiet", "--project", pyProject, "python", "-c", pyConfigScript)
			cmd.Dir = cwd
			cmd.Env = append(append(append([]string{}, base...), "XDG_DATA_HOME="+xdg), lha...)
			out, err := cmd.Output()
			if err != nil {
				stderr := ""
				if ee, ok := err.(*exec.ExitError); ok {
					stderr = string(ee.Stderr)
				}
				t.Fatalf("python failed: %v\n%s", err, stderr)
			}
			if got := string(out); got != want {
				t.Errorf("python and go `lha config` differ\n--- python\n%s--- go\n%s", got, want)
			}
		})
	}
}

func TestOTelAliasesAndPrecedence(t *testing.T) {
	s, err := LoadFrom([]string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://std:4318", "OTEL_SDK_DISABLED=true"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.OTelExporterOTLPEndpoint == nil || *s.OTelExporterOTLPEndpoint != "http://std:4318" || !s.OTelSDKDisabled {
		t.Fatalf("standard OTel variables not read: %+v", s)
	}
	s, err = LoadFrom([]string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://std:4318", "LHA_OTEL_EXPORTER_OTLP_ENDPOINT=http://lha:4318", "LHA_OTEL_SDK_DISABLED=true"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if *s.OTelExporterOTLPEndpoint != "http://lha:4318" {
		t.Fatalf("LHA_ spelling must win, got %s", *s.OTelExporterOTLPEndpoint)
	}
	if s.OTelSDKDisabled {
		t.Fatal("otel_sdk_disabled is read only as OTEL_SDK_DISABLED (as in Python)")
	}
	if _, err := LoadFrom([]string{"LHA_FLAKY_RETRIES=6"}, ""); err == nil {
		t.Fatal("flaky_retries > 5 must be rejected")
	}
}

func TestResetKeepPaths(t *testing.T) {
	for _, entry := range []string{".", "*", "**", "*/*", "/abs", "../up", "a/../b", ".git", ".git/hooks", ".lha", "./"} {
		s, err := LoadFrom([]string{"LHA_RESET_KEEP=" + entry}, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResetKeepPaths(); err == nil || !strings.Contains(err.Error(), "LHA_RESET_KEEP") {
			t.Errorf("%q: %v", entry, err)
		}
	}
	s, _ := LoadFrom([]string{"LHA_RESET_KEEP=target,.cache/,build/out"}, "")
	if got, err := s.ResetKeepPaths(); err != nil || strings.Join(got, "|") != "target|.cache/|build/out" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestWorkerDeploymentVersion(t *testing.T) {
	for _, c := range []struct {
		env               []string
		name, build, fail string
	}{
		{nil, "", "", ""},
		{[]string{"LHA_WORKER_DEPLOYMENT=lha", "LHA_WORKER_BUILD_ID=b7"}, "lha", "b7", ""},
		{[]string{"LHA_WORKER_DEPLOYMENT=lha"}, "", "", "LHA_WORKER_DEPLOYMENT and LHA_WORKER_BUILD_ID must be set together (worker versioning)"},
		{[]string{"LHA_WORKER_BUILD_ID=b1"}, "", "", "LHA_WORKER_DEPLOYMENT and LHA_WORKER_BUILD_ID must be set together (worker versioning)"},
		{[]string{"LHA_WORKER_PROMOTE=true"}, "", "", "LHA_WORKER_PROMOTE needs LHA_WORKER_DEPLOYMENT and LHA_WORKER_BUILD_ID"},
		{[]string{"LHA_WORKER_DEPLOYMENT=lha.v2", "LHA_WORKER_BUILD_ID=b1"}, "", "", "LHA_WORKER_DEPLOYMENT 'lha.v2' must not contain '.'"},
	} {
		s, err := LoadFrom(c.env, "")
		if err != nil {
			t.Fatal(err)
		}
		name, build, ok, err := s.WorkerDeploymentVersion()
		if c.fail != "" {
			if err == nil || err.Error() != c.fail {
				t.Errorf("%v: %v", c.env, err)
			}
			continue
		}
		if err != nil || name != c.name || build != c.build || ok != (c.name != "") {
			t.Errorf("%v: %q %q %v %v", c.env, name, build, ok, err)
		}
	}
	if _, err := LoadFrom([]string{"LHA_WORKER_VERSIONING_BEHAVIOR=sometimes"}, ""); err == nil {
		t.Error("an unknown versioning behaviour was accepted")
	}
}
