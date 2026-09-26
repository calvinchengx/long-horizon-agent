package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Cosine is the cosine similarity of two equal-length vectors (0.0 if either is all-zero). A
// length mismatch is a bug (embedder/column dimension drift), never "no similarity": an error.
func Cosine(a, b []float64) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("cosine: vector length mismatch (%d != %d)", len(a), len(b))
	}
	prod := make([]float64, len(a))
	sa := make([]float64, len(a))
	sb := make([]float64, len(a))
	for i := range a {
		prod[i] = a[i] * b[i]
		sa[i] = a[i] * a[i]
		sb[i] = b[i] * b[i]
	}
	dot := PySum(prod)
	na := math.Sqrt(PySum(sa))
	nb := math.Sqrt(PySum(sb))
	if na == 0 || nb == 0 {
		return 0, nil
	}
	return dot / (na * nb), nil
}

type entry struct {
	record contracts.MemoryRecord
	vector []float64
}

// InMemorySemanticIndex is an exact-cosine SemanticIndex (python: InMemorySemanticIndex), the
// $0/offline default. Records are keyed by id (re-adding replaces and moves to the newest
// position); with MaxRecords > 0 the oldest are evicted. Queries only compare within the
// embedder's (model, version).
type InMemorySemanticIndex struct {
	embedder   contracts.Embedder
	maxRecords int
	order      []string
	entries    map[string]*entry
}

// NewInMemorySemanticIndex builds an index (maxRecords 0 = unbounded; negative is an error).
func NewInMemorySemanticIndex(embedder contracts.Embedder, maxRecords int) (*InMemorySemanticIndex, error) {
	if maxRecords < 0 {
		return nil, errors.New("max_records must be >= 1")
	}
	return &InMemorySemanticIndex{embedder: embedder, maxRecords: maxRecords, entries: map[string]*entry{}}, nil
}

func (x *InMemorySemanticIndex) drop(id string) {
	if _, ok := x.entries[id]; !ok {
		return
	}
	delete(x.entries, id)
	for i, o := range x.order {
		if o == id {
			x.order = append(x.order[:i], x.order[i+1:]...)
			break
		}
	}
}

// Add embeds and stores records, stamped with the embedder's model+version.
func (x *InMemorySemanticIndex) Add(ctx context.Context, records []contracts.MemoryRecord) error {
	if len(records) == 0 {
		return nil
	}
	texts := make([]string, len(records))
	for i, r := range records {
		texts[i] = r.Text
	}
	vectors, err := x.embedder.Embed(ctx, texts)
	if err != nil {
		return err
	}
	if len(vectors) != len(records) {
		return fmt.Errorf("embedder %s returned %d vectors for %d texts", contracts.PyRepr(x.embedder.Name()), len(vectors), len(records))
	}
	for i, r := range records {
		if len(vectors[i]) != x.embedder.Dim() {
			return fmt.Errorf("embedder %s returned a dim=%d vector; declared dim=%d",
				contracts.PyRepr(x.embedder.Name()), len(vectors[i]), x.embedder.Dim())
		}
		stamped := r
		stamped.EmbeddingModel, stamped.EmbeddingVersion = x.embedder.Name(), x.embedder.Version()
		x.drop(r.ID)
		x.entries[r.ID] = &entry{record: stamped, vector: vectors[i]}
		x.order = append(x.order, r.ID)
	}
	for x.maxRecords > 0 && len(x.order) > x.maxRecords {
		x.drop(x.order[0])
	}
	return nil
}

// Remove drops ids (size-bounded eviction; unknown ids ignored).
func (x *InMemorySemanticIndex) Remove(ids []string) {
	for _, id := range ids {
		x.drop(id)
	}
}

// Invalidate soft-forgets ids; returns the count.
func (x *InMemorySemanticIndex) Invalidate(ids []string) int {
	count := 0
	for _, id := range ids {
		if e, ok := x.entries[id]; ok && e.record.Valid {
			e.record.Valid = false
			count++
		}
	}
	return count
}

// Len is the number of entries.
func (x *InMemorySemanticIndex) Len() int { return len(x.order) }

// Query is the k most similar valid records of this embedder's model+version.
func (x *InMemorySemanticIndex) Query(ctx context.Context, text string, k int) ([]contracts.RetrievalHit, error) {
	if len(x.order) == 0 {
		return []contracts.RetrievalHit{}, nil
	}
	qv, err := x.embedder.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	hits := []contracts.RetrievalHit{}
	for _, id := range x.order {
		e := x.entries[id]
		if !e.record.Valid || e.record.EmbeddingModel != x.embedder.Name() || e.record.EmbeddingVersion != x.embedder.Version() {
			continue
		}
		score, err := Cosine(qv[0], e.vector)
		if err != nil {
			return nil, err
		}
		hits = append(hits, contracts.RetrievalHit{Record: e.record, Score: score})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}
