package verify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Ported from python/tests/unit/test_witnesses.py and test_trusted_runner.py.

var trustedMap = map[string][]string{"e2e": {"make", "e2e"}, "warehouse-tds": {"./ci.sh", "warehouse-tds"}}

func TestValidWitnesses(t *testing.T) {
	for _, w := range []string{"go:TestX", "go:TestX/sub_case", "go:TestX@./internal/...", "go:TestX@example.com/mod/pkg",
		"pytest:tests/test_a.py::test_b", "pytest:tests/test_a.py::TestC::test_d[a-1]", "cmd:make check && echo ok",
		"trusted:e2e", "ci:warehouse-tds", "  go:TestPadded  "} {
		if err := ValidateWitness(w); err != nil {
			t.Errorf("%q: %v", w, err)
		}
	}
}

func TestInvalidWitnesses(t *testing.T) {
	for w, fragment := range map[string]string{
		"TestX": "unknown kind", "sdk:TestX": "unknown kind", "go:": "Go test name", "go:1Test": "Go test name",
		"go:TestX/": "Go test name", "go:TestX;rm -rf /": "Go test name", "go:TestX@./...;rm": "package pattern",
		"go:TestX@-exec=evil": "package pattern", "go:TestX@$(evil)": "package pattern", "pytest:": "pytest node id",
		"pytest:tests/a.py -x": "pytest node id", "pytest:--rootdir=/": "pytest node id", "pytest:a.py;evil": "pytest node id",
		"cmd:   ": "needs a shell command", "trusted:": "operator-defined", "ci:has space": "operator-defined",
	} {
		err := ValidateWitness(w)
		if err == nil || !strings.Contains(err.Error(), "witness") || !strings.Contains(err.Error(), fragment) {
			t.Errorf("%q: %v (want %q)", w, err, fragment)
		}
	}
	if err := ValidateWitness("TestX"); err.Error() != "witness 'TestX': unknown kind; expected one of go:, pytest:, cmd:, trusted:, ci:" {
		t.Fatal(err)
	}
}

func TestParseWitnessKinds(t *testing.T) {
	c, _ := ParseWitness("pytest:tests/test_a.py::test_b", trustedMap)
	if !reflect.DeepEqual(c.Command, []string{"uv", "run", "pytest", "-q", "tests/test_a.py::test_b"}) ||
		c.Name != "pytest:tests/test_a.py::test_b" || c.Where != "sandbox" || !c.Gating {
		t.Fatalf("%+v", c)
	}
	c, _ = ParseWitness("cmd: make check ", trustedMap)
	if !reflect.DeepEqual(c.Command, []string{"sh", "-c", "make check"}) || c.Name != "cmd: make check" {
		t.Fatalf("%+v", c)
	}
	c, _ = ParseWitness("cmd:"+strings.Repeat("echo x; ", 30), trustedMap)
	if len(c.Name) != 80 || !strings.HasSuffix(c.Name, "...") {
		t.Fatalf("%q", c.Name)
	}
	c, _ = ParseWitness("go:TestX", trustedMap)
	if c.Command[0] != "sh" || !strings.Contains(c.Command[2], "go test -count=1 -run '^TestX$' -v ./...") ||
		!strings.Contains(c.Command[2], "--- PASS: TestX( |$)") {
		t.Fatalf("%q", c.Command)
	}
	c, _ = ParseWitness("go:TestX/sub@./internal/...", trustedMap)
	if !strings.Contains(c.Command[2], "-run '^TestX$/^sub$' -v ./internal/...") || !strings.Contains(c.Command[2], "--- PASS: TestX/sub( |$)") {
		t.Fatalf("%q", c.Command[2])
	}
	c, _ = ParseWitness("trusted:e2e", trustedMap)
	if c.Where != "trusted" || !reflect.DeepEqual(c.Command, []string{"make", "e2e"}) {
		t.Fatalf("%+v", c)
	}
	c.Command[0] = "mutated"
	if trustedMap["e2e"][0] != "make" {
		t.Fatal("trusted argv not copied")
	}
	c, _ = ParseWitness("ci:warehouse-tds", trustedMap)
	if c.Where != "trusted" || c.Name != "ci:warehouse-tds" {
		t.Fatalf("%+v", c)
	}
}

func TestUnknownTrustedCheckNamesKnownOnes(t *testing.T) {
	var unknown *UnknownTrustedCheckError
	_, err := ParseWitness("trusted:nope", trustedMap)
	if !errors.As(err, &unknown) || !strings.Contains(err.Error(), "known: e2e, warehouse-tds") {
		t.Fatal(err)
	}
	_, err = ParseWitness("ci:e2e", nil)
	if !errors.As(err, &unknown) || !strings.Contains(err.Error(), "LHA_TRUSTED_CHECKS") {
		t.Fatal(err)
	}
}

func TestItemChecksAreOrderedAndUnique(t *testing.T) {
	item := contracts.NewChecklistItem("01", "x")
	item.Witnesses = []string{"go:TestA", "trusted:e2e", "go:TestA", "cmd:true"}
	checks, err := ItemChecks(item, trustedMap)
	if err != nil {
		t.Fatal(err)
	}
	names, wheres := []string{}, []string{}
	for _, c := range checks {
		names, wheres = append(names, c.Name), append(wheres, c.Where)
	}
	if strings.Join(names, ",") != "go:TestA,trusted:e2e,go:TestA-2,cmd:true" || strings.Join(wheres, ",") != "sandbox,trusted,sandbox,sandbox" {
		t.Fatalf("%v %v", names, wheres)
	}
	item.Witnesses = []string{"ci:missing"}
	if _, err := ItemChecks(item, trustedMap); err == nil {
		t.Fatal("unknown trusted check accepted")
	}
}

const goDemoTest = `package demo

import "testing"

func TestPasses(t *testing.T) {
	t.Run("inner", func(t *testing.T) {})
}

func TestFails(t *testing.T) { t.Fatal("boom") }

func TestSkips(t *testing.T) { t.Skip("needs a sidecar") }
`

func TestGoWitnessAgainstARealToolchain(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil || testing.Short() {
		t.Skip("go toolchain not on PATH (or -short)")
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n\ngo 1.21\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "demo_test.go"), []byte(goDemoTest), 0o644)
	run := func(argv []string) (int, string) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		out, _ := cmd.CombinedOutput()
		return cmd.ProcessState.ExitCode(), string(out)
	}
	w := func(s string) []string {
		c, err := ParseWitness(s, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.Command
	}
	if code, out := run(w("go:TestPasses")); code != 0 || !strings.Contains(out, "--- PASS: TestPasses") {
		t.Fatalf("%d %s", code, out)
	}
	if code, _ := run(GoTestCommand("TestPasses/inner", "")); code != 0 {
		t.Fatal("subtest")
	}
	if code, out := run(w("go:TestDoesNotExist")); code == 0 || !strings.Contains(out, "did not run and pass") {
		t.Fatalf("missing: %d %s", code, out)
	}
	if code, _ := run(GoTestCommand("TestPass", "")); code == 0 {
		t.Fatal("a prefix matched")
	}
	if code, out := run(w("go:TestFails")); code == 0 || !strings.Contains(out, "boom") {
		t.Fatalf("failing: %d %s", code, out)
	}
	if code, _ := run(w("go:TestSkips@.")); code == 0 {
		t.Fatal("a skipped test passed")
	}
}

func TestCandidateCommitLeavesHEADIndexAndTreeAlone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := state.InitRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("1"), 0o644)
	if _, err := state.CommitAll(ctx, dir, "base"); err != nil {
		t.Fatal(err)
	}
	head, _ := state.HeadSHA(ctx, dir)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("2"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "new.txt"), []byte("n"), 0o644)
	commit, err := CandidateCommit(ctx, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := state.HeadSHA(ctx, dir)
	status, _ := state.RunGit(ctx, dir, "status", "--porcelain")
	parent, _ := state.RunGit(ctx, dir, "rev-parse", commit+"^")
	content, _ := state.RunGit(ctx, dir, "show", commit+":new.txt")
	if after != head || parent != head || content != "n" || !strings.Contains(status, "?? new.txt") {
		t.Fatalf("head %s after %s parent %s content %q status %q", head, after, parent, content, status)
	}
}

type fakeInner struct{ seen []contracts.Check }

func (f *fakeInner) Verify(_ context.Context, _ contracts.SandboxSession, checks []contracts.Check) (contracts.VerificationResult, error) {
	f.seen = checks
	results := []contracts.CheckResult{}
	for _, c := range checks {
		results = append(results, contracts.CheckResult{Name: c.Name, Passed: true, Gating: true})
	}
	return contracts.NewVerificationResult(results), nil
}

func TestTrustedAwareVerifierRoutesAndRunsOnTheCandidate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_ = state.InitRepo(ctx, dir)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("1"), 0o644)
	_, _ = state.CommitAll(ctx, dir, "base")
	_ = os.WriteFile(filepath.Join(dir, "uncommitted.txt"), []byte("agent work"), 0o644)
	inner := &fakeInner{}
	v := NewTrustedAwareVerifier(inner, NewCommandTrustedRunner(), dir)
	res, err := v.Verify(ctx, nil, []contracts.Check{
		{Name: "unit", Command: []string{"true"}, Gating: true, Where: "sandbox"},
		{Name: "e2e", Command: []string{"sh", "-c", `test -f uncommitted.txt && test "$LHA_CHECK_NAME" = e2e`}, Gating: true, Where: "trusted"},
		{Name: "red", Command: []string{"sh", "-c", "echo nope; exit 4"}, Gating: true, Where: "trusted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inner.seen) != 1 || inner.seen[0].Name != "unit" {
		t.Fatalf("inner saw %+v", inner.seen)
	}
	if len(res.Results) != 3 || !res.Results[1].Passed || res.Results[2].Passed || res.Results[2].ExitCode != 4 ||
		res.Results[2].OutputTail != "nope" || res.Verdict != "failed" {
		t.Fatalf("%+v", res)
	}
	if wt, _ := state.RunGit(ctx, dir, "worktree", "list"); strings.Count(wt, "\n") != 0 {
		t.Fatalf("worktree left behind: %s", wt)
	}
	// Not a repository: every trusted check fails, the verifier never errors.
	res, err = NewTrustedAwareVerifier(inner, NewCommandTrustedRunner(), t.TempDir()).Verify(ctx, nil,
		[]contracts.Check{{Name: "e2e", Command: []string{"true"}, Gating: true, Where: "trusted"}})
	if err != nil || res.AllGreen || !strings.HasPrefix(res.Results[0].OutputTail, "[trusted] could not create the candidate commit: GitError: ") {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestHarnessGlobs(t *testing.T) {
	for glob, cases := range map[string]map[string]bool{
		"Makefile":       {"Makefile": true, "sub/Makefile": false},
		"e2e/**":         {"e2e/a/b.sh": true, "e2e": false, "x/e2e/a": false},
		"**/*.yml":       {"a.yml": true, "a/b/c.yml": true, "a.yaml": false},
		"/.github/*":     {".github/ci.yml": true, ".github/w/ci.yml": false},
		"file?.txt":      {"file1.txt": true, "file12.txt": false, "file/.txt": false},
		"docs/a+b(c).md": {"docs/a+b(c).md": true},
	} {
		rx := GlobRegex(glob)
		for path, want := range cases {
			if rx.MatchString(path) != want {
				t.Errorf("%q vs %q: want %v", glob, path, want)
			}
		}
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "Makefile"), []byte("x"), 0o644)
	if snap := SnapshotHarnessGlobs(dir, []string{"Makefile", " "}); len(snap) != 1 {
		t.Fatalf("%v", snap)
	}
	if snap := SnapshotHarness(dir); len(snap) != 0 {
		t.Fatalf("%v", snap)
	}
}
