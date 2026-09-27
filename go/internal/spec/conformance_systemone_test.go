package spec

import (
	"encoding/json"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/memory"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
)

// ordered decodes raw keeping key order (a value as Python wrote it).
func ordered(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	v, err := contracts.DecodeOrdered(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// orderedJSON is v written by the ordered writer (key order and numbers as Python writes them).
func orderedJSON(t *testing.T, v any) string {
	t.Helper()
	m := contracts.NewOrderedMap("v", v)
	b, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func namedFromWire(t *testing.T, raw json.RawMessage) []systemone.Named {
	t.Helper()
	m, ok := ordered(t, raw).(*contracts.OrderedMap)
	if !ok {
		t.Fatalf("questions are not an object: %s", raw)
	}
	out := []systemone.Named{}
	for _, id := range m.Keys {
		q, err := systemone.QuestionFromWire(m.Values[id])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, systemone.Named{ID: id, Question: q})
	}
	return out
}

// TestSystemOneWire runs spec/systemone/wire.json: request bodies (with key and option order),
// strict answer parsing, the confidence formulas, the stall-triage question, action table and
// state, System One reranking, and endpoint locality and default prices.
func TestSystemOneWire(t *testing.T) {
	var s struct {
		Limits struct {
			MaxChoiceOptions int     `json:"max_choice_options"`
			MinScoreLevels   int     `json:"min_score_levels"`
			MaxScoreLevels   int     `json:"max_score_levels"`
			SumTolerance     float64 `json:"sum_tolerance"`
		} `json:"limits"`
		Requests []struct {
			Model     string          `json:"model"`
			State     json.RawMessage `json:"state"`
			Questions json.RawMessage `json:"questions"`
			Body      json.RawMessage `json:"body"`
			Error     bool            `json:"error"`
		} `json:"requests"`
		Questions json.RawMessage `json:"questions"`
		Responses []struct {
			Response json.RawMessage `json:"response"`
			Result   json.RawMessage `json:"result"`
			Error    bool            `json:"error"`
		} `json:"responses"`
		ChoiceConfidence []struct {
			Probabilities []float64 `json:"probabilities"`
			Confidence    float64   `json:"confidence"`
		} `json:"choice_confidence"`
		ScoreConfidence []struct {
			Probabilities []float64 `json:"probabilities"`
			Confidence    float64   `json:"confidence"`
		} `json:"score_confidence"`
		Triage struct {
			QuestionID      string          `json:"question_id"`
			Question        json.RawMessage `json:"question"`
			MaxFailureChars int             `json:"max_failure_chars"`
			Actions         []struct {
				Choice     string  `json:"choice"`
				Confidence float64 `json:"confidence"`
				Threshold  float64 `json:"threshold"`
				CanSplit   bool    `json:"can_split"`
				Action     string  `json:"action"`
			} `json:"actions"`
			States []struct {
				Item     contracts.ChecklistItem `json:"item"`
				Latest   string                  `json:"latest"`
				Previous string                  `json:"previous"`
				MaxChars int                     `json:"max_chars"`
				State    json.RawMessage         `json:"state"`
			} `json:"states"`
		} `json:"triage"`
		Rerank struct {
			Requests []struct {
				Query     string          `json:"query"`
				Texts     []string        `json:"texts"`
				State     json.RawMessage `json:"state"`
				Questions json.RawMessage `json:"questions"`
			} `json:"requests"`
			Apply []struct {
				Relevance []float64 `json:"relevance"`
				K         *int      `json:"k"`
				MinP      float64   `json:"min_p"`
				Order     []int     `json:"order"`
				Scores    []float64 `json:"scores"`
			} `json:"apply"`
		} `json:"rerank"`
		Endpoints []struct {
			Endpoint string   `json:"endpoint"`
			Local    bool     `json:"local"`
			Price    *float64 `json:"default_price_in_per_mtok"`
		} `json:"endpoints"`
	}
	Load(t, "systemone/wire.json", &s)
	if s.Limits.MaxChoiceOptions != systemone.MaxChoiceOptions || s.Limits.MinScoreLevels != systemone.MinScoreLevels ||
		s.Limits.MaxScoreLevels != systemone.MaxScoreLevels || s.Limits.SumTolerance != systemone.SumTolerance {
		t.Fatalf("limits differ from python: %+v", s.Limits)
	}
	if len(s.Requests) == 0 || len(s.Responses) == 0 || len(s.Triage.Actions) == 0 || len(s.Rerank.Apply) == 0 {
		t.Fatal("no cases")
	}

	for i, c := range s.Requests {
		body, err := systemone.RequestBody(c.Model, ordered(t, c.State), namedFromWire(t, c.Questions))
		if c.Error {
			if err == nil {
				t.Errorf("request %d: want an error", i)
			}
			continue
		}
		if err != nil {
			t.Errorf("request %d: %v", i, err)
			continue
		}
		if got, want := orderedJSON(t, body), orderedJSON(t, ordered(t, c.Body)); got != want {
			t.Errorf("request %d:\n got  %s\n want %s", i, got, want)
		}
	}

	asked := namedFromWire(t, s.Questions)
	for i, c := range s.Responses {
		result, err := systemone.ParseResponse(c.Response, asked)
		if c.Error {
			if err == nil {
				t.Errorf("response %d: want an error for %s", i, c.Response)
			}
			continue
		}
		if err != nil {
			t.Errorf("response %d: %v", i, err)
			continue
		}
		JSONEqual(t, "response", result.JSON(), c.Result)
	}

	for _, c := range s.ChoiceConfidence {
		if got := systemone.ChoiceConfidence(c.Probabilities); got != c.Confidence {
			t.Errorf("ChoiceConfidence(%v) = %v, want %v", c.Probabilities, got, c.Confidence)
		}
	}
	for _, c := range s.ScoreConfidence {
		if got := systemone.ScoreConfidence(c.Probabilities); got != c.Confidence {
			t.Errorf("ScoreConfidence(%v) = %v, want %v", c.Probabilities, got, c.Confidence)
		}
	}

	if s.Triage.QuestionID != systemone.TriageQuestionID || s.Triage.MaxFailureChars != systemone.MaxFailureChars {
		t.Errorf("triage constants differ: %q %d", s.Triage.QuestionID, s.Triage.MaxFailureChars)
	}
	if got, want := orderedJSON(t, systemone.TriageQuestion().Body()), orderedJSON(t, ordered(t, s.Triage.Question)); got != want {
		t.Errorf("triage question:\n got  %s\n want %s", got, want)
	}
	causes := systemone.Causes().Keys
	for _, c := range s.Triage.Actions {
		probs := make([]float64, len(causes))
		for i, k := range causes {
			probs[i] = 0.05
			if k == c.Choice {
				probs[i] = 0.9
			}
		}
		a := systemone.ChoiceAnswer(causes, probs)
		a.Confidence = c.Confidence
		if got := systemone.TriageAction(a, c.Threshold, c.CanSplit); got != c.Action {
			t.Errorf("TriageAction(%s, %v, %v, %v) = %s, want %s", c.Choice, c.Confidence, c.Threshold, c.CanSplit, got, c.Action)
		}
	}
	for i, c := range s.Triage.States {
		state := systemone.TriageState(c.Item, c.Latest, c.Previous, c.MaxChars)
		if got, want := orderedJSON(t, state), orderedJSON(t, ordered(t, c.State)); got != want {
			t.Errorf("triage state %d:\n got  %s\n want %s", i, got, want)
		}
	}

	hits := func(texts []string) []contracts.RetrievalHit {
		out := make([]contracts.RetrievalHit, len(texts))
		for i, text := range texts {
			out[i] = contracts.RetrievalHit{Record: contracts.MemoryRecord{ID: "r" + string(rune('0'+i)), Kind: "semantic", Text: text}, Score: 0.1}
		}
		return out
	}
	for i, c := range s.Rerank.Requests {
		state, questions := memory.SystemOneRerankRequest(c.Query, hits(c.Texts))
		if got, want := orderedJSON(t, state), orderedJSON(t, ordered(t, c.State)); got != want {
			t.Errorf("rerank state %d:\n got  %s\n want %s", i, got, want)
		}
		bodies := contracts.NewOrderedMap()
		for _, q := range questions {
			bodies.Set(q.ID, q.Question.Body())
		}
		if got, want := orderedJSON(t, bodies), orderedJSON(t, ordered(t, c.Questions)); got != want {
			t.Errorf("rerank questions %d:\n got  %s\n want %s", i, got, want)
		}
	}
	for _, c := range s.Rerank.Apply {
		texts := make([]string, len(c.Relevance))
		for i := range texts {
			texts[i] = "t"
		}
		ranked := memory.ApplyRelevance(hits(texts), c.Relevance, c.K, c.MinP)
		order, scores := []int{}, []float64{}
		for _, h := range ranked {
			order = append(order, int(h.Record.ID[1]-'0'))
			scores = append(scores, h.Score)
		}
		JSONEqual(t, "apply order", order, rawJSON(t, c.Order))
		JSONEqual(t, "apply scores", scores, rawJSON(t, c.Scores))
	}

	for _, c := range s.Endpoints {
		target, err := safety.ParseURL(c.Endpoint)
		if err != nil {
			t.Fatal(err)
		}
		if systemone.IsLocalEndpoint(target) != c.Local {
			t.Errorf("IsLocalEndpoint(%s) != %v", c.Endpoint, c.Local)
		}
		price, err := systemone.DefaultPriceInPerMTok(c.Endpoint)
		if err != nil {
			t.Fatal(err)
		}
		if (price == nil) != (c.Price == nil) || (price != nil && *price != *c.Price) {
			t.Errorf("DefaultPriceInPerMTok(%s) = %v, want %v", c.Endpoint, price, c.Price)
		}
	}
}

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
