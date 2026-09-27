package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
)

func hitsOf(texts ...string) []contracts.RetrievalHit {
	out := make([]contracts.RetrievalHit, len(texts))
	for i, text := range texts {
		out[i] = contracts.RetrievalHit{Record: contracts.MemoryRecord{ID: text, Kind: "semantic", Text: text}, Score: 0.5}
	}
	return out
}

func TestSystemOneRerankerRanksByRelevanceAndFallsBackOnFailure(t *testing.T) {
	stub := &systemone.Stub{Respond: func(_ any, qs []systemone.Named) map[string]systemone.Answer {
		out := map[string]systemone.Answer{}
		for _, q := range qs {
			passage, _ := q.Question.Instructions.(*contracts.OrderedMap).String("passage")
			p := 0.1
			if strings.Contains(passage, "parser") {
				p = 0.9
			}
			out[q.ID] = systemone.Answer{Type: "noul", Noul: p}
		}
		return out
	}}
	hits := hitsOf("unrelated note", "the parser fails on tabs")
	ranked, err := (&SystemOneReranker{Model: stub}).Rerank(context.Background(), "fix it", hits, 1)
	if err != nil || len(ranked) != 1 || ranked[0].Record.Text != "the parser fails on tabs" || ranked[0].Score != 0.9 {
		t.Fatalf("%+v %v", ranked, err)
	}
	broken := &SystemOneReranker{Model: &systemone.Stub{Err: "down"}}
	if got, _ := broken.Rerank(context.Background(), "fix it", hits, 1); len(got) != 1 || got[0].Record.Text != "unrelated note" {
		t.Fatalf("fallback: %+v", got)
	}
	if got, _ := broken.Rerank(context.Background(), "fix it", nil, 1); len(got) != 0 {
		t.Fatalf("empty: %+v", got)
	}
}
