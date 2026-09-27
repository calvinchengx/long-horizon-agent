package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
)

// The Voyage AI embedder (python: lha.memory.embeddings.VoyageEmbedder), selected by
// LHA_MEMORY_EMBEDDER=voyage.
//
// ConnectVoyage checks the key and the endpoint (a one-host egress policy, https only) and
// measures the dimension by embedding a probe text; a missing or refused key, an unreachable API
// or a refused endpoint is a *VoyageUnavailableError and the memory service runs lexical-only.
//
// Every request re-resolves the endpoint host, refuses it unless every address is public, and
// dials only those vetted addresses (safety.PinnedDialer: no DNS rebinding, no proxy from the
// environment); TLS and the Host header keep the hostname. The key is bound by a
// CredentialBroker to the endpoint host and scrubbed from error messages. Documents are embedded
// with input_type "document" (Embed), search text with input_type "query" (EmbedQuery).
// Transient failures (408/409/429/5xx, timeouts, connection errors) are retried with backoff
// (Retry-After honoured); 401/403 are not. The version is the model name.

const (
	// VoyageEndpoint is the default embeddings endpoint.
	VoyageEndpoint = "https://api.voyageai.com/v1/embeddings"
	// VoyageDefaultModel is LHA_MEMORY_EMBEDDING_MODEL's default for voyage (1024-wide).
	VoyageDefaultModel = "voyage-4"
	// VoyageBatchTexts is the number of texts per request (the API accepts up to 1000).
	VoyageBatchTexts = 128
	// VoyageBatchChars is the number of characters (code points) per request, under every
	// model's per-request token cap.
	VoyageBatchChars = 100_000
	// VoyageTimeout is the HTTP timeout of the probe and every embed call.
	VoyageTimeout = 30 * time.Second

	voyageKeyPlaceholder = "{{LHA_VOYAGE_API_KEY}}"
	voyageMaxResponse    = 64 << 20
)

// VoyageRetry is the retry policy of every Voyage request (tests replace Sleep).
var VoyageRetry = model.RetryPolicy{MaxRetries: 3, BaseDelaySeconds: 1.0, MaxDelaySeconds: 30.0}

// VoyageUnavailableError is the Voyage API being unreachable, refusing the key, or its endpoint
// being refused by egress.
type VoyageUnavailableError struct{ Message string }

func (e *VoyageUnavailableError) Error() string { return e.Message }

// VoyageBatches splits texts in order into batches of at most maxTexts texts and maxChars
// characters (code points); a single longer text travels alone.
func VoyageBatches(texts []string, maxTexts, maxChars int) [][]string {
	batches := [][]string{}
	current := []string{}
	chars := 0
	for _, text := range texts {
		n := utf8.RuneCountInString(text)
		if len(current) > 0 && (len(current) >= maxTexts || chars+n > maxChars) {
			batches = append(batches, current)
			current, chars = []string{}, 0
		}
		current = append(current, text)
		chars += n
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

// VoyageRequest is the JSON body of one POST /v1/embeddings.
type VoyageRequest struct {
	Input     []string `json:"input"`
	Model     string   `json:"model"`
	InputType string   `json:"input_type"`
}

// VoyageRequestBody is the body for texts ("document" or "query" inputType).
func VoyageRequestBody(model string, texts []string, inputType string) VoyageRequest {
	return VoyageRequest{Input: append([]string{}, texts...), Model: model, InputType: inputType}
}

// ParseVoyageResponse is the vectors of a /v1/embeddings answer, in input order (by
// data[].index); anything but exactly count non-empty numeric vectors is an error.
func ParseVoyageResponse(body []byte, count int) ([][]float64, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var data any
	if err := dec.Decode(&data); err != nil {
		return nil, fmt.Errorf("JSONDecodeError: %w", err)
	}
	obj, _ := data.(map[string]any)
	items, ok := obj["data"].([]any)
	if !ok {
		return nil, errors.New("voyage response has no data list")
	}
	byIndex := map[int][]float64{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("voyage response item is not an object")
		}
		index, ok := voyageIndex(item["index"], count)
		if !ok {
			return nil, fmt.Errorf("voyage response has an invalid index %s", pyfmt.PyReprValue(item["index"]))
		}
		values, ok := item["embedding"].([]any)
		if !ok || len(values) == 0 {
			return nil, fmt.Errorf("voyage response has no embedding at index %d", index)
		}
		vector := make([]float64, len(values))
		for i, v := range values {
			n, ok := v.(json.Number)
			if !ok {
				return nil, fmt.Errorf("voyage response has a non-numeric embedding at index %d", index)
			}
			f, err := strconv.ParseFloat(string(n), 64)
			if err != nil && !errors.Is(err, strconv.ErrRange) { // python overflows to inf too

				return nil, fmt.Errorf("voyage response has a non-numeric embedding at index %d", index)
			}
			vector[i] = f
		}
		if _, dup := byIndex[index]; dup {
			return nil, fmt.Errorf("voyage response repeats index %d", index)
		}
		byIndex[index] = vector
	}
	if len(byIndex) != count {
		return nil, fmt.Errorf("voyage returned %d vectors for %d texts", len(byIndex), count)
	}
	out := make([][]float64, count)
	for i := range out {
		out[i] = byIndex[i]
	}
	return out, nil
}

// voyageIndex is an integer JSON number in [0, count) (python: an int, not a float or bool).
func voyageIndex(v any, count int) (int, bool) {
	n, ok := v.(json.Number)
	if !ok || strings.ContainsAny(string(n), ".eE") {
		return 0, false
	}
	i, err := strconv.Atoi(string(n))
	if err != nil || i < 0 || i >= count {
		return 0, false
	}
	return i, true
}

// VoyageStatusMessage is why an HTTP status from the API makes the embedder unusable.
func VoyageStatusMessage(model string, status int) string {
	if status == 401 || status == 403 {
		return fmt.Sprintf("voyage rejected the API key for %s (HTTP %d)", contracts.PyRepr(model), status)
	}
	return fmt.Sprintf("voyage could not embed with %s (HTTP %d)", contracts.PyRepr(model), status)
}

// VoyageOptions configure ConnectVoyage / NewVoyageEmbedder.
type VoyageOptions struct {
	APIKey   string
	Model    string        // "" => VoyageDefaultModel
	Endpoint string        // "" => VoyageEndpoint
	Timeout  time.Duration // <= 0 => VoyageTimeout
	// Resolver resolves the endpoint host for the egress check (nil => safety.SystemResolver).
	Resolver safety.Resolver
	// Transport is a test seam (an in-memory RoundTripper; nothing is dialled, so nothing is
	// pinned). nil => a safety.NewPinnedTransport over Dial.
	Transport http.RoundTripper
	// Dial is the socket layer under the pinning (nil => a net.Dialer); a test seam.
	Dial safety.DialFunc
}

// VoyageEmbedder is the Voyage AI embedder. Build it with ConnectVoyage.
type VoyageEmbedder struct {
	model    string
	dim      int
	endpoint string
	key      string
	policy   *safety.EgressPolicy
	broker   *safety.CredentialBroker
	resolver safety.Resolver
	dialer   *safety.PinnedDialer // nil with a Transport seam
	client   *http.Client
}

// egressStop is an egress refusal that is never retried (python: EgressDenied is not a
// transport error), whatever resolver error it carries.
type egressStop struct{ err error }

func (e *egressStop) Error() string { return e.err.Error() }
func (e *egressStop) Unwrap() error { return safety.ErrEgressDenied }

// NewVoyageEmbedder builds the embedder with a declared dim (ConnectVoyage measures it). A
// malformed or non-https endpoint is an egress refusal.
func NewVoyageEmbedder(o VoyageOptions, dim int) (*VoyageEmbedder, error) {
	mdl := o.Model
	if mdl == "" {
		mdl = VoyageDefaultModel
	}
	endpoint := o.Endpoint
	if endpoint == "" {
		endpoint = VoyageEndpoint
	}
	target, err := safety.ParseURL(endpoint)
	if err != nil {
		return nil, err
	}
	if target.Scheme != "https" {
		return nil, &safety.EgressDeniedError{Reason: "the voyage endpoint must use https: " + contracts.PyRepr(endpoint)}
	}
	broker := safety.NewCredentialBroker()
	if err := broker.Register(voyageKeyPlaceholder, o.APIKey, target.Host); err != nil {
		return nil, err
	}
	resolver := o.Resolver
	if resolver == nil {
		resolver = safety.SystemResolver
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = VoyageTimeout
	}
	e := &VoyageEmbedder{model: mdl, dim: dim, endpoint: endpoint, key: o.APIKey, broker: broker, resolver: resolver,
		// The embedder may reach exactly its endpoint (scheme, host and port), nothing else.
		policy: &safety.EgressPolicy{AllowHosts: []string{target.Host}, AllowSchemes: []string{"https"}, AllowPorts: []int{target.Port}}}
	transport := o.Transport
	if transport == nil {
		e.dialer = safety.NewPinnedDialer(o.Dial, timeout)
		transport = safety.NewPinnedTransport(e.dialer, timeout, nil)
	}
	e.client = &http.Client{Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e, nil
}

// ConnectVoyage checks the key, the endpoint and the model by embedding a probe text; any failure
// is a *VoyageUnavailableError.
func ConnectVoyage(ctx context.Context, o VoyageOptions) (*VoyageEmbedder, error) {
	if strings.TrimSpace(o.APIKey) == "" {
		return nil, &VoyageUnavailableError{Message: "voyage: LHA_VOYAGE_API_KEY is not set"}
	}
	e, err := NewVoyageEmbedder(o, 0)
	if err != nil {
		return nil, &VoyageUnavailableError{Message: "voyage endpoint refused: " + err.Error()}
	}
	vectors, err := e.Embed(ctx, []string{"dimension probe"})
	if err != nil {
		e.Close()
		return nil, &VoyageUnavailableError{Message: e.describe(err)}
	}
	e.dim = len(vectors[0])
	return e, nil
}

// describe is why the probe failed, without the key.
func (e *VoyageEmbedder) describe(err error) string {
	var status *model.HTTPStatusError
	var uerr *url.Error
	var why string
	switch {
	case errors.As(err, &status):
		why = VoyageStatusMessage(e.model, status.StatusCode)
	case errors.As(err, &uerr):
		why = fmt.Sprintf("voyage unreachable at %s (ConnectError: %s)", e.endpoint, uerr.Err)
	case safety.IsEgressDenied(err):
		why = "voyage endpoint refused: " + err.Error()
	default:
		why = fmt.Sprintf("voyage could not embed with %s (%s)", contracts.PyRepr(e.model), err)
	}
	return e.scrub(why)
}

func (e *VoyageEmbedder) scrub(text string) string {
	if e.key == "" {
		return text
	}
	return strings.ReplaceAll(text, e.key, "***")
}

// String never shows the key.
func (e *VoyageEmbedder) String() string {
	return fmt.Sprintf("VoyageEmbedder(model=%s, dim=%d, endpoint=%s)", contracts.PyRepr(e.model), e.dim, contracts.PyRepr(e.endpoint))
}

// Name is "voyage:<model>".
func (e *VoyageEmbedder) Name() string { return "voyage:" + e.model }

// Version is the model name (Voyage publishes no weight digest; a model name's weights are fixed).
func (e *VoyageEmbedder) Version() string { return e.model }

// Dim is the measured vector width.
func (e *VoyageEmbedder) Dim() int { return e.dim }

// Close releases idle connections.
func (e *VoyageEmbedder) Close() error {
	e.client.CloseIdleConnections()
	return nil
}

// Embed is the vectors of documents (input_type "document").
func (e *VoyageEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	return e.embed(ctx, texts, "document")
}

// EmbedQuery is the vectors of search text (input_type "query").
func (e *VoyageEmbedder) EmbedQuery(ctx context.Context, texts []string) ([][]float64, error) {
	return e.embed(ctx, texts, "query")
}

func (e *VoyageEmbedder) embed(ctx context.Context, texts []string, inputType string) ([][]float64, error) {
	out := [][]float64{}
	for _, batch := range VoyageBatches(texts, VoyageBatchTexts, VoyageBatchChars) {
		body, err := json.Marshal(VoyageRequestBody(e.model, batch, inputType))
		if err != nil {
			return nil, err
		}
		raw, err := model.WithRetries(ctx, VoyageRetry, func(ctx context.Context) ([]byte, error) {
			return e.post(ctx, body)
		})
		if err != nil {
			return nil, err
		}
		vectors, err := ParseVoyageResponse(raw, len(batch))
		if err != nil {
			return nil, err
		}
		for _, v := range vectors {
			if e.dim > 0 && len(v) != e.dim {
				return nil, fmt.Errorf("voyage returned a vector whose dim is not %d", e.dim)
			}
		}
		out = append(out, vectors...)
	}
	return out, nil
}

func (e *VoyageEmbedder) post(ctx context.Context, body []byte) ([]byte, error) {
	parsed, err := e.policy.Check(e.endpoint)
	if err != nil {
		return nil, &egressStop{err}
	}
	addresses, err := safety.CheckResolvedAddresses(ctx, parsed.Host, parsed.Port, e.resolver)
	if err != nil {
		return nil, &egressStop{err}
	}
	if e.dialer != nil {
		if err := e.dialer.Pin(parsed.Host, addresses); err != nil { // the connection dials only these
			return nil, &egressStop{err}
		}
	}
	headers := e.broker.ResolveHeaders(map[string]string{
		"Authorization": "Bearer " + voyageKeyPlaceholder,
		"Content-Type":  "application/json",
	}, parsed.Host)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, voyageMaxResponse))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		reason := strings.TrimPrefix(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)), " ")
		return nil, &model.HTTPStatusError{Method: http.MethodPost, URL: e.endpoint, StatusCode: resp.StatusCode,
			Reason: reason, Header: resp.Header, Body: data}
	}
	return data, nil
}

// QueryEmbedder is an embedder that embeds search text differently from documents (Voyage's
// input_type).
type QueryEmbedder interface {
	EmbedQuery(ctx context.Context, texts []string) ([][]float64, error)
}

// EmbedQueries is the vectors of search text: EmbedQuery when the embedder has it, else Embed.
func EmbedQueries(ctx context.Context, e contracts.Embedder, texts []string) ([][]float64, error) {
	if q, ok := e.(QueryEmbedder); ok {
		return q.EmbedQuery(ctx, texts)
	}
	return e.Embed(ctx, texts)
}
