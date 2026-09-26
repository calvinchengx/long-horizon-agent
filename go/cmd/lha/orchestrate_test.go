package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// `lha orchestrate`: the refusals, --checklist, the planned ownership reaching the anchor, and —
// when `uv` is on PATH — the Python implementation side by side: the same inputs give the same
// commits and anchor, and a mission started by one implementation is resumed by the other.

func runOrg(t *testing.T, planner contracts.ModelProvider, models map[string]contracts.ModelProvider, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	c := &cli{stdout: &out, stderr: &errOut, ctx: context.Background(), leadModel: planner, orgModels: models}
	code := c.run(args)
	return result{out.String(), errOut.String(), code}
}

var orgBase = []string{"--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", "true"}

func TestOrchestrateRefusalsAndResume(t *testing.T) {
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub")
	work := filepath.Join(dir, "w")
	missing := runOrg(t, nil, nil, append([]string{"orchestrate", "--resume", "--workdir", work}, orgBase...)...)
	if missing.code != 2 || missing.stderr != "error: --resume: no mission anchor in '"+work+"' (start one without --resume)\n" {
		t.Fatalf("%+v", missing)
	}
	noTask := runOrg(t, nil, nil, append([]string{"orchestrate", "--workdir", work}, orgBase...)...)
	if noTask.code != 2 || !strings.Contains(noTask.stderr, "--task") {
		t.Fatalf("%+v", noTask)
	}
	if _, err := state.NewGitMissionAnchor(work).Initialize(context.Background(), "T", "D",
		contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{contracts.NewChecklistItem("01", "x")}}); err != nil {
		t.Fatal(err)
	}
	again := runOrg(t, nil, nil, append([]string{"orchestrate", "--task", "t", "--workdir", work}, orgBase...)...)
	if again.code != 2 || again.stderr != "error: '"+work+"' already holds a mission: pass --resume to continue it, or use a new "+
		"--workdir (starting over would replace its checklist)\n" {
		t.Fatalf("%+v", again)
	}
	both := runOrg(t, nil, nil, append([]string{"orchestrate", "--resume", "--checklist", "x.md", "--workdir", work}, orgBase...)...)
	if both.code != 2 || !strings.Contains(both.stderr, "--checklist cannot be combined with --resume") {
		t.Fatalf("%+v", both)
	}
	done := contracts.TurnResult{Text: `{"done": true, "summary": "done"}`}
	approve := contracts.TurnResult{Text: `{"done": true, "verdict": "approve", "blocking_issues": []}`}
	models := map[string]contracts.ModelProvider{"lead": model.NewStub([]contracts.TurnResult{done}),
		"researcher": model.NewStub([]contracts.TurnResult{done}), "reviewer": model.NewStub([]contracts.TurnResult{approve})}
	resumed := runOrg(t, nil, models, append([]string{"orchestrate", "--resume", "--workdir", work}, orgBase...)...)
	m := reportRE.FindStringSubmatch(resumed.stdout)
	if resumed.code != 0 || m == nil || m[2] != "complete" || m[3] != "1" {
		t.Fatalf("%+v", resumed)
	}
}

func TestOrchestrateWithAChecklistFile(t *testing.T) {
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub")
	roadmap := filepath.Join(dir, "roadmap.md")
	if err := os.WriteFile(roadmap, []byte("# Greeter\n\n## Build\n\n- [ ] say hello\n- [ ] say bye\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := contracts.TurnResult{Text: `{"done": true, "summary": "done"}`}
	approve := contracts.TurnResult{Text: `{"done": true, "verdict": "approve", "blocking_issues": []}`}
	models := map[string]contracts.ModelProvider{"lead": model.NewStub([]contracts.TurnResult{done}),
		"researcher": model.NewStub([]contracts.TurnResult{done}), "reviewer": model.NewStub([]contracts.TurnResult{approve})}
	work := filepath.Join(dir, "w")
	r := runOrg(t, failingPlanner{}, models, append([]string{"orchestrate", "--checklist", roadmap, "--workdir", work}, orgBase...)...)
	m := reportRE.FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil || m[2] != "complete" || m[3] != "2" || m[4] != "2" {
		t.Fatalf("%+v", r)
	}
	var mission contracts.MissionSpec
	if err := json.Unmarshal([]byte(git(t, work, "show", "HEAD:.lha/mission.json")), &mission); err != nil || mission.Title != "Greeter" {
		t.Fatalf("%+v %v", mission, err)
	}
	if _, err := os.Stat(filepath.Join(work, ".lha", "ownership.json")); !os.IsNotExist(err) {
		t.Fatal("an imported checklist has no ownership map")
	}
}

// failingPlanner proves --checklist never plans.
type failingPlanner struct{}

func (failingPlanner) Name() string { return "fake:no-planning" }
func (failingPlanner) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	return contracts.TurnResult{}, errors.New("planned")
}
func (failingPlanner) EstimateCostUSD(contracts.Usage) (float64, error) { return 0, nil }

// --- the scripted parallel mission (both implementations) ---------------------------------------

var (
	orgWriteSet = regexp.MustCompile(`Your write-set \(the ONLY files you may create or modify\): (.*)`)
	orgItemID   = regexp.MustCompile(`Checklist item \[([\w.]+)\]`)
)

// implementerFake is testdata/scripted_orchestrate.py's _Implementers: record a decision, write
// the write-set, say done.
type implementerFake struct{ *model.StubModel }

func (implementerFake) Complete(_ context.Context, messages []contracts.ModelMessage, _ []map[string]any, _ int) (contracts.TurnResult, error) {
	objective := messages[1].Content
	item, ws := orgItemID.FindStringSubmatch(objective), orgWriteSet.FindStringSubmatch(objective)
	if item == nil || ws == nil {
		return contracts.TurnResult{}, errors.New("not an implementer objective")
	}
	act := func(tool string, args map[string]any) string {
		data, _ := json.Marshal(map[string]any{"tool": tool, "arguments": args})
		return string(data)
	}
	actions := []string{act("record_decision", map[string]any{"decision": "item " + item[1] + " layout", "rationale": "r"})}
	for _, p := range strings.Split(ws[1], ",") {
		if p = strings.TrimSpace(p); p != "" {
			actions = append(actions, act("write_file", map[string]any{"path": p, "content": "x\n"}))
		}
	}
	turn := 0
	for _, m := range messages {
		if m.Role == "assistant" {
			turn++
		}
	}
	if turn < len(actions) {
		return contracts.TurnResult{Text: actions[turn]}, nil
	}
	return contracts.TurnResult{Text: `{"done": true, "summary": "done"}`}, nil
}

const orgPlan = `[{"description": "write a", "files": ["a.py"]}, {"description": "write b", "files": ["b.py"]}, ` +
	`{"description": "wire them", "depends_on": [1, 2], "files": ["pyproject.toml"]}]`

// orgWorkspace is a run's commits, tracked files and committed anchor (JSON files compared as
// data; events with mission ids masked and durations zeroed, sorted when sortEvents: implementers
// of one wave post to the board in whichever order they finish).
func orgWorkspace(t *testing.T, workdir string, sortEvents bool) workspace {
	t.Helper()
	w := workspace{
		Commits: strings.Split(git(t, workdir, "log", "--format=%s%n%b"), "\n"),
		Files:   strings.Split(git(t, workdir, "ls-files"), "\n"),
		Anchor:  map[string]string{},
	}
	for _, f := range []string{"checklist.json", "decisions.ndjson", "events.ndjson", "mission.json", "progress.md", "ownership.json"} {
		body := ""
		if ok, _ := state.ExistsAtHead(context.Background(), workdir, ".lha/"+f); ok {
			body = git(t, workdir, "show", "HEAD:.lha/"+f)
		}
		switch {
		case f == "events.ndjson":
			lines := strings.Split(missionIDRE.ReplaceAllString(normalizeNDJSON(t, body), "mission_X"), "\n")
			if sortEvents {
				sort.Strings(lines)
			}
			body = strings.Join(lines, "\n")
		case strings.HasSuffix(f, ".json") && body != "":
			var v any
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(v)
			body = string(data)
		}
		w.Anchor[f] = body
	}
	return w
}

func compareOrgWorkspaces(t *testing.T, goW, pyW workspace) {
	t.Helper()
	if strings.Join(goW.Commits, "\n") != strings.Join(pyW.Commits, "\n") {
		t.Errorf("commits differ:\ngo: %q\npy: %q", goW.Commits, pyW.Commits)
	}
	if strings.Join(goW.Files, "\n") != strings.Join(pyW.Files, "\n") {
		t.Errorf("tracked files differ:\ngo: %q\npy: %q", goW.Files, pyW.Files)
	}
	for f, body := range goW.Anchor {
		if body != pyW.Anchor[f] {
			t.Errorf(".lha/%s differs:\ngo: %s\npy: %s", f, body, pyW.Anchor[f])
		}
	}
}

// TestE2EOrchestrateScriptedParallelMatchesPython: the Planner assigns two items disjoint files
// and serializes a third behind them (a shared file); the two run as a parallel wave in their
// own worktrees, merge, then the Lead does the third. Same commits and anchor in Python.
func TestE2EOrchestrateScriptedParallelMatchesPython(t *testing.T) {
	env := []string{"LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_MODEL_BACKEND=stub"}
	dir := cleanEnv(t, env...)
	workdir := filepath.Join(dir, "ws")
	done := contracts.TurnResult{Text: `{"done": true, "summary": "done"}`}
	approve := contracts.TurnResult{Text: `{"done": true, "verdict": "approve", "blocking_issues": []}`}
	models := map[string]contracts.ModelProvider{
		"implementer": implementerFake{model.NewStubNamed("implementers", nil)},
		"lead":        model.NewStub([]contracts.TurnResult{done}),
		"researcher":  model.NewStub([]contracts.TurnResult{done}),
		"reviewer":    model.NewStub([]contracts.TurnResult{approve}),
	}
	planner := model.NewStub([]contracts.TurnResult{{Text: orgPlan}})
	goRun := runOrg(t, planner, models, "orchestrate", "--title", "Pair", "--task", "two files then wire",
		"--no-default-checks", "--check", "true", "--workdir", workdir)
	if goRun.code != 0 || !strings.Contains(goRun.stdout, ": complete\nitems 3/3  cycles 3") {
		t.Fatalf("go: %+v", goRun)
	}
	log := git(t, workdir, "log", "--format=%s")
	if !strings.Contains(log, "lha: complete 01 (write a) [merged lha/implementer-01/c1]") ||
		!strings.Contains(log, "lha: complete 02 (write b) [merged lha/implementer-02/c2]") {
		t.Fatal(log)
	}
	if owners := git(t, workdir, "show", "HEAD~3:.lha/ownership.json"); !strings.Contains(owners, `"a.py": "implementer-01"`) {
		t.Fatalf("the planned ownership never reached the anchor: %s", owners)
	}
	goWS := orgWorkspace(t, workdir, true)
	if !pythonAvailable(t) {
		return
	}
	pyDir := t.TempDir()
	spec, _ := json.Marshal(map[string]any{
		"workdir": filepath.Join(pyDir, "ws"), "title": "Pair", "task": "two files then wire",
		"checks": [][]string{{"true"}}, "plan": orgPlan,
	})
	specPath := filepath.Join(pyDir, "spec.json")
	if err := os.WriteFile(specPath, spec, 0o644); err != nil {
		t.Fatal(err)
	}
	pyRun := runProcess(t, pyDir, pythonEnv(pyDir, processEnv(env...)), "uv", "run", "--quiet", "--project",
		filepath.Join(repoRoot, "python"), "python", filepath.Join(repoRoot, "go", "cmd", "lha", "testdata", "scripted_orchestrate.py"), specPath)
	if pyRun.code != goRun.code {
		t.Fatalf("exit codes differ: go %d, python %d\n%s%s", goRun.code, pyRun.code, pyRun.stdout, pyRun.stderr)
	}
	if g, p := report(t, goRun.stdout), report(t, pyRun.stdout); g != p {
		t.Errorf("reports differ:\ngo: %s\npy: %s", g, p)
	}
	compareOrgWorkspaces(t, goWS, orgWorkspace(t, filepath.Join(pyDir, "ws"), true))
}

// --- the binary against the Python CLI -----------------------------------------------------------

var stubOrgEnv = []string{"LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_MODEL_BACKEND=stub", "LHA_MAX_TURNS_PER_CYCLE=2"}

var orgArgs = []string{"--title", "Greeter", "--task", "say hello", "--no-default-checks", "--check", "true", "--workdir", "ws"}

// TestE2EOrchestrateBinaryMatchesPython runs `lha orchestrate` with the stub model (echo mode:
// every Reviewer verdict is unparseable, so the item is reopened until the loop detector stops
// the mission) on the real local sandbox. Every researcher, Lead and Reviewer prompt feeds the
// stub's digest into the committed board and reflections, so matching anchors mean
// byte-identical prompts.
func TestE2EOrchestrateBinaryMatchesPython(t *testing.T) {
	bin := lhaBinary(t)
	env := processEnv(stubOrgEnv...)
	goDir := t.TempDir()
	goRun := runProcess(t, goDir, env, bin, append([]string{"orchestrate"}, orgArgs...)...)
	if goRun.code != 1 || !strings.Contains(goRun.stdout, ": loop on item 01 (review keeps blocking)\n") {
		t.Fatalf("go: %+v", goRun)
	}
	goWS := orgWorkspace(t, filepath.Join(goDir, "ws"), false)
	if !pythonAvailable(t) {
		return
	}
	pyDir := t.TempDir()
	pyRun := runPythonLHA(t, pyDir, env, append([]string{"orchestrate"}, orgArgs...)...)
	if pyRun.code != goRun.code {
		t.Fatalf("exit codes differ: go %d, python %d\n%s%s", goRun.code, pyRun.code, pyRun.stdout, pyRun.stderr)
	}
	if g, p := report(t, goRun.stdout), report(t, pyRun.stdout); g != p {
		t.Errorf("reports differ:\ngo: %s\npy: %s", g, p)
	}
	compareOrgWorkspaces(t, goWS, orgWorkspace(t, filepath.Join(pyDir, "ws"), false))
}

type lhaRunner func(t *testing.T, dir string, env []string, args ...string) result

func goLHA(bin string) lhaRunner {
	return func(t *testing.T, dir string, env []string, args ...string) result {
		return runProcess(t, dir, env, bin, args...)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	if out, err := exec.Command("cp", "-R", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v\n%s", err, out)
	}
}

// TestE2EOrchestrateCrossImplementationResume: a mission started by one implementation and
// interrupted (max cycles, plus uncommitted residue) is resumed by the other; the result is what
// the starting implementation's own --resume produces, and the mission id is kept.
func TestE2EOrchestrateCrossImplementationResume(t *testing.T) {
	bin := lhaBinary(t)
	if !pythonAvailable(t) {
		t.Skip("the cross-implementation resume needs the Python implementation (uv)")
	}
	pyLHA := func(t *testing.T, dir string, env []string, args ...string) result {
		return runPythonLHA(t, dir, env, args...)
	}
	for _, c := range []struct {
		name          string
		start, resume lhaRunner
		other         lhaRunner
	}{
		{"python-then-go", pyLHA, goLHA(bin), pyLHA},
		{"go-then-python", goLHA(bin), pyLHA, goLHA(bin)},
	} {
		t.Run(c.name, func(t *testing.T) {
			startDir := t.TempDir()
			first := c.start(t, startDir, processEnv(append(stubOrgEnv, "LHA_MAX_CYCLES=2")...), append([]string{"orchestrate"}, orgArgs...)...)
			if first.code != 1 || !strings.Contains(first.stdout, ": max_cycles\n") {
				t.Fatalf("start: %+v", first)
			}
			missionID := missionIDRE.FindString(first.stdout)
			if err := os.WriteFile(filepath.Join(startDir, "ws", "residue.txt"), []byte("left by a crash\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			crossDir, sameDir := t.TempDir(), t.TempDir()
			copyTree(t, filepath.Join(startDir, "ws"), filepath.Join(crossDir, "ws"))
			copyTree(t, filepath.Join(startDir, "ws"), filepath.Join(sameDir, "ws"))
			resumeArgs := []string{"orchestrate", "--resume", "--no-default-checks", "--check", "true", "--workdir", "ws"}
			env := processEnv(append(stubOrgEnv, "LHA_MAX_CYCLES=4")...)
			cross := c.resume(t, crossDir, env, resumeArgs...)
			same := c.other(t, sameDir, env, resumeArgs...)
			if cross.code != same.code || missionIDRE.FindString(cross.stdout) != missionID || missionIDRE.FindString(same.stdout) != missionID {
				t.Fatalf("cross %+v\nsame %+v\n(mission %s)", cross, same, missionID)
			}
			if g, p := report(t, cross.stdout), report(t, same.stdout); g != p {
				t.Errorf("reports differ:\ncross: %s\nsame:  %s", g, p)
			}
			if _, err := os.Stat(filepath.Join(crossDir, "ws", "residue.txt")); !os.IsNotExist(err) {
				t.Fatal("the resumed run kept uncommitted residue")
			}
			crossWS := orgWorkspace(t, filepath.Join(crossDir, "ws"), false)
			if !strings.Contains(crossWS.Anchor["events.ndjson"], `"resumed":true,"run":2`) {
				t.Fatalf("no resumed run event: %s", crossWS.Anchor["events.ndjson"])
			}
			compareOrgWorkspaces(t, crossWS, orgWorkspace(t, filepath.Join(sameDir, "ws"), false))
		})
	}
}
