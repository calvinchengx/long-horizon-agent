package model

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Ported from python/tests/unit/test_model_failover_health.py (no real network: providers share
// an *http.Client over an in-memory RoundTripper).

var hiMsg = []contracts.ModelMessage{{Role: "user", Content: "hi"}}

func TestParseFallbackEntries(t *testing.T) {
	if s, err := ParseFallbackEntry("ollama:qwen3:8b"); err != nil || s.Model != "qwen3:8b" || s.Backend != "ollama" {
		t.Fatalf("%+v %v", s, err)
	}
	priced, err := ParseFallbackEntry(" OpenAI_Compat:llama-3.3-70b@0.59/0.79 ")
	if err != nil || priced.Backend != "openai_compat" || priced.Model != "llama-3.3-70b" || priced.Price == nil ||
		*priced.Price != NewModelPrice(0.59, 0.79) {
		t.Fatalf("%+v %v", priced, err)
	}
	if s, _ := ParseFallbackEntry("claude:claude-haiku-4-5"); s.Price != nil {
		t.Fatal("unexpected price")
	}
	if s, err := ParseFallbackEntry("openai_compat:m@1_0/2e1"); err != nil || s.Price.InputPerMTok != 10 || s.Price.OutputPerMTok != 20 {
		t.Fatalf("python float syntax: %+v %v", s, err)
	}
	if s, err := ParseFallbackEntry("openai_compat:a@b@ 1 / 2 "); err != nil || s.Model != "a@b" || s.Price.OutputPerMTok != 2 {
		t.Fatalf("last @ splits the price: %+v %v", s, err)
	}
}

func TestParseFallbackEntryErrors(t *testing.T) {
	const expected = ": expected 'backend:model[@in/out]' with backend one of stub, ollama, openai_compat, claude, claude_code"
	const badPrice = ": expected '@<in>/<out>' USD per 1M tokens"
	for entry, want := range map[string]string{
		"claude":                "invalid LHA_FALLBACK_MODELS entry 'claude'" + expected,
		"gpt:4o":                "invalid LHA_FALLBACK_MODELS entry 'gpt:4o'" + expected,
		"claude:":               "invalid LHA_FALLBACK_MODELS entry 'claude:': empty model name",
		"openai_compat:m@abc":   "invalid price in LHA_FALLBACK_MODELS entry 'openai_compat:m@abc'" + badPrice,
		"openai_compat:m@1":     "invalid price in LHA_FALLBACK_MODELS entry 'openai_compat:m@1'" + badPrice,
		"openai_compat:m@-1/2":  "invalid price in LHA_FALLBACK_MODELS entry 'openai_compat:m@-1/2'" + badPrice,
		"openai_compat:m@0x1/2": "invalid price in LHA_FALLBACK_MODELS entry 'openai_compat:m@0x1/2'" + badPrice,
		"openai_compat:@1/2":    "invalid LHA_FALLBACK_MODELS entry 'openai_compat:@1/2': empty model name",
	} {
		if _, err := ParseFallbackEntry(entry); err == nil || err.Error() != want {
			t.Errorf("%q: %v\nwant %s", entry, err, want)
		}
	}
}

func TestNoFallbacksReturnsTheSingleBackend(t *testing.T) {
	p, err := BuildProvider(settingsFrom(t), "", nil)
	if _, ok := p.(*StubModel); !ok || err != nil {
		t.Fatalf("%T %v", p, err)
	}
}

func chainSettings(extra ...string) []string {
	return append([]string{"LHA_MODEL_BACKEND=claude", "LHA_MODEL_NAME=claude-sonnet-4-6", "LHA_ANTHROPIC_API_KEY=sk-ant-test",
		"LHA_OPENAI_BASE_URL=https://api.groq.test/openai/v1"}, extra...)
}

func TestFallbacksBuildAFailoverChainInOrder(t *testing.T) {
	s := settingsFrom(t, chainSettings(
		"LHA_FALLBACK_MODELS= openai_compat:llama-3.3-70b@0.59/0.79, ollama:qwen3:8b,,claude:claude-haiku-4-5",
		"LHA_FALLBACK_MAX_ROUNDS=3")...)
	p, err := BuildProvider(s, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := p.(*FailoverModel)
	if !ok {
		t.Fatalf("%T", p)
	}
	if f.Name() != "failover:claude:claude-sonnet-4-6,openai_compat:llama-3.3-70b,ollama:qwen3:8b,claude:claude-haiku-4-5" {
		t.Fatal(f.Name())
	}
	if _, ok := f.providers[0].(*ClaudeModel); !ok {
		t.Fatalf("%T", f.providers[0])
	}
	if _, ok := f.providers[1].(*OpenAICompatModel); !ok {
		t.Fatalf("%T", f.providers[1])
	}
	for _, m := range f.providers {
		var retries int
		switch x := m.(type) {
		case *ClaudeModel:
			retries = x.retry.MaxRetries
		case *OpenAICompatModel:
			retries = x.retry.MaxRetries
		}
		if retries != ChainMemberRetries {
			t.Fatalf("%s retries %d", m.Name(), retries)
		}
	}
	if f.maxRounds != 3 {
		t.Fatal(f.maxRounds)
	}
	routed, _ := BuildProvider(s, "claude-opus-4-8", nil)
	if !strings.HasPrefix(routed.Name(), "failover:claude:claude-opus-4-8,openai_compat:") {
		t.Fatal(routed.Name())
	}
	single, _ := BuildProvider(settingsFrom(t, chainSettings()...), "", nil)
	if single.(*ClaudeModel).retry.MaxRetries != 3 {
		t.Fatal("a lone provider keeps 3 retries")
	}
}

func TestFallbackConfigErrorsSurfaceAtBuild(t *testing.T) {
	for _, c := range []struct {
		env  []string
		want string
	}{
		{[]string{"LHA_FALLBACK_MODELS=openai_compat:m"}, "LHA_OPENAI_BASE_URL"},
		{[]string{"LHA_FALLBACK_MODELS=claude:claude-haiku-4-5"}, "LHA_ANTHROPIC_API_KEY"},
		{[]string{"LHA_FALLBACK_MODELS=gpt:4o"}, "invalid LHA_FALLBACK_MODELS entry 'gpt:4o'"},
		{[]string{"LHA_MODEL_BACKEND=openai_compat", "LHA_OPENAI_BASE_URL=https://x.test/v1", "LHA_OPENAI_PRICE_IN_PER_MTOK=1.0"}, "or neither"},
	} {
		if _, err := BuildProvider(settingsFrom(t, c.env...), "", nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v", c.env, err)
		}
	}
}

func TestFailoverServesFromFallbackAndIsPricedByTheServingModel(t *testing.T) {
	var hosts []string
	var mc mockClient
	client := mc.client(t, func(_ int, r *http.Request) *http.Response {
		hosts = append(hosts, r.URL.Host)
		if r.URL.Host == "api.anthropic.com" {
			return jsonResponse(529, map[string]any{"error": "overloaded"}, "retry-after", "0")
		}
		return jsonResponse(200, map[string]any{
			"model":   "llama-3.3-70b-versatile",
			"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1_000_000, "completion_tokens": 1_000_000},
		})
	})
	p, err := BuildProvider(settingsFrom(t, chainSettings("LHA_FALLBACK_MODELS=openai_compat:llama-3.3-70b@0.59/0.79")...), "", client)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Complete(context.Background(), hiMsg, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"api.anthropic.com", "api.anthropic.com", "api.groq.test"} // 1 + ChainMemberRetries, then the fallback
	if strings.Join(hosts, ",") != strings.Join(want, ",") {
		t.Fatalf("hosts %v", hosts)
	}
	if result.Usage.Provider != "openai_compat:llama-3.3-70b" || result.Usage.Model != "llama-3.3-70b-versatile" {
		t.Fatalf("%+v", result.Usage)
	}
	usd, err := p.EstimateCostUSD(result.Usage)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "served cost", usd, 0.59+0.79)
	claudeCost, _ := p.EstimateCostUSD(contracts.Usage{InputTokens: 1_000_000, Provider: "claude:claude-sonnet-4-6"})
	approx(t, "claude cost", claudeCost, 3.0)
}

// --- health probes ---------------------------------------------------------------------------

func probe(t *testing.T, env []string, handler func(r *http.Request) (*http.Response, error)) ModelHealth {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := handler(r)
		if resp != nil {
			resp.Request = r
		}
		return resp, err
	})}
	return ProbeModel(context.Background(), settingsFrom(t, env...), 0, client)
}

func TestStubProbeIsHealthy(t *testing.T) {
	h := ProbeModel(context.Background(), settingsFrom(t), 0, nil)
	if !h.OK || !strings.Contains(h.Detail, "stub") {
		t.Fatalf("%+v", h)
	}
}

func TestOpenAICompatProbeListsModels(t *testing.T) {
	var seen []*http.Request
	h := probe(t, []string{"LHA_MODEL_BACKEND=openai_compat", "LHA_OPENAI_BASE_URL=https://api.groq.test/openai/v1",
		"LHA_OPENAI_API_KEY=gsk-key", "LHA_MODEL_NAME=m"}, func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r)
		return jsonResponse(200, map[string]any{"data": []any{}}), nil
	})
	if !h.OK || h.Detail != "openai_compat:m: reachable" || len(seen) != 1 ||
		seen[0].URL.String() != "https://api.groq.test/openai/v1/models" || seen[0].Method != "GET" ||
		seen[0].Header.Get("Authorization") != "Bearer gsk-key" {
		t.Fatalf("%+v", h)
	}
}

func TestOpenAICompatProbeReportsDown(t *testing.T) {
	env := []string{"LHA_MODEL_BACKEND=openai_compat", "LHA_OPENAI_BASE_URL=https://x.test/v1"}
	for _, code := range []int{401, 503} {
		h := probe(t, env, func(*http.Request) (*http.Response, error) { return jsonResponse(code, nil), nil })
		want := "openai_compat:stub-1: HTTP " + strconv.Itoa(code) + " from https://x.test/v1/models"
		if h.OK || h.Detail != want {
			t.Fatalf("%d: %+v", code, h)
		}
	}
	refused := probe(t, env, func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") })
	if refused.OK || !strings.Contains(refused.Detail, "connection refused") {
		t.Fatalf("%+v", refused)
	}
	timedOut := probe(t, env, func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })
	if timedOut.OK || !strings.Contains(timedOut.Detail, "timed out") {
		t.Fatalf("%+v", timedOut)
	}
}

func TestOllamaProbeChecksTheModelIsPulled(t *testing.T) {
	for _, c := range []struct {
		tags  []any
		model string
		ok    bool
	}{
		{[]any{map[string]any{"name": "qwen3:8b"}}, "qwen3:8b", true},
		{[]any{map[string]any{"name": "llama3:latest"}}, "llama3", true},
		{[]any{map[string]any{"name": "llama3:latest"}, "junk"}, "qwen3:8b", false},
	} {
		var seen []string
		h := probe(t, []string{"LHA_MODEL_BACKEND=ollama", "LHA_MODEL_NAME=" + c.model, "LHA_OLLAMA_BASE_URL=http://ollama.test:11434/"},
			func(r *http.Request) (*http.Response, error) {
				seen = append(seen, r.URL.String())
				return jsonResponse(200, map[string]any{"models": c.tags}), nil
			})
		if h.OK != c.ok || len(seen) != 1 || seen[0] != "http://ollama.test:11434/api/tags" {
			t.Fatalf("%+v %v", h, seen)
		}
		if !c.ok && h.Detail != "ollama:qwen3:8b: model 'qwen3:8b' is not pulled" {
			t.Fatal(h.Detail)
		}
	}
	garbage := probe(t, []string{"LHA_MODEL_BACKEND=ollama", "LHA_MODEL_NAME=m"},
		func(*http.Request) (*http.Response, error) { return jsonResponse(200, "not json"), nil })
	if garbage.OK {
		t.Fatal("garbage reported healthy")
	}
	list := probe(t, []string{"LHA_MODEL_BACKEND=ollama", "LHA_MODEL_NAME=m"},
		func(*http.Request) (*http.Response, error) { return jsonResponse(200, []any{}), nil })
	if list.OK || !strings.Contains(list.Detail, "AttributeError: 'list' object has no attribute 'get'") {
		t.Fatalf("%+v", list)
	}
}

func TestClaudeProbeGetsTheModel(t *testing.T) {
	var seen []*http.Request
	handler := func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r)
		code := 404
		if strings.Contains(r.URL.Path, "haiku") {
			code = 200
		}
		return jsonResponse(code, nil), nil
	}
	base := []string{"LHA_MODEL_BACKEND=claude", "LHA_ANTHROPIC_API_KEY=sk-ant-test"}
	ok := probe(t, append(base, "LHA_MODEL_NAME=claude-haiku-4-5"), handler)
	missing := probe(t, append(base, "LHA_MODEL_NAME=claude-sonnet-4-6"), handler)
	if !ok.OK || missing.OK || !strings.Contains(missing.Detail, "HTTP 404") {
		t.Fatalf("%+v %+v", ok, missing)
	}
	if seen[0].URL.String() != "https://api.anthropic.com/v1/models/claude-haiku-4-5" ||
		seen[0].Header.Get("x-api-key") != "sk-ant-test" || seen[0].Header.Get("anthropic-version") != ClaudeAPIVersion {
		t.Fatalf("%v %v", seen[0].URL, seen[0].Header)
	}
}

func TestFailoverProbeIsHealthyIfAnyMemberIs(t *testing.T) {
	env := []string{"LHA_MODEL_BACKEND=claude", "LHA_MODEL_NAME=claude-haiku-4-5", "LHA_ANTHROPIC_API_KEY=sk-ant-test",
		"LHA_OPENAI_BASE_URL=https://x.test/v1", "LHA_FALLBACK_MODELS=openai_compat:m"}
	h := probe(t, env, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.anthropic.com" {
			return jsonResponse(503, nil), nil
		}
		return jsonResponse(200, map[string]any{}), nil
	})
	if !h.OK || !strings.Contains(h.Detail, "HTTP 503") || !strings.Contains(h.Detail, "openai_compat:m: reachable") {
		t.Fatalf("%+v", h)
	}
	down := probe(t, env, func(*http.Request) (*http.Response, error) { return jsonResponse(500, nil), nil })
	if down.OK {
		t.Fatal("all members down reported healthy")
	}
}

// bare is a provider without a probe.
type bare struct{ name string }

func (b bare) Name() string { return b.name }
func (bare) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	return contracts.TurnResult{}, nil
}
func (bare) EstimateCostUSD(contracts.Usage) (float64, error) { return 0, nil }

// hangs never answers its probe in time.
type hangs struct{ bare }

func (hangs) HealthCheck(ctx context.Context, _ time.Duration) ModelHealth {
	<-ctx.Done()
	time.Sleep(50 * time.Millisecond)
	return ModelHealth{true, "too late"}
}

func TestProbeProviderEdgeCases(t *testing.T) {
	built := ProbeProvider(context.Background(), bare{"custom:x"}, time.Second)
	if !built.OK || built.Detail != "custom:x: no probe available (built only)" {
		t.Fatalf("%+v", built)
	}
	hung := ProbeProvider(context.Background(), hangs{bare{"h"}}, 10*time.Millisecond)
	if hung.OK || !strings.Contains(hung.Detail, "timed out") {
		t.Fatalf("%+v", hung)
	}
}

func TestProbeReportsConfigErrors(t *testing.T) {
	h := ProbeModel(context.Background(), settingsFrom(t, "LHA_MODEL_BACKEND=openai_compat"), 0, nil)
	if h.OK || h.Detail != "ValueError: LHA_OPENAI_BASE_URL is required for the 'openai_compat' backend." {
		t.Fatalf("%+v", h)
	}
}
