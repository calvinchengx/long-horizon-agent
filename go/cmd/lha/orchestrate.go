package main

import (
	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents/org"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

const (
	orchestrateHelp = "Plan, then run the FULL multi-agent org (research, Lead or parallel waves, review) locally."
	resumeHelp      = "Continue the mission already anchored in --workdir (no planning; its checklist, " +
		"ownership map and decisions are kept)."
)

// orchestrate is python's `lha orchestrate`: plan the task (or import --checklist), then run the
// multi-agent organization; --resume continues the mission anchored in --workdir.
func (c *cli) orchestrate(args []string) error {
	fs := c.newFlags("orchestrate", orchestrateHelp)
	var f runFlags
	f.register(fs, ".lha/workspaces/org")
	task := fs.String("task", "", "The mission / task description (required unless --resume or --checklist).")
	fs.Var(&f.title, "title", "Mission title (default: the checklist's, else 'mission').")
	resume := fs.Bool("resume", false, resumeHelp)
	if err := c.parse(fs, args); err != nil {
		return err
	}
	workdir := f.workdir
	hasAnchor := org.AnchorExists(c.ctx, workdir)
	if *resume && !hasAnchor {
		return fail(2, "--resume: no mission anchor in %s (start one without --resume)", contracts.PyRepr(workdir))
	}
	if !*resume && hasAnchor {
		return fail(2, "%s already holds a mission: pass --resume to continue it, or use a new "+
			"--workdir (starting over would replace its checklist)", contracts.PyRepr(workdir))
	}
	if *resume && f.checklist.value != "" {
		return fail(2, "--checklist cannot be combined with --resume (the mission keeps its checklist)")
	}
	if !*resume && pyfmt.PyStrip(*task) == "" && f.checklist.value == "" {
		return fail(2, "give --task or --checklist FILE (or --resume to continue an existing mission)")
	}
	commands, err := resolveCheckCommands(f.check, f.noDefaultChecks)
	if err != nil {
		return err
	}
	checks := contracts.ChecksFromCommands(commands, true)
	settings, err := runSettings(&f)
	if err != nil {
		return err
	}
	obs.ConfigureLogging(c.stderr, false)
	var gate contracts.HITLGate
	if f.approveInteractive {
		gate = consoleGate(settings)
	}
	meter := agent.BuildMeter(settings) // planner + every org role share one budget
	opts := org.DefaultOrchestratorOptions()
	opts.Meter = meter
	opts.Models = c.orgModels
	orchestrator := org.NewOrchestrator(settings, opts)
	mission := org.MissionOptions{Workdir: workdir, Checks: checks, Gate: gate, Resume: *resume}
	if !*resume {
		if f.checklist.value != "" {
			imported, err := loadChecklistFile(f.checklist.value)
			if err != nil {
				return err
			}
			mission.Title = firstNonEmpty(f.title.value, imported.Title, "mission")
			mission.Description = firstNonEmpty(*task, imported.Description)
			mission.Checklist = &imported.Checklist
			mission.References = mergeReferences(f.reference, imported.References)
		} else {
			plan, err := c.plan(settings, meter, firstNonEmpty(f.title.value, "mission"), *task)
			if err != nil {
				return runError(err)
			}
			mission.Title = firstNonEmpty(f.title.value, "mission")
			mission.Description = *task
			mission.Checklist = &plan.Checklist
			mission.References = append([]string{}, f.reference...)
			mission.Ownership = plan.Ownership
		}
	}
	summary, err := orchestrator.RunMission(c.ctx, mission)
	if err != nil {
		return runError(err)
	}
	return c.report(summary)
}

// plan runs the Planner on the configured model (or the test override), metered as "planner".
func (c *cli) plan(settings *config.Settings, meter *governor.CostMeter, title, task string) (agents.MissionPlan, error) {
	plannerModel := c.leadModel
	if plannerModel == nil {
		built, err := model.BuildProvider(settings, "", nil)
		if err != nil {
			return agents.MissionPlan{}, err
		}
		defer agent.CloseProvider(c.ctx, built)
		plannerModel = built
	}
	plan, err := agents.NewPlanner(meter.Wrap(plannerModel, "planner")).PlanMission(c.ctx, title, task, "")
	if err != nil {
		return agents.MissionPlan{}, err
	}
	if plan.Ownership == nil {
		plan.Ownership = coordination.NewFileOwnershipMap()
	}
	return plan, nil
}
