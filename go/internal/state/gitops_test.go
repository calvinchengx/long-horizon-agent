package state

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=u", "-c", "user.email=e@x"}, args...)...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// must panics on err (a panicking test fails with a stack trace); it keeps f(g()) call sites
// compact.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func newRepo(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := InitRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	must(CommitAll(ctx, dir, "init"))
	return dir
}

func parentRepo(t *testing.T, root string, ignoreLHA bool) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-q", "-b", "main")
	if ignoreLHA {
		writeFile(t, filepath.Join(root, ".gitignore"), ".lha/\n")
		git(t, root, "add", ".gitignore")
	}
	git(t, root, "commit", "-q", "--allow-empty", "-m", "base")
	return root
}

func TestGitEnvIsNonInteractiveAndCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	env := map[string]string{}
	for _, kv := range gitEnv() {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := env[k]; dup {
			t.Fatalf("duplicate env key %s", k)
		}
		env[k] = v
	}
	if env["LC_ALL"] != "C" || env["LANG"] != "C" || env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("env = %v", env)
	}
	if v, ok := env["GIT_ASKPASS"]; !ok || v != "" {
		t.Fatal("GIT_ASKPASS must be set empty")
	}
}

func TestCommitAllCleanTreeIsNoopUnderAnyLocale(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	head := must(HeadSHA(ctx, repo))
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")
	if got := must(CommitAll(ctx, repo, "noop")); got != head {
		t.Fatalf("CommitAll = %q, want %q", got, head)
	}
	if n := len(must(LogOneline(ctx, repo, 10))); n != 1 {
		t.Fatalf("log has %d commits", n)
	}
}

func TestRunGitTimeoutAndFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, err := RunGitWith(ctx, dir, RunOptions{Timeout: time.Nanosecond}, "status")
	var ge *GitError
	if !errors.As(err, &ge) || !strings.Contains(ge.Error(), "timed out after") {
		t.Fatalf("err = %v", err)
	}
	if want := "git status timed out after 1e-09s"; ge.Error() != want {
		t.Fatalf("err = %q, want %q", ge.Error(), want)
	}
	_, err = RunGit(ctx, dir, "rev-parse", "HEAD")
	if !errors.As(err, &ge) || !strings.HasPrefix(ge.Error(), "git rev-parse HEAD failed (128): ") {
		t.Fatalf("err = %v", err)
	}
	out, err := RunGitWith(ctx, dir, RunOptions{NoCheck: true}, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("NoCheck returned %v (%q)", err, out)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := RunGit(cctx, dir, "status"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx err = %v", err)
	}
	if _, err := RunGit(ctx, filepath.Join(dir, "missing"), "status"); err == nil || errors.As(err, &ge) {
		t.Fatalf("missing cwd err = %v", err)
	}
}

func TestEmptyRepoHelpers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := InitRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := InitRepo(ctx, dir); err != nil { // idempotent
		t.Fatal(err)
	}
	if must(HasCommits(ctx, dir)) || must(HeadSHA(ctx, dir)) != "" {
		t.Fatal("fresh repo has commits")
	}
	if must(ExistsAtHead(ctx, dir, "x")) {
		t.Fatal("ExistsAtHead on empty repo")
	}
	if b := must(ListBranches(ctx, dir, true)); len(b) != 0 {
		t.Fatal(b)
	}
	if l := must(LogOneline(ctx, dir, 10)); len(l) != 0 {
		t.Fatal(l)
	}
	if err := ResetToHead(ctx, dir); err != nil {
		t.Fatal(err)
	}
	gd := must(GitDir(ctx, dir))
	if filepath.Base(gd) != ".git" || !filepath.IsAbs(gd) {
		t.Fatal(gd)
	}
	if name := must(RunGit(ctx, dir, "config", "user.name")); name != DefaultAuthorName {
		t.Fatal(name)
	}
	if top := must(Toplevel(ctx, t.TempDir())); top != "" {
		t.Fatalf("Toplevel outside a repo = %q", top)
	}
}

func TestResetToHeadDiscardsAllResidueButKeepsEnvs(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "build/\n")
	must(CommitAll(ctx, repo, "ignore build"))
	writeFile(t, filepath.Join(repo, "a.txt"), "edited")
	writeFile(t, filepath.Join(repo, "new.txt"), "untracked")
	writeFile(t, filepath.Join(repo, "build", "out.o"), "ignored")
	writeFile(t, filepath.Join(repo, ".venv", "keep"), "env")
	must(RunGit(ctx, repo, "add", "new.txt"))

	if err := ResetToHead(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "a" {
		t.Fatalf("a.txt = %q", b)
	}
	if exists(filepath.Join(repo, "new.txt")) || exists(filepath.Join(repo, "build")) {
		t.Fatal("residue survived")
	}
	if !exists(filepath.Join(repo, ".venv", "keep")) {
		t.Fatal(".venv removed")
	}
	if st := must(RunGit(ctx, repo, "status", "--porcelain")); st != "?? .venv/" {
		t.Fatalf("status = %q", st)
	}
	if err := ResetToHeadKeep(ctx, repo, nil); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(repo, ".venv")) {
		t.Fatal(".venv kept with an empty keep list")
	}
}

func TestListBranchesAndCommitsAhead(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	must(RunGit(ctx, repo, "branch", "empty"))
	got := must(ListBranches(ctx, repo, false))
	slices.Sort(got)
	if !slices.Equal(got, []string{"empty", "main"}) {
		t.Fatal(got)
	}
	if n := must(CommitsAhead(ctx, repo, "main", "empty")); n != 0 {
		t.Fatal(n)
	}
	must(RunGit(ctx, repo, "checkout", "-q", "empty"))
	writeFile(t, filepath.Join(repo, "b.txt"), "b")
	must(CommitAll(ctx, repo, "b"))
	if n := must(CommitsAhead(ctx, repo, "main", "empty")); n != 1 {
		t.Fatal(n)
	}
	// Remote-tracking branches, skipping the <remote>/HEAD alias.
	clone := filepath.Join(t.TempDir(), "clone")
	git(t, filepath.Dir(clone), "clone", "-q", repo, clone)
	all := must(ListBranches(ctx, clone, true))
	if !slices.Contains(all, "origin/main") || !slices.Contains(all, "origin/empty") {
		t.Fatal(all)
	}
	for _, b := range all {
		if strings.HasSuffix(b, "/HEAD") {
			t.Fatal(all)
		}
	}
	log := must(LogOneline(ctx, repo, 1))
	if len(log) != 1 || !strings.HasSuffix(log[0], " b") {
		t.Fatal(log)
	}
}

func TestIsRepoRequiresTheToplevel(t *testing.T) {
	ctx := context.Background()
	parent := parentRepo(t, filepath.Join(t.TempDir(), "parent"), false)
	nested := filepath.Join(parent, "sub", "work")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if !must(IsRepo(ctx, parent)) {
		t.Fatal("parent is not a repo")
	}
	if must(IsRepo(ctx, nested)) {
		t.Fatal("nested dir treated as its own repo")
	}
}

func TestHeadLookupsAreRelativeToCwd(t *testing.T) {
	ctx := context.Background()
	repo := parentRepo(t, filepath.Join(t.TempDir(), "repo"), false)
	sub := filepath.Join(repo, "sub")
	writeFile(t, filepath.Join(sub, "f.txt"), "hello\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "f")
	if !must(ExistsAtHead(ctx, sub, "f.txt")) {
		t.Fatal("f.txt not found relative to sub")
	}
	if got := must(ShowAtHead(ctx, sub, "f.txt")); got != "hello" {
		t.Fatalf("ShowAtHead = %q", got)
	}
	if must(ExistsAtHead(ctx, sub, "missing.txt")) {
		t.Fatal("missing.txt exists")
	}
}

func TestCommitAllForcePathsIgnoredFiles(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "secret/\n")
	writeFile(t, filepath.Join(repo, "secret", "x"), "x")
	must(CommitAll(ctx, repo, "force", "secret/x", "secret/absent"))
	if !must(ExistsAtHead(ctx, repo, "secret/x")) {
		t.Fatal("force path not committed")
	}
}
