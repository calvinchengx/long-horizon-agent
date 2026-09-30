package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// Ported from python/tests/unit/test_mutation_gate.py (the wiring half): LHA_MUTATION_CHECK gates
// a local mission's item, and the command sees the item's changed files.

// writesThenDone writes parser.py, then (after the write's tool result) signals done, in every
// cycle and after every failed verification (a failed attempt is rolled back).
type writesThenDone struct{ *model.StubModel }

func (m *writesThenDone) Complete(_ context.Context, msgs []contracts.ModelMessage, _ []map[string]any, _ int) (contracts.TurnResult, error) {
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == "tool" {
		return done, nil
	}
	return nativeWrite("w", "parser.py", "x = 1\n"), nil
}

func TestALocalMissionIsGatedByTheMutationCheck(t *testing.T) {
	for _, c := range []struct {
		code      string
		completed bool
	}{{"0", true}, {"1", false}} {
		tmp := t.TempDir()
		seen := filepath.Join(tmp, "seen")
		command := `printf "%s" "$LHA_CHANGED_FILES" > ` + seen + `; exit ` + c.code
		settings := runnerSettings(t, "LHA_MUTATION_CHECK="+command, "LHA_MAX_CYCLES=2", "LHA_MAX_REPLANS=0")
		if _, ok := LeadVerifier(tmp, settings).(*verify.MutationGateVerifier); !ok {
			t.Fatal("LHA_MUTATION_CHECK did not wrap the lead's verifier")
		}
		m := &writesThenDone{StubModel: model.NewStub(nil)}
		s, err := RunMissionLocal(context.Background(), runOpts(t, filepath.Join(tmp, "ws"), settings, m, passCheck))
		got, _ := os.ReadFile(seen)
		if err != nil || s.Completed != c.completed || string(got) != "parser.py" {
			t.Fatalf("exit %s: %+v %v %q", c.code, s, err, got)
		}
	}
}
