package agent

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/egressproxy"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs/tracing"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// The integrated inner agent loop: gather -> act (tool loop) -> verify -> checkpoint, for ONE
// cycle of work on ONE checklist item (python: lha.agent.loop).
//
// Harness truth (the agent can never write its own verdict):
//   - The checklist is loaded from the committed anchor at cycle start and owned IN MEMORY by the
//     loop; CommitCheckpoint rewrites .lha/ from that state, discarding agent edits.
//   - An item is marked done ONLY when the deterministic verifier returns "passed" — which
//     requires at least one gating check.
//   - Pre-existing test-harness files are hashed at cycle start; modifying/deleting them adds a
//     failing harness_integrity check (and the files are reverted) unless the item allows it.
//   - The model's "done" only ends the acting phase. Unparseable or truncated replies get a
//     corrective turn — they never count as done.
//   - A failed attempt keeps the item in_progress; after MaxConsecutiveFailures in a row it
//     becomes blocked. No actionable item + not complete => IsDeadlocked.
//   - An item's own witnesses gate it on top of the mission checks; an unresolvable witness is a
//     failing check, never a skipped one.
//   - With a Replanner, a newly blocked item is split into smaller children (bounded by
//     MaxReplans per mission and MaxSplitDepth).
//   - A FAILED attempt's code is never committed to the mission's branch: its work tree is saved
//     as refs/lha/attempts/<mission>/<cycle> and the checkout returns to the last verified state.
//     Only the anchor (checklist, progress, events: the failure record) is committed.

const observationCap = 4000

// Splitter splits a blocked item into ordered child drafts (python: Replanner.split). A result
// with fewer than two drafts means "no split".
type Splitter interface {
	Split(ctx context.Context, missionText string, item contracts.ChecklistItem) ([]contracts.ChecklistItem, error)
}

// EventDrainer is implemented by dispatchers that keep human-gate answers/reminders as events
// (python: drain_events); they are committed with the cycle's checkpoint.
type EventDrainer interface {
	DrainEvents() []contracts.EventRecord
}

// CycleOutcome is the result of one agent cycle. IsComplete means every item is verified done;
// IsDeadlocked means nothing is actionable but the mission is NOT complete — callers must stop
// (or escalate) rather than report completion.
type CycleOutcome struct {
	ItemID       string // "" when no item was worked (python: None)
	Advanced     bool
	Verified     bool
	IsComplete   bool
	HeadSHA      string
	ToolCalls    int
	Turns        int
	Verdict      string // "passed" | "failed" | "unverified" | "" (no item worked)
	IsDeadlocked bool
	ItemBlocked  bool // this cycle's failure pushed the item to blocked
	ItemSplit    bool // ...and the replanner replaced it with smaller child items
	Reason       string
	ItemsDone    int
	ItemsTotal   int
}

// LoopOptions configures an AgentLoop. Start from DefaultLoopOptions (the Python defaults).
type LoopOptions struct {
	// Model: pass a metered provider (governor.CostMeter.Wrap) so every call is budget-checked; a
	// refusal (*governor.BudgetExceeded) is returned by RunCycle before anything is checkpointed.
	Model                  contracts.ModelProvider
	Dispatcher             contracts.ToolDispatcher
	Verifier               contracts.Verifier
	Anchor                 *state.GitMissionAnchor
	Recorder               *obs.TraceRecorder
	MaxTurns               int
	MaxConsecutiveFailures int
	VerifyOnDone           bool
	TrustedChecks          map[string][]string
	HarnessGlobs           []string
	Replanner              Splitter
	MaxReplans             int
	MaxSplitDepth          int
	// Engine, when set, is the claude_code lead engine: the acting phase runs as one claude -p
	// session instead of Model turns (Model still meters it and serves the replanner).
	// Everything before and after acting is the same.
	Engine *ClaudeCodeEngine
}

// DefaultLoopOptions are the Python defaults (max_turns=8, max_consecutive_failures=3,
// verify_on_done=True, max_replans=0, max_split_depth=2).
func DefaultLoopOptions() LoopOptions {
	return LoopOptions{MaxTurns: 8, MaxConsecutiveFailures: 3, VerifyOnDone: true, MaxSplitDepth: 2}
}

// AgentLoop runs one verified cycle of work using a model + tools + sandbox + verifier + anchor.
type AgentLoop struct {
	opts LoopOptions
}

// NewAgentLoop returns a loop (Model, Dispatcher, Verifier and Anchor are required).
func NewAgentLoop(opts LoopOptions) *AgentLoop {
	if opts.TrustedChecks == nil {
		opts.TrustedChecks = map[string][]string{}
	}
	return &AgentLoop{opts: opts}
}

type acting struct {
	toolCalls   int
	toolsUsed   []string
	doneSummary string
	turns       int
	core        *contracts.VerificationResult // verifier verdict, before harness integrity
	dirty       bool                          // workspace changed since the last verification
}

// cycleState is what the acting and verification phases share.
type cycleState struct {
	tctx          contracts.ToolContext
	missionID     string
	cycleID       string
	gate          []contracts.Check
	witnessErrors []contracts.CheckResult
	harness       verify.HarnessSnapshot // nil when harness edits are allowed
	tampered      []string               // sticky for the whole cycle, even after reverting
}

// RunCycle runs one cycle on the next actionable item. anchorText is optional caller context;
// checks are the mission's gating checks.
// The cycle is one "lha.cycle" span (internal/obs/tracing).
func (l *AgentLoop) RunCycle(ctx context.Context, tctx contracts.ToolContext, missionID, cycleID, anchorText string, checks []contracts.Check) (CycleOutcome, error) {
	ctx, span := tracing.SpanCycle(ctx, missionID, cycleID)
	outcome, err := l.runCycle(ctx, tctx, missionID, cycleID, anchorText, checks)
	if err == nil {
		span.Set(map[string]any{
			"lha.item_id": nilIfEmpty(outcome.ItemID), "lha.verdict": nilIfEmpty(outcome.Verdict),
			"lha.verified": outcome.Verified, "lha.tool_calls": outcome.ToolCalls,
			"lha.turns": outcome.Turns, "lha.head_sha": nilIfEmpty(outcome.HeadSHA),
		})
	}
	span.End(err)
	return outcome, err
}

// nilIfEmpty is python's None for an unset optional string (skipped as a span attribute).
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (l *AgentLoop) runCycle(ctx context.Context, tctx contracts.ToolContext, missionID, cycleID, anchorText string, checks []contracts.Check) (CycleOutcome, error) {
	anchor := l.opts.Anchor
	checklist, err := anchor.ReadChecklist(ctx) // committed truth, owned in memory
	if err != nil {
		return CycleOutcome{}, err
	}
	snapshot, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return CycleOutcome{}, err
	}
	mission, err := anchor.ReadMission(ctx)
	if err != nil {
		return CycleOutcome{}, err
	}
	if checklist.IsComplete() || checklist.IsDeadlocked() {
		return idleOutcome(&checklist, snapshot.HeadSHA), nil
	}
	next := checklist.NextActionable()
	if next == nil { // unreachable: not complete and not deadlocked implies an item
		return idleOutcome(&checklist, snapshot.HeadSHA), nil
	}
	started, err := checklist.Start(next.ID)
	if err != nil {
		return CycleOutcome{}, err
	}
	item := *started
	witnessChecks, witnessErrors := l.witnessChecks(item)
	cs := &cycleState{
		tctx: tctx, missionID: missionID, cycleID: cycleID,
		gate:          contracts.EnsureUniqueCheckNames(append(append([]contracts.Check{}, checks...), witnessChecks...)),
		witnessErrors: witnessErrors,
	}
	if !item.AllowHarnessEdits {
		cs.harness = verify.SnapshotHarnessGlobs(anchor.Workdir(), l.opts.HarnessGlobs)
	}
	missionText := snapshot.AnchorText()
	if mission != nil {
		missionText = mission.RenderAnchor()
	}
	messages := BuildMessages(PromptInput{
		AnchorText:  anchorText,
		MissionText: missionText,
		Snapshot:    snapshot,
		Item:        item,
		Specs:       l.opts.Dispatcher.Specs(),
		Engine:      l.opts.Engine != nil,
	})
	l.emit("cycle_started", missionID, cycleID, obs.F("item_id", item.ID))

	act := &acting{dirty: true}
	if l.opts.Engine != nil {
		if err := l.engineSession(ctx, act, messages, cs); err != nil {
			return CycleOutcome{}, err
		}
	} else if err := l.modelTurns(ctx, act, messages, cs); err != nil {
		return CycleOutcome{}, err
	}
	core := act.core
	if core == nil || act.dirty {
		v, err := l.verifyCore(ctx, cs)
		if err != nil {
			return CycleOutcome{}, err
		}
		core = &v
	}
	// Always re-check the harness right before committing (tampering needs no tool call).
	verification, err := l.withIntegrity(ctx, *core, cs)
	if err != nil {
		return CycleOutcome{}, err
	}
	egress := l.egressEvents(ctx, cs.tctx.Session, missionID, cycleID)
	head, err := l.checkpoint(ctx, &checklist, item.ID, cycleID, verification, act.toolCalls, missionText, missionID, egress)
	if err != nil {
		return CycleOutcome{}, err
	}
	l.emit("checkpoint", missionID, cycleID, obs.F("head_sha", head),
		obs.F("verified", verification.AllGreen), obs.F("verdict", verification.Verdict))
	final := checklist.Get(item.ID)
	reason := checklist.DeadlockReason()
	if reason == "" {
		reason = pyfmt.Head(final.LastFailure, 500)
	}
	return CycleOutcome{
		ItemID:       item.ID,
		Advanced:     true,
		Verified:     verification.AllGreen,
		IsComplete:   checklist.IsComplete(),
		HeadSHA:      head,
		ToolCalls:    act.toolCalls,
		Turns:        act.turns,
		Verdict:      verification.Verdict,
		IsDeadlocked: checklist.IsDeadlocked(),
		ItemBlocked:  final.Status == contracts.StatusBlocked,
		ItemSplit:    final.Status == contracts.StatusSplit,
		Reason:       reason,
		ItemsDone:    checklist.ItemsDone(),
		ItemsTotal:   checklist.ItemsTotal(),
	}, nil
}

// modelTurns is the built-in lead: model turns, tool calls and verify-on-done, up to MaxTurns.
func (l *AgentLoop) modelTurns(ctx context.Context, act *acting, messages []contracts.ModelMessage, cs *cycleState) error {
	for turns := 1; turns <= l.opts.MaxTurns; turns++ {
		act.turns = turns
		result, err := l.opts.Model.Complete(ctx, messages, nil, 0)
		if err != nil {
			return err
		}
		l.traceTurn(result, cs)

		if len(result.ToolCalls) > 0 && !IsTruncated(result.StopReason) {
			calls := make([]contracts.ToolCall, len(result.ToolCalls))
			for i, c := range result.ToolCalls {
				if c.ID == "" {
					c.ID = fmt.Sprintf("%s-%d-%d", cs.cycleID, turns, i)
				}
				calls[i] = c
			}
			// Native round trip: the assistant turn carries its tool_use blocks and each result
			// answers its call by id.
			messages = append(messages, contracts.ModelMessage{Role: "assistant", Content: result.Text, ToolCalls: calls})
			for _, call := range calls { // execute EVERY requested tool call, in order
				observation := l.dispatch(ctx, call, cs)
				id := call.ID
				messages = append(messages, contracts.ModelMessage{Role: "tool", Content: observation, ToolCallID: &id})
				act.toolCalls++
				act.toolsUsed = append(act.toolsUsed, call.Name)
			}
			act.dirty = true
			continue
		}

		action := ParseAction(result.Text, nil, result.StopReason)
		messages = append(messages, contracts.ModelMessage{Role: "assistant", Content: result.Text})
		if !action.IsValid() {
			l.emit("invalid_reply", cs.missionID, cs.cycleID, obs.F("reason", action.Error))
			messages = append(messages, CorrectiveMessage(action.Error))
			continue
		}
		if action.Done {
			act.doneSummary = action.Summary
			if !l.opts.VerifyOnDone || turns == l.opts.MaxTurns {
				return nil
			}
			core, err := l.verifyCore(ctx, cs)
			if err != nil {
				return err
			}
			act.core, act.dirty = &core, false
			verification, err := l.withIntegrity(ctx, core, cs)
			if err != nil {
				return err
			}
			if verification.Verdict != contracts.VerdictFailed {
				return nil // green, or unverified (more turns can't create a gate)
			}
			messages = append(messages, contracts.ModelMessage{Role: "user", Content: "VERIFICATION FAILED — the item is not done yet. Fix the cause and " +
				"signal done again.\n" + verification.FailureReport(0)})
			continue
		}
		call := contracts.ToolCall{ID: fmt.Sprintf("%s-%d", cs.cycleID, turns), Name: action.Tool, Arguments: action.Arguments}
		messages = append(messages, contracts.ModelMessage{Role: "user", Content: l.dispatch(ctx, call, cs)})
		act.toolCalls++
		act.toolsUsed = append(act.toolsUsed, call.Name)
		act.dirty = true
	}
	return nil
}

// engineSession is the claude_code lead: one Claude Code session using LHA's tools and verify.
func (l *AgentLoop) engineSession(ctx context.Context, act *acting, messages []contracts.ModelMessage, cs *cycleState) error {
	engine := l.opts.Engine
	var mu sync.Mutex // the bridge serializes calls; this guards act against a torn-down session
	dispatch := func(ctx context.Context, call contracts.ToolCall) contracts.ToolResult {
		result := l.dispatchResult(ctx, call, cs)
		mu.Lock()
		act.dirty = true
		mu.Unlock()
		return result
	}
	verifyFn := func(ctx context.Context) (contracts.VerificationResult, error) {
		core, err := l.verifyCore(ctx, cs)
		if err != nil {
			return contracts.VerificationResult{}, err
		}
		mu.Lock()
		act.core = &core
		act.dirty = engine.Native() // native edits bypass the dispatcher: re-verify after
		mu.Unlock()
		return l.withIntegrity(ctx, core, cs)
	}
	run, err := engine.Run(ctx, EngineRequest{
		Messages: messages,
		Cwd:      l.opts.Anchor.Workdir(),
		CycleID:  cs.cycleID,
		Specs:    l.opts.Dispatcher.Specs(),
		Dispatch: dispatch,
		Verify:   verifyFn,
		Meter:    l.opts.Model,
	})
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	act.toolCalls = run.ToolCalls
	act.toolsUsed = run.ToolsUsed
	act.turns = run.Turns
	act.doneSummary = pyfmt.Head(run.Summary, engineSummaryCap)
	if run.EditsUntracked {
		act.dirty = true
	}
	l.emit("claude_code_session", cs.missionID, cs.cycleID,
		obs.F("turns", run.Turns),
		obs.F("tool_calls", run.ToolCalls),
		obs.F("session_id", run.SessionID),
		obs.F("stopped", run.Stopped))
	return nil
}

func idleOutcome(checklist *contracts.Checklist, headSHA string) CycleOutcome {
	return CycleOutcome{
		IsComplete:   checklist.IsComplete(),
		HeadSHA:      headSHA,
		IsDeadlocked: checklist.IsDeadlocked(),
		Reason:       checklist.DeadlockReason(),
		ItemsDone:    checklist.ItemsDone(),
		ItemsTotal:   checklist.ItemsTotal(),
	}
}

func (l *AgentLoop) traceTurn(result contracts.TurnResult, cs *cycleState) {
	// Spend is recorded by the metered provider (governor), not here.
	stop := ""
	if result.StopReason != nil {
		stop = *result.StopReason
	}
	l.emit("llm_turn", cs.missionID, cs.cycleID, obs.F("model", result.Usage.Model),
		obs.F("output_tokens", result.Usage.OutputTokens), obs.F("stop_reason", stop))
}

func (l *AgentLoop) dispatch(ctx context.Context, call contracts.ToolCall, cs *cycleState) string {
	result := l.dispatchResult(ctx, call, cs)
	observation := result.Content
	if observation == "" {
		observation = result.ErrorText()
	}
	return fmt.Sprintf("OBSERVATION (%s, tool_use_id=%s): %s", call.Name, call.ID, pyfmt.Head(observation, observationCap))
}

// dispatchResult runs one tool call through the dispatcher and traces it.
func (l *AgentLoop) dispatchResult(ctx context.Context, call contracts.ToolCall, cs *cycleState) contracts.ToolResult {
	if call.Arguments == nil {
		call.Arguments = map[string]any{}
	}
	result := tracing.TracedDispatch(ctx, l.opts.Dispatcher, call, cs.tctx)
	l.emit("tool_call", cs.missionID, cs.cycleID, obs.F("tool", call.Name), obs.F("ok", result.OK))
	return result
}

// witnessChecks are the item's witnesses as checks, plus a failing result per unusable witness.
func (l *AgentLoop) witnessChecks(item contracts.ChecklistItem) ([]contracts.Check, []contracts.CheckResult) {
	checks := []contracts.Check{}
	errs := []contracts.CheckResult{}
	for _, w := range item.Witnesses {
		c, err := verify.ParseWitness(w, l.opts.TrustedChecks)
		if err != nil {
			errs = append(errs, contracts.CheckResult{
				Name: w, Passed: false, ExitCode: 2, Gating: true,
				OutputTail: "invalid witness: " + err.Error(),
			})
			continue
		}
		checks = append(checks, c)
	}
	return checks, errs
}

func (l *AgentLoop) verifyCore(ctx context.Context, cs *cycleState) (contracts.VerificationResult, error) {
	v, err := l.opts.Verifier.Verify(ctx, cs.tctx.Session, cs.gate)
	if err != nil {
		return contracts.VerificationResult{}, err
	}
	if len(cs.witnessErrors) > 0 {
		v = v.WithResults(cs.witnessErrors)
	}
	return v, nil
}

// withIntegrity adds a failing harness_integrity result if pre-existing harness files changed.
func (l *AgentLoop) withIntegrity(ctx context.Context, v contracts.VerificationResult, cs *cycleState) (contracts.VerificationResult, error) {
	if cs.harness == nil {
		return v, nil
	}
	after := verify.SnapshotHarnessGlobs(l.opts.Anchor.Workdir(), l.opts.HarnessGlobs)
	violations := verify.HarnessViolations(cs.harness, after)
	if len(violations) > 0 {
		// Revert the tampering so it is never committed (nor the next cycle's baseline).
		if _, err := l.opts.Anchor.RestoreFromHead(ctx, verify.ViolatedPaths(violations)); err != nil {
			return v, err
		}
		for _, vio := range violations {
			if !contains(cs.tampered, vio) {
				cs.tampered = append(cs.tampered, vio)
			}
		}
	}
	if len(cs.tampered) == 0 {
		return v, nil
	}
	return v.WithResults([]contracts.CheckResult{verify.IntegrityResult(cs.tampered)}), nil
}

func (l *AgentLoop) checkpoint(ctx context.Context, checklist *contracts.Checklist, itemID, cycleID string, v contracts.VerificationResult, toolCalls int, missionText, missionID string, egress []contracts.EventRecord) (string, error) {
	splitInto := []string{}
	rolledBack := []string{}
	var verb, note string
	if v.AllGreen {
		proved := []string{}
		for _, r := range v.Results {
			if r.Gating && r.Passed {
				proved = append(proved, r.Name)
			}
		}
		if _, err := checklist.RecordSuccess(itemID, proved); err != nil {
			return "", err
		}
		verb, note = "complete", "verified"
	} else {
		mission := missionID
		if mission == "" {
			mission = "mission"
		}
		ref := "refs/lha/attempts/" + mission + "/" + cycleID
		var err error
		rolledBack, err = l.rollBackAttempt(ctx, ref)
		if err != nil {
			return "", err
		}
		report := v.FailureReport(0)
		if len(rolledBack) > 0 {
			shown := strings.Join(rolledBack[:min(20, len(rolledBack))], ", ")
			if len(rolledBack) > 20 {
				shown += " ..."
			}
			report += "\n\nThis attempt's changes were rolled back to the last verified state " +
				"(kept at " + ref + "): " + shown + ". Start again from the committed code."
		}
		item, err := checklist.RecordFailure(itemID, report, l.opts.MaxConsecutiveFailures)
		if err != nil {
			return "", err
		}
		verb = "attempt"
		if item.Status == contracts.StatusBlocked {
			verb = "block"
		}
		note = fmt.Sprintf("%s (attempt %d, status %s)", v.Verdict, item.Attempts, item.Status)
		if item.Status == contracts.StatusBlocked {
			splitInto, err = l.maybeSplit(ctx, checklist, *item, missionText)
			if err != nil {
				return "", err
			}
			if len(splitInto) > 0 {
				verb = "split"
				note += "; split into " + strings.Join(splitInto, ", ")
			}
		}
	}
	item := checklist.Get(itemID)
	checks := make([]any, 0, len(v.Results))
	for _, r := range v.Results {
		checks = append(checks, map[string]any{
			"name": r.Name, "passed": r.Passed, "gating": r.Gating, "exit_code": r.ExitCode,
			"duration_s": math.Round(r.DurationS*1000) / 1000,
		})
	}
	events := l.gateEvents(cycleID)
	events = append(events, l.verifierEvents(missionID, cycleID)...)
	events = append(events, egress...)
	events = append(events, contracts.EventRecord{
		Kind:    "cycle",
		CycleID: cycleID,
		Payload: map[string]any{
			"item_id":     item.ID,
			"verified":    v.AllGreen,
			"verdict":     v.Verdict,
			"status":      item.Status,
			"tool_calls":  toolCalls,
			"split_into":  splitInto,
			"rolled_back": rolledBack,
			"checks":      checks,
		},
	})
	return l.opts.Anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID:         cycleID,
		ProgressSummary: fmt.Sprintf("- %s [%s] %s: %s", cycleID, item.ID, item.Description, note),
		Checklist:       *checklist,
		Decisions:       []contracts.DecisionRecord{},
		Events:          events,
		CommitMessage:   fmt.Sprintf("lha: %s %s (%s)", verb, item.ID, item.Description),
	})
}

// rollBackAttempt saves the work tree under ref and restores HEAD; it returns the files that
// changed (anchor files excluded: the checkpoint rewrites them anyway).
func (l *AgentLoop) rollBackAttempt(ctx context.Context, ref string) ([]string, error) {
	workdir := l.opts.Anchor.Workdir()
	snapshot, err := verify.CandidateCommit(ctx, workdir, "lha: failed attempt ("+ref+")")
	if err != nil {
		return nil, err
	}
	out, err := state.RunGit(ctx, workdir, "diff", "--name-only", "HEAD", snapshot)
	if err != nil {
		return nil, err
	}
	changed := []string{}
	for _, p := range strings.Split(out, "\n") {
		if p != "" && !strings.HasPrefix(p, ".lha/") {
			changed = append(changed, p)
		}
	}
	if len(changed) == 0 {
		return changed, nil
	}
	if _, err := state.RunGit(ctx, workdir, "update-ref", ref, snapshot); err != nil {
		return nil, err
	}
	if err := state.DiscardChanges(ctx, workdir); err != nil {
		return nil, err
	}
	return changed, nil
}

// maybeSplit splits a newly blocked item via the replanner, within the mission's replan budget.
func (l *AgentLoop) maybeSplit(ctx context.Context, checklist *contracts.Checklist, item contracts.ChecklistItem, missionText string) ([]string, error) {
	if l.opts.Replanner == nil || l.opts.MaxReplans <= 0 {
		return []string{}, nil
	}
	splits := 0
	for _, i := range checklist.Items {
		if i.Status == contracts.StatusSplit {
			splits++
		}
	}
	if splits >= l.opts.MaxReplans || strings.Count(item.ID, ".") >= l.opts.MaxSplitDepth {
		return []string{}, nil
	}
	drafts, err := l.opts.Replanner.Split(ctx, missionText, item)
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

// gateEvents are the human-gate answers/reminders the dispatcher kept this cycle.
func (l *AgentLoop) gateEvents(cycleID string) []contracts.EventRecord {
	drainer, ok := l.opts.Dispatcher.(EventDrainer)
	if !ok {
		return []contracts.EventRecord{}
	}
	events := []contracts.EventRecord{}
	for _, e := range drainer.DrainEvents() {
		if e.CycleID == "" {
			e.CycleID = cycleID
		}
		events = append(events, e)
	}
	return events
}

// verifierEvents are the flaky-check quarantine events the verifier kept this cycle (committed
// with it, and recorded in the trace).
func (l *AgentLoop) verifierEvents(missionID, cycleID string) []contracts.EventRecord {
	drainer, ok := l.opts.Verifier.(EventDrainer)
	if !ok {
		return []contracts.EventRecord{}
	}
	events := []contracts.EventRecord{}
	for _, e := range drainer.DrainEvents() {
		e.CycleID = cycleID
		events = append(events, e)
		l.emit(e.Kind, missionID, cycleID, payloadFields(e.Payload)...)
	}
	return events
}

// EgressEventDrainer is implemented by sandbox sessions that route egress through a proxy whose
// requests become sandbox_egress events (the Docker sandbox with an egress allow-list).
type EgressEventDrainer interface {
	DrainEgressEvents(ctx context.Context) []contracts.EventRecord
}

// egressEvents are the sandbox egress proxy's requests this cycle (committed with it, and
// recorded in the trace) (python: _egress_events). Reading them never fails the cycle.
func (l *AgentLoop) egressEvents(ctx context.Context, session contracts.SandboxSession, missionID, cycleID string) []contracts.EventRecord {
	drainer, ok := session.(EgressEventDrainer)
	if !ok {
		return nil
	}
	events := []contracts.EventRecord{}
	for _, e := range drainer.DrainEgressEvents(ctx) {
		e.CycleID = cycleID
		events = append(events, e)
		l.emit(e.Kind, missionID, cycleID, payloadFields(e.Payload)...)
	}
	return events
}

// payloadFields orders an event payload for the trace: the flaky events' keys in Python's order,
// then any other keys sorted.
func payloadFields(payload map[string]any) []obs.Field {
	fields := []obs.Field{}
	seen := map[string]bool{}
	for _, k := range append(append([]string{}, verify.QuarantineEventFields...), egressproxy.EgressEventFields...) {
		if v, ok := payload[k]; ok {
			fields = append(fields, obs.F(k, v))
			seen[k] = true
		}
	}
	rest := []string{}
	for k := range payload {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		fields = append(fields, obs.F(k, payload[k]))
	}
	return fields
}

func (l *AgentLoop) emit(kind, missionID, cycleID string, data ...obs.Field) {
	if l.opts.Recorder != nil {
		l.opts.Recorder.Record(kind, missionID, cycleID, data...)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
