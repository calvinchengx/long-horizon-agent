package memory

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// Ported from python/tests/unit/test_memory.py, test_hybrid_memory.py and test_mem_fixes.py.

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

var bg = context.Background()

func rec(id, text string) contracts.MemoryRecord {
	return contracts.NewMemoryRecord(id, "semantic", text, nil)
}

func threeRecords() []contracts.MemoryRecord {
	return []contracts.MemoryRecord{
		rec("1", "postgres database with pgvector for embeddings"),
		rec("2", "react frontend written in typescript"),
		rec("3", "temporal workflow durable execution spine"),
	}
}

func TestHashEmbedderIsDeterministicAndUnitNorm(t *testing.T) {
	e := NewHashEmbedder(64)
	v1, v2 := e.EmbedOne("hello world"), e.EmbedOne("hello world")
	sum := 0.0
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatal("not deterministic")
		}
		sum += v1[i] * v1[i]
	}
	if len(v1) != 64 || math.Abs(sum-1) > 1e-9 {
		t.Fatal(len(v1), sum)
	}
	if e.Name() != "hash" || e.Version() != "1" || NewHashEmbedder(0).Dim() != 256 {
		t.Fatal("identity")
	}
}

func TestSemanticIndexRetrievesMostSimilarAndStampsModelVersion(t *testing.T) {
	index, _ := NewInMemorySemanticIndex(NewHashEmbedder(1024), 0)
	if err := index.Add(bg, []contracts.MemoryRecord{rec("1", "the database uses postgres and pgvector"), rec("2", "the frontend uses react and typescript")}); err != nil {
		t.Fatal(err)
	}
	hits, err := index.Query(bg, "how is data stored in postgres", 1)
	if err != nil || len(hits) != 1 || hits[0].Record.ID != "1" {
		t.Fatal(hits, err)
	}
	if hits[0].Record.EmbeddingModel != "hash" || hits[0].Record.EmbeddingVersion != "1" {
		t.Fatalf("%+v", hits[0].Record)
	}
	invalid := rec("x", "postgres")
	invalid.Valid = false
	other, _ := NewInMemorySemanticIndex(NewHashEmbedder(0), 0)
	_ = other.Add(bg, []contracts.MemoryRecord{invalid})
	if hits, _ := other.Query(bg, "postgres", 5); len(hits) != 0 {
		t.Fatal(hits)
	}
}

func TestCosineBasics(t *testing.T) {
	if c, _ := Cosine([]float64{1, 0}, []float64{1, 0}); math.Abs(c-1) > 1e-12 {
		t.Fatal(c)
	}
	if c, _ := Cosine([]float64{1, 0}, []float64{0, 1}); c != 0 {
		t.Fatal(c)
	}
	if c, _ := Cosine([]float64{0, 0}, []float64{1, 0}); c != 0 {
		t.Fatal(c)
	}
	if _, err := Cosine([]float64{1, 0}, nil); err == nil || !strings.Contains(err.Error(), "length mismatch") {
		t.Fatal(err)
	}
}

func TestPySumIsCPythonsCompensatedSum(t *testing.T) {
	// sum([0.1] * 10) is exactly 1.0 in CPython >= 3.12 (naive addition gives 0.9999999999999999).
	xs := make([]float64, 10)
	for i := range xs {
		xs[i] = 0.1
	}
	if PySum(xs) != 1.0 || PySum(nil) != 0 || PySum([]float64{1e300, 1e300, -1e300}) != 1e300 {
		t.Fatal(PySum(xs))
	}
}

func TestSkillStoreRequiresVerificationThenFinds(t *testing.T) {
	store := NewInMemorySkillStore(NewHashEmbedder(0))
	var notVerified *contracts.SkillNotVerifiedError
	if err := store.Add(bg, contracts.Skill{ID: "s1", Name: "run tests", Description: "run pytest"}); !errors.As(err, &notVerified) {
		t.Fatal(err)
	}
	if err := store.Add(bg, contracts.Skill{ID: "s1", Name: "run tests", Description: "run the pytest suite with uv", Code: "uv run pytest", Verified: true}); err != nil {
		t.Fatal(err)
	}
	found, err := store.Find(bg, "how do I execute the test suite", 1, "")
	if err != nil || len(found) != 1 || found[0].ID != "s1" {
		t.Fatal(found, err)
	}
	if found, _ := store.Find(bg, "test suite", 1, "/other"); len(found) != 0 {
		t.Fatal("namespace leak")
	}
}

func TestBM25RanksKeywordMatchFirst(t *testing.T) {
	index := NewBM25Index()
	index.Add(threeRecords())
	if ranked := index.Query("pgvector embeddings", 3); len(ranked) == 0 || ranked[0].ID != "1" {
		t.Fatal(ranked)
	}
}

func TestRRFFusesRankings(t *testing.T) {
	fused := ReciprocalRankFusion([][]string{{"a", "b", "c"}, {"b", "a", "d"}}, 0)
	ids := map[string]bool{}
	for _, f := range fused {
		ids[f.ID] = true
	}
	if (fused[0].ID != "a" && fused[0].ID != "b") || len(ids) != 4 {
		t.Fatal(fused)
	}
}

func TestHybridRetrieverCombinesSignals(t *testing.T) {
	sem, _ := NewInMemorySemanticIndex(NewHashEmbedder(512), 0)
	h, _ := NewHybridRetriever(sem, nil, 0)
	if err := h.Add(bg, threeRecords()); err != nil {
		t.Fatal(err)
	}
	hits, err := h.Query(bg, "durable temporal workflow", 2, 20)
	if err != nil || hits[0].Record.ID != "3" {
		t.Fatal(hits, err)
	}
}

func TestHybridNeverReturnsInvalidatedRecordsAndReaddingReplaces(t *testing.T) {
	sem, _ := NewInMemorySemanticIndex(NewHashEmbedder(0), 0)
	h, _ := NewHybridRetriever(sem, nil, 0)
	_ = h.Add(bg, threeRecords())
	if n := h.Invalidate([]string{"3", "zzz"}); n != 1 {
		t.Fatal(n)
	}
	hits, _ := h.Query(bg, "durable temporal workflow", 5, 20)
	for _, hit := range hits {
		if hit.Record.ID == "3" {
			t.Fatal("invalidated record returned")
		}
	}
	_ = h.Add(bg, []contracts.MemoryRecord{rec("1", "first"), rec("1", "second copy")})
	if h.Len() != 3 {
		t.Fatal(h.Len())
	}
	hits, _ = h.Query(bg, "second copy", 5, 20)
	if len(hits) == 0 || hits[0].Record.Text != "second copy" {
		t.Fatal(hits)
	}
}

func TestMaxRecordsEvictsOldestEverywhere(t *testing.T) {
	sem, _ := NewInMemorySemanticIndex(NewHashEmbedder(0), 0)
	h, _ := NewHybridRetriever(sem, nil, 2)
	_ = h.Add(bg, threeRecords())
	if h.Len() != 2 || sem.Len() != 2 {
		t.Fatal(h.Len(), sem.Len())
	}
	bounded, _ := NewInMemorySemanticIndex(NewHashEmbedder(0), 1)
	_ = bounded.Add(bg, threeRecords())
	if hits, _ := bounded.Query(bg, "temporal workflow", 5); bounded.Len() != 1 || len(hits) != 1 || hits[0].Record.ID != "3" {
		t.Fatal(hits)
	}
	if _, err := NewHybridRetriever(sem, nil, -1); err == nil {
		t.Fatal("negative max accepted")
	}
}

type wrongDim struct{ *HashEmbedder }

func (w wrongDim) Dim() int { return 3 }

func TestIndexesRejectVectorsOfTheWrongWidth(t *testing.T) {
	index, _ := NewInMemorySemanticIndex(wrongDim{NewHashEmbedder(8)}, 0)
	if err := index.Add(bg, threeRecords()); err == nil || !strings.Contains(err.Error(), "declared dim=3") {
		t.Fatal(err)
	}
	if _, err := NewPgSemanticIndex(nil, NewHashEmbedder(256), 1024, ""); err == nil || !strings.Contains(err.Error(), "vector(1024)") {
		t.Fatal(err)
	}
}

func TestNoopRerankerIsPassthrough(t *testing.T) {
	hits := []contracts.RetrievalHit{{Record: threeRecords()[0], Score: 0.5}, {Record: threeRecords()[1], Score: 0.4}}
	got, _ := NoopReranker{}.Rerank(bg, "q", hits, 0)
	if len(got) != 2 {
		t.Fatal(got)
	}
	if got, _ := (NoopReranker{}).Rerank(bg, "q", hits, 1); len(got) != 1 {
		t.Fatal(got)
	}
	if _, err := NewCrossEncoderReranker(""); !errors.Is(err, ErrCrossEncoderUnsupported) {
		t.Fatal(err)
	}
}

func TestConsolidateParsesFactsAndFallsBackOnGarbage(t *testing.T) {
	result, err := Consolidate(bg, model.NewStub([]contracts.TurnResult{{Text: `["fact one", "fact two"]`}}), []string{"e1", "e2"}, "", 0)
	if err != nil || !result.OK() || len(result.Facts) != 2 || result.Facts[0].Text != "fact one" || result.Facts[1].Kind != "semantic" {
		t.Fatalf("%+v %v", result, err)
	}
	result, _ = Consolidate(bg, model.NewStub([]contracts.TurnResult{{Text: "no json here"}}), []string{"only episode", "only episode"}, "m1", 0)
	if result.OK() || result.FailedBatches != 1 || len(result.Facts) != 1 || result.Facts[0].Text != "only episode" ||
		result.Facts[0].Metadata["consolidation"] != "verbatim" || result.Facts[0].Metadata["mission_id"] != "m1" {
		t.Fatalf("%+v", result)
	}
	result, _ = Consolidate(bg, model.NewStub([]contracts.TurnResult{{Text: "[]"}}), []string{"e"}, "", 0)
	if !result.OK() || len(result.Facts) != 0 {
		t.Fatalf("%+v", result) // an empty list is a valid (if unhelpful) answer
	}
}

func TestConsolidationCoversEveryEpisodeInBatches(t *testing.T) {
	episodes := []string{"a", "b", "c", "d", "e"}
	stub := model.NewStub([]contracts.TurnResult{{Text: `["x"]`}})
	result, err := Consolidate(bg, stub, episodes, "", 2)
	if err != nil || result.Batches != 3 || len(result.Facts) != 3 {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err := Consolidate(bg, stub, episodes, "", -1); err == nil {
		t.Fatal("bad batch size accepted")
	}
}

func TestSoftInvalidateMarksMatching(t *testing.T) {
	records := threeRecords()
	if n := SoftInvalidate(records, func(r contracts.MemoryRecord) bool { return r.ID == "2" }); n != 1 || records[1].Valid {
		t.Fatal(n)
	}
}

func TestParseFactsEdgeCases(t *testing.T) {
	for text, want := range map[string]bool{
		`Here: ["a", " b ", 3]`: true, `[1] and [2]`: false, `{"a": 1}`: false, `[`: false, `nothing`: false,
	} {
		if _, ok := parseFacts(text); ok != want {
			t.Errorf("parseFacts(%q) ok = %v", text, ok)
		}
	}
	if facts, _ := parseFacts(`["a", " b ", 3, ""]`); strings.Join(facts, "|") != "a|b|3" {
		t.Fatal(facts)
	}
}
