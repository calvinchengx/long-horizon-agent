package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/durable"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// The durable commands (python: lha.cli.main worker / mission-start / mission-status /
// mission-approve / mission-snooze / mission-abort), with the Python options, output and exit
// codes. They talk to Temporal at LHA_TEMPORAL_ADDRESS with the ClaimCheck data converter, so a
// mission started by either implementation can be queried, signalled and aborted by the other.

// dialTemporal connects to Temporal (tests replace it).
var dialTemporal = func(ctx context.Context, settings *config.Settings) (client.Client, error) {
	return durable.Dial(ctx, settings, nil)
}

// parseWithArgs parses flags that may come before or after positional arguments (click accepts
// both) and returns the positional arguments.
func (c *cli) parseWithArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &exitError{code: 2}
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// missionID takes the single MISSION_ID argument (click's messages on a missing / extra one).
func (c *cli) missionID(name string, positional []string) (string, error) {
	switch {
	case len(positional) == 0:
		return "", &exitError{code: 2, message: fmt.Sprintf("Usage: lha %s [OPTIONS] MISSION_ID\nTry 'lha %s --help' for help.\n\nError: Missing argument 'MISSION_ID'.", name, name)}
	case len(positional) > 1:
		return "", &exitError{code: 2, message: fmt.Sprintf("Error: Got unexpected extra argument (%s)", positional[1])}
	}
	return positional[0], nil
}

// intFlag / floatFlag are options that remember whether they were given (python: int | None).
type intFlag struct {
	value int
	set   bool
}

func (f *intFlag) String() string { return strconv.Itoa(f.value) }
func (f *intFlag) Set(v string) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("'%s' is not a valid integer", v)
	}
	f.value, f.set = n, true
	return nil
}

type floatFlag struct {
	value float64
	set   bool
}

func (f *floatFlag) String() string { return strconv.FormatFloat(f.value, 'g', -1, 64) }
func (f *floatFlag) Set(v string) error {
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return fmt.Errorf("'%s' is not a valid float", v)
	}
	f.value, f.set = n, true
	return nil
}

// --- worker ----------------------------------------------------------------------------------

func (c *cli) worker(args []string) error {
	if err := c.parse(c.newFlags("worker", "Run a Temporal worker that serves missions (requires a Temporal server)."), args); err != nil {
		return err
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	if _, err := settings.ResetKeepPaths(); err != nil { // fails here, not in every cycle
		return fail(2, "%s", err.Error())
	}
	if _, err := durable.SweepObjects(settings); err != nil {
		return err
	}
	deployment, versioned, err := durable.DeploymentOptions(settings) // a half-set deployment fails here
	if err != nil {
		return fail(2, "%s", err.Error())
	}
	obs.ConfigureLogging(c.stderr, false)
	cl, err := durable.Dial(c.ctx, settings, durable.Logger(c.stderr, slog.LevelInfo))
	if err != nil {
		return err
	}
	defer cl.Close()
	// Fail closed: never share a task queue with Python workers (see durable.CheckTaskQueuePollers).
	if err := durable.CheckTaskQueuePollers(c.ctx, cl, settings.TaskQueue); err != nil {
		var mixed *durable.MixedWorkersError
		if errors.As(err, &mixed) {
			return fail(2, "%s", err)
		}
		return err
	}
	acts := &durable.Activities{Settings: settings, OpenToolbox: openToolbox}
	if c.leadModel != nil {
		lead := c.leadModel
		acts.ModelFactory = func(*config.Settings, contracts.SituationSnapshot) (contracts.ModelProvider, error) { return lead, nil }
	}
	w := durable.NewVersionedWorker(cl, settings.TaskQueue, acts, deployment)
	var onStart func(context.Context) error
	if versioned {
		version := deployment.Version
		onStart = func(ctx context.Context) error {
			if err := durable.AnnounceVersion(ctx, cl.WorkflowService(), settings.TemporalNamespace,
				version.DeploymentName, version.BuildID, settings.WorkerPromote, slog.Default()); err != nil {
				return &promoteError{err}
			}
			return nil
		}
	}
	// Keep re-checking: a Python worker that raced past its own startup check stops this one.
	interval := time.Duration(settings.WorkerGuardIntervalS * float64(time.Second))
	err = durable.RunGuardedWith(c.ctx, w, cl, settings.TaskQueue, interval, func(err error) {
		slog.Warn("cannot re-check who polls the task queue", "task_queue", settings.TaskQueue, "error", err)
	}, onStart)
	var mixed *durable.MixedWorkersError
	if errors.As(err, &mixed) {
		return fail(2, "%s", err)
	}
	var promote *promoteError
	if errors.As(err, &promote) { // LHA_WORKER_PROMOTE could not promote this build
		return fail(1, "%s", promote.err)
	}
	return err
}

// --- mission-start ---------------------------------------------------------------------------

func (c *cli) missionStart(args []string) error {
	fs := c.newFlags("mission-start", commandHelpFor("mission-start"))
	task := fs.String("task", "", "The mission / task description (the Planner decomposes it).")
	var title, checklistFile, deadlockDefault optional
	fs.Var(&title, "title", "Mission title (default: 'mission').")
	fs.Var(&checklistFile, "checklist", checklistHelp)
	var reference, check multi
	fs.Var(&reference, "reference", referenceHelp)
	workdir := fs.String("workdir", ".lha/workspaces/durable",
		"Workspace dir (becomes a git repo); resolved to an absolute path for the worker.")
	fs.Var(&check, "check", checkHelp)
	noDefaultChecks := fs.Bool("no-default-checks", false, noDefaultChecksHelp)
	deadlockGateHours := fs.Float64("deadlock-gate-hours", 24.0,
		"On deadlock, wait this long for a human 'retry', 'abort' or 'impossible' (0 = end immediately).")
	var approvalTimeoutHours floatFlag
	fs.Var(&approvalTimeoutHours, "approval-timeout-hours", "How long an irreversible action waits for approval "+
		"before it is rejected (default: LHA_APPROVAL_TIMEOUT_S, 24h).")
	var maxCycles, cyclePause intFlag
	fs.Var(&maxCycles, "max-cycles", "Cycle ceiling (default: LHA_MAX_CYCLES).")
	fs.Var(&deadlockDefault, "deadlock-default", "Deadlock gate decision when nobody answers: abort | impossible "+
		"(default: LHA_DEADLOCK_GATE_DEFAULT, else abort).")
	fs.Var(&cyclePause, "cycle-pause-seconds", "Durable pause between cycles (status SLEEPING). Default: LHA_CYCLE_PAUSE_SECONDS.")
	startIn := fs.Int("start-in-seconds", 0, "Sleep (status SLEEPING) before the first cycle.")
	research := fs.Int("research", 0, "Read-only researcher child workflows per item before each round (0 = none).")
	review := fs.Bool("review", false, "An independent reviewer after every verified item; a blocking review reopens it.")
	noReview := fs.Bool("no-review", false, "No independent reviewer (the default).")
	maxParallel := fs.Int("max-parallel", 0, "Parallel implementer waves of up to N items with disjoint Planner-assigned "+
		"files, each in its own git worktree (below 2 = never).")
	if err := c.parse(fs, args); err != nil {
		return err
	}
	switch {
	case cyclePause.set && cyclePause.value < 0:
		return rangeError("mission-start", "cycle-pause-seconds", cyclePause.value, 0, 0, false)
	case *startIn < 0:
		return rangeError("mission-start", "start-in-seconds", *startIn, 0, 0, false)
	case *research < 0 || *research > durable.MaxResearchPerItem:
		return rangeError("mission-start", "research", *research, 0, durable.MaxResearchPerItem, true)
	case *maxParallel < 0 || *maxParallel > durable.MaxParallel:
		return rangeError("mission-start", "max-parallel", *maxParallel, 0, durable.MaxParallel, true)
	}
	withReview := *review && !*noReview
	if pyfmt.PyStrip(*task) == "" && checklistFile.value == "" {
		return fail(2, "give --task (to plan) or --checklist FILE (to import a checklist)")
	}
	checkCommands, err := resolveCheckCommands(check, *noDefaultChecks)
	if err != nil {
		return err
	}
	var imported *importedChecklist
	if checklistFile.value != "" {
		got, err := loadChecklistFile(checklistFile.value)
		if err != nil {
			return err
		}
		imported = &importedChecklist{got.Title, got.Description, got.Checklist, got.References}
	}
	absWorkdir, err := filepath.Abs(*workdir) // the worker may run from another directory
	if err != nil {
		return err
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	choice := settings.DeadlockGateDefault
	if deadlockDefault.set {
		choice = deadlockDefault.value
	}
	choice = strings.ToLower(pyfmt.PyStrip(choice))
	if choice != "abort" && choice != "impossible" {
		return fail(2, "--deadlock-default must be one of %s", strings.Join(durable.DeadlockDefaults, ", "))
	}
	pause := settings.CyclePauseSeconds
	if cyclePause.set {
		pause = cyclePause.value
	}
	resumeAt := 0.0
	if *startIn > 0 {
		resumeAt = float64(time.Now().UnixNano())/1e9 + float64(*startIn)
	}

	if imported != nil && *maxParallel >= 2 {
		fmt.Fprintln(c.stderr, "note: an imported checklist declares no file ownership, so no parallel wave can run "+
			"(items are worked serially)")
	}
	missionID, err := c.startMission(settings, startRequest{
		task: *task, title: title.value, imported: imported, references: reference, workdir: absWorkdir,
		withOwnership: *maxParallel >= 2,
		build: func(id string) durable.MissionInput {
			inp := durable.NewMissionInput(id, absWorkdir)
			inp.CheckCommands = checkCommands
			inp.MaxCycles = settings.MaxCycles
			if maxCycles.set {
				inp.MaxCycles = maxCycles.value
			}
			inp.DeadlockGateSeconds = int(*deadlockGateHours * 3600)
			inp.ApprovalTimeoutSeconds = settings.ApprovalTimeoutS
			if approvalTimeoutHours.set {
				inp.ApprovalTimeoutSeconds = int(approvalTimeoutHours.value * 3600)
			}
			inp.GateEscalationSeconds = append([]int{}, settings.GateEscalationSeconds...)
			inp.DeadlockGateDefault = choice
			inp.ImpossibleAfterFailures = settings.ImpossibleAfterFailures
			inp.CyclePauseSeconds = pause
			inp.ResumeAt = resumeAt
			inp.ResearchPerItem = *research
			inp.Review = withReview
			inp.MaxParallel = *maxParallel
			return inp
		},
		sleeping: resumeAt != 0,
	})
	if err != nil {
		return runError(err)
	}
	fmt.Fprintf(c.stdout, "started mission %s (workflow id: %s)\n", missionID, durable.MissionWorkflowID(missionID))
	return nil
}

type importedChecklist struct {
	title, description string
	checklist          contracts.Checklist
	references         []string
}

type startRequest struct {
	task, title string
	imported    *importedChecklist
	references  []string
	workdir     string
	// withOwnership: plan with the Planner's single-writer file ownership (parallel waves need it).
	withOwnership bool
	build         func(missionID string) durable.MissionInput
	sleeping      bool
}

// startMission plans (or imports) the checklist, initializes the anchor, writes the mission row
// (and the Planner's spend), and starts the MissionWorkflow (python: mission_start._start).
func (c *cli) startMission(settings *config.Settings, r startRequest) (string, error) {
	ctx := c.ctx
	references := append([]string{}, r.references...)
	var planned contracts.Checklist
	var mTitle, description string
	var plannerSpend []governor.CostEntry
	var ownershipJSON []byte
	if r.imported != nil {
		planned = r.imported.checklist
		mTitle = firstNonEmpty(r.title, r.imported.title, "mission")
		description = firstNonEmpty(r.task, r.imported.description)
		references = mergeReferences(references, r.imported.references)
	} else {
		mTitle, description = firstNonEmpty(r.title, "mission"), r.task
		meter := agent.BuildMeter(settings)
		plannerModel := c.leadModel
		if plannerModel == nil {
			built, err := model.BuildProvider(settings, "", nil)
			if err != nil {
				return "", err
			}
			defer agent.CloseProvider(context.WithoutCancel(ctx), built)
			plannerModel = built
		}
		plan, err := agents.NewPlanner(meter.Wrap(plannerModel, "planner")).PlanMission(ctx, mTitle, r.task, "")
		if err != nil {
			return "", err
		}
		planned = plan.Checklist
		if r.withOwnership { // waves need the Planner's single-writer file ownership
			data, err := coordination.OwnershipJSON(plan.Ownership)
			if err != nil {
				return "", err
			}
			ownershipJSON = data
		}
		for _, e := range meter.Ledger.Entries() {
			plannerSpend = append(plannerSpend, e)
		}
	}
	anchor := state.NewGitMissionAnchor(r.workdir)
	if _, err := anchor.InitializeSpecWithOwnership(ctx, contracts.MissionSpec{
		Title: mTitle, Description: description, References: references,
	}, planned, ownershipJSON); err != nil {
		return "", err
	}
	cl, err := dialTemporal(ctx, settings)
	if err != nil {
		return "", err
	}
	defer cl.Close()
	missionID := agent.NewID("mission")
	workflowID := durable.MissionWorkflowID(missionID)
	// The mission row + the Planner's spend, written BEFORE the workflow starts so a status the
	// workflow records right away (e.g. SLEEPING for a scheduled start) is never overwritten.
	store, err := durable.DefaultStoreOpener(ctx, settings, r.workdir)
	if err != nil {
		return "", err
	}
	defer store.Close(context.WithoutCancel(ctx))
	for i, e := range plannerSpend {
		if _, err := store.RecordCost(ctx, missionID, e, fmt.Sprintf("planner#%d", i)); err != nil {
			obs.Logger("lha.persistence").Warn("cost_ledger_write_failed", "mission_id", missionID, "error", err.Error())
		}
	}
	setStatus := func(status string) {
		if err := store.UpsertMission(context.WithoutCancel(ctx), durable.MissionRow{
			MissionID: missionID, Title: mTitle, Description: description, Status: status, WorkflowID: workflowID,
		}); err != nil {
			obs.Logger("lha.persistence").Warn("mission_upsert_failed", "mission_id", missionID, "status", status, "error", err.Error())
		}
	}
	if r.sleeping {
		setStatus(durable.StatusSleeping)
	} else {
		setStatus(durable.StatusRunning)
	}
	if _, err := cl.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: workflowID, TaskQueue: settings.TaskQueue},
		durable.WorkflowMission, r.build(missionID)); err != nil {
		setStatus(durable.StatusAborted)
		return "", err
	}
	return missionID, nil
}

// --- mission-status / approve / snooze / abort -------------------------------------------------

// formatGate is python's format_gate: human-readable lines for the open gate.
func formatGate(gate *durable.GateView) []string {
	if gate == nil {
		return nil
	}
	reminders := fmt.Sprintf("  reminders sent: %d", gate.EscalationsSent)
	if gate.NextEscalationAt != "" {
		reminders += "  next reminder: " + gate.NextEscalationAt
	}
	lines := []string{
		fmt.Sprintf("gate: %s %s", gate.Kind, gate.GateID),
		"  question: " + gate.Question,
		fmt.Sprintf("  options: %s  (default on timeout: %s)", strings.Join(gate.Options, " | "), gate.DefaultAction),
		fmt.Sprintf("  opened: %s  deadline: %s", gate.OpenedAt, gate.Deadline),
		reminders,
	}
	if gate.Recommended != "" {
		lines = append(lines, "  recommended: "+gate.Recommended)
	}
	if gate.Request != nil {
		lines = append(lines,
			fmt.Sprintf("  pending action: %s %s", gate.Request.Tool, gate.Request.Arguments),
			"  reason: "+gate.Request.Reason,
			"  fingerprint: "+gate.Request.Fingerprint,
		)
	}
	return lines
}

// allDecisions are every gate's options (python: _ALL_DECISIONS).
var allDecisions = []string{"approve", "reject", "retry", "abort", "impossible"}

// checkDecision is python's check_decision: the normalized decision, validated against the open
// gate's options; with no gate open any known decision is accepted (the workflow holds it).
func checkDecision(gate *durable.GateView, decision string) (string, error) {
	choice := strings.ToLower(pyfmt.PyStrip(decision))
	options := allDecisions
	where := "a mission gate"
	if gate != nil {
		options, where = gate.Options, "the open "+gate.Kind+" gate"
	}
	for _, o := range options {
		if o == choice {
			return choice, nil
		}
	}
	return "", fmt.Errorf("unknown --decision %s for %s; expected %s", contracts.PyRepr(decision), where, strings.Join(options, ", "))
}

// query runs a query into out; optional=true turns "the workflow does not answer it" into
// (false, nil) (python: _optional_query).
func query(ctx context.Context, cl client.Client, workflowID, name string, out any, optional bool) (bool, error) {
	v, err := cl.QueryWorkflow(ctx, workflowID, "", name)
	if err != nil {
		var failed *serviceerror.QueryFailed
		if optional && errors.As(err, &failed) {
			return false, nil
		}
		return false, err
	}
	if !v.HasValue() {
		return false, nil
	}
	if err := v.Get(out); err != nil {
		return false, err
	}
	return true, nil
}

func queryGate(ctx context.Context, cl client.Client, workflowID string) (*durable.GateView, error) {
	var gate *durable.GateView
	if _, err := query(ctx, cl, workflowID, durable.QueryGate, &gate, true); err != nil {
		return nil, err
	}
	return gate, nil
}

func (c *cli) connect() (client.Client, error) {
	settings, err := config.Load()
	if err != nil {
		return nil, err
	}
	return dialTemporal(c.ctx, settings)
}

func (c *cli) missionStatus(args []string) error {
	fs := c.newFlags("mission-status", commandHelpFor("mission-status"))
	positional, err := c.parseWithArgs(fs, args)
	if err != nil {
		return err
	}
	id, err := c.missionID("mission-status", positional)
	if err != nil {
		return err
	}
	cl, err := c.connect()
	if err != nil {
		return err
	}
	defer cl.Close()
	wid := durable.MissionWorkflowID(id)
	var status any
	var cycles any
	if _, err := query(c.ctx, cl, wid, durable.QueryStatus, &status, false); err != nil {
		return err
	}
	if _, err := query(c.ctx, cl, wid, durable.QueryCycles, &cycles, false); err != nil {
		return err
	}
	lines := []string{fmt.Sprintf("status=%s cycles=%s", pyValue(status), pyValue(cycles))}
	gate, err := queryGate(c.ctx, cl, wid)
	if err != nil {
		return err
	}
	if gate != nil {
		lines = append(lines, formatGate(gate)...)
	} else {
		var question string
		if _, err := query(c.ctx, cl, wid, durable.QueryOpenQuestion, &question, true); err != nil {
			return err
		}
		if question != "" {
			lines = append(lines, "waiting on: "+question)
		}
	}
	var resumeAt float64
	if _, err := query(c.ctx, cl, wid, durable.QueryResumeAt, &resumeAt, true); err != nil {
		return err
	}
	if resumeAt != 0 {
		lines = append(lines, "sleeping until "+durable.IsoFull(resumeAt))
	}
	var notes []string
	if _, err := query(c.ctx, cl, wid, durable.QuerySteerNotes, &notes, true); err != nil {
		return err
	}
	if len(notes) > 0 {
		lines = append(lines, fmt.Sprintf("steering notes (%d, latest last):", len(notes)))
		if len(notes) > 3 {
			notes = notes[len(notes)-3:]
		}
		for _, n := range notes {
			lines = append(lines, "  "+oneLine(n, 120))
		}
	}
	var log []string
	if _, err := query(c.ctx, cl, wid, durable.QueryGateLog, &log, true); err != nil {
		return err
	}
	if len(log) > 0 {
		lines = append(lines, "recent gate events:")
		if len(log) > 8 {
			log = log[len(log)-8:]
		}
		for _, l := range log {
			lines = append(lines, "  "+l)
		}
	}
	for _, l := range lines {
		fmt.Fprintln(c.stdout, l)
	}
	return nil
}

// pyValue renders a JSON query answer like Python's f-string of the decoded value.
func pyValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}

func (c *cli) missionApprove(args []string) error {
	fs := c.newFlags("mission-approve", commandHelpFor("mission-approve"))
	var decision, by optional
	fs.Var(&by, "as", "Who decides; recorded as the gate's resolved_by (`lha gates`).")
	fs.Var(&decision, "decision", "approve | reject (an irreversible action), retry | abort | impossible (a deadlock); "+
		"checked against the open gate - see 'lha mission-status'.")
	positional, err := c.parseWithArgs(fs, args)
	if err != nil {
		return err
	}
	id, err := c.missionID("mission-approve", positional)
	if err != nil {
		return err
	}
	if !decision.set {
		return &exitError{code: 2, message: "Usage: lha mission-approve [OPTIONS] MISSION_ID\nTry 'lha mission-approve --help' for help.\n\nError: Missing option '--decision'."}
	}
	if _, err := checkDecision(nil, decision.value); err != nil {
		return fail(2, "unknown --decision %s; expected %s", contracts.PyRepr(decision.value), strings.Join(allDecisions, ", "))
	}
	who := pyfmt.PyStrip(by.value)
	if len([]rune(who)) > 200 {
		return fail(2, "--as is longer than 200 characters")
	}
	cl, err := c.connect()
	if err != nil {
		return err
	}
	defer cl.Close()
	wid := durable.MissionWorkflowID(id)
	gate, err := queryGate(c.ctx, cl, wid)
	if err != nil {
		return err
	}
	choice, err := checkDecision(gate, decision.value)
	if err != nil {
		for _, l := range formatGate(gate) {
			fmt.Fprintln(c.stderr, l)
		}
		return fail(2, "%s", err)
	}
	if who != "" {
		if err := cl.SignalWorkflow(c.ctx, wid, "", durable.SignalHumanDecisionV2, map[string]any{"decision": choice, "by": who}); err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "sent decision '%s' to mission %s as %s\n", choice, id, who)
		return nil
	}
	if err := cl.SignalWorkflow(c.ctx, wid, "", durable.SignalHumanDecision, choice); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "sent decision '%s' to mission %s\n", choice, id)
	return nil
}

func (c *cli) missionSnooze(args []string) error {
	fs := c.newFlags("mission-snooze", commandHelpFor("mission-snooze"))
	var seconds intFlag
	fs.Var(&seconds, "seconds", "Sleep (status SLEEPING) this long before the next cycle; 0 wakes it.")
	positional, err := c.parseWithArgs(fs, args)
	if err != nil {
		return err
	}
	id, err := c.missionID("mission-snooze", positional)
	if err != nil {
		return err
	}
	if !seconds.set {
		return &exitError{code: 2, message: "Usage: lha mission-snooze [OPTIONS] MISSION_ID\nTry 'lha mission-snooze --help' for help.\n\nError: Missing option '--seconds'."}
	}
	if seconds.value < 0 {
		return rangeError("mission-snooze [OPTIONS] MISSION_ID", "seconds", seconds.value, 0, 0, false)
	}
	cl, err := c.connect()
	if err != nil {
		return err
	}
	defer cl.Close()
	if err := cl.SignalWorkflow(c.ctx, durable.MissionWorkflowID(id), "", durable.SignalSnooze, seconds.value); err != nil {
		return err
	}
	if seconds.value > 0 {
		fmt.Fprintf(c.stdout, "mission %s: snoozed %ds\n", id, seconds.value)
	} else {
		fmt.Fprintf(c.stdout, "mission %s: woken\n", id)
	}
	return nil
}

func (c *cli) missionAbort(args []string) error {
	fs := c.newFlags("mission-abort", commandHelpFor("mission-abort"))
	positional, err := c.parseWithArgs(fs, args)
	if err != nil {
		return err
	}
	id, err := c.missionID("mission-abort", positional)
	if err != nil {
		return err
	}
	cl, err := c.connect()
	if err != nil {
		return err
	}
	defer cl.Close()
	if err := cl.CancelWorkflow(c.ctx, durable.MissionWorkflowID(id), ""); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "cancelled mission %s\n", id)
	return nil
}

// promoteError marks a failure to announce or promote the worker's build.
type promoteError struct{ err error }

func (e *promoteError) Error() string { return e.err.Error() }
func (e *promoteError) Unwrap() error { return e.err }

// oneLine is text on one line, cut to limit characters with an ellipsis (python: _one_line).
func oneLine(text string, limit int) string {
	flat := strings.Join(strings.Fields(text), " ")
	r := []rune(flat)
	if len(r) <= limit {
		return flat
	}
	return string(r[:limit-3]) + "..."
}

// missionSteer is python's mission-steer: append an operator note every following cycle's
// prompt includes.
func (c *cli) missionSteer(args []string) error {
	fs := c.newFlags("mission-steer", commandHelpFor("mission-steer"))
	var note optional
	fs.Var(&note, "note", "The note (at most 2000 characters); every following cycle's prompt includes it.")
	positional, err := c.parseWithArgs(fs, args)
	if err != nil {
		return err
	}
	id, err := c.missionID("mission-steer", positional)
	if err != nil {
		return err
	}
	if !note.set {
		return &exitError{code: 2, message: "Usage: lha mission-steer [OPTIONS] MISSION_ID\nTry 'lha mission-steer --help' for help.\n\nError: Missing option '--note'."}
	}
	text := pyfmt.PyStrip(note.value)
	if text == "" {
		return fail(2, "--note must not be empty")
	}
	if n := len([]rune(text)); n > durable.MaxSteerChars {
		return fail(2, "--note is %d characters; at most %d are kept", n, durable.MaxSteerChars)
	}
	cl, err := c.connect()
	if err != nil {
		return err
	}
	defer cl.Close()
	if err := cl.SignalWorkflow(c.ctx, durable.MissionWorkflowID(id), "", durable.SignalSteer, text); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "mission %s: steering note added (%d chars; the last %d notes are kept)\n", id, len([]rune(text)), durable.MaxSteerNotes)
	return nil
}
