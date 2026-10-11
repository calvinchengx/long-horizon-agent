package systemone

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
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

func TestReviewQuestionIsAChoiceInOptionOrder(t *testing.T) {
	if ReviewQuestionID != "soundness" || MaxDiffChars != DiffCap {
		t.Fatalf("%q %d", ReviewQuestionID, MaxDiffChars)
	}
	q := ReviewQuestion()
	if q.Type != "choice" || strings.Join(q.options(), ",") != "approve,block" {
		t.Fatalf("%+v", q)
	}
	if instructions, _ := q.Instructions.(string); !strings.Contains(instructions, "blocking defect") {
		t.Fatalf("instructions: %v", q.Instructions)
	}
}

func TestReviewStateIsRedactedAndTakesTheDiffTail(t *testing.T) {
	task := "token ghp_" + strings.Repeat("c", 36)
	diff := strings.Repeat("x", 5000) + " token sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA end"
	state := ReviewState(task, diff, 100)
	taskText, _ := state.String("task")
	diffText, _ := state.String("diff")
	if strings.Contains(taskText, "ghp_") {
		t.Fatalf("task: %q", taskText)
	}
	if len([]rune(diffText)) > 103 || strings.Contains(diffText, "sk-ant-api03") || !strings.HasSuffix(diffText, "end") {
		t.Fatalf("diff: %q", diffText)
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

func soundnessStub(choice string, confidence float64) *Stub {
	return &Stub{Respond: func(state any, questions []Named) map[string]Answer {
		return map[string]Answer{ReviewQuestionID: {
			Type: "choice", Choice: choice, Confidence: confidence,
			Probabilities: contracts.NewOrderedMap(choice, confidence),
		}}
	}}
}

const screenDiff = "--- a/src/x.py\n+++ b/src/x.py\n@@\n-x = 1\n+x = 2\n"

func TestAConfidentBlockAddsTheFindingAndPayload(t *testing.T) {
	stub := soundnessStub("block", 0.95)
	screen := NewReviewScreen(stub)
	if got := screen.Findings(context.Background(), screenDiff, "do the task"); !reflect.DeepEqual(got, []string{"system_one: block (confidence 0.95)"}) {
		t.Fatalf("findings: %v", got)
	}
	v := screen.Assess(context.Background(), screenDiff, "do the task")
	if v.Action != ActionBlock || v.Answer != "block" || !v.Asked {
		t.Fatalf("verdict: %+v", v)
	}
	p := v.Payload("01", 0.9)
	if p.Value("use") != "pre_review" || p.Value("action") != "block" || p.Value("item_id") != "01" ||
		p.Value("model") != "stub-1" || p.Value("threshold") != 0.9 || p.Value("error") != "" ||
		p.Value("confidence") != 0.95 || p.Value("answer") != "block" {
		t.Fatalf("payload: %v", p.Plain())
	}
	probs, _ := p.Value("probabilities").(*contracts.OrderedMap)
	if probs == nil || probs.Value("block") != 0.95 {
		t.Fatalf("probabilities: %v", probs)
	}
}

func TestAnApproveOrALowConfidenceAddsNothing(t *testing.T) {
	for _, c := range []struct {
		choice     string
		confidence float64
	}{{"approve", 0.99}, {"block", 0.5}} {
		screen := NewReviewScreen(soundnessStub(c.choice, c.confidence))
		if got := screen.Findings(context.Background(), screenDiff, "do the task"); len(got) != 0 {
			t.Fatalf("%s %v: findings %v", c.choice, c.confidence, got)
		}
		v := screen.Assess(context.Background(), screenDiff, "do the task")
		if v.Action != ActionContinue || !v.Asked || v.Error != "" {
			t.Fatalf("%s %v: verdict %+v", c.choice, c.confidence, v)
		}
	}
}

func TestAThresholdAboveTheConfidenceAddsNothing(t *testing.T) {
	screen := &ReviewScreen{Model: soundnessStub("block", 0.95), Threshold: 0.99}
	if got := screen.Findings(context.Background(), screenDiff, "do the task"); len(got) != 0 {
		t.Fatalf("findings: %v", got)
	}
}

func TestAFailedCallAddsNothingAndRecordsTheError(t *testing.T) {
	screen := NewReviewScreen(&Stub{Err: "system one call failed: timeout"})
	if got := screen.Findings(context.Background(), screenDiff, "do the task"); len(got) != 0 {
		t.Fatalf("findings: %v", got)
	}
	v := screen.Assess(context.Background(), screenDiff, "do the task")
	if v.Action != ActionContinue || !v.Asked || !strings.Contains(v.Error, "timeout") {
		t.Fatalf("verdict: %+v", v)
	}
}

func TestAnEmptyDiffIsNotAsked(t *testing.T) {
	stub := soundnessStub("block", 0.99)
	screen := NewReviewScreen(stub)
	if got := screen.Findings(context.Background(), "", "do the task"); len(got) != 0 || stub.Calls() != 0 {
		t.Fatalf("findings %v calls %d", got, stub.Calls())
	}
	v := screen.Assess(context.Background(), "", "do the task")
	if v.Asked || v.Action != ActionContinue {
		t.Fatalf("verdict: %+v", v)
	}
}

func TestBuildReviewScreenOnlyWhenEnabledAndConfigured(t *testing.T) {
	if BuildReviewScreen(settings(t), &Stub{}) != nil {
		t.Fatal("screen without the setting")
	}
	if BuildReviewScreen(settings(t, "LHA_SYSTEM_ONE_REVIEW=true"), nil) != nil {
		t.Fatal("screen without a model")
	}
	if s := BuildReviewScreen(settings(t, "LHA_SYSTEM_ONE_REVIEW=true", "LHA_SYSTEM_ONE_REVIEW_THRESHOLD=0.8"), &Stub{}); s == nil || s.Threshold != 0.8 {
		t.Fatalf("screen: %+v", s)
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
