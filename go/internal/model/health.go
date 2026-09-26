package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Cheap, real model-provider health probes, used by a parked durable mission
// (python/src/lha/model/health.py). Building a provider only proves the configuration parses;
// ProbeModel builds the configured provider and CONTACTS it with the cheapest request each
// backend offers, under a tight timeout, without spending tokens:
//
//	StubModel          always healthy (in-process)
//	Ollama             GET <root>/api/tags; the model must be pulled
//	OpenAI-compatible  GET <base>/models (with the bearer key)
//	ClaudeModel        GET /v1/models/<model> (with the API key)
//	FailoverModel      healthy if ANY member is healthy
//
// Any transport error, timeout or non-2xx answer (401/403 included) is DOWN with the reason.

// ModelHealth is the outcome of one probe.
type ModelHealth struct {
	OK     bool
	Detail string
}

// HealthChecker is a provider that can be contacted cheaply.
type HealthChecker interface {
	HealthCheck(ctx context.Context, timeout time.Duration) ModelHealth
}

// httpFailure maps a probe error to a compact reason (python: http_failure).
func httpFailure(err error, status int, url string) string {
	if status != 0 {
		return fmt.Sprintf("HTTP %d from %s", status, url)
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return "timed out (ConnectTimeout)"
		}
		return "timed out (ReadTimeout)"
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return "ConnectError: " + op.Err.Error()
	}
	return fmt.Sprintf("%T: %v", err, err)
}

// probeGET GETs url and returns the body of a 2xx answer.
func probeGET(ctx context.Context, client *http.Client, url string, header http.Header, timeout time.Duration) ([]byte, string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, httpFailure(err, 0, url)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, httpFailure(err, 0, url)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, httpFailure(nil, resp.StatusCode, url)
	}
	if err != nil {
		return nil, httpFailure(err, 0, url)
	}
	return body, ""
}

// HealthCheck: in-process, always healthy.
func (s *StubModel) HealthCheck(context.Context, time.Duration) ModelHealth {
	return ModelHealth{OK: true, Detail: s.Name() + ": in-process stub"}
}

// HealthCheck contacts the endpoint without spending tokens: Ollama's GET <root>/api/tags (the
// configured model must be pulled), else GET <base>/models with the bearer key.
func (m *OpenAICompatModel) HealthCheck(ctx context.Context, timeout time.Duration) ModelHealth {
	if strings.HasPrefix(m.name, "ollama:") {
		root := strings.TrimSuffix(m.baseURL, "/v1")
		body, failure := probeGET(ctx, m.http.client, root+"/api/tags", nil, timeout)
		if failure != "" {
			return ModelHealth{Detail: m.name + ": " + failure}
		}
		var tags struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(body, &tags); err != nil {
			return ModelHealth{Detail: m.name + ": " + httpFailure(err, 0, root)}
		}
		for _, t := range tags.Models {
			if name, _ := t["name"].(string); name == m.model || name == m.model+":latest" {
				return ModelHealth{OK: true, Detail: m.name + ": reachable"}
			}
		}
		return ModelHealth{Detail: fmt.Sprintf("%s: model %s is not pulled", m.name, contracts.PyRepr(m.model))}
	}
	header := http.Header{}
	if m.apiKey != "" {
		header.Set("Authorization", "Bearer "+m.apiKey)
	}
	if _, failure := probeGET(ctx, m.http.client, m.baseURL+"/models", header, timeout); failure != "" {
		return ModelHealth{Detail: m.name + ": " + failure}
	}
	return ModelHealth{OK: true, Detail: m.name + ": reachable"}
}

// HealthCheck is GET /v1/models/<model>: the API is up, the key works and the model exists.
func (m *ClaudeModel) HealthCheck(ctx context.Context, timeout time.Duration) ModelHealth {
	models := strings.TrimSuffix(m.endpoint, "/messages") + "/models"
	header := http.Header{}
	header.Set("x-api-key", m.apiKey)
	header.Set("anthropic-version", ClaudeAPIVersion)
	if _, failure := probeGET(ctx, m.http.client, models+"/"+m.model, header, timeout); failure != "" {
		return ModelHealth{Detail: m.name + ": " + failure}
	}
	return ModelHealth{OK: true, Detail: m.name + ": reachable"}
}

// HealthCheck is healthy if ANY member can serve (failover routes around the others).
func (f *FailoverModel) HealthCheck(ctx context.Context, timeout time.Duration) ModelHealth {
	results := make([]ModelHealth, len(f.providers))
	done := make(chan struct{}, len(f.providers))
	for i, p := range f.providers {
		go func() {
			results[i] = ProbeProvider(ctx, p, timeout)
			done <- struct{}{}
		}()
	}
	for range f.providers {
		<-done
	}
	var details []string
	ok := false
	for _, r := range results {
		ok = ok || r.OK
		if r.Detail != "" {
			details = append(details, r.Detail)
		}
	}
	return ModelHealth{OK: ok, Detail: strings.Join(details, "; ")}
}

// ProbeProvider runs provider's HealthCheck bounded by timeout (python: probe_provider). A
// provider without one is reported healthy only as far as it could be built.
func ProbeProvider(ctx context.Context, provider contracts.ModelProvider, timeout time.Duration) ModelHealth {
	checker, ok := provider.(HealthChecker)
	if !ok {
		return ModelHealth{OK: true, Detail: provider.Name() + ": no probe available (built only)"}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+time.Second)
	defer cancel()
	return checker.HealthCheck(ctx, timeout)
}

// ProbeModel builds the configured provider and contacts it cheaply (python: probe_model);
// timeout 0 = LHA_MODEL_PROBE_TIMEOUT_S.
func ProbeModel(ctx context.Context, settings *config.Settings, timeout time.Duration) ModelHealth {
	if timeout <= 0 {
		timeout = time.Duration(settings.ModelProbeTimeoutS * float64(time.Second))
	}
	provider, err := BuildProvider(settings, "", nil)
	if err != nil {
		return ModelHealth{Detail: fmt.Sprintf("ValueError: %v", err)}
	}
	defer func() {
		if c, ok := provider.(io.Closer); ok {
			c.Close()
		}
	}()
	return ProbeProvider(ctx, provider, timeout)
}
