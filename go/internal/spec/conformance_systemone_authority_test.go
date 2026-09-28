package spec

import (
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/memory"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
)

// TestSystemOneAuthority runs spec/systemone/authority.json: a System One answer can only ever
// narrow what happens. docs/25-system-one.md's central safety claim -- an answer "never allows a
// command, never answers a human gate and never marks an item done" -- as executable cases.
//
// The sweep includes answers a compromised or off-distribution model might return: option labels
// that were never offered, labels naming an authority-widening outcome, and maximum confidence
// under a zero threshold. Whatever comes back, triage may only continue, split or block, and
// reranking may only reorder and drop recalled passages, never add one.
func TestSystemOneAuthority(t *testing.T) {
	var s struct {
		Permitted []string `json:"permitted_actions"`
		Widening  []string `json:"widening_actions"`
		Triage    []struct {
			Choice     string  `json:"choice"`
			Confidence float64 `json:"confidence"`
			Threshold  float64 `json:"threshold"`
			CanSplit   bool    `json:"can_split"`
			Action     string  `json:"action"`
		} `json:"triage_actions"`
		Rerank []struct {
			Relevance []float64 `json:"relevance"`
			K         *int      `json:"k"`
			MinP      float64   `json:"min_p"`
			GivenIDs  []string  `json:"given_ids"`
			KeptIDs   []string  `json:"kept_ids"`
		} `json:"rerank"`
	}
	Load(t, "systemone/authority.json", &s)
	if len(s.Triage) == 0 || len(s.Rerank) == 0 {
		t.Fatal("the sweep must not be empty")
	}

	permitted := map[string]bool{}
	for _, a := range s.Permitted {
		permitted[a] = true
	}
	widening := map[string]bool{}
	for _, a := range s.Widening {
		if permitted[a] {
			t.Fatalf("%q is both permitted and authority-widening", a)
		}
		widening[a] = true
	}

	for _, c := range s.Triage {
		probs := contracts.NewOrderedMap()
		probs.Set(c.Choice, c.Confidence)
		a := systemone.Answer{
			Type:          "choice",
			Choice:        c.Choice,
			Probabilities: probs,
			Confidence:    c.Confidence,
		}
		got := systemone.TriageAction(a, c.Threshold, c.CanSplit)
		if got != c.Action {
			t.Errorf("TriageAction(%q, %v, %v, %v) = %s, want %s",
				c.Choice, c.Confidence, c.Threshold, c.CanSplit, got, c.Action)
		}
		// the invariant itself, not just the pinned value
		if !permitted[got] {
			t.Errorf("triage reached %q, outside the permitted actions %v (choice %q)", got, s.Permitted, c.Choice)
		}
		if widening[got] {
			t.Errorf("triage widened authority: %q (choice %q)", got, c.Choice)
		}
		if got == systemone.ActionSplit && !c.CanSplit {
			t.Errorf("split without the replanner's permission (choice %q)", c.Choice)
		}
	}

	for i, c := range s.Rerank {
		given := make([]contracts.RetrievalHit, len(c.GivenIDs))
		names := map[string]bool{}
		for j, id := range c.GivenIDs {
			given[j] = contracts.RetrievalHit{
				Record: contracts.MemoryRecord{ID: id, Kind: "semantic", Text: id},
				Score:  0.1,
			}
			names[id] = true
		}
		kept := memory.ApplyRelevance(given, c.Relevance, c.K, c.MinP)
		ids := []string{}
		seen := map[string]bool{}
		for _, h := range kept {
			// reranking may reorder and drop, never introduce
			if !names[h.Record.ID] {
				t.Errorf("rerank %d invented a passage: %q", i, h.Record.ID)
			}
			if seen[h.Record.ID] {
				t.Errorf("rerank %d duplicated a passage: %q", i, h.Record.ID)
			}
			seen[h.Record.ID] = true
			ids = append(ids, h.Record.ID)
		}
		if len(ids) != len(c.KeptIDs) {
			t.Errorf("rerank %d kept %v, want %v", i, ids, c.KeptIDs)
			continue
		}
		for j := range ids {
			if ids[j] != c.KeptIDs[j] {
				t.Errorf("rerank %d kept %v, want %v", i, ids, c.KeptIDs)
				break
			}
		}
	}
}
