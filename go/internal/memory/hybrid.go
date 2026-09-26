// Package memory is the memory plane (python: lha.memory): embedders, the semantic index, hybrid
// retrieval (BM25 + dense, fused with Reciprocal Rank Fusion), reranking, consolidation, the
// skill library and MissionMemory, the tiered memory wired into the agent loop.
//
// Every number and ordering that reaches a prompt is computed exactly as in Python (float sums use
// CPython's compensated sum(), ties keep insertion order), so the same inputs render the same
// memory block byte for byte (spec/memory/*.json).
package memory

import (
	"context"
	"errors"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

var tokenRE = regexp.MustCompile(`[a-z0-9]+`)

// Tokenize is the lexical tokenizer: runs of [a-z0-9] in the lowercased text.
func Tokenize(text string) []string {
	return tokenRE.FindAllString(strings.ToLower(text), -1)
}

// PySum is CPython (>= 3.12) sum() over floats starting from int 0: the first item is taken
// exactly, the rest are added with Neumaier compensation, and the compensation is added at the
// end when non-zero and finite.
func PySum(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum, c := xs[0], 0.0
	for _, x := range xs[1:] {
		t := sum + x
		if math.Abs(sum) >= math.Abs(x) {
			c += (sum - t) + x
		} else {
			c += (x - t) + sum
		}
		sum = t
	}
	if c != 0 && !math.IsInf(c, 0) && !math.IsNaN(c) {
		sum += c
	}
	return sum
}

// Scored is a document id with its score.
type Scored struct {
	ID    string
	Score float64
}

// BM25Index is a minimal in-memory BM25 keyword index. Re-adding an id replaces its document
// (and moves it to the newest position, like Python's dict pop + insert).
type BM25Index struct {
	k1, b float64
	order []string
	docs  map[string][]string
}

// NewBM25Index is an index with k1=1.5, b=0.75.
func NewBM25Index() *BM25Index { return &BM25Index{k1: 1.5, b: 0.75, docs: map[string][]string{}} }

func (x *BM25Index) removeID(id string) {
	if _, ok := x.docs[id]; !ok {
		return
	}
	delete(x.docs, id)
	for i, o := range x.order {
		if o == id {
			x.order = append(x.order[:i], x.order[i+1:]...)
			break
		}
	}
}

// Add indexes records (by id).
func (x *BM25Index) Add(records []contracts.MemoryRecord) {
	for _, r := range records {
		x.removeID(r.ID)
		x.docs[r.ID] = Tokenize(r.Text)
		x.order = append(x.order, r.ID)
	}
}

// Remove drops ids (unknown ids ignored).
func (x *BM25Index) Remove(ids []string) {
	for _, id := range ids {
		x.removeID(id)
	}
}

// Len is the number of documents.
func (x *BM25Index) Len() int { return len(x.order) }

// Query is the top k documents with a positive score, best first (ties in insertion order).
func (x *BM25Index) Query(text string, k int) []Scored {
	if len(x.order) == 0 {
		return []Scored{}
	}
	n := len(x.order)
	total := 0
	for _, id := range x.order {
		total += len(x.docs[id])
	}
	avgdl := float64(total) / float64(n)
	if avgdl == 0 {
		avgdl = 1.0
	}
	qTerms := []string{}
	seen := map[string]bool{}
	for _, t := range Tokenize(text) {
		if !seen[t] {
			seen[t] = true
			qTerms = append(qTerms, t)
		}
	}
	df := map[string]int{}
	for _, id := range x.order {
		inDoc := map[string]bool{}
		for _, t := range x.docs[id] {
			if seen[t] && !inDoc[t] {
				inDoc[t] = true
				df[t]++
			}
		}
	}
	scored := []Scored{}
	for _, id := range x.order {
		toks := x.docs[id]
		tf := map[string]int{}
		for _, t := range toks {
			tf[t]++
		}
		dl := float64(len(toks))
		score := 0.0
		for _, term := range qTerms {
			if df[term] == 0 {
				continue
			}
			d := float64(df[term])
			idf := math.Log(1 + (float64(n)-d+0.5)/(d+0.5))
			freq := float64(tf[term])
			denom := freq + x.k1*(1-x.b+x.b*dl/avgdl)
			if denom != 0 {
				score += idf * (freq * (x.k1 + 1)) / denom
			}
		}
		if score > 0 {
			scored = append(scored, Scored{id, score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if k >= 0 && len(scored) > k {
		scored = scored[:k]
	}
	return scored
}

// ReciprocalRankFusion fuses ranked id lists: score = sum 1/(k + rank), best first (ties in
// first-seen order). k <= 0 means 60.
func ReciprocalRankFusion(rankings [][]string, k int) []Scored {
	if k <= 0 {
		k = 60
	}
	index := map[string]int{}
	fused := []Scored{}
	for _, ranking := range rankings {
		for rank, id := range ranking {
			inc := 1.0 / float64(k+rank+1)
			if i, ok := index[id]; ok {
				fused[i].Score += inc
				continue
			}
			index[id] = len(fused)
			fused = append(fused, Scored{id, 0.0 + inc})
		}
	}
	sort.SliceStable(fused, func(i, j int) bool { return fused[i].Score > fused[j].Score })
	return fused
}

type remover interface{ Remove(ids []string) }
type invalidator interface{ Invalidate(ids []string) int }

// HybridRetriever combines a dense SemanticIndex and a BM25Index via RRF. Records are keyed by
// id: re-adding an id replaces it in both parts. Soft-invalidated records are never returned.
// With MaxRecords > 0 the oldest-inserted records are evicted (from both parts when the dense
// index supports Remove).
type HybridRetriever struct {
	semantic   contracts.SemanticIndex
	bm25       *BM25Index
	maxRecords int
	order      []string
	records    map[string]*contracts.MemoryRecord
}

// NewHybridRetriever builds a retriever (bm25 nil => a fresh index; maxRecords 0 = unbounded).
func NewHybridRetriever(semantic contracts.SemanticIndex, bm25 *BM25Index, maxRecords int) (*HybridRetriever, error) {
	if maxRecords < 0 {
		return nil, errors.New("max_records must be >= 1")
	}
	if bm25 == nil {
		bm25 = NewBM25Index()
	}
	return &HybridRetriever{semantic: semantic, bm25: bm25, maxRecords: maxRecords, records: map[string]*contracts.MemoryRecord{}}, nil
}

func (h *HybridRetriever) drop(id string) {
	if _, ok := h.records[id]; !ok {
		return
	}
	delete(h.records, id)
	for i, o := range h.order {
		if o == id {
			h.order = append(h.order[:i], h.order[i+1:]...)
			break
		}
	}
}

// Add adds (or replaces) records in both parts.
func (h *HybridRetriever) Add(ctx context.Context, records []contracts.MemoryRecord) error {
	// De-duplicate within the batch too (last one wins, first position), so both parts see one
	// copy per id.
	pos := map[string]int{}
	batch := []contracts.MemoryRecord{}
	for _, r := range records {
		if i, ok := pos[r.ID]; ok {
			batch[i] = r
			continue
		}
		pos[r.ID] = len(batch)
		batch = append(batch, r)
	}
	for _, r := range batch {
		h.drop(r.ID)
		rec := r
		h.records[r.ID] = &rec
		h.order = append(h.order, r.ID)
	}
	h.bm25.Add(batch)
	if err := h.semantic.Add(ctx, batch); err != nil {
		return err
	}
	if h.maxRecords > 0 && len(h.order) > h.maxRecords {
		evicted := append([]string{}, h.order[:len(h.order)-h.maxRecords]...)
		for _, id := range evicted {
			h.drop(id)
		}
		h.bm25.Remove(evicted)
		if r, ok := h.semantic.(remover); ok {
			r.Remove(evicted)
		}
	}
	return nil
}

// Invalidate soft-forgets ids (never returned again); returns the count.
func (h *HybridRetriever) Invalidate(ids []string) int {
	count := 0
	for _, id := range ids {
		if r, ok := h.records[id]; ok && r.Valid {
			r.Valid = false
			count++
		}
	}
	h.bm25.Remove(ids)
	if inv, ok := h.semantic.(invalidator); ok {
		inv.Invalidate(ids)
	}
	return count
}

// Len is the number of records.
func (h *HybridRetriever) Len() int { return len(h.order) }

// Query is the top k fused hits (candidateK from each part).
func (h *HybridRetriever) Query(ctx context.Context, text string, k, candidateK int) ([]contracts.RetrievalHit, error) {
	found, err := h.semantic.Query(ctx, text, candidateK)
	if err != nil {
		return nil, err
	}
	dense := []string{}
	for _, hit := range found {
		if hit.Record.Valid {
			dense = append(dense, hit.Record.ID)
		}
	}
	lexical := []string{}
	for _, s := range h.bm25.Query(text, candidateK) {
		lexical = append(lexical, s.ID)
	}
	hits := []contracts.RetrievalHit{}
	for _, f := range ReciprocalRankFusion([][]string{dense, lexical}, 60) {
		if r, ok := h.records[f.ID]; ok && r.Valid {
			hits = append(hits, contracts.RetrievalHit{Record: *r, Score: f.Score})
		}
		if len(hits) >= k {
			break
		}
	}
	return hits, nil
}
