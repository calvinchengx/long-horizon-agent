package durable

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// One round of the multi-agent organization inside MissionWorkflow (workflow code; python:
// lha.durable.org_round).
//
// Used instead of the single run_agent_cycle call when a mission opts in (ResearchPerItem > 0,
// Review, or MaxParallel >= 2); with all three off the workflow runs exactly as before.
// Deterministic scheduling only; the work happens in activities (org_activities.go) and in
// researcher child workflows (SubAgentWorkflow). A round is:
//
//  1. plan_round reads the committed checklist and ownership map: a wave when two or more
//     actionable items each own a disjoint write-set (at most MaxParallel, and never more than
//     the cycles left), otherwise the next actionable item (serial).
//  2. Research (ResearchPerItem > 0): that many read-only researcher child workflows per item,
//     concurrently (fanOutChildren; bounded by MaxResearchPerItem per item). Every failure is
//     kept: it is written to the gate log (lha mission-status) and committed as a research event
//     with the round's checkpoint; the round goes on with the briefs that did arrive.
//  3. Serial: one run_agent_cycle with the briefs in the Lead's prompt. Wave: one run_implementer
//     activity per item, concurrently, each in its own worktree; then one integrate_branch
//     activity per item, in checklist order: each integration commit is a checkpoint and counts
//     as a cycle.
//  4. Review (Review): after every verified item (a passed serial cycle, a merged branch),
//     review_cycle. A blocking review reopens the item; a review that cannot run (its activity
//     failed) is written to the gate log and the item stays done.
//
// Failures: an implementer whose activity fails for good (a non-retryable error) is recorded as a
// failed attempt at integration. When implementers fail only with retryable errors (an outage),
// the other branches are still integrated, then that error is returned so the mission parks as
// after a failed cycle. A budget refusal anywhere ends the mission. state.CyclesDone is advanced
// here, as each cycle-consuming activity completes, so a round that fails has already counted
// the checkpoints it committed.
//
// Cancellation (lha mission-abort): every activity (and researcher child) of a round is started
// with WaitForCancellation (python: WAIT_CANCELLATION_COMPLETED), so the workflow writes ABORTED
// only after the work in flight has acknowledged the cancel (an implementer's worktree removed
// and its spend journaled, a cycle's last row write landed); cancelRequested after each one keeps
// a cancel that an activity finished through from being lost.

// Organization timeouts and retry policies (python: org_round.py).
const (
	orgWorkTimeout = time.Hour
	orgHeartbeat   = 2 * time.Minute
	orgPlanTimeout = 5 * time.Minute
)

// ResearchTemplates are the researchers' objectives, one per researcher of an item ({task} is
// the item's description).
var ResearchTemplates = []string{
	"Find context relevant to: {task}",
	"Find existing files/code related to: {task}",
	"Find the tests and checks that cover: {task}",
	"Find the conventions and interfaces to respect for: {task}",
}

// cancelRequested is the re-raise of a workflow cancellation that a waited-for activity finished
// through (python: raise_if_cancel_requested): the activity handed its result back, but the
// mission was cancelled, and the abort must never be lost that way.
func cancelRequested(ctx workflow.Context) error {
	if ctx.Err() != nil {
		return temporal.NewCanceledError("mission cancelled while its work was finishing")
	}
	return nil
}

func isActivityError(err error) bool {
	var actErr *temporal.ActivityError
	return errors.As(err, &actErr)
}

func isBudgetError(err error) bool {
	app := applicationCause(err)
	return app != nil && app.Type() == ErrorBudgetExceeded
}

// workOptions are the options of every work activity of a round (python: _WORK_RETRY, ...).
func workOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: orgWorkTimeout,
		HeartbeatTimeout:    orgHeartbeat,
		RetryPolicy:         cycleRetry,
		WaitForCancellation: true,
	})
}

// itemResearch is one item's research: the briefs that arrived and every failure.
type itemResearch struct {
	briefs   []string
	failures []string
}

func (r *itemResearch) get() ([]string, []string) {
	if r == nil {
		return []string{}, []string{}
	}
	return append([]string{}, r.briefs...), append([]string{}, r.failures...)
}

// runOrgRound is one round (see above); it advances state.CyclesDone itself.
func (w *missionRun) runOrgRound(ctx workflow.Context) (CycleResult, error) {
	inp, st := w.inp, w.state
	remaining := max(1, inp.MaxCycles-st.CyclesDone)
	maxParallel := 0
	if inp.MaxParallel >= 2 {
		maxParallel = min(inp.MaxParallel, remaining)
	}
	pctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: orgPlanTimeout,
		RetryPolicy:         shortRetry,
		WaitForCancellation: true,
	})
	var plan RoundPlan
	if err := workflow.ExecuteActivity(pctx, ActivityPlanRound, RoundInput{
		MissionID: inp.MissionID, Workdir: inp.Workdir, MaxParallel: maxParallel,
	}).Get(ctx, &plan); err != nil {
		return CycleResult{}, err
	}
	if err := cancelRequested(ctx); err != nil {
		return CycleResult{}, err
	}
	first := fmt.Sprintf("c%d", st.CyclesDone+1)
	research, err := w.research(ctx, plan.Items, first)
	if err != nil {
		return CycleResult{}, err
	}
	if plan.Parallel {
		return w.wave(ctx, plan, research)
	}

	var researchItem *string
	briefs, failures := []string{}, []string{}
	if len(plan.Items) > 0 {
		id := plan.Items[0].ItemID
		if r, ok := research[id]; ok {
			researchItem = &id
			briefs, failures = r.get()
		}
	}
	var result CycleResult
	if err := workflow.ExecuteActivity(workOptions(ctx), ActivityRunAgentCycle, CycleInput{
		MissionID:        inp.MissionID,
		Workdir:          inp.Workdir,
		CycleID:          first,
		CheckCommands:    inp.CheckCommands,
		BudgetUSD:        inp.BudgetUSD,
		MaxCycles:        inp.MaxCycles,
		SteerNotes:       append([]string{}, st.SteerNotes...),
		ApprovedActions:  append([]ApprovedAction{}, st.ApprovedActions...),
		ResearchItem:     researchItem,
		ResearchBriefs:   briefs,
		ResearchFailures: failures,
	}).Get(ctx, &result); err != nil {
		return CycleResult{}, err
	}
	st.CyclesDone++
	if err := cancelRequested(ctx); err != nil {
		return CycleResult{}, err
	}
	return w.review(ctx, result, first)
}

// research runs ResearchPerItem researcher children per item: {item: briefs + failures} (nil when
// research is off).
func (w *missionRun) research(ctx workflow.Context, items []RoundItem, cycleID string) (map[string]*itemResearch, error) {
	inp := w.inp
	perItem := min(inp.ResearchPerItem, MaxResearchPerItem)
	if perItem <= 0 || len(items) == 0 {
		return nil, nil
	}
	var inputs []SubAgentInput
	var owners []string
	for _, item := range items {
		for _, template := range ResearchTemplates[:perItem] {
			inputs = append(inputs, SubAgentInput{
				RoleName:    "researcher",
				Objective:   strings.ReplaceAll(template, "{task}", item.Description),
				Workdir:     inp.Workdir,
				MissionID:   inp.MissionID,
				AllowEgress: true,
				BudgetUSD:   inp.BudgetUSD,
				CycleID:     cycleID + "-research",
			})
			owners = append(owners, item.ItemID)
		}
	}
	results, err := w.fanOutChildren(ctx, inputs)
	if err != nil {
		return nil, err
	}
	out := map[string]*itemResearch{}
	for _, item := range items {
		out[item.ItemID] = &itemResearch{briefs: []string{}, failures: []string{}}
	}
	for i, res := range results {
		r := out[owners[i]]
		switch {
		case res.failed:
			r.failures = append(r.failures, res.failure)
			w.log(ctx, fmt.Sprintf("research for %s failed: %s", owners[i], pyfmt.Head(res.failure, 300)))
		case res.output.Brief != "":
			r.briefs = append(r.briefs, res.output.Brief)
		}
	}
	return out, nil
}

// fanOutEntry is one child's outcome: its output, or a description of its failure.
type fanOutEntry struct {
	output  SubAgentOutput
	failure string
	failed  bool
}

// childID is a deterministic (replay-safe), never-colliding sub-agent child workflow id:
// "subagent:<mission>:<role>:<12 hex>" (python: workflow.uuid4().hex[:12]), derived from the run
// id and the number of children this run started.
func (w *missionRun) childID(ctx workflow.Context, inp SubAgentInput) string {
	w.children++
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", workflow.GetInfo(ctx).WorkflowExecution.RunID, w.children)))
	return fmt.Sprintf("subagent:%s:%s:%s", inp.MissionID, inp.RoleName, hex.EncodeToString(sum[:])[:12])
}

// fanOutChildren fans out sub-agent child workflows concurrently and waits for every one: one
// entry per input, in order — the sub-agent's output, or a description of its failure
// ("<role>: <error>"). A cancellation (the mission was cancelled: the children are cancelled with
// it and waited for) is returned, never swallowed.
func (w *missionRun) fanOutChildren(ctx workflow.Context, inputs []SubAgentInput) ([]fanOutEntry, error) {
	futures := make([]workflow.ChildWorkflowFuture, len(inputs))
	for i, inp := range inputs {
		cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			WorkflowID:          w.childID(ctx, inp),
			WaitForCancellation: true,
		})
		futures[i] = workflow.ExecuteChildWorkflow(cctx, WorkflowSubAgent, inp)
	}
	out := make([]fanOutEntry, len(inputs))
	var cancelled error
	for i, f := range futures {
		var o SubAgentOutput
		err := f.Get(ctx, &o)
		switch {
		case err == nil:
			out[i] = fanOutEntry{output: o}
		case temporal.IsCanceledError(err):
			if cancelled == nil {
				cancelled = err
			}
		default:
			out[i] = fanOutEntry{failure: describeFailure(inputs[i].RoleName, err), failed: true}
		}
	}
	if cancelled != nil {
		return nil, cancelled
	}
	if err := cancelRequested(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// describeFailure is "<role>: <error type>: <message>" of the innermost cause below the child
// workflow and activity wrappers (python: _describe).
func describeFailure(role string, err error) string {
	cause := err
	for {
		var next error
		switch c := cause.(type) {
		case *temporal.ChildWorkflowExecutionError:
			next = errors.Unwrap(c)
		case *temporal.ActivityError:
			next = errors.Unwrap(c)
		}
		if next == nil {
			break
		}
		cause = next
	}
	name, message := pyTypeName(cause), cause.Error()
	switch c := cause.(type) {
	case *temporal.ApplicationError:
		name, message = "ApplicationError", c.Message()
	case *temporal.TimeoutError:
		name, message = "TimeoutError", c.Message()
	case *temporal.CanceledError:
		name = "CancelledError"
	case *temporal.TerminatedError:
		name = "TerminatedError"
	}
	return role + ": " + name + ": " + message
}

// review reviews a verified item and folds the verdict into the cycle's result.
func (w *missionRun) review(ctx workflow.Context, result CycleResult, cycleID string) (CycleResult, error) {
	inp := w.inp
	if !(inp.Review && result.Advanced && result.Verdict == "passed" && result.ItemID != nil) {
		return result, nil
	}
	itemID := *result.ItemID
	var rev CycleResult
	err := workflow.ExecuteActivity(workOptions(ctx), ActivityReviewCycle, ReviewInput{
		MissionID: inp.MissionID,
		Workdir:   inp.Workdir,
		CycleID:   cycleID,
		ItemID:    itemID,
		HeadSHA:   result.HeadSHA,
		BaseSHA:   result.BaseSHA,
		BudgetUSD: inp.BudgetUSD,
		MaxCycles: inp.MaxCycles,
	}).Get(ctx, &rev)
	if err == nil {
		err = cancelRequested(ctx)
		if err != nil {
			return CycleResult{}, err
		}
	} else {
		if temporal.IsCanceledError(err) || isBudgetError(err) || !isActivityError(err) {
			return CycleResult{}, err
		}
		w.log(ctx, fmt.Sprintf("review of %s (%s) failed: %s; left unreviewed", itemID, cycleID, causeString(err)))
		return result, nil
	}
	if !rev.Advanced {
		return result, nil
	}
	if rev.Verdict != "passed" {
		how := "reopened it"
		if rev.ItemBlocked {
			how = "blocked it"
		}
		w.log(ctx, fmt.Sprintf("review of %s (%s) %s", itemID, cycleID, how))
	}
	out := result
	if rev.HeadSHA != "" {
		out.HeadSHA = rev.HeadSHA
	}
	out.IsComplete = rev.IsComplete
	out.IsDeadlocked = rev.IsDeadlocked
	out.ItemsDone = rev.ItemsDone
	out.ItemsTotal = rev.ItemsTotal
	if rev.IsDeadlocked {
		out.Reason = rev.Reason
	}
	if rev.Verdict != "passed" {
		out.Verdict = rev.Verdict
	}
	out.ItemBlocked = rev.ItemBlocked
	out.SpentUSD = result.SpentUSD + rev.SpentUSD
	out.Note = result.Note + "; review: " + rev.Verdict
	return out, nil
}

// wave runs a parallel implementer wave, then integrates (and reviews) its branches one at a
// time in checklist order.
func (w *missionRun) wave(ctx workflow.Context, plan RoundPlan, research map[string]*itemResearch) (CycleResult, error) {
	inp, st := w.inp, w.state
	start := st.CyclesDone + 1
	cycleIDs := make([]string, len(plan.Items))
	ids := make([]string, len(plan.Items))
	for n, item := range plan.Items {
		cycleIDs[n] = fmt.Sprintf("c%d", start+n)
		ids[n] = item.ItemID
	}
	w.log(ctx, fmt.Sprintf("parallel wave %s at %s", strings.Join(ids, ", "), pyfmt.Head(plan.HeadSHA, 12)))
	wctx := workOptions(ctx)
	futures := make([]workflow.Future, len(plan.Items))
	for n, item := range plan.Items {
		briefs, failures := research[item.ItemID].get()
		futures[n] = workflow.ExecuteActivity(wctx, ActivityRunImplementer, ImplementerInput{
			MissionID:        inp.MissionID,
			Workdir:          inp.Workdir,
			CycleID:          cycleIDs[n],
			ItemID:           item.ItemID,
			BaseSHA:          plan.HeadSHA,
			CheckCommands:    inp.CheckCommands,
			BudgetUSD:        inp.BudgetUSD,
			MaxCycles:        inp.MaxCycles,
			SteerNotes:       append([]string{}, st.SteerNotes...),
			ApprovedActions:  append([]ApprovedAction{}, st.ApprovedActions...),
			ResearchBriefs:   briefs,
			ResearchFailures: failures,
		})
	}
	// Every implementer is waited for (a cancelled one acknowledges first), like asyncio.gather.
	outputs := make([]ImplementerOutput, len(futures))
	errs := make([]error, len(futures))
	for n, f := range futures {
		errs[n] = f.Get(ctx, &outputs[n])
	}
	if err := cancelRequested(ctx); err != nil {
		return CycleResult{}, err
	}

	var deferred error
	var last *CycleResult
	pending := []PendingApproval{}
	used := []string{}
	for n, item := range plan.Items {
		var output *ImplementerOutput
		errText := ""
		switch err := errs[n]; {
		case err == nil:
			o := outputs[n]
			output = &o
			pending = append(pending, o.PendingApprovals...)
			used = append(used, o.UsedApprovals...)
		case isActivityError(err) && !temporal.IsCanceledError(err):
			if isBudgetError(err) {
				return CycleResult{}, err
			}
			app := applicationCause(err)
			if app == nil || !app.NonRetryable() { // transient: retry in a later round
				if deferred == nil {
					deferred = err
				}
				w.log(ctx, fmt.Sprintf("implementer for %s failed (retryable): %s", item.ItemID, causeString(err)))
				continue
			}
			kind := app.Type()
			if kind == "" {
				kind = "error"
			}
			errText = pyfmt.Head(kind+": "+app.Message(), 500)
		default:
			return CycleResult{}, err
		}
		briefs, failures := research[item.ItemID].get()
		var result CycleResult
		if err := workflow.ExecuteActivity(wctx, ActivityIntegrateBranch, IntegrateInput{
			MissionID:        inp.MissionID,
			Workdir:          inp.Workdir,
			CycleID:          cycleIDs[n],
			ItemID:           item.ItemID,
			BaseSHA:          plan.HeadSHA,
			Output:           output,
			Error:            errText,
			CheckCommands:    inp.CheckCommands,
			BudgetUSD:        inp.BudgetUSD,
			MaxCycles:        inp.MaxCycles,
			ResearchBriefs:   len(briefs),
			ResearchFailures: failures,
		}).Get(ctx, &result); err != nil {
			return CycleResult{}, err
		}
		if result.Advanced {
			st.CyclesDone++
		}
		if err := cancelRequested(ctx); err != nil {
			return CycleResult{}, err
		}
		reviewed, err := w.review(ctx, result, cycleIDs[n])
		if err != nil {
			return CycleResult{}, err
		}
		last = &reviewed
	}
	if deferred != nil {
		return CycleResult{}, deferred // an outage: the mission parks, then a later round redoes those items
	}
	out := *last // every item was integrated or deferred
	out.PendingApprovals = pending
	out.UsedApprovals = used
	return out, nil
}
