package memory

import (
	"context"
	"errors"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Reranking, the second stage of two-stage retrieval (python: lha.memory.rerank).

// Reranker rescores fused hits; k <= 0 keeps them all.
type Reranker interface {
	Name() string
	Rerank(ctx context.Context, query string, hits []contracts.RetrievalHit, k int) ([]contracts.RetrievalHit, error)
}

// NoopReranker keeps fusion order (the $0 default).
type NoopReranker struct{}

// Name is "noop".
func (NoopReranker) Name() string { return "noop" }

// Rerank returns hits[:k].
func (NoopReranker) Rerank(_ context.Context, _ string, hits []contracts.RetrievalHit, k int) ([]contracts.RetrievalHit, error) {
	if k > 0 && len(hits) > k {
		return hits[:k], nil
	}
	return hits, nil
}

// ErrCrossEncoderUnsupported: LHA_MEMORY_RERANK=cross_encoder needs Python's sentence-transformers
// (the `embeddings` extra); the Go implementation has no cross-encoder. The memory service logs
// it (memory_rerank_unavailable) and keeps fusion order, as Python does without the extra.
var ErrCrossEncoderUnsupported = errors.New("the cross_encoder reranker is not available in the Go " +
	"implementation (it needs the Python `embeddings` extra); fusion order is kept")

// NewCrossEncoderReranker always fails in Go (see ErrCrossEncoderUnsupported).
func NewCrossEncoderReranker(string) (Reranker, error) { return nil, ErrCrossEncoderUnsupported }
