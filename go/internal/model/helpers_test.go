package model

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// roundTripFunc is an in-memory http.RoundTripper (python tests: httpx.MockTransport).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type captured struct {
	URL    string
	Header http.Header
	Body   []byte
}

// mockClient answers every request with handler(n, req) and records the requests.
type mockClient struct {
	mu       sync.Mutex
	requests []captured
}

func (m *mockClient) client(t *testing.T, handler func(n int, r *http.Request) *http.Response) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.requests = append(m.requests, captured{URL: r.URL.String(), Header: r.Header.Clone(), Body: body})
		n := len(m.requests)
		m.mu.Unlock()
		resp := handler(n, r)
		resp.Request = r
		return resp, nil
	})}
}

func (m *mockClient) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *mockClient) body(t *testing.T, i int) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(m.requests[i].Body, &v); err != nil {
		t.Fatalf("request %d body: %v", i, err)
	}
	return v
}

func jsonResponse(status int, v any, header ...string) *http.Response {
	var body []byte
	switch x := v.(type) {
	case nil:
	case string:
		body = []byte(x)
	default:
		body, _ = json.Marshal(v)
	}
	h := http.Header{"Content-Type": {"application/json"}}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	return &http.Response{
		StatusCode: status,
		Status:     strconv.Itoa(status) + " " + http.StatusText(status),
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// sleeps records requested backoff delays without sleeping.
type sleeps struct {
	mu    sync.Mutex
	calls []float64
}

func (s *sleeps) sleep(_ context.Context, seconds float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, seconds)
	return nil
}

func statusError(status int, header ...string) *HTTPStatusError {
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	return &HTTPStatusError{Method: "POST", URL: "http://x/", StatusCode: status, Reason: http.StatusText(status), Header: h}
}

func approx(t *testing.T, label string, got, want float64) {
	t.Helper()
	tol := 1e-6 * max(1.0, abs(want)) // pytest.approx default: rel 1e-6
	if abs(got-want) > tol {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func user(content string) contracts.ModelMessage {
	return contracts.ModelMessage{Role: "user", Content: content}
}

var okClaude = map[string]any{
	"model":       "claude-sonnet-4-6",
	"content":     []any{map[string]any{"type": "text", "text": "ok"}},
	"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
	"stop_reason": "end_turn",
}

func mustClaude(t *testing.T, o ClaudeOptions) *ClaudeModel {
	t.Helper()
	if o.APIKey == "" {
		o.APIKey = "k"
	}
	if o.ModelName == "" {
		o.ModelName = "claude-sonnet-4-6"
	}
	m, err := NewClaude(o)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustOpenAI(t *testing.T, o OpenAICompatOptions) *OpenAICompatModel {
	t.Helper()
	if o.BaseURL == "" {
		o.BaseURL = "http://x/v1"
	}
	if o.ModelName == "" {
		o.ModelName = "m"
	}
	m, err := NewOpenAICompat(o)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var toolConversation = []contracts.ModelMessage{
	user("read it"),
	{Role: "assistant", Content: "", ToolCalls: []contracts.ToolCall{
		{ID: "call_1", Name: "read_file", Arguments: map[string]any{"path": "a"}},
		{ID: "call_2", Name: "read_file", Arguments: map[string]any{"path": "b"}},
	}},
	{Role: "tool", Content: "A", ToolCallID: contracts.Str("call_1")},
	{Role: "tool", Content: "B", ToolCallID: contracts.Str("call_2")},
}
