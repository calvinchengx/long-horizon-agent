package org

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/tools"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// Ports of python/tests/unit/test_subagent.py, test_orchestrator.py and the orchestrate halves
// of test_ownership_integration.py and test_leases_and_resume.py.

func localTools(t *testing.T, dir string) (contracts.ToolDispatcher, contracts.ToolContext) {
	t.Helper()
	d, err := execution.ForTools(tools.DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true})
	if err != nil {
		t.Fatal(err)
	}
	session, err := execution.NewLocalSandbox().Open(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	return d, contracts.ToolContext{MissionID: "m1", Session: session}
}

// --- sub-agents and research ----------------------------------------------------------------------

func TestSubAgentReturnsABrief(t *testing.T) {
	t.Parallel()
	d, tctx := localTools(t, t.TempDir())
	result, err := NewSubAgent(agents.Roles["researcher"], stub(), d, 0).Run(context.Background(), "describe the repo layout", tctx, "")
	if err != nil || result.Role != "researcher" || result.Brief == "" {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestSubAgentUsesAToolThenFinishes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello repo"), 0o644)
	d, tctx := localTools(t, dir)
	model := stub(actText("read_file", map[string]any{"path": "README.md"}),
		contracts.TurnResult{Text: `{"done": true, "summary": "the readme says hello repo"}`})
	result, err := NewSubAgent(agents.Roles["researcher"], model, d, 0).Run(context.Background(), "read the readme", tctx, "")
	if err != nil || result.ToolCalls != 1 || !strings.Contains(result.Brief, "hello repo") {
		t.Fatalf("%+v %v", result, err)
	}
	// A read-only role never gets (or dispatches) a mutating tool.
	write := stub(actText("write_file", map[string]any{"path": "x.txt", "content": "x"}), doneTurn)
	if _, err := NewSubAgent(agents.Roles["researcher"], write, d, 0).Run(context.Background(), "q", tctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("a researcher wrote a file")
	}
}

func TestResearchFanoutOneBriefPerQuery(t *testing.T) {
	t.Parallel()
	d, tctx := localTools(t, t.TempDir())
	results, err := ResearchFanout(context.Background(), stub(), d, tctx, []string{"q1", "q2", "q3"}, nil)
	if err != nil || len(results) != 3 {
		t.Fatalf("%v %v", results, err)
	}
	for _, r := range results {
		if r.Role != "researcher" || r.Brief == "" || r.Error != "" {
			t.Fatalf("%+v", r)
		}
	}
	failed, err := ResearchFanout(context.Background(), broken{stub()}, d, tctx, []string{"q"}, nil)
	if err != nil || failed[0].Error != "RuntimeError: model fell over" || failed[0].Brief != "" {
		t.Fatalf("%+v %v", failed, err)
	}
	meter := governor.NewCostMeter(governor.NewCostLedger(), governor.NewBudgetGovernor(0.5, 10, false))
	if _, err := ResearchFanout(context.Background(), meter.Wrap(pricey{}, "researcher"), d, tctx, []string{"q"}, nil); !errors.As(err, new(*governor.BudgetExceeded)) {
		t.Fatalf("a budget refusal must stop the fan-out: %v", err)
	}
	lead := agents.Roles["lead"]
	if _, err := ResearchFanout(context.Background(), stub(), d, tctx, []string{"q"}, &lead); !errors.Is(err, ErrMutatingResearchRole) {
		t.Fatal(err)
	}
}

// --- the orchestrator: serial rounds ----------------------------------------------------------------

func checklist2() *contracts.Checklist {
	return &contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{
		contracts.NewChecklistItem("01", "do thing one"), contracts.NewChecklistItem("02", "do thing two"),
	}}
}

func run(t *testing.T, settingsEnv []string, opts OrchestratorOptions, m MissionOptions) agent.MissionSummary {
	t.Helper()
	summary, err := NewOrchestrator(settingsFor(t, settingsEnv...), opts).RunMission(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

func TestOrchestratorCompletesMissionWithOrg(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	summary := run(t, nil, OrchestratorOptions{ResearchPerItem: 1, DoReview: true, Models: map[string]contracts.ModelProvider{
		"lead": stub(doneTurn), "researcher": stub(doneTurn), "reviewer": stub(approve),
	}}, MissionOptions{Workdir: dir, Title: "Org test", Description: "exercise the org", Checklist: checklist2(),
		Checks: []contracts.Check{pass}})
	if summary.ItemsTotal != 2 || summary.ItemsDone != 2 || !summary.Completed || summary.StoppedReason != "complete" {
		t.Fatalf("%+v", summary)
	}
	if n := strings.Count(git(t, dir, "log", "--format=%s"), "lha: complete"); n != 2 {
		t.Fatalf("%d completes", n)
	}
	if summary.HeadSHA != git(t, dir, "rev-parse", "HEAD") {
		t.Fatal("head")
	}
	runs := committedEvents(t, dir, RunEvent)
	if len(runs) != 1 || runs[0].Payload["mission_id"] != summary.MissionID || runs[0].Payload["resumed"] != false || runs[0].Payload["run"] != float64(1) {
		t.Fatalf("%+v", runs)
	}
	boards := committedEvents(t, dir, BoardEvent)
	if len(boards) != 2 || boards[0].Payload["author"] != "researcher:01" || boards[0].Payload["text"] != "done" {
		t.Fatalf("%+v", boards)
	}
	// Each approval is committed by itself, so the last verdict is not lost with the run.
	log := strings.Split(git(t, dir, "log", "--format=%s"), "\n")
	if log[0] != "lha: review approved 02" || !strings.Contains(strings.Join(log, "\n"), "lha: review approved 01") {
		t.Fatalf("%v", log)
	}
	reviews := committedEvents(t, dir, agents.ReviewEvent)
	if len(reviews) != 2 || reviews[0].CycleID != "c1" || reviews[0].Payload["item_id"] != "01" || reviews[0].Payload["verdict"] != "approve" ||
		reviews[1].CycleID != "c2" || reviews[1].Payload["item_id"] != "02" || reviews[1].Payload["verdict"] != "approve" {
		t.Fatalf("%+v", reviews)
	}
	for _, r := range reviews {
		if r.Payload["base"] == "" || r.Payload["head"] == "" {
			t.Fatalf("%+v", r)
		}
	}
}

// TestRecordsAppendedAfterTheLastCheckpointAreCommittedAtRunEnd is python's
// test_records_appended_after_the_last_checkpoint_are_committed_at_run_end: a failed attempt's
// reflection is appended after its checkpoint; a run that then ends (the cycle limit) commits it.
func TestRecordsAppendedAfterTheLastCheckpointAreCommittedAtRunEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	red := contracts.Check{Name: "always_red", Command: []string{"false"}, Gating: true, Where: "sandbox"}
	summary := run(t, []string{"LHA_MAX_CYCLES=1"}, OrchestratorOptions{ResearchPerItem: 0, DoReview: false, Models: map[string]contracts.ModelProvider{
		"lead": stub(doneTurn), "researcher": stub(doneTurn), "reviewer": stub(approve),
	}}, MissionOptions{Workdir: dir, Title: "Flush test", Description: "one failing attempt", Checklist: checklist2(),
		Checks: []contracts.Check{red}})
	if summary.Completed || summary.Cycles != 1 {
		t.Fatalf("%+v", summary)
	}
	if log := strings.Split(git(t, dir, "log", "--format=%s"), "\n"); log[0] != "lha: anchor records at run end" {
		t.Fatalf("%v", log)
	}
	reflections := committedEvents(t, dir, ReflectionEvent)
	if len(reflections) != 1 || reflections[0].Payload["item"] != "01" {
		t.Fatalf("%+v", reflections)
	}
	if summary.HeadSHA != git(t, dir, "rev-parse", "HEAD") {
		t.Fatal("head")
	}
	if status := git(t, dir, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("dirty: %q", status)
	}
}

func TestBlockingReviewReopensTheItem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	block := contracts.TurnResult{Text: `{"done": true, "verdict": "block", "blocking_issues": ["no error handling"]}`}
	summary := run(t, nil, OrchestratorOptions{ResearchPerItem: 0, DoReview: true, Models: map[string]contracts.ModelProvider{
		"lead": stub(doneTurn), "researcher": stub(doneTurn), "reviewer": stub(block, approve),
	}}, MissionOptions{Workdir: dir, Title: "Review test", Description: "review gates items", Checklist: checklist2(),
		Checks: []contracts.Check{pass}})
	if !summary.Completed || summary.Cycles != 3 { // 01, 01 again after the block, 02
		t.Fatalf("%+v", summary)
	}
	if n := len(kinds(traceOf(t, summary), "review_reopened")); n != 1 {
		t.Fatalf("%d reopened", n)
	}
	if !strings.Contains(git(t, dir, "log", "--format=%s"), "lha: review reopened 01") {
		t.Fatal("reopen commit")
	}
	reflections := committedEvents(t, dir, ReflectionEvent)
	if len(reflections) != 1 || reflections[0].Payload["text"] != "\nReview verdict: block\n- BLOCKING: no error handling\n" {
		t.Fatalf("%+v", reflections)
	}
	// The second review of 01 diffs from the same base as the first (the item's first attempt).
	reviews := []anchorEvent{}
	for _, e := range committedEvents(t, dir, agents.ReviewEvent) {
		if e.Payload["item_id"] == "01" {
			reviews = append(reviews, e)
		}
	}
	if len(reviews) != 2 || reviews[0].Payload["verdict"] != "block" || reviews[1].Payload["verdict"] != "approve" ||
		reviews[0].Payload["base"] != reviews[1].Payload["base"] || reviews[0].Payload["head"] == reviews[1].Payload["head"] ||
		reviews[0].Payload["tool_calls"] != float64(0) || !strings.Contains(reviews[0].Payload["brief"].(string), "verdict") {
		t.Fatalf("%+v", reviews)
	}
}

func TestReviewBaseTracksTheFirstBlockingReviewUntilAnApproval(t *testing.T) {
	review := func(item string, blocking bool, base, cycle string) contracts.EventRecord {
		return contracts.EventRecord{Kind: "review", CycleID: cycle, Payload: contracts.Payload("item_id", item, "blocking", blocking, "base", base)}
	}
	events := []contracts.EventRecord{}
	if ReviewBase(events, "01", "fb") != "fb" {
		t.Fatal("empty")
	}
	events = append(events, review("01", true, "b1", "c1"))
	if ReviewBase(events, "01", "fb") != "b1" {
		t.Fatal("first")
	}
	events = append(events, review("01", true, "b2", "c2"))
	if ReviewBase(events, "01", "fb") != "b1" {
		t.Fatal("still first")
	}
	events = append(events, review("01", false, "b1", "c3"), review("02", true, "x", "c4"))
	if ReviewBase(events, "01", "fb") != "fb" || ReviewBase(events, "02", "fb") != "x" {
		t.Fatal("after approval")
	}
}

// pricey costs $1 per call (worst case == actual) and always says done / approve.
type pricey struct{}

func (pricey) Name() string { return "fake:pricey" }
func (pricey) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	return contracts.TurnResult{Text: `{"done": true, "verdict": "approve", "blocking_issues": []}`,
		Usage: contracts.Usage{InputTokens: 1, OutputTokens: 1, Model: "pricey"}}, nil
}
func (pricey) EstimateCostUSD(contracts.Usage) (float64, error) { return 1.0, nil }

func TestEveryRoleIsMeteredAndBudgetHardStops(t *testing.T) {
	t.Parallel()
	summary := run(t, []string{"LHA_BUDGET_USD_CEILING=2.5"}, OrchestratorOptions{ResearchPerItem: 1, DoReview: true,
		Models: map[string]contracts.ModelProvider{"lead": pricey{}, "researcher": pricey{}, "reviewer": pricey{}}},
		MissionOptions{Workdir: t.TempDir(), Title: "Budget test", Description: "spend", Checklist: checklist2(), Checks: []contracts.Check{pass}})
	// research ($1) + lead ($1) = $2 recorded; the reviewer's call ($2 + $1 worst case > $2.5) is
	// refused BEFORE it runs, so spend never exceeds the ceiling.
	if !strings.HasPrefix(summary.StoppedReason, "governor:") || summary.TotalUSD != 2.0 {
		t.Fatalf("%+v", summary)
	}
}

// --- worktrees and the integrator ----------------------------------------------------------------

func TestWorktreeBranchChangesAndIntegrator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	anchor := state.NewGitMissionAnchor(dir)
	base, err := anchor.Initialize(ctx, "T", "D", contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{contracts.NewChecklistItem("01", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	path, err := AddWorktree(ctx, dir, "lha/implementer-01/c1", base)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := WorktreeRoot(ctx, dir)
	if !strings.HasPrefix(path, root) {
		t.Fatalf("%s not under %s", path, root)
	}
	_ = os.WriteFile(filepath.Join(path, "a.py"), []byte("print('a')\n"), 0o644)
	_ = os.WriteFile(filepath.Join(path, ".lha", "checklist.json"), []byte("{}"), 0o644) // never committed
	head, err := CommitWorktree(ctx, path, "lha: implementer-01 01")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := coordination.ChangedPaths(ctx, path, base, head); !reflect.DeepEqual(got, []string{"a.py"}) {
		t.Fatalf("%v", got)
	}
	if got, _ := coordination.ChangedPaths(ctx, path, base, base); len(got) != 0 {
		t.Fatal(got)
	}
	session, _ := execution.NewLocalSandbox().Open(ctx, dir, "")
	noA := contracts.Check{Name: "no_a", Command: []string{"sh", "-c", "test ! -e a.py"}, Gating: true, Where: "sandbox"}
	integrator := &BranchIntegrator{Workdir: dir, Session: session, Verifier: verify.NewDeterministicVerifier(), Checks: []contracts.Check{noA}}
	refused, _ := integrator.Integrate(ctx, "lha/implementer-01/c1", head, base, false, nil, nil)
	if refused.Merged || !strings.Contains(refused.Reason, "failed verification") {
		t.Fatalf("%+v", refused)
	}
	violation := coordination.NewFileOwnershipMap().Violations("implementer-01", []string{"a.py"})
	refused, _ = integrator.Integrate(ctx, "lha/implementer-01/c1", head, base, true, violation, nil)
	if refused.Merged || refused.Reason != "ownership violation: the branch changed files it does not own: a.py (owner: lead)" {
		t.Fatalf("%+v", refused)
	}
	// Verified in the worktree, but red once merged: the merge is aborted, nothing changes.
	red, err := integrator.Integrate(ctx, "lha/implementer-01/c1", head, base, true, nil, nil)
	if err != nil || red.Merged || !strings.Contains(red.Reason, "merged mission branch") {
		t.Fatalf("%+v %v", red, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.py")); !os.IsNotExist(err) {
		t.Fatal("a.py merged")
	}
	if git(t, dir, "rev-parse", "HEAD") != base {
		t.Fatal("HEAD moved")
	}
	if err := PruneWorktrees(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) || len(lhaBranches(t, dir)) != 0 {
		t.Fatal("worktree or branch left behind")
	}
}

func TestParallelBatch(t *testing.T) {
	t.Parallel()
	owners := coordination.NewFileOwnershipMap()
	for _, n := range []string{"01", "02", "03"} {
		_ = owners.Assign(n+".py", coordination.WriterForItem(n))
	}
	items := contracts.Checklist{Items: []contracts.ChecklistItem{
		contracts.NewChecklistItem("01", "a"), contracts.NewChecklistItem("02", "b"),
		contracts.NewChecklistItem("03", "c", "01"), contracts.NewChecklistItem("04", "d"),
	}}
	ids := func(batch []contracts.ChecklistItem) []string {
		out := []string{}
		for _, i := range batch {
			out = append(out, i.ID)
		}
		return out
	}
	if got := ids(ParallelBatch(items, owners, 3)); !reflect.DeepEqual(got, []string{"01", "02"}) {
		t.Fatal(got)
	}
	if len(ParallelBatch(items, owners, 1)) != 0 {
		t.Fatal("limit 1")
	}
	items.Items[1].Status = contracts.StatusBlocked
	if len(ParallelBatch(items, owners, 3)) != 0 {
		t.Fatal("one candidate is not a wave")
	}
}

// --- the orchestrator: parallel waves ---------------------------------------------------------------

func TestParallelWaveMergesVerifiedBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	checklist, owners := twoItems()
	wroteCode := contracts.Check{Name: "wrote_code", Command: []string{"sh", "-c", "ls *.py"}, Gating: true, Where: "sandbox"}
	summary := run(t, nil, OrchestratorOptions{ResearchPerItem: 1, DoReview: true, Models: map[string]contracts.ModelProvider{
		"implementer": newImplementers(nil), "lead": stub(doneTurn), "researcher": stub(doneTurn), "reviewer": stub(approve),
	}}, MissionOptions{Workdir: dir, Title: "Parallel", Description: "two disjoint items", Checklist: &checklist,
		Checks: []contracts.Check{pass, wroteCode}, Ownership: owners})
	if !summary.Completed || summary.StoppedReason != "complete" || summary.Cycles != 2 {
		t.Fatalf("%+v", summary)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "a.py")); string(data) != "x\n" {
		t.Fatalf("a.py %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.py")); err != nil {
		t.Fatal(err)
	}
	merges := []string{}
	for _, line := range strings.Split(git(t, dir, "log", "--format=%s"), "\n") {
		if strings.Contains(line, "[merged lha/implementer-") {
			merges = append(merges, line)
		}
	}
	sort.Strings(merges)
	if !reflect.DeepEqual(merges, []string{"lha: complete 01 (write a) [merged lha/implementer-01/c1]",
		"lha: complete 02 (write b) [merged lha/implementer-02/c2]"}) {
		t.Fatalf("%q", merges)
	}
	// The checkpoint IS the merge commit (the approved review's anchor commit follows it).
	if parents := strings.Fields(git(t, dir, "log", "-1", "--pretty=%P", "--grep=lha: complete 02")); len(parents) != 2 {
		t.Fatal("the checkpoint IS the merge commit")
	}
	if log := strings.Split(git(t, dir, "log", "--format=%s"), "\n"); log[0] != "lha: review approved 02" {
		t.Fatalf("%v", log)
	}
	tickets := committedEvents(t, dir, "ticket")
	if len(tickets) != 2 || tickets[0].Payload["status"] != "done" || tickets[1].Payload["status"] != "done" {
		t.Fatalf("%+v", tickets)
	}
	history := []string{}
	for _, h := range tickets[0].Payload["history"].([]any) {
		history = append(history, h.(map[string]any)["status"].(string))
	}
	if !reflect.DeepEqual(history, []string{"created", "in_progress", "awaiting_verify", "awaiting_merge", "done"}) {
		t.Fatal(history)
	}
	if !reflect.DeepEqual(tickets[0].Payload["write_set"], []any{"a.py"}) || tickets[0].Payload["ticket_id"] != "c1-01" {
		t.Fatalf("%+v", tickets[0].Payload)
	}
	cycles := committedEvents(t, dir, "cycle")
	if len(cycles) != 2 || cycles[0].Payload["writer"] != "implementer-01" || cycles[0].Payload["verified"] != true || cycles[0].Payload["verdict"] != "passed" {
		t.Fatalf("%+v", cycles)
	}
	anchor := state.NewGitMissionAnchor(dir)
	decisions, err := anchor.ReadDecisions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, d := range decisions {
		names = append(names, d.Decision)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"item 01 layout", "item 02 layout"}) {
		t.Fatal(names)
	}
	if check, _ := anchor.VerifyDecisions(ctx); !check.OK {
		t.Fatal(check)
	}
	// Finished items released their files (committed with the later checkpoints).
	if got, _ := coordination.ReadOwnership(ctx, anchor); len(got.Snapshot()) != 0 {
		t.Fatal(got.Snapshot())
	}
	root, _ := WorktreeRoot(ctx, dir)
	if entries, _ := os.ReadDir(root); len(entries) != 0 || len(lhaBranches(t, dir)) != 0 {
		t.Fatal("worktrees / branches left behind")
	}
	trace := traceOf(t, summary)
	if len(kinds(trace, "parallel_wave")) != 1 || len(kinds(trace, "integration")) != 2 {
		t.Fatal("trace")
	}
}

func TestForeignWritesAreRefusedByTheGuardAndTheGitLayer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	checklist, owners := twoItems()
	impl := newImplementers(map[string][]contracts.TurnResult{"01": {
		actText("write_file", map[string]any{"path": "b.py", "content": "mine now"}),
		actText("run_command", map[string]any{"argv": []any{"sh", "-c", "printf shell > b.py"}}),
	}})
	summary := run(t, []string{"LHA_MAX_CYCLES=2"}, OrchestratorOptions{DoReview: false, Models: map[string]contracts.ModelProvider{
		"implementer": impl, "lead": stub(doneTurn)}},
		MissionOptions{Workdir: dir, Title: "Guard", Description: "d", Checklist: &checklist, Checks: []contracts.Check{pass}, Ownership: owners})
	if summary.Cycles != 2 {
		t.Fatalf("%+v", summary)
	}
	refused := ""
	for _, o := range impl.seen() {
		if strings.Contains(o, "ownership:") {
			refused = o
			break
		}
	}
	if !strings.Contains(refused, "owned by 'implementer-02'") {
		t.Fatalf("%q", refused)
	}
	final, _ := state.NewGitMissionAnchor(dir).ReadChecklist(ctx)
	one, two := final.Items[0], final.Items[1]
	if one.Status != "in_progress" || !strings.Contains(one.LastFailure, "ownership violation") || !strings.Contains(one.LastFailure, "b.py") || two.Status != "done" {
		t.Fatalf("%+v %+v", one, two)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "b.py")); string(data) != "x\n" {
		t.Fatalf("b.py %q", data) // item 02's content, not the shell write
	}
	if _, err := os.Stat(filepath.Join(dir, "a.py")); !os.IsNotExist(err) {
		t.Fatal("item 01 was merged")
	}
	tickets := committedEvents(t, dir, "ticket")
	if tickets[0].Payload["status"] != "failed" || !reflect.DeepEqual(tickets[0].Payload["ownership_violations"], []any{"b.py"}) {
		t.Fatalf("%+v", tickets[0].Payload)
	}
	// Unmerged work's decisions are dropped with it.
	decisions, _ := state.NewGitMissionAnchor(dir).ReadDecisions(ctx)
	if len(decisions) != 1 || decisions[0].Decision != "item 02 layout" {
		t.Fatalf("%+v", decisions)
	}
}

func TestIntegrationReverifiesTheMergedBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	checklist, owners := twoItems()
	notBoth := contracts.Check{Name: "not_both", Command: []string{"sh", "-c", "! { test -e a.py && test -e b.py; }"}, Gating: true, Where: "sandbox"}
	summary := run(t, []string{"LHA_MAX_CYCLES=2"}, OrchestratorOptions{Models: map[string]contracts.ModelProvider{
		"implementer": newImplementers(nil), "lead": stub(doneTurn)}},
		MissionOptions{Workdir: dir, Title: "Merge gate", Description: "d", Checklist: &checklist, Checks: []contracts.Check{notBoth}, Ownership: owners})
	if summary.StoppedReason != "max_cycles" {
		t.Fatalf("%+v", summary)
	}
	final, _ := state.NewGitMissionAnchor(dir).ReadChecklist(ctx)
	if final.Items[0].Status != "done" || final.Items[1].Status != "in_progress" ||
		!strings.Contains(final.Items[1].LastFailure, "verification failed on the merged mission branch") {
		t.Fatalf("%+v", final.Items)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.py")); !os.IsNotExist(err) {
		t.Fatal("b.py merged")
	}
	gitDir, _ := state.GitDir(ctx, dir)
	if _, err := os.Stat(filepath.Join(gitDir, "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Fatal("a half-finished merge was left")
	}
}

func TestAFailingImplementerIsAFailedAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	checklist, owners := twoItems()
	summary := run(t, []string{"LHA_MAX_CYCLES=2"}, OrchestratorOptions{Models: map[string]contracts.ModelProvider{
		"implementer": broken{stub()}, "lead": stub(doneTurn)}},
		MissionOptions{Workdir: dir, Title: "Broken", Description: "d", Checklist: &checklist, Checks: []contracts.Check{pass}, Ownership: owners})
	if summary.Cycles != 2 || summary.Completed {
		t.Fatalf("%+v", summary)
	}
	final, _ := state.NewGitMissionAnchor(dir).ReadChecklist(ctx)
	for _, item := range final.Items {
		if !strings.Contains(item.LastFailure, "implementer failed: RuntimeError") {
			t.Fatalf("%+v", item)
		}
	}
	if len(lhaBranches(t, dir)) != 0 {
		t.Fatal("branches left")
	}
}

func TestSerialLeadIsGuardedAuditedAndReleasesLeases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	checklist, owners := twoItems()
	lead := newScripted(
		actText("write_file", map[string]any{"path": "b.py", "content": "lead"}), // refused: 02's leased file
		actText("run_command", map[string]any{"argv": []any{"sh", "-c", "printf shell > b.py"}}),
		doneTurn,
	)
	summary := run(t, []string{"LHA_MAX_CYCLES=2"}, OrchestratorOptions{MaxParallel: intp(1), // the Lead works both items serially
		Models: map[string]contracts.ModelProvider{"lead": lead}},
		MissionOptions{Workdir: dir, Title: "Serial", Description: "d", Checklist: &checklist, Checks: []contracts.Check{pass}, Ownership: owners})
	if !summary.Completed {
		t.Fatalf("%+v", summary)
	}
	if !lead.saw("may not write 'b.py'", "implementer-02") {
		t.Fatal("the guard's refusal never reached the Lead")
	}
	trace := traceOf(t, summary)
	audit := kinds(trace, "ownership_violation")
	if len(audit) != 1 || !reflect.DeepEqual(audit[0].Data["paths"], []any{"b.py"}) || audit[0].Data["writer"] != "lead+implementer-01" {
		t.Fatalf("%+v", audit)
	}
	released := kinds(trace, "ownership_released")
	if len(released) != 1 || !reflect.DeepEqual(released[0].Data["paths"], []any{"a.py"}) {
		t.Fatalf("%+v", released)
	}
	// 01's lease ended with the next checkpoint; 02's would end with the one after it.
	if got, _ := coordination.ReadOwnership(ctx, state.NewGitMissionAnchor(dir)); !reflect.DeepEqual(got.Snapshot(), map[string]string{"b.py": "implementer-02"}) {
		t.Fatal(got.Snapshot())
	}
}

func TestParallelItemsAreReviewedAndCanBeReopened(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	checklist, owners := twoItems()
	block := contracts.TurnResult{Text: `{"done": true, "verdict": "block", "blocking_issues": ["no docstring"]}`}
	summary := run(t, []string{"LHA_MAX_CYCLES=3"}, OrchestratorOptions{DoReview: true, Models: map[string]contracts.ModelProvider{
		"implementer": newImplementers(nil), "lead": stub(doneTurn), "reviewer": stub(block, approve)}},
		MissionOptions{Workdir: dir, Title: "Review", Description: "d", Checklist: &checklist, Checks: []contracts.Check{pass}, Ownership: owners})
	reopened := kinds(traceOf(t, summary), "review_reopened")
	if len(reopened) != 1 || reopened[0].Data["item"] != "01" {
		t.Fatalf("%+v", reopened)
	}
	if !strings.Contains(git(t, dir, "log", "--format=%s"), "lha: review reopened 01") {
		t.Fatal("reopen commit")
	}
	// The reopened item's lease was released when it merged, so the Lead redoes it serially.
	if !summary.Completed || summary.Cycles != 3 {
		t.Fatalf("%+v", summary)
	}
}

func TestOrchestratorStopsOnATamperedDecisionChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	checklist, owners := twoItems()
	impl := newImplementers(nil)
	impl.before = func() {
		bad := `{"prev": "` + strings.Repeat("0", 64) + `", "hash": "` + strings.Repeat("f", 64) + `", "record": {"decision": "x", "rationale": "y"}}` + "\n"
		_ = os.WriteFile(filepath.Join(dir, ".lha", "decisions.ndjson"), []byte(bad), 0o644)
		git(t, dir, "add", "-f", ".lha/decisions.ndjson")
		git(t, dir, "commit", "-q", "-m", "forge")
	}
	summary := run(t, nil, OrchestratorOptions{Models: map[string]contracts.ModelProvider{"implementer": impl, "lead": stub(doneTurn)}},
		MissionOptions{Workdir: dir, Title: "Tamper", Description: "d", Checklist: &checklist, Checks: []contracts.Check{pass}, Ownership: owners})
	if !strings.HasPrefix(summary.StoppedReason, "decision log failed verification") || summary.Completed ||
		!strings.Contains(summary.TraceJSONL, "decision_chain_invalid") {
		t.Fatalf("%+v", summary)
	}
	gitDir, _ := state.GitDir(ctx, dir)
	if _, err := os.Stat(filepath.Join(gitDir, "MERGE_HEAD")); !os.IsNotExist(err) || len(lhaBranches(t, dir)) != 0 {
		t.Fatal("merge / branches left behind")
	}
}

func TestParallelItemsAreGatedByWitnessesAndSplitWhenBlocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	checklist, owners := twoItems()
	for i := range checklist.Items {
		checklist.Items[i].Witnesses = []string{"cmd:test -f never.txt", "go:"} // a failing and an invalid witness
	}
	split := contracts.TurnResult{Text: `[{"description": "first half"}, {"description": "second half"}]`}
	summary := run(t, []string{"LHA_MAX_CYCLES=6"}, OrchestratorOptions{Models: map[string]contracts.ModelProvider{
		"implementer": newImplementers(nil), "lead": stub(split)}},
		MissionOptions{Workdir: dir, Title: "Witnessed", Description: "d", Checklist: &checklist, Checks: []contracts.Check{pass}, Ownership: owners})
	if summary.Cycles != 6 || summary.Completed {
		t.Fatalf("%+v", summary)
	}
	final, _ := state.NewGitMissionAnchor(dir).ReadChecklist(ctx)
	byID := map[string]contracts.ChecklistItem{}
	for _, i := range final.Items {
		byID[i.ID] = i
	}
	if byID["01"].Status != "split" || byID["02"].Status != "split" {
		t.Fatalf("%+v", final.Items)
	}
	for _, id := range []string{"01.1", "01.2", "02.1", "02.2"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("no %s", id)
		}
	}
	if !strings.Contains(byID["01"].LastFailure, "cmd:test -f never.txt") || !strings.Contains(byID["01"].LastFailure, "invalid witness") {
		t.Fatal(byID["01"].LastFailure)
	}
	cycles := committedEvents(t, dir, "cycle")
	last := cycles[len(cycles)-1].Payload["split_into"]
	if !reflect.DeepEqual(last, []any{"01.1", "01.2"}) && !reflect.DeepEqual(last, []any{"02.1", "02.2"}) {
		t.Fatal(last)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.py")); !os.IsNotExist(err) {
		t.Fatal("never merged")
	}
}

// --- leases inside a wave, and resume -------------------------------------------------------------

func TestImplementerIsGrantedALeaseMidWave(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	checklist, owners := twoItems()
	impl := newImplementers(map[string][]contracts.TurnResult{"01": {
		actText("request_lease", map[string]any{"path": "shared_util.py", "reason": "helper"}),
		actText("write_file", map[string]any{"path": "shared_util.py", "content": "u\n"}),
		actText("request_lease", map[string]any{"path": "b.py", "reason": "steal"}),
	}})
	summary := run(t, nil, OrchestratorOptions{Models: map[string]contracts.ModelProvider{"implementer": impl, "lead": stub(doneTurn)}},
		MissionOptions{Workdir: dir, Title: "Lease", Description: "d", Checklist: &checklist, Checks: []contracts.Check{pass}, Ownership: owners})
	if !summary.Completed {
		t.Fatalf("%+v", summary)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "shared_util.py")); string(data) != "u\n" {
		t.Fatalf("%q", data) // leased, written, merged
	}
	granted, refused := false, false
	for _, o := range impl.seen() {
		granted = granted || strings.Contains(o, "lease granted")
		refused = refused || (strings.Contains(o, "lease refused") && strings.Contains(o, "implementer-02"))
	}
	if !granted || !refused {
		t.Fatal(impl.seen())
	}
	leases := committedEvents(t, dir, coordination.LeaseEvent)
	if len(leases) != 2 || leases[0].Payload["path"] != "shared_util.py" || leases[0].Payload["granted"] != true ||
		leases[1].Payload["path"] != "b.py" || leases[1].Payload["granted"] != false {
		t.Fatalf("%+v", leases)
	}
	for _, ticket := range committedEvents(t, dir, "ticket") {
		if ticket.Payload["item_id"] == "01" {
			if l := ticket.Payload["leases"].([]any); len(l) != 2 || l[0].(map[string]any)["path"] != "shared_util.py" {
				t.Fatalf("%+v", ticket.Payload)
			}
		}
	}
	traced := kinds(traceOf(t, summary), "lease")
	if len(traced) != 2 || traced[0].Data["granted"] != true || traced[1].Data["granted"] != false {
		t.Fatalf("%+v", traced)
	}
	if got, _ := coordination.ReadOwnership(ctx, state.NewGitMissionAnchor(dir)); len(got.Snapshot()) != 0 {
		t.Fatal("not all released", got.Snapshot())
	}
}

// leadRecordingDecision records a decision, then says done; it keeps every prompt it sees.
func leadRecordingDecision() *scripted {
	s := newScripted()
	s.fn = func(messages []contracts.ModelMessage) contracts.TurnResult {
		for _, m := range messages {
			if m.Role == "assistant" {
				return contracts.TurnResult{Text: `{"done": true, "summary": "did it"}`}
			}
		}
		s.mu.Lock()
		n := len(s.seen)
		s.mu.Unlock()
		return actText("record_decision", map[string]any{"decision": "d" + string(rune('0'+n%10)), "rationale": "r"})
	}
	return s
}

func serialOrg(lead *scripted) OrchestratorOptions {
	return OrchestratorOptions{ResearchPerItem: 1, DoReview: true, Models: map[string]contracts.ModelProvider{
		"lead": lead, "researcher": stub(contracts.TurnResult{Text: `{"done": true, "summary": "B1"}`}), "reviewer": stub(approve),
	}}
}

func TestOrchestrateResumesTheSameMission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	items := &contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{
		contracts.NewChecklistItem("01", "first"), contracts.NewChecklistItem("02", "second")}}
	first := run(t, []string{"LHA_MAX_CYCLES=1"}, serialOrg(leadRecordingDecision()),
		MissionOptions{Workdir: dir, Title: "Resume", Description: "two steps", Checklist: items, Checks: []contracts.Check{pass}})
	if first.Completed || first.Cycles != 1 || first.ItemsDone != 1 || !AnchorExists(ctx, dir) {
		t.Fatalf("%+v", first)
	}
	_ = os.WriteFile(filepath.Join(dir, "residue.txt"), []byte("left by a crash\n"), 0o644) // uncommitted, discarded
	before, _ := state.NewGitMissionAnchor(dir).ReadDecisions(ctx)

	lead := leadRecordingDecision()
	second := run(t, nil, serialOrg(lead), MissionOptions{Workdir: dir, Checks: []contracts.Check{pass}, Resume: true})
	if !second.Completed || second.Cycles != 1 || second.ItemsDone != 2 || second.MissionID != first.MissionID {
		t.Fatalf("%+v", second)
	}
	if _, err := os.Stat(filepath.Join(dir, "residue.txt")); !os.IsNotExist(err) {
		t.Fatal("residue kept")
	}
	anchor := state.NewGitMissionAnchor(dir)
	decisions, _ := anchor.ReadDecisions(ctx)
	if len(decisions) != 2 || !reflect.DeepEqual(decisions[:len(before)], before) {
		t.Fatalf("%+v", decisions)
	}
	if check, _ := anchor.VerifyDecisions(ctx); !check.OK {
		t.Fatal(check)
	}
	cycleIDs := []string{}
	for _, e := range committedEvents(t, dir, "cycle") {
		cycleIDs = append(cycleIDs, e.CycleID)
	}
	if !reflect.DeepEqual(cycleIDs, []string{"c1", "c2"}) { // numbering continues; nothing re-done
		t.Fatal(cycleIDs)
	}
	runs := committedEvents(t, dir, RunEvent)
	if len(runs) != 2 || runs[0].Payload["run"] != float64(1) || runs[0].Payload["resumed"] != false ||
		runs[1].Payload["run"] != float64(2) || runs[1].Payload["resumed"] != true {
		t.Fatalf("%+v", runs)
	}
	// The blackboard of the first run (its research brief) reached the resumed Lead.
	if !lead.saw("Team board (earlier rounds)", "[researcher:01] B1") {
		t.Fatal("board not restored")
	}
	resumed := kinds(traceOf(t, second), "resumed")
	if len(resumed) != 1 || resumed[0].Data["cycle_offset"] != float64(1) {
		t.Fatalf("%+v", resumed)
	}
	if mission, _ := anchor.ReadMission(ctx); mission == nil || mission.Title != "Resume" {
		t.Fatal(mission)
	}
	var resumeErr *MissionResumeError
	if _, err := NewOrchestrator(settingsFor(t), serialOrg(leadRecordingDecision())).RunMission(ctx,
		MissionOptions{Workdir: filepath.Join(dir, "nowhere"), Resume: true}); !errors.As(err, &resumeErr) {
		t.Fatal(err)
	}
	if _, err := NewOrchestrator(settingsFor(t), serialOrg(leadRecordingDecision())).RunMission(ctx,
		MissionOptions{Workdir: filepath.Join(dir, "x")}); err == nil || !strings.Contains(err.Error(), "checklist") {
		t.Fatal(err)
	}
}

func TestResumeRestoresReflectionsForOpenItems(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	checklist := contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{contracts.NewChecklistItem("01", "first")}}
	anchor := state.NewGitMissionAnchor(dir)
	if _, err := anchor.Initialize(ctx, "T", "D", checklist); err != nil {
		t.Fatal(err)
	}
	_ = anchor.AppendEvent(ctx, contracts.EventRecord{Kind: "reflection", CycleID: "c7", Payload: contracts.Payload("item", "01", "text", "REFLECT-01")})
	_ = anchor.AppendEvent(ctx, contracts.EventRecord{Kind: "reflection", Payload: contracts.Payload("item", "zz", "text", "no")})
	if _, err := anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c7", Checklist: checklist}); err != nil {
		t.Fatal(err)
	}
	lead := leadRecordingDecision()
	summary := run(t, []string{"LHA_MAX_CYCLES=1"}, serialOrg(lead), MissionOptions{Workdir: dir, Checks: []contracts.Check{pass}, Resume: true})
	if !summary.Completed || !lead.saw("REFLECT-01") {
		t.Fatalf("%+v", summary)
	}
	cycles := committedEvents(t, dir, "cycle")
	if len(cycles) != 1 || cycles[0].CycleID != "c8" {
		t.Fatalf("%+v", cycles)
	}
}

func TestReviewParsingAndReopen(t *testing.T) {
	t.Parallel()
	review := agents.ReviewResult{Verdict: "block", Blocking: true, BlockingIssues: []string{"a", "b"}, Advisory: []string{"c"}}
	checklist := contracts.Checklist{Items: []contracts.ChecklistItem{{ID: "01", Status: "done", VerifiedBy: []string{"x"}}}}
	item := ReopenForReview(&checklist, "01", review, false)
	if item == nil || item.Status != "todo" || len(item.VerifiedBy) != 0 || item.LastFailure != "reviewer blocked: a; b" ||
		item.Notes != "Review verdict: block\n- BLOCKING: a\n- BLOCKING: b\n- advisory: c" {
		t.Fatalf("%+v", item)
	}
	if ReopenForReview(&checklist, "zz", review, true) != nil {
		t.Fatal("gone item")
	}
	unparsed := ReopenForReview(&checklist, "01", agents.ReviewResult{Verdict: "unparsed"}, true)
	if unparsed.Status != "blocked" || unparsed.LastFailure != "reviewer blocked: unparsed" {
		t.Fatalf("%+v", unparsed)
	}
	if DiffSince(context.Background(), ".", "a", "a") != "(no new commits)" {
		t.Fatal("diff")
	}
}

// TestFindingsForceTheReviewWhenReviewIsOff is python's
// test_findings_force_the_review_when_review_is_off (which stubs the diff): item 01 adds a skipped test, item 02 is
// clean; with DoReview off only the flagged item is reviewed, and both screens are committed.
func TestFindingsForceTheReviewWhenReviewIsOff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A Go test file: a test path for the screen, and not a harness file the lead may not write.
	skipped := actText("write_file", map[string]any{"path": "pkg/x_test.go",
		"content": "package pkg\n\nfunc TestX(t *testing.T) {\n\tt.Skip(\"later\")\n}\n"})
	summary := run(t, nil, OrchestratorOptions{ResearchPerItem: 0, DoReview: false, Models: map[string]contracts.ModelProvider{
		"lead": stub(skipped, doneTurn, doneTurn), "researcher": stub(doneTurn), "reviewer": stub(approve),
	}}, MissionOptions{Workdir: dir, Title: "Screen test", Description: "review is off", Checklist: checklist2(),
		Checks: []contracts.Check{pass}})
	if !summary.Completed {
		t.Fatalf("%+v", summary)
	}
	screens := kinds(traceOf(t, summary), "review_screen")
	if len(screens) != 2 || screens[0].Data["item"] != "01" || screens[0].Data["findings"] != float64(1) || screens[0].Data["forced"] != true ||
		screens[1].Data["item"] != "02" || screens[1].Data["findings"] != float64(0) || screens[1].Data["forced"] != false {
		t.Fatalf("%+v", screens)
	}
	if n := len(kinds(traceOf(t, summary), "review")); n != 1 { // only the flagged item was reviewed
		t.Fatalf("%d reviews", n)
	}
	committed := committedEvents(t, dir, "review_screen")
	if len(committed) != 2 || committed[0].Payload["forced"] != true || committed[1].Payload["forced"] != false ||
		len(committed[0].Payload["findings"].([]any)) != 1 || committed[0].Payload["findings"].([]any)[0] != "added skip to pkg/x_test.go: t.Skip(\"later\")" {
		t.Fatalf("%+v", committed)
	}
	reviews := committedEvents(t, dir, agents.ReviewEvent)
	if len(reviews) != 1 || reviews[0].Payload["item_id"] != "01" || reviews[0].Payload["verdict"] != "approve" {
		t.Fatalf("%+v", reviews)
	}
}

// TestDiffSinceReturnsThePatchDespiteHardening is python's
// test_diff_since_returns_the_patch_despite_hardening: the hardening config's empty diff.external
// must not empty the reviewer's diff.
func TestDiffSinceReturnsThePatchDespiteHardening(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "x_test.go"), []byte("package pkg\n\nfunc TestX(t *testing.T) {\n\tt.Skip(\"later\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".lha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".lha", "events.ndjson"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "c1")
	diff := DiffSince(context.Background(), dir, git(t, dir, "rev-parse", "HEAD~1"), git(t, dir, "rev-parse", "HEAD"))
	if !strings.HasPrefix(diff, "diff --git a/pkg/x_test.go b/pkg/x_test.go") || !strings.Contains(diff, "+\tt.Skip(\"later\")") || strings.Contains(diff, ".lha") {
		t.Fatalf("%q", diff)
	}
	if got := verify.ScreenDiff(diff); !reflect.DeepEqual(got, []string{"added skip to pkg/x_test.go: t.Skip(\"later\")"}) {
		t.Fatalf("%q", got)
	}
}

// TestSubAgentGetsOneFinalTurnWhenItsBudgetRunsOut is python's
// test_subagent_gets_one_final_turn_when_its_budget_runs_out.
func TestSubAgentGetsOneFinalTurnWhenItsBudgetRunsOut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello repo"), 0o644)
	d, tctx := localTools(t, dir)
	read := actText("read_file", map[string]any{"path": "README.md"})
	var seen []string
	recording := &lastPromptRecorder{inner: stub(read, read, contracts.TurnResult{Text: `{"done": true, "summary": "verdict: it reads hello"}`}), seen: &seen}
	result, err := NewSubAgent(agents.Roles["reviewer"], recording, d, 2).Run(context.Background(), "review it", tctx, "")
	if err != nil || result.ToolCalls != 2 || result.Turns != 3 || result.Brief != "verdict: it reads hello" {
		t.Fatalf("%+v %v", result, err)
	}
	if len(seen) == 0 || !strings.HasSuffix(seen[len(seen)-1], FinalTurnMessage) {
		t.Fatalf("last prompt %q", seen)
	}
}

// lastPromptRecorder records the last message of every prompt it answers.
type lastPromptRecorder struct {
	inner contracts.ModelProvider
	seen  *[]string
}

func (r *lastPromptRecorder) Name() string { return r.inner.Name() }
func (r *lastPromptRecorder) Complete(ctx context.Context, messages []contracts.ModelMessage, tools []map[string]any, maxTokens int) (contracts.TurnResult, error) {
	*r.seen = append(*r.seen, messages[len(messages)-1].Content)
	return r.inner.Complete(ctx, messages, tools, maxTokens)
}
func (r *lastPromptRecorder) EstimateCostUSD(u contracts.Usage) (float64, error) {
	return r.inner.EstimateCostUSD(u)
}
