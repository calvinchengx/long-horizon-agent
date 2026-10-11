package spec

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
)

// TestCommandSecondOpinionEventMatchesTheSchema proves the command second opinion's system_one
// event (action "gate", no item_id) matches its kind's schema (python:
// test_a_confident_dangerous_gives_the_reason asserts event_errors == []).
func TestCommandSecondOpinionEventMatchesTheSchema(t *testing.T) {
	var c eventContract
	Load(t, "obs/mission_events.json", &c)
	stub := &systemone.Stub{Respond: func(_ any, _ []systemone.Named) map[string]systemone.Answer {
		return map[string]systemone.Answer{systemone.CommandQuestionID: {
			Type: "choice", Choice: "dangerous", Confidence: 0.94,
			Probabilities: contracts.NewOrderedMap("dangerous", 0.94, "routine", 0.06),
		}}
	}}
	reason, payload := systemone.NewCommandSecondOpinion(stub).Reason(context.Background(), []string{"ls"})
	if reason != "the command looks dangerous (system one, confidence 0.94)" {
		t.Fatalf("reason: %q", reason)
	}
	raw, err := json.Marshal(payload)
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
