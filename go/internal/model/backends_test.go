package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

var ctx = context.Background()

func TestOpenAICompatMapsRequestAndResponse(t *testing.T) {
	mc := &mockClient{}
	client := mc.client(t, func(int, *http.Request) *http.Response {
		return jsonResponse(200, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "hello world"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 3},
		})
	})
	m := mustOpenAI(t, OpenAICompatOptions{BaseURL: "http://local/v1", ModelName: "m1", Client: client,
		PriceInPerMTok: Float(1.0), PriceOutPerMTok: Float(2.0)})
	result, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(mc.requests[0].URL, "/chat/completions") {
		t.Errorf("url = %s", mc.requests[0].URL)
	}
	if mc.body(t, 0)["model"] != "m1" {
		t.Errorf("body = %s", mc.requests[0].Body)
	}
	if result.Text != "hello world" || result.Usage.InputTokens != 12 || result.Usage.OutputTokens != 3 {
		t.Errorf("result = %+v", result)
	}
	if *result.StopReason != "stop" || result.Usage.Model != "m1" || result.Usage.Provider != "openai_compat:m1" {
		t.Errorf("result = %+v", result)
	}
	cost, err := m.EstimateCostUSD(result.Usage)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "cost", cost, 12/1e6*1.0+3/1e6*2.0)
	if h := mc.requests[0].Header.Get("Authorization"); h != "" {
		t.Errorf("no api key => no Authorization header, got %q", h)
	}
}

func TestOpenAICompatParsesToolCalls(t *testing.T) {
	mc := &mockClient{}
	client := mc.client(t, func(int, *http.Request) *http.Response {
		return jsonResponse(200, `{"model":"served-model","choices":[{"message":{"content":null,"tool_calls":[
			{"id":"t1","function":{"name":"search","arguments":"{\"q\": \"x\", \"n\": 2}"}},
			{"id":"t2","function":{"name":"bad","arguments":"not json"}},
			{"id":"t3","function":{"name":"list","arguments":"[1]"}},
			{"id":"t4","function":{"name":"obj","arguments":{"k":1}}},
			{"id":5,"function":{"name":null}},
			"junk"
		]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	m := mustOpenAI(t, OpenAICompatOptions{ModelName: "m1", Client: client})
	result, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []contracts.ToolCall{
		{ID: "t1", Name: "search", Arguments: map[string]any{"q": "x", "n": 2.0}},
		{ID: "t2", Name: "bad", Arguments: map[string]any{"_raw": "not json"}},
		{ID: "t3", Name: "list", Arguments: map[string]any{"_raw": "[1]"}},
		{ID: "t4", Name: "obj", Arguments: map[string]any{"k": 1.0}},
		{ID: "5", Name: "None", Arguments: map[string]any{}},
	}
	if !reflect.DeepEqual(result.ToolCalls, want) {
		t.Errorf("tool calls = %#v", result.ToolCalls)
	}
	if result.Text != "" || result.Usage.Model != "served-model" {
		t.Errorf("result = %+v", result)
	}
}

func TestOpenAICompatMalformedResponses(t *testing.T) {
	for _, body := range []string{
		`not json`, `[]`, `{}`, `{"choices":[]}`, `{"choices":[1]}`,
		`{"choices":[{"message":null}]}`, `{"choices":[{"message":{"content":5}}]}`,
		`{"choices":[{"message":{"tool_calls":[{"function":1}]}}]}`,
		`{"choices":[{"message":{}}],"usage":{"prompt_tokens":"x"}}`,
		`{"choices":[{"message":{}}],"usage":{"completion_tokens":[1]}}`,
	} {
		mc := &mockClient{}
		client := mc.client(t, func(int, *http.Request) *http.Response { return jsonResponse(200, body) })
		m := mustOpenAI(t, OpenAICompatOptions{Client: client, Sleep: (&sleeps{}).sleep})
		if _, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0); err == nil {
			t.Errorf("body %s: expected an error", body)
		} else if mc.count() != 1 {
			t.Errorf("body %s: malformed responses are not retried", body)
		}
	}
}

func TestClaudeMapsSystemContentBlocksAndCost(t *testing.T) {
	mc := &mockClient{}
	client := mc.client(t, func(int, *http.Request) *http.Response {
		return jsonResponse(200, map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "hi there"},
				map[string]any{"type": "thinking", "thinking": "hmm"},
				map[string]any{"type": "tool_use", "id": "u1", "name": "edit", "input": map[string]any{"path": "a.py"}},
			},
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 4},
			"stop_reason": "tool_use",
		})
	})
	m := mustClaude(t, ClaudeOptions{APIKey: "sk-test", Client: client})
	result, err := m.Complete(ctx, []contracts.ModelMessage{
		{Role: "system", Content: "be terse"},
		user("hi"),
	}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := mc.body(t, 0)
	if body["system"] != "be terse" {
		t.Errorf("system = %v", body["system"])
	}
	if !reflect.DeepEqual(body["messages"], []any{map[string]any{"role": "user", "content": "hi"}}) {
		t.Errorf("messages = %v", body["messages"])
	}
	req := mc.requests[0]
	if req.URL != ClaudeEndpoint || req.Header.Get("x-api-key") != "sk-test" ||
		req.Header.Get("anthropic-version") != "2023-06-01" || req.Header.Get("content-type") != "application/json" {
		t.Errorf("request = %s %v", req.URL, req.Header)
	}
	if result.Text != "hi there" || result.ToolCalls[0].Name != "edit" ||
		!reflect.DeepEqual(result.ToolCalls[0].Arguments, map[string]any{"path": "a.py"}) {
		t.Errorf("result = %+v", result)
	}
	if result.Thinking != nil {
		t.Errorf("python does not surface thinking blocks; got %q", *result.Thinking)
	}
	if result.Usage.InputTokens != 10 || result.Usage.Model != "claude-sonnet-4-6" || *result.StopReason != "tool_use" {
		t.Errorf("usage = %+v", result.Usage)
	}
	cost, _ := m.EstimateCostUSD(result.Usage)
	approx(t, "cost", cost, 10/1e6*3.0+4/1e6*15.0)
}

func TestClaudeUsageRecordsReportedModelAndCacheBreakdown(t *testing.T) {
	mc := &mockClient{}
	client := mc.client(t, func(int, *http.Request) *http.Response {
		return jsonResponse(200, map[string]any{
			"model":   "claude-haiku-4-5-20251001",
			"content": []any{map[string]any{"type": "text", "text": "ok"}},
			"usage": map[string]any{
				"input_tokens": 5, "output_tokens": 2, "cache_read_input_tokens": 7,
				"cache_creation_input_tokens": 11,
				"cache_creation":              map[string]any{"ephemeral_5m_input_tokens": 8, "ephemeral_1h_input_tokens": 3},
			},
		})
	})
	m := mustClaude(t, ClaudeOptions{Client: client})
	result, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	u := result.Usage
	if u.Model != "claude-haiku-4-5-20251001" || u.CacheReadInputTokens != 7 || u.CacheCreationInputTokens != 11 ||
		u.CacheCreation1hInputTokens != 3 || u.Provider != "claude:claude-sonnet-4-6" || result.StopReason != nil {
		t.Errorf("usage = %+v", result)
	}
}

func TestClaudeMalformedResponses(t *testing.T) {
	for _, body := range []string{
		`nope`, `[1]`, `{"content":null}`, `{"content":[1]}`, `{"content":[{"type":"text","text":null}]}`,
		`{"content":[{"type":"tool_use","input":[1]}]}`, `{"usage":[1]}`,
		`{"usage":{"input_tokens":"5.0"}}`, `{"usage":{"cache_creation":{"ephemeral_1h_input_tokens":{"a":1}}}}`,
	} {
		mc := &mockClient{}
		client := mc.client(t, func(int, *http.Request) *http.Response { return jsonResponse(200, body) })
		m := mustClaude(t, ClaudeOptions{Client: client})
		if _, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0); err == nil {
			t.Errorf("body %s: expected an error", body)
		}
	}
	// Lenient paths Python also accepts.
	mc := &mockClient{}
	client := mc.client(t, func(int, *http.Request) *http.Response {
		return jsonResponse(200, `{"content":[{"type":"tool_use"}],"usage":{"input_tokens":"7","output_tokens":2.9,"cache_read_input_tokens":true,"cache_creation":[1]},"model":""}`)
	})
	m := mustClaude(t, ClaudeOptions{Client: client})
	r, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := contracts.Usage{InputTokens: 7, OutputTokens: 2, CacheReadInputTokens: 1, Model: "claude-sonnet-4-6", Provider: "claude:claude-sonnet-4-6"}
	if r.Usage != want || !reflect.DeepEqual(r.ToolCalls, []contracts.ToolCall{{Arguments: map[string]any{}}}) {
		t.Errorf("result = %+v", r)
	}
}

func TestOpenAICompatWithoutPricesIsUnknownNotZero(t *testing.T) {
	m := mustOpenAI(t, OpenAICompatOptions{})
	_, err := m.EstimateCostUSD(contracts.Usage{InputTokens: 10, OutputTokens: 10, Model: "m"})
	if !errors.Is(err, contracts.ErrUnknownPrice) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "model 'm' on provider 'openai_compat:m'") {
		t.Errorf("err = %v", err)
	}
	free := mustOpenAI(t, OpenAICompatOptions{PriceInPerMTok: Float(0), PriceOutPerMTok: Float(0)})
	if c, err := free.EstimateCostUSD(contracts.Usage{InputTokens: 10, OutputTokens: 10}); c != 0 || err != nil {
		t.Errorf("free = %v, %v", c, err)
	}
	if _, err := NewOpenAICompat(OpenAICompatOptions{BaseURL: "http://x", ModelName: "m", PriceInPerMTok: Float(1)}); err == nil ||
		err.Error() != "configure both price_in_per_mtok and price_out_per_mtok, or neither" {
		t.Errorf("err = %v", err)
	}
}

func TestClaudeRetries429WithRetryAfterThenSucceeds(t *testing.T) {
	mc := &mockClient{}
	client := mc.client(t, func(n int, _ *http.Request) *http.Response {
		if n < 3 {
			return jsonResponse(429, map[string]any{"error": "rate"}, "retry-after", "2")
		}
		return jsonResponse(200, okClaude)
	})
	s := &sleeps{}
	m := mustClaude(t, ClaudeOptions{Client: client, Sleep: s.sleep})
	result, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "ok" || mc.count() != 3 || !reflect.DeepEqual(s.calls, []float64{2, 2}) {
		t.Errorf("text=%q attempts=%d sleeps=%v", result.Text, mc.count(), s.calls)
	}
}

func TestClaudeDoesNotRetryClientErrors(t *testing.T) {
	mc := &mockClient{}
	client := mc.client(t, func(int, *http.Request) *http.Response {
		return jsonResponse(400, map[string]any{"error": "bad request"})
	})
	m := mustClaude(t, ClaudeOptions{Client: client, Sleep: (&sleeps{}).sleep})
	_, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0)
	var status *HTTPStatusError
	if !errors.As(err, &status) || status.StatusCode != 400 || mc.count() != 1 {
		t.Fatalf("err = %v attempts = %d", err, mc.count())
	}
	want := "Client error '400 Bad Request' for url 'https://api.anthropic.com/v1/messages'\n" +
		"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/400"
	if err.Error() != want {
		t.Errorf("message = %q", err.Error())
	}
	if string(status.Body) != `{"error":"bad request"}` {
		t.Errorf("body = %s", status.Body)
	}
}

func TestClaudeRetriesAreBounded(t *testing.T) {
	mc := &mockClient{}
	client := mc.client(t, func(int, *http.Request) *http.Response { return jsonResponse(503, nil) })
	m := mustClaude(t, ClaudeOptions{Client: client, Sleep: (&sleeps{}).sleep, MaxRetries: Int(2)})
	_, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0)
	var status *HTTPStatusError
	if !errors.As(err, &status) || mc.count() != 3 {
		t.Fatalf("err = %v attempts = %d", err, mc.count())
	}
}

func TestTransportErrorsAreRetried(t *testing.T) {
	n := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n++
		if n == 1 {
			return nil, errors.New("connection reset by peer")
		}
		return jsonResponse(200, okClaude), nil
	})}
	s := &sleeps{}
	m := mustClaude(t, ClaudeOptions{Client: client, Sleep: s.sleep, RetryBaseDelay: Float(0.5)})
	if _, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0); err != nil {
		t.Fatal(err)
	}
	if n != 2 || !reflect.DeepEqual(s.calls, []float64{0.5}) {
		t.Errorf("attempts=%d sleeps=%v", n, s.calls)
	}
}

func TestBodyReadErrorIsRetryable(t *testing.T) {
	n := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n++
		if n == 1 {
			resp := jsonResponse(200, nil)
			resp.Body = errReader{}
			return resp, nil
		}
		return jsonResponse(200, okClaude), nil
	})}
	m := mustClaude(t, ClaudeOptions{Client: client, Sleep: (&sleeps{}).sleep})
	if _, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0); err != nil || n != 2 {
		t.Fatalf("err=%v attempts=%d", err, n)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }
func (errReader) Close() error             { return nil }

func TestOpenAICompatSendsToolCallIDs(t *testing.T) {
	mc := &mockClient{}
	client := mc.client(t, func(int, *http.Request) *http.Response {
		return jsonResponse(200, `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`)
	})
	m := mustOpenAI(t, OpenAICompatOptions{Client: client})
	if _, err := m.Complete(ctx, toolConversation, nil, 0); err != nil {
		t.Fatal(err)
	}
	msgs := mc.body(t, 0)["messages"].([]any)
	asst := msgs[1].(map[string]any)
	calls := asst["tool_calls"].([]any)
	if calls[0].(map[string]any)["id"] != "call_1" || asst["content"] != nil {
		t.Errorf("assistant = %v", asst)
	}
	args := calls[1].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil || !reflect.DeepEqual(parsed, map[string]any{"path": "b"}) {
		t.Errorf("arguments = %q", args)
	}
	if !reflect.DeepEqual(msgs[2], map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "A"}) ||
		!reflect.DeepEqual(msgs[3], map[string]any{"role": "tool", "tool_call_id": "call_2", "content": "B"}) {
		t.Errorf("tool messages = %v %v", msgs[2], msgs[3])
	}
}

func TestOpenAICompatNeverSendsToolRoleWithoutID(t *testing.T) {
	mc := &mockClient{}
	client := mc.client(t, func(int, *http.Request) *http.Response {
		return jsonResponse(200, `{"choices":[{"message":{"content":"x"}}]}`)
	})
	m := mustOpenAI(t, OpenAICompatOptions{Client: client})
	if _, err := m.Complete(ctx, []contracts.ModelMessage{
		{Role: "tool", Content: "orphan result"},
		{Role: "tool", Content: "empty id", ToolCallID: contracts.Str("")},
	}, nil, 0); err != nil {
		t.Fatal(err)
	}
	msgs := mc.body(t, 0)["messages"].([]any)
	for _, msg := range msgs {
		if msg.(map[string]any)["role"] != "user" {
			t.Errorf("message = %v", msg)
		}
	}
	if msgs[0].(map[string]any)["content"] != "TOOL RESULT:\norphan result" {
		t.Errorf("content = %v", msgs[0])
	}
}

func TestClaudeMapsToolUseAndMergesToolResults(t *testing.T) {
	mc := &mockClient{}
	client := mc.client(t, func(int, *http.Request) *http.Response { return jsonResponse(200, okClaude) })
	m := mustClaude(t, ClaudeOptions{Client: client})
	conv := append(append([]contracts.ModelMessage{}, toolConversation...),
		contracts.ModelMessage{Role: "assistant", Content: "", ToolCalls: []contracts.ToolCall{{ID: "call_3", Name: "noop"}}},
		contracts.ModelMessage{Role: "tool", Content: "C", ToolCallID: contracts.Str("call_3")},
		user("next"),
		contracts.ModelMessage{Role: "tool", Content: "D", ToolCallID: contracts.Str("call_4")},
	)
	if _, err := m.Complete(ctx, conv, nil, 0); err != nil {
		t.Fatal(err)
	}
	msgs := mc.body(t, 0)["messages"].([]any)
	if len(msgs) != 7 { // both tool results of the first turn merged into one user turn
		t.Fatalf("messages = %v", msgs)
	}
	var types []any
	for _, b := range msgs[1].(map[string]any)["content"].([]any) {
		types = append(types, b.(map[string]any)["type"])
	}
	if !reflect.DeepEqual(types, []any{"tool_use", "tool_use"}) {
		t.Errorf("types = %v", types)
	}
	var ids []any
	for _, b := range msgs[2].(map[string]any)["content"].([]any) {
		ids = append(ids, b.(map[string]any)["tool_use_id"])
	}
	if !reflect.DeepEqual(ids, []any{"call_1", "call_2"}) {
		t.Errorf("ids = %v", ids)
	}
	noop := msgs[3].(map[string]any)["content"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(noop["input"], map[string]any{}) {
		t.Errorf("nil arguments must be sent as {}: %v", noop)
	}
	if msgs[5].(map[string]any)["content"] != "next" || len(msgs[6].(map[string]any)["content"].([]any)) != 1 {
		t.Errorf("a tool result after a plain user turn starts a new turn: %v", msgs[5:])
	}
}

// The exact request bytes Python's httpx sends for the same conversation (captured from the
// Python implementation with httpx.MockTransport).
func TestRequestBodiesMatchPythonBytes(t *testing.T) {
	conv := []contracts.ModelMessage{
		{Role: "system", Content: "sys <&> \u00fc"},
		user("read it"),
		{Role: "assistant", Content: "thinking", ToolCalls: []contracts.ToolCall{
			{ID: "c1", Name: "read_file", Arguments: map[string]any{"path": "a/\u00fc\n\"x\""}},
		}},
		{Role: "tool", Content: "A", ToolCallID: contracts.Str("c1")},
		{Role: "tool", Content: "orphan"},
		{Role: "developer", Content: "d"},
	}
	mc := &mockClient{}
	client := mc.client(t, func(_ int, r *http.Request) *http.Response {
		if strings.Contains(r.URL.Host, "anthropic") {
			return jsonResponse(200, `{"content":[{"type":"text","text":"ok"}],"usage":{}}`)
		}
		return jsonResponse(200, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	claude := mustClaude(t, ClaudeOptions{Client: client})
	if _, err := claude.Complete(ctx, conv, []map[string]any{{"name": "t"}}, 0); err != nil {
		t.Fatal(err)
	}
	openai := mustOpenAI(t, OpenAICompatOptions{BaseURL: "http://x/v1/", APIKey: "key", Client: client})
	if _, err := openai.Complete(ctx, conv, nil, 7); err != nil {
		t.Fatal(err)
	}
	wantClaude := `{"model":"claude-sonnet-4-6","max_tokens":4096,"messages":[{"role":"user","content":"read it"},{"role":"assistant","content":[{"type":"text","text":"thinking"},{"type":"tool_use","id":"c1","name":"read_file","input":{"path":"a/ü\n\"x\""}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"A"}]},{"role":"user","content":"TOOL RESULT:\norphan"},{"role":"user","content":"d"}],"system":"sys <&> ü","tools":[{"name":"t"}]}`
	wantOpenAI := `{"model":"m","messages":[{"role":"system","content":"sys <&> ü"},{"role":"user","content":"read it"},{"role":"assistant","content":"thinking","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\": \"a/\\u00fc\\n\\\"x\\\"\"}"}}]},{"role":"tool","tool_call_id":"c1","content":"A"},{"role":"user","content":"TOOL RESULT:\norphan"},{"role":"developer","content":"d"}],"max_tokens":7}`
	if got := string(mc.requests[0].Body); got != wantClaude {
		t.Errorf("claude body:\n got  %s\n want %s", got, wantClaude)
	}
	if got := string(mc.requests[1].Body); got != wantOpenAI {
		t.Errorf("openai body:\n got  %s\n want %s", got, wantOpenAI)
	}
	if mc.requests[1].URL != "http://x/v1/chat/completions" || mc.requests[1].Header.Get("Authorization") != "Bearer key" ||
		mc.requests[1].Header.Get("Content-Type") != "application/json" {
		t.Errorf("openai request = %s %v", mc.requests[1].URL, mc.requests[1].Header)
	}
}

func TestOwnedHTTPClientIsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okClaude)
	}))
	defer srv.Close()

	m := mustClaude(t, ClaudeOptions{Endpoint: srv.URL + "/v1/messages"})
	if r, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0); err != nil || r.Text != "ok" {
		t.Fatalf("real HTTP round trip: %v %v", r, err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0); err == nil ||
		err.Error() != "Cannot send a request, as the client has been closed." {
		t.Errorf("after Close: %v", err)
	}

	shared := srv.Client()
	borrowed := mustOpenAI(t, OpenAICompatOptions{BaseURL: srv.URL, Client: shared})
	if err := borrowed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := borrowed.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0); err == nil ||
		strings.Contains(err.Error(), "closed") {
		// okClaude has no choices, so parsing fails — but the borrowed client itself still works.
		t.Errorf("a borrowed client belongs to the caller: %v", err)
	}
}

func TestOwnedClientDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	m := mustOpenAI(t, OpenAICompatOptions{BaseURL: srv.URL})
	defer m.Close()
	_, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0)
	want := "Redirect response '307 Temporary Redirect' for url '" + srv.URL + "/chat/completions'\n" +
		"Redirect location: '/elsewhere'\n" +
		"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/307"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v", err)
	}
}

func TestOpenAICompatAlwaysSendsMaxTokens(t *testing.T) {
	mc := &mockClient{}
	client := mc.client(t, func(int, *http.Request) *http.Response {
		return jsonResponse(200, `{"model":"m","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	m := mustOpenAI(t, OpenAICompatOptions{Client: client, DefaultMaxTokens: 1234,
		PriceInPerMTok: Float(1), PriceOutPerMTok: Float(1)})
	if _, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 50); err != nil {
		t.Fatal(err)
	}
	if mc.body(t, 0)["max_tokens"] != 1234.0 || mc.body(t, 1)["max_tokens"] != 50.0 {
		t.Errorf("max_tokens = %v, %v", mc.body(t, 0)["max_tokens"], mc.body(t, 1)["max_tokens"])
	}
	if d := mustOpenAI(t, OpenAICompatOptions{}).DefaultMaxTokens(); d != 8192 {
		t.Errorf("default = %d", d)
	}
	if d := mustClaude(t, ClaudeOptions{}).DefaultMaxTokens(); d != 4096 {
		t.Errorf("claude default = %d", d)
	}
}

func TestToolsAreSentOnlyWhenNonEmpty(t *testing.T) {
	mc := &mockClient{}
	client := mc.client(t, func(int, *http.Request) *http.Response { return jsonResponse(200, okClaude) })
	m := mustClaude(t, ClaudeOptions{Client: client})
	_, _ = m.Complete(ctx, []contracts.ModelMessage{user("hi")}, []map[string]any{}, 0)
	if _, ok := mc.body(t, 0)["tools"]; ok {
		t.Error("empty tools must be omitted")
	}
	if _, ok := mc.body(t, 0)["system"]; ok {
		t.Error("empty system must be omitted")
	}
}

func TestContextCancellationStopsTheCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	m := mustClaude(t, ClaudeOptions{Endpoint: srv.URL, Sleep: (&sleeps{}).sleep})
	defer m.Close()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Complete(cctx, []contracts.ModelMessage{user("hi")}, nil, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
