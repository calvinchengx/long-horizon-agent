package verify

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// fakeSession records Exec calls and returns canned results by argv[0]; it never runs anything.
type fakeSession struct {
	results map[string]contracts.ExecResult
	calls   [][]string
	timeout []int
}

func (s *fakeSession) Workdir() string { return "/sandbox" }
func (s *fakeSession) Exec(_ context.Context, argv []string, opts contracts.ExecOptions) (contracts.ExecResult, error) {
	s.calls = append(s.calls, argv)
	s.timeout = append(s.timeout, opts.TimeoutS)
	if argv[0] == "explode" {
		return contracts.ExecResult{}, errors.New("container gone")
	}
	return s.results[argv[0]], nil
}
func (s *fakeSession) WriteFile(context.Context, string, string) error  { return nil }
func (s *fakeSession) ReadFile(context.Context, string) (string, error) { return "", nil }
func (s *fakeSession) Close(context.Context) error                      { return nil }

// execSession runs commands for real with os/exec in a temp dir (a stand-in for the local
// sandbox, which lives in go/internal/execution).
type execSession struct{ dir string }

func (s *execSession) Workdir() string { return s.dir }
func (s *execSession) Exec(ctx context.Context, argv []string, opts contracts.ExecOptions) (contracts.ExecResult, error) {
	tctx, cancel := context.WithTimeout(ctx, time.Duration(opts.TimeoutS)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, argv[0], argv[1:]...)
	cmd.Dir = s.dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := contracts.ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), TimedOut: tctx.Err() != nil}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case err != nil:
		return res, err
	}
	return res, nil
}
func (s *execSession) WriteFile(context.Context, string, string) error  { return nil }
func (s *execSession) ReadFile(context.Context, string) (string, error) { return "", nil }
func (s *execSession) Close(context.Context) error                      { return nil }

func intp(i int) *int { return &i }

func TestDefaultPythonChecks(t *testing.T) {
	checks := DefaultPythonChecks()
	names := []string{}
	for _, c := range checks {
		names = append(names, c.Name)
		if !c.Gating {
			t.Fatal("default checks must gate")
		}
	}
	if !slices.Equal(names, []string{"ruff", "ty", "pytest"}) {
		t.Fatal(names)
	}
	if !slices.Equal(checks[0].Command, []string{"uv", "run", "ruff", "check", "."}) ||
		!slices.Equal(checks[1].Command, []string{"uv", "run", "ty", "check"}) ||
		!slices.Equal(checks[2].Command, []string{"uv", "run", "pytest", "-q"}) {
		t.Fatal(checks)
	}
	checks[0].Command[0] = "mutated"
	if DefaultPythonCheckCommands[0][0] != "uv" {
		t.Fatal("DefaultPythonChecks aliases the package-level commands")
	}
}

func TestVerifierExecutesThroughSessionAndCapturesOutput(t *testing.T) {
	session := &fakeSession{results: map[string]contracts.ExecResult{
		"pytest": {ExitCode: 1, Stdout: "..F\nFAILED test_x - assert 1 == 2"},
		"ruff":   {ExitCode: 0, Stdout: "All checks passed!"},
	}}
	v := NewDeterministicVerifier()
	v.DefaultTimeoutS = 77
	result, err := v.Verify(context.Background(), session, []contracts.Check{
		{Name: "ruff", Command: []string{"ruff", "check"}, Gating: true},
		{Name: "pytest", Command: []string{"pytest"}, Gating: true, TimeoutS: intp(5)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(session.calls) != 2 || !slices.Equal(session.calls[0], []string{"ruff", "check"}) ||
		!slices.Equal(session.timeout, []int{77, 5}) {
		t.Fatal(session.calls, session.timeout)
	}
	byName := map[string]contracts.CheckResult{}
	for _, r := range result.Results {
		byName[r.Name] = r
	}
	if !strings.Contains(byName["pytest"].OutputTail, "assert 1 == 2") || byName["pytest"].DurationS < 0 {
		t.Fatal(byName["pytest"])
	}
	if result.Verdict != contracts.VerdictFailed || !strings.Contains(result.FailureReport(0), "assert 1 == 2") {
		t.Fatal(result)
	}
}

func TestVerifierSandboxErrorIsAFailure(t *testing.T) {
	result, err := NewDeterministicVerifier().Verify(context.Background(), &fakeSession{},
		[]contracts.Check{{Name: "boom", Command: []string{"explode"}, Gating: true}})
	if err != nil {
		t.Fatal(err)
	}
	r := result.Results[0]
	if result.AllGreen || r.ExitCode != -1 || r.Passed ||
		r.OutputTail != "[verifier] could not execute check: errorString: container gone" {
		t.Fatalf("%+v", r)
	}
}

func TestVerifierPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewDeterministicVerifier().Verify(ctx, &fakeSession{},
		[]contracts.Check{{Name: "boom", Command: []string{"explode"}, Gating: true}})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestVerifierWithNoChecksIsUnverified(t *testing.T) {
	result, err := NewDeterministicVerifier().Verify(context.Background(), &fakeSession{}, nil)
	if err != nil || result.Verdict != contracts.VerdictUnverified || result.AllGreen {
		t.Fatal(result, err)
	}
}

func TestVerifierDoesNotReserveHarnessIntegrityName(t *testing.T) {
	// Python passes reserved=() here: a user check named harness_integrity keeps its name, and
	// duplicate names are still suffixed.
	result, err := NewDeterministicVerifier().Verify(context.Background(), &fakeSession{}, []contracts.Check{
		{Name: contracts.HarnessIntegrityCheck, Command: []string{"x"}, Gating: true},
		{Name: "x", Command: []string{"x"}, Gating: true},
		{Name: "x", Command: []string{"x"}, Gating: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, r := range result.Results {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"harness_integrity", "x", "x-2"}) || !result.AllGreen {
		t.Fatal(names, result)
	}
}

func TestVerifierRunsRealCommands(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	session := &execSession{dir: dir}
	result, err := NewDeterministicVerifier().Verify(context.Background(), session, []contracts.Check{
		{Name: "cat", Command: []string{"cat", "f.txt"}, Gating: true},
		{Name: "fail", Command: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}, Gating: false},
		{Name: "missing", Command: []string{"definitely-not-a-binary-lha"}, Gating: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AllGreen || result.Results[0].OutputTail != "hello" {
		t.Fatalf("%+v", result)
	}
	fail := result.Results[1]
	if fail.Passed || fail.ExitCode != 3 || fail.OutputTail != "out\n--- stderr ---\nerr" {
		t.Fatalf("%+v", fail)
	}
	missing := result.Results[2]
	if missing.ExitCode != -1 || !strings.HasPrefix(missing.OutputTail, "[verifier] could not execute check: Error: ") {
		t.Fatalf("%+v", missing)
	}
	timed, err := (&DeterministicVerifier{DefaultTimeoutS: 1, OutputTail: 10}).Verify(context.Background(), session,
		[]contracts.Check{{Name: "sleep", Command: []string{"sleep", "5"}, Gating: true}})
	if err != nil || timed.AllGreen || !timed.Results[0].TimedOut {
		t.Fatalf("%+v %v", timed, err)
	}
}

func TestClipOutputTail(t *testing.T) {
	clipped := ClipOutputTail(strings.Repeat("a", 5000)+"END", "ERR", 100)
	if !strings.HasSuffix(clipped, "ERR") || !strings.Contains(clipped, "END") || len(clipped) >= 150 {
		t.Fatal(clipped)
	}
	cases := []struct {
		stdout, stderr string
		limit          int
		want           string
	}{
		{"out \n", "", 100, "out"},
		{"", "err\x1c", 100, "err"},
		{"", "", 100, ""},
		{"a", "b", 100, "a\n--- stderr ---\nb"},
		{"ééééé", "", 3, "...[truncated]...\néé" + "é"},
		{"abcde", "", 0, "...[truncated]...\nabcde"}, // Python: s[-0:] is the whole string
		{"abcde", "", -2, "...[truncated]...\ncde"},  // Python: s[-(-2):] is s[2:]
	}
	for _, c := range cases {
		if got := ClipOutputTail(c.stdout, c.stderr, c.limit); got != c.want {
			t.Errorf("ClipOutputTail(%q, %q, %d) = %q, want %q", c.stdout, c.stderr, c.limit, got, c.want)
		}
	}
}

func TestMarkFlakyRequiresEvidence(t *testing.T) {
	q := NewDefaultFlakyQuarantine()
	var fe *FlakeEvidenceError
	err := q.MarkFlaky("pytest")
	if !errors.As(err, &fe) || err.Error() != "refusing to quarantine 'pytest': no recorded pass+fail on the same revision (need >= 1 of each over >= 2 runs)" {
		t.Fatal(err)
	}
	// Failing on different revisions is a regression, not a flake.
	q.Record("pytest", "r1", true)
	q.Record("pytest", "r2", false)
	q.Record("pytest", "r2", false)
	if err := q.MarkFlaky("pytest"); !errors.As(err, &fe) {
		t.Fatal(err)
	}
	q.RecordResults([]contracts.CheckResult{{Name: "pytest", Passed: true, Gating: true}}, "r2")
	if err := q.MarkFlaky("pytest"); err != nil {
		t.Fatal(err)
	}
	if !q.IsFlaky("pytest") || q.IsFlaky("other") || !q.HasFlakeEvidence("pytest") {
		t.Fatal("flaky state")
	}
	if clamped := NewFlakyQuarantine(0, 0); clamped.minFlips != 1 || clamped.minRuns != 2 {
		t.Fatal(clamped.minFlips, clamped.minRuns)
	}
}

func TestFlakyQuarantinePartitions(t *testing.T) {
	q := NewDefaultFlakyQuarantine()
	for _, passed := range []bool{true, false, true} {
		q.Record("flaky_test", "abc", passed)
	}
	if err := q.MarkFlaky("flaky_test"); err != nil {
		t.Fatal(err)
	}
	gating, quarantined := q.Partition([]contracts.Check{
		{Name: "solid", Command: []string{"x"}, Gating: true},
		{Name: "flaky_test", Command: []string{"y"}, Gating: true},
	})
	if len(gating) != 1 || gating[0].Name != "solid" || len(quarantined) != 1 ||
		quarantined[0].Name != "flaky_test" || quarantined[0].Gating {
		t.Fatal(gating, quarantined)
	}
}
