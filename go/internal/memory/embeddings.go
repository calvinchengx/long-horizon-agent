package memory

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// Embedders (python: lha.memory.embeddings).
//
//   - HashEmbedder: deterministic, offline, $0. A hashed bag-of-tokens vector — lexical, NOT
//     semantic — byte-identical to Python's (spec/memory/hash_embedder.json).
//   - OllamaEmbedder: a real local semantic embedder served by Ollama (POST /api/embed).
//     ConnectOllama probes the server, the model and its digest; the memory service turns an
//     unreachable server or an unpulled model into lexical-only retrieval.
//   - VoyageEmbedder (voyage.go): the paid Voyage AI API over DNS-pinned, egress-checked HTTPS;
//     ConnectVoyage probes the key, the endpoint and the dimension, and the memory service turns
//     an unreachable API or a refused key into lexical-only retrieval.
//   - PaddedEmbedder: zero-pads a narrower embedder to pgvector's vector(1024) column.
//   - sentence_transformers is Python-only: NewSentenceTransformerEmbedder always returns
//     ErrSentenceTransformersUnsupported, which the memory service turns into lexical-only
//     retrieval, exactly as Python does when the `embeddings` extra is not installed.

// HashEmbedder is the deterministic offline embedder (dev/CI/offline only).
type HashEmbedder struct{ dim int }

// NewHashEmbedder is a hash embedder of dim dimensions (<= 0 => 256).
func NewHashEmbedder(dim int) *HashEmbedder {
	if dim <= 0 {
		dim = 256
	}
	return &HashEmbedder{dim: dim}
}

// Name is "hash".
func (h *HashEmbedder) Name() string { return "hash" }

// Version is "1".
func (h *HashEmbedder) Version() string { return "1" }

// Dim is the vector width.
func (h *HashEmbedder) Dim() int { return h.dim }

// Embed returns one unit-norm vector per text.
func (h *HashEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i, t := range texts {
		out[i] = h.EmbedOne(t)
	}
	return out, nil
}

// EmbedOne is the vector of one text: each token's sha1 (as an integer) mod dim is a bucket.
func (h *HashEmbedder) EmbedOne(text string) []float64 {
	vec := make([]float64, h.dim)
	mod := big.NewInt(int64(h.dim))
	for _, token := range Tokenize(text) {
		sum := sha1.Sum([]byte(token))
		bucket := new(big.Int).Mod(new(big.Int).SetBytes(sum[:]), mod).Int64()
		vec[bucket] += 1.0
	}
	squares := make([]float64, h.dim)
	for i, v := range vec {
		squares[i] = v * v
	}
	norm := math.Sqrt(PySum(squares))
	if norm == 0 {
		norm = 1.0
	}
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

// ErrSentenceTransformersUnsupported: the sentence_transformers embedder / cross-encoder need the
// Python `embeddings` extra and do not exist in the Go implementation.
var ErrSentenceTransformersUnsupported = errors.New("sentence-transformers is not available in the Go implementation " +
	"(it is a Python-only extra); use LHA_MEMORY_EMBEDDER=ollama or hash, or the Python lha")

// NewSentenceTransformerEmbedder always fails in Go (see ErrSentenceTransformersUnsupported).
func NewSentenceTransformerEmbedder(string) (contracts.Embedder, error) {
	return nil, ErrSentenceTransformersUnsupported
}

// OllamaUnavailableError is Ollama being unreachable, or the model not pulled.
type OllamaUnavailableError struct{ Message string }

func (e *OllamaUnavailableError) Error() string { return e.Message }

// OllamaBatch is the number of texts per /api/embed request.
var OllamaBatch = 64

// OllamaEmbedder is a local semantic embedder served by Ollama. Build it with ConnectOllama.
type OllamaEmbedder struct {
	model   string
	version string
	dim     int
	base    string
	client  *http.Client
}

// Name is "ollama:<model>".
func (o *OllamaEmbedder) Name() string { return "ollama:" + o.model }

// Version is "<model>@<digest[:12]>" (just the model without a digest): a re-pulled model with
// different weights is never compared with vectors from the old one.
func (o *OllamaEmbedder) Version() string { return o.version }

// Dim is the measured vector width.
func (o *OllamaEmbedder) Dim() int { return o.dim }

// Close releases idle connections.
func (o *OllamaEmbedder) Close() error {
	o.client.CloseIdleConnections()
	return nil
}

func ollamaVersion(model, digest string) string {
	if digest != "" {
		if len(digest) > 12 {
			digest = digest[:12]
		}
		return model + "@" + digest
	}
	return model
}

// OllamaOptions configure ConnectOllama.
type OllamaOptions struct {
	Model   string
	BaseURL string // "" => http://localhost:11434
	Timeout time.Duration
	// Transport is a test seam (nil => a transport that ignores proxy environment variables,
	// python: trust_env=False).
	Transport http.RoundTripper
}

// ConnectOllama probes the server (GET /api/tags) and the model, pins the model's digest into the
// version and measures the dimension by embedding a probe text. Any failure is an
// *OllamaUnavailableError.
func ConnectOllama(ctx context.Context, o OllamaOptions) (*OllamaEmbedder, error) {
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = "http://localhost:11434"
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := o.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		transport = t
	}
	client := &http.Client{Timeout: timeout, Transport: transport}
	tags, err := getTags(ctx, client, base)
	if err != nil {
		client.CloseIdleConnections()
		return nil, &OllamaUnavailableError{Message: fmt.Sprintf("ollama unreachable at %s (%s)", base, httpExcText(err))}
	}
	digest, ok := findDigest(tags, o.Model)
	if !ok {
		client.CloseIdleConnections()
		return nil, &OllamaUnavailableError{Message: fmt.Sprintf(
			"ollama model %s is not pulled at %s (run `ollama pull %s`)", contracts.PyRepr(o.Model), base, o.Model)}
	}
	emb := &OllamaEmbedder{model: o.Model, version: ollamaVersion(o.Model, digest), base: base, client: client}
	vectors, err := emb.Embed(ctx, []string{"dimension probe"})
	if err != nil {
		client.CloseIdleConnections()
		return nil, &OllamaUnavailableError{Message: fmt.Sprintf("ollama could not embed with %s (%s)", contracts.PyRepr(o.Model), httpExcText(err))}
	}
	if len(vectors) != 1 || len(vectors[0]) == 0 {
		client.CloseIdleConnections()
		return nil, &OllamaUnavailableError{Message: fmt.Sprintf("ollama returned an empty vector for %s", contracts.PyRepr(o.Model))}
	}
	emb.dim = len(vectors[0])
	return emb, nil
}

func getTags(ctx context.Context, client *http.Client, base string) ([]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ConnectError: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTPStatusError: HTTP %d from %s/api/tags", resp.StatusCode, base)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("JSONDecodeError: %w", err)
	}
	models, _ := parsed["models"].([]any)
	return models, nil
}

// findDigest is the digest of model in an /api/tags listing ("name" or "name:latest").
func findDigest(tags []any, model string) (string, bool) {
	wanted := map[string]bool{model: true}
	if !strings.Contains(model, ":") {
		wanted[model+":latest"] = true
	}
	for _, raw := range tags {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		mdl, _ := entry["model"].(string)
		if wanted[name] || wanted[mdl] {
			digest, _ := entry["digest"].(string)
			return digest, true
		}
	}
	return "", false
}

// Embed embeds texts in batches of OllamaBatch.
func (o *OllamaEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	out := [][]float64{}
	for start := 0; start < len(texts); start += OllamaBatch {
		batch := texts[start:min(len(texts), start+OllamaBatch)]
		body, _ := json.Marshal(contracts.NewOrderedMap("model", o.model, "input", batch))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/api/embed", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := o.client.Do(req)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("HTTP %d from %s/api/embed", resp.StatusCode, o.base)
		}
		var parsed struct {
			Embeddings [][]float64 `json:"embeddings"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			return nil, err
		}
		if len(parsed.Embeddings) != len(batch) {
			return nil, fmt.Errorf("ollama returned %d vectors for %d texts", len(parsed.Embeddings), len(batch))
		}
		out = append(out, parsed.Embeddings...)
	}
	return out, nil
}

// PaddedEmbedder zero-pads another embedder's vectors to dim (pgvector's fixed vector(N)
// column). Appending zeros changes neither dot products nor norms, so cosine similarity is
// unchanged. Name and version are the inner embedder's.
type PaddedEmbedder struct {
	Inner contracts.Embedder
	dim   int
}

// NewPaddedEmbedder pads inner to dim (an error when inner is wider).
func NewPaddedEmbedder(inner contracts.Embedder, dim int) (*PaddedEmbedder, error) {
	if inner.Dim() > dim {
		return nil, fmt.Errorf("cannot pad dim=%d vectors down to %d", inner.Dim(), dim)
	}
	return &PaddedEmbedder{Inner: inner, dim: dim}, nil
}

// Name is the inner embedder's.
func (p *PaddedEmbedder) Name() string { return p.Inner.Name() }

// Version is the inner embedder's.
func (p *PaddedEmbedder) Version() string { return p.Inner.Version() }

// Dim is the padded width.
func (p *PaddedEmbedder) Dim() int { return p.dim }

// Embed pads every inner vector with zeros.
func (p *PaddedEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	vectors, err := p.Inner.Embed(ctx, texts)
	return p.pad(vectors, err)
}

// EmbedQuery pads the inner embedder's query vectors (EmbedQueries).
func (p *PaddedEmbedder) EmbedQuery(ctx context.Context, texts []string) ([][]float64, error) {
	vectors, err := EmbedQueries(ctx, p.Inner, texts)
	return p.pad(vectors, err)
}

func (p *PaddedEmbedder) pad(vectors [][]float64, err error) ([][]float64, error) {
	if err != nil {
		return nil, err
	}
	out := make([][]float64, len(vectors))
	for i, v := range vectors {
		padded := make([]float64, max(p.dim, len(v)))
		copy(padded, v)
		out[i] = padded
	}
	return out, nil
}

// Close closes the inner embedder when it has a Close.
func (p *PaddedEmbedder) Close() error {
	if c, ok := p.Inner.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// httpExcText is python's f"{type(exc).__name__}: {exc}" for an HTTP client failure (without Go's
// `Get "<url>": ` prefix).
func httpExcText(err error) string {
	name := pyfmt.ExcTypeName(err)
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	return name + ": " + err.Error()
}
