package org

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/tools"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// The building blocks of a parallel implementer wave, shared by every run path (python:
// lha.agents.waves):
//
//   - ParallelBatch picks the actionable items that each own a non-empty write-set;
//   - NewImplementerRun creates the item's Ticket / TaskContract;
//   - ImplementInWorktree runs one Implementer in its own git worktree behind an OwnershipGuard
//     (plus record_decision and, with a lease handler, request_lease), verifies the worktree and
//     commits the work on the implementer's branch;
//   - IntegrateRun offers the branch to the BranchIntegrator and commits the checkpoint: the merge
//     commit IS the checkpoint when the branch is merged, and a failed attempt is recorded
//     otherwise (blocked after 3 in a row, then optionally split by the replanner).

// MaxConsecutiveFailures is the same threshold as the Lead's AgentLoop.
const MaxConsecutiveFailures = 3

// ParallelBatch is the actionable items that may run concurrently: each has a non-empty
// write-set of its own (write-sets are disjoint by construction). Fewer than two such items
// means there is nothing to parallelize (the Lead works serially).
func ParallelBatch(checklist contracts.Checklist, ownership *coordination.FileOwnershipMap, limit int) []contracts.ChecklistItem {
	if limit < 2 {
		return nil
	}
	done := map[string]bool{}
	for _, item := range checklist.Items {
		if item.Status == contracts.StatusDone {
			done[item.ID] = true
		}
	}
	batch := []contracts.ChecklistItem{}
	for _, item := range checklist.Items {
		if !item.IsActionableStatus() || len(ownership.WriteSet(coordination.WriterForItem(item.ID))) == 0 {
			continue
		}
		ready := true
		for _, dep := range item.DependsOn {
			ready = ready && done[dep]
		}
		if ready {
			batch = append(batch, item)
		}
	}
	if len(batch) < 2 {
		return nil
	}
	return batch[:min(limit, len(batch))]
}

// DiffSince is `git diff base..head` without the harness's .lha/ files.
func DiffSince(ctx context.Context, workdir, base, head string) string {
	if base == "" || head == "" || base == head {
		return "(no new commits)"
	}
	diff, _ := state.RunGitWith(ctx, workdir, state.RunOptions{NoCheck: true},
		"diff", base+".."+head, "--", ".", ":(exclude).lha")
	if diff == "" {
		return "(empty diff)"
	}
	return diff
}

// TicketHistoryEntry is one step of a ticket's lifecycle as the ticket event records it.
type TicketHistoryEntry struct {
	Status string
	Note   string
	// KeysSorted marks an entry that crossed an activity boundary: Python's Temporal JSON
	// converter writes dicts with sorted keys, so the ticket event lists it as {"note", "status"}.
	KeysSorted bool
}

// ImplementerRun is one implementer's attempt at one item, before integration.
type ImplementerRun struct {
	Item         contracts.ChecklistItem
	CycleID      string
	Writer       string
	Ticket       coordination.Ticket
	Branch       string
	Worktree     string // "" until created
	Head         string
	Brief        string
	ToolCalls    int
	Verification *contracts.VerificationResult
	Violations   []coordination.OwnershipViolation
	Decisions    []contracts.DecisionRecord
	Error        string
	Tickets      []TicketHistoryEntry // the lifecycle, for events
	Leases       *coordination.LeaseLog
}

// Advance moves the ticket to "to" and records the step.
func (r *ImplementerRun) Advance(to coordination.TicketStatus, note string) {
	if next, err := r.Ticket.Transition(to); err == nil {
		r.Ticket = next
	} else {
		panic(err) // the wave only walks legal edges
	}
	r.Tickets = append(r.Tickets, TicketHistoryEntry{Status: string(to), Note: pyfmt.Head(note, 300)})
}

// NewImplementerRun is the run (and its created ticket) for item's implementer.
func NewImplementerRun(item contracts.ChecklistItem, cycleID string, ownership *coordination.FileOwnershipMap, toolBudget int, acceptance []string) *ImplementerRun {
	writer := coordination.WriterForItem(item.ID)
	contract := coordination.TaskContract{
		Objective:    item.Description,
		Role:         "implementer",
		OutputSchema: "a short summary of what you changed and why",
		Boundaries: []string{
			"write only the files in write_set; request a lease instead of writing others",
			"do not edit .lha/ (harness-owned) or existing tests / test configuration",
		},
		WriteSet:   ownership.WriteSet(writer),
		ToolBudget: toolBudget,
		Acceptance: append([]string{}, acceptance...),
	}
	run := &ImplementerRun{
		Item: item, CycleID: cycleID, Writer: writer,
		Ticket: coordination.NewTicket(cycleID+"-"+item.ID, contract, item.ID, item.Attempts),
		Leases: &coordination.LeaseLog{},
	}
	run.Tickets = append(run.Tickets, TicketHistoryEntry{Status: string(coordination.TicketCreated)})
	return run
}

// ObjectiveInput is the context ImplementerObjective renders.
type ObjectiveInput struct {
	MissionText   string
	Reflection    string
	DecisionsText string
	Briefs        []string
	Board         string
	// NoLeaseTool: the implementer has no request_lease tool (python: lease_tool=False).
	NoLeaseTool bool
}

// ImplementerObjective is the implementer's objective (from its contract) and its extra context.
func ImplementerObjective(run *ImplementerRun, in ObjectiveInput) (objective, extra string) {
	contract := run.Ticket.Contract
	lease := "If the item truly needs a file outside your write-set, call request_lease first. "
	if in.NoLeaseTool {
		lease = ""
	}
	acceptance := strings.Join(contract.Acceptance, ", ")
	if acceptance == "" {
		acceptance = "(none)"
	}
	objective = "Checklist item [" + run.Item.ID + "]: " + contract.Objective + "\n\n" +
		"Your write-set (the ONLY files you may create or modify): " + strings.Join(contract.WriteSet, ", ") + "\n" +
		"Boundaries: " + strings.Join(contract.Boundaries, "; ") + "\n" +
		lease +
		"Acceptance: the gating checks " + acceptance + " must " +
		"pass in your worktree. When the item is done, reply with " +
		`{"done": true, "summary": "` + contract.OutputSchema + `"}.`
	parts := []string{}
	if in.MissionText != "" {
		parts = append(parts, in.MissionText)
	}
	if run.Item.LastFailure != "" {
		parts = append(parts, "Previous attempt FAILED:\n"+pyfmt.Tail(run.Item.LastFailure, 3000))
	}
	if in.Reflection != "" {
		parts = append(parts, pyfmt.PyStrip(in.Reflection))
	}
	if in.DecisionsText != "" {
		parts = append(parts, in.DecisionsText)
	}
	if len(in.Briefs) > 0 {
		parts = append(parts, "Research briefs:\n"+strings.Join(in.Briefs, "\n---\n"))
	}
	if in.Board != "" {
		parts = append(parts, in.Board)
	}
	return objective, strings.Join(parts, "\n\n")
}

// ItemChecks are the mission checks plus the item's witnesses (as the Lead's cycle would gate
// it), and a failing result for each witness that cannot be turned into a check.
func ItemChecks(item contracts.ChecklistItem, missionChecks []contracts.Check, trusted map[string][]string) ([]contracts.Check, []contracts.CheckResult) {
	witnesses := []contracts.Check{}
	errs := []contracts.CheckResult{}
	for _, w := range item.Witnesses {
		check, err := verify.ParseWitness(w, trusted)
		if err != nil {
			errs = append(errs, contracts.CheckResult{
				Name: w, Passed: false, ExitCode: 2, Gating: true, OutputTail: "invalid witness: " + err.Error(),
			})
			continue
		}
		witnesses = append(witnesses, check)
	}
	return contracts.EnsureUniqueCheckNames(append(append([]contracts.Check{}, missionChecks...), witnesses...)), errs
}

// ImplementOptions are ImplementInWorktree's inputs.
type ImplementOptions struct {
	Settings      *config.Settings
	Workdir       string // the mission checkout
	Base          string // the mission branch's HEAD the worktree starts from
	Ownership     *coordination.FileOwnershipMap
	Model         contracts.ModelProvider
	Gate          contracts.HITLGate
	AllowEgress   *bool
	MissionID     string
	MissionChecks []contracts.Check
	Objective     string
	Extra         string
	// Lease, when non-nil, gives the implementer request_lease.
	Lease coordination.LeaseHandler
	// OnSummary receives the implementer's brief.
	OnSummary func(string)
}

// ImplementInWorktree runs one implementer in its own worktree, verifies there and commits on
// its branch. It sets run.Branch / run.Worktree first (the caller removes the worktree), then the
// brief, decisions, verification, branch head and the git-layer ownership violations (against
// the ownership map as it is after any lease the implementer was granted). An error (the
// implementer's model failed, ...) leaves the run unverified; the caller records it.
func ImplementInWorktree(ctx context.Context, run *ImplementerRun, o ImplementOptions) error {
	settings := o.Settings
	run.Branch = BranchFor(run.Writer, run.CycleID)
	worktree, err := AddWorktree(ctx, o.Workdir, run.Branch, o.Base)
	if err != nil {
		return err
	}
	run.Worktree = worktree
	run.Ticket.Branch = run.Branch
	run.Advance(coordination.TicketInProgress, "")
	globs := settings.HarnessGlobs()
	trusted, err := settings.TrustedCheckCommands()
	if err != nil {
		return err
	}
	session, err := OpenLeadSandbox(ctx, settings, worktree)
	if err != nil {
		return err
	}
	verifyErr := func() error {
		defer session.Close(context.WithoutCancel(ctx))
		var before verify.HarnessSnapshot
		if !run.Item.AllowHarnessEdits {
			before = verify.SnapshotHarnessGlobs(worktree, globs)
		}
		buffer := &tools.DecisionBuffer{}
		// The Lead's tools and human gate, behind the implementer's ownership guard; the lease
		// tool sits outside the guard (asking is always allowed, writing only after a grant).
		lead, err := LeadDispatcher(settings, o.Gate, o.AllowEgress)
		if err != nil {
			return err
		}
		var guarded contracts.ToolDispatcher = coordination.NewOwnershipGuard(lead, o.Ownership, []string{run.Writer}, o.Lease != nil)
		if o.Lease != nil {
			guarded = tools.WithLeaseTool(guarded, tools.LeaseHandler(o.Lease))
		}
		implementer := NewImplementer(o.Model, tools.WithDecisionTool(guarded, buffer), settings.MaxTurnsPerCycle)
		result, err := implementer.Run(ctx, o.Objective, contracts.ToolContext{MissionID: o.MissionID, Session: session}, o.Extra)
		if err != nil {
			return err
		}
		run.Brief, run.ToolCalls = result.Brief, result.ToolCalls
		run.Decisions = append([]contracts.DecisionRecord{}, buffer.Records...)
		if o.OnSummary != nil {
			o.OnSummary(result.Brief)
		}
		run.Advance(coordination.TicketAwaitingVerify, "")
		checks, witnessErrors := ItemChecks(run.Item, o.MissionChecks, trusted)
		verification, err := LeadVerifier(worktree, settings).Verify(ctx, session, checks)
		if err != nil {
			return err
		}
		if len(witnessErrors) > 0 {
			verification = verification.WithResults(witnessErrors)
		}
		if before != nil {
			after := verify.SnapshotHarnessGlobs(worktree, globs)
			if tampered := verify.HarnessViolations(before, after); len(tampered) > 0 {
				verification = verification.WithResults([]contracts.CheckResult{verify.IntegrityResult(tampered)})
			}
		}
		run.Verification = &verification
		return nil
	}()
	if verifyErr != nil {
		return verifyErr
	}
	head, err := CommitWorktree(ctx, worktree, "lha: "+run.Writer+" "+run.Item.ID+" ("+run.Item.Description+")")
	if err != nil {
		return err
	}
	run.Head = head
	paths, err := coordination.ChangedPaths(ctx, worktree, o.Base, run.Head)
	if err != nil {
		return err
	}
	run.Violations = o.Ownership.Violations(run.Writer, paths)
	return nil
}

// IntegrationReport is what IntegrateRun did with one implementer's branch.
type IntegrationReport struct {
	Merged    bool
	Reason    string
	Head      string // the checkpoint commit
	Before    string // the mission branch's HEAD before the merge
	Status    string // the item's status after the checkpoint ("split" when split)
	Verdict   string
	SplitInto []string
	Checklist contracts.Checklist
}

// SplitFn splits a newly blocked item: (checklist, blocked item) -> child ids (empty: no split).
type SplitFn func(ctx context.Context, checklist *contracts.Checklist, item contracts.ChecklistItem) ([]string, error)

// IntegrateOptions are IntegrateRun's inputs.
type IntegrateOptions struct {
	Anchor     *state.GitMissionAnchor
	Integrator *BranchIntegrator
	Workdir    string
	Base       string
	// Checks gate the merged workspace (the mission checks plus the item's witnesses).
	Checks []contracts.Check
	Split  SplitFn
	// Events are added to the checkpoint.
	Events []contracts.EventRecord
}

func roundDuration(d float64) float64 { return math.Round(d*1000) / 1000 }

// IntegrateRun offers one implementer branch to the integrator and commits the checkpoint. A
// merged branch is recorded done and the checkpoint IS the merge commit; the item's lease (and
// those of other finished items) is released in the same commit. Otherwise the failed attempt is
// recorded (blocked after MaxConsecutiveFailures, then split if Split returns children).
func IntegrateRun(ctx context.Context, run *ImplementerRun, o IntegrateOptions) (IntegrationReport, error) {
	item := run.Item
	own := run.Verification
	verified := own != nil && own.AllGreen
	before, err := state.HeadSHA(ctx, o.Workdir)
	if err != nil {
		return IntegrationReport{}, err
	}
	var post *contracts.VerificationResult
	var reason string
	switch {
	case run.Error != "":
		reason = "implementer failed: " + run.Error
	case !verified:
		reason = "the branch was not verified"
		if own != nil {
			reason = own.FailureReport(0)
		}
	default:
		run.Advance(coordination.TicketAwaitingMerge, "")
		integration, err := o.Integrator.Integrate(ctx, run.Branch, run.Head, o.Base, true, run.Violations, o.Checks)
		if err != nil {
			return IntegrationReport{}, err
		}
		if integration.Merged {
			post = integration.Verification
		}
		reason = integration.Reason
	}

	checklist, err := o.Anchor.ReadChecklist(ctx)
	if err != nil {
		return IntegrationReport{}, err
	}
	merged := post != nil
	if merged {
		run.Advance(coordination.TicketDone, "merged "+run.Branch)
		proved := []string{}
		for _, r := range post.Results {
			if r.Gating && r.Passed {
				proved = append(proved, r.Name)
			}
		}
		if _, err := checklist.RecordSuccess(item.ID, proved); err != nil {
			return IntegrationReport{}, err
		}
		// The slice is finished: its lease ends in the same commit that merges it (as do those of
		// any other finished item not yet released).
		persisted, err := coordination.ReadOwnership(ctx, o.Anchor)
		if err != nil {
			return IntegrationReport{}, err
		}
		released := coordination.EffectiveOwnership(persisted, doneWriters(checklist))
		if !released.Equal(persisted) {
			if err := coordination.StageOwnership(o.Anchor, released); err != nil {
				return IntegrationReport{}, err
			}
		}
	} else {
		run.Advance(coordination.TicketFailed, reason)
		if _, err := checklist.RecordFailure(item.ID, reason, MaxConsecutiveFailures); err != nil {
			return IntegrationReport{}, err
		}
	}
	status, attempts := "", 0
	current := checklist.Get(item.ID)
	if current != nil {
		status, attempts = current.Status, current.Attempts
	}
	splitInto := []string{}
	var verb, note, verdict string
	if merged {
		verb, note, verdict = "complete", "verified + integrated", "passed"
	} else {
		verb = "attempt"
		if status == contracts.StatusBlocked {
			verb = "block"
		}
		note = fmt.Sprintf("not integrated (attempt %d, status %s)", attempts, status)
		verdict = "failed"
		if own != nil && !own.AllGreen {
			verdict = own.Verdict
		}
		if status == contracts.StatusBlocked && current != nil && o.Split != nil {
			ids, err := o.Split(ctx, &checklist, *current)
			if err != nil {
				return IntegrationReport{}, err
			}
			if len(ids) > 0 {
				splitInto = ids
				verb, status = "split", "split"
				note += "; split into " + strings.Join(ids, ", ")
			}
		}
	}
	shown := own
	if post != nil {
		shown = post
	}
	checks := []any{}
	if shown != nil {
		for _, r := range shown.Results {
			checks = append(checks, contracts.NewOrderedMap(
				"name", r.Name, "passed", r.Passed, "gating", r.Gating, "exit_code", r.ExitCode,
				"duration_s", roundDuration(r.DurationS),
			))
		}
	}
	history := make([]any, len(run.Tickets))
	for i, h := range run.Tickets {
		if h.KeysSorted {
			history[i] = contracts.NewOrderedMap("note", h.Note, "status", h.Status)
		} else {
			history[i] = contracts.NewOrderedMap("status", h.Status, "note", h.Note)
		}
	}
	violations := make([]any, len(run.Violations))
	for i, v := range run.Violations {
		violations[i] = v.Path
	}
	leases := []any{}
	for _, d := range run.Leases.Decisions() {
		leases = append(leases, contracts.NewOrderedMap("path", d.Path, "granted", d.Granted, "why", d.Why))
	}
	writeSet := make([]any, len(run.Ticket.Contract.WriteSet))
	for i, p := range run.Ticket.Contract.WriteSet {
		writeSet[i] = p
	}
	events := append(append([]contracts.EventRecord{}, o.Events...),
		contracts.EventRecord{Kind: "cycle", CycleID: run.CycleID, Payload: contracts.Payload(
			"item_id", item.ID, "verified", merged, "verdict", verdict, "status", status,
			"tool_calls", run.ToolCalls, "writer", run.Writer, "branch", run.Branch,
			"split_into", splitInto, "checks", checks,
		)},
		contracts.EventRecord{Kind: "ticket", CycleID: run.CycleID, Payload: contracts.Payload(
			"ticket_id", run.Ticket.ID, "item_id", item.ID, "role", run.Ticket.Contract.Role,
			"write_set", writeSet, "branch", run.Branch, "status", string(run.Ticket.Status),
			"history", history, "ownership_violations", violations, "leases", leases,
		)},
	)
	branchShown := run.Branch
	if branchShown == "" {
		branchShown = "no branch"
	}
	decisions := []contracts.DecisionRecord{}
	if merged { // decisions travel with the code they describe: only merged work records them
		decisions = run.Decisions
	}
	message := "lha: " + verb + " " + item.ID + " (" + item.Description + ")"
	if merged && run.Head != o.Base {
		message += " [merged " + run.Branch + "]"
	}
	head, err := o.Anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID:         run.CycleID,
		ProgressSummary: "- " + run.CycleID + " [" + item.ID + "] " + item.Description + ": " + note + " (" + run.Writer + ", " + branchShown + ")",
		Checklist:       checklist,
		Decisions:       decisions,
		Events:          events,
		CommitMessage:   message,
	})
	if err != nil {
		return IntegrationReport{}, err
	}
	if err := o.Integrator.Abort(ctx); err != nil { // never leave a half-finished merge
		return IntegrationReport{}, err
	}
	return IntegrationReport{
		Merged: merged, Reason: reason, Head: head, Before: before, Status: status,
		Verdict: verdict, SplitInto: splitInto, Checklist: checklist,
	}, nil
}

func doneWriters(checklist contracts.Checklist) []string {
	out := []string{}
	for _, item := range checklist.Items {
		if item.Status == contracts.StatusDone {
			out = append(out, coordination.WriterForItem(item.ID))
		}
	}
	return out
}

// MaybeSplit splits a newly blocked item with the replanner, within the mission's replan budget
// (the same bounds as the Lead's AgentLoop); it returns the child ids (empty if not split).
func MaybeSplit(ctx context.Context, checklist *contracts.Checklist, item contracts.ChecklistItem, settings *config.Settings, model contracts.ModelProvider, missionText string) ([]string, error) {
	if settings.MaxReplans <= 0 {
		return []string{}, nil
	}
	splits := 0
	for _, i := range checklist.Items {
		if i.Status == contracts.StatusSplit {
			splits++
		}
	}
	if splits >= settings.MaxReplans || strings.Count(item.ID, ".") >= settings.MaxSplitDepth {
		return []string{}, nil
	}
	drafts, err := agents.NewReplanner(model).Split(ctx, missionText, item)
	if err != nil {
		return nil, err
	}
	if len(drafts) < 2 {
		return []string{}, nil
	}
	children, err := checklist.Split(item.ID, drafts)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(children))
	for i, c := range children {
		ids[i] = c.ID
	}
	return ids, nil
}

// ReopenForReview puts a verified-but-review-blocked item back to todo (blocked when block) with
// the review notes; nil if the item is gone.
func ReopenForReview(checklist *contracts.Checklist, itemID string, review agents.ReviewResult, block bool) *contracts.ChecklistItem {
	item := checklist.Get(itemID)
	if item == nil {
		return nil
	}
	item.Status = contracts.StatusTodo
	if block {
		item.Status = contracts.StatusBlocked
	}
	item.VerifiedBy = []string{}
	item.Notes = review.Notes()
	issues := review.BlockingIssues
	if len(issues) == 0 {
		issues = []string{"unparsed"}
	}
	item.LastFailure = "reviewer blocked: " + strings.Join(issues, "; ")
	return item
}

// runGroup runs fns concurrently and returns their errors in order (python: asyncio.gather with
// return_exceptions=True).
func runGroup(fns []func() error) []error {
	errs := make([]error, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs[i] = fmt.Errorf("%v", r)
				}
			}()
			errs[i] = fn()
		}()
	}
	wg.Wait()
	return errs
}

// CapList is the first n of items as JSON values (a review event's issue lists are capped).
func CapList(items []string, n int) []any {
	out := []any{}
	for i, s := range items {
		if i >= n {
			break
		}
		out = append(out, s)
	}
	return out
}

// --- the blackboard and reflections, as committed events ---------------------------------
// Both run paths record a board post and a reflection as anchor events; `lha orchestrate` keeps
// them in memory too, the durable organization reads them back from the committed log
// (python: board_event, reflection_event, board_context, reflection_for in agents/waves.py).

// ReflectionCap is how much of a reflection an event keeps.
const ReflectionCap = 2_000

// BoardEventRecord is a blackboard event for one post (capped as the board caps its entries).
func BoardEventRecord(author, text, cycleID string) contracts.EventRecord {
	return contracts.EventRecord{Kind: BoardEvent, CycleID: cycleID, Payload: contracts.Payload(
		"author", author, "text", pyfmt.Head(text, boardEntryCap),
	)}
}

// ReflectionEventRecord is a reflection event: the lesson for itemID's next attempt.
func ReflectionEventRecord(itemID, text, cycleID string) contracts.EventRecord {
	return contracts.EventRecord{Kind: ReflectionEvent, CycleID: cycleID, Payload: contracts.Payload(
		"item", itemID, "text", pyfmt.Head(text, ReflectionCap),
	)}
}

// BoardContextFromEvents is the newest committed board posts as the "Team board" block a prompt
// shows ("" if none).
func BoardContextFromEvents(events []contracts.EventRecord) string {
	posts := []contracts.EventRecord{}
	for _, e := range events {
		if e.Kind == BoardEvent {
			posts = append(posts, e)
		}
	}
	if len(posts) > boardEntries {
		posts = posts[len(posts)-boardEntries:]
	}
	if len(posts) == 0 {
		return ""
	}
	lines := make([]string, len(posts))
	for i, e := range posts {
		lines[i] = "[" + payloadStr(e.Payload, "author") + "] " + pyfmt.Head(payloadStr(e.Payload, "text"), boardEntryCap)
	}
	return "Team board (earlier rounds):\n" + strings.Join(lines, "\n---\n")
}

// ReflectionFor is the latest committed reflection for itemID ("" if none).
func ReflectionFor(events []contracts.EventRecord, itemID string) string {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Kind == ReflectionEvent && payloadStr(e.Payload, "item") == itemID {
			return payloadStr(e.Payload, "text")
		}
	}
	return ""
}
