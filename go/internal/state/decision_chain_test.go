package state

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

func legacyLine(t *testing.T, decision string) string {
	t.Helper()
	b := must(pydanticJSON(contracts.DecisionRecord{Decision: decision, Rationale: "r"}, false))
	return string(b)
}

func commitDecisions(t *testing.T, dir, text string) {
	t.Helper()
	ctx := context.Background()
	writeFile(t, filepath.Join(dir, AnchorDir, DecisionsFile), text)
	must(RunGit(ctx, dir, "add", "-f", AnchorDir+"/"+DecisionsFile))
	must(RunGit(ctx, dir, "commit", "-qm", "edit decisions"))
}

func TestDecisionChainLegacyPrefixIsSealed(t *testing.T) {
	legacy := []string{legacyLine(t, "old one"), legacyLine(t, "old two")}
	running := GenesisHash
	for _, ln := range legacy {
		v, err := decodeLine([]byte(ln))
		if err != nil {
			t.Fatal(err)
		}
		running = must(ChainHash(running, v))
	}
	chained, _, err := EncodeDecisionLink(running, contracts.DecisionRecord{Decision: "new", Rationale: "r"})
	if err != nil {
		t.Fatal(err)
	}
	join := func(lines ...string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }

	check := VerifyDecisionChain(join(legacy[0], legacy[1], chained))
	if !check.OK || check.Checked != 3 || check.Legacy != 2 {
		t.Fatalf("%+v", check)
	}
	chain := must(ParseDecisionChain(join(legacy[0], legacy[1], chained)))
	if len(chain.Records) != 3 || chain.Records[2].Decision != "new" || chain.Legacy != 2 {
		t.Fatalf("%+v", chain)
	}
	if c := VerifyDecisionChain(join(legacyLine(t, "forged"), legacy[1], chained)); c.OK || c.Problem != "line 3: prev-hash mismatch" {
		t.Fatalf("edited legacy line: %+v", c)
	}
	if c := VerifyDecisionChain(join(legacy[0], legacy[1], chained, legacyLine(t, "sneaky"))); c.OK ||
		c.Problem != "line 4: unchained record after the chain began" {
		t.Fatalf("inserted legacy line: %+v", c)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(chained), &env); err != nil {
		t.Fatal(err)
	}
	env["record"].(map[string]any)["decision"] = "rewritten"
	edited, _ := json.Marshal(env)
	if c := VerifyDecisionChain(join(legacy[0], legacy[1], string(edited))); c.OK || c.Problem != "line 3: hash mismatch" {
		t.Fatalf("edited chained record: %+v", c)
	}
	// A torn final line is reported, not fatal, for a plain log.
	if c := VerifyDecisionChain([]byte(chained)); !c.OK || !c.TornTail || c.Checked != 0 {
		t.Fatalf("torn: %+v", c)
	}
	if _, err := ParseDecisionChain(join(legacy[0], "{garbage", chained)); err == nil {
		t.Fatal("mid-file garbage accepted")
	}
	if c := VerifyDecisionChain(join("[1]", chained)); c.OK || !strings.Contains(c.Problem, "unreadable record") {
		t.Fatalf("non-object line: %+v", c)
	}
}

func TestAnchorChainsDecisionsAndRefusesTampering(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir)
	must(anchor.Initialize(ctx, "t", "d", oneItem()))
	// A pre-chain anchor: bare records, as older Python/Go versions wrote them.
	commitDecisions(t, dir, legacyLine(t, "legacy")+"\n")
	if got := must(anchor.ReadDecisions(ctx)); len(got) != 1 || got[0].Decision != "legacy" {
		t.Fatal(got)
	}
	must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID: "c1", ProgressSummary: "- c1", Checklist: oneItem(),
		Decisions: []contracts.DecisionRecord{{Decision: "chained", Rationale: "r"}},
	}))
	check := must(anchor.VerifyDecisions(ctx))
	if !check.OK || check.Checked != 2 || check.Legacy != 1 {
		t.Fatalf("%+v", check)
	}
	snap := must(NewGitMissionAnchor(dir).ReadSituationalAwareness(ctx))
	if len(snap.LastDecisions) != 2 || snap.LastDecisions[1].CycleID != "c1" {
		t.Fatal(snap.LastDecisions)
	}

	// Rewriting the committed history is refused by reads and checkpoints alike.
	lines := strings.Split(strings.TrimSuffix(readText(t, filepath.Join(dir, AnchorDir, DecisionsFile)), "\n"), "\n")
	commitDecisions(t, dir, legacyLine(t, "forged")+"\n"+lines[1]+"\n")
	var chainErr *DecisionChainError
	if _, err := anchor.ReadSituationalAwareness(ctx); !errors.As(err, &chainErr) ||
		!strings.Contains(err.Error(), "prev-hash mismatch") {
		t.Fatalf("read: %v", err)
	}
	head := must(HeadSHA(ctx, dir))
	if _, err := anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c2", Checklist: oneItem()}); !errors.As(err, &chainErr) {
		t.Fatalf("commit: %v", err)
	}
	if must(HeadSHA(ctx, dir)) != head {
		t.Fatal("committed on top of an altered log")
	}
	if _, err := anchor.ReadDecisions(ctx); !errors.As(err, &chainErr) {
		t.Fatalf("read decisions: %v", err)
	}

	// An incomplete final line in the committed anchor counts as tampering too.
	commitDecisions(t, dir, lines[1])
	if c := must(anchor.VerifyDecisions(ctx)); c.OK || !c.TornTail {
		t.Fatalf("%+v", c)
	}
}

func TestPyStringEscapes(t *testing.T) {
	var b strings.Builder
	writePyString(&b, "q\"\\\n\r\t\b\f\x01<&> é")
	if got := b.String(); got != `"q\"\\\n\r\t\b\f\u0001<&>`+" é\"" {
		t.Fatalf("%q", got)
	}
	if _, err := CanonicalJSON(struct{}{}); err == nil {
		t.Fatal("unsupported value accepted")
	}
	if got := must(CanonicalJSON(map[string]any{"b": []any{nil, true, false, 1.5}, "a": "x"})); got != `{"a":"x","b":[null,true,false,1.5]}` {
		t.Fatal(got)
	}
}
