package durable

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// A cycle uses the key the environment holds now and records the rotation in the anchor
// (python: test_secret_rotation.py::test_a_cycle_uses_the_rotated_key_and_records_it).
func TestACycleUsesTheRotatedKeyAndRecordsIt(t *testing.T) {
	t.Setenv("LHA_ANTHROPIC_API_KEY", "key-rotated")
	// The worker started with another key (and a .env-free directory).
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	inp := initMission(t, 1)
	started := testSettings(t, "LHA_ANTHROPIC_API_KEY=key-at-start")
	var seen []string
	factory := func(s *config.Settings, snap contracts.SituationSnapshot) (contracts.ModelProvider, error) {
		seen = append(seen, s.AnthropicAPIKey.Value())
		return model.NewStub([]contracts.TurnResult{writeTurn("work/01.txt"), doneTurn()}), nil
	}
	acts := &Activities{Settings: started, ModelFactory: factory, OpenToolbox: testToolbox}
	res, err := acts.RunAgentCycle(context.Background(), CycleInput{MissionID: inp.MissionID, Workdir: inp.Workdir, CycleID: "c1", CheckCommands: checkCommands})
	if err != nil || !res.Advanced {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Join(seen, ",") != "key-rotated" {
		t.Fatalf("the model saw %v", seen)
	}
	if started.AnthropicAPIKey.Value() != "key-at-start" {
		t.Fatal("the worker's settings changed")
	}
	events := committedEvents(t, inp.Workdir)
	n := 0
	for _, e := range events {
		if e.Kind != SecretsRotatedEvent {
			continue
		}
		n++
		raw, _ := e.Payload.MarshalJSON()
		fp := config.SecretFingerprint(config.NewSecret("key-rotated"))
		if e.CycleID != "c1" || !strings.Contains(string(raw), `"fields":["anthropic_api_key"]`) ||
			!strings.Contains(string(raw), `"anthropic_api_key":"`+fp+`"`) {
			t.Fatalf("event %s", raw)
		}
		if strings.Contains(string(raw), "key-rotated") {
			t.Fatal("the event holds the value")
		}
	}
	if n != 1 {
		t.Fatalf("%d rotation events", n)
	}
}
