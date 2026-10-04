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
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Ported from python/tests/unit/test_mutation_gate.py (the verifier half).

// fixedVerifier returns one verdict: "passed", "failed" or "unverified".
type fixedVerifier struct{ verdict string }

func (f fixedVerifier) Verify(context.Context, contracts.SandboxSession, []contracts.Check) (contracts.VerificationResult, error) {
	if f.verdict == "unverified" {
		return contracts.NewVerificationResult(nil), nil
	}
	ok := f.verdict == "passed"
	return contracts.NewVerificationResult([]contracts.CheckResult{{Name: "tests", Passed: ok, Gating: true}}), nil
}

func (f fixedVerifier) DrainEvents() []contracts.EventRecord {
	return []contracts.EventRecord{{Kind: "inner-event"}}
}

// envSession runs commands for real in dir like execSession, with opts.Env set (the gate's
// LHA_CHANGED_FILES).
type envSession struct{ execSession }

func (s *envSession) Exec(ctx context.Context, argv []string, opts contracts.ExecOptions) (contracts.ExecResult, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = s.dir
	cmd.Env = os.Environ()
	for k, v := range opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	res := contracts.ExecResult{Stdout: string(out)}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case err != nil:
		return res, err
	}
	return res, nil
}

type brokenSession struct{ execSession }

func (s *brokenSession) Exec(context.Context, []string, contracts.ExecOptions) (contracts.ExecResult, error) {
	return contracts.ExecResult{}, errors.New("sandbox gone")
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "i"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
	}
	return dir
}

func writeText(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// recorder records LHA_CHANGED_FILES to out, prints a survivor, and exits with code.
func recorder(out string, code string) string {
	return `printf "%s" "$` + ChangedFilesEnv + `" > ` + out + `; echo "survived: parser.py:42 > to >="; exit ` + code
}

func TestChangedFilesAreTrackedEditsAndUntrackedFilesOutsideLha(t *testing.T) {
	repo := gitRepo(t)
	writeText(t, filepath.Join(repo, "kept.py"), "a\n")
	writeText(t, filepath.Join(repo, ".gitignore"), "ignored.log\n")
	for _, args := range [][]string{{"add", "kept.py", ".gitignore"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "c"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
	}
	if got, err := ChangedFiles(context.Background(), repo); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
	writeText(t, filepath.Join(repo, "kept.py"), "b\n")
	writeText(t, filepath.Join(repo, "new file.py"), "x\n")
	writeText(t, filepath.Join(repo, "ignored.log"), "x\n")
	writeText(t, filepath.Join(repo, ".lha", "checklist.json"), "{}")
	if got, err := ChangedFiles(context.Background(), repo); err != nil || !reflect.DeepEqual(got, []string{"kept.py", "new file.py"}) {
		t.Fatal(got, err)
	}
}

func TestAGreenVerdictMustSurviveTheMutationCheck(t *testing.T) {
	repo := gitRepo(t)
	writeText(t, filepath.Join(repo, "parser.py"), "x = 1\n")
	seen := filepath.Join(t.TempDir(), "seen")
	session := &envSession{execSession{dir: repo}}
	gate := &MutationGateVerifier{Inner: fixedVerifier{"passed"}, Command: recorder(seen, "1"), Workdir: repo, TimeoutS: 60}
	red, err := gate.Verify(context.Background(), session, nil)
	got, _ := os.ReadFile(seen)
	if err != nil || red.Verdict != contracts.VerdictFailed || string(got) != "parser.py" {
		t.Fatalf("%+v %v %q", red, err, got)
	}
	last := red.Results[len(red.Results)-1]
	if last.Name != MutationCheckName || last.ExitCode != 1 || !last.Gating || !strings.Contains(last.OutputTail, "survived: parser.py:42") ||
		!strings.Contains(red.FailureReport(0), "survived: parser.py:42") {
		t.Fatalf("%+v", last)
	}
	gate.Command = recorder(seen, "0")
	green, err := gate.Verify(context.Background(), session, nil)
	if err != nil || green.Verdict != contracts.VerdictPassed || len(green.Results) != 2 || green.Results[1].Name != "mutation" {
		t.Fatalf("%+v %v", green, err)
	}
}

func TestTheMutationCheckNeverRunsOnARedOrUnverifiedVerdict(t *testing.T) {
	for _, inner := range []string{"failed", "unverified"} {
		repo := gitRepo(t)
		writeText(t, filepath.Join(repo, "parser.py"), "x = 1\n")
		seen := filepath.Join(t.TempDir(), "seen")
		gate := &MutationGateVerifier{Inner: fixedVerifier{inner}, Command: recorder(seen, "0"), Workdir: repo, TimeoutS: 60}
		res, err := gate.Verify(context.Background(), &envSession{execSession{dir: repo}}, nil)
		if _, statErr := os.Stat(seen); err != nil || res.Verdict != inner || statErr == nil {
			t.Fatalf("%s: %+v %v", inner, res, err)
		}
	}
}

func TestTheMutationCheckDoesNotRunWithoutChanges(t *testing.T) {
	repo := gitRepo(t)
	seen := filepath.Join(t.TempDir(), "seen")
	gate := &MutationGateVerifier{Inner: fixedVerifier{"passed"}, Command: recorder(seen, "1"), Workdir: repo, TimeoutS: 60}
	res, err := gate.Verify(context.Background(), &envSession{execSession{dir: repo}}, nil)
	if _, statErr := os.Stat(seen); err != nil || res.Verdict != contracts.VerdictPassed || statErr == nil {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestTheMutationCheckFailsClosed(t *testing.T) {
	plain := t.TempDir()
	gate := &MutationGateVerifier{Inner: fixedVerifier{"passed"}, Command: "exit 0", Workdir: plain, TimeoutS: 60}
	res, err := gate.Verify(context.Background(), &envSession{execSession{dir: plain}}, nil)
	if err != nil || res.Verdict != contracts.VerdictFailed || !strings.Contains(res.Results[len(res.Results)-1].OutputTail, "cannot list the changed files") {
		t.Fatalf("%+v %v", res, err)
	}
	repo := gitRepo(t)
	writeText(t, filepath.Join(repo, "parser.py"), "x = 1\n")
	gate.Workdir = repo
	res, err = gate.Verify(context.Background(), &brokenSession{execSession{dir: repo}}, nil)
	if err != nil || res.Verdict != contracts.VerdictFailed || !strings.Contains(res.Results[len(res.Results)-1].OutputTail, "could not execute check") {
		t.Fatalf("%+v %v", res, err)
	}
	if events := gate.DrainEvents(); len(events) != 1 || events[0].Kind != "inner-event" {
		t.Fatal(events)
	}
}

// A trusted check's worktree add and remove take the repository's worktree lock, like an
// implementer's (agents/org TestWorktreeOperationsWaitForTheRepositorysWorktreeLock).
func TestTrustedWorktreesWaitForTheRepositorysWorktreeLock(t *testing.T) {
	repo := gitRepo(t)
	worktree := filepath.Join(t.TempDir(), "trusted")
	for _, step := range []struct {
		name string
		op   func()
	}{
		{"add", func() {
			if err := addWorktree(context.Background(), repo, worktree, "HEAD"); err != nil {
				t.Error(err)
			}
		}},
		{"remove", func() { removeWorktree(repo, worktree) }},
	} {
		done := make(chan struct{})
		unlock := state.WorktreeLock(context.Background(), repo)
		go func() { step.op(); close(done) }()
		select {
		case <-done:
			unlock()
			t.Fatalf("%s ran while the lock was held", step.name)
		case <-time.After(500 * time.Millisecond):
		}
		unlock()
		<-done
	}
}
