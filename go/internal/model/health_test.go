package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// The parked mission's model probe (python: tests for lha.model.health).

func TestProbeStubIsHealthy(t *testing.T) {
	h := ProbeProvider(context.Background(), NewStub(nil), time.Second)
	if !h.OK || !strings.HasSuffix(h.Detail, ": in-process stub") {
		t.Fatalf("%+v", h)
	}
}

func TestProbeOpenAICompatAndOllama(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			auth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models": [{"name": "qwen:latest"}]}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	zero := 0.0
	compat, _ := NewOpenAICompat(OpenAICompatOptions{BaseURL: srv.URL + "/v1", ModelName: "m", APIKey: "k"})
	if h := ProbeProvider(context.Background(), compat, time.Second); !h.OK || auth != "Bearer k" {
		t.Fatalf("%+v auth %q", h, auth)
	}
	pulled, _ := NewOpenAICompat(OpenAICompatOptions{BaseURL: srv.URL + "/v1", ModelName: "qwen", Label: "ollama", PriceInPerMTok: &zero, PriceOutPerMTok: &zero})
	if h := ProbeProvider(context.Background(), pulled, time.Second); !h.OK {
		t.Fatalf("%+v", h)
	}
	missing, _ := NewOpenAICompat(OpenAICompatOptions{BaseURL: srv.URL + "/v1", ModelName: "llama", Label: "ollama", PriceInPerMTok: &zero, PriceOutPerMTok: &zero})
	if h := ProbeProvider(context.Background(), missing, time.Second); h.OK || h.Detail != "ollama:llama: model 'llama' is not pulled" {
		t.Fatalf("%+v", h)
	}
	bad, _ := NewOpenAICompat(OpenAICompatOptions{BaseURL: srv.URL + "/nope", ModelName: "m"})
	if h := ProbeProvider(context.Background(), bad, time.Second); h.OK || !strings.Contains(h.Detail, "HTTP 401 from ") {
		t.Fatalf("%+v", h)
	}
	down, _ := NewOpenAICompat(OpenAICompatOptions{BaseURL: "http://127.0.0.1:1/v1", ModelName: "m"})
	if h := ProbeProvider(context.Background(), down, time.Second); h.OK || !strings.Contains(h.Detail, "ConnectError") {
		t.Fatalf("%+v", h)
	}
}

func TestProbeClaudeAndFailover(t *testing.T) {
	var path, key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	claude, err := NewClaude(ClaudeOptions{APIKey: "sk", ModelName: "claude-haiku-4-5", Endpoint: srv.URL + "/v1/messages"})
	if err != nil {
		t.Skipf("no price for the test model: %v", err)
	}
	h := ProbeProvider(context.Background(), claude, time.Second)
	if h.OK || path != "/v1/models/claude-haiku-4-5" || key != "sk" || !strings.Contains(h.Detail, "HTTP 503") {
		t.Fatalf("%+v path %s", h, path)
	}
	failover, _ := NewFailover([]contracts.ModelProvider{claude, NewStub(nil)}, FailoverOptions{})
	if h := ProbeProvider(context.Background(), failover, time.Second); !h.OK || !strings.Contains(h.Detail, "; ") {
		t.Fatalf("%+v", h)
	}
}
