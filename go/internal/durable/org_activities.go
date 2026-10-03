package durable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents/org"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// Activities of the durable multi-agent organization (python: lha.durable.org_activities;
// research runs as SubAgentWorkflow children, see run_subagent below). MissionWorkflow uses them
// when a mission opts in (MissionInput.ResearchPerItem / Review / MaxParallel; org_round.go):
//
//   - plan_round — read-only: the committed checklist and ownership map decide whether the next
//     round is a parallel wave (items that each own a disjoint write-set) or a serial Lead cycle.
//     It also removes worktrees, branches and cached results an interrupted wave left behind.
//   - run_implementer — one parallel implementer (org.ImplementInWorktree) in its own git worktree
//     on its own branch lha/implementer-<item>/<cycle>. Heartbeats while it runs. Retry-safe: an
//     exclusive per-cycle lock, a fresh worktree from the round's base on every attempt, and a
//     result cached under .git/lha/implementers/ once the branch is committed, so a retry after a
//     crash returns the same branch instead of redoing the work. Leases (request_lease) are
//     decided and committed through the LeaseBroker.
//   - integrate_branch — the BranchIntegrator merges one implementer branch (verified, owned per
//     the committed map including leases, conflict-free, re-verified on the merged checkout with
//     the Lead's verifier, flaky retries included) and commits the checkpoint: the integration
//     commit IS the checkpoint. Exactly once per cycle id (a retry finds the committed cycle
//     event). A failed implementer or a refused branch is a failed attempt (blocked after 3,
//     then split by the replanner).
//   - review_cycle — an independent Reviewer (fresh context, read-only tools) reviews one verified
//     item's diff and commits its verdict as a review event; a blocking verdict reopens the item,
//     and the third blocking review in a row blocks it instead, so a human decides at the
//     deadlock gate. Exactly once per reviewed cycle.
//
// Every activity that changes the mission checkout takes the checkout's lock and starts from a
// clean HEAD. Spend is metered against the mission budget (seeded with the recorded spend),
// appended to the spend journal and written to the cost ledger call by call, like a cycle's.

// ReviewEvent is the event kind of a committed review verdict.
const ReviewEvent = agents.ReviewEvent

const (
	implementerCacheDir = "lha/implementers"
	reviewDiffCap       = 20_000
)

var unsafeIDChars = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func safeID(s string) string { return unsafeIDChars.ReplaceAllString(s, "_") }

// budgetError is the non-retryable ApplicationError of a budget refusal.
func budgetError(err *governor.BudgetExceeded) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), ErrorBudgetExceeded, nil)
}

// asBudget maps a budget refusal (anywhere in err's chain) to its ApplicationError.
func asBudget(err error) error {
	var budget *governor.BudgetExceeded
	if errors.As(err, &budget) {
		return budgetError(budget)
	}
	return err
}

// roleModel builds an organization role's model: factory when set (tests), else the role's model
// (python: lha.agents.router.model_for_role).
func roleModel(factory ModelFactory, role string, settings *config.Settings, snapshot contracts.SituationSnapshot) (contracts.ModelProvider, error) {
	if factory != nil {
		return factory(settings, snapshot)
	}
	return agents.ModelForRole(role, settings, nil)
}

// attachLedger hooks the meter to the persistent cost ledger with python's key prefix
// ("<prefix>:<workflow id>:<activity id>@<attempt>"); the returned func detaches and closes it.
// attachLedger hooks meter to the persistent cost ledger and returns a recorder whose events go to
// the shared event record (mission_events; python: _attach_ledger); detach closes both.
func (a *Activities) attachLedger(ctx context.Context, settings *config.Settings, workdir, missionID string, meter *governor.CostMeter, prefix string) (func(), *obs.TraceRecorder, error) {
	store, err := a.openStore(ctx, settings, workdir)
	if err != nil {
		if errors.Is(err, ErrStoreUnavailable) {
			return nil, nil, configError(fmt.Sprintf("cannot open the mission store: %v", err), err)
		}
		return nil, nil, err
	}
	recorder, closeEvents := recorderFor(store)
	if activity.IsActivity(ctx) {
		info := activity.GetInfo(ctx)
		prefix = fmt.Sprintf("%s:%s:%s@%d", prefix, info.WorkflowExecution.ID, info.ActivityID, info.Attempt)
	}
	meter.SetHook(newLedgerHook(store, missionID, prefix))
	return func() {
		meter.SetHook(nil)
		closeEvents() // before the store closes; best effort, never fails
		_ = store.Close(context.Background())
	}, recorder, nil
}

// recorderFor is a recorder whose events go to store's shared event record when store is backed
// by the mission store (a test fake is not), with the function that flushes and stops it.
func recorderFor(store Store) (*obs.TraceRecorder, func()) {
	recorder := obs.NewTraceRecorder(nil)
	backed, ok := store.(persistenceBacked)
	if !ok {
		return recorder, func() {}
	}
	events := persistence.NewMissionEventLog(backed.Persistence())
	recorder.AddListener(events.Add)
	events.Start()
	return recorder, func() { events.Close(context.Background()) }
}

func spentOn(meter *governor.CostMeter, cycleID string) float64 {
	var spent float64
	for _, e := range meter.Ledger.Entries() {
		if e.CycleID == cycleID {
			spent += e.USD
		}
	}
	return spent
}

func checkNames(checks []contracts.Check) []string {
	names := make([]string, len(checks))
	for i, c := range checks {
		names[i] = c.Name
	}
	return names
}

// --- plan_round -----------------------------------------------------------------------------

func implementerCache(ctx context.Context, workdir string) (string, error) {
	gitDir, err := state.GitDir(ctx, workdir)
	if err != nil {
		return "", err
	}
	return filepath.Join(gitDir, filepath.FromSlash(implementerCacheDir)), nil
}

// cleanLeftovers removes the worktrees, implementer branches and cached results of an
// interrupted wave.
func cleanLeftovers(ctx context.Context, workdir string) error {
	if err := org.PruneWorktrees(ctx, workdir); err != nil {
		return err
	}
	cache, err := implementerCache(ctx, workdir)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		return nil // no cache yet
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(cache, e.Name()))
	}
	return nil
}

// PlanRound is plan_round: decide the next round (a parallel wave or a serial Lead cycle) from
// the committed truth.
func (a *Activities) PlanRound(ctx context.Context, inp RoundInput) (RoundPlan, error) {
	release, err := lockWorkdir(ctx, inp.Workdir, LockWait)
	if err != nil {
		return RoundPlan{}, err
	}
	err = cleanLeftovers(ctx, inp.Workdir)
	release()
	if err != nil {
		return RoundPlan{}, err
	}
	anchor := state.NewGitMissionAnchor(inp.Workdir)
	snapshot, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return RoundPlan{}, err
	}
	checklist, err := anchor.ReadChecklist(ctx)
	if err != nil {
		return RoundPlan{}, err
	}
	persisted, err := coordination.ReadOwnership(ctx, anchor)
	if err != nil {
		return RoundPlan{}, err
	}
	ownership := coordination.EffectiveOwnership(persisted, coordination.FinishedWriters(checklist))
	batch := org.ParallelBatch(checklist, ownership, inp.MaxParallel)
	items := batch
	if len(batch) == 0 {
		items = nil
		if next := checklist.NextActionable(); next != nil {
			items = []contracts.ChecklistItem{*next}
		}
	}
	plan := RoundPlan{
		HeadSHA: snapshot.HeadSHA, Items: []RoundItem{}, Parallel: len(batch) > 0,
		IsComplete: snapshot.IsComplete, IsDeadlocked: snapshot.IsDeadlocked,
	}
	for _, item := range items {
		plan.Items = append(plan.Items, RoundItem{ItemID: item.ID, Description: item.Description})
	}
	return plan, nil
}

// --- the rich models in ImplementerOutput, as pydantic's model_dump_json writes them -----------

type pyTaskContract struct {
	Objective    string   `json:"objective"`
	Role         string   `json:"role"`
	OutputSchema string   `json:"output_schema"`
	Boundaries   []string `json:"boundaries"`
	WriteSet     []string `json:"write_set"`
	TokenBudget  int      `json:"token_budget"`
	ToolBudget   int      `json:"tool_budget"`
	Acceptance   []string `json:"acceptance"`
}

type pyTicket struct {
	ID            string         `json:"id"`
	Contract      pyTaskContract `json:"contract"`
	ItemID        *string        `json:"item_id"`
	Status        string         `json:"status"`
	Branch        *string        `json:"branch"`
	ResultSummary string         `json:"result_summary"`
	Attempts      int            `json:"attempts"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// TicketJSON is python's Ticket.model_dump_json().
func TicketJSON(t coordination.Ticket) string {
	c := t.Contract
	data, _ := state.PydanticJSON(pyTicket{
		ID: t.ID,
		Contract: pyTaskContract{
			Objective: c.Objective, Role: c.Role, OutputSchema: c.OutputSchema, Boundaries: nz(c.Boundaries),
			WriteSet: nz(c.WriteSet), TokenBudget: c.TokenBudget, ToolBudget: c.ToolBudget, Acceptance: nz(c.Acceptance),
		},
		ItemID: optional(t.ItemID), Status: string(t.Status), Branch: optional(t.Branch),
		ResultSummary: t.ResultSummary, Attempts: t.Attempts,
	}, false)
	return string(data)
}

// ParseTicketJSON reads TicketJSON back.
func ParseTicketJSON(text string) (coordination.Ticket, error) {
	var p pyTicket
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		return coordination.Ticket{}, err
	}
	c := p.Contract
	return coordination.Ticket{
		ID: p.ID,
		Contract: coordination.TaskContract{
			Objective: c.Objective, Role: c.Role, OutputSchema: c.OutputSchema, Boundaries: c.Boundaries,
			WriteSet: c.WriteSet, TokenBudget: c.TokenBudget, ToolBudget: c.ToolBudget, Acceptance: c.Acceptance,
		},
		ItemID: deref(p.ItemID), Status: coordination.TicketStatus(p.Status), Branch: deref(p.Branch),
		ResultSummary: p.ResultSummary, Attempts: p.Attempts,
	}, nil
}

type pyLeaseDecision struct {
	Writer        string  `json:"writer"`
	Path          string  `json:"path"`
	Reason        string  `json:"reason"`
	Granted       bool    `json:"granted"`
	PreviousOwner *string `json:"previous_owner"`
	Why           string  `json:"why"`
}

// LeaseDecisionJSON is python's LeaseDecision.model_dump_json().
func LeaseDecisionJSON(d coordination.LeaseDecision) string {
	var previous *string
	if d.HasPrevious {
		previous = &d.PreviousOwner
	}
	data, _ := state.PydanticJSON(pyLeaseDecision{
		Writer: d.Writer, Path: d.Path, Reason: d.Reason, Granted: d.Granted, PreviousOwner: previous, Why: d.Why,
	}, false)
	return string(data)
}

// ParseLeaseDecisionJSON reads LeaseDecisionJSON back.
func ParseLeaseDecisionJSON(text string) (coordination.LeaseDecision, error) {
	var p pyLeaseDecision
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		return coordination.LeaseDecision{}, err
	}
	return coordination.LeaseDecision{
		Writer: p.Writer, Path: p.Path, Reason: p.Reason, Granted: p.Granted,
		PreviousOwner: deref(p.PreviousOwner), HasPrevious: p.PreviousOwner != nil, Why: p.Why,
	}, nil
}

// --- run_implementer --------------------------------------------------------------------------

func cachePath(ctx context.Context, workdir, cycleID string) (string, error) {
	dir, err := implementerCache(ctx, workdir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, safeID(cycleID)+".json"), nil
}

// loadCachedImplementer is a previous attempt's result, if its branch still points at the
// recorded head.
func loadCachedImplementer(ctx context.Context, inp ImplementerInput) *ImplementerOutput {
	path, err := cachePath(ctx, inp.Workdir, inp.CycleID)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out ImplementerOutput
	if json.Unmarshal(data, &out) != nil || out.Branch == "" {
		return nil
	}
	tip, _ := state.RunGitWith(ctx, inp.Workdir, state.RunOptions{NoCheck: true},
		"rev-parse", "--verify", "-q", "refs/heads/"+out.Branch)
	if pyfmt.PyStrip(tip) != out.Head {
		return nil
	}
	return &out
}

func saveCachedImplementer(ctx context.Context, workdir string, out ImplementerOutput) error {
	path, err := cachePath(ctx, workdir, out.CycleID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// contextNotes is the implementer's board: operator steering and approved actions.
func contextNotes(inp ImplementerInput) string {
	var parts []string
	if len(inp.SteerNotes) > 0 {
		notes := make([]string, len(inp.SteerNotes))
		for i, n := range inp.SteerNotes {
			notes[i] = "- " + n
		}
		parts = append(parts, "Operator steering (most recent last):\n"+strings.Join(notes, "\n"))
	}
	if len(inp.ApprovedActions) > 0 {
		approved := make([]string, len(inp.ApprovedActions))
		for i, a := range inp.ApprovedActions {
			approved[i] = "- " + a.Summary
		}
		parts = append(parts, "An operator APPROVED these previously queued actions; each is allowed once, "+
			"exactly as requested:\n"+strings.Join(approved, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

func implementerOutput(run *org.ImplementerRun, inp ImplementerInput, gate *DeferredApprovalGate, spent float64) ImplementerOutput {
	out := ImplementerOutput{
		ItemID: run.Item.ID, CycleID: inp.CycleID, Branch: run.Branch, Head: run.Head,
		Brief: pyfmt.Head(run.Brief, 8_000), ToolCalls: run.ToolCalls, Error: run.Error,
		DecisionsJSON: []string{}, TicketJSON: TicketJSON(run.Ticket), TicketHistory: []map[string]string{},
		Leases: []string{}, SpentUSD: spent,
		PendingApprovals: append([]PendingApproval{}, gate.Pending...),
		UsedApprovals:    append([]string{}, gate.Used...),
	}
	if run.Verification != nil {
		data, _ := state.PydanticJSON(*run.Verification, false)
		out.VerificationJSON = string(data)
	}
	for _, d := range run.Decisions {
		data, _ := state.PydanticJSON(d, false)
		out.DecisionsJSON = append(out.DecisionsJSON, string(data))
	}
	for _, h := range run.Tickets {
		out.TicketHistory = append(out.TicketHistory, map[string]string{"status": h.Status, "note": h.Note})
	}
	for _, d := range run.Leases.Decisions() {
		out.Leases = append(out.Leases, LeaseDecisionJSON(d))
	}
	return out
}

// RunImplementer is run_implementer: one parallel implementer in its own worktree (safe to
// retry).
func (a *Activities) RunImplementer(ctx context.Context, inp ImplementerInput) (ImplementerOutput, error) {
	settings, err := a.settings()
	if err != nil {
		return ImplementerOutput{}, configError(fmt.Sprintf("invalid configuration: %v", err), err)
	}
	checks, err := ResolveChecks(inp.CheckCommands)
	if err != nil {
		return ImplementerOutput{}, err
	}
	release, err := WorkdirLock(ctx, inp.Workdir, "lha-impl-"+safeID(inp.CycleID)+".lock", LockWait,
		func() { heartbeat(ctx, "waiting for the implementer lock") })
	if err != nil {
		return ImplementerOutput{}, err
	}
	defer release()
	if cached := loadCachedImplementer(ctx, inp); cached != nil {
		return *cached, nil // a previous attempt committed the branch, then crashed
	}
	anchor := state.NewGitMissionAnchor(inp.Workdir)
	snapshot, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return ImplementerOutput{}, err
	}
	checklist, err := anchor.ReadChecklist(ctx)
	if err != nil {
		return ImplementerOutput{}, err
	}
	item := checklist.Get(inp.ItemID)
	if item == nil || !item.IsActionableStatus() {
		status := "missing"
		if item != nil {
			status = item.Status
		}
		return ImplementerOutput{
			ItemID: inp.ItemID, CycleID: inp.CycleID,
			Error: fmt.Sprintf("item %s is no longer actionable (%s)", inp.ItemID, status),
		}, nil
	}
	persisted, err := coordination.ReadOwnership(ctx, anchor)
	if err != nil {
		return ImplementerOutput{}, err
	}
	ownership := coordination.EffectiveOwnership(persisted, coordination.FinishedWriters(checklist))
	meter, err := BuildWaveMeter(ctx, settings, inp.Workdir, inp.CycleID, inp.BudgetUSD, inp.MaxCycles, inp.WaveSize)
	if err != nil {
		return ImplementerOutput{}, err
	}
	focused := snapshot
	focusedItem := *item
	focused.ActiveItem = &focusedItem
	inner, err := roleModel(a.ImplementerModel, "implementer", settings, focused)
	if err != nil {
		return ImplementerOutput{}, configError(fmt.Sprintf("cannot build the model: %v", err), err)
	}
	closeModel := func() { _ = agent.CloseProvider(context.WithoutCancel(ctx), inner) }
	detach, recorder, err := a.attachLedger(ctx, settings, inp.Workdir, inp.MissionID, meter, "impl")
	if err != nil {
		closeModel()
		return ImplementerOutput{}, err
	}
	gate := NewDeferredApprovalGate(approvedFingerprints(inp.ApprovedActions))
	run := org.NewImplementerRun(*item, inp.CycleID, ownership, settings.MaxTurnsPerCycle, checkNames(checks))
	missionText := ""
	if snapshot.Mission != nil {
		missionText = snapshot.Mission.RenderAnchor()
	}
	// The board and this item's reflection come from the committed log, as a resumed
	// `lha orchestrate` rebuilds them; the operator's notes follow the board.
	events, err := anchor.ReadEvents(ctx)
	if err != nil {
		closeModel()
		detach()
		return ImplementerOutput{}, err
	}
	board := []string{}
	for _, part := range []string{org.BoardContextFromEvents(events), contextNotes(inp)} {
		if part != "" {
			board = append(board, part)
		}
	}
	objective, extra := org.ImplementerObjective(run, org.ObjectiveInput{
		MissionText:   missionText,
		Reflection:    org.ReflectionFor(events, item.ID),
		DecisionsText: agent.RenderDecisions(snapshot.LastDecisions),
		Briefs:        append([]string{}, inp.ResearchBriefs...),
		Board:         strings.Join(board, "\n\n"),
	})
	lease := coordination.NewLeaseHandler(coordination.NewLeaseBroker(state.NewGitMissionAnchor(inp.Workdir)),
		run.Writer, inp.CycleID, ownership, run.Leases)
	runErr := func() error {
		defer func() {
			closeModel()
			if run.Worktree != "" { // the branch stays for the integrator
				org.RemoveWorktree(ctx, inp.Workdir, run.Worktree, "")
			}
			key := IdempotencyKey(inp.MissionID, inp.CycleID, "impl", strconv.Itoa(attemptOf(ctx)))
			if err := RecordSpend(context.WithoutCancel(ctx), inp.Workdir, key, inp.CycleID, meter.Ledger); err != nil {
				activityLogger().Warn("spend_journal_write_failed", "error", err.Error())
			}
			detach()
		}()
		_, err := withHeartbeat(ctx, a.heartbeatEvery(), "implementer:"+inp.ItemID, func() (struct{}, error) {
			return struct{}{}, org.ImplementInWorktree(ctx, run, org.ImplementOptions{
				Settings: settings, Workdir: inp.Workdir, Base: inp.BaseSHA, Ownership: ownership,
				Model: meter.Wrap(inner, "implementer"), Gate: gate, AllowEgress: nil, MissionID: inp.MissionID,
				MissionChecks: checks, Objective: objective, Extra: extra, Lease: lease, Recorder: recorder,
			})
		})
		return err
	}()
	if runErr != nil {
		var budget *governor.BudgetExceeded
		switch {
		case errors.As(runErr, &budget):
			return ImplementerOutput{}, budgetError(budget)
		case isToolboxConfigError(runErr):
			return ImplementerOutput{}, configError(fmt.Sprintf("cannot open the sandbox: %v", runErr), runErr)
		}
		return ImplementerOutput{}, runErr
	}
	out := implementerOutput(run, inp, gate, spentOn(meter, inp.CycleID))
	if err := saveCachedImplementer(ctx, inp.Workdir, out); err != nil {
		return ImplementerOutput{}, err
	}
	return out, nil
}

// --- integrate_branch -------------------------------------------------------------------------

func deleteBranch(ctx context.Context, workdir, branch string) {
	if branch != "" {
		_, _ = state.RunGitWith(context.WithoutCancel(ctx), workdir, state.RunOptions{NoCheck: true}, "branch", "-D", branch)
	}
}

// runFromOutput rebuilds the implementer's run from its activity result (fallback when it
// produced none).
func runFromOutput(inp IntegrateInput, item contracts.ChecklistItem, fallback *org.ImplementerRun) (*org.ImplementerRun, error) {
	out := inp.Output
	if out == nil {
		fallback.Error = inp.Error
		if fallback.Error == "" {
			fallback.Error = "the implementer produced no result"
		}
		return fallback, nil
	}
	run := &org.ImplementerRun{
		Item: item, CycleID: inp.CycleID, Writer: coordination.WriterForItem(item.ID), Ticket: fallback.Ticket,
		Branch: out.Branch, Head: out.Head, Brief: out.Brief, ToolCalls: out.ToolCalls, Error: out.Error,
		Tickets: fallback.Tickets, Leases: &coordination.LeaseLog{},
	}
	if out.TicketJSON != "" {
		ticket, err := ParseTicketJSON(out.TicketJSON)
		if err != nil {
			return nil, err
		}
		run.Ticket = ticket
	}
	if out.VerificationJSON != "" {
		var v contracts.VerificationResult
		if err := json.Unmarshal([]byte(out.VerificationJSON), &v); err != nil {
			return nil, err
		}
		run.Verification = &v
	}
	for _, d := range out.DecisionsJSON {
		var rec contracts.DecisionRecord
		if err := json.Unmarshal([]byte(d), &rec); err != nil {
			return nil, err
		}
		run.Decisions = append(run.Decisions, rec)
	}
	if len(out.TicketHistory) > 0 {
		run.Tickets = nil
		for _, h := range out.TicketHistory {
			run.Tickets = append(run.Tickets, org.TicketHistoryEntry{Status: h["status"], Note: h["note"], KeysSorted: true})
		}
	}
	for _, text := range out.Leases {
		d, err := ParseLeaseDecisionJSON(text)
		if err != nil {
			return nil, err
		}
		run.Leases.Append(d)
	}
	if run.Ticket.Status == coordination.TicketDone || run.Ticket.Status == coordination.TicketFailed {
		run.Ticket = fallback.Ticket // never re-advance a terminal ticket
	}
	return run, nil
}

// IntegrateBranch is integrate_branch: merge one implementer branch and commit the checkpoint
// (exactly once per cycle id).
func (a *Activities) IntegrateBranch(ctx context.Context, inp IntegrateInput) (CycleResult, error) {
	result, err := a.integrateBranch(ctx, inp)
	return result, asBudget(err)
}

func (a *Activities) integrateBranch(ctx context.Context, inp IntegrateInput) (CycleResult, error) {
	settings, err := a.settings()
	if err != nil {
		return CycleResult{}, configError(fmt.Sprintf("invalid configuration: %v", err), err)
	}
	checks, err := ResolveChecks(inp.CheckCommands)
	if err != nil {
		return CycleResult{}, err
	}
	branch := ""
	if inp.Output != nil {
		branch = inp.Output.Branch
	}
	item := strPtr(inp.ItemID)
	release, err := lockWorkdir(ctx, inp.Workdir, LockWait)
	if err != nil {
		return CycleResult{}, err
	}
	defer release()
	if err := org.AbortMerge(ctx, inp.Workdir); err != nil {
		return CycleResult{}, err
	}
	if err := a.resetWorkdir(ctx, inp.Workdir); err != nil {
		return CycleResult{}, err
	}
	anchor := state.NewGitMissionAnchor(inp.Workdir)
	if payload, ok := CommittedCycleEvent(ctx, inp.Workdir, inp.CycleID, "cycle"); ok {
		// A previous attempt committed this integration, then crashed.
		deleteBranch(ctx, inp.Workdir, branch)
		snapshot, err := anchor.ReadSituationalAwareness(ctx)
		if err != nil {
			return CycleResult{}, err
		}
		status, _ := payload["status"].(string)
		verdict := ""
		if v, present := payload["verdict"]; present && v != nil {
			verdict = fmt.Sprint(v)
		}
		return resultFromSnapshot(snapshot, resultOpts{
			itemID: item, advanced: true, verdict: verdict, itemBlocked: status == "blocked",
			itemSplit: status == "split", note: "already integrated by a previous attempt",
		}), nil
	}
	checklist, err := anchor.ReadChecklist(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	current := checklist.Get(inp.ItemID)
	if current == nil || !current.IsActionableStatus() {
		deleteBranch(ctx, inp.Workdir, branch)
		snapshot, err := anchor.ReadSituationalAwareness(ctx)
		if err != nil {
			return CycleResult{}, err
		}
		return resultFromSnapshot(snapshot, resultOpts{itemID: item, note: "item no longer actionable"}), nil
	}
	target := *current
	persisted, err := coordination.ReadOwnership(ctx, anchor)
	if err != nil {
		return CycleResult{}, err
	}
	ownership := coordination.EffectiveOwnership(persisted, coordination.FinishedWriters(checklist))
	fallback := org.NewImplementerRun(target, inp.CycleID, ownership, settings.MaxTurnsPerCycle, checkNames(checks))
	run, err := runFromOutput(inp, target, fallback)
	if err != nil {
		return CycleResult{}, err
	}
	if run.Head != "" { // the git-layer check, against the committed map (leases included)
		paths, err := coordination.ChangedPaths(ctx, inp.Workdir, inp.BaseSHA, run.Head)
		if err != nil {
			return CycleResult{}, err
		}
		run.Violations = ownership.Violations(run.Writer, paths)
	}
	snapshot, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	missionText := ""
	if snapshot.Mission != nil {
		missionText = snapshot.Mission.RenderAnchor()
	}
	split := func(ctx context.Context, list *contracts.Checklist, blocked contracts.ChecklistItem) ([]string, error) {
		meter, err := BuildCycleMeter(ctx, settings, inp.Workdir, inp.CycleID, inp.BudgetUSD, inp.MaxCycles)
		if err != nil {
			return nil, err
		}
		inner, err := a.leadModel(settings, snapshot)
		if err != nil {
			return nil, configError(fmt.Sprintf("cannot build the model: %v", err), err)
		}
		detach, _, err := a.attachLedger(ctx, settings, inp.Workdir, inp.MissionID, meter, "split")
		if err != nil {
			_ = agent.CloseProvider(context.WithoutCancel(ctx), inner)
			return nil, err
		}
		defer func() {
			_ = agent.CloseProvider(context.WithoutCancel(ctx), inner)
			key := IdempotencyKey(inp.MissionID, inp.CycleID, "split", strconv.Itoa(attemptOf(ctx)))
			if err := RecordSpend(context.WithoutCancel(ctx), inp.Workdir, key, inp.CycleID, meter.Ledger); err != nil {
				activityLogger().Warn("spend_journal_write_failed", "error", err.Error())
			}
			detach()
		}()
		return org.MaybeSplit(ctx, list, blocked, settings, meter.Wrap(inner, "replanner"), missionText)
	}
	// What `lha orchestrate` posts to its board during a round is committed here with the
	// integration checkpoint: each research brief and the implementer's summary.
	var events []contracts.EventRecord
	for _, brief := range inp.ResearchBriefTexts {
		events = append(events, org.BoardEventRecord("researcher:"+inp.ItemID, brief, inp.CycleID))
	}
	if inp.Output != nil && inp.Output.Brief != "" {
		events = append(events, org.BoardEventRecord(run.Writer, "["+inp.ItemID+"] "+inp.Output.Brief, inp.CycleID))
	}
	if inp.ResearchBriefs > 0 || len(inp.ResearchFailures) > 0 {
		failures := make([]any, len(inp.ResearchFailures))
		for i, f := range inp.ResearchFailures {
			failures[i] = headRunes(f, 500)
		}
		events = append(events, contracts.EventRecord{Kind: "research", CycleID: inp.CycleID, Payload: contracts.Payload(
			"item", inp.ItemID, "n", inp.ResearchBriefs, "failed", len(inp.ResearchFailures), "failures", failures,
		)})
	}
	trusted, err := settings.TrustedCheckCommands()
	if err != nil {
		return CycleResult{}, configError(fmt.Sprintf("invalid configuration: %v", err), err)
	}
	session, err := org.OpenLeadSandbox(ctx, settings, inp.Workdir)
	if err != nil {
		if isToolboxConfigError(err) {
			return CycleResult{}, configError(fmt.Sprintf("cannot open the sandbox: %v", err), err)
		}
		return CycleResult{}, err
	}
	integrator := &org.BranchIntegrator{
		Workdir: inp.Workdir, Session: session, Verifier: org.LeadVerifier(inp.Workdir, settings), Checks: checks,
	}
	itemChecks, _ := org.ItemChecks(target, checks, trusted)
	report, err := func() (org.IntegrationReport, error) {
		defer func() {
			_ = session.Close(context.WithoutCancel(ctx))
			_ = org.AbortMerge(ctx, inp.Workdir)
		}()
		return withHeartbeat(ctx, a.heartbeatEvery(), "integrate:"+inp.ItemID, func() (org.IntegrationReport, error) {
			return org.IntegrateRun(ctx, run, org.IntegrateOptions{
				Anchor: anchor, Integrator: integrator, Workdir: inp.Workdir, Base: inp.BaseSHA,
				Checks: itemChecks, Split: split, Events: events,
			})
		})
	}()
	if err != nil {
		return CycleResult{}, err
	}
	deleteBranch(ctx, inp.Workdir, branch)
	if !report.Merged && report.Status != contracts.StatusBlocked && report.Status != "split" {
		if err := a.reflect(ctx, settings, inp, target, report.Reason); err != nil {
			return CycleResult{}, err
		}
	}
	after, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	note := "verified + integrated"
	if !report.Merged {
		note = "not integrated: " + report.Reason
	}
	spent := 0.0
	if inp.Output != nil {
		spent = inp.Output.SpentUSD
	}
	return resultFromSnapshot(after, resultOpts{
		itemID: item, advanced: true, verdict: report.Verdict, itemBlocked: report.Status == contracts.StatusBlocked,
		itemSplit: report.Status == "split", note: headRunes(note, 2_000), spentUSD: spent, baseSHA: report.Before,
	}), nil
}

// reflect reflects on a failed attempt and commits the lesson as a reflection event.
// `lha orchestrate` does the same after a failed integration; here the event gets an anchor-only
// commit of its own, since the integration checkpoint is already made. Best effort: a model
// failure is logged and the attempt stays recorded as failed (python: _reflect).
func (a *Activities) reflect(ctx context.Context, settings *config.Settings, inp IntegrateInput, item contracts.ChecklistItem, reason string) error {
	anchor := state.NewGitMissionAnchor(inp.Workdir)
	snapshot, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return err
	}
	meter, err := BuildCycleMeter(ctx, settings, inp.Workdir, inp.CycleID, inp.BudgetUSD, inp.MaxCycles)
	if err != nil {
		return err
	}
	inner, err := a.leadModel(settings, snapshot)
	if err != nil {
		activityLogger().Warn("reflection skipped: cannot build the model", "error", err.Error())
		return nil
	}
	closeModel := func() { _ = agent.CloseProvider(context.WithoutCancel(ctx), inner) }
	detach, _, err := a.attachLedger(ctx, settings, inp.Workdir, inp.MissionID, meter, "reflect")
	if err != nil {
		closeModel()
		return err
	}
	text, reflectErr := func() (string, error) {
		defer func() {
			closeModel()
			key := IdempotencyKey(inp.MissionID, inp.CycleID, "reflect", strconv.Itoa(attemptOf(ctx)))
			if err := RecordSpend(context.WithoutCancel(ctx), inp.Workdir, key, inp.CycleID, meter.Ledger); err != nil {
				activityLogger().Warn("spend_journal_write_failed", "error", err.Error())
			}
			detach()
		}()
		return agents.ReflectOnFailure(ctx, meter.Wrap(inner, "reflection"), item.Description, item.ID+" was not integrated: "+reason)
	}()
	if reflectErr != nil {
		if budget := asBudget(reflectErr); budget != nil {
			return budget
		}
		activityLogger().Warn("reflection failed", "item", item.ID, "error", reflectErr.Error())
		return nil // the lesson is help, not a gate
	}
	checklist, err := anchor.ReadChecklist(ctx)
	if err != nil {
		return err
	}
	_, err = anchor.CommitAnchorUpdate(ctx, contracts.Checkpoint{
		CycleID: inp.CycleID, Checklist: checklist, Decisions: []contracts.DecisionRecord{},
		Events:        []contracts.EventRecord{org.ReflectionEventRecord(item.ID, "\nReflection on "+item.ID+": "+text+"\n", inp.CycleID)},
		CommitMessage: "lha: reflection on " + item.ID,
	})
	return err
}

// leadModel is the Lead's model (the replanner of a blocked branch): ModelFactory, else the
// configured model (python: _default_model_factory).
func (a *Activities) leadModel(settings *config.Settings, snapshot contracts.SituationSnapshot) (contracts.ModelProvider, error) {
	if a.ModelFactory != nil {
		return a.ModelFactory(settings, snapshot)
	}
	return model.BuildProvider(settings, "", nil)
}

// --- review_cycle -----------------------------------------------------------------------------

// reviewBoardPost is the blocking verdict's board post, as `lha orchestrate` posts it (none when
// the review approved).
func reviewBoardPost(itemID string, review agents.ReviewResult, cycleID string) []contracts.EventRecord {
	if !review.Blocking {
		return []contracts.EventRecord{}
	}
	return []contracts.EventRecord{org.BoardEventRecord("reviewer:"+itemID, review.Notes(), cycleID)}
}

// blockingStreak is the number of blocking reviews of itemID since its last approval.
func blockingStreak(events []contracts.EventRecord, itemID string) int {
	streak := 0
	for _, e := range events {
		if e.Kind == ReviewEvent && e.Payload.Value("item_id") == itemID {
			if blocking, _ := e.Payload.Value("blocking").(bool); blocking {
				streak++
			} else {
				streak = 0
			}
		}
	}
	return streak
}

// ReviewCycle is review_cycle: an independent review of one verified item, its verdict committed
// (exactly once per reviewed cycle).
func (a *Activities) ReviewCycle(ctx context.Context, inp ReviewInput) (CycleResult, error) {
	result, err := a.reviewCycle(ctx, inp)
	return result, asBudget(err)
}

func (a *Activities) reviewCycle(ctx context.Context, inp ReviewInput) (CycleResult, error) {
	settings, err := a.settings()
	if err != nil {
		return CycleResult{}, configError(fmt.Sprintf("invalid configuration: %v", err), err)
	}
	reviewID := inp.CycleID + "-review"
	item := strPtr(inp.ItemID)
	release, err := lockWorkdir(ctx, inp.Workdir, LockWait)
	if err != nil {
		return CycleResult{}, err
	}
	defer release()
	if err := a.resetWorkdir(ctx, inp.Workdir); err != nil {
		return CycleResult{}, err
	}
	anchor := state.NewGitMissionAnchor(inp.Workdir)
	if payload, ok := CommittedCycleEvent(ctx, inp.Workdir, reviewID, ReviewEvent); ok {
		snapshot, err := anchor.ReadSituationalAwareness(ctx)
		if err != nil {
			return CycleResult{}, err
		}
		verdict := "passed"
		if blocking, _ := payload["blocking"].(bool); blocking {
			verdict = "review_blocked"
		}
		blocked, _ := payload["blocked"].(bool)
		return resultFromSnapshot(snapshot, resultOpts{
			itemID: item, advanced: true, verdict: verdict, itemBlocked: blocked,
			note: "already reviewed by a previous attempt",
		}), nil
	}
	checklist, err := anchor.ReadChecklist(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	snapshot, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	target := checklist.Get(inp.ItemID)
	if target == nil || target.Status != contracts.StatusDone {
		return resultFromSnapshot(snapshot, resultOpts{itemID: item, note: "item not done: no review"}), nil
	}
	criteria := target.Description
	base := inp.BaseSHA
	if base == "" {
		base = inp.HeadSHA + "^1"
	}
	priorEvents, err := anchor.ReadEvents(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	base = org.ReviewBase(priorEvents, inp.ItemID, base)
	diff := org.DiffSince(ctx, inp.Workdir, base, inp.HeadSHA)
	findings := verify.ScreenDiff(diff) // weakened tests go to the reviewer as criteria
	criteria = verify.ScreenCriteria(findings, criteria)
	meter, err := BuildCycleMeter(ctx, settings, inp.Workdir, reviewID, inp.BudgetUSD, inp.MaxCycles)
	if err != nil {
		return CycleResult{}, err
	}
	inner, err := roleModel(a.ReviewerModel, "reviewer", settings, snapshot)
	if err != nil {
		return CycleResult{}, configError(fmt.Sprintf("cannot assemble the reviewer: %v", err), err)
	}
	closeModel := func() { _ = agent.CloseProvider(context.WithoutCancel(ctx), inner) }
	noEgress := false
	tools, err := org.RunDispatcher(settings, false, nil, &noEgress)
	if err != nil {
		closeModel()
		return CycleResult{}, configError(fmt.Sprintf("cannot assemble the reviewer: %v", err), err)
	}
	session, err := org.OpenLeadSandbox(ctx, settings, inp.Workdir)
	if err != nil {
		closeModel()
		if isToolboxConfigError(err) {
			return CycleResult{}, configError(fmt.Sprintf("cannot open the sandbox: %v", err), err)
		}
		return CycleResult{}, err
	}
	detach, recorder, err := a.attachLedger(ctx, settings, inp.Workdir, inp.MissionID, meter, "review")
	if err != nil {
		_ = session.Close(context.WithoutCancel(ctx))
		closeModel()
		return CycleResult{}, err
	}
	review, err := func() (agents.ReviewResult, error) {
		defer func() {
			_ = session.Close(context.WithoutCancel(ctx))
			closeModel()
			key := IdempotencyKey(inp.MissionID, reviewID, strconv.Itoa(attemptOf(ctx)))
			if err := RecordSpend(context.WithoutCancel(ctx), inp.Workdir, key, reviewID, meter.Ledger); err != nil {
				activityLogger().Warn("spend_journal_write_failed", "error", err.Error())
			}
			detach()
		}()
		return withHeartbeat(ctx, a.heartbeatEvery(), reviewID, func() (agents.ReviewResult, error) {
			return org.NewReviewer(meter.Wrap(inner, "reviewer"), tools).WithRecorder(recorder, inp.CycleID).Review(ctx, pyfmt.Head(diff, reviewDiffCap), criteria,
				contracts.ToolContext{MissionID: inp.MissionID, Session: session})
		})
	}()
	if err != nil {
		return CycleResult{}, err
	}
	blocked := false
	if review.Blocking {
		events, err := anchor.ReadEvents(ctx)
		if err != nil {
			return CycleResult{}, err
		}
		blocked = blockingStreak(events, inp.ItemID)+1 >= org.MaxConsecutiveFailures
		org.ReopenForReview(&checklist, inp.ItemID, review, blocked)
	}
	outcome := "approved"
	if review.Blocking {
		outcome = "reopened"
		if blocked {
			outcome = "blocked"
		}
	}
	if _, err := anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID:         reviewID,
		ProgressSummary: fmt.Sprintf("- %s review of [%s]: %s", reviewID, inp.ItemID, outcome),
		Checklist:       checklist,
		Events: append(append([]contracts.EventRecord{verify.ReviewScreenEventRecord(inp.ItemID, base, inp.HeadSHA, findings, false, reviewID)},
			reviewBoardPost(inp.ItemID, review, reviewID)...), contracts.EventRecord{Kind: ReviewEvent, CycleID: reviewID, Payload: contracts.Payload(
			"item_id", inp.ItemID, "verdict", review.Verdict, "blocking", review.Blocking,
			"blocking_issues", org.CapList(review.BlockingIssues, 20), "advisory", org.CapList(review.Advisory, 20),
			"reopened", review.Blocking && !blocked, "blocked", blocked, "base", base, "head", inp.HeadSHA,
			"tool_calls", review.ToolCalls, "brief", pyfmt.Head(obs.RedactText(review.Brief), 2_000),
		)}),
		CommitMessage: fmt.Sprintf("lha: review %s %s", outcome, inp.ItemID),
	}); err != nil {
		return CycleResult{}, err
	}
	after, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	verdict := "passed"
	if review.Blocking {
		verdict = "review_blocked"
	}
	return resultFromSnapshot(after, resultOpts{
		itemID: item, advanced: true, verdict: verdict, itemBlocked: blocked,
		note: headRunes(review.Notes(), 2_000), spentUSD: spentOn(meter, reviewID),
	}), nil
}

// --- run_subagent -----------------------------------------------------------------------------

// runSubAgent is run_subagent's body (python: lha.durable.agent_activities.run_subagent): one
// sub-agent, budgeted like a cycle — in a mission checkout its meter is seeded with the mission's
// recorded spend and its own spend is appended to the journal — with every metered call written
// to the cost ledger under the parent mission id, in the Lead's sandbox.
func (a *Activities) runSubAgent(ctx context.Context, inp SubAgentInput) (SubAgentOutput, error) {
	settings, err := a.settings()
	if err != nil {
		return SubAgentOutput{}, configError(fmt.Sprintf("invalid configuration: %v", err), err)
	}
	role, ok := agents.Roles[inp.RoleName]
	if !ok {
		return SubAgentOutput{}, configError("unknown sub-agent role "+contracts.PyRepr(inp.RoleName), nil)
	}
	journal, _ := state.IsRepo(ctx, inp.Workdir)
	cycleID := "subagent:" + inp.RoleName
	if inp.CycleID != "" {
		cycleID = inp.CycleID + ":" + inp.RoleName
	}
	var meter *governor.CostMeter
	if journal { // a mission checkout: the mission's spend so far counts
		meter, err = BuildCycleMeter(ctx, settings, inp.Workdir, cycleID, inp.BudgetUSD, settings.MaxCycles)
		if err != nil {
			return SubAgentOutput{}, err
		}
	} else {
		ceiling := settings.BudgetUSDCeiling
		if inp.BudgetUSD != nil {
			ceiling = *inp.BudgetUSD
		}
		meter = governor.NewCostMeter(governor.NewCostLedger(),
			governor.NewBudgetGovernor(ceiling, settings.MaxCycles, settings.AllowUnpricedModels))
	}
	var egress *bool // web tools iff LHA_EGRESS_ALLOW_HOSTS and the role + input allow egress
	if !(role.AllowEgress && inp.AllowEgress) {
		off := false
		egress = &off
	}
	dispatcher, err := org.RunDispatcher(settings, role.AllowMutating, nil, egress)
	if err != nil { // RuleOfTwoViolation / WebConfigError: fail closed
		return SubAgentOutput{}, configError(fmt.Sprintf("cannot assemble the sub-agent's tools: %v", err), err)
	}
	var inner contracts.ModelProvider
	if a.SubAgentModel != nil {
		inner, err = a.SubAgentModel(settings, contracts.SituationSnapshot{})
	} else {
		inner, err = model.BuildProvider(settings, "", nil)
	}
	if err != nil {
		return SubAgentOutput{}, configError(fmt.Sprintf("cannot start sub-agent: %v", err), err)
	}
	closeModel := func() { _ = agent.CloseProvider(context.WithoutCancel(ctx), inner) }
	// The same sandbox as the lead: image, egress allow-list and resource limits.
	session, err := org.OpenLeadSandbox(ctx, settings, inp.Workdir)
	if err != nil {
		closeModel()
		if isToolboxConfigError(err) {
			return SubAgentOutput{}, configError(fmt.Sprintf("cannot start sub-agent: %v", err), err)
		}
		return SubAgentOutput{}, err
	}
	meter.SetCycleID(cycleID)
	store, err := a.openStore(ctx, settings, inp.Workdir)
	if err != nil {
		_ = session.Close(context.WithoutCancel(ctx))
		closeModel()
		if errors.Is(err, ErrStoreUnavailable) {
			return SubAgentOutput{}, configError(fmt.Sprintf("cannot open the mission store: %v", err), err)
		}
		return SubAgentOutput{}, err // e.g. an unwritable SQLite path: retryable
	}
	prefix := "sub:" + inp.RoleName
	if activity.IsActivity(ctx) {
		info := activity.GetInfo(ctx)
		prefix = fmt.Sprintf("sub:%s:%s@%d", info.WorkflowExecution.ID, info.ActivityID, info.Attempt)
	}
	spendKey := IdempotencyKey(inp.MissionID, prefix)
	meter.SetHook(newLedgerHook(store, inp.MissionID, prefix))
	recorder, closeEvents := recorderFor(store) // the sub-agent's tool calls -> mission_events
	result, err := func() (org.SubAgentResult, error) {
		defer func() {
			_ = session.Close(context.WithoutCancel(ctx))
			closeModel()
			if journal {
				if err := RecordSpend(context.WithoutCancel(ctx), inp.Workdir, spendKey, cycleID, meter.Ledger); err != nil {
					activityLogger().Warn("spend_journal_write_failed", "error", err.Error())
				}
			}
			meter.SetHook(nil)
			closeEvents()
			_ = store.Close(context.WithoutCancel(ctx))
		}()
		return withHeartbeat(ctx, a.heartbeatEvery(), "subagent:"+inp.RoleName, func() (org.SubAgentResult, error) {
			return org.NewSubAgent(role, meter.Wrap(inner, inp.RoleName), dispatcher, 0).WithRecorder(recorder, inp.CycleID).Run(ctx, inp.Objective,
				contracts.ToolContext{MissionID: inp.MissionID, Session: session}, "")
		})
	}()
	if err != nil {
		return SubAgentOutput{}, asBudget(err)
	}
	return SubAgentOutput{Role: result.Role, Brief: result.Brief, ToolCalls: result.ToolCalls, Turns: result.Turns}, nil
}
