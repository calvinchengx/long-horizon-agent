package spec

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/memory"
	"github.com/calvinchengx/long-horizon-agent/go/internal/ops"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// TestMemoryHashEmbedderAndCosine runs spec/memory/hash_embedder.json: the hash embedder's
// vectors and cosine similarities are byte-identical to Python's (exact float equality).
func TestMemoryHashEmbedderAndCosine(t *testing.T) {
	var s struct {
		Hash []struct {
			Dim    int       `json:"dim"`
			Text   string    `json:"text"`
			Vector []float64 `json:"vector"`
		} `json:"hash"`
		Cosine []struct {
			A      []float64 `json:"a"`
			B      []float64 `json:"b"`
			Cosine float64   `json:"cosine"`
		} `json:"cosine"`
	}
	Load(t, "memory/hash_embedder.json", &s)
	if len(s.Hash) == 0 || len(s.Cosine) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range s.Hash {
		got := memory.NewHashEmbedder(c.Dim).EmbedOne(c.Text)
		if len(got) != len(c.Vector) {
			t.Fatalf("%q dim %d: len %d", c.Text, c.Dim, len(got))
		}
		for i := range got {
			if got[i] != c.Vector[i] {
				t.Fatalf("%q dim %d: [%d] = %v, want %v", c.Text, c.Dim, i, got[i], c.Vector[i])
			}
		}
	}
	for _, c := range s.Cosine {
		got, err := memory.Cosine(c.A, c.B)
		if err != nil || got != c.Cosine {
			t.Errorf("Cosine(%v, %v) = %v (%v), want %v", c.A, c.B, got, err, c.Cosine)
		}
	}
}

// TestMemoryRetrieval runs spec/memory/retrieval.json: BM25 ranking and scores (relative 1e-12),
// and Reciprocal Rank Fusion order and scores (exact).
func TestMemoryRetrieval(t *testing.T) {
	var s struct {
		Docs [][2]string `json:"bm25_docs"`
		BM25 []struct {
			Query string  `json:"query"`
			K     int     `json:"k"`
			Hits  [][]any `json:"hits"`
		} `json:"bm25"`
		Fusion []struct {
			Rankings [][]string `json:"rankings"`
			Fused    [][]any    `json:"fused"`
		} `json:"fusion"`
	}
	Load(t, "memory/retrieval.json", &s)
	index := memory.NewBM25Index()
	for _, d := range s.Docs {
		index.Add([]contracts.MemoryRecord{contracts.NewMemoryRecord(d[0], "semantic", d[1], nil)})
	}
	for _, c := range s.BM25 {
		got := index.Query(c.Query, c.K)
		if len(got) != len(c.Hits) {
			t.Fatalf("%q k=%d: %v, want %v", c.Query, c.K, got, c.Hits)
		}
		for i, h := range c.Hits {
			want, _ := h[1].(json.Number).Float64()
			if got[i].ID != h[0].(string) || math.Abs(got[i].Score-want) > 1e-12*math.Abs(want) {
				t.Errorf("%q k=%d hit %d: %v, want %v", c.Query, c.K, i, got[i], h)
			}
		}
	}
	for _, c := range s.Fusion {
		got := memory.ReciprocalRankFusion(c.Rankings, 60)
		if len(got) != len(c.Fused) {
			t.Fatalf("%v: %v, want %v", c.Rankings, got, c.Fused)
		}
		for i, f := range c.Fused {
			want, _ := f[1].(json.Number).Float64()
			if got[i].ID != f[0].(string) || got[i].Score != want {
				t.Errorf("%v [%d]: %v, want %v", c.Rankings, i, got[i], f)
			}
		}
	}
}

type memoryCase struct {
	Name     string            `json:"name"`
	Embedder string            `json:"embedder"`
	Files    map[string]string `json:"files"`
	Config   struct {
		PromptBudgetChars int    `json:"prompt_budget_chars"`
		EpisodicK         int    `json:"episodic_k"`
		SemanticK         int    `json:"semantic_k"`
		SkillsK           int    `json:"skills_k"`
		ConsolidateEvery  int    `json:"consolidate_every"`
		Consolidation     string `json:"consolidation"`
		IndexRepoFiles    bool   `json:"index_repo_files"`
		MaxRepoFiles      int    `json:"max_repo_files"`
	} `json:"config"`
	Observations []struct {
		CycleID         string   `json:"cycle_id"`
		ItemID          string   `json:"item_id"`
		ItemDescription string   `json:"item_description"`
		Verdict         string   `json:"verdict"`
		Verified        bool     `json:"verified"`
		Status          string   `json:"status"`
		Attempts        int      `json:"attempts"`
		Failure         string   `json:"failure"`
		DoneSummary     string   `json:"done_summary"`
		Tools           []string `json:"tools"`
	} `json:"observations"`
	Recalls []struct {
		CycleID   string                     `json:"cycle_id"`
		Item      contracts.ChecklistItem    `json:"item"`
		Decisions []contracts.DecisionRecord `json:"decisions"`
	} `json:"recalls"`
	Blocks []string `json:"blocks"`
}

// runMemoryCase is python/tests/unit/memory_spec_fixture.run_case.
func runMemoryCase(t *testing.T, c memoryCase) []string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := state.InitRepo(ctx, ws); err != nil {
		t.Fatal(err)
	}
	for rel, text := range c.Files {
		path := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := state.CommitAll(ctx, ws, "init"); err != nil {
		t.Fatal(err)
	}
	store, err := persistence.OpenSQLite(ctx, filepath.Join(root, "mem.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hybrid := c.Embedder == "hash"
	statuses := []ops.DependencyStatus{}
	var embedder contracts.Embedder
	if hybrid {
		embedder = memory.NewHashEmbedder(256)
	} else {
		statuses = append(statuses, ops.DependencyStatus{Name: "embeddings", Health: ops.HealthDegraded, Detail: "disabled (LHA_MEMORY_EMBEDDER=none)"})
	}
	cfg := c.Config
	mem := memory.New(memory.Options{
		Store: store, Workdir: ws, Mode: ops.DecideMemoryMode(statuses), Embedder: embedder, Namespace: "spec",
		Config: memory.Config{PromptBudgetChars: cfg.PromptBudgetChars, EpisodicK: cfg.EpisodicK, SemanticK: cfg.SemanticK,
			SkillsK: cfg.SkillsK, ConsolidateEvery: cfg.ConsolidateEvery, Consolidation: cfg.Consolidation,
			IndexRepoFiles: cfg.IndexRepoFiles, MaxRepoFiles: cfg.MaxRepoFiles},
	})
	for _, o := range c.Observations {
		mem.ObserveCycle(ctx, memory.CycleObservation{
			MissionID: "m1", CycleID: o.CycleID, ItemID: o.ItemID, ItemDescription: o.ItemDescription,
			Verdict: o.Verdict, Verified: o.Verified, Status: o.Status, Attempts: o.Attempts,
			Failure: o.Failure, DoneSummary: o.DoneSummary, Tools: o.Tools,
		})
	}
	blocks := []string{}
	for _, r := range c.Recalls {
		blocks = append(blocks, mem.Recall(ctx, "m1", r.CycleID, r.Item,
			contracts.SituationSnapshot{LastDecisions: r.Decisions}))
	}
	if mem.Errors() != 0 {
		t.Fatalf("%s: %d memory errors", c.Name, mem.Errors())
	}
	return blocks
}

// TestMemoryRecall runs spec/memory/recall.json: the episodic line format, the search terms, and
// the memory blocks MissionMemory recalls for a fixture (hybrid on SQLite, lexical with git grep,
// a tight budget), byte for byte.
func TestMemoryRecall(t *testing.T) {
	var s struct {
		Episodes []struct {
			Payload  json.RawMessage `json:"payload"`
			CycleID  string          `json:"cycle_id"`
			Rendered string          `json:"rendered"`
		} `json:"episodes"`
		Terms []struct {
			Text  string   `json:"text"`
			Terms []string `json:"terms"`
		} `json:"terms"`
		Cases []memoryCase `json:"cases"`
	}
	Load(t, "memory/recall.json", &s)
	for _, c := range s.Episodes {
		var payload map[string]any
		if err := decodeNumbers(c.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if got := memory.RenderEpisode(payload, c.CycleID); got != c.Rendered {
			t.Errorf("RenderEpisode:\n got %q\nwant %q", got, c.Rendered)
		}
	}
	for _, c := range s.Terms {
		got := memory.Terms(c.Text)
		if len(got) != len(c.Terms) {
			t.Errorf("Terms(%q) = %v, want %v", c.Text, got, c.Terms)
			continue
		}
		for i := range got {
			if got[i] != c.Terms[i] {
				t.Errorf("Terms(%q) = %v, want %v", c.Text, got, c.Terms)
			}
		}
	}
	if len(s.Cases) == 0 {
		t.Fatal("no recall cases")
	}
	for _, c := range s.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := runMemoryCase(t, c)
			if len(got) != len(c.Blocks) {
				t.Fatalf("%d blocks, want %d", len(got), len(c.Blocks))
			}
			for i := range got {
				if got[i] != c.Blocks[i] {
					t.Errorf("block %d:\n--- got\n%s\n--- want\n%s", i, got[i], c.Blocks[i])
				}
			}
		})
	}
}

func decodeNumbers(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}
