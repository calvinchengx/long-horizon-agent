package durable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// The multi-agent organization on the durable path (python: tests/durability/test_org_workflow.py),
// with real git, worktrees, sandboxed checks and checkpoints; the models are scripted. A mission
// that opts in gets research fan-out as child workflows (failures surfaced in the gate log and a
// committed research event), parallel implementer waves integrated one branch at a time (every
// integration commit a checkpoint), independent review after every verified item (a blocking
// review reopens it), validated options; an abort during a wave waits for every implementer and
// ends ABORTED; and the org activities are retry-safe.

var (
	approveTurn = contracts.TurnResult{Text: `{"done": true, "verdict": "approve", "blocking_issues": []}`}
	blockTurn   = contracts.TurnResult{Text: `{"done": true, "verdict": "block", "blocking_issues": ["no tests"]}`}
)

// initOrgMission initializes n independent items; the first owned each own work/<id>.txt (wave
// candidates), the rest have no write-set (serial).
func initOrgMission(t *testing.T, n, owned int, set func(*MissionInput)) MissionInput {
	t.Helper()
	workdir := filepath.Join(t.TempDir(), "work")
	ownership := coordination.NewFileOwnershipMap()
	cl := testChecklist(n, false)
	for _, item := range cl.Items[:owned] {
		if err := ownership.Assign("work/"+item.ID+".txt", coordination.WriterForItem(item.ID)); err != nil {
			t.Fatal(err)
		}
	}
	var ownershipJSON []byte
	if owned > 0 {
		data, err := coordination.OwnershipJSON(ownership)
		if err != nil {
			t.Fatal(err)
		}
		ownershipJSON = data
	}
	if _, err := state.NewGitMissionAnchor(workdir).InitializeSpecWithOwnership(context.Background(),
		contracts.MissionSpec{Title: "Org mission", Description: "durable organization"}, cl, ownershipJSON); err != nil {
		t.Fatal(err)
	}
	inp := NewMissionInput(fmt.Sprintf("org-%d", time.Now().UnixNano()%1_000_000_000), workdir)
	inp.MaxCycles = 20
	inp.CheckCommands = checkCommands
	if set != nil {
		set(&inp)
	}
	return inp
}

// fakeResearchers records every researcher and fails the "Find the tests" ones.
type fakeResearchers struct {
	mu    sync.Mutex
	calls []SubAgentInput
}

func (f *fakeResearchers) run(_ context.Context, inp SubAgentInput) (SubAgentOutput, error) {
	f.mu.Lock()
	f.calls = append(f.calls, inp)
	f.mu.Unlock()
	if strings.HasPrefix(inp.Objective, "Find the tests") {
		return SubAgentOutput{}, temporal.NewNonRetryableApplicationError("search backend down (401)", "ApplicationError", nil)
	}
	return SubAgentOutput{Role: inp.RoleName, Brief: "BRIEF[" + inp.Objective + "]", Turns: 1}, nil
}

// reviewers hands out one scripted verdict per review (approve once the script is used up).
func reviewers(verdicts ...contracts.TurnResult) ModelFactory {
	var mu sync.Mutex
	queue := append([]contracts.TurnResult{}, verdicts...)
	return func(*config.Settings, contracts.SituationSnapshot) (contracts.ModelProvider, error) {
		mu.Lock()
		defer mu.Unlock()
		next := approveTurn
		if len(queue) > 0 {
			next, queue = queue[0], queue[1:]
		}
		return model.NewStub([]contracts.TurnResult{next}), nil
	}
}

// orgActs are the real activities with org models: the implementers and the Lead work the active
// item, the reviewers approve, the researchers are fakes.
func orgActs(t *testing.T, store *recordingStore) (*Activities, *fakeResearchers) {
	researchers := &fakeResearchers{}
	acts := newActs(t, workingModel, store)
	acts.ImplementerModel = workingModel
	acts.ReviewerModel = reviewers()
	acts.SubAgent = researchers.run
	return acts, researchers
}

// activityLog records the name of every activity attempt the test environment starts.
type activityLog struct {
	mu    sync.Mutex
	names []string
}

func (l *activityLog) listen(env *testEnv) {
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.names = append(l.names, info.ActivityType.Name)
	})
}

func (l *activityLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.names...)
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func count(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

func eventsOf(t *testing.T, workdir, kind string) []contracts.EventRecord {
	t.Helper()
	var out []contracts.EventRecord
	for _, e := range committedEvents(t, workdir) {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func gateLog(t *testing.T, env *testEnv) []string {
	t.Helper()
	var log []string
	env.query(t, QueryGateLog, &log)
	return log
}

func anyLine(lines []string, parts ...string) bool {
	for _, l := range lines {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func TestParallelWaveWithResearchAndReviewCompletes(t *testing.T) {
	inp := initOrgMission(t, 2, 2, func(in *MissionInput) {
		in.ResearchPerItem, in.Review, in.MaxParallel = 1, true, 2
	})
	acts, researchers := orgActs(t, nil)
	env := newEnv(t, acts, nil)
	res, err := env.run(inp)
	if err != nil || !res.Completed || res.Cycles != 2 || res.ItemsDone != 2 {
		t.Fatalf("result %+v err %v", res, err)
	}
	log := gateLog(t, env)
	work := inp.Workdir
	if !fileExists(filepath.Join(work, "work", "01.txt")) || !fileExists(filepath.Join(work, "work", "02.txt")) {
		t.Fatal("the work files were not merged")
	}
	if n := commitsWith(t, work, "[merged lha/implementer-"); n != 2 {
		t.Fatalf("%d merge checkpoints", n) // each integration commit is a checkpoint
	}
	tickets := eventsOf(t, work, "ticket")
	if len(tickets) != 2 || tickets[0].Payload.Plain()["status"] != "done" || tickets[1].Payload.Plain()["status"] != "done" {
		t.Fatalf("tickets %v", tickets)
	}
	screens := eventsOf(t, work, "review_screen") // the pre-review screen ran before each review
	reviewed := eventsOf(t, work, "review")
	if len(screens) != 2 || len(reviewed) != 2 || screens[0].CycleID != reviewed[0].CycleID || screens[1].CycleID != reviewed[1].CycleID ||
		screens[0].Payload.Plain()["forced"] != false || len(screens[0].Payload.Plain()["findings"].([]any)) != 0 {
		t.Fatalf("screens %v", screens)
	}
	// The round's board posts are committed with the integration checkpoints: each research
	// brief and each implementer's summary, as `lha orchestrate` posts them.
	authors := map[string]string{}
	for _, b := range eventsOf(t, work, "blackboard") {
		authors[b.Payload.Plain()["author"].(string)] = b.Payload.Plain()["text"].(string)
	}
	if len(authors) != 4 || !strings.HasPrefix(authors["researcher:01"], "BRIEF[") || !strings.HasPrefix(authors["researcher:02"], "BRIEF[") ||
		!strings.HasPrefix(authors["implementer-01"], "[01] ") || !strings.HasPrefix(authors["implementer-02"], "[02] ") {
		t.Fatalf("board %v", authors)
	}
	var history []string
	for _, h := range tickets[0].Payload.Plain()["history"].([]any) {
		history = append(history, h.(map[string]any)["status"].(string))
	}
	if strings.Join(history, ",") != "created,in_progress,awaiting_verify,awaiting_merge,done" {
		t.Fatalf("ticket history %v", history)
	}
	reviews := eventsOf(t, work, "review")
	if len(reviews) != 2 || reviews[0].Payload.Plain()["verdict"] != "approve" || reviews[1].Payload.Plain()["verdict"] != "approve" {
		t.Fatalf("reviews %v", reviews)
	}
	var researched []string
	for _, r := range eventsOf(t, work, "research") {
		researched = append(researched, r.Payload.Plain()["item"].(string))
	}
	sort.Strings(researched)
	if strings.Join(researched, ",") != "01,02" {
		t.Fatalf("research events %v", researched)
	}
	if len(researchers.calls) != 2 {
		t.Fatalf("%d researchers", len(researchers.calls))
	}
	for _, c := range researchers.calls {
		if c.RoleName != "researcher" || c.BudgetUSD != nil || !c.AllowEgress || c.CycleID != "c1-research" {
			t.Fatalf("researcher input %+v", c)
		}
	}
	if !anyLine(log, "parallel wave 01, 02") {
		t.Fatalf("gate log %v", log)
	}
	// Worktrees and branches are gone; the ownership map was released as items finished.
	branches, _ := state.ListBranches(context.Background(), work, false)
	for _, b := range branches {
		if strings.HasPrefix(b, "lha/") {
			t.Fatalf("branch %s left behind", b)
		}
	}
	owners, err := coordination.ReadOwnership(context.Background(), state.NewGitMissionAnchor(work))
	if err != nil || len(owners.Snapshot()) != 0 {
		t.Fatalf("owners %v %v", owners.Snapshot(), err)
	}
}

// recorder is a scripted model that keeps every prompt it was sent.
type recorder struct {
	*model.StubModel
	mu   *sync.Mutex
	seen *[]string
}

func (r recorder) Complete(ctx context.Context, messages []contracts.ModelMessage, tools []map[string]any, maxTokens int) (contracts.TurnResult, error) {
	r.mu.Lock()
	for _, m := range messages {
		*r.seen = append(*r.seen, m.Content)
	}
	r.mu.Unlock()
	return r.StubModel.Complete(ctx, messages, tools, maxTokens)
}

func TestSerialRoundResearchFailuresSurfaceAndReviewReopens(t *testing.T) {
	inp := initOrgMission(t, 1, 0, func(in *MissionInput) { in.ResearchPerItem, in.Review = 3, true })
	var mu sync.Mutex
	var seen []string
	var cycles atomic.Int32
	lead := func(_ *config.Settings, snap contracts.SituationSnapshot) (contracts.ModelProvider, error) {
		if snap.ActiveItem == nil {
			return nil, errors.New("no active item")
		}
		n := cycles.Add(1)
		return recorder{model.NewStub([]contracts.TurnResult{writeTurn(fmt.Sprintf("work/%d.txt", n)), doneTurn()}), &mu, &seen}, nil
	}
	acts, researchers := orgActs(t, nil)
	acts.ModelFactory = lead
	acts.ReviewerModel = reviewers(blockTurn, approveTurn)
	env := newEnv(t, acts, nil)
	res, err := env.run(inp)
	if err != nil || !res.Completed || res.Cycles != 2 {
		t.Fatalf("result %+v err %v", res, err)
	}
	log := gateLog(t, env)
	if len(researchers.calls) != 6 { // 3 per round, 2 rounds
		t.Fatalf("%d researchers", len(researchers.calls))
	}
	if !anyLine(log, "research for 01 failed", "401") || !anyLine(log, "review of 01 (c1) reopened it") {
		t.Fatalf("gate log %v", log)
	}
	research := eventsOf(t, inp.Workdir, "research")
	if len(research) != 2 || research[0].Payload.Plain()["failed"] != 1.0 || research[1].Payload.Plain()["failed"] != 1.0 {
		t.Fatalf("research events %v", research)
	}
	if f := research[0].Payload.Plain()["failures"].([]any)[0].(string); !strings.Contains(f, "search backend down") ||
		!strings.HasPrefix(f, "researcher: ApplicationError: ") {
		t.Fatalf("failure %q", f)
	}
	if !anyLine(seen, "BRIEF[Find context relevant to: task 1]") {
		t.Fatal("the briefs never reached the Lead's prompt")
	}
	reviews := eventsOf(t, inp.Workdir, "review")
	if len(reviews) != 2 || reviews[0].Payload.Plain()["reopened"] != true || reviews[1].Payload.Plain()["reopened"] != false {
		t.Fatalf("reviews %v", reviews)
	}
	// The blocking verdict was posted to the board, and the second round's Lead saw the board
	// in its prompt.
	board := eventsOf(t, inp.Workdir, "blackboard")
	if len(board) != 1 || board[0].Payload.Plain()["author"] != "reviewer:01" || !strings.Contains(board[0].Payload.Plain()["text"].(string), "Review verdict: block") {
		t.Fatalf("board %v", board)
	}
	if !anyLine(seen, "Team board (earlier rounds):\n[reviewer:01] Review verdict: block") {
		t.Fatal("the board never reached the Lead's prompt")
	}
	cl, _ := state.NewGitMissionAnchor(inp.Workdir).ReadChecklist(context.Background())
	if item := cl.Items[0]; item.Status != contracts.StatusDone || strings.Contains(item.LastFailure, "reviewer blocked") {
		t.Fatalf("item %+v", item)
	}
}

func TestOrgOptionsAreValidated(t *testing.T) {
	inp := initOrgMission(t, 1, 1, func(in *MissionInput) { in.ResearchPerItem = 9 })
	if !strings.Contains(OrgConfigError(inp), "research_per_item") {
		t.Fatal(OrgConfigError(inp))
	}
	if !OrgEnabled(inp) || OrgEnabled(NewMissionInput("m", ".")) {
		t.Fatal("OrgEnabled")
	}
	bad := NewMissionInput("m", ".")
	bad.MaxParallel = 99
	if OrgConfigError(bad) == "" {
		t.Fatal("max_parallel 99 accepted")
	}
	acts, _ := orgActs(t, nil)
	if _, err := newEnv(t, acts, nil).run(inp); err == nil || !strings.Contains(err.Error(), "research_per_item must be 0..4 (got 9)") {
		t.Fatalf("err %v", err)
	}
}

// failingImplementer: the implementer for itemID fails `times` times, with a model that cannot be
// built (a non-retryable configuration error) or, with outage, a model call that errors (a
// retryable failure).
func failingImplementer(itemID string, outage bool, times int32) ModelFactory {
	var left atomic.Int32
	left.Store(times)
	return func(s *config.Settings, snap contracts.SituationSnapshot) (contracts.ModelProvider, error) {
		if snap.ActiveItem == nil {
			return nil, errors.New("no active item")
		}
		if snap.ActiveItem.ID == itemID && left.Add(-1) >= 0 {
			if !outage {
				return nil, errors.New("model endpoint unavailable")
			}
			return erroringModel{}, nil
		}
		return workingModel(s, snap)
	}
}

type erroringModel struct{}

func (erroringModel) Name() string { return "fake:down" }
func (erroringModel) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	return contracts.TurnResult{}, errors.New("model endpoint unavailable")
}
func (erroringModel) EstimateCostUSD(contracts.Usage) (float64, error) { return 0, nil }

func cycleItems(t *testing.T, workdir string) string {
	var ids []string
	for _, c := range eventsOf(t, workdir, "cycle") {
		ids = append(ids, fmt.Sprint(c.Payload.Plain()["item_id"]))
	}
	return strings.Join(ids, ",")
}

func TestAFailedImplementerIsAFailedAttemptAndTheItemGoesSerial(t *testing.T) {
	inp := initOrgMission(t, 2, 2, func(in *MissionInput) { in.MaxParallel = 2 })
	acts, _ := orgActs(t, nil)
	acts.ImplementerModel = failingImplementer("02", false, 1)
	res, err := newEnv(t, acts, nil).run(inp)
	if err != nil || !res.Completed || res.Cycles != 3 { // 01 merged, 02 failed, 02 by the Lead
		t.Fatalf("result %+v err %v", res, err)
	}
	tickets := eventsOf(t, inp.Workdir, "ticket")
	if len(tickets) != 2 || tickets[0].Payload.Plain()["status"] != "done" || tickets[1].Payload.Plain()["status"] != "failed" {
		t.Fatalf("tickets %v", tickets)
	}
	if got := cycleItems(t, inp.Workdir); got != "01,02,02" {
		t.Fatalf("cycles %s", got)
	}
	if !fileExists(filepath.Join(inp.Workdir, "work", "02.txt")) {
		t.Fatal("02 was never done")
	}
	cl, _ := state.NewGitMissionAnchor(inp.Workdir).ReadChecklist(context.Background())
	if cl.Items[1].Attempts < 1 {
		t.Fatalf("no failed attempt recorded: %+v", cl.Items[1])
	}
}

func TestAnImplementerOutageParksTheMissionAfterIntegratingTheRest(t *testing.T) {
	inp := initOrgMission(t, 2, 2, func(in *MissionInput) { in.MaxParallel, in.ParkInitialSeconds = 2, 30 })
	store := &recordingStore{}
	acts, _ := orgActs(t, store)
	acts.ImplementerModel = failingImplementer("02", true, 5) // every retry of the first round fails
	env := newEnv(t, acts, nil)
	var log activityLog
	log.listen(env)
	res, err := env.run(inp)
	if err != nil || !res.Completed || res.Cycles != 2 {
		t.Fatalf("result %+v err %v", res, err)
	}
	started := log.all()
	if count(started, ActivityIntegrateBranch) != 1 || count(started, ActivityRunImplementer) != 6 { // 01 once, 02 five times
		t.Fatalf("activities %v", started)
	}
	if h := indexOf(started, ActivityCheckMissionHealth); h < 0 || indexOf(started, ActivityIntegrateBranch) > h {
		t.Fatalf("01 was not integrated before parking: %v", started)
	}
	if !contains(store.Statuses(), StatusDegradedPark) {
		t.Fatalf("never parked: %v", store.Statuses())
	}
	if got := cycleItems(t, inp.Workdir); got != "01,02" {
		t.Fatalf("cycles %s", got)
	}
}

func TestAReviewThatCannotRunLeavesTheItemDone(t *testing.T) {
	inp := initOrgMission(t, 1, 0, func(in *MissionInput) { in.Review = true })
	acts, _ := orgActs(t, nil)
	acts.ReviewerModel = func(*config.Settings, contracts.SituationSnapshot) (contracts.ModelProvider, error) {
		return nil, errors.New("no reviewer model configured")
	}
	env := newEnv(t, acts, nil)
	res, err := env.run(inp)
	if err != nil || !res.Completed || res.Cycles != 1 {
		t.Fatalf("result %+v err %v", res, err)
	}
	if log := gateLog(t, env); !anyLine(log, "review of 01 (c1) failed", "unreviewed", "no reviewer model configured") {
		t.Fatalf("gate log %v", log)
	}
	if len(eventsOf(t, inp.Workdir, "review")) != 0 {
		t.Fatal("a review was committed")
	}
}

// leaseThenWrite: the implementer asks for a lease on extra, writes it and its own file.
func leaseThenWrite(extra string) ModelFactory {
	return func(_ *config.Settings, snap contracts.SituationSnapshot) (contracts.ModelProvider, error) {
		if snap.ActiveItem == nil {
			return nil, errors.New("no active item")
		}
		own := "work/" + snap.ActiveItem.ID + ".txt"
		lease := contracts.TurnResult{ToolCalls: []contracts.ToolCall{{ID: "l1", Name: "request_lease",
			Arguments: map[string]any{"path": extra, "reason": "the item needs a helper"}}}, StopReason: strPtr("tool_use")}
		return model.NewStub([]contracts.TurnResult{lease, writeContent(extra, "helper"), writeTurn(own), doneTurn()}), nil
	}
}

func writeContent(path, content string) contracts.TurnResult {
	turn := writeTurn(path)
	turn.ToolCalls[0].Arguments["content"] = content
	return turn
}

// A lease granted inside a durable wave is committed at once and its file merged with the branch.
func TestALeaseInsideADurableWaveIsGrantedAndMerged(t *testing.T) {
	inp := initOrgMission(t, 2, 2, func(in *MissionInput) { in.MaxParallel = 2 })
	acts, _ := orgActs(t, nil)
	helper := leaseThenWrite("lib/helper.txt")
	acts.ImplementerModel = func(s *config.Settings, snap contracts.SituationSnapshot) (contracts.ModelProvider, error) {
		if snap.ActiveItem != nil && snap.ActiveItem.ID == "01" {
			return helper(s, snap)
		}
		return workingModel(s, snap)
	}
	res, err := newEnv(t, acts, nil).run(inp)
	if err != nil || !res.Completed || res.Cycles != 2 {
		t.Fatalf("result %+v err %v", res, err)
	}
	if data, _ := os.ReadFile(filepath.Join(inp.Workdir, "lib", "helper.txt")); string(data) != "helper" {
		t.Fatalf("the leased file was not merged: %q", data)
	}
	leases := eventsOf(t, inp.Workdir, "lease")
	if len(leases) != 1 || leases[0].Payload.Plain()["granted"] != true || leases[0].Payload.Plain()["writer"] != "implementer-01" {
		t.Fatalf("leases %v", leases)
	}
	ticket := eventsOf(t, inp.Workdir, "ticket")[0]
	if l := ticket.Payload.Plain()["leases"].([]any); len(l) != 1 || l[0].(map[string]any)["granted"] != true {
		t.Fatalf("ticket leases %v", ticket.Payload.Plain()["leases"])
	}
}

// --- the activities, called directly (retry safety) ------------------------------------------

func TestPlanRoundPicksAWaveOrTheNextItem(t *testing.T) {
	inp := initOrgMission(t, 3, 3, nil)
	acts, _ := orgActs(t, nil)
	ctx := context.Background()
	plan, err := acts.PlanRound(ctx, RoundInput{MissionID: "m", Workdir: inp.Workdir, MaxParallel: 2})
	if err != nil || !plan.Parallel || len(plan.Items) != 2 || plan.Items[0].ItemID != "01" || plan.Items[1].ItemID != "02" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	serial, err := acts.PlanRound(ctx, RoundInput{MissionID: "m", Workdir: inp.Workdir})
	if err != nil || serial.Parallel || len(serial.Items) != 1 || serial.Items[0].ItemID != "01" {
		t.Fatalf("serial %+v %v", serial, err)
	}
	if head, _ := state.HeadSHA(ctx, inp.Workdir); serial.HeadSHA != head {
		t.Fatalf("head %s", serial.HeadSHA)
	}
}

func TestImplementerAndIntegrationAreRetrySafeAndHonourLeases(t *testing.T) {
	inp := initOrgMission(t, 2, 2, nil)
	work := inp.Workdir
	ctx := context.Background()
	base, _ := state.HeadSHA(ctx, work)
	acts, _ := orgActs(t, nil)
	acts.ImplementerModel = leaseThenWrite("lib/helper.txt")
	impl := ImplementerInput{MissionID: "m", Workdir: work, CycleID: "c1", ItemID: "01", BaseSHA: base,
		CheckCommands: checkCommands, MaxCycles: 1000}
	out, err := acts.RunImplementer(ctx, impl)
	if err != nil || out.Error != "" || out.Branch != "lha/implementer-01/c1" || out.Head == "" {
		t.Fatalf("out %+v err %v", out, err)
	}
	var verification map[string]any
	if json.Unmarshal([]byte(out.VerificationJSON), &verification) != nil || verification["all_green"] != true {
		t.Fatalf("verification %s", out.VerificationJSON)
	}
	if len(out.Leases) != 1 || !strings.Contains(out.Leases[0], `"granted":true`) {
		t.Fatalf("leases %v", out.Leases)
	}
	anchor := state.NewGitMissionAnchor(work)
	owners, _ := coordination.ReadOwnership(ctx, anchor)
	if owners.OwnerOf("lib/helper.txt") != "implementer-01" {
		t.Fatalf("owners %v", owners.Snapshot())
	}
	// The grant is committed at once (ownership + a lease event), only .lha/ in that commit.
	names, _ := state.RunGit(ctx, work, "show", "--name-only", "--pretty=format:", "HEAD")
	for _, n := range strings.Fields(names) {
		if n != ".lha/ownership.json" && n != ".lha/events.ndjson" && n != ".lha/progress.md" {
			t.Fatalf("lease commit has %s", n)
		}
	}
	if leases := eventsOf(t, work, "lease"); leases[0].Payload.Plain()["path"] != "lib/helper.txt" {
		t.Fatalf("lease events %v", leases)
	}
	// A retried attempt (e.g. after a crash before reporting) returns the committed branch.
	acts.ImplementerModel = workingModel
	again, err := acts.RunImplementer(ctx, impl)
	if err != nil || !reflect.DeepEqual(again, out) {
		t.Fatalf("again %+v\nout %+v\nerr %v", again, out, err)
	}
	// Nothing of the implementer's work is in the mission branch until it is integrated.
	if fileExists(filepath.Join(work, "lib", "helper.txt")) {
		t.Fatal("the implementer wrote the mission checkout")
	}

	integrate := IntegrateInput{MissionID: "m", Workdir: work, CycleID: "c1", ItemID: "01", BaseSHA: base,
		Output: &out, CheckCommands: checkCommands, MaxCycles: 1000}
	first, err := acts.IntegrateBranch(ctx, integrate)
	if err != nil || !first.Advanced || first.Verdict != "passed" || first.ItemsDone != 1 {
		t.Fatalf("first %+v err %v", first, err)
	}
	if data, _ := os.ReadFile(filepath.Join(work, "lib", "helper.txt")); string(data) != "helper" {
		t.Fatalf("the leased file was not merged: %q", data)
	}
	head, _ := state.HeadSHA(ctx, work)
	repeat, err := acts.IntegrateBranch(ctx, integrate)
	if err != nil || repeat.Note != "already integrated by a previous attempt" {
		t.Fatalf("repeat %+v err %v", repeat, err)
	}
	if now, _ := state.HeadSHA(ctx, work); now != head {
		t.Fatal("committed twice")
	}
	// The finished writer's files (including the lease) are released.
	owners, _ = coordination.ReadOwnership(ctx, anchor)
	if snap := owners.Snapshot(); len(snap) != 1 || snap["work/02.txt"] != "implementer-02" {
		t.Fatalf("owners %v", snap)
	}
}

func TestALeaseOwnedByAnOpenItemIsRefused(t *testing.T) {
	inp := initOrgMission(t, 2, 2, nil)
	ctx := context.Background()
	base, _ := state.HeadSHA(ctx, inp.Workdir)
	acts, _ := orgActs(t, nil)
	acts.ImplementerModel = leaseThenWrite("work/02.txt")
	out, err := acts.RunImplementer(ctx, ImplementerInput{MissionID: "m", Workdir: inp.Workdir, CycleID: "c1",
		ItemID: "01", BaseSHA: base, CheckCommands: checkCommands, MaxCycles: 1000})
	if err != nil || len(out.Leases) != 1 {
		t.Fatalf("out %+v err %v", out, err)
	}
	decision, _ := ParseLeaseDecisionJSON(out.Leases[0])
	if decision.Granted || !strings.Contains(decision.Why, "implementer-02") {
		t.Fatalf("decision %+v", decision)
	}
	// It tried to write the file anyway (the guard refused write_file; nothing reached the branch).
	changed, _ := state.RunGit(ctx, inp.Workdir, "diff", "--name-only", base+".."+out.Head)
	if contains(strings.Fields(changed), "work/02.txt") {
		t.Fatalf("the branch changed work/02.txt: %s", changed)
	}
	if refused := eventsOf(t, inp.Workdir, "lease")[0]; refused.Payload.Plain()["granted"] != false {
		t.Fatalf("lease event %v", refused)
	}
}

func TestFailedImplementerIsRecordedAsAFailedAttempt(t *testing.T) {
	inp := initOrgMission(t, 2, 2, nil)
	ctx := context.Background()
	base, _ := state.HeadSHA(ctx, inp.Workdir)
	acts, _ := orgActs(t, nil)
	result, err := acts.IntegrateBranch(ctx, IntegrateInput{MissionID: "m", Workdir: inp.Workdir, CycleID: "c1",
		ItemID: "01", BaseSHA: base, Error: "MissionConfigError: cannot open the sandbox", CheckCommands: checkCommands,
		MaxCycles: 1000})
	if err != nil || !result.Advanced || result.Verdict != "failed" {
		t.Fatalf("result %+v err %v", result, err)
	}
	cl, _ := state.NewGitMissionAnchor(inp.Workdir).ReadChecklist(ctx)
	if item := cl.Items[0]; item.Status != contracts.StatusInProgress || !strings.Contains(item.LastFailure, "cannot open the sandbox") {
		t.Fatalf("item %+v", item)
	}
	if tickets := eventsOf(t, inp.Workdir, "ticket"); len(tickets) != 1 || tickets[0].Payload.Plain()["status"] != "failed" {
		t.Fatalf("tickets %v", tickets)
	}
	// The failed attempt was reflected on, and the lesson committed by itself for the next try.
	reflections := eventsOf(t, inp.Workdir, "reflection")
	if len(reflections) != 1 || reflections[0].Payload.Plain()["item"] != "01" ||
		!strings.HasPrefix(reflections[0].Payload.Plain()["text"].(string), "\nReflection on 01: ") {
		t.Fatalf("reflections %v", reflections)
	}
	if n := commitsWith(t, inp.Workdir, "lha: reflection on 01"); n != 1 {
		t.Fatalf("%d reflection commits", n)
	}
}

func TestReviewIsExactlyOnceAndRepeatedBlocksBlockTheItem(t *testing.T) {
	inp := initOrgMission(t, 1, 0, nil)
	work := inp.Workdir
	ctx := context.Background()
	anchor := state.NewGitMissionAnchor(work)
	acts, _ := orgActs(t, nil)
	for n := 1; n <= 3; n++ {
		cl, _ := anchor.ReadChecklist(ctx)
		if _, err := cl.RecordSuccess("01", []string{"check"}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("f%d.txt", n)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		head, err := anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: fmt.Sprintf("c%d", n),
			ProgressSummary: fmt.Sprintf("- c%d", n), Checklist: cl})
		if err != nil {
			t.Fatal(err)
		}
		review := ReviewInput{MissionID: "m", Workdir: work, CycleID: fmt.Sprintf("c%d", n), ItemID: "01", HeadSHA: head, MaxCycles: 1000}
		acts.ReviewerModel = reviewers(blockTurn)
		result, err := acts.ReviewCycle(ctx, review)
		if err != nil || result.Verdict != "review_blocked" || result.ItemBlocked != (n == 3) {
			t.Fatalf("review %d: %+v err %v", n, result, err)
		}
		acts.ReviewerModel = reviewers()
		again, err := acts.ReviewCycle(ctx, review)
		if err != nil || again.Note != "already reviewed by a previous attempt" {
			t.Fatalf("again %+v err %v", again, err)
		}
	}
	cl, _ := anchor.ReadChecklist(ctx)
	if item := cl.Items[0]; item.Status != contracts.StatusBlocked || !strings.Contains(item.LastFailure, "no tests") {
		t.Fatalf("item %+v", item)
	}
	var blocked []any
	for _, r := range eventsOf(t, work, "review") {
		blocked = append(blocked, r.Payload.Plain()["blocked"])
	}
	if !reflect.DeepEqual(blocked, []any{false, false, true}) {
		t.Fatalf("blocked %v", blocked)
	}
}

// lha mission-abort during a parallel wave, on a real Temporal server (the test environment does
// not wait for a cancelled activity): every implementer in flight is waited for (one
// acknowledging, one finishing anyway), nothing is integrated, and the workflow then ends
// cancelled with the missions row ABORTED.
func TestAbortDuringAnOrgWaveWaitsForTheImplementersAndEndsAborted(t *testing.T) {
	addr := temporalServer(t)
	dc, err := NewDataConverter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cl, err := client.Dial(client.Options{HostPort: addr, DataConverter: dc, Logger: Logger(io.Discard, slog.LevelError)})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	inp := initOrgMission(t, 2, 2, func(in *MissionInput) { in.MaxParallel = 2 })
	store := &recordingStore{}
	acts, _ := orgActs(t, store)
	var running atomic.Int32
	bothRunning := make(chan struct{})
	late := func(ctx context.Context, in ImplementerInput) (ImplementerOutput, error) {
		if running.Add(1) == 2 {
			close(bothRunning)
		}
		for ctx.Err() == nil {
			activity.RecordHeartbeat(ctx, in.ItemID)
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond) // the workflow must wait for this, not write ABORTED first
		_, _ = acts.RecordMissionStatus(context.Background(), MissionStatusInput{MissionID: in.MissionID, Workdir: in.Workdir, Status: StatusRunning})
		if in.ItemID == "01" {
			return ImplementerOutput{}, ctx.Err()
		}
		return ImplementerOutput{ItemID: in.ItemID, CycleID: in.CycleID}, nil
	}
	queue := fmt.Sprintf("lha-org-abort-%d", time.Now().UnixNano())
	w := worker.New(cl, queue, worker.Options{Identity: WorkerIdentity(), MaxHeartbeatThrottleInterval: 200 * time.Millisecond})
	registerWith(w, acts, map[string]any{ActivityRunImplementer: late})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	wid := MissionWorkflowID(inp.MissionID)
	run, err := cl.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{ID: wid, TaskQueue: queue}, WorkflowMission, inp)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-bothRunning:
	case <-time.After(60 * time.Second):
		t.Fatal("the implementers never ran")
	}
	if err := cl.CancelWorkflow(context.Background(), wid, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := run.Get(ctx, nil); !temporal.IsCanceledError(err) {
		t.Fatalf("err %v", err)
	}
	var order []string
	names := map[int64]string{}
	iter := cl.GetWorkflowHistory(context.Background(), wid, run.GetRunID(), false, 0)
	for iter.HasNext() {
		ev, err := iter.Next()
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case ev.GetActivityTaskScheduledEventAttributes() != nil:
			names[ev.EventId] = ev.GetActivityTaskScheduledEventAttributes().GetActivityType().GetName()
			order = append(order, "scheduled:"+names[ev.EventId])
		case ev.GetActivityTaskCompletedEventAttributes() != nil:
			order = append(order, "completed:"+names[ev.GetActivityTaskCompletedEventAttributes().GetScheduledEventId()])
		case ev.GetActivityTaskCanceledEventAttributes() != nil:
			order = append(order, "canceled:"+names[ev.GetActivityTaskCanceledEventAttributes().GetScheduledEventId()])
		case ev.GetActivityTaskFailedEventAttributes() != nil:
			order = append(order, "failed:"+names[ev.GetActivityTaskFailedEventAttributes().GetScheduledEventId()])
		}
	}
	want := []string{"scheduled:plan_round", "completed:plan_round", "scheduled:run_implementer", "scheduled:run_implementer"}
	if len(order) != 8 || !reflect.DeepEqual(order[:4], want) {
		t.Fatalf("history %v", order)
	}
	closed := append([]string{}, order[4:6]...)
	sort.Strings(closed)
	// Both implementers closed (after their late RUNNING writes) before ABORTED was written.
	if !reflect.DeepEqual(closed, []string{"canceled:run_implementer", "completed:run_implementer"}) ||
		!reflect.DeepEqual(order[6:], []string{"scheduled:record_mission_status", "completed:record_mission_status"}) {
		t.Fatalf("history %v", order)
	}
	if s := store.Statuses(); strings.Join(s, ",") != "RUNNING,RUNNING,ABORTED" {
		t.Fatalf("row %v", s)
	}
	var status string
	if v, err := cl.QueryWorkflow(context.Background(), wid, "", QueryStatus); err != nil || v.Get(&status) != nil || status != StatusAborted {
		t.Fatalf("status %q %v", status, err)
	}
	checklist, _ := state.NewGitMissionAnchor(inp.Workdir).ReadChecklist(context.Background())
	for _, item := range checklist.Items {
		if item.Status == contracts.StatusDone {
			t.Fatal("an item was integrated")
		}
	}
}

// A Go history recorded before the organization (VersionOrg) refused the options; the version
// marker is only written for a mission that opts in.
func TestTheOrgPathIsVersioned(t *testing.T) {
	inp := initOrgMission(t, 1, 0, func(in *MissionInput) { in.Review = true })
	acts, _ := orgActs(t, nil)
	env := newEnv(t, acts, nil)
	env.OnGetVersion(VersionOrg, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	if _, err := env.run(inp); err == nil || !strings.Contains(err.Error(), "not yet available in the Go implementation") {
		t.Fatalf("err %v", err)
	}
}

// Every role's spend lands in the spend journal (one line per attempt, python's cycle ids) and in
// the cost ledger call by call (python's key prefixes); the researchers run the real sub-agent.
func TestOrgSpendIsJournaledAndLedgeredForEveryRole(t *testing.T) {
	inp := initOrgMission(t, 2, 2, func(in *MissionInput) {
		in.ResearchPerItem, in.Review, in.MaxParallel = 1, true, 2
	})
	store := &recordingStore{}
	acts, _ := orgActs(t, store)
	acts.SubAgent = nil // the real run_subagent
	acts.SubAgentModel = func(*config.Settings, contracts.SituationSnapshot) (contracts.ModelProvider, error) {
		return model.NewStub([]contracts.TurnResult{{Text: `{"done": true, "summary": "the brief"}`}}), nil
	}
	env := newEnv(t, acts, nil)
	if res, err := env.run(inp); err != nil || !res.Completed {
		t.Fatalf("result %+v err %v", res, err)
	}
	prefixes := map[string]int{}
	for _, key := range store.costs {
		prefixes[strings.SplitN(key, ":", 2)[0]]++
	}
	if prefixes["impl"] != 4 || prefixes["review"] != 2 || prefixes["sub"] != 2 { // two calls per implementer
		t.Fatalf("cost ledger keys %v", store.costs)
	}
	keyRE := regexp.MustCompile(`^(impl|review):[^:]+:\d+@1#\d+$|^sub:subagent:[^:]+:researcher:[0-9a-f]{12}:\d+@1#0$`)
	for _, key := range store.costs {
		if !keyRE.MatchString(key) {
			t.Fatalf("key %s", key)
		}
	}
	gitDir, _ := state.GitDir(context.Background(), inp.Workdir)
	journal, err := os.ReadFile(filepath.Join(gitDir, "lha", "spend.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for _, cycle := range []string{`"c1"`, `"c2"`, `"c1-review"`, `"c2-review"`, `"c1-research:researcher"`} {
		if !strings.Contains(string(journal), `"cycle_id": `+cycle) {
			t.Fatalf("no %s spend in the journal:\n%s", cycle, journal)
		}
	}
	research := eventsOf(t, inp.Workdir, "research")
	if len(research) != 2 || research[0].Payload.Plain()["n"] != 1.0 {
		t.Fatalf("research %v", research)
	}
}

// A wave can advance several cycles: Continue-As-New happens when a multiple of
// cycles_before_can is crossed (2 -> 4 crosses 3), not only when one is hit.
func TestAWaveCrossingCyclesBeforeCANContinuesAsNew(t *testing.T) {
	inp := initOrgMission(t, 6, 6, func(in *MissionInput) { in.MaxParallel, in.CyclesBeforeCAN = 2, 3 })
	acts, _ := orgActs(t, nil)
	res, runs := runFollowingCAN(t, func() *testEnv { return newEnv(t, acts, nil) }, inp)
	if !res.Completed || res.Cycles != 6 || runs != 2 {
		t.Fatalf("result %+v runs %d", res, runs)
	}
	if n := commitsWith(t, inp.Workdir, "[merged lha/implementer-"); n != 6 {
		t.Fatalf("%d merges", n)
	}
}

// integrate_branch re-verifies the merged checkout with the Lead's verifier, flaky re-runs
// included (LHA_FLAKY_RETRIES): a check that fails once on the merge is re-run, not a refusal.
func TestIntegrationReRunsAFlakyCheck(t *testing.T) {
	for _, retries := range []string{"1", "0"} {
		t.Run("retries="+retries, func(t *testing.T) {
			inp := initOrgMission(t, 2, 2, nil)
			ctx := context.Background()
			base, _ := state.HeadSHA(ctx, inp.Workdir)
			counter := filepath.Join(t.TempDir(), "runs")
			// Passes on its 1st run (the implementer's worktree), fails on its 2nd (the merge).
			flaky := []string{"sh", "-c", fmt.Sprintf(`n=$(cat %s 2>/dev/null || echo 0); n=$((n+1)); echo $n > %s; [ $n -ne 2 ]`, counter, counter)}
			checks := [][]string{checkCommands[0], flaky}
			acts, _ := orgActs(t, nil)
			acts.Settings = testSettings(t, "LHA_FLAKY_RETRIES="+retries)
			out, err := acts.RunImplementer(ctx, ImplementerInput{MissionID: "m", Workdir: inp.Workdir, CycleID: "c1",
				ItemID: "01", BaseSHA: base, CheckCommands: checks, MaxCycles: 1000})
			if err != nil || !strings.Contains(out.VerificationJSON, `"all_green":true`) {
				t.Fatalf("out %+v err %v", out, err)
			}
			result, err := acts.IntegrateBranch(ctx, IntegrateInput{MissionID: "m", Workdir: inp.Workdir, CycleID: "c1",
				ItemID: "01", BaseSHA: base, Output: &out, CheckCommands: checks, MaxCycles: 1000})
			if err != nil {
				t.Fatal(err)
			}
			if merged := result.Verdict == "passed"; merged != (retries == "1") {
				t.Fatalf("retries=%s: %+v", retries, result)
			}
		})
	}
}
