package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agents/org"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/durable"
	"github.com/calvinchengx/long-horizon-agent/go/internal/memory"
	"github.com/calvinchengx/long-horizon-agent/go/internal/ops"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
)

// The mission-store commands (python: lha.cli.main missions / costs / gates / db migrate): they
// read the same store a run writes (SQLite by default, Postgres with LHA_POSTGRES_DSN), with
// Python's output and exit codes.

const (
	missionsHelp = "List persisted missions with their status and recorded spend."
	costsHelp    = "Show a mission's persisted cost ledger: every metered model call, plus totals."
	gatesHelp    = "List recorded human gates: kind, question, options, reminders, decision, who and when."
	dbHelp       = "Database maintenance (Postgres)."
	migrateHelp  = "Apply the SQL migrations to the Postgres database at LHA_POSTGRES_DSN."
	memoryHelp   = "Tiered memory maintenance."
	objectsHelp  = "ClaimCheck object store maintenance."
	pruneHelp    = "Delete old payloads from the object store at LHA_OBJECT_STORE_ROOT."
	reembedHelp  = "Re-embed stored memory with the configured embedder."
)

// parseInterleaved parses flags and positional arguments in any order (click semantics); at
// most maxPositional positionals are accepted.
func (c *cli) parseInterleaved(fs *flag.FlagSet, args []string, maxPositional int) ([]string, error) {
	positional := []string{}
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &exitError{code: 2}
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) > maxPositional {
		return nil, &exitError{code: 2, message: fmt.Sprintf("Error: Got unexpected extra argument (%s)", positional[maxPositional])}
	}
	return positional, nil
}

// rangeError is click's IntRange usage error: x>=lo, or lo<=x<=hi when hasHi. usage is the
// command's usage line after "lha " (e.g. "mission-snooze [OPTIONS] MISSION_ID"); a bare command
// name means "<command> [OPTIONS]".
func rangeError(usage, option string, value, lo, hi int, hasHi bool) error {
	command, _, _ := strings.Cut(usage, " ")
	if command == usage {
		usage += " [OPTIONS]"
	}
	bound := fmt.Sprintf("x>=%d", lo)
	if hasHi {
		bound = fmt.Sprintf("%d<=x<=%d", lo, hi)
	}
	return &exitError{code: 2, message: fmt.Sprintf("Usage: lha %s\nTry 'lha %s --help' for help.\n\n"+
		"Error: Invalid value for '--%s': %d is not in the range %s.", usage, command, option, value, bound)}
}

// withStore opens the configured store (no workdir) for a read command.
func (c *cli) withStore(fn func(persistence.Store) error) error {
	settings, err := config.Load()
	if err != nil {
		return err
	}
	store, err := persistence.OpenStore(c.ctx, settings, "")
	if err != nil {
		var unavailable *persistence.StoreUnavailableError
		if errors.As(err, &unavailable) {
			return fail(2, "%s", err)
		}
		return err
	}
	defer store.Close()
	if reason := store.DegradedReason(); reason != "" {
		fmt.Fprintf(c.stderr, "warning: %s; reading SQLite instead\n", reason)
	}
	return fn(store)
}

func usd(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("$%.4f", *value)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (c *cli) missions(args []string) error {
	fs := c.newFlags("missions", missionsHelp)
	limit := fs.Int("limit", 20, "How many missions (most recently updated first).")
	if err := c.parse(fs, args); err != nil {
		return err
	}
	if *limit < 1 {
		return rangeError("missions", "limit", *limit, 1, 0, false)
	}
	return c.withStore(func(store persistence.Store) error {
		rows, err := store.ListMissions(c.ctx, *limit)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			fmt.Fprintln(c.stdout, "no missions recorded")
			return nil
		}
		for _, row := range rows {
			cost, err := store.CostSummary(c.ctx, row.MissionID)
			if err != nil {
				return err
			}
			unknown := ""
			if cost.UnknownCostCalls > 0 {
				unknown = fmt.Sprintf(" (+%d unknown-cost)", cost.UnknownCostCalls)
			}
			known := cost.KnownUSD
			fmt.Fprintf(c.stdout, "%s  %-16s %s%s  calls %d  head %s  updated %s  %s\n", row.MissionID, row.Status,
				usd(&known), unknown, cost.Calls, pyfmt.Head(orDash(row.HeadSHA), 12), pyfmt.Head(row.UpdatedAt, 19), row.Title)
		}
		return nil
	})
}

func (c *cli) costs(args []string) error {
	fs := c.newFlags("costs", costsHelp)
	limit := fs.Int("limit", 50, "Show the most recent N calls (0 = summary only).")
	positional, err := c.parseInterleaved(fs, args, 1)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return &exitError{code: 2, message: "Usage: lha costs [OPTIONS] MISSION_ID\nTry 'lha costs --help' for help.\n\n" +
			"Error: Missing argument 'MISSION_ID'."}
	}
	if *limit < 0 {
		return rangeError("costs", "limit", *limit, 0, 0, false)
	}
	missionID := positional[0]
	return c.withStore(func(store persistence.Store) error {
		rows := []persistence.CostRow{}
		if *limit > 0 {
			var err error
			if rows, err = store.ListCosts(c.ctx, missionID, *limit); err != nil {
				return err
			}
		}
		summary, err := store.CostSummary(c.ctx, missionID)
		if err != nil {
			return err
		}
		if summary.Calls == 0 {
			return fail(1, "no cost ledger rows for mission %s", missionID)
		}
		for _, row := range rows {
			fmt.Fprintf(c.stdout, "%s  %-12s %-11s %-24s in %7d  out %6d  %s\n", pyfmt.Head(row.TS, 19), row.CycleID,
				orDash(row.Role), row.Model, row.InputTokens, row.OutputTokens, usd(row.USD))
		}
		fmt.Fprintln(c.stdout, ops.FormatCostTotal(summary))
		return nil
	})
}

// FormatGateRow is the human-readable lines for one recorded gate (python: format_gate_row).
func FormatGateRow(row persistence.GateRow) []string { return ops.FormatGateRow(row) }

func isPySpace(r rune) bool {
	return r == ' ' || (r >= '\t' && r <= '\r') || (r >= 0x1c && r <= 0x1f) || r == 0x85 || r == 0xa0 ||
		r == 0x1680 || (r >= 0x2000 && r <= 0x200a) || r == 0x2028 || r == 0x2029 || r == 0x202f ||
		r == 0x205f || r == 0x3000
}

func (c *cli) gates(args []string) error {
	fs := c.newFlags("gates", gatesHelp)
	limit := fs.Int("limit", 50, "How many gates (most recently opened first).")
	positional, err := c.parseInterleaved(fs, args, 1)
	if err != nil {
		return err
	}
	if *limit < 1 {
		return rangeError("gates", "limit", *limit, 1, 0, false)
	}
	missionID := ""
	if len(positional) > 0 {
		missionID = positional[0]
	}
	return c.withStore(func(store persistence.Store) error {
		rows, err := store.ListGates(c.ctx, missionID, *limit)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			suffix := ""
			if missionID != "" {
				suffix = " for mission " + missionID
			}
			fmt.Fprintln(c.stdout, "no gates recorded"+suffix)
			return nil
		}
		for _, row := range rows {
			for _, line := range FormatGateRow(row) {
				fmt.Fprintln(c.stdout, line)
			}
		}
		return nil
	})
}

func (c *cli) db(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		w := c.stdout
		fmt.Fprintf(w, "Usage: lha db [OPTIONS] COMMAND [ARGS]...\n\n%s\n\nCommands:\n  %-12s %s\n", dbHelp, "migrate", migrateHelp)
		if len(args) == 0 {
			return &exitError{code: 2} // typer's no_args_is_help
		}
		return nil
	}
	if args[0] != "migrate" {
		return &exitError{code: 2, message: "Usage: lha db [OPTIONS] COMMAND [ARGS]...\nTry 'lha db --help' for help.\n\n" +
			"Error: No such command " + pyQuote(args[0]) + "."}
	}
	fs := c.newFlags("db migrate", migrateHelp)
	var dir optional
	fs.Var(&dir, "migrations-dir", "Directory of *.sql migrations (default: db/migrations here or in the parent dir).")
	if err := c.parse(fs, args[1:]); err != nil {
		return err
	}
	migrationsDir := dir.value
	if !dir.set {
		for _, candidate := range []string{"db/migrations", "../db/migrations"} {
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				migrationsDir = candidate
				break
			}
		}
		if migrationsDir == "" {
			return fail(2, "cannot find db/migrations here or in the parent dir; pass --migrations-dir")
		}
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	dsn := settings.PostgresDSN.Value()
	if dsn == "" {
		return fail(2, "LHA_POSTGRES_DSN is not set; export it (or put it in .env) to run migrations")
	}
	applied, err := applyMigrations(c, dsn, migrationsDir)
	if err != nil {
		return err
	}
	quoted := make([]string, len(applied))
	for i, v := range applied {
		quoted[i] = "'" + v + "'"
	}
	fmt.Fprintf(c.stdout, "migrations applied: [%s]\n", strings.Join(quoted, ", "))
	return nil
}

// applyMigrations is persistence.ApplyMigrations (tests replace it).
var applyMigrations = func(c *cli, dsn, dir string) ([]string, error) {
	return persistence.ApplyMigrations(c.ctx, dsn, dir)
}

// memoryCmd is `lha memory reembed [MISSION_ID] [--dry-run]` (python: lha.cli.main reembed).
// Rows stored while the dense channel was down, or embedded by another model or model version,
// are invisible to dense recall until re-embedded; missions re-embed up to
// memory.ReembedPerRecall of them per cycle on their own, and this does them all now.
func (c *cli) memoryCmd(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(c.stdout, "Usage: lha memory [OPTIONS] COMMAND [ARGS]...\n\n%s\n\nCommands:\n  %-12s %s\n", memoryHelp, "reembed", reembedHelp)
		if len(args) == 0 {
			return &exitError{code: 2} // typer's no_args_is_help
		}
		return nil
	}
	if args[0] != "reembed" {
		return &exitError{code: 2, message: "Usage: lha memory [OPTIONS] COMMAND [ARGS]...\nTry 'lha memory --help' for help.\n\n" +
			"Error: No such command " + pyQuote(args[0]) + "."}
	}
	fs := c.newFlags("memory reembed", reembedHelp)
	dryRun := fs.Bool("dry-run", false, "Only count the rows to re-embed.")
	positional, err := c.parseInterleaved(fs, args[1:], 1)
	if err != nil {
		return err
	}
	missionID := ""
	if len(positional) > 0 {
		missionID = positional[0]
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	return c.withStore(func(store persistence.Store) error {
		mem := memory.OpenMissionMemory(c.ctx, settings, store, ".", missionID, memory.OpenOptions{})
		if mem == nil {
			return fail(2, "memory is disabled (LHA_MEMORY_ENABLED=false)")
		}
		defer mem.Close()
		embedder := mem.Embedder()
		if embedder == nil {
			return fail(2, "no embedder to re-embed with: %s", mem.Mode().Reason)
		}
		var counts map[string]int
		verb := "re-embedded"
		if *dryRun {
			counts, err = store.CountStaleMemory(c.ctx, missionID, embedder.Name(), embedder.Version())
			verb = "to re-embed"
		} else {
			counts, err = mem.Reembed(c.ctx, missionID, 0, "")
		}
		if err != nil {
			return err
		}
		using := fmt.Sprintf("%s (%s)", embedder.Name(), embedder.Version())
		if len(counts) == 0 {
			fmt.Fprintf(c.stdout, "nothing to re-embed for %s\n", using)
			return nil
		}
		owners := make([]string, 0, len(counts))
		total := 0
		for owner, n := range counts {
			owners = append(owners, owner)
			total += n
		}
		sort.Strings(owners)
		for _, owner := range owners {
			fmt.Fprintf(c.stdout, "%s  %d rows\n", owner, counts[owner])
		}
		fmt.Fprintf(c.stdout, "%d rows %s with %s\n", total, verb, using)
		return nil
	})
}

// objectsCmd is `lha objects prune --older-than-days N [--dry-run]` (python: lha.cli.main prune).
func (c *cli) objectsCmd(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(c.stdout, "Usage: lha objects [OPTIONS] COMMAND [ARGS]...\n\n%s\n\nCommands:\n  %-12s %s\n", objectsHelp, "prune", pruneHelp)
		if len(args) == 0 {
			return &exitError{code: 2} // typer's no_args_is_help
		}
		return nil
	}
	if args[0] != "prune" {
		return &exitError{code: 2, message: "Usage: lha objects [OPTIONS] COMMAND [ARGS]...\nTry 'lha objects --help' for help.\n\n" +
			"Error: No such command " + pyQuote(args[0]) + "."}
	}
	fs := c.newFlags("objects prune", pruneHelp)
	days := fs.Int("older-than-days", 0, "Delete objects not modified for this many days.")
	dryRun := fs.Bool("dry-run", false, "Only count and size them.")
	if err := c.parse(fs, args[1:]); err != nil {
		return err
	}
	if *days < 1 {
		if !isFlagSet(fs, "older-than-days") {
			return &exitError{code: 2, message: "Usage: lha objects prune [OPTIONS]\nTry 'lha objects prune --help' for help.\n\n" +
				"Error: Missing option '--older-than-days'."}
		}
		return rangeError("objects prune", "older-than-days", *days, 1, 0, false)
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	result, err := durable.PruneObjects(config.ResolvePath(config.ExpandUser(settings.ObjectStoreRoot)), *days, *dryRun)
	if err != nil {
		return err
	}
	verb := "deleted"
	if *dryRun {
		verb = "to delete"
	}
	fmt.Fprintf(c.stdout, "%d objects %s (%.1f MiB), %d kept\n", result.Count, verb, float64(result.Bytes)/(1<<20), result.Kept)
	return nil
}

func isFlagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

const labelsHelp = "Labels for fitting System One thresholds."

const exportHelp = "Export the mission's judgments as JSON Lines labels: one object per human gate answer,\n" +
	"tool approval, verifier verdict and review verdict, secrets redacted.\n\n" +
	"Reads the anchor's committed events at WORKDIR and the mission's closed gates from the\n" +
	"mission store. Without an anchor, MISSION_ID is required and only the gates are exported."

// labelsCmd is python's `lha labels export` (lha.systemone.labels).
func (c *cli) labelsCmd(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(c.stdout, "Usage: lha labels [OPTIONS] COMMAND [ARGS]...\n\n%s\n\nCommands:\n  %-12s %s\n", labelsHelp, "export", exportHelp)
		if len(args) == 0 {
			return &exitError{code: 2} // typer's no_args_is_help
		}
		return nil
	}
	if args[0] != "export" {
		return &exitError{code: 2, message: "Usage: lha labels [OPTIONS] COMMAND [ARGS]...\nTry 'lha labels --help' for help.\n\n" +
			"Error: No such command " + pyQuote(args[0]) + "."}
	}
	fs := c.newFlags("labels export", exportHelp)
	workdir := fs.String("workdir", ".", "The mission workspace (the git repo holding .lha/).")
	out := fs.String("out", "-", "Where to write the JSON Lines ('-' = stdout).")
	diffs := fs.Bool("diffs", false, "Include each reviewed diff (git diff base..head, capped).")
	positional, err := c.parseInterleaved(fs, args[1:], 1)
	if err != nil {
		return err
	}
	missionID := ""
	if len(positional) > 0 {
		missionID = positional[0]
	}
	info, statErr := os.Stat(filepath.Join(*workdir, state.AnchorDir))
	hasAnchor := statErr == nil && info.IsDir()
	if !hasAnchor && missionID == "" {
		return fail(2, "no mission anchor at %s (expected a %s/ directory); pass MISSION_ID to export a mission's gates alone",
			contracts.PyRepr(*workdir), state.AnchorDir)
	}
	events := []contracts.EventRecord{}
	if hasAnchor {
		if events, err = state.NewGitMissionAnchor(*workdir).ReadEvents(c.ctx); err != nil {
			return err
		}
	}
	mission := missionID
	if mission == "" {
		mission = systemone.MissionIDOf(events)
	}
	gates := []persistence.GateRow{}
	if mission == "" {
		fmt.Fprintln(c.stderr, "warning: the anchor names no mission; gates are not exported")
	} else if err := c.withStore(func(store persistence.Store) error {
		gates, err = store.ListGates(c.ctx, mission, 100_000)
		return err
	}); err != nil {
		return err
	}
	var supplier func(base, head string) string
	if *diffs {
		supplier = func(base, head string) string { return org.DiffSince(c.ctx, *workdir, base, head) }
	}
	rows := systemone.LabelRows(events, gates, mission, supplier)
	text := systemone.ToJSONL(rows)
	if *out == "-" {
		fmt.Fprint(c.stdout, text)
	} else if err := os.WriteFile(*out, []byte(text), 0o644); err != nil {
		return err
	}
	counts := []string{}
	for _, source := range systemone.Sources {
		n := 0
		for _, row := range rows {
			if row.Source == source {
				n++
			}
		}
		counts = append(counts, fmt.Sprintf("%d %s", n, source))
	}
	fmt.Fprintf(c.stderr, "%d labels (%s)\n", len(rows), strings.Join(counts, ", "))
	return nil
}

const evalHelp = "Gold evaluation sets: check them, and score a judge against them."

const evalCheckHelp = "Check gold files: every line a schema-1 label row with a 'gold' judgment inside its\n" +
	"source's vocabulary, no judgment recorded twice. Prints the row counts; exit 2 on the first\n" +
	"bad file."

const evalRunHelp = "Score a judge against gold files: per source, agreement with the gold labels and the\n" +
	"precision and recall of its refusing label, then every disagreement."

// evalCmd is python's lha eval (lha.systemone.gold): check and run.
func (c *cli) evalCmd(args []string) error {
	usage := fmt.Sprintf("Usage: lha eval [OPTIONS] COMMAND [ARGS]...\n\n%s\n\nCommands:\n  %-8s %s\n  %-8s %s\n",
		evalHelp, "check", evalCheckHelp, "run", evalRunHelp)
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(c.stdout, usage)
		if len(args) == 0 {
			return &exitError{code: 2} // typer's no_args_is_help
		}
		return nil
	}
	switch args[0] {
	case "check":
		fs := c.newFlags("eval check", evalCheckHelp)
		files, err := c.parseInterleaved(fs, args[1:], 1<<30)
		if err != nil {
			return err
		}
		rows, err := c.loadGold(files)
		if err != nil {
			return err
		}
		plural := "s"
		if len(files) == 1 {
			plural = ""
		}
		fmt.Fprintf(c.stdout, "%s in %d file%s\n", systemone.CountBySource(rows), len(files), plural)
		return nil
	case "run":
		fs := c.newFlags("eval run", evalRunHelp)
		judgeName := fs.String("judge", "recorded", "recorded (the label the mission recorded) or screen (the pre-review screen re-run on each review row's diff).")
		files, err := c.parseInterleaved(fs, args[1:], 1<<30)
		if err != nil {
			return err
		}
		judge, err := systemone.JudgeNamed(*judgeName)
		if err != nil {
			return fail(2, "unknown --judge %s; expected %s", contracts.PyRepr(*judgeName), strings.Join(systemone.Judges, ", "))
		}
		rows, err := c.loadGold(files)
		if err != nil {
			return err
		}
		fmt.Fprint(c.stdout, systemone.RenderScorecards(systemone.ScoreGold(rows, judge), *judgeName))
		return nil
	}
	return &exitError{code: 2, message: "Usage: lha eval [OPTIONS] COMMAND [ARGS]...\nTry 'lha eval --help' for help.\n\n" +
		"Error: No such command " + pyQuote(args[0]) + "."}
}

// loadGold is python's _load_gold: every gold row in files; a bad line or set exits 2.
func (c *cli) loadGold(files []string) ([]systemone.GoldRow, error) {
	if len(files) == 0 {
		return nil, &exitError{code: 2, message: "Usage: lha eval check [OPTIONS] FILES...\nTry 'lha eval check --help' for help.\n\nError: Missing argument 'FILES...'."}
	}
	rows := []systemone.GoldRow{}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, fail(2, "cannot read %s: %v", contracts.PyRepr(name), err)
		}
		parsed, err := systemone.ParseGold(string(data), name)
		if err != nil {
			return nil, fail(2, "%s", err.Error())
		}
		rows = append(rows, parsed...)
	}
	if errs := systemone.CheckGold(rows); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(c.stderr, "error: "+e)
		}
		return nil, &exitError{code: 2}
	}
	return rows, nil
}

const missionReportHelp = "One page about a mission: items and their status, the cycles' verdicts, reviews and\n" +
	"screens, the human gates, the spend and the commits, from the anchor and the mission store.\n\n" +
	"Reads the anchor at WORKDIR and, for the mission it names (or MISSION_ID), the store's\n" +
	"mission row, gates and cost ledger. Without an anchor, MISSION_ID is required."

// missionReport is python's mission-report (lha.ops.report).
func (c *cli) missionReport(args []string) error {
	fs := c.newFlags("mission-report", missionReportHelp)
	workdir := fs.String("workdir", ".", "The mission workspace (the git repo holding .lha/).")
	positional, err := c.parseInterleaved(fs, args, 1)
	if err != nil {
		return err
	}
	missionID := ""
	if len(positional) > 0 {
		missionID = positional[0]
	}
	info, statErr := os.Stat(filepath.Join(*workdir, state.AnchorDir))
	hasAnchor := statErr == nil && info.IsDir()
	if !hasAnchor && missionID == "" {
		return fail(2, "no mission anchor at %s (expected a %s/ directory); pass MISSION_ID to report from the store alone",
			contracts.PyRepr(*workdir), state.AnchorDir)
	}
	inp := ops.ReportInput{Checklist: contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{}}, Events: []contracts.EventRecord{}}
	if hasAnchor {
		anchor := state.NewGitMissionAnchor(*workdir)
		if inp.Spec, err = anchor.ReadMission(c.ctx); err != nil {
			return err
		}
		if inp.Checklist, err = anchor.ReadChecklist(c.ctx); err != nil {
			return err
		}
		if inp.Events, err = anchor.ReadEvents(c.ctx); err != nil {
			return err
		}
		if inp.HeadSHA, err = state.HeadSHA(c.ctx, *workdir); err != nil {
			return err
		}
		count, err := state.RunGit(c.ctx, *workdir, "rev-list", "--count", "HEAD")
		if err != nil {
			return err
		}
		fmt.Sscanf(strings.TrimSpace(count), "%d", &inp.Commits)
	}
	inp.MissionID = missionID
	if inp.MissionID == "" {
		inp.MissionID = systemone.MissionIDOf(inp.Events)
	}
	if inp.MissionID != "" {
		if err := c.withStore(func(store persistence.Store) error {
			if inp.Row, err = store.GetMission(c.ctx, inp.MissionID); err != nil {
				return err
			}
			if inp.Gates, err = store.ListGates(c.ctx, inp.MissionID, 100_000); err != nil {
				return err
			}
			sort.SliceStable(inp.Gates, func(i, j int) bool {
				if inp.Gates[i].OpenedAt != inp.Gates[j].OpenedAt {
					return inp.Gates[i].OpenedAt < inp.Gates[j].OpenedAt
				}
				return inp.Gates[i].GateID < inp.Gates[j].GateID
			})
			cost, err := store.CostSummary(c.ctx, inp.MissionID)
			if err != nil {
				return err
			}
			inp.Cost = &cost
			return nil
		}); err != nil {
			return err
		}
	}
	fmt.Fprint(c.stdout, ops.RenderReport(inp))
	return nil
}
