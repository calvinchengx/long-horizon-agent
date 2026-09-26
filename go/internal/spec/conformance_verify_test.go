package spec

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// scriptedVerifier mirrors python/tests/unit/test_flaky_retry_verifier.py's _Scripted: one
// outcome ("pass" | "fail" | "timeout") per run, the last one repeating.
type scriptedVerifier struct {
	outcomes map[string][]string
	runs     []string
}

func (s *scriptedVerifier) Verify(_ context.Context, _ contracts.SandboxSession, checks []contracts.Check) (contracts.VerificationResult, error) {
	results := []contracts.CheckResult{}
	for _, c := range checks {
		s.runs = append(s.runs, c.Name)
		seq := s.outcomes[c.Name]
		outcome := seq[0]
		if len(seq) > 1 {
			s.outcomes[c.Name] = seq[1:]
		}
		exit := 1
		if outcome == "pass" {
			exit = 0
		}
		results = append(results, contracts.CheckResult{
			Name: c.Name, Passed: outcome == "pass", ExitCode: exit, Gating: c.Gating,
			TimedOut: outcome == "timeout", OutputTail: outcome,
		})
	}
	return contracts.NewVerificationResult(results), nil
}

func TestFlakyRetry(t *testing.T) {
	var s struct {
		Revision string `json:"revision"`
		Cases    []struct {
			Name        string              `json:"name"`
			Retries     int                 `json:"retries"`
			Calls       int                 `json:"calls"`
			Quarantined []string            `json:"quarantined"`
			Checks      [][2]any            `json:"checks"`
			Outcomes    map[string][]string `json:"outcomes"`
			Expected    json.RawMessage     `json:"expected"`
		} `json:"cases"`
	}
	Load(t, "verify/flaky_retry.json", &s)
	if len(s.Cases) == 0 || s.Revision == "" {
		t.Fatal("no cases")
	}
	type result struct {
		Name       string `json:"name"`
		Passed     bool   `json:"passed"`
		Gating     bool   `json:"gating"`
		TimedOut   bool   `json:"timed_out"`
		OutputTail string `json:"output_tail"`
	}
	type event struct {
		Kind    string         `json:"kind"`
		Payload map[string]any `json:"payload"`
	}
	type call struct {
		Verdict string   `json:"verdict"`
		Results []result `json:"results"`
		Events  []event  `json:"events"`
	}
	for _, c := range s.Cases {
		inner := &scriptedVerifier{outcomes: c.Outcomes}
		v := verify.NewFlakyRetryVerifier(inner, c.Retries, "")
		v.Revision = func(context.Context) (string, error) { return s.Revision, nil }
		restored := map[string]bool{}
		for _, name := range c.Quarantined {
			restored[name] = true
		}
		v.Quarantine.Restore(restored)
		checks := []contracts.Check{}
		for _, pair := range c.Checks {
			checks = append(checks, contracts.Check{Name: pair[0].(string), Command: []string{"x"},
				Gating: pair[1].(bool), Where: "sandbox"})
		}
		calls := []call{}
		for range max(1, c.Calls) {
			got, err := v.Verify(context.Background(), nil, checks)
			if err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
			out := call{Verdict: got.Verdict, Results: []result{}, Events: []event{}}
			for _, r := range got.Results {
				out.Results = append(out.Results, result{r.Name, r.Passed, r.Gating, r.TimedOut, r.OutputTail})
			}
			for _, e := range v.DrainEvents() {
				out.Events = append(out.Events, event{e.Kind, e.Payload})
			}
			calls = append(calls, out)
		}
		JSONEqual(t, c.Name, map[string]any{"runs": inner.runs, "calls": calls}, c.Expected)
	}
}
