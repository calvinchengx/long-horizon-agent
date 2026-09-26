// Command lha is the Go implementation of the LHA command line (python: lha.cli.main), built on
// the standard library flag package. It implements version, config, run-local, mission and
// decisions with the Python CLI's options, output and exit codes; every other Python command
// prints that it is not yet available and exits 2. The hidden egress-proxy command serves the
// Docker sandbox's allow-list egress proxy.
//
//	go build -o lha ./cmd/lha
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/checklistimport"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/egressproxy"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// Version is the LHA version (python: lha.__version__).
const Version = "0.1.0"

const appHelp = "LHA — a durable, self-improving agent organization for long-horizon software missions."

// notPorted are the Python commands the Go CLI does not implement yet.
var notPorted = []string{
	"orchestrate", "worker", "mission-start", "mission-status", "mission-approve", "mission-abort",
	"mission-snooze", "missions", "costs", "db", "vendor",
}

var commandHelp = []struct{ name, help string }{
	{"version", "Print the installed LHA version."},
	{"config", "Show the resolved runtime configuration (secrets redacted), and where the store is."},
	{"run-local", "Run a mission locally (no Temporal) until complete / deadlocked / over-budget."},
	{"mission", "Plan a task into a checklist (or import one), then run it locally to completion."},
	{"decisions", "Print the mission's committed design decisions (.lha/decisions.ndjson), or verify them."},
}

// exitError ends a command with an exit code (message "" prints nothing).
type exitError struct {
	code    int
	message string
}

func (e *exitError) Error() string { return e.message }

// fail is python's _fail: "error: <message>" on stderr, exit code (default 2).
func fail(code int, format string, args ...any) error {
	return &exitError{code: code, message: "error: " + fmt.Sprintf(format, args...)}
}

type cli struct {
	stdout, stderr io.Writer
	ctx            context.Context
	// leadModel overrides the configured model for the planner and the lead (tests only).
	leadModel contracts.ModelProvider
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c := &cli{stdout: os.Stdout, stderr: os.Stderr, ctx: ctx}
	shutdownTracing := startTracing(os.Args[1:])
	code := c.run(os.Args[1:])
	shutdownTracing()
	os.Exit(code)
}

func (c *cli) usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: lha [OPTIONS] COMMAND [ARGS]...")
	fmt.Fprintln(w)
	fmt.Fprintln(w, appHelp)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, cmd := range commandHelp {
		fmt.Fprintf(w, "  %-12s %s\n", cmd.name, cmd.help)
	}
	fmt.Fprintf(w, "\nNot yet available in the Go implementation (use the Python lha): %s\n",
		strings.Join(notPorted, ", "))
}

// run executes one command line and returns the process exit code.
func (c *cli) run(args []string) int {
	if len(args) == 0 {
		c.usage(c.stdout) // like typer's no_args_is_help: help, exit 2
		return 2
	}
	name, rest := args[0], args[1:]
	var err error
	switch name {
	case "--help", "-h", "help":
		c.usage(c.stdout)
		return 0
	case "version":
		err = c.version(rest)
	case "config":
		err = c.config(rest)
	case "run-local":
		err = c.runLocal(rest)
	case "mission":
		err = c.mission(rest)
	case "decisions":
		err = c.decisions(rest)
	case "egress-proxy": // hidden: the Docker sandbox's allow-list egress proxy
		err = c.egressProxy(rest)
	default:
		for _, np := range notPorted {
			if name == np {
				fmt.Fprintf(c.stderr, "error: 'lha %s' is not yet available in the Go implementation; use the Python lha\n", name)
				return 2
			}
		}
		fmt.Fprintln(c.stderr, "Usage: lha [OPTIONS] COMMAND [ARGS]...")
		fmt.Fprintln(c.stderr, "Try 'lha --help' for help.")
		fmt.Fprintf(c.stderr, "\nError: No such command %s.\n", pyQuote(name))
		return 2
	}
	return c.exitCode(err)
}

func pyQuote(s string) string { return "'" + s + "'" }

func (c *cli) exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exitError
	if errors.As(err, &exit) {
		if exit.message != "" {
			fmt.Fprintln(c.stderr, exit.message)
		}
		return exit.code
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintln(c.stderr, "error: "+err.Error())
	return 1
}

// newFlags is a subcommand FlagSet whose parse errors exit 2 (like click's usage errors).
func (c *cli) newFlags(name, help string) *flag.FlagSet {
	fs := flag.NewFlagSet("lha "+name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.Usage = func() {
		fmt.Fprintf(c.stderr, "Usage: lha %s [OPTIONS]\n\n%s\n\nOptions:\n", name, help)
		fs.PrintDefaults()
	}
	return fs
}

func (c *cli) parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &exitError{code: 2}
	}
	if fs.NArg() > 0 {
		return &exitError{code: 2, message: fmt.Sprintf("Error: Got unexpected extra argument (%s)", fs.Arg(0))}
	}
	return nil
}

// multi is a repeatable string option.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// optional is a string option that remembers whether it was given (python: str | None).
type optional struct {
	value string
	set   bool
}

func (o *optional) String() string     { return o.value }
func (o *optional) Set(v string) error { o.value, o.set = v, true; return nil }

// --- version / config ------------------------------------------------------------------------

func (c *cli) version(args []string) error {
	if err := c.parse(c.newFlags("version", "Print the installed LHA version."), args); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "lha %s\n", Version)
	return nil
}

func (c *cli) config(args []string) error {
	if err := c.parse(c.newFlags("config", commandHelp[1].help), args); err != nil {
		return err
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	for _, kv := range settings.Redacted() {
		fmt.Fprintf(c.stdout, "%s = %s\n", kv.Key, config.FormatPy(kv.Value))
	}
	fmt.Fprintf(c.stdout, "mission store = %s\n", settings.DescribeStore())
	return nil
}

// --- run-local / mission -----------------------------------------------------------------------

const (
	checkHelp = `A gating verification command, shell-quoted (repeatable), e.g. --check "uv run pytest -q". ` +
		"Added to the default Python checks (ruff, ty, pytest) unless --no-default-checks."
	noDefaultChecksHelp = "Do not add the default Python checks; requires at least one --check " +
		"(an item is never marked done without a gating check)."
	sandboxHelp     = "Execution sandbox: docker | e2b | local (default: LHA_SANDBOX, else docker)."
	unsafeLocalHelp = "Allow the 'local' sandbox: agent commands run directly on this host with NO isolation."
	allowHostHelp   = "Add a host to this run's web allow-list (repeatable; added to LHA_WEB_ALLOW_HOSTS). A " +
		"non-empty allow-list registers the web tools (fetch_url, and web_search when configured)."
	checklistHelp = "Seed the mission with your own checklist instead of planning: a .json checklist or a " +
		".md roadmap ('- [ ] item (witness: go:TestX)'; each '## ' section depends on the previous)."
	referenceHelp = "A workspace-relative path of vendored reference material (repeatable; see 'lha vendor'), " +
		"recited to the agent every cycle."
	approveHelp = "Ask on this terminal (y/N, default reject) before any irreversible command (git push, " +
		"publish, uploads); rejected without asking when stdin is not a TTY, and after " +
		"LHA_CONSOLE_APPROVAL_TIMEOUT_S (default 1h) without an answer. Without this flag such " +
		"commands are refused."
	unsafeLocalMessage = "the 'local' sandbox runs agent commands directly on the host with no isolation " +
		"or network restriction; use sandbox='docker' (or 'e2b'), or opt in explicitly " +
		"with allow_unsafe_local=True (LHA_ALLOW_UNSAFE_LOCAL=true)"
)

// sandboxKinds is python's SANDBOX_KINDS (in its order).
var sandboxKinds = []string{"local", "docker", "e2b"}

// runFlags are the options run-local and mission share.
type runFlags struct {
	check              multi
	noDefaultChecks    bool
	sandbox            optional
	unsafeLocal        bool
	allowHost          multi
	reference          multi
	approveInteractive bool
	checklist          optional
	title              optional
	workdir            string
}

func (f *runFlags) register(fs *flag.FlagSet, defaultWorkdir string) {
	fs.Var(&f.check, "check", checkHelp)
	fs.BoolVar(&f.noDefaultChecks, "no-default-checks", false, noDefaultChecksHelp)
	fs.Var(&f.sandbox, "sandbox", sandboxHelp)
	fs.BoolVar(&f.unsafeLocal, "unsafe-local", false, unsafeLocalHelp)
	fs.Var(&f.allowHost, "allow-host", allowHostHelp)
	fs.Var(&f.reference, "reference", referenceHelp)
	fs.BoolVar(&f.approveInteractive, "approve-interactive", false, approveHelp)
	fs.Var(&f.checklist, "checklist", checklistHelp)
	fs.StringVar(&f.workdir, "workdir", defaultWorkdir, "Workspace dir (becomes a git repo).")
}

// resolveCheckCommands is python's resolve_check_commands: --check values (shlex-split) plus
// the default Python checks unless opted out.
func resolveCheckCommands(check []string, noDefaultChecks bool) ([][]string, error) {
	extra := [][]string{}
	for _, value := range check {
		argv, err := shlexSplit(value)
		if err != nil {
			return nil, fail(2, "invalid --check value: %s", err)
		}
		if len(argv) > 0 {
			extra = append(extra, argv)
		}
	}
	if noDefaultChecks && len(extra) == 0 {
		return nil, fail(2, "--no-default-checks requires at least one non-empty --check")
	}
	out := [][]string{}
	if !noDefaultChecks {
		for _, cmd := range verify.DefaultPythonCheckCommands {
			out = append(out, append([]string{}, cmd...))
		}
	}
	return append(out, extra...), nil
}

// runSettings is python's _run_settings: the CLI's sandbox / web overrides; refuses an unsafe
// local sandbox, a lethal-trifecta run and invalid web settings up front.
func runSettings(f *runFlags) (*config.Settings, error) {
	settings, err := config.Load()
	if err != nil {
		return nil, err
	}
	settings = settings.Clone()
	if f.sandbox.set {
		kind := strings.ToLower(pyfmt.PyStrip(f.sandbox.value))
		known := false
		for _, k := range sandboxKinds {
			known = known || k == kind
		}
		if !known {
			return nil, fail(2, "unknown --sandbox %s; expected one of %s",
				contracts.PyRepr(f.sandbox.value), strings.Join(sandboxKinds, ", "))
		}
		settings.Sandbox = kind
	}
	if f.unsafeLocal {
		settings.AllowUnsafeLocal = true
	}
	settings = agent.WithAllowHosts(settings, f.allowHost)
	if settings.Sandbox == "local" && !settings.AllowUnsafeLocal {
		return nil, fail(2, "%s (or pass --unsafe-local)", unsafeLocalMessage)
	}
	if err := agent.CheckRunRuleOfTwo(settings); err != nil {
		return nil, fail(2, "%s", err)
	}
	if err := validateWebTools(settings); err != nil {
		return nil, fail(2, "invalid web settings: %s", err)
	}
	return settings, nil
}

func loadChecklistFile(path string) (checklistimport.ImportedChecklist, error) {
	imported, err := checklistimport.Load(path, false)
	if err != nil {
		var importErr *checklistimport.ImportError
		if errors.As(err, &importErr) {
			return imported, fail(2, "cannot import checklist %s: %s", contracts.PyRepr(path), err)
		}
		return imported, err
	}
	return imported, nil
}

func mergeReferences(references, extra []string) []string {
	out := append([]string{}, references...)
	for _, r := range extra {
		found := false
		for _, have := range references {
			found = found || have == r
		}
		if !found {
			out = append(out, r)
		}
	}
	return out
}

// runError maps a run's error to python's _run exit codes: operator errors exit 2, a budget
// refusal exits 3, anything else is an unexpected failure (exit 1).
func runError(err error) error {
	var budget *governor.BudgetExceeded
	var unsafeLocal *execution.UnsafeSandboxError
	var e2b execution.E2BUnsupportedError
	switch {
	case errors.As(err, &budget):
		return fail(3, "%s", err)
	case errors.Is(err, safety.ErrRuleOfTwoViolation), errors.As(err, &unsafeLocal),
		errors.As(err, &e2b), errors.Is(err, agent.ErrExecutionNotLinked):
		return fail(2, "%s", err)
	}
	return err
}

func (c *cli) report(summary agent.MissionSummary) error {
	fmt.Fprintf(c.stdout, "mission %s: %s\n", summary.MissionID, summary.StoppedReason)
	fmt.Fprintf(c.stdout, "items %d/%d  cycles %d  cost $%.4f\n", summary.ItemsDone, summary.ItemsTotal,
		summary.Cycles, summary.TotalUSD)
	head := summary.HeadSHA
	if head == "" {
		head = "(none)"
	}
	fmt.Fprintf(c.stdout, "head %s\n", head)
	if !summary.Completed {
		return &exitError{code: 1}
	}
	return nil
}

func (c *cli) baseRun(f *runFlags, settings *config.Settings, checks []contracts.Check) agent.RunOptions {
	obs.ConfigureLogging(c.stderr, false)
	return agent.RunOptions{
		Workdir:            f.workdir,
		Checks:             checks,
		Settings:           settings,
		Model:              c.leadModel,
		OpenToolbox:        openToolbox,
		ApproveInteractive: f.approveInteractive,
		Preflight:          preflightRunTools,
	}
}

func (c *cli) runLocal(args []string) error {
	fs := c.newFlags("run-local", commandHelp[2].help)
	var f runFlags
	f.register(fs, ".lha/workspaces/local")
	var items multi
	fs.Var(&f.title, "title", "Mission title (default: the checklist's).")
	fs.Var(&items, "item", "A checklist item description (repeatable).")
	description := fs.String("description", "", "Mission description.")
	if err := c.parse(fs, args); err != nil {
		return err
	}
	if (len(items) > 0) == (f.checklist.value != "") {
		return fail(2, "give either --item (repeatable) or --checklist FILE")
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
	title, desc := f.title.value, *description
	references := append([]string{}, f.reference...)
	var checklist contracts.Checklist
	if f.checklist.value != "" {
		imported, err := loadChecklistFile(f.checklist.value)
		if err != nil {
			return err
		}
		checklist = imported.Checklist
		if title == "" {
			title = imported.Title
		}
		if desc == "" {
			desc = imported.Description
		}
		references = mergeReferences(references, imported.References)
	} else {
		checklist = contracts.Checklist{SchemaVersion: 1}
		for i, d := range items {
			checklist.Items = append(checklist.Items, contracts.NewChecklistItem(fmt.Sprintf("%02d", i+1), d))
		}
	}
	if title == "" {
		title = "mission"
	}
	o := c.baseRun(&f, settings, checks)
	o.Title, o.Description, o.Checklist, o.References = title, desc, checklist, references
	summary, err := agent.RunMissionLocal(c.ctx, o)
	if err != nil {
		return runError(err)
	}
	return c.report(summary)
}

func (c *cli) mission(args []string) error {
	fs := c.newFlags("mission", commandHelp[3].help)
	var f runFlags
	f.register(fs, ".lha/workspaces/mission")
	task := fs.String("task", "", "The mission / task description (the Planner decomposes it).")
	fs.Var(&f.title, "title", "Mission title (default: 'mission').")
	if err := c.parse(fs, args); err != nil {
		return err
	}
	if pyfmt.PyStrip(*task) == "" && f.checklist.value == "" {
		return fail(2, "give --task (to plan) or --checklist FILE (to import a checklist)")
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
	o := c.baseRun(&f, settings, checks)
	var summary agent.MissionSummary
	if f.checklist.value != "" {
		imported, err := loadChecklistFile(f.checklist.value)
		if err != nil {
			return err
		}
		o.Title = firstNonEmpty(f.title.value, imported.Title, "mission")
		o.Description = firstNonEmpty(*task, imported.Description)
		o.Checklist = imported.Checklist
		o.References = mergeReferences(f.reference, imported.References)
		summary, err = agent.RunMissionLocal(c.ctx, o)
		if err != nil {
			return runError(err)
		}
	} else {
		o.Title = firstNonEmpty(f.title.value, "mission")
		o.References = append([]string{}, f.reference...)
		summary, err = agent.PlanAndRunLocal(c.ctx, agent.PlanOptions{RunOptions: o, Task: *task, PlannerModel: c.leadModel})
		if err != nil {
			return runError(err)
		}
	}
	return c.report(summary)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// --- egress-proxy (hidden) -------------------------------------------------------------------

// egressProxy serves the Docker sandbox's allow-list egress proxy (python: python -m
// lha.execution.egress_proxy), configured from LHA_PROXY_ALLOW, LHA_PROXY_PORT and LHA_PROXY_BIND,
// until SIGINT/SIGTERM. It is not listed in the help: the Docker sandbox runs the proxy in its own
// container, and by default that container runs the stdlib-only Python proxy source
// (egressproxy.PythonSource); this command lets an image that ships the lha binary run the Go
// proxy instead (DockerOptions.ProxyCommand = ["lha", "egress-proxy"]).
func (c *cli) egressProxy(args []string) error {
	if err := c.parse(c.newFlags("egress-proxy", "Serve the sandbox egress allow-list proxy."), args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := func(level, message string) {
		fmt.Fprintf(c.stderr, "%s %s %s\n", time.Now().Format("2006-01-02 15:04:05,000"), level, message)
	}
	if err := egressproxy.ServeEnv(ctx, os.Getenv, logger); err != nil {
		return fail(2, "%s", err)
	}
	return nil
}

// --- decisions -------------------------------------------------------------------------------

func (c *cli) decisions(args []string) error {
	fs := c.newFlags("decisions", commandHelp[4].help)
	workdir := fs.String("workdir", ".", "The mission workspace (the git repo holding .lha/).")
	verifyChain := fs.Bool("verify", false, "Verify the hash chain; exit 1 if it does not verify.")
	limit := fs.Int("limit", 0, "Print only the newest N decisions (0 = all).")
	if err := c.parse(fs, args); err != nil {
		return err
	}
	if info, err := os.Stat(*workdir + "/" + state.AnchorDir); err != nil || !info.IsDir() {
		return fail(2, "no mission anchor at %s (expected a %s/ directory)", contracts.PyRepr(*workdir), state.AnchorDir)
	}
	anchor := state.NewGitMissionAnchor(*workdir)
	check, err := anchor.VerifyDecisions(c.ctx)
	if err != nil {
		return err
	}
	if *verifyChain {
		if !check.OK {
			fmt.Fprintf(c.stdout, "decision chain BROKEN: %s\n", check.Problem)
			return &exitError{code: 1}
		}
		chained := check.Checked - check.Legacy
		fmt.Fprintf(c.stdout, "decision chain OK: %d record(s), %d chained\n", check.Checked, chained)
		if check.Legacy > 0 {
			sealed := "NOT protected until one is chained"
			if chained > 0 {
				sealed = "sealed by the chain"
			}
			fmt.Fprintf(c.stdout, "  %d legacy (pre-chain) record(s), %s\n", check.Legacy, sealed)
		}
		return nil
	}
	if !check.OK {
		return fail(1, "%s/%s failed verification (%s); run `lha decisions --verify`",
			state.AnchorDir, state.DecisionsFile, check.Problem)
	}
	records, err := anchor.ReadDecisions(c.ctx)
	if err != nil {
		return err
	}
	shown := records
	if *limit > 0 && *limit < len(records) {
		shown = records[len(records)-*limit:]
	}
	if len(shown) == 0 {
		fmt.Fprintln(c.stdout, "(no decisions recorded)")
	}
	first := len(records) - len(shown) + 1
	for i, record := range shown {
		cycle := ""
		if record.CycleID != "" {
			cycle = " [" + record.CycleID + "]"
		}
		fmt.Fprintf(c.stdout, "%d.%s %s\n", first+i, cycle, record.Decision)
		fmt.Fprintf(c.stdout, "   why: %s\n", record.Rationale)
		if record.AlternativesRejected != "" {
			fmt.Fprintf(c.stdout, "   rejected: %s\n", record.AlternativesRejected)
		}
		if len(record.Affected) > 0 {
			fmt.Fprintf(c.stdout, "   affects: %s\n", strings.Join(record.Affected, ", "))
		}
	}
	return nil
}
