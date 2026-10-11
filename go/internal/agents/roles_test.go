package agents

import (
	"context"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

func TestModelForRoleRoutesTiersOnClaudeOnly(t *testing.T) {
	claude, err := config.LoadFrom([]string{"LHA_MODEL_BACKEND=claude", "LHA_ANTHROPIC_API_KEY=k"}, "")
	if err != nil {
		t.Fatal(err)
	}
	for role, want := range map[string]string{
		"lead": "claude-opus-4-8", "reviewer": "claude-opus-4-8", "researcher": "claude-haiku-4-5-20251001",
		"implementer": "claude-sonnet-4-6",
	} {
		p, err := ModelForRole(role, claude, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.(*model.ClaudeModel).Name(); got != "claude:"+want && got != want {
			t.Errorf("%s: %s, want %s", role, got, want)
		}
	}
	stub, _ := config.LoadFrom([]string{"LHA_MODEL_BACKEND=stub"}, "")
	for _, role := range []string{"lead", "researcher", "unknown"} {
		p, err := ModelForRole(role, stub, nil)
		if err != nil || p.Name() != "stub:"+stub.ModelName {
			t.Fatalf("%s: %v %v", role, p, err)
		}
	}
}

func TestCheaperTierAndModelForTierMapTheClaudeTiers(t *testing.T) {
	for _, c := range []struct {
		tier ModelTier
		want ModelTier
		ok   bool
	}{
		{TierOpus, TierSonnet, true},
		{TierSonnet, TierHaiku, true},
		{TierHaiku, "", false},
	} {
		if got, ok := CheaperTier(c.tier); got != c.want || ok != c.ok {
			t.Errorf("CheaperTier(%q) = %q, %v; want %q, %v", c.tier, got, ok, c.want, c.ok)
		}
	}
	claude, err := config.LoadFrom([]string{"LHA_MODEL_BACKEND=claude", "LHA_ANTHROPIC_API_KEY=k"}, "")
	if err != nil {
		t.Fatal(err)
	}
	for tier, want := range map[ModelTier]string{
		TierSonnet: "claude-sonnet-4-6", TierHaiku: "claude-haiku-4-5-20251001",
	} {
		p, err := ModelForTier(tier, claude, nil)
		if err != nil || p.(*model.ClaudeModel).Name() != "claude:"+want {
			t.Errorf("ModelForTier(%q): %v %v", tier, p, err)
		}
	}
	stub, _ := config.LoadFrom([]string{"LHA_MODEL_BACKEND=stub"}, "")
	p, err := ModelForTier(TierHaiku, stub, nil)
	if err != nil || p.Name() != "stub:"+stub.ModelName {
		t.Fatalf("non-claude: %v %v", p, err)
	}
}

type fixed struct{ *model.StubModel }

func TestReflectOnFailureCapsTheText(t *testing.T) {
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'x'
	}
	m := model.NewStub([]contracts.TurnResult{{Text: string(long)}})
	got, err := ReflectOnFailure(context.Background(), fixed{m}, "item", "failure")
	if err != nil || len(got) != ReflectionCap {
		t.Fatalf("%d %v", len(got), err)
	}
}
