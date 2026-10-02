package durable

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/calvinchengx/long-horizon-agent/go/internal/hitl"
	"github.com/calvinchengx/long-horizon-agent/go/internal/ops"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// MissionWorkflow is the durable control plane of one mission (python:
// lha.durable.workflows.MissionWorkflow). A deterministic scheduler — no LLM, tool or IO here. It
// repeatedly dispatches one run_agent_cycle activity and ends with an explicit outcome:
//
//	completed        every checklist item verified done
//	deadlocked       items remain but none is actionable, and no deadlock gate is configured
//	                 (or "retry" unblocked nothing)
//	aborted          "abort" at the deadlock gate (by a human or its default)
//	impossible       "impossible" at the deadlock gate: a final checkpoint records it
//	budget_exhausted the budget governor refused a model call
//	max_cycles       the iteration ceiling was hit
//
// A cycle that keeps failing with retryable errors PARKS the mission (DEGRADED_PARK): a durable
// sleep with capped exponential backoff, probing dependency health until it recovers.
// Non-retryable errors (budget, configuration) end or fail the mission. Continue-As-New keeps
// the history bounded; everything the workflow needs rides in MissionInput.State.
//
// Human gates (WAITING_ON_HUMAN): an irreversible tool call a cycle queued (approve / reject,
// default reject) and, when deadlock_gate_seconds > 0, a deadlock (retry / abort / impossible,
// default deadlock_gate_default). Every gate has a timeout, a default and an escalation ladder
// (reminders at gate_escalation_seconds, each recorded by the notify_gate activity). SLEEPING is
// the status on a durable timer by design (scheduled start, pause between cycles, snooze).
//
// The missions row: the workflow writes the statuses only it knows (SLEEPING, DEGRADED_PARK,
// WAITING_ON_HUMAN when a gate opens, and the final status of every ending, including a failure
// and a cancellation) through record_mission_status, best effort. A cancellation waits for the
// cycle in flight to acknowledge it (WaitForCancellation, python's WAIT_CANCELLATION_COMPLETED)
// before ABORTED is written.
//
// The multi-agent organization (opt-in per mission: MissionInput.ResearchPerItem, Review,
// MaxParallel) replaces the single cycle activity with one round of org_round.go: researcher
// child workflows before the round, a serial Lead cycle or a parallel implementer wave (one
// activity per implementer, in its own git worktree, then one integration activity per branch),
// and an independent review after every verified item. With all three off the workflow issues
// exactly the commands it always did.
//
// The behaviour is Python's latest (every workflow.patched() branch taken); behaviour added to the
// Go workflow later is guarded by workflow.GetVersion (VersionOrg); see the package doc.
func MissionWorkflow(ctx workflow.Context, inp MissionInput) (MissionResult, error) {
	w := newMissionRun(ctx, inp)
	result, err := w.runMission(ctx)
	if err == nil || isContinueAsNew(err) {
		return result, err
	}
	reason := ""
	if temporal.IsCanceledError(err) || errors.Is(ctx.Err(), workflow.ErrCanceled) {
		reason = "mission cancelled"
	} else {
		reason = "mission failed: " + errorMessage(err)
	}
	w.state.Status = StatusAborted
	dctx, _ := workflow.NewDisconnectedContext(ctx)
	_ = w.recordRow(dctx, StatusAborted, reason)
	return MissionResult{}, err
}

// Retry policies and timeouts are part of the recorded commands (docs/14 "How a cycle runs").
var (
	cycleRetry = &temporal.RetryPolicy{
		InitialInterval:        time.Second,
		BackoffCoefficient:     2.0,
		MaximumInterval:        30 * time.Second,
		MaximumAttempts:        5,
		NonRetryableErrorTypes: []string{ErrorBudgetExceeded, ErrorConfig},
	}
	oneShot    = &temporal.RetryPolicy{MaximumAttempts: 1}
	shortRetry = &temporal.RetryPolicy{MaximumAttempts: 3}
	rowRetry   = &temporal.RetryPolicy{
		InitialInterval:        time.Second,
		MaximumInterval:        5 * time.Second,
		MaximumAttempts:        3,
		NonRetryableErrorTypes: []string{ErrorConfig},
	}
)

// Workflow constants (python: workflows.py).
const (
	CycleStartToClose     = time.Hour
	CycleHeartbeatTimeout = 2 * time.Minute
	MaxSteerNotes         = 20
	MaxSteerChars         = 2000
	MaxGateLog            = 50
	notifyTimeout         = 3 * time.Minute
	rowTimeout            = 30 * time.Second
	rowDeadline           = 2 * time.Minute
)

var terminalStatus = map[string]string{
	OutcomeCompleted:       StatusDone,
	OutcomeDeadlocked:      StatusImpossible,
	OutcomeImpossible:      StatusImpossible,
	OutcomeBudgetExhausted: StatusAborted,
	OutcomeMaxCycles:       StatusAborted,
	OutcomeAborted:         StatusAborted,
}

type missionRun struct {
	inp               MissionInput
	state             *MissionState
	parkReason        string
	rejectedDecisions []string
	openQuestion      string
	gate              *GateView
	children          int // sub-agent children started by this run (their ids)

	decisionCh workflow.ReceiveChannel
	steerCh    workflow.ReceiveChannel
	snoozeCh   workflow.ReceiveChannel
}

func newMissionRun(ctx workflow.Context, inp MissionInput) *missionRun {
	// Signals that arrived before the run started land in an "early" state and are merged into
	// the carried state exactly as Python merges them (python: MissionWorkflow.__init__ + run).
	early := NewMissionState()
	w := &missionRun{inp: inp, state: &early}
	w.registerQueries(ctx)
	w.decisionCh = workflow.GetSignalChannel(ctx, SignalHumanDecision)
	w.steerCh = workflow.GetSignalChannel(ctx, SignalSteer)
	w.snoozeCh = workflow.GetSignalChannel(ctx, SignalSnooze)
	w.drainSignals(ctx)

	st := NewMissionState()
	if inp.State != nil {
		st = copyState(*inp.State)
	}
	if early.PendingDecision != nil {
		st.PendingDecision = early.PendingDecision
	}
	st.SteerNotes = lastN(append(append([]string{}, st.SteerNotes...), early.SteerNotes...), MaxSteerNotes)
	if inp.State == nil {
		st.ResumeAt = math.Max(inp.ResumeAt, early.ResumeAt)
	} else if early.ResumeAt != 0 {
		st.ResumeAt = early.ResumeAt
	}
	w.state = &st

	// Later signals: one coroutine applies them in arrival order.
	workflow.Go(ctx, func(gctx workflow.Context) {
		for {
			sel := workflow.NewSelector(gctx)
			w.addSignalHandlers(gctx, sel)
			sel.Select(gctx)
		}
	})
	return w
}

func copyState(s MissionState) MissionState {
	c := s
	c.SteerNotes = append([]string{}, s.SteerNotes...)
	c.ApprovedActions = append([]ApprovedAction{}, s.ApprovedActions...)
	c.RejectedActions = append([]string{}, s.RejectedActions...)
	c.GateLog = append([]string{}, s.GateLog...)
	return c
}

func lastN(s []string, n int) []string {
	if len(s) > n {
		return append([]string{}, s[len(s)-n:]...)
	}
	return s
}

func (w *missionRun) addSignalHandlers(ctx workflow.Context, sel workflow.Selector) {
	sel.AddReceive(w.decisionCh, func(c workflow.ReceiveChannel, _ bool) {
		var v any
		c.Receive(ctx, &v)
		w.onHumanDecision(v)
	})
	sel.AddReceive(w.steerCh, func(c workflow.ReceiveChannel, _ bool) {
		var v any
		c.Receive(ctx, &v)
		w.onSteer(v)
	})
	sel.AddReceive(w.snoozeCh, func(c workflow.ReceiveChannel, _ bool) {
		var v any
		c.Receive(ctx, &v)
		w.onSnooze(ctx, v)
	})
}

// drainSignals applies every buffered signal now (before the run starts and before
// Continue-As-New: a Go workflow loses signals left unread in its channels).
func (w *missionRun) drainSignals(ctx workflow.Context) {
	for {
		var v any
		switch {
		case w.decisionCh.ReceiveAsync(&v):
			w.onHumanDecision(v)
		case w.steerCh.ReceiveAsync(&v):
			w.onSteer(v)
		case w.snoozeCh.ReceiveAsync(&v):
			w.onSnooze(ctx, v)
		default:
			return
		}
	}
}

// onHumanDecision holds a decision until a gate consumes it (early signals are kept, and
// survive Continue-As-New); a gate only accepts one of the options it offered. A value that is
// not a string is dropped (python: a signal whose argument cannot be converted is dropped).
func (w *missionRun) onHumanDecision(v any) {
	s, ok := v.(string)
	if !ok {
		return
	}
	d := pyfmt.PyStrip(s)
	w.state.PendingDecision = &d
}

// onSteer appends an operator steering note; every following cycle's prompt includes it.
func (w *missionRun) onSteer(v any) {
	s, ok := v.(string)
	if !ok {
		return
	}
	note := headRunes(pyfmt.PyStrip(s), MaxSteerChars)
	if note != "" {
		w.state.SteerNotes = lastN(append(append([]string{}, w.state.SteerNotes...), note), MaxSteerNotes)
	}
}

// onSnooze sleeps (SLEEPING) before the next cycle for seconds; 0 wakes a sleeping mission.
func (w *missionRun) onSnooze(ctx workflow.Context, v any) {
	f, ok := v.(float64) // JSON numbers decode as float64
	if !ok || f != math.Trunc(f) {
		return // python: snooze(seconds: int)
	}
	if f > 0 {
		w.state.ResumeAt = epoch(workflow.Now(ctx)) + f
	} else {
		w.state.ResumeAt = 0
	}
}

func (w *missionRun) registerQueries(ctx workflow.Context) {
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	must(workflow.SetQueryHandler(ctx, QueryStatus, func() (string, error) { return w.state.Status, nil }))
	must(workflow.SetQueryHandler(ctx, QueryCycles, func() (int, error) { return w.state.CyclesDone, nil }))
	must(workflow.SetQueryHandler(ctx, QueryLastItem, func() (*string, error) { return w.state.LastItem, nil }))
	must(workflow.SetQueryHandler(ctx, QueryParkReason, func() (string, error) { return w.parkReason, nil }))
	must(workflow.SetQueryHandler(ctx, QueryGate, func() (*GateView, error) { return w.gate, nil }))
	must(workflow.SetQueryHandler(ctx, QueryGateLog, func() ([]string, error) {
		return append([]string{}, w.state.GateLog...), nil
	}))
	must(workflow.SetQueryHandler(ctx, QueryResumeAt, func() (float64, error) { return w.state.ResumeAt, nil }))
	must(workflow.SetQueryHandler(ctx, QueryOpenQuestion, func() (string, error) { return w.openQuestion, nil }))
	must(workflow.SetQueryHandler(ctx, QuerySteerNotes, func() ([]string, error) {
		return append([]string{}, w.state.SteerNotes...), nil
	}))
	must(workflow.SetQueryHandler(ctx, QueryRejectedDecisions, func() ([]string, error) {
		return append([]string{}, w.rejectedDecisions...), nil
	}))
}

// --- the mission loop ---------------------------------------------------------------------

func (w *missionRun) runMission(ctx workflow.Context) (MissionResult, error) {
	inp, st := w.inp, w.state
	if inp.CheckCommands != nil {
		empty := true
		for _, c := range inp.CheckCommands {
			empty = empty && len(c) == 0
		}
		if empty {
			return MissionResult{}, configError("check_commands is empty: at least one gating check is required; omit it to use "+
				"the default Python checks", nil)
		}
	}
	if inp.CyclesBeforeCAN < 1 {
		return MissionResult{}, configError(fmt.Sprintf("cycles_before_can must be >= 1 (got %d)", inp.CyclesBeforeCAN), nil)
	}
	if inp.MaxCycles < 0 {
		return MissionResult{}, configError(fmt.Sprintf("max_cycles must be >= 0 (got %d)", inp.MaxCycles), nil)
	}
	if msg := OrgConfigError(inp); msg != "" {
		return MissionResult{}, configError(msg, nil)
	}
	org := false
	if OrgEnabled(inp) {
		// The organization is new in the Go workflow: a history recorded before it refused the
		// options (DefaultVersion) and must replay down that path.
		if workflow.GetVersion(ctx, VersionOrg, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
			return MissionResult{}, configError(ErrOrgNotPorted, nil)
		}
		org = true
	}

	for {
		if st.CyclesDone >= inp.MaxCycles {
			return w.terminal(ctx, OutcomeMaxCycles, "iteration ceiling reached")
		}
		if err := w.sleepUntilResume(ctx); err != nil {
			return MissionResult{}, err
		}
		st.Status = StatusRunning
		cyclesBefore := st.CyclesDone
		var result CycleResult
		var err error
		if org { // research / review / parallel waves (org_round.go)
			result, err = w.runOrgRound(ctx)
		} else {
			actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
				StartToCloseTimeout: CycleStartToClose,
				HeartbeatTimeout:    CycleHeartbeatTimeout,
				RetryPolicy:         cycleRetry,
				// A cancelled cycle is waited for (its last row write lands before ABORTED).
				WaitForCancellation: true,
			})
			err = workflow.ExecuteActivity(actx, ActivityRunAgentCycle, CycleInput{
				MissionID:       inp.MissionID,
				Workdir:         inp.Workdir,
				CycleID:         fmt.Sprintf("c%d", st.CyclesDone+1),
				CheckCommands:   inp.CheckCommands,
				BudgetUSD:       inp.BudgetUSD,
				MaxCycles:       inp.MaxCycles,
				SteerNotes:      append([]string{}, st.SteerNotes...),
				ApprovedActions: append([]ApprovedAction{}, st.ApprovedActions...),
			}).Get(ctx, &result)
		}
		if err == nil {
			// The work finished although the mission was cancelled: the cancellation stands.
			err = cancelRequested(ctx)
		}
		if err != nil {
			if temporal.IsCanceledError(err) || !isActivityError(err) {
				return MissionResult{}, err // the mission was cancelled: never park on it
			}
			if app := applicationCause(err); app != nil {
				if app.Type() == ErrorBudgetExceeded {
					return w.terminal(ctx, OutcomeBudgetExhausted, app.Message())
				}
				if app.NonRetryable() {
					st.Status = StatusAborted
					return MissionResult{}, temporal.NewNonRetryableApplicationError(
						fmt.Sprintf("mission %s failed: %s", inp.MissionID, app.Message()), app.Type(), err)
				}
			}
			// Retries exhausted on a transient failure: park instead of failing the mission.
			if err := w.park(ctx, causeString(err)); err != nil {
				return MissionResult{}, err
			}
			if err := w.maybeContinueAsNew(ctx); err != nil {
				return MissionResult{}, err
			}
			continue
		}

		if !org {
			st.CyclesDone++ // an org round counts its own cycles as it goes
		}
		w.absorb(result)
		w.trackFailures(result)
		skip := result.IsComplete && len(result.PendingApprovals) > 0 &&
			workflow.GetVersion(ctx, VersionCompleteSkipsApprovals, workflow.DefaultVersion, 1) != workflow.DefaultVersion
		if !skip { // no later cycle could use an approval the completing cycle queued
			if err := w.resolveApprovals(ctx, result); err != nil {
				return MissionResult{}, err
			}
		}
		if result.IsComplete {
			return w.terminal(ctx, OutcomeCompleted, "")
		}
		if result.IsDeadlocked {
			outcome, reason, keepGoing, err := w.onDeadlock(ctx, result)
			if err != nil {
				return MissionResult{}, err
			}
			if keepGoing {
				continue
			}
			return w.terminal(ctx, outcome, reason)
		}
		if inp.CyclePauseSeconds > 0 {
			wake := epoch(workflow.Now(ctx)) + float64(inp.CyclePauseSeconds)
			st.ResumeAt = math.Max(st.ResumeAt, wake)
		}
		cbc := inp.CyclesBeforeCAN
		if org { // a wave can advance several cycles: CAN when a multiple is crossed
			if st.CyclesDone/cbc > cyclesBefore/cbc {
				return MissionResult{}, w.continueAsNew(ctx)
			}
		} else if st.CyclesDone%cbc == 0 {
			return MissionResult{}, w.continueAsNew(ctx)
		}
		if err := w.maybeContinueAsNew(ctx); err != nil {
			return MissionResult{}, err
		}
	}
}

func (w *missionRun) continueAsNew(ctx workflow.Context) error {
	w.drainSignals(ctx)
	next := w.inp
	st := copyState(*w.state)
	next.State = &st
	return workflow.NewContinueAsNewError(ctx, WorkflowMission, next)
}

func (w *missionRun) maybeContinueAsNew(ctx workflow.Context) error {
	if workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
		return w.continueAsNew(ctx)
	}
	return nil
}

func isContinueAsNew(err error) bool {
	var can *workflow.ContinueAsNewError
	return errors.As(err, &can)
}

// applicationCause is the ApplicationError an activity failed with (python: err.cause).
func applicationCause(err error) *temporal.ApplicationError {
	var actErr *temporal.ActivityError
	if !errors.As(err, &actErr) {
		return nil
	}
	app, _ := errors.Unwrap(actErr).(*temporal.ApplicationError)
	return app
}

// causeString is python's str(err.cause or err).
func causeString(err error) string {
	var actErr *temporal.ActivityError
	if errors.As(err, &actErr) {
		if cause := errors.Unwrap(actErr); cause != nil {
			return errorMessage(cause)
		}
	}
	return errorMessage(err)
}

// errorMessage is python's str(err): an ApplicationError's message, else the error text.
func errorMessage(err error) string {
	if app, ok := err.(*temporal.ApplicationError); ok {
		return app.Message()
	}
	return err.Error()
}

func (w *missionRun) absorb(result CycleResult) {
	st := w.state
	if result.HeadSHA != "" {
		st.HeadSHA = result.HeadSHA
	}
	st.ItemsDone = result.ItemsDone
	st.ItemsTotal = result.ItemsTotal
	if result.ItemID != nil {
		st.LastItem = strPtr(*result.ItemID)
	}
}

// trackFailures counts consecutive non-passing verified attempts on the same item.
func (w *missionRun) trackFailures(result CycleResult) {
	st := w.state
	if !result.Advanced || result.ItemID == nil {
		return
	}
	switch {
	case result.Verdict == "passed":
		st.FailItem, st.FailStreak = nil, 0
	case st.FailItem != nil && *result.ItemID == *st.FailItem:
		st.FailStreak++
	default:
		st.FailItem, st.FailStreak = strPtr(*result.ItemID), 1
	}
}

// resolveApprovals asks a human about each irreversible action the cycle attempted (durably, one
// by one). Approved actions are passed to the following cycles and allowed ONCE (by
// fingerprint); rejected ones are remembered so the same request is not asked again.
func (w *missionRun) resolveApprovals(ctx workflow.Context, result CycleResult) error {
	st := w.state
	used := map[string]bool{}
	for _, fp := range result.UsedApprovals {
		used[fp] = true
	}
	kept := []ApprovedAction{}
	for _, a := range st.ApprovedActions {
		if !used[a.Fingerprint] {
			kept = append(kept, a)
		}
	}
	st.ApprovedActions = kept
	known := map[string]bool{}
	for _, a := range st.ApprovedActions {
		known[a.Fingerprint] = true
	}
	for _, fp := range st.RejectedActions {
		known[fp] = true
	}
	for _, req := range result.PendingApprovals {
		if known[req.Fingerprint] {
			continue
		}
		known[req.Fingerprint] = true
		summary := pyfmt.PyStrip(req.Tool + " " + req.Arguments)
		question := fmt.Sprintf("Mission %s wants to run an irreversible action: %s (%s). Approve or reject?",
			w.inp.MissionID, summary, req.Reason)
		r := req
		decision, _, err := w.runGate(ctx, &GateView{
			GateID:        "approval-" + headRunes(req.Fingerprint, 12),
			Kind:          GateToolCall,
			Question:      question,
			Options:       append([]string{}, ApprovalOptions...),
			DefaultAction: "reject",
			Request:       &r,
		}, w.inp.ApprovalTimeoutSeconds)
		if err != nil {
			return err
		}
		if decision == "approve" {
			st.ApprovedActions = append(st.ApprovedActions, ApprovedAction{Fingerprint: req.Fingerprint, Summary: headRunes(summary, 500)})
		} else {
			st.RejectedActions = append(st.RejectedActions, req.Fingerprint)
		}
	}
	return nil
}

// park durably sleeps with capped exponential backoff until the dependencies are healthy again.
func (w *missionRun) park(ctx workflow.Context, reason string) error {
	st := w.state
	st.Status = StatusDegradedPark
	st.Parks++
	w.parkReason = reason
	if err := w.recordRow(ctx, StatusDegradedPark, reason); err != nil {
		return err
	}
	capS := max(1, w.inp.ParkMaxSeconds)
	delay := min(max(1, w.inp.ParkInitialSeconds), capS)
	hctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         oneShot,
	})
	for {
		if err := workflow.Sleep(ctx, time.Duration(delay)*time.Second); err != nil {
			return err
		}
		var report HealthReport
		err := workflow.ExecuteActivity(hctx, ActivityCheckMissionHealth, HealthInput{
			MissionID: w.inp.MissionID, Workdir: w.inp.Workdir,
		}).Get(ctx, &report)
		if err != nil {
			if temporal.IsCanceledError(err) {
				return err
			}
			w.parkReason = "health check failed: " + causeString(err)
		} else if report.Healthy {
			break
		} else {
			w.parkReason = report.Reason
		}
		delay = min(delay*2, capS)
		if err := w.maybeContinueAsNew(ctx); err != nil {
			return err
		}
	}
	st.Status = StatusRunning
	w.parkReason = ""
	return nil
}

func (w *missionRun) log(ctx workflow.Context, line string) {
	w.state.GateLog = lastN(append(append([]string{}, w.state.GateLog...), isoSeconds(workflow.Now(ctx))+" "+line), MaxGateLog)
}

// sleepUntilResume is SLEEPING: a durable timer until resume_at (a snooze can move or end it).
func (w *missionRun) sleepUntilResume(ctx workflow.Context) error {
	st := w.state
	if st.ResumeAt <= epoch(workflow.Now(ctx)) {
		return nil
	}
	st.Status = StatusSleeping
	until := isoSeconds(fromEpoch(st.ResumeAt))
	w.log(ctx, "sleeping until "+until)
	if err := w.recordRow(ctx, StatusSleeping, "sleeping until "+until); err != nil {
		return err
	}
	for {
		target := st.ResumeAt
		remaining := target - epoch(workflow.Now(ctx))
		if remaining <= 0 {
			break
		}
		moved, err := workflow.AwaitWithTimeout(ctx, seconds(remaining), func() bool { return st.ResumeAt != target })
		if err != nil {
			return err
		}
		if !moved {
			break
		}
	}
	st.ResumeAt = 0
	st.Status = StatusRunning
	w.log(ctx, "woke up")
	return nil
}

// notify records a gate event in the anchor + webhook (an activity); never fails the gate.
func (w *missionRun) notify(ctx workflow.Context, view *GateView, event, decision string, step int) error {
	nctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: notifyTimeout,
		RetryPolicy:         shortRetry,
	})
	var result NoticeResult
	err := workflow.ExecuteActivity(nctx, ActivityNotifyGate, GateNotice{
		MissionID:     w.inp.MissionID,
		Workdir:       w.inp.Workdir,
		GateID:        view.GateID,
		Kind:          view.Kind,
		Event:         event,
		Question:      view.Question,
		Options:       append([]string{}, view.Options...),
		DefaultAction: view.DefaultAction,
		Decision:      decision,
		Step:          step,
		Deadline:      view.Deadline,
		Request:       view.Request,
		At:            isoSeconds(workflow.Now(ctx)),
	}).Get(ctx, &result)
	if err != nil {
		if temporal.IsCanceledError(err) {
			return err
		}
		w.log(ctx, fmt.Sprintf("gate %s: notification failed: %s", view.GateID, causeString(err)))
	}
	return nil
}

// takeDecision consumes the held human_decision; invalid ones are recorded and discarded.
func (w *missionRun) takeDecision(options []string) (string, bool) {
	allowed := map[string]string{}
	for _, opt := range options {
		allowed[strings.ToLower(pyfmt.PyStrip(opt))] = opt
	}
	st := w.state
	for st.PendingDecision != nil {
		pending := *st.PendingDecision
		st.PendingDecision = nil // consumed
		if choice, ok := allowed[strings.ToLower(pyfmt.PyStrip(pending))]; ok {
			return choice, true
		}
		w.rejectedDecisions = append(w.rejectedDecisions, pending)
	}
	return "", false
}

// runGate opens view (WAITING_ON_HUMAN) and walks the escalation ladder until a decision: a
// valid human_decision (held decisions count), or the default after reminders at each
// gate_escalation_seconds offset inside the timeout. Returns (decision, defaulted).
func (w *missionRun) runGate(ctx workflow.Context, view *GateView, timeoutSeconds int) (string, bool, error) {
	st := w.state
	previous := st.Status
	st.Status = StatusWaitingOnHuman
	timeout := float64(max(1, timeoutSeconds))
	opened := workflow.Now(ctx)
	steps := make([]float64, len(w.inp.GateEscalationSeconds))
	for i, s := range w.inp.GateEscalationSeconds {
		steps[i] = float64(s)
	}
	schedule := hitl.EscalationSchedule(timeout, steps)
	view.OpenedAt = isoSeconds(opened)
	view.Deadline = isoSeconds(opened.Add(seconds(timeout)))
	view.NextEscalationAt = ""
	if len(schedule) > 0 {
		view.NextEscalationAt = isoSeconds(opened.Add(seconds(schedule[0])))
	}
	w.gate = view
	w.openQuestion = fmt.Sprintf("%s [%s]", view.Question, strings.Join(view.Options, " / "))
	defer func() {
		st.Status = previous
		w.gate = nil
		w.openQuestion = ""
	}()
	ready := func() bool { return st.PendingDecision != nil }
	w.log(ctx, fmt.Sprintf("%s gate %s opened (default %s)", view.Kind, view.GateID, view.DefaultAction))
	if err := w.recordRow(ctx, StatusWaitingOnHuman, fmt.Sprintf("%s gate %s", view.Kind, view.GateID)); err != nil {
		return "", false, err
	}
	if err := w.notify(ctx, view, "opened", "", 0); err != nil {
		return "", false, err
	}
	sent := 0
	for {
		if choice, ok := w.takeDecision(view.Options); ok {
			w.log(ctx, fmt.Sprintf("%s gate %s resolved: %s", view.Kind, view.GateID, choice))
			if err := w.notify(ctx, view, "resolved", choice, 0); err != nil {
				return "", false, err
			}
			return choice, false, nil
		}
		elapsed := workflow.Now(ctx).Sub(opened).Seconds()
		rung := hitl.NextRung(elapsed, timeout, schedule, sent)
		if rung.WaitSeconds > 0 {
			arrived, err := workflow.AwaitWithTimeout(ctx, seconds(rung.WaitSeconds), ready)
			if err != nil {
				return "", false, err
			}
			if arrived {
				continue // a decision arrived: validate it at the top
			}
		}
		if rung.Step == 0 {
			def := view.DefaultAction
			w.log(ctx, fmt.Sprintf("%s gate %s timed out: default %s", view.Kind, view.GateID, def))
			if err := w.notify(ctx, view, "defaulted", def, 0); err != nil {
				return "", false, err
			}
			return def, true, nil
		}
		sent = rung.Step
		st.Escalations++
		view.EscalationsSent = sent
		view.NextEscalationAt = ""
		if sent < len(schedule) {
			view.NextEscalationAt = isoSeconds(opened.Add(seconds(schedule[sent])))
		}
		w.log(ctx, fmt.Sprintf("%s gate %s: reminder %d (escalation)", view.Kind, view.GateID, sent))
		if err := w.notify(ctx, view, "reminder", "", sent); err != nil {
			return "", false, err
		}
	}
}

// onDeadlock returns keepGoing (blocked items were retried) or the ending (outcome, reason).
func (w *missionRun) onDeadlock(ctx workflow.Context, result CycleResult) (string, string, bool, error) {
	reason := result.Reason
	if reason == "" {
		reason = "no actionable item"
	}
	if w.inp.DeadlockGateSeconds <= 0 {
		return OutcomeDeadlocked, reason, false, nil
	}
	st := w.state
	recommended := ""
	if ops.ShouldDeclareImpossible(st.FailStreak, w.inp.ImpossibleAfterFailures) {
		recommended = "impossible"
	}
	def := strings.ToLower(pyfmt.PyStrip(w.inp.DeadlockGateDefault))
	if !contains(DeadlockDefaults, def) {
		def = "abort"
	}
	advice := ""
	if recommended != "" {
		advice = fmt.Sprintf(" Recommended: impossible (item %s failed %d times in a row).", pyNone(st.FailItem), st.FailStreak)
	}
	decision, defaulted, err := w.runGate(ctx, &GateView{
		GateID: fmt.Sprintf("deadlock-%d", st.CyclesDone),
		Kind:   GateDeadlock,
		Question: fmt.Sprintf("Mission %s is deadlocked (%s). Retry the blocked items, abort, or declare the "+
			"mission impossible?%s", w.inp.MissionID, reason, advice),
		Options:       append([]string{}, DeadlockOptions...),
		DefaultAction: def,
		Recommended:   recommended,
	}, w.inp.DeadlockGateSeconds)
	if err != nil {
		return "", "", false, err
	}
	how := "by a human"
	if defaulted {
		how = "by default (no human answered)"
	}
	switch decision {
	case "retry":
		advanced, err := w.unblock(ctx)
		if err != nil {
			return "", "", false, err
		}
		if advanced {
			return "", "", true, nil
		}
		return OutcomeDeadlocked, reason, false, nil
	case "impossible":
		why := fmt.Sprintf("declared impossible %s: %s", how, reason)
		actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: 5 * time.Minute,
			RetryPolicy:         shortRetry,
		})
		var final CycleResult
		if err := workflow.ExecuteActivity(actx, ActivityDeclareImpossible, FinalizeInput{
			MissionID: w.inp.MissionID, Workdir: w.inp.Workdir,
			CycleID: fmt.Sprintf("impossible-%d", st.CyclesDone), Reason: why,
		}).Get(ctx, &final); err != nil {
			return "", "", false, err
		}
		w.absorb(final)
		return OutcomeImpossible, why, false, nil
	}
	return OutcomeAborted, fmt.Sprintf("aborted at the deadlock gate %s: %s", how, reason), false, nil
}

func pyNone(p *string) string {
	if p == nil {
		return "None"
	}
	return *p
}

func (w *missionRun) unblock(ctx workflow.Context) (bool, error) {
	st := w.state
	st.DeadlockRetries++
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy:         shortRetry,
	})
	var unblocked CycleResult
	if err := workflow.ExecuteActivity(actx, ActivityUnblockItems, UnblockInput{
		MissionID: w.inp.MissionID, Workdir: w.inp.Workdir, CycleID: fmt.Sprintf("u%d", st.DeadlockRetries),
	}).Get(ctx, &unblocked); err != nil {
		return false, err
	}
	w.absorb(unblocked)
	st.FailItem, st.FailStreak = nil, 0
	return unblocked.Advanced, nil
}

// recordRow writes a status the workflow owns to the missions row (an activity; best effort):
// never fails or blocks the mission beyond the row deadline — an error is only logged. Only a
// cancellation is returned.
func (w *missionRun) recordRow(ctx workflow.Context, status, reason string) error {
	head := w.state.HeadSHA
	var headPtr *string
	if head != "" {
		headPtr = &head
	}
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    rowTimeout,
		ScheduleToCloseTimeout: rowDeadline,
		RetryPolicy:            rowRetry,
	})
	var ok bool
	err := workflow.ExecuteActivity(actx, ActivityRecordMissionStatus, MissionStatusInput{
		MissionID: w.inp.MissionID, Workdir: w.inp.Workdir, Status: status, HeadSHA: headPtr,
		Reason: headRunes(reason, 500),
	}).Get(ctx, &ok)
	if err != nil {
		if temporal.IsCanceledError(err) {
			return err
		}
		workflow.GetLogger(ctx).Warn("mission row not updated", "status", status, "error", causeString(err))
	}
	return nil
}

func (w *missionRun) terminal(ctx workflow.Context, outcome, reason string) (MissionResult, error) {
	st := w.state
	if st.HeadSHA == "" && st.ItemsTotal == 0 {
		// No cycle reported yet in this mission: read the committed truth once.
		actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: 2 * time.Minute,
			RetryPolicy:         shortRetry,
		})
		var snap CycleResult
		if err := workflow.ExecuteActivity(actx, ActivityReadMissionSnapshot, HealthInput{
			MissionID: w.inp.MissionID, Workdir: w.inp.Workdir,
		}).Get(ctx, &snap); err != nil {
			return MissionResult{}, err
		}
		w.absorb(snap)
	}
	st.Status = terminalStatus[outcome]
	rowReason := outcome
	if reason != "" {
		rowReason = outcome + ": " + reason
	}
	if err := w.recordRow(ctx, st.Status, rowReason); err != nil {
		return MissionResult{}, err
	}
	return MissionResult{
		MissionID:  w.inp.MissionID,
		Completed:  outcome == OutcomeCompleted,
		Cycles:     st.CyclesDone,
		HeadSHA:    st.HeadSHA,
		ItemsDone:  st.ItemsDone,
		ItemsTotal: st.ItemsTotal,
		Outcome:    outcome,
		Reason:     reason,
		Status:     st.Status,
	}, nil
}

// --- the organization (org_round.go) -------------------------------------------------------

// ErrOrgNotPorted is the configuration error a Go build before the durable organization raised
// for a mission that opted in; kept so the histories it recorded replay (VersionOrg).
const ErrOrgNotPorted = "the multi-agent organization (research_per_item / review / max_parallel) is not yet " +
	"available in the Go implementation; run this mission on a Python worker"

// OrgEnabled is whether the mission opted into any part of the organization.
func OrgEnabled(inp MissionInput) bool {
	return inp.ResearchPerItem > 0 || inp.Review || inp.MaxParallel >= 2
}

// OrgConfigError is why the organization options are invalid ("" when they are fine).
func OrgConfigError(inp MissionInput) string {
	if inp.ResearchPerItem < 0 || inp.ResearchPerItem > MaxResearchPerItem {
		return fmt.Sprintf("research_per_item must be 0..%d (got %d)", MaxResearchPerItem, inp.ResearchPerItem)
	}
	if inp.MaxParallel < 0 || inp.MaxParallel > MaxParallel {
		return fmt.Sprintf("max_parallel must be 0..%d (got %d)", MaxParallel, inp.MaxParallel)
	}
	return ""
}

// --- time helpers (workflow-safe: they only format values) -----------------------------------

func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// fromEpoch is python's datetime.fromtimestamp(f, UTC): microseconds rounded half-even.
func fromEpoch(f float64) time.Time {
	sec := math.Floor(f)
	us := math.RoundToEven((f - sec) * 1e6)
	return time.Unix(int64(sec), int64(us)*1000).UTC()
}

func seconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

// isoSeconds is python's moment.astimezone(UTC).isoformat(timespec="seconds").
func isoSeconds(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05") + "+00:00" }

// IsoFull is python's datetime.fromtimestamp(ts, UTC).isoformat() (microseconds when non-zero).
func IsoFull(ts float64) string {
	t := fromEpoch(ts)
	us := t.Nanosecond() / 1000
	base := t.Format("2006-01-02T15:04:05")
	if us != 0 {
		base += fmt.Sprintf(".%06d", us)
	}
	return base + "+00:00"
}
