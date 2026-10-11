package spec

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
)

// TestModelTierEventMatchesTheSchema proves the difficulty-based model-tier screen's system_one
// event (action "downgrade") matches its kind's schema, whose action enum now allows "downgrade"
// (python: test_a_confident_small_selects_the_next_cheaper_tier asserts event_errors == []).
func TestModelTierEventMatchesTheSchema(t *testing.T) {
	var c eventContract
	Load(t, "obs/mission_events.json", &c)
	stub := &systemone.Stub{Respond: func(_ any, _ []systemone.Named) map[string]systemone.Answer {
		return map[string]systemone.Answer{systemone.TierQuestionID: {
			Type: "choice", Choice: "small", Confidence: 0.94,
			Probabilities: contracts.NewOrderedMap("small", 0.94, "large", 0.06),
		}}
	}}
	verdict := systemone.NewTierScreen(stub).Assess(context.Background(), "one line", nil)
	if verdict.Action != systemone.ActionDowngrade || verdict.Tier != agents.TierHaiku {
		t.Fatalf("verdict: %+v", verdict)
	}
	raw, err := json.Marshal(verdict.Payload("01", 0.9))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if errs := obs.EventErrors(c.Kinds, "system_one", decoded); len(errs) != 0 {
		t.Fatalf("errors: %v (%s)", errs, raw)
	}
}
