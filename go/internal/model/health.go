package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// Cheap, real model-provider health probes, for a parked durable mission
// (python/src/lha/model/health.py).
//
// Building a provider only proves the configuration parses; it says nothing about whether the
// model can serve a turn. ProbeModel builds the configured provider and CONTACTS it with the
// cheapest request each backend offers, under a tight timeout, without spending tokens:
//
//	StubModel          always healthy (in-process)
//	Ollama             GET <base>/api/tags; the model must be pulled
//	OpenAI-compatible  GET <base>/models (with the bearer key)
//	ClaudeModel        GET /v1/models/<model> (with the API key)
//	FailoverModel      healthy if ANY member is healthy
//
// Any transport error, timeout or non-2xx answer (including 401/403: a bad key cannot serve turns
// either) is reported as DOWN with the reason, so the workflow keeps the mission parked.
//
// Parity: the ok/down verdicts and the "<provider>: reachable" / "HTTP <code> from <url>" /
// "not pulled" details are python's; transport error texts are Go's.

// ModelHealth is the outcome of one probe: OK plus a short human-readable detail.
type ModelHealth struct {
	OK     bool
	Detail string
}

// HealthChecker is implemented by providers that can be contacted cheaply.
type HealthChecker interface {
	HealthCheck(ctx context.Context, timeout time.Duration) ModelHealth
}

// HTTPFailure maps a probe error to a DOWN result with a compact reason (python: http_failure).
func HTTPFailure(err error) ModelHealth {
	var status *HTTPStatusError
	if errors.As(err, &status) {
		return ModelHealth{false, fmt.Sprintf("HTTP %d from %s", status.StatusCode, status.URL)}
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return ModelHealth{false, "timed out (" + timeoutTypeName(err) + ")"}
	}
	return ModelHealth{false, errorTypeName(err) + ": " + unwrapURLError(err).Error()}
}

func errorTypeName(err error) string {
	if n, ok := err.(interface{ PyTypeName() string }); ok {
		return n.PyTypeName()
	}
	var uerr *url.Error
	if errors.As(err, &uerr) && !errors.Is(err, errClientClosed) { // httpx's transport errors
		return pyfmt.HTTPErrorTypeName(err)
	}
	var unknown *contracts.UnknownPriceError
	var opErr *net.OpError
	var syntax *json.SyntaxError
	switch {
	case errors.As(err, &unknown):
		return "UnknownPriceError"
	case errors.As(err, &syntax):
		return "JSONDecodeError"
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "ConnectError"
	case errors.Is(err, errClientClosed):
		return "RuntimeError"
	}
	return "ValueError"
}

// timeoutTypeName names a timeout the way python's type(exc).__name__ does: httpx's
// ConnectTimeout (the dial) or ReadTimeout (the request), or asyncio's TimeoutError when the
// probe's overall bound (ProbeProvider) fired outside any request.
func timeoutTypeName(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return "ConnectTimeout"
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return "ReadTimeout"
	}
	return "TimeoutError"
}

// unwrapURLError drops net/http's `Get "<url>": ` prefix.
func unwrapURLError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// ProbeProvider runs p's HealthCheck bounded by timeout (plus one second of slack); DOWN on any
// error or timeout. A provider without a probe is reported healthy only as far as it could be
// built (the detail says so).
func ProbeProvider(ctx context.Context, p contracts.ModelProvider, timeout time.Duration) ModelHealth {
	checker, ok := p.(HealthChecker)
	if !ok {
		return ModelHealth{true, p.Name() + ": no probe available (built only)"}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+time.Second)
	defer cancel()
	done := make(chan ModelHealth, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil { // the probe must never fail the health activity
				done <- ModelHealth{false, fmt.Sprintf("RuntimeError: %v", r)}
			}
		}()
		done <- checker.HealthCheck(ctx, timeout)
	}()
	select {
	case h := <-done:
		return h
	case <-ctx.Done():
		return HTTPFailure(ctx.Err())
	}
}

// ProbeModel builds the configured provider (primary + fallbacks) and contacts it cheaply.
// timeout 0 uses settings.ModelProbeTimeoutS; client (nil = owned clients) is passed to
// BuildProvider. Configuration errors are reported as DOWN ("ValueError: ...").
func ProbeModel(ctx context.Context, settings *config.Settings, timeout time.Duration, client *http.Client) ModelHealth {
	if timeout <= 0 {
		timeout = time.Duration(settings.ModelProbeTimeoutS * float64(time.Second))
	}
	provider, err := BuildProvider(settings, "", client)
	if err != nil {
		return ModelHealth{false, errorTypeName(err) + ": " + err.Error()}
	}
	defer func() {
		if c, ok := provider.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	return ProbeProvider(ctx, provider, timeout)
}

// probeBodyLimit bounds a probe's response body (an Ollama tag list is a few KiB).
const probeBodyLimit = 8 << 20

// getOK GETs url with header, bounded by timeout, and returns the (2xx) body.
func (c *httpClient) getOK(ctx context.Context, url string, header http.Header, timeout time.Duration) ([]byte, error) {
	if c.closed.Load() {
		return nil, errClientClosed
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
	if err != nil {
		return nil, &transportError{op: "read response body", err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &HTTPStatusError{Method: http.MethodGet, URL: req.URL.String(), StatusCode: resp.StatusCode,
			Reason: reasonPhrase(resp), Header: resp.Header, Body: data}
	}
	return data, nil
}

// HealthCheck is in-process: always healthy.
func (s *StubModel) HealthCheck(context.Context, time.Duration) ModelHealth {
	return ModelHealth{true, s.Name() + ": in-process stub"}
}

// HealthCheck contacts the endpoint without spending tokens. Ollama: GET <root>/api/tags and the
// configured model must be pulled. Any other OpenAI-compatible endpoint: GET <base>/models with
// the bearer key.
func (m *OpenAICompatModel) HealthCheck(ctx context.Context, timeout time.Duration) ModelHealth {
	down := func(err error) ModelHealth { return ModelHealth{false, m.name + ": " + HTTPFailure(err).Detail} }
	if strings.HasPrefix(m.name, "ollama:") {
		root := strings.TrimSuffix(m.baseURL, "/v1")
		body, err := m.http.getOK(ctx, root+"/api/tags", nil, timeout)
		if err != nil {
			return down(err)
		}
		var decoded any
		if err := json.Unmarshal(body, &decoded); err != nil {
			return down(err)
		}
		obj, ok := decoded.(map[string]any)
		if !ok {
			return ModelHealth{false, m.name + ": AttributeError: '" + pyTypeName(decoded) + "' object has no attribute 'get'"}
		}
		names := map[string]bool{}
		if tags, ok := obj["models"].([]any); ok {
			for _, t := range tags {
				if tag, ok := t.(map[string]any); ok {
					names[pyStr(orEmpty(tag["name"]))] = true
				}
			}
		}
		if !names[m.model] && !names[m.model+":latest"] {
			return ModelHealth{false, m.name + ": model " + contracts.PyRepr(m.model) + " is not pulled"}
		}
		return ModelHealth{true, m.name + ": reachable"}
	}
	header := http.Header{}
	if m.apiKey != "" {
		header.Set("Authorization", "Bearer "+m.apiKey)
	}
	if _, err := m.http.getOK(ctx, m.baseURL+"/models", header, timeout); err != nil {
		return down(err)
	}
	return ModelHealth{true, m.name + ": reachable"}
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// ClaudeModelsEndpoint is the Models API (python: ClaudeModel.MODELS_ENDPOINT).
const ClaudeModelsEndpoint = "https://api.anthropic.com/v1/models"

// modelsEndpoint is ClaudeModelsEndpoint, or the Models API next to an overridden endpoint.
func (m *ClaudeModel) modelsEndpoint() string {
	if m.endpoint == ClaudeEndpoint || !strings.HasSuffix(m.endpoint, "/messages") {
		return ClaudeModelsEndpoint
	}
	return strings.TrimSuffix(m.endpoint, "/messages") + "/models"
}

// HealthCheck is GET /v1/models/<model>: it proves the API is up, the key works and the model
// exists, without spending tokens.
func (m *ClaudeModel) HealthCheck(ctx context.Context, timeout time.Duration) ModelHealth {
	header := http.Header{}
	header.Set("x-api-key", m.apiKey)
	header.Set("anthropic-version", ClaudeAPIVersion)
	if _, err := m.http.getOK(ctx, m.modelsEndpoint()+"/"+m.model, header, timeout); err != nil {
		return ModelHealth{false, m.name + ": " + HTTPFailure(err).Detail}
	}
	return ModelHealth{true, m.name + ": reachable"}
}

// HealthCheck is healthy if ANY member can serve (failover routes around the others); the
// detail joins every member's detail with "; ". Members are probed concurrently.
func (f *FailoverModel) HealthCheck(ctx context.Context, timeout time.Duration) ModelHealth {
	results := make([]ModelHealth, len(f.providers))
	var wg sync.WaitGroup
	for i, p := range f.providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = ProbeProvider(ctx, p, timeout)
		}()
	}
	wg.Wait()
	var ok bool
	var details []string
	for _, r := range results {
		ok = ok || r.OK
		if r.Detail != "" {
			details = append(details, r.Detail)
		}
	}
	return ModelHealth{ok, strings.Join(details, "; ")}
}
