package systemone

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
)

// Ported from python/tests/unit/test_system_one.py (client, build and triage).

const key = "ts-secret-key-0123456789"

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(body))}
}

const answers = `{"model": "jev-1.13.0", "answers": {"urgent": {"type": "noul", "noul": 0.93}},
 "usage": {"input_tokens": 1000000, "output_tokens": 40}}`

var urgent = []Named{{ID: "urgent", Question: Noul("Is it urgent?", nil)}}

func price(p float64) *float64 { return &p }

func meter(ceiling float64) *governor.CostMeter {
	return governor.NewCostMeter(governor.NewCostLedger(), governor.NewBudgetGovernor(ceiling, 100, false))
}

func TestClientPostsWithTheKeyAndMetersInputTokens(t *testing.T) {
	var seen *http.Request
	var body map[string]any
	m := meter(10)
	c, err := NewClient(Options{Model: "jev-1.13.0", APIKey: key, PriceInPerMTok: price(0.042), Meter: m,
		Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			seen = r
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			return reply(200, answers), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Evaluate(context.Background(), contracts.NewOrderedMap("ticket", "x"), urgent)
	if err != nil || result.Answers["urgent"].Noul != 0.93 || result.Model != "jev-1.13.0" {
		t.Fatalf("%+v %v", result, err)
	}
	if seen.URL.String() != TypeSafeEndpoint || seen.Header.Get("Authorization") != "Bearer "+key || body["model"] != "jev-1.13.0" {
		t.Fatalf("request: %s %v %v", seen.URL, seen.Header, body)
	}
	entries := m.Ledger.Entries()
	if len(entries) != 1 || entries[0].Role != Role || !entries[0].CostKnown || entries[0].USD < 0.0419 || entries[0].USD > 0.0421 {
		t.Fatalf("ledger: %+v", entries)
	}
	if strings.Contains(c.String(), key) {
		t.Fatal("the key is in String()")
	}
}

func TestClientErrorsAreSystemOneErrorsWithoutTheKey(t *testing.T) {
	c, _ := NewClient(Options{Model: "m", APIKey: key, PriceInPerMTok: price(0),
		Transport: roundTrip(func(*http.Request) (*http.Response, error) { return reply(401, "{}"), nil })})
	_, err := c.Evaluate(context.Background(), "s", urgent)
	var sysErr *Error
	if !errors.As(err, &sysErr) || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), key) {
		t.Fatalf("err: %v", err)
	}
}

func TestClientRetriesAnOverloadedServerOnce(t *testing.T) {
	calls := 0
	c, _ := NewClient(Options{Model: "m", APIKey: key, PriceInPerMTok: price(0),
		Sleep: func(context.Context, float64) error { return nil },
		Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return reply(529, "{}"), nil
			}
			return reply(200, answers), nil
		})})
	if _, err := c.Evaluate(context.Background(), "s", urgent); err != nil || calls != 2 {
		t.Fatalf("calls %d err %v", calls, err)
	}
}

func TestClientRefusedByTheBudgetIsASystemOneError(t *testing.T) {
	c, _ := NewClient(Options{Model: "m", APIKey: key, PriceInPerMTok: price(1e6), Meter: meter(0.001),
		Transport: roundTrip(func(*http.Request) (*http.Response, error) { return reply(200, answers), nil })})
	_, err := c.Evaluate(context.Background(), "a long enough state", urgent)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err: %v", err)
	}
}

func TestRemoteEndpointMustBePublicHTTPS(t *testing.T) {
	if _, err := NewClient(Options{Model: "m", Endpoint: "http://decisions.example.com/v1/systemone"}); !safety.IsEgressDenied(err) {
		t.Fatalf("err: %v", err)
	}
	c, err := NewClient(Options{Model: "m", Endpoint: "https://decisions.example.com/v1/systemone",
		Resolver: func(context.Context, string, int) ([]string, error) { return []string{"10.0.0.7"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Evaluate(context.Background(), "s", urgent); err == nil || !strings.Contains(err.Error(), "resolves to a non-public address") {
		t.Fatalf("err: %v", err)
	}
}

func TestLoopbackEndpointMayUseHTTPWithoutAKey(t *testing.T) {
	var seen *http.Request
	c, err := NewClient(Options{Model: "kev-latest", Endpoint: "http://127.0.0.1:8009/v1/systemone",
		Transport: roundTrip(func(r *http.Request) (*http.Response, error) { seen = r; return reply(200, answers), nil })})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Evaluate(context.Background(), "s", urgent); err != nil || seen.Header.Get("Authorization") != "" {
		t.Fatalf("err %v auth %q", err, seen.Header.Get("Authorization"))
	}
}

func settings(t *testing.T, env ...string) *config.Settings {
	t.Helper()
	s, err := config.LoadFrom(env, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOffByDefaultAndStubForTests(t *testing.T) {
	if m, err := Build(settings(t), nil); m != nil || err != nil {
		t.Fatalf("%v %v", m, err)
	}
	if m, _ := Build(settings(t, "LHA_SYSTEM_ONE_BACKEND=stub"), nil); m == nil {
		t.Fatal("no stub")
	}
}

func TestUnsafeConfigurationsAreRefused(t *testing.T) {
	cases := map[string][]string{
		"API_KEY":      {},
		"PRIVATE_DATA": {"LHA_SYSTEM_ONE_API_KEY=" + key, "LHA_PRIVATE_DATA=true"},
		"PRICE":        {"LHA_SYSTEM_ONE_API_KEY=" + key, "LHA_SYSTEM_ONE_ENDPOINT=https://kev.example.com/v1/x"},
		"not usable":   {"LHA_SYSTEM_ONE_ENDPOINT=http://kev.example.com/v1/systemone"},
	}
	for want, env := range cases {
		_, err := Build(settings(t, append([]string{"LHA_SYSTEM_ONE_BACKEND=systemone"}, env...)...), nil)
		if !IsConfigError(err) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

func TestUsableConfigurationsBuild(t *testing.T) {
	for _, env := range [][]string{
		{"LHA_SYSTEM_ONE_ENDPOINT=http://127.0.0.1:8009/v1/systemone", "LHA_PRIVATE_DATA=true"},
		{"LHA_SYSTEM_ONE_API_KEY=" + key, "LHA_PRIVATE_DATA=true", "LHA_SYSTEM_ONE_PRIVATE_DATA_OK=true"},
	} {
		m, err := Build(settings(t, append([]string{"LHA_SYSTEM_ONE_BACKEND=systemone"}, env...)...), meter(10))
		if _, ok := m.(*Client); !ok || err != nil {
			t.Errorf("%v: %v %v", env, m, err)
		}
		_ = Close(m)
	}
}

func TestTriageStateIsRedactedAndBounded(t *testing.T) {
	it := contracts.NewChecklistItem("01", "call the API")
	it.Witnesses = []string{"go:TestX"}
	state := TriageState(it, strings.Repeat("x", 5000)+" token sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA end", "", 100)
	latest, _ := state.String("latest_failure")
	if len([]rune(latest)) > 103 || strings.Contains(latest, "sk-ant-api03") {
		t.Fatalf("latest: %q", latest)
	}
}

func TestBuildStallTriageOnlyWhenConfigured(t *testing.T) {
	if BuildStallTriage(settings(t), nil) != nil || BuildStallTriage(settings(t, "LHA_SYSTEM_ONE_TRIAGE=false"), &Stub{}) != nil {
		t.Fatal("triage without a model or with triage off")
	}
	if tr := BuildStallTriage(settings(t, "LHA_SYSTEM_ONE_TRIAGE_THRESHOLD=0.8"), &Stub{}); tr == nil || tr.Threshold != 0.8 || tr.MinFailures != 2 {
		t.Fatalf("triage: %+v", tr)
	}
}

func TestStubAnswersNeutrallyUnlessScripted(t *testing.T) {
	s := &Stub{}
	qs := []Named{
		{ID: "n", Question: Noul("yes?", nil)},
		{ID: "c", Question: Choice("which?", contracts.NewOrderedMap("a", nil, "b", "the other"))},
		{ID: "s", Question: Score("how much?", []any{"low", "mid", "high"})},
	}
	r, err := s.Evaluate(context.Background(), "state", qs)
	if err != nil || s.Name() != "systemone:stub" || s.Calls() != 1 {
		t.Fatalf("%v %s %d", err, s.Name(), s.Calls())
	}
	if r.Answers["n"].Noul != 0.5 || r.Answers["c"].Confidence != 0 || r.Answers["c"].Prob("b") != 0.5 {
		t.Fatalf("neutral: %+v", r.Answers)
	}
	if sc := r.Answers["s"]; sc.Score != 1 || sc.Confidence != 0 || sc.Prob("2") != 1.0/3 {
		t.Fatalf("score: %+v", sc)
	}
	if got := r.JSON(); got.Keys[0] != "model" || got.Value("answers").(*contracts.OrderedMap).Keys[2] != "s" {
		t.Fatalf("json: %+v", got)
	}
}

func TestParseResponseRefusesWhatIsNotAnAnswer(t *testing.T) {
	score := []Named{{ID: "s", Question: Score("how much?", []any{"low", "high"})}}
	for _, body := range []string{
		`not json`,
		`{"answers": {"urgent": {"type": "noul", "noul": 1e999}}}`, // overflows to inf: refused
		`{"answers": {"urgent": {"type": "noul", "noul": "0.5"}}}`,
	} {
		if _, err := ParseResponse([]byte(body), urgent); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	good := `{"answers": {"s": {"type": "score", "score": 0.5, "probabilities": {"0": 0.5, "1": 0.5}, "confidence": 0}}}`
	if r, err := ParseResponse([]byte(good), score); err != nil || r.Answers["s"].Score != 0.5 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestQuestionFromWireRefusesMalformedQuestions(t *testing.T) {
	for _, raw := range []string{
		`[]`,
		`{"type": "essay", "instructions": "x"}`,
		`{"type": "noul", "instructions": "x", "criteria": ["yes"]}`,
		`{"type": "choice", "instructions": "x", "criteria": ["a"]}`,
		`{"type": "score", "instructions": "x", "criteria": {"a": null}}`,
	} {
		v, _ := contracts.DecodeOrdered([]byte(raw))
		if _, err := QuestionFromWire(v); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	v, _ := contracts.DecodeOrdered([]byte(`{"type": "noul", "instructions": "x", "criteria": {"true": "yes"}}`))
	if q, err := QuestionFromWire(v); err != nil || q.Criteria == nil {
		t.Fatalf("%+v %v", q, err)
	}
}

func TestUnpricedEndpointHasNoWorstCase(t *testing.T) {
	c, _ := NewClient(Options{Model: "m", Endpoint: "https://kev.example.com/v1/systemone", APIKey: key})
	if c.WorstCaseUSD([]byte("{}")) != nil {
		t.Fatal("an unpriced endpoint has a worst case")
	}
	if _, err := NewClient(Options{Model: "m", Endpoint: "ftp://x/"}); err == nil {
		t.Fatal("a malformed endpoint was accepted")
	}
}
