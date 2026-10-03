package durable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents/org"
	"github.com/calvinchengx/long-horizon-agent/go/internal/checklistedit"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/tools"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/hitl"
	"github.com/calvinchengx/long-horizon-agent/go/internal/memory"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/ops"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// Temporal activities — where all the non-deterministic work happens
// (python/src/lha/durable/activities.py).
//
// The cycle activity runs the REAL agent loop (model -> tools-in-sandbox -> deterministic verify
// -> git checkpoint) in the configured sandbox, every model call metered against the mission
// budget. Temporal journals each activity's result; work inside an attempt that crashes is not
// journaled and the attempt is retried from scratch, so each attempt is safe to repeat:
//
//   - Exclusive: the per-workdir flock (.git/lha-cycle.lock, shared with Python workers).
//   - Clean start: every attempt resets the checkout to HEAD first.
//   - Exactly-once commit per cycle id: if HEAD already carries this cycle's checkpoint (the
//     previous attempt committed, then crashed before reporting), that result is returned.
//   - Heartbeats every 5 s while the cycle runs, so a dead worker is detected by the heartbeat
//     timeout instead of the (long) start-to-close timeout.
//
// Budget: every attempt's spend is appended to .git/lha/spend.ndjson and seeds the next attempt's
// ledger. BudgetExceeded and configuration errors are NON-retryable ApplicationErrors (types
// ErrorBudgetExceeded / ErrorConfig).

// HeartbeatEvery is the default heartbeat period of a running cycle.
const HeartbeatEvery = 5 * time.Second

// ModelFactory builds the lead model for one cycle (tests inject scripted models here).
type ModelFactory func(settings *config.Settings, snapshot contracts.SituationSnapshot) (contracts.ModelProvider, error)

// SubAgentRunner runs one sub-agent (the body of run_subagent).
type SubAgentRunner func(ctx context.Context, inp SubAgentInput) (SubAgentOutput, error)

// Activities are the durable activities bound to one worker's configuration.
type Activities struct {
	// Settings are the worker's settings (nil = config.Load() on every call).
	Settings *config.Settings
	// ModelFactory builds the lead model (nil = model.BuildProvider from settings).
	ModelFactory ModelFactory
	// OpenToolbox opens the lead's sandbox session + dispatcher (required for cycles and the
	// sandbox probe; cmd/lha passes its wiring). The request carries the cycle's
	// DeferredApprovalGate in ToolboxRequest.Gate.
	OpenToolbox agent.ToolboxOpener
	// OpenStore opens the mission store (nil = DefaultStoreOpener).
	OpenStore StoreOpener
	// WebhookTransport is the gate webhook's HTTP transport (nil = the default).
	WebhookTransport http.RoundTripper
	// ProbeModel contacts the model for the health probe (nil = model.ProbeModel).
	ProbeModel func(ctx context.Context, settings *config.Settings) model.ModelHealth
	// SubAgent replaces run_subagent's body (nil = the real sub-agent: org.SubAgent in the Lead's
	// sandbox, metered against the mission budget; tests inject fake researchers here).
	SubAgent SubAgentRunner
	// ImplementerModel / ReviewerModel build the organization roles' models (nil = the role's
	// model, agents.ModelForRole); SubAgentModel builds a sub-agent's (nil = the configured
	// model). The Lead's model (a cycle, and the replanner of a blocked branch) is ModelFactory.
	ImplementerModel ModelFactory
	ReviewerModel    ModelFactory
	SubAgentModel    ModelFactory
	// HeartbeatEvery is the heartbeat period (0 = HeartbeatEvery).
	HeartbeatEvery time.Duration
}

// resetKeep is state.ResetKeep plus LHA_RESET_KEEP.
func resetKeep(settings *config.Settings) ([]string, error) {
	extra, err := settings.ResetKeepPaths()
	if err != nil {
		return nil, err
	}
	return append(append([]string{}, state.ResetKeep...), extra...), nil
}

// resetWorkdir resets workdir to HEAD at the start of an attempt, keeping the built-in paths
// plus LHA_RESET_KEEP (python: _reset_workdir). An invalid LHA_RESET_KEEP is a config error.
func (a *Activities) resetWorkdir(ctx context.Context, workdir string) error {
	settings, err := a.settings()
	if err != nil {
		return err
	}
	keep, err := resetKeep(settings)
	if err != nil {
		return configError(fmt.Sprintf("invalid configuration: %v", err), err)
	}
	return state.ResetToHeadKeep(ctx, workdir, keep)
}

// SecretsRotatedEvent is the anchor event kind recording a rotation (fields and fingerprints).
const SecretsRotatedEvent = "secrets_rotated"

// settings is the worker's settings with the secrets the environment / .env holds NOW
// (config.RefreshSecrets; python: current_settings), so a key rotated mid-mission is used by the
// next activity without a worker restart. The rotated field names are returned for the record.
func (a *Activities) settings() (*config.Settings, error) {
	s, _, err := a.currentSettings()
	return s, err
}

func (a *Activities) currentSettings() (*config.Settings, []string, error) {
	base := a.Settings
	if base == nil {
		loaded, err := config.Load()
		if err != nil {
			return nil, nil, err
		}
		base = loaded
	}
	fresh, rotated, err := config.RefreshSecrets(base)
	if err != nil { // a broken .env must not fail the mission: keep what we have
		activityLogger().Warn("secrets_refresh_failed", "error", err.Error())
		return base, nil, nil
	}
	if len(rotated) > 0 {
		fps := []string{}
		for _, kv := range config.SecretFingerprints(fresh) {
			fps = append(fps, kv.Key+"="+fmt.Sprint(kv.Value))
		}
		activityLogger().Info("secrets_rotated", "fields", strings.Join(rotated, ","), "fingerprints", strings.Join(fps, ","))
	}
	return fresh, rotated, nil
}

// secretsRotatedEvent is python's secrets_rotated_event: which secrets changed and their new
// fingerprints (never a value), committed with the cycle that first used them.
func secretsRotatedEvent(cycleID string, settings *config.Settings, rotated []string) contracts.EventRecord {
	current := map[string]string{}
	for _, kv := range config.SecretFingerprints(settings) {
		current[kv.Key] = fmt.Sprint(kv.Value)
	}
	fps := contracts.NewOrderedMap()
	for _, name := range rotated {
		fps.Set(name, current[name])
	}
	return contracts.EventRecord{Kind: SecretsRotatedEvent, CycleID: cycleID, Payload: contracts.Payload("fields", rotated, "fingerprints", fps)}
}

func (a *Activities) openStore(ctx context.Context, settings *config.Settings, workdir string) (Store, error) {
	opener := a.OpenStore
	if opener == nil {
		opener = DefaultStoreOpener
	}
	return opener(ctx, settings, workdir)
}

func configError(message string, cause error) error {
	return temporal.NewNonRetryableApplicationError(message, ErrorConfig, cause)
}

// pyTypeName names an error like the Python exception class it mirrors (pyfmt.ExcTypeName).
func pyTypeName(err error) string { return pyfmt.ExcTypeName(err) }

func heartbeat(ctx context.Context, details ...any) {
	if activity.IsActivity(ctx) {
		activity.RecordHeartbeat(ctx, details...)
	}
}

func (a *Activities) heartbeatEvery() time.Duration {
	if a.HeartbeatEvery > 0 {
		return a.HeartbeatEvery
	}
	return HeartbeatEvery
}

// withHeartbeat runs fn while heartbeating detail every period.
func withHeartbeat[T any](ctx context.Context, period time.Duration, detail string, fn func() (T, error)) (T, error) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(period)
		defer ticker.Stop()
		heartbeat(ctx, detail)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				heartbeat(ctx, detail)
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()
	return fn()
}

// lockWorkdir takes the checkout's cycle lock, heartbeating while it waits.
func lockWorkdir(ctx context.Context, workdir string, wait time.Duration) (func(), error) {
	return WorkdirLock(ctx, workdir, CycleLock, wait, func() { heartbeat(ctx, "waiting for workdir lock") })
}

// ResolveChecks: nil -> the default Python gate; an explicit empty list is a configuration error.
func ResolveChecks(commands [][]string) ([]contracts.Check, error) {
	if commands == nil {
		return verify.DefaultPythonChecks(), nil
	}
	var nonEmpty [][]string
	for _, c := range commands {
		if len(c) > 0 {
			nonEmpty = append(nonEmpty, c)
		}
	}
	if len(nonEmpty) == 0 {
		return nil, configError("check_commands is empty: at least one gating check is required (an item is only "+
			"marked done by a passing check); omit it to use the default Python checks", nil)
	}
	return contracts.ChecksFromCommands(nonEmpty, true), nil
}

type resultOpts struct {
	itemID           *string
	advanced         bool
	note             string
	verdict          string
	itemBlocked      bool
	reason           string
	spentUSD         float64
	itemSplit        bool
	pendingApprovals []PendingApproval
	usedApprovals    []string
	baseSHA          string
}

func resultFromSnapshot(s contracts.SituationSnapshot, o resultOpts) CycleResult {
	reason := o.reason
	if reason == "" {
		reason = s.DeadlockReason
	}
	return CycleResult{
		ItemID:           o.itemID,
		Advanced:         o.advanced,
		HeadSHA:          s.HeadSHA,
		IsComplete:       s.IsComplete,
		ItemsDone:        s.ItemsDone,
		ItemsTotal:       s.ItemsTotal,
		Note:             o.note,
		Verdict:          o.verdict,
		IsDeadlocked:     s.IsDeadlocked,
		ItemBlocked:      o.itemBlocked,
		Reason:           reason,
		SpentUSD:         o.spentUSD,
		ItemSplit:        o.itemSplit,
		PendingApprovals: append([]PendingApproval{}, o.pendingApprovals...),
		UsedApprovals:    append([]string{}, o.usedApprovals...),
		BaseSHA:          o.baseSHA,
	}
}

// AnchorText is the cycle's extra prompt context: operator steering, approved actions, the
// research briefs for the active item, and from the committed events the item's reflection and
// the newest board posts (the loop recites the mission spec itself).
func AnchorText(snapshot contracts.SituationSnapshot, inp CycleInput, events []contracts.EventRecord) string {
	var parts []string
	if snapshot.Mission == nil {
		parts = append(parts, "Mission "+inp.MissionID)
	}
	if snapshot.ActiveItem != nil {
		if reflection := org.ReflectionFor(events, snapshot.ActiveItem.ID); reflection != "" {
			parts = append(parts, pyfmt.PyStrip(reflection))
		}
	}
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
	var active *string
	if snapshot.ActiveItem != nil {
		active = &snapshot.ActiveItem.ID
	}
	if len(inp.ResearchBriefs) > 0 && inp.ResearchItem != nil && active != nil && *inp.ResearchItem == *active {
		parts = append(parts, "Research briefs:\n"+strings.Join(inp.ResearchBriefs, "\n---\n"))
	}
	if board := org.BoardContextFromEvents(events); board != "" {
		parts = append(parts, board)
	}
	return strings.Join(parts, "\n\n")
}

// researchEvent is the research event of the fan-out done before this cycle (failures included).
func researchEvent(inp CycleInput) *contracts.EventRecord {
	if inp.ResearchItem == nil {
		return nil
	}
	failures := make([]any, len(inp.ResearchFailures))
	for i, f := range inp.ResearchFailures {
		failures[i] = headRunes(f, 500)
	}
	return &contracts.EventRecord{
		Kind:    "research",
		CycleID: inp.CycleID,
		Payload: contracts.Payload(
			"item", *inp.ResearchItem,
			"n", len(inp.ResearchBriefs),
			"failed", len(inp.ResearchFailures),
			"failures", failures,
		),
	}
}

func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// CycleStatus is missions.status after a cycle, from the committed truth: complete -> DONE; an
// approval queued -> WAITING_ON_HUMAN; otherwise RUNNING, including a deadlocked checklist (the
// workflow decides whether a deadlock ends the mission).
func CycleStatus(after contracts.SituationSnapshot, awaitingApproval bool) string {
	switch {
	case after.IsComplete:
		return StatusDone
	case awaitingApproval:
		return StatusWaitingOnHuman
	}
	return StatusRunning
}

func activityLogger() *slog.Logger { return obs.Logger("lha.durable") }

func attemptOf(ctx context.Context) int {
	if activity.IsActivity(ctx) {
		return int(activity.GetInfo(ctx).Attempt)
	}
	return 1
}

func workflowIDOf(ctx context.Context) string {
	if activity.IsActivity(ctx) {
		return activity.GetInfo(ctx).WorkflowExecution.ID
	}
	return ""
}

// isToolboxConfigError: the toolbox could not be assembled from the settings (python:
// UnsafeSandboxError / ValueError from open_lead_sandbox and the dispatcher): never transient.
func isToolboxConfigError(err error) bool {
	var unsafe *execution.UnsafeSandboxError
	var e2b execution.E2BUnsupportedError
	var e2bp *execution.E2BUnsupportedError
	var rot *tools.RuleOfTwoViolation
	var web *tools.WebConfigError
	return errors.As(err, &unsafe) || errors.As(err, &e2b) || errors.As(err, &e2bp) || errors.As(err, &rot) ||
		errors.As(err, &web) || errors.Is(err, agent.ErrExecutionNotLinked)
}

// RunAgentCycle is run_agent_cycle: advance the mission by one verified item (safe to retry).
func (a *Activities) RunAgentCycle(ctx context.Context, inp CycleInput) (CycleResult, error) {
	result, err := a.executeCycle(ctx, inp)
	var chain *state.DecisionChainError
	if errors.As(err, &chain) { // an altered decision log is not transient: fail the mission
		return CycleResult{}, configError("decision log failed verification: "+chain.Error(), err)
	}
	return result, err
}

func (a *Activities) executeCycle(ctx context.Context, inp CycleInput) (CycleResult, error) {
	settings, rotated, err := a.currentSettings()
	if err != nil {
		return CycleResult{}, configError(fmt.Sprintf("invalid configuration: %v", err), err)
	}
	checks, err := ResolveChecks(inp.CheckCommands)
	if err != nil {
		return CycleResult{}, err
	}
	attempt := attemptOf(ctx)

	release, err := lockWorkdir(ctx, inp.Workdir, LockWait)
	if err != nil {
		return CycleResult{}, err
	}
	defer release()
	if err := a.resetWorkdir(ctx, inp.Workdir); err != nil {
		return CycleResult{}, err
	}
	anchor := state.NewGitMissionAnchor(inp.Workdir)
	snapshot, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	if payload, ok := CommittedCycleEvent(ctx, inp.Workdir, inp.CycleID, "cycle"); ok {
		// A previous attempt committed this cycle, then crashed before reporting.
		var item *string
		if v, present := payload["item_id"]; present && v != nil {
			item = strPtr(fmt.Sprint(v))
		}
		verdict := ""
		if v, present := payload["verdict"]; present && v != nil {
			verdict = fmt.Sprint(v)
		}
		return resultFromSnapshot(snapshot, resultOpts{
			itemID: item, advanced: true, verdict: verdict,
			itemBlocked: payload["status"] == "blocked",
			note:        "already committed by a previous attempt",
		}), nil
	}
	if snapshot.IsComplete || snapshot.IsDeadlocked {
		return resultFromSnapshot(snapshot, resultOpts{note: "nothing actionable"}), nil
	}

	meter, err := BuildCycleMeter(ctx, settings, inp.Workdir, inp.CycleID, inp.BudgetUSD, inp.MaxCycles)
	if err != nil {
		return CycleResult{}, err
	}
	factory := a.ModelFactory
	if factory == nil {
		factory = func(s *config.Settings, _ contracts.SituationSnapshot) (contracts.ModelProvider, error) {
			return model.BuildProvider(s, "", nil)
		}
	}
	inner, err := factory(settings, snapshot)
	if err != nil { // e.g. a missing API key / base URL for the configured backend
		return CycleResult{}, configError(fmt.Sprintf("cannot build the model: %v", err), err)
	}
	lead := meter.Wrap(inner, "lead")
	closeModel := func() { _ = agent.CloseProvider(context.WithoutCancel(ctx), inner) }

	gate := NewDeferredApprovalGate(approvedFingerprints(inp.ApprovedActions))
	if a.OpenToolbox == nil {
		closeModel()
		return CycleResult{}, configError("cannot open the sandbox: "+agent.ErrExecutionNotLinked.Error(), nil)
	}
	toolbox, err := a.OpenToolbox(ctx, agent.ToolboxRequest{
		Settings: settings, Workdir: inp.Workdir, Anchor: anchor, Gate: gate,
	})
	if err != nil {
		closeModel()
		if isToolboxConfigError(err) {
			return CycleResult{}, configError(fmt.Sprintf("cannot open the sandbox: %v", err), err)
		}
		return CycleResult{}, err
	}

	// Persistence (activity-side only; the workflow never touches a database).
	store, err := a.openStore(ctx, settings, inp.Workdir)
	if err != nil {
		_ = toolbox.Close(context.WithoutCancel(ctx))
		closeModel()
		if errors.Is(err, ErrStoreUnavailable) {
			return CycleResult{}, configError(fmt.Sprintf("cannot open the mission store: %v", err), err)
		}
		return CycleResult{}, err
	}
	defer store.Close(context.WithoutCancel(ctx))
	systemOne, err := systemone.Build(settings, meter)
	if err != nil { // e.g. a remote System One endpoint without a key
		_ = toolbox.Close(context.WithoutCancel(ctx))
		closeModel()
		return CycleResult{}, configError(fmt.Sprintf("cannot build the System One model: %v", err), err)
	}
	defer func() { _ = systemone.Close(systemOne) }()
	// Every metered call lands in cost_ledger as it happens (python: LedgerSink attached with
	// key_prefix=f"{cycle_id}@{attempt}", backfill=False: the seeded prior spend is already there).
	meter.SetHook(newLedgerHook(store, inp.MissionID, fmt.Sprintf("%s@%d", inp.CycleID, attempt)))
	defer meter.SetHook(nil)
	// The memory plane over the mission store (python: open_run_services -> open_mission_memory),
	// its librarian model metered like the lead. A store without a persistence.Store (a test fake)
	// runs without memory.
	// The cycle's trace events go to the shared event record (mission_events) through recorder
	// (python: open_run_services(recorder=...)); closed before the store, which is deferred above.
	recorder := obs.NewTraceRecorder(nil)
	var cycleMemory memory.CycleMemory
	if backed, ok := store.(persistenceBacked); ok {
		events := persistence.NewMissionEventLog(backed.Persistence())
		recorder.AddListener(events.Add)
		events.Start()
		defer events.Close(context.WithoutCancel(ctx))
		if mem := memory.OpenMissionMemory(ctx, settings, backed.Persistence(), inp.Workdir, inp.MissionID,
			memory.OpenOptions{Model: meter.Wrap(inner, "librarian"), Recorder: recorder, SystemOne: systemOne}); mem != nil {
			defer mem.Close()
			cycleMemory = mem
		}
	}
	title, description := "", ""
	if snapshot.Mission != nil {
		title, description = snapshot.Mission.Title, snapshot.Mission.Description
	}
	tracker := func(status, head string) {
		if err := store.UpsertMission(context.WithoutCancel(ctx), MissionRow{
			MissionID: inp.MissionID, Title: title, Description: description, Status: status,
			HeadSHA: head, WorkflowID: workflowIDOf(ctx),
		}); err != nil {
			activityLogger().Warn("mission_upsert_failed", "mission_id", inp.MissionID, "status", status,
				"error", fmt.Sprintf("%s: %v", pyTypeName(err), err))
		}
	}
	tracker(StatusRunning, snapshot.HeadSHA)
	if ev := researchEvent(inp); ev != nil {
		if err := anchor.AppendEvent(ctx, *ev); err != nil { // committed with this checkpoint
			_ = toolbox.Close(context.WithoutCancel(ctx))
			closeModel()
			return CycleResult{}, err
		}
	}
	if len(rotated) > 0 {
		if err := anchor.AppendEvent(ctx, secretsRotatedEvent(inp.CycleID, settings, rotated)); err != nil {
			_ = toolbox.Close(context.WithoutCancel(ctx))
			closeModel()
			return CycleResult{}, err
		}
	}

	outcome, cycleErr := func() (agent.CycleOutcome, error) {
		defer func() {
			_ = toolbox.Close(context.WithoutCancel(ctx))
			closeModel()
			key := IdempotencyKey(inp.MissionID, inp.CycleID, strconv.Itoa(attempt))
			if err := RecordSpend(context.WithoutCancel(ctx), inp.Workdir, key, inp.CycleID, meter.Ledger); err != nil {
				activityLogger().Warn("spend_journal_write_failed", "error", err.Error())
			}
		}()
		active := ""
		if snapshot.ActiveItem != nil {
			active = snapshot.ActiveItem.ID
		}
		dispatcher, err := LeadGuard(ctx, anchor, toolbox.Dispatcher(), active)
		if err != nil {
			return agent.CycleOutcome{}, err
		}
		loop, err := agent.BuildLeadLoop(settings, lead, anchor, dispatcher, recorder)
		if err != nil { // e.g. malformed LHA_TRUSTED_CHECKS
			return agent.CycleOutcome{}, configError(fmt.Sprintf("invalid configuration: %v", err), err)
		}
		loop.SetMemory(cycleMemory)
		loop.SetTriage(systemone.BuildStallTriage(settings, systemOne))
		tctx := contracts.ToolContext{MissionID: inp.MissionID, Session: toolbox.Session()}
		return withHeartbeat(ctx, a.heartbeatEvery(), inp.CycleID, func() (agent.CycleOutcome, error) {
			events, err := anchor.ReadEvents(ctx)
			if err != nil {
				return agent.CycleOutcome{}, err
			}
			return loop.RunCycle(ctx, tctx, inp.MissionID, inp.CycleID, AnchorText(snapshot, inp, events), checks)
		})
	}()
	if cycleErr != nil {
		var budget *governor.BudgetExceeded
		if errors.As(cycleErr, &budget) {
			tracker(StatusAborted, "")
			return CycleResult{}, temporal.NewNonRetryableApplicationError(budget.Error(), ErrorBudgetExceeded, nil)
		}
		return CycleResult{}, cycleErr
	}

	after, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	tracker(CycleStatus(after, len(gate.Pending) > 0), after.HeadSHA)
	var spent float64
	for _, e := range meter.Ledger.Entries() {
		if e.CycleID == inp.CycleID {
			spent += e.USD
		}
	}
	var item *string
	if outcome.ItemID != "" {
		item = strPtr(outcome.ItemID)
	}
	return resultFromSnapshot(after, resultOpts{
		itemID: item, advanced: outcome.Advanced, verdict: outcome.Verdict,
		itemBlocked: outcome.ItemBlocked, reason: outcome.Reason,
		note:     fmt.Sprintf("verdict=%s tools=%d turns=%d", outcome.Verdict, outcome.ToolCalls, outcome.Turns),
		spentUSD: spent, itemSplit: outcome.ItemSplit,
		pendingApprovals: gate.Pending, usedApprovals: gate.Used, baseSHA: snapshot.HeadSHA,
	}), nil
}

// LeadGuard is python's lead_guard: the Lead's dispatcher behind an OwnershipGuard when the
// mission has an ownership map (a durable mission with parallel waves), else inner unchanged. As
// in orchestrate's serial rounds, the Lead may write unassigned space, shared files and the active
// item's own files, never files leased to another open item.
func LeadGuard(ctx context.Context, anchor *state.GitMissionAnchor, inner contracts.ToolDispatcher, itemID string) (contracts.ToolDispatcher, error) {
	checklist, err := anchor.ReadChecklist(ctx)
	if err != nil {
		return nil, err
	}
	persisted, err := coordination.ReadOwnership(ctx, anchor)
	if err != nil {
		return nil, err
	}
	ownership := coordination.EffectiveOwnership(persisted, coordination.FinishedWriters(checklist))
	if len(ownership.Snapshot()) == 0 {
		return inner, nil
	}
	writers := []string{coordination.Lead}
	if itemID != "" {
		writers = append(writers, coordination.WriterForItem(itemID))
	}
	return coordination.NewOwnershipGuard(inner, ownership, writers, false), nil
}

func approvedFingerprints(actions []ApprovedAction) []string {
	out := make([]string, len(actions))
	for i, a := range actions {
		out[i] = a.Fingerprint
	}
	return out
}

// --- health probe (used while parked) -------------------------------------------------------

// CheckMissionHealth is check_mission_health: probe the critical dependencies (git checkout, a
// real model round trip, the sandbox).
func (a *Activities) CheckMissionHealth(ctx context.Context, inp HealthInput) (HealthReport, error) {
	settings, err := a.settings()
	if err != nil {
		return HealthReport{}, err
	}
	var statuses []ops.DependencyStatus
	okRepo, _ := state.IsRepo(ctx, inp.Workdir)
	if okRepo {
		okRepo, _ = state.HasCommits(ctx, inp.Workdir)
	}
	if okRepo {
		statuses = append(statuses, ops.DependencyStatus{Name: "git", Health: ops.HealthOK})
	} else {
		statuses = append(statuses, ops.DependencyStatus{Name: "git", Health: ops.HealthDown, Detail: "no usable repo"})
	}
	probe := a.ProbeModel
	if probe == nil {
		probe = func(ctx context.Context, s *config.Settings) model.ModelHealth {
			return model.ProbeModel(ctx, s, 0, nil)
		}
	}
	if m := probe(ctx, settings); m.OK {
		statuses = append(statuses, ops.DependencyStatus{Name: "model", Health: ops.HealthOK})
	} else {
		statuses = append(statuses, ops.DependencyStatus{Name: "model", Health: ops.HealthDown, Detail: m.Detail})
	}
	sandboxErr := errors.New("execution layer not linked")
	if a.OpenToolbox != nil {
		var toolbox agent.Toolbox
		toolbox, sandboxErr = a.OpenToolbox(ctx, agent.ToolboxRequest{Settings: settings, Workdir: inp.Workdir})
		if sandboxErr == nil {
			sandboxErr = toolbox.Close(ctx)
		}
	}
	if sandboxErr == nil {
		statuses = append(statuses, ops.DependencyStatus{Name: "sandbox", Health: ops.HealthOK})
	} else {
		statuses = append(statuses, ops.DependencyStatus{Name: "sandbox", Health: ops.HealthDown,
			Detail: fmt.Sprintf("%s: %v", pyTypeName(sandboxErr), sandboxErr)})
	}
	decision := ops.DecideSafePark(statuses)
	var details []string
	for _, s := range statuses {
		if s.Detail != "" {
			details = append(details, s.Name+": "+s.Detail)
		}
	}
	reason := decision.Reason
	if len(details) > 0 {
		reason += " (" + strings.Join(details, "; ") + ")"
	}
	return HealthReport{Healthy: !decision.Park, Reason: reason, Degraded: decision.Degraded}, nil
}

// --- human gates: anchor events + webhook, final "impossible" checkpoint --------------------

// GateNoticePayload is the JSON a gate event is recorded / POSTed as (question and arguments
// redacted), in Python's key order (python: gate_notice_payload).
func GateNoticePayload(n GateNotice) hitl.Payload {
	p := hitl.Payload{
		{Key: "source", Value: "lha"},
		{Key: "mission_id", Value: n.MissionID},
		{Key: "gate_id", Value: n.GateID},
		{Key: "kind", Value: n.Kind},
		{Key: "event", Value: n.Event},
		{Key: "question", Value: obs.RedactText(n.Question)},
		{Key: "options", Value: nz(n.Options)},
		{Key: "default_action", Value: n.DefaultAction},
		{Key: "deadline", Value: n.Deadline},
	}
	if n.Decision != "" {
		p = append(p, hitl.Field{Key: "decision", Value: n.Decision})
	}
	if n.Step != 0 {
		p = append(p, hitl.Field{Key: "step", Value: n.Step})
	}
	if n.Request != nil {
		p = append(p, hitl.Field{Key: "request", Value: hitl.Payload{
			{Key: "fingerprint", Value: n.Request.Fingerprint},
			{Key: "tool", Value: n.Request.Tool},
			{Key: "arguments", Value: obs.RedactText(n.Request.Arguments)},
			{Key: "reason", Value: n.Request.Reason},
		}})
	}
	return p
}

// GatePayloadMap is the gate payload as an event payload, in the same key order.
func GatePayloadMap(p hitl.Payload) *contracts.OrderedMap {
	m := contracts.Payload()
	for _, f := range p {
		switch v := f.Value.(type) {
		case hitl.Payload:
			m.Set(f.Key, GatePayloadMap(v))
		case []string:
			m.Set(f.Key, append([]string{}, v...))
		default:
			m.Set(f.Key, v)
		}
	}
	return m
}

// recordGateEvent commits the gate event to the anchor's event log (no cycle runs while a gate
// is open; the reset only discards the partial work of a cycle that ended waiting for approval).
func recordGateEvent(ctx context.Context, n GateNotice, payload hitl.Payload, keep []string) bool {
	release, err := lockWorkdir(ctx, n.Workdir, 60*time.Second)
	if err != nil {
		return false
	}
	defer release()
	if err := state.ResetToHeadKeep(ctx, n.Workdir, keep); err != nil {
		return false
	}
	anchor := state.NewGitMissionAnchor(n.Workdir)
	checklist, err := anchor.ReadChecklist(ctx)
	if err != nil {
		return false
	}
	cycleID := "gate:" + n.GateID
	_, err = anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID:       cycleID,
		Checklist:     checklist,
		Events:        []contracts.EventRecord{{Kind: "gate_" + n.Event, CycleID: cycleID, Payload: GatePayloadMap(payload)}},
		CommitMessage: fmt.Sprintf("lha: gate %s (%s %s)", n.Event, n.Kind, n.GateID),
	})
	return err == nil
}

// hitl_gates.resolved_by on the durable path: the human_decision signal carries no identity.
const (
	ResolvedBySignal  = "human (human_decision signal)"
	ResolvedByTimeout = "default (timeout)"
)

// GateEventFromNotice is the hitl_gates event for a durable gate notice (python:
// gate_event_from_notice).
func GateEventFromNotice(n GateNotice, payload hitl.Payload) GateEvent {
	at := n.At
	if at == "" {
		at = isoSeconds(time.Now())
	}
	resolvedBy := ResolvedByFor(n)
	risk := n.Kind
	if n.Request != nil {
		risk = "irreversible"
	}
	var request map[string]string
	if r, ok := payload.Get("request").(hitl.Payload); ok {
		request = map[string]string{}
		for _, f := range r {
			request[f.Key] = fmt.Sprint(f.Value)
		}
	}
	question, _ := payload.Get("question").(string)
	return GateEvent{
		MissionID: n.MissionID, GateID: n.GateID, Kind: n.Kind, Event: n.Event, At: at,
		Question: question, Options: append([]string{}, n.Options...), DefaultAction: n.DefaultAction,
		Deadline: n.Deadline, Decision: n.Decision, ResolvedBy: resolvedBy, Step: n.Step, Risk: risk,
		Request: request,
	}
}

// NotifyGate is notify_gate: record a gate event in the anchor and the hitl_gates table and POST
// it to the optional webhook. A notification problem never fails the gate: outcomes are reported.
func (a *Activities) NotifyGate(ctx context.Context, n GateNotice) (NoticeResult, error) {
	settings, err := a.settings()
	if err != nil {
		return NoticeResult{}, err
	}
	payload := GateNoticePayload(n)
	keep, err := resetKeep(settings)
	if err != nil {
		return NoticeResult{}, configError(fmt.Sprintf("invalid configuration: %v", err), err)
	}
	recorded := recordGateEvent(ctx, n, payload, keep)
	stored := false
	if store, err := a.openStore(ctx, settings, n.Workdir); err == nil {
		if err := store.RecordGateEvent(ctx, GateEventFromNotice(n, payload)); err == nil {
			stored = !IsNopStore(store)
		} else {
			activityLogger().Warn("gate_row_write_failed", "mission_id", n.MissionID, "gate_id", n.GateID,
				"gate_event", n.Event, "error", fmt.Sprintf("%s: %v", pyTypeName(err), err))
		}
		_ = store.Close(ctx)
	} else {
		activityLogger().Warn("gate_row_write_failed", "mission_id", n.MissionID, "gate_id", n.GateID,
			"gate_event", n.Event, "error", fmt.Sprintf("%s: %v", pyTypeName(err), err))
	}
	url := ""
	if settings.GateWebhookURL != nil {
		url = settings.GateWebhookURL.Value()
	}
	timeout := time.Duration(settings.GateWebhookTimeoutSeconds * float64(time.Second))
	webhook := hitl.PostWebhook(url, payload, timeout, a.WebhookTransport)
	return NoticeResult{Recorded: recorded, Webhook: webhook, Stored: stored}, nil
}

func pyList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = contracts.PyRepr(n)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func blockedIDs(checklist contracts.Checklist) []string {
	ids := []string{}
	for _, it := range checklist.BlockedItems() {
		ids = append(ids, it.ID)
	}
	return ids
}

// DeclareImpossible is declare_impossible: the final checkpoint of a mission declared impossible
// (idempotent per cycle id).
func (a *Activities) DeclareImpossible(ctx context.Context, inp FinalizeInput) (CycleResult, error) {
	release, err := lockWorkdir(ctx, inp.Workdir, LockWait)
	if err != nil {
		return CycleResult{}, err
	}
	defer release()
	if err := a.resetWorkdir(ctx, inp.Workdir); err != nil {
		return CycleResult{}, err
	}
	anchor := state.NewGitMissionAnchor(inp.Workdir)
	checklist, err := anchor.ReadChecklist(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	blocked := blockedIDs(checklist)
	if _, done := CommittedCycleEvent(ctx, inp.Workdir, inp.CycleID, "cycle"); !done {
		_, err := anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
			CycleID:         inp.CycleID,
			ProgressSummary: fmt.Sprintf("- %s mission declared IMPOSSIBLE: %s", inp.CycleID, inp.Reason),
			Checklist:       checklist,
			Events: []contracts.EventRecord{
				{Kind: "mission_impossible", CycleID: inp.CycleID, Payload: contracts.Payload(
					"reason", inp.Reason, "blocked", blocked, "items_done", checklist.ItemsDone(),
					"items_total", len(checklist.Items),
				)},
				// Marks the final checkpoint as done for this id (a retry is a no-op).
				{Kind: "cycle", CycleID: inp.CycleID, Payload: contracts.Payload("outcome", "impossible", "blocked", blocked)},
			},
			CommitMessage: "lha: mission declared impossible",
		})
		if err != nil {
			return CycleResult{}, err
		}
	}
	snapshot, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	return resultFromSnapshot(snapshot, resultOpts{note: "declared impossible", reason: inp.Reason}), nil
}

// UnblockItems is unblock_items: reset every blocked item to retryable (a human chose "retry").
func (a *Activities) UnblockItems(ctx context.Context, inp UnblockInput) (CycleResult, error) {
	release, err := lockWorkdir(ctx, inp.Workdir, LockWait)
	if err != nil {
		return CycleResult{}, err
	}
	defer release()
	if err := a.resetWorkdir(ctx, inp.Workdir); err != nil {
		return CycleResult{}, err
	}
	anchor := state.NewGitMissionAnchor(inp.Workdir)
	checklist, err := anchor.ReadChecklist(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	blocked := blockedIDs(checklist)
	for _, id := range blocked {
		if _, err := checklist.Unblock(id); err != nil {
			return CycleResult{}, err
		}
	}
	if len(blocked) > 0 {
		joined := strings.Join(blocked, ", ")
		_, err := anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
			CycleID:         inp.CycleID,
			ProgressSummary: fmt.Sprintf("- %s human retry: unblocked %s", inp.CycleID, joined),
			Checklist:       checklist,
			Events:          []contracts.EventRecord{{Kind: "unblock", CycleID: inp.CycleID, Payload: contracts.Payload("items", blocked)}},
			CommitMessage:   fmt.Sprintf("lha: unblock %s (human retry)", joined),
		})
		if err != nil {
			return CycleResult{}, err
		}
	}
	snapshot, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	return resultFromSnapshot(snapshot, resultOpts{advanced: len(blocked) > 0, note: "unblocked " + pyList(blocked)}), nil
}

// EditEvent is the anchor event kind of an applied operator checklist edit batch.
const EditEvent = "checklist_edit"

// EditSummary is the one-line account of an applied batch (python: edit_summary).
func EditSummary(by string, lines []string) string {
	who := by
	if who == "" {
		who = "an operator"
	}
	return "checklist edited by " + who + ": " + strings.Join(lines, "; ")
}

// EditChecklist is edit_checklist: apply inp.Edits to the committed checklist in one anchor-only
// commit. The batch is applied atomically or refused (Advanced false, the refusal in Note); a
// refusal never fails the activity. Idempotent per cycle id: a retry that finds its own
// checklist_edit event (same edits, same sender) in HEAD applies nothing again.
func (a *Activities) EditChecklist(ctx context.Context, inp EditInput) (CycleResult, error) {
	release, err := lockWorkdir(ctx, inp.Workdir, LockWait)
	if err != nil {
		return CycleResult{}, err
	}
	defer release()
	if err := a.resetWorkdir(ctx, inp.Workdir); err != nil {
		return CycleResult{}, err
	}
	anchor := state.NewGitMissionAnchor(inp.Workdir)
	if done, ok := CommittedCycleEvent(ctx, inp.Workdir, inp.CycleID, EditEvent); ok && sameEdit(done, inp) {
		snapshot, err := anchor.ReadSituationalAwareness(ctx)
		if err != nil {
			return CycleResult{}, err
		}
		return resultFromSnapshot(snapshot, resultOpts{advanced: true, note: fmt.Sprint(done["summary"])}), nil
	}
	checklist, err := anchor.ReadChecklist(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	events, err := anchor.ReadEvents(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	edits := make([]any, len(inp.Edits))
	for i, e := range inp.Edits {
		edits[i] = e
	}
	lines, err := checklistedit.Apply(&checklist, edits, inp.By, checklistedit.WorkedItemIDs(events))
	var refused *checklistedit.Error
	if errors.As(err, &refused) {
		snapshot, err := anchor.ReadSituationalAwareness(ctx)
		if err != nil {
			return CycleResult{}, err
		}
		return resultFromSnapshot(snapshot, resultOpts{note: "checklist edit refused: " + refused.Message}), nil
	}
	if err != nil {
		return CycleResult{}, err
	}
	summary := EditSummary(inp.By, lines)
	who := inp.By
	if who == "" {
		who = "an operator"
	}
	if _, err := anchor.CommitAnchorUpdate(ctx, contracts.Checkpoint{
		CycleID:         inp.CycleID,
		ProgressSummary: "- " + inp.CycleID + " " + summary,
		Checklist:       checklist,
		Events: []contracts.EventRecord{{Kind: EditEvent, CycleID: inp.CycleID, Payload: contracts.Payload(
			"by", inp.By, "edits", nz(inp.Edits), "summary", summary,
		)}},
		CommitMessage: "lha: checklist edited by " + who,
	}); err != nil {
		return CycleResult{}, err
	}
	snapshot, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	return resultFromSnapshot(snapshot, resultOpts{advanced: true, note: summary}), nil
}

// sameEdit is whether a committed checklist_edit payload holds exactly inp's batch.
func sameEdit(done map[string]any, inp EditInput) bool {
	if fmt.Sprint(done["by"]) != inp.By {
		return false
	}
	want, err := json.Marshal(nz(inp.Edits))
	if err != nil {
		return false
	}
	got, err := json.Marshal(done["edits"])
	if err != nil {
		return false
	}
	var a, b any
	if json.Unmarshal(want, &a) != nil || json.Unmarshal(got, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// ReadMissionSnapshot is read_mission_snapshot: the committed counts + HEAD (read-only, no reset).
func (a *Activities) ReadMissionSnapshot(ctx context.Context, inp HealthInput) (CycleResult, error) {
	snapshot, err := state.NewGitMissionAnchor(inp.Workdir).ReadSituationalAwareness(ctx)
	if err != nil {
		return CycleResult{}, err
	}
	return resultFromSnapshot(snapshot, resultOpts{note: "snapshot"}), nil
}

// RecordMissionStatus is record_mission_status: upsert the missions row's status (idempotent).
// A store that cannot be used at all is a non-retryable MissionConfigError; the workflow never
// lets a failure here fail or block the mission.
func (a *Activities) RecordMissionStatus(ctx context.Context, inp MissionStatusInput) (bool, error) {
	settings, err := a.settings()
	if err != nil {
		return false, configError(fmt.Sprintf("invalid configuration: %v", err), err)
	}
	store, err := a.openStore(ctx, settings, inp.Workdir)
	if err != nil {
		if errors.Is(err, ErrStoreUnavailable) {
			return false, configError(fmt.Sprintf("cannot open the mission store: %v", err), err)
		}
		return false, err
	}
	defer store.Close(context.WithoutCancel(ctx))
	if err := store.UpsertMission(ctx, MissionRow{
		MissionID: inp.MissionID, Status: inp.Status, HeadSHA: deref(inp.HeadSHA), WorkflowID: workflowIDOf(ctx),
	}); err != nil {
		return false, err
	}
	activityLogger().Info("mission_status_recorded", "mission_id", inp.MissionID, "status", inp.Status, "reason", inp.Reason)
	return true, nil
}

// RunSubAgent is run_subagent: one sub-agent (the body of a SubAgentWorkflow).
func (a *Activities) RunSubAgent(ctx context.Context, inp SubAgentInput) (SubAgentOutput, error) {
	if a.SubAgent != nil {
		return a.SubAgent(ctx, inp)
	}
	return a.runSubAgent(ctx, inp)
}

// ResolvedByFor is hitl_gates.resolved_by for a notice: the decider when human_decision_v2
// named one, else how the gate closed (python: resolved_by_for).
func ResolvedByFor(n GateNotice) string {
	switch n.Event {
	case "resolved":
		if n.By != "" {
			return n.By + " (human_decision signal)"
		}
		return ResolvedBySignal
	case "defaulted":
		return ResolvedByTimeout
	}
	return ""
}
