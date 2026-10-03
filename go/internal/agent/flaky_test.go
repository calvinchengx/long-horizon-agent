package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// Ported from python/tests/unit/test_flaky_retry_verifier.py (the wiring half): LHA_FLAKY_RETRIES
// reaches the lead's verifier, and a quarantine is committed with the cycle's checkpoint.

func TestLeadVerifierUsesTheConfiguredRetries(t *testing.T) {
	v, ok := LeadVerifier(t.TempDir(), runnerSettings(t, "LHA_FLAKY_RETRIES=3")).(*verify.FlakyRetryVerifier)
	if !ok || v.Retries() != 3 {
		t.Fatal(ok, v)
	}
	if def, ok := LeadVerifier(t.TempDir(), runnerSettings(t)).(*verify.FlakyRetryVerifier); !ok || def.Retries() != 1 {
		t.Fatal(ok, def)
	}
}

func TestLocalMissionQuarantinesAFlakyCheckAndCommitsTheEvent(t *testing.T) {
	tmp := t.TempDir()
	counter := filepath.Join(tmp, "counter")
	// Fails on its first run only.
	flip := `n=$(cat "$1" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$1"; [ "$n" -ne 1 ]`
	flaky := contracts.Check{Name: "flaky", Gating: true, Where: "sandbox", Command: []string{"sh", "-c", flip, "sh", counter}}
	solid := contracts.Check{Name: "solid", Gating: true, Where: "sandbox", Command: []string{"true"}}
	workdir := filepath.Join(tmp, "ws")
	o := runOpts(t, workdir, runnerSettings(t), model.NewStub([]contracts.TurnResult{done}), flaky, solid)
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil || !s.Completed { // "solid" gated the item; "flaky" was quarantined, not trusted
		t.Fatalf("%+v %v", s, err)
	}
	ctx := context.Background()
	raw, err := state.RunGit(ctx, workdir, "show", "HEAD:.lha/events.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	var quarantine, cycle *contracts.EventRecord
	for _, line := range strings.Split(raw, "\n") {
		var e contracts.EventRecord
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(line, err)
		}
		switch e.Kind {
		case verify.QuarantineEvent:
			quarantine = &e
		case "cycle":
			if cycle == nil {
				cycle = &e
			}
		}
	}
	if quarantine == nil || quarantine.CycleID == "" || quarantine.Payload.Plain()["check"] != "flaky" ||
		quarantine.Payload.Plain()["passes"] != 1.0 || quarantine.Payload.Plain()["fails"] != 1.0 {
		t.Fatalf("quarantine event: %+v\n%s", quarantine, raw)
	}
	if cycle == nil {
		t.Fatal(raw)
	}
	found := false
	for _, c := range cycle.Payload.Plain()["checks"].([]any) {
		c := c.(map[string]any)
		if c["name"] == "flaky" {
			found = true
			if c["passed"] != true || c["gating"] != false {
				t.Fatalf("%+v", c)
			}
		}
	}
	if !found {
		t.Fatalf("no flaky check in %+v", cycle.Payload)
	}
	if got := verify.CommittedQuarantine(ctx, workdir); len(got) != 1 || !got["flaky"] {
		t.Fatal(got)
	}
	if !strings.Contains(s.TraceJSONL, `"kind":"check_quarantined"`) {
		t.Fatalf("trace: %s", s.TraceJSONL)
	}
}

// The flaky-retry wrapper must keep the trusted runner's LHA_TRUSTED_CHECK_ENV allow-list (both
// landed separately; a verifier built without it would silently drop the operator's names).
func TestLeadVerifierKeepsTheTrustedCheckEnvAllowList(t *testing.T) {
	runner := trustedRunnerFor(runnerSettings(t, "LHA_TRUSTED_CHECK_ENV=GOFLAGS,GOPROXY"))
	if strings.Join(runner.EnvAllow, ",") != "GOFLAGS,GOPROXY" {
		t.Fatalf("EnvAllow = %v", runner.EnvAllow)
	}
	if got := trustedRunnerFor(runnerSettings(t, "LHA_TRUSTED_CHECK_ENV=LHA_SECRET")); len(got.EnvAllow) != 0 {
		t.Fatalf("an invalid allow-list must fall back to passing nothing through, got %v", got.EnvAllow)
	}
}

func TestAQuarantinedCheckThatThenAlwaysFailsIsTraced(t *testing.T) {
	tmp := t.TempDir()
	counter := filepath.Join(tmp, "counter")
	// Fails, passes (quarantined in the first cycle), then fails every run after.
	flip := `n=$(cat "$1" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$1"; [ "$n" -eq 2 ]`
	flaky := contracts.Check{Name: "flaky", Gating: true, Where: "sandbox", Command: []string{"sh", "-c", flip, "sh", counter}}
	solid := contracts.Check{Name: "solid", Gating: true, Where: "sandbox", Command: []string{"true"}}
	o := runOpts(t, filepath.Join(tmp, "ws"), runnerSettings(t, "LHA_MAX_CYCLES=2"),
		model.NewStub([]contracts.TurnResult{done, done}), flaky, solid)
	o.Checklist = contracts.Checklist{Items: []contracts.ChecklistItem{item("01", "a"), item("02", "b")}}
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	type traced struct {
		Kind    string         `json:"kind"`
		CycleID string         `json:"cycle_id"`
		Data    map[string]any `json:"data"`
	}
	var events []traced
	for _, line := range strings.Split(strings.TrimSpace(s.TraceJSONL), "\n") {
		var e traced
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(e.Kind, "quarantine") {
			events = append(events, e)
		}
	}
	// Quarantined in c1; in c2 every verification (one per "done") fails it on every attempt.
	if len(events) < 2 || events[0].Kind != verify.QuarantineEvent || events[0].CycleID != "c1" {
		t.Fatalf("%+v", events)
	}
	for _, e := range events[1:] {
		if e.Kind != verify.QuarantinedFailureEvent || e.CycleID != "c2" || e.Data["check"] != "flaky" || e.Data["passes"] != 0.0 {
			t.Fatalf("%+v", events)
		}
	}
}
