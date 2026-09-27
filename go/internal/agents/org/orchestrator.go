package org

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/tools"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// The multi-agent orchestrator — drives the full org through a mission (python:
// lha.agents.orchestrator). Each round is either a SERIAL cycle or a PARALLEL wave:
//
//   - Serial cycle (the default): fan out read-only Researchers, hand the briefs to the single
//     Lead Engineer loop (implement + verify + checkpoint in the mission workspace), then on
//     failure run reflection (fed into the next attempt) or on success an independent Reviewer
//     whose structured verdict can REOPEN the item.
//   - Parallel wave: when two or more actionable items have disjoint, Planner-assigned write-sets,
//     each is worked by its own Implementer in its own git worktree on its own branch,
//     concurrently, behind an OwnershipGuard; the BranchIntegrator merges each verified branch
//     into the mission branch, re-verifies the merged result and commits the merge together with
//     the checkpoint; the Reviewer then reviews each integrated item.
//
// Research briefs, implementer summaries and review notes go to a Blackboard whose response board
// is promoted between rounds. Every model call of every role goes through ONE CostMeter.
//
// Resuming (RunMission with Resume, `lha orchestrate --resume`) keeps the existing anchor;
// uncommitted residue, a half-finished merge and leftover implementer worktrees are discarded;
// the mission id, the blackboard's main board and the latest reflections are rebuilt from the
// committed "orchestrate" / "blackboard" / "reflection" events, and cycle ids continue after the
// last committed one. The anchor, commits and events are the Python implementation's, so a
// mission started by one implementation can be resumed by the other.

// Committed event kinds that let a resume rebuild the in-memory state of a run.
const (
	RunEvent        = "orchestrate"
	BoardEvent      = "blackboard"
	ReflectionEvent = "reflection"
)

var orgRoles = []string{"lead", "researcher", "reviewer", "implementer"}

const (
	reviewDiffCap = 20_000
	boardEntries  = 6 // newest blackboard entries shown to later rounds
	boardEntryCap = 1_500
)

var cycleIDRE = regexp.MustCompile(`^c(\d+)$`)

// MissionResumeError is a resume asked for a workdir without a mission anchor.
type MissionResumeError struct{ Message string }

func (e *MissionResumeError) Error() string { return e.Message }

// AnchorExists reports whether workdir holds a committed mission anchor (.lha/mission.json at
// HEAD).
func AnchorExists(ctx context.Context, workdir string) bool {
	if info, err := os.Stat(filepath.Join(workdir, state.AnchorDir)); err != nil || !info.IsDir() {
		return false
	}
	if ok, err := state.IsRepo(ctx, workdir); err != nil || !ok {
		return false
	}
	ok, err := state.ExistsAtHead(ctx, workdir, state.AnchorDir+"/"+state.MissionFile)
	return err == nil && ok
}

// OrchestratorOptions configure an Orchestrator (the Python constructor's keyword arguments).
type OrchestratorOptions struct {
	// ResearchPerItem is how many research queries each item gets (0 disables research; the
	// Python default is 2 — use DefaultOrchestratorOptions).
	ResearchPerItem int
	// DoReview runs the Reviewer on every verified item.
	DoReview bool
	// Meter shares one budget/ledger with other callers (e.g. the Planner); nil => a fresh one.
	Meter *governor.CostMeter
	// Models are per-role provider overrides ("lead" / "researcher" / "reviewer" /
	// "implementer"); they are metered here either way and not closed.
	Models map[string]contracts.ModelProvider
	// MaxParallel is the items per parallel wave (nil => settings.MaxParallelImplementers; below
	// 2 disables parallel waves).
	MaxParallel *int
	// Services opens the run's persistence (nil => DefaultServices).
	Services ServicesOpener
}

// DefaultOrchestratorOptions are the Python defaults (research_per_item=2, do_review=True).
func DefaultOrchestratorOptions() OrchestratorOptions {
	return OrchestratorOptions{ResearchPerItem: 2, DoReview: true}
}

// Orchestrator runs a mission with the full org (research + Lead or parallel implementers +
// review).
type Orchestrator struct {
	settings *config.Settings
	opts     OrchestratorOptions
}

// NewOrchestrator returns an orchestrator (settings nil => config.Load() at run time).
func NewOrchestrator(settings *config.Settings, opts OrchestratorOptions) *Orchestrator {
	return &Orchestrator{settings: settings, opts: opts}
}

// MissionOptions are RunMission's inputs.
type MissionOptions struct {
	Workdir     string
	Title       string
	Description string
	// Checklist is required unless Resume.
	Checklist *contracts.Checklist
	// Checks are the gating checks (nil => verify.DefaultPythonChecks()).
	Checks []contracts.Check
	// AllowEgress: nil => the Lead and the Researchers get the web tools iff LHA_WEB_ALLOW_HOSTS
	// is set (the Reviewer's role hides them); false drops them.
	AllowEgress *bool
	// Gate is where irreversible commands go for a human decision (nil denies them).
	Gate contracts.HITLGate
	// References are vendored reference paths recited every cycle.
	References []string
	// Ownership is the Planner's file-ownership map (persisted in the anchor; nil => none).
	Ownership *coordination.FileOwnershipMap
	// Resume continues the mission anchored in Workdir (Title / Description / Checklist /
	// References / Ownership are then ignored); a *MissionResumeError without an anchor.
	Resume bool
}

// RunMission runs the org until complete / deadlocked / over-budget / looping. A budget refusal
// or an altered decision history ends the run with a StoppedReason (not an error).
func (o *Orchestrator) RunMission(ctx context.Context, m MissionOptions) (agent.MissionSummary, error) {
	settings := o.settings
	if settings == nil {
		loaded, err := config.Load()
		if err != nil {
			return agent.MissionSummary{}, err
		}
		settings = loaded
	}
	if err := tools.PreflightRunTools(settings); err != nil {
		return agent.MissionSummary{}, err
	}
	if m.Resume && !AnchorExists(ctx, m.Workdir) {
		return agent.MissionSummary{}, &MissionResumeError{"no mission anchor to resume at " + contracts.PyRepr(m.Workdir)}
	}
	if !m.Resume && m.Checklist == nil {
		return agent.MissionSummary{}, errors.New("a checklist is required unless resuming")
	}
	checks := m.Checks
	if checks == nil {
		checks = verify.DefaultPythonChecks()
	}
	trusted, err := settings.TrustedCheckCommands()
	if err != nil {
		return agent.MissionSummary{}, err
	}
	maxParallel := settings.MaxParallelImplementers
	if o.opts.MaxParallel != nil {
		maxParallel = *o.opts.MaxParallel
	}
	meter := o.opts.Meter
	if meter == nil {
		meter = agent.BuildMeter(settings)
	}
	run := &missionRun{
		org: o, settings: settings, workdir: m.Workdir, checks: checks, allowEgress: m.AllowEgress,
		gate: m.Gate, trusted: trusted, maxParallel: maxParallel,
		recorder: obs.NewTraceRecorder(nil), meter: meter,
		loopDetector: governor.NewLoopDetector(settings.StallLimit),
		missionID:    agent.NewID("mission"), board: &coordination.Blackboard{},
		reflections: map[string]string{}, runNumber: 1, stopped: "max_cycles",
	}
	return run.execute(ctx, m)
}

// missionRun is the state of one RunMission call.
type missionRun struct {
	org          *Orchestrator
	settings     *config.Settings
	workdir      string
	checks       []contracts.Check
	allowEgress  *bool
	gate         contracts.HITLGate
	trusted      map[string][]string
	maxParallel  int
	recorder     *obs.TraceRecorder
	meter        *governor.CostMeter
	loopDetector *governor.LoopDetector
	missionID    string
	board        *coordination.Blackboard
	reflections  map[string]string
	cycles       int
	cycleOffset  int // cycle ids continue after the last committed one when resumed
	runNumber    int
	lastHead     string
	stopped      string

	anchor           *state.GitMissionAnchor
	session          contracts.SandboxSession
	leadModel        contracts.ModelProvider
	researchModel    contracts.ModelProvider
	reviewModel      contracts.ModelProvider
	reflectionModel  contracts.ModelProvider
	implementerModel contracts.ModelProvider
	readTools        contracts.ToolDispatcher
	leadGuard        *coordination.OwnershipGuard
	lead             *agent.AgentLoop
	reviewer         *Reviewer
	integrator       *BranchIntegrator
	leases           *coordination.LeaseBroker
	tctx             contracts.ToolContext
}

func (r *missionRun) cycleID(n int) string { return "c" + strconv.Itoa(r.cycleOffset+n) }

func (r *missionRun) record(kind string, data ...obs.Field) {
	r.recorder.Record(kind, r.missionID, "", data...)
}

func (r *missionRun) execute(ctx context.Context, m MissionOptions) (agent.MissionSummary, error) {
	settings := r.settings
	title, description := m.Title, m.Description
	r.anchor = state.NewGitMissionAnchor(r.workdir)
	if m.Resume {
		if err := r.recoverWorkspace(ctx); err != nil {
			return agent.MissionSummary{}, err
		}
		var err error
		if title, description, err = r.restoreRunState(ctx, title, description); err != nil {
			return agent.MissionSummary{}, err
		}
	} else {
		var ownershipJSON []byte
		if m.Ownership != nil {
			data, err := coordination.OwnershipJSON(m.Ownership)
			if err != nil {
				return agent.MissionSummary{}, err
			}
			ownershipJSON = data
		}
		spec := contracts.MissionSpec{Title: title, Description: description, References: m.References}
		if _, err := r.anchor.InitializeSpecWithOwnership(ctx, spec, *m.Checklist, ownershipJSON); err != nil {
			return agent.MissionSummary{}, err
		}
	}
	if err := PruneWorktrees(ctx, r.workdir); err != nil {
		return agent.MissionSummary{}, err
	}
	if err := r.anchor.AppendEvent(ctx, contracts.EventRecord{Kind: RunEvent, Payload: contracts.Payload(
		"mission_id", r.missionID, "resumed", m.Resume, "run", r.runNumber,
	)}); err != nil {
		return agent.MissionSummary{}, err
	}
	session, err := OpenLeadSandbox(ctx, settings, r.workdir)
	if err != nil {
		return agent.MissionSummary{}, err
	}
	r.session = session
	defer session.Close(context.WithoutCancel(ctx))

	// One shared HTTP pool for every role's provider, closed when the mission ends.
	client := &http.Client{Timeout: 300 * time.Second}
	defer client.CloseIdleConnections()
	raw := map[string]contracts.ModelProvider{}
	for _, role := range orgRoles {
		if p := r.org.opts.Models[role]; p != nil {
			raw[role] = p
			continue
		}
		built, err := agents.ModelForRole(role, settings, client)
		if err != nil {
			return agent.MissionSummary{}, err
		}
		defer agent.CloseProvider(context.WithoutCancel(ctx), built)
		raw[role] = built
	}
	meter := r.meter
	r.leadModel = meter.Wrap(raw["lead"], "lead")
	r.researchModel = meter.Wrap(raw["researcher"], "researcher")
	r.reviewModel = meter.Wrap(raw["reviewer"], "reviewer")
	r.reflectionModel = meter.Wrap(raw["lead"], "reflection")
	r.implementerModel = meter.Wrap(raw["implementer"], "implementer")

	// Researchers get the web tools with the lead (the Reviewer's role hides them).
	if r.readTools, err = RunDispatcher(settings, false, nil, r.allowEgress); err != nil {
		return agent.MissionSummary{}, err
	}
	keyPrefix := ""
	if r.runNumber > 1 {
		keyPrefix = fmt.Sprintf("run%d", r.runNumber)
	}
	opener := r.org.opts.Services
	if opener == nil {
		opener = DefaultServices
	}
	systemOne, err := systemone.Build(settings, meter)
	if err != nil {
		return agent.MissionSummary{}, err
	}
	defer func() { _ = systemone.Close(systemOne) }()
	services, err := opener(ctx, ServicesRequest{
		Settings: settings, MissionID: r.missionID, Workdir: r.workdir, Meter: meter,
		Title: title, Description: description, Model: meter.Wrap(raw["lead"], "librarian"),
		Recorder: r.recorder, KeyPrefix: keyPrefix, Gate: r.gate, SystemOne: systemOne,
	})
	if err != nil {
		return agent.MissionSummary{}, err
	}
	defer services.Close(context.WithoutCancel(ctx))
	if err := services.Running(ctx); err != nil {
		return agent.MissionSummary{}, err
	}
	// The Lead writes through an ownership guard: unassigned space, shared files and the active
	// item's own files — never another open item's leased files.
	leadDispatcher, err := LeadDispatcher(settings, r.gate, r.allowEgress)
	if err != nil {
		return agent.MissionSummary{}, err
	}
	r.leadGuard = coordination.NewOwnershipGuard(leadDispatcher, coordination.NewFileOwnershipMap(), []string{coordination.Lead}, false)
	if r.lead, err = agent.BuildLeadLoop(settings, r.leadModel, r.anchor, tools.WithDecisionTool(r.leadGuard, r.anchor), r.recorder); err != nil {
		return agent.MissionSummary{}, err
	}
	r.lead.SetMemory(services.Memory())
	r.lead.SetTriage(systemone.BuildStallTriage(settings, systemOne))
	r.reviewer = NewReviewer(r.reviewModel, r.readTools)
	// Integration is gated exactly like a Lead cycle would be for the same item: the mission
	// checks plus the item's witnesses, on the lead verifier.
	r.integrator = &BranchIntegrator{Workdir: r.workdir, Session: session, Verifier: LeadVerifier(r.workdir, settings), Checks: r.checks}
	// Leases are decided against this run's own anchor instance, so a grant and the ownership
	// release staged by currentOwnership are committed together.
	r.leases = coordination.NewLeaseBroker(r.anchor)
	r.tctx = contracts.ToolContext{MissionID: r.missionID, Session: session}

	loopErr := r.loop(ctx, title, description)
	_ = r.integrator.Abort(ctx)
	_ = PruneWorktrees(ctx, r.workdir)
	if loopErr != nil {
		var budget *governor.BudgetExceeded
		var chain *state.DecisionChainError
		switch {
		case errors.As(loopErr, &budget):
			r.record("governor_block", obs.F("reason", loopErr.Error()))
			r.stopped = "governor: " + budget.Decision.Reason
		case errors.As(loopErr, &chain): // altered decision history: refuse to continue
			r.record("decision_chain_invalid", obs.F("reason", loopErr.Error()))
			r.stopped = agent.DecisionChainStop + ": " + loopErr.Error()
		default:
			_ = services.Finish(context.WithoutCancel(ctx), "error: "+ExcTypeName(loopErr), r.lastHead)
			return agent.MissionSummary{}, loopErr
		}
	}
	final, err := r.anchor.ReadChecklist(ctx)
	if err != nil {
		return agent.MissionSummary{}, err
	}
	if err := services.Finish(ctx, r.stopped, r.lastHead); err != nil {
		return agent.MissionSummary{}, err
	}
	trace, err := r.recorder.ToJSONL()
	if err != nil {
		return agent.MissionSummary{}, err
	}
	return agent.MissionSummary{
		MissionID:     r.missionID,
		Completed:     final.IsComplete(),
		Cycles:        r.cycles,
		ItemsDone:     final.ItemsDone(),
		ItemsTotal:    final.ItemsTotal(),
		TotalUSD:      meter.Ledger.TotalUSD(),
		HeadSHA:       r.lastHead,
		StoppedReason: r.stopped,
		TraceJSONL:    trace,
	}, nil
}

// --- resume ------------------------------------------------------------------------------------

// recoverWorkspace discards what an interrupted run left uncommitted (never anything committed).
func (r *missionRun) recoverWorkspace(ctx context.Context) error {
	if err := AbortMerge(ctx, r.workdir); err != nil {
		return err
	}
	if err := state.DiscardChanges(ctx, r.workdir); err != nil {
		return err
	}
	head, err := state.HeadSHA(ctx, r.workdir)
	r.lastHead = head
	return err
}

// restoreRunState rebuilds the mission id, board, reflections and cycle numbering from committed
// events; it returns the committed mission's title and description (the arguments are
// fallbacks).
func (r *missionRun) restoreRunState(ctx context.Context, title, description string) (string, string, error) {
	// The decision chain must still verify before anything builds on it.
	if _, err := r.anchor.ReadDecisions(ctx); err != nil {
		return "", "", err
	}
	spec, err := r.anchor.ReadMission(ctx)
	if err != nil {
		return "", "", err
	}
	events, err := r.anchor.ReadEvents(ctx)
	if err != nil {
		return "", "", err
	}
	checklist, err := r.anchor.ReadChecklist(ctx)
	if err != nil {
		return "", "", err
	}
	runs := 0
	for _, e := range events {
		if e.Kind != RunEvent {
			continue
		}
		runs++
		if id := payloadStr(e.Payload, "mission_id"); id != "" {
			r.missionID = id
		}
	}
	r.runNumber = runs + 1
	for _, e := range events {
		if m := cycleIDRE.FindStringSubmatch(e.CycleID); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > r.cycleOffset {
				r.cycleOffset = n
			}
		}
	}
	for _, e := range events {
		if e.Kind == BoardEvent {
			r.board.Post(payloadStr(e.Payload, "author"), payloadStr(e.Payload, "text"))
		}
	}
	open := map[string]bool{}
	for _, item := range checklist.Items {
		if item.IsOpen() {
			open[item.ID] = true
		}
	}
	for _, e := range events {
		if item := payloadStr(e.Payload, "item"); e.Kind == ReflectionEvent && open[item] {
			r.reflections[item] = payloadStr(e.Payload, "text")
		}
	}
	r.record("resumed", obs.F("run", r.runNumber), obs.F("cycle_offset", r.cycleOffset),
		obs.F("board", len(r.board.Read())))
	if spec == nil {
		return title, description, nil
	}
	return spec.Title, spec.Description, nil
}

// payloadStr is str(payload.get(key, "")) for the string payloads the org writes.
func payloadStr(payload *contracts.OrderedMap, key string) string {
	v, ok := payload.Get(key)
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyfmt.PyStr(v)
}

// post writes to this round's response board, and records it for a later resume.
func (r *missionRun) post(ctx context.Context, author, content string) error {
	r.board.Respond(author, content)
	return r.anchor.AppendEvent(ctx, contracts.EventRecord{Kind: BoardEvent, Payload: contracts.Payload(
		"author", author, "text", pyfmt.Head(content, boardEntryCap),
	)})
}

// --- the mission loop --------------------------------------------------------------------------

func (r *missionRun) loop(ctx context.Context, title, description string) error {
	for r.cycles < r.settings.MaxCycles {
		decision := r.meter.Governor.AuthorizeNext(r.meter.Ledger, r.cycles, nil)
		if !decision.Allow {
			r.record("governor_block", obs.F("reason", decision.Reason))
			r.stopped = "governor: " + decision.Reason
			return nil
		}
		snapshot, err := r.anchor.ReadSituationalAwareness(ctx)
		if err != nil {
			return err
		}
		if snapshot.IsComplete {
			r.stopped = "complete"
			return nil
		}
		if snapshot.IsDeadlocked || snapshot.ActiveItem == nil {
			r.stopped = "deadlocked: " + orDefault(snapshot.DeadlockReason, "no actionable item")
			return nil
		}
		checklist, err := r.anchor.ReadChecklist(ctx)
		if err != nil {
			return err
		}
		ownership, err := r.currentOwnership(ctx, checklist)
		if err != nil {
			return err
		}
		var keepGoing bool
		if batch := ParallelBatch(checklist, ownership, r.maxParallel); len(batch) > 0 {
			keepGoing, err = r.parallelWave(ctx, snapshot, batch, ownership)
		} else {
			keepGoing, err = r.serialCycle(ctx, snapshot, *snapshot.ActiveItem, ownership, title, description)
		}
		if err != nil {
			return err
		}
		r.board.CommitRound()
		if !keepGoing {
			return nil
		}
	}
	return nil
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// currentOwnership is the committed map with finished items' files released (staged for the
// next commit).
func (r *missionRun) currentOwnership(ctx context.Context, checklist contracts.Checklist) (*coordination.FileOwnershipMap, error) {
	persisted, err := coordination.ReadOwnership(ctx, r.anchor)
	if err != nil {
		return nil, err
	}
	effective := coordination.EffectiveOwnership(persisted, doneWriters(checklist))
	if !effective.Equal(persisted) {
		if err := coordination.StageOwnership(r.anchor, effective); err != nil {
			return nil, err
		}
		kept := effective.Snapshot()
		released := []string{}
		for path := range persisted.Snapshot() {
			if _, ok := kept[path]; !ok {
				released = append(released, path)
			}
		}
		sort.Strings(released)
		r.record("ownership_released", obs.F("paths", released))
	}
	return effective, nil
}

// --- context shared by both round kinds -------------------------------------------------------

func (r *missionRun) boardContext() string {
	entries := r.board.Read()
	if len(entries) > boardEntries {
		entries = entries[len(entries)-boardEntries:]
	}
	if len(entries) == 0 {
		return ""
	}
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = "[" + e.Author + "] " + pyfmt.Head(e.Content, boardEntryCap)
	}
	return "Team board (earlier rounds):\n" + strings.Join(lines, "\n---\n")
}

// research fans out every item's research queries together; it returns the briefs per item.
func (r *missionRun) research(ctx context.Context, items []contracts.ChecklistItem) (map[string][]string, error) {
	perItem := r.org.opts.ResearchPerItem
	briefs := map[string][]string{}
	if perItem <= 0 {
		return briefs, nil
	}
	type query struct{ itemID, text string }
	queries := []query{}
	for _, item := range items {
		templates := []string{
			"Find context relevant to: " + item.Description,
			"Find existing files/code related to: " + item.Description,
		}
		for _, q := range templates[:min(perItem, len(templates))] {
			queries = append(queries, query{item.ID, q})
		}
	}
	texts := make([]string, len(queries))
	for i, q := range queries {
		texts[i] = q.text
	}
	results, err := ResearchFanout(ctx, r.researchModel, r.readTools, r.tctx, texts, nil)
	if err != nil {
		return nil, err
	}
	failed := map[string][]string{}
	for _, item := range items {
		briefs[item.ID] = []string{}
	}
	for i, result := range results {
		itemID := queries[i].itemID
		switch {
		case result.Error != "":
			failed[itemID] = append(failed[itemID], result.Error)
		case result.Brief != "":
			briefs[itemID] = append(briefs[itemID], result.Brief)
			if err := r.post(ctx, "researcher:"+itemID, result.Brief); err != nil {
				return nil, err
			}
		}
	}
	for _, item := range items {
		r.record("research", obs.F("item", item.ID), obs.F("n", len(briefs[item.ID])), obs.F("failed", len(failed[item.ID])))
		for _, e := range failed[item.ID] {
			r.record("research_failed", obs.F("item", item.ID), obs.F("error", e))
		}
	}
	return briefs, nil
}

// --- serial round: the Lead ---------------------------------------------------------------------

func (r *missionRun) serialCycle(ctx context.Context, snapshot contracts.SituationSnapshot, item contracts.ChecklistItem, ownership *coordination.FileOwnershipMap, title, description string) (bool, error) {
	cycleID := r.cycleID(r.cycles + 1)
	r.meter.SetCycleID(cycleID)
	allBriefs, err := r.research(ctx, []contracts.ChecklistItem{item})
	if err != nil {
		return false, err
	}
	briefs := allBriefs[item.ID]

	// The anchor comes from the immutable mission spec, never from progress.md.
	missionText := "Mission: " + title + "\n\n" + description
	if snapshot.Mission != nil {
		missionText = snapshot.Mission.RenderAnchor()
	}
	joined := strings.Join(briefs, "\n---\n")
	if joined == "" {
		joined = "(none)"
	}
	anchorText := missionText + "\n" + r.reflections[item.ID] + "\n\nResearch briefs:\n" + joined
	if board := r.boardContext(); board != "" {
		anchorText += "\n\n" + board
	}

	writers := []string{coordination.Lead, coordination.WriterForItem(item.ID)}
	r.leadGuard.Update(ownership, writers)
	outcome, err := r.lead.RunCycle(ctx, r.tctx, r.missionID, cycleID, anchorText, r.checks)
	if err != nil {
		return false, err
	}
	if !outcome.Advanced { // nothing actionable after all
		r.stopped = "deadlocked: " + orDefault(outcome.Reason, "no actionable item")
		return false, nil
	}
	r.cycles++
	if outcome.HeadSHA != "" {
		r.lastHead = outcome.HeadSHA
	}
	if err := r.auditLeadWrites(ctx, ownership, writers, snapshot.HeadSHA, outcome.HeadSHA); err != nil {
		return false, err
	}

	if !outcome.Verified {
		if outcome.IsDeadlocked {
			r.record("deadlocked", obs.F("reason", outcome.Reason))
			r.stopped = "deadlocked: " + orDefault(outcome.Reason, "no actionable item")
			return false, nil
		}
		if r.loopDetector.Observe(item.ID+":failed", true) {
			r.stopped = "loop on item " + item.ID
			return false, nil
		}
		if outcome.ItemBlocked || outcome.ItemSplit { // not re-picked; nothing to reflect on
			return true, nil
		}
		err := r.reflect(ctx, item, fmt.Sprintf("%s failed verification (%s) after %d turns: %s",
			item.ID, outcome.Verdict, outcome.Turns, outcome.Reason))
		return err == nil, err
	}
	r.loopDetector.Observe(item.ID+":failed", false)

	review, err := r.review(ctx, item, cycleID, snapshot.HeadSHA, outcome.HeadSHA)
	if err != nil || review != "approved" {
		return review == "reopened", err // "looping" stops the mission
	}
	if outcome.IsComplete {
		r.stopped = "complete"
		return false, nil
	}
	return true, nil
}

// auditLeadWrites is the git-layer check of the Lead's cycle: it records writes to other items'
// leased files (detection only: in a serial round no other writer is active).
func (r *missionRun) auditLeadWrites(ctx context.Context, ownership *coordination.FileOwnershipMap, writers []string, base, head string) error {
	paths, err := coordination.ChangedPaths(ctx, r.workdir, base, head)
	if err != nil {
		return err
	}
	if violations := ownership.ViolationsAny(writers, paths); len(violations) > 0 {
		shown := make([]string, len(violations))
		for i, v := range violations {
			shown[i] = v.Path
		}
		r.record("ownership_violation", obs.F("writer", strings.Join(writers, "+")), obs.F("paths", shown))
	}
	return nil
}

func (r *missionRun) reflect(ctx context.Context, item contracts.ChecklistItem, failureSummary string) error {
	reflection, err := agents.ReflectOnFailure(ctx, r.reflectionModel, item.Description, failureSummary)
	if err != nil {
		return err
	}
	if err := r.setReflection(ctx, item.ID, "\nReflection on "+item.ID+": "+reflection+"\n"); err != nil {
		return err
	}
	r.record("reflection", obs.F("item", item.ID))
	return nil
}

// setReflection remembers text for the item's next attempt (recorded for a later resume).
func (r *missionRun) setReflection(ctx context.Context, itemID, text string) error {
	r.reflections[itemID] = text
	return r.anchor.AppendEvent(ctx, contracts.EventRecord{Kind: ReflectionEvent, Payload: contracts.Payload(
		"item", itemID, "text", pyfmt.Head(text, 2_000),
	)})
}

// review is the independent review of a verified item's base..head diff: "approved",
// "reopened" (a blocking verdict put the item back to todo) or "looping" (the review keeps
// blocking; stopped is set).
func (r *missionRun) review(ctx context.Context, item contracts.ChecklistItem, cycleID, base, head string) (string, error) {
	if !r.org.opts.DoReview {
		return "approved", nil
	}
	diff := DiffSince(ctx, r.workdir, base, head)
	review, err := r.reviewer.Review(ctx, pyfmt.Head(diff, reviewDiffCap), item.Description, r.tctx)
	if err != nil {
		return "", err
	}
	r.record("review", obs.F("item", item.ID), obs.F("blocking", review.Blocking), obs.F("verdict", review.Verdict))
	if !review.Blocking {
		r.loopDetector.Observe(item.ID+":review_blocked", false)
		return "approved", nil
	}
	notes := review.Notes()
	if err := r.setReflection(ctx, item.ID, "\n"+notes+"\n"); err != nil {
		return "", err
	}
	if err := r.post(ctx, "reviewer:"+item.ID, notes); err != nil {
		return "", err
	}
	head, err = r.reopen(ctx, item.ID, cycleID, review)
	if err != nil {
		return "", err
	}
	if head != "" {
		r.lastHead = head
	}
	r.record("review_reopened", obs.F("item", item.ID))
	if r.loopDetector.Observe(item.ID+":review_blocked", true) {
		r.stopped = "loop on item " + item.ID + " (review keeps blocking)"
		return "looping", nil
	}
	return "reopened", nil
}

// reopen puts a verified-but-review-blocked item back to todo with the review notes.
func (r *missionRun) reopen(ctx context.Context, itemID, cycleID string, review agents.ReviewResult) (string, error) {
	checklist, err := r.anchor.ReadChecklist(ctx)
	if err != nil {
		return "", err
	}
	if ReopenForReview(&checklist, itemID, review, false) == nil {
		return "", nil
	}
	return r.anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID:         cycleID,
		ProgressSummary: cycleID + ": review reopened " + itemID + " (" + review.Verdict + ")",
		Checklist:       checklist,
		Decisions:       []contracts.DecisionRecord{},
		Events:          []contracts.EventRecord{},
		CommitMessage:   "lha: review reopened " + itemID,
	})
}

// --- parallel round: implementers + integrator --------------------------------------------------

func (r *missionRun) parallelWave(ctx context.Context, snapshot contracts.SituationSnapshot, batch []contracts.ChecklistItem, ownership *coordination.FileOwnershipMap) (bool, error) {
	base := snapshot.HeadSHA
	first := r.cycles + 1
	r.meter.SetCycleID(r.cycleID(first))
	ids := make([]string, len(batch))
	for i, item := range batch {
		ids[i] = item.ID
	}
	r.record("parallel_wave", obs.F("items", ids), obs.F("base", base))
	briefs, err := r.research(ctx, batch)
	if err != nil {
		return false, err
	}
	runs := make([]*ImplementerRun, len(batch))
	acceptance := make([]string, len(r.checks))
	for i, c := range r.checks {
		acceptance[i] = c.Name
	}
	for n, item := range batch {
		runs[n] = NewImplementerRun(item, r.cycleID(first+n), ownership, r.settings.MaxTurnsPerCycle, acceptance)
	}
	defer func() {
		for _, run := range runs {
			if run.Worktree != "" {
				RemoveWorktree(ctx, r.workdir, run.Worktree, run.Branch)
			}
		}
	}()
	fns := make([]func() error, len(runs))
	for i, run := range runs {
		fns[i] = func() error { return r.implement(ctx, run, base, ownership, snapshot, briefs[run.Item.ID]) }
	}
	for i, err := range runGroup(fns) {
		if err == nil {
			continue
		}
		var budget *governor.BudgetExceeded
		if errors.As(err, &budget) || errors.Is(err, context.Canceled) {
			return false, err // cancellation / budget: stop, don't paper over it
		}
		runs[i].Error = ErrorSummary(err)
	}
	for _, run := range runs {
		r.cycles++
		keep, err := r.integrate(ctx, run, snapshot)
		if err != nil {
			return false, err
		}
		if !keep {
			return false, nil
		}
	}
	return true, nil
}

// implement runs one implementer in its own worktree; verifies there; commits on its branch.
func (r *missionRun) implement(ctx context.Context, run *ImplementerRun, base string, ownership *coordination.FileOwnershipMap, snapshot contracts.SituationSnapshot, briefs []string) error {
	missionText := ""
	if snapshot.Mission != nil {
		missionText = snapshot.Mission.RenderAnchor()
	}
	objective, extra := ImplementerObjective(run, ObjectiveInput{
		MissionText:   missionText,
		Reflection:    r.reflections[run.Item.ID],
		DecisionsText: agent.RenderDecisions(snapshot.LastDecisions),
		Briefs:        briefs,
		Board:         r.boardContext(),
	})
	summaries := []string{}
	if err := ImplementInWorktree(ctx, run, ImplementOptions{
		Settings: r.settings, Workdir: r.workdir, Base: base, Ownership: ownership,
		Model: r.implementerModel, Gate: r.gate, AllowEgress: r.allowEgress, MissionID: r.missionID,
		MissionChecks: r.checks, Objective: objective, Extra: extra,
		Lease:     coordination.NewLeaseHandler(r.leases, run.Writer, run.CycleID, ownership, run.Leases),
		OnSummary: func(s string) { summaries = append(summaries, s) },
	}); err != nil {
		return err
	}
	for _, d := range run.Leases.Decisions() {
		r.record("lease", obs.F("writer", d.Writer), obs.F("path", d.Path), obs.F("granted", d.Granted), obs.F("why", d.Why))
	}
	for _, summary := range summaries {
		if err := r.post(ctx, run.Writer, "["+run.Item.ID+"] "+summary); err != nil {
			return err
		}
	}
	return nil
}

// integrate offers one implementer branch to the integrator and checkpoints the result.
func (r *missionRun) integrate(ctx context.Context, run *ImplementerRun, snapshot contracts.SituationSnapshot) (bool, error) {
	item := run.Item
	missionText := ""
	if snapshot.Mission != nil {
		missionText = snapshot.Mission.RenderAnchor()
	}
	checks, _ := ItemChecks(item, r.checks, r.trusted)
	report, err := IntegrateRun(ctx, run, IntegrateOptions{
		Anchor: r.anchor, Integrator: r.integrator, Workdir: r.workdir, Base: snapshot.HeadSHA,
		Checks: checks,
		Split: func(ctx context.Context, checklist *contracts.Checklist, current contracts.ChecklistItem) ([]string, error) {
			return MaybeSplit(ctx, checklist, current, r.settings, r.leadModel, missionText)
		},
	})
	if err != nil {
		return false, err
	}
	if report.Head != "" {
		r.lastHead = report.Head
	}
	r.record("integration", obs.F("item", item.ID), obs.F("merged", report.Merged), obs.F("branch", run.Branch),
		obs.F("reason", pyfmt.Head(report.Reason, 500)))

	if !report.Merged {
		if report.Checklist.IsDeadlocked() {
			r.stopped = "deadlocked: " + report.Checklist.DeadlockReason()
			return false, nil
		}
		if r.loopDetector.Observe(item.ID+":failed", true) {
			r.stopped = "loop on item " + item.ID
			return false, nil
		}
		if report.Status != contracts.StatusBlocked && report.Status != contracts.StatusSplit {
			if err := r.reflect(ctx, item, item.ID+" was not integrated: "+report.Reason); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	r.loopDetector.Observe(item.ID+":failed", false)
	review, err := r.review(ctx, item, run.CycleID, report.Before, report.Head)
	if err != nil || review != "approved" {
		return review == "reopened", err
	}
	if report.Checklist.IsComplete() {
		r.stopped = "complete"
		return false, nil
	}
	return true, nil
}
