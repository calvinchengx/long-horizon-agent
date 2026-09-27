package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
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

// SystemOnePassageChars is the characters (code points) of each passage sent to a System One
// reranker.
const SystemOnePassageChars = 1500

const systemOneRelevance = "Does `passage` contain information that helps to carry out `task` (a fact, a past attempt " +
	"or its outcome, a decision, or code it needs)?"

// SystemOneRerankRequest is the state (the task) and one relevance Noul per hit (p0, p1, ...)
// (python: system_one_rerank_request).
func SystemOneRerankRequest(query string, hits []contracts.RetrievalHit) (*contracts.OrderedMap, []systemone.Named) {
	questions := make([]systemone.Named, len(hits))
	for i, hit := range hits {
		text := []rune(hit.Record.Text)
		if len(text) > SystemOnePassageChars {
			text = text[:SystemOnePassageChars]
		}
		questions[i] = systemone.Named{ID: fmt.Sprintf("p%d", i), Question: systemone.Noul(
			contracts.NewOrderedMap("passage", obs.RedactText(string(text)), "question", systemOneRelevance), nil)}
	}
	return contracts.NewOrderedMap("task", obs.RedactText(query)), questions
}

// ApplyRelevance is hits rescored by relevance, most relevant first (ties keep fusion order),
// without those below minP, cut to k (nil keeps them all) (python: apply_relevance).
func ApplyRelevance(hits []contracts.RetrievalHit, relevance []float64, k *int, minP float64) []contracts.RetrievalHit {
	type ranked struct {
		p   float64
		i   int
		hit contracts.RetrievalHit
	}
	kept := []ranked{}
	for i, hit := range hits {
		if relevance[i] >= minP {
			rescored := hit
			rescored.Score = relevance[i]
			kept = append(kept, ranked{relevance[i], i, rescored})
		}
	}
	sort.SliceStable(kept, func(a, b int) bool { return kept[a].p > kept[b].p })
	out := make([]contracts.RetrievalHit, 0, len(kept))
	for _, r := range kept {
		out = append(out, r.hit)
	}
	if k != nil && *k < len(out) {
		out = out[:max(*k, 0)]
	}
	return out
}

// SystemOneReranker reranks by a System One model's probability that each passage helps with the
// task: one request per recall, the passages as parallel questions. Any failure keeps fusion
// order (logged): recall never fails a cycle.
type SystemOneReranker struct {
	Model systemone.Model
	MinP  float64
}

// Name is "system-one:<model>".
func (r *SystemOneReranker) Name() string { return "system-one:" + r.Model.Name() }

// Rerank rescores hits (k <= 0 keeps them all).
func (r *SystemOneReranker) Rerank(ctx context.Context, query string, hits []contracts.RetrievalHit, k int) ([]contracts.RetrievalHit, error) {
	if len(hits) == 0 {
		return []contracts.RetrievalHit{}, nil
	}
	state, questions := SystemOneRerankRequest(query, hits)
	result, err := r.Model.Evaluate(ctx, state, questions)
	if err != nil {
		log().Warn("memory_rerank_failed", "error", err.Error())
		return NoopReranker{}.Rerank(ctx, query, hits, k)
	}
	relevance := make([]float64, len(questions))
	for i, q := range questions {
		if a := result.Answers[q.ID]; a.Type == "noul" {
			relevance[i] = a.Noul
		}
	}
	var cut *int
	if k > 0 {
		cut = &k
	}
	return ApplyRelevance(hits, relevance, cut, r.MinP), nil
}
