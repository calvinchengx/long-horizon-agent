package state

// Harness git cannot be steered by the work tree it runs in, nor by the operator's environment
// (mirrors python/tests/unit/test_git_hardening.py): planted hooks / fsmonitor / core.hooksPath /
// attribute drivers never execute, a tampered gitdir pointer is refused before git runs, and the
// operator's GIT_DIR / GIT_WORK_TREE / GIT_CONFIG_* do not leak in.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func plantHook(t *testing.T, path, marker string) {
	t.Helper()
	writeFile(t, path, "#!/bin/sh\ntouch '"+marker+"'\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// addWorktree mirrors lha.agents.integrator.add_worktree: <repo>/.git/lha-worktrees/<name>.
func addWorktree(t *testing.T, repo, name string) string {
	t.Helper()
	ctx := context.Background()
	gitDir := must(GitDir(ctx, repo))
	path := filepath.Join(gitDir, "lha-worktrees", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	must(RunGit(ctx, repo, "worktree", "add", "--quiet", "-B", "lha/"+name, path, "HEAD"))
	return path
}

func wantGitError(t *testing.T, err error, substr string) {
	t.Helper()
	var ge *GitError
	if !errors.As(err, &ge) || !strings.Contains(ge.Message, substr) {
		t.Fatalf("err = %v, want a *GitError containing %q", err, substr)
	}
}

func TestPlantedHooksFsmonitorAndDriversDoNotRunOnCommitOrMerge(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	marker := filepath.Join(t.TempDir(), "PWNED")
	for _, name := range []string{"pre-commit", "commit-msg", "post-commit", "post-merge", "pre-merge-commit"} {
		plantHook(t, filepath.Join(repo, ".git", "hooks", name), marker)
	}
	evilHooks := t.TempDir()
	plantHook(t, filepath.Join(evilHooks, "pre-commit"), marker)
	git(t, repo, "config", "core.hooksPath", evilHooks)
	git(t, repo, "config", "core.fsmonitor", "touch '"+marker+"'; echo")
	git(t, repo, "config", "filter.evil.clean", "sh -c \"touch '"+marker+"'; cat\"")
	git(t, repo, "config", "filter.evil.smudge", "sh -c \"touch '"+marker+"'; cat\"")
	git(t, repo, "config", "merge.evil.driver", "touch '"+marker+"'; false")
	git(t, repo, "config", "diff.evil.textconv", "sh -c \"touch '"+marker+"'; cat\"")
	writeFile(t, filepath.Join(repo, ".gitattributes"), "* filter=evil merge=evil diff=evil\n")

	writeFile(t, filepath.Join(repo, "a.txt"), "edited\n")
	must(CommitAll(ctx, repo, "edit"))
	must(RunGit(ctx, repo, "branch", "other"))
	must(RunGit(ctx, repo, "checkout", "-q", "other"))
	writeFile(t, filepath.Join(repo, "g.txt"), "on other\n")
	must(CommitAll(ctx, repo, "other"))
	must(RunGit(ctx, repo, "checkout", "-q", "main"))
	must(RunGit(ctx, repo, "merge", "--no-ff", "--no-commit", "other"))
	must(CommitAll(ctx, repo, "merged"))
	must(ShowAtHead(ctx, repo, "a.txt"))
	if err := ResetToHead(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if exists(marker) {
		t.Fatal("planted code ran during host-side git")
	}
	if log := must(LogOneline(ctx, repo, 1)); !strings.HasSuffix(log[0], "merged") {
		t.Fatal(log)
	}
}

func TestConfigIncludedFromTheWorkTreeIsRefused(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	git(t, repo, "config", "include.path", "../agent.cfg")
	writeFile(t, filepath.Join(repo, "agent.cfg"), "[core]\n\teditor = vi\n")
	_, err := CommitAll(ctx, repo, "x")
	wantGitError(t, err, "inside the work tree")
}

func TestConfigIncludedFromOutsideTheWorkTreeIsAllowed(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	cfg := filepath.Join(t.TempDir(), "operator.cfg")
	writeFile(t, cfg, "[core]\n\tabbrev = 12\n")
	git(t, repo, "config", "include.path", cfg)
	writeFile(t, filepath.Join(repo, "a.txt"), "again\n")
	if head := must(CommitAll(ctx, repo, "fine")); head == "" {
		t.Fatal("no commit")
	}
}

func TestHarnessArgvCarriesTheHardeningOverrides(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	git(t, repo, "config", "filter.Lfs.clean", "git-lfs clean -- %f")
	argv := must(GitArgv(ctx, repo, []string{"add", "-A"}))
	if !slices.Equal(argv[len(argv)-2:], []string{"add", "-A"}) {
		t.Fatal(argv)
	}
	var pairs []string
	for i, a := range argv {
		if a == "-c" {
			pairs = append(pairs, argv[i+1])
		}
	}
	for _, want := range []string{"core.hooksPath=" + os.DevNull, "core.fsmonitor=false", "filter.Lfs.clean="} {
		if !slices.Contains(pairs, want) {
			t.Errorf("missing -c %s in %v", want, pairs)
		}
	}
	if slices.Contains(must(GitArgv(ctx, repo, []string{"rev-parse", "HEAD"})), "filter.Lfs.clean=") {
		t.Fatal("plumbing should skip the config enumeration")
	}
	if extra := must(driverOverrides(ctx, t.TempDir())); len(extra) != 0 {
		t.Fatal(extra)
	}
}

func TestTamperedGitdirPointerIsRefusedBeforeGitRuns(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	wt := addWorktree(t, repo, "implementer-a")
	common := must(CommonDir(ctx, repo))
	if err := CheckGitLink(wt, common); err != nil {
		t.Fatal(err)
	}
	// The sandboxed agent plants a repository of its own, with a hook, and repoints .git at it.
	marker := filepath.Join(t.TempDir(), "PWNED")
	planted := filepath.Join(wt, "planted")
	source := filepath.Join(newRepo(t), ".git") // not repo/.git: the worktree lives inside it
	if out, err := exec.Command("cp", "-R", source, planted).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	plantHook(t, filepath.Join(planted, "hooks", "pre-commit"), marker)
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+planted+"\n")
	wantGitError(t, CheckGitLink(wt, common), "refusing to run git")
	_, err := CommitAll(ctx, wt, "evil")
	wantGitError(t, err, "inside the work tree")
	if exists(marker) {
		t.Fatal("planted hook ran")
	}
}

func TestPointerToAnotherRepositoryIsRefused(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	other := newRepo(t)
	wt := addWorktree(t, other, "implementer-b")
	if err := CheckGitLink(wt, ""); err != nil { // structurally a genuine worktree
		t.Fatal(err)
	}
	wantGitError(t, CheckGitLink(wt, must(CommonDir(ctx, repo))), "expected")
	wantGitError(t, CheckGitLink(repo, filepath.Join(other, ".git")), "expected")
	if err := CheckGitLink(repo, filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedPointersAreRefused(t *testing.T) {
	ctx := context.Background()
	for content, want := range map[string]string{
		"not a pointer\n":                    "single 'gitdir",
		"gitdir: \n":                         "empty gitdir",
		"gitdir: a\ngitdir: b\n":             "single 'gitdir",
		"gitdir: /nonexistent/lha/nowhere\n": "not a directory",
		strings.Repeat("x", 5000):            "too large",
	} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, ".git"), content)
		_, err := RunGit(ctx, dir, "status")
		wantGitError(t, err, want)
	}
	if _, err := ReadGitPointer(t.TempDir()); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatal(err)
	}
}

func TestStructurallyWrongWorktreeDirsAreRefused(t *testing.T) {
	repo := newRepo(t)
	wt := addWorktree(t, repo, "implementer-a")
	link, err := ValidateGitLink(wt, "")
	if err != nil {
		t.Fatal(err)
	}
	backlink := filepath.Join(link.GitDir, "gitdir")
	good, _ := os.ReadFile(backlink)
	writeFile(t, backlink, filepath.Join(t.TempDir(), ".git")+"\n")
	if _, err := ValidateGitLink(wt, ""); err == nil || !strings.Contains(err.Error(), "link back") {
		t.Fatal(err)
	}
	writeFile(t, backlink, string(good))
	fake := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fake, "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(link.GitDir, "commondir"), fake+"\n")
	if _, err := ValidateGitLink(wt, ""); err == nil || !strings.Contains(err.Error(), "not a worktree of") {
		t.Fatal(err)
	}
	bare, loose := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(loose, ".git"), "gitdir: "+bare+"\n")
	if _, err := ValidateGitLink(loose, ""); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatal(err)
	}
}

func TestSubmoduleStyleSeparateGitDirIsAccepted(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	sep := filepath.Join(t.TempDir(), "separate")
	if err := os.Rename(filepath.Join(repo, ".git"), sep); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, ".git"), "gitdir: "+sep+"\n")
	link, err := ValidateGitLink(repo, "")
	if err != nil || link.CommonDir != resolvePath(sep) {
		t.Fatal(link, err)
	}
	if head := must(HeadSHA(ctx, repo)); head == "" {
		t.Fatal("no HEAD")
	}
}

func TestSymlinkedDotGitIsRefused(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	link := t.TempDir()
	if err := os.Symlink(filepath.Join(repo, ".git"), filepath.Join(link, ".git")); err != nil {
		t.Fatal(err)
	}
	_, err := RunGit(ctx, link, "status")
	wantGitError(t, err, "symlink")
}

func TestGitDirsInTree(t *testing.T) {
	repo := newRepo(t)
	wt := addWorktree(t, repo, "implementer-a")
	if got := GitDirsInTree(wt); len(got) != 0 { // the git dir lives outside the work tree
		t.Fatal(got)
	}
	inner := t.TempDir()
	store := filepath.Join(inner, "store")
	if out, err := exec.Command("cp", "-R", filepath.Join(repo, ".git"), store).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	writeFile(t, filepath.Join(inner, ".git"), "gitdir: store\n")
	if got := GitDirsInTree(inner); !slices.Equal(got, []string{resolvePath(store)}) {
		t.Fatal(got)
	}
	if _, err := RunGit(context.Background(), inner, "status"); err == nil {
		t.Fatal("an in-tree git dir must be refused on the host")
	}
	garbage := t.TempDir()
	writeFile(t, filepath.Join(garbage, ".git"), "garbage")
	if got := GitDirsInTree(garbage); len(got) != 0 {
		t.Fatal(got)
	}
	if got := GitDirsInTree(t.TempDir()); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestOperatorGitEnvDoesNotLeakIntoHarnessGit(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	decoy := newRepo(t)
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "PWNED")
	evilHooks := filepath.Join(tmp, "evil-hooks")
	plantHook(t, filepath.Join(evilHooks, "pre-commit"), marker)
	globalCfg := filepath.Join(tmp, "global.cfg")
	writeFile(t, globalCfg, "[core]\n\thooksPath = "+evilHooks+"\n")
	writeFile(t, filepath.Join(tmp, ".gitconfig"), "[core]\n\thooksPath = "+evilHooks+"\n")
	decoyHead := must(HeadSHA(ctx, decoy))
	for k, v := range map[string]string{
		"GIT_DIR":               filepath.Join(decoy, ".git"),
		"GIT_WORK_TREE":         decoy,
		"GIT_INDEX_FILE":        filepath.Join(tmp, "index"),
		"GIT_CONFIG_GLOBAL":     globalCfg,
		"GIT_CONFIG_COUNT":      "1",
		"GIT_CONFIG_KEY_0":      "core.hooksPath",
		"GIT_CONFIG_VALUE_0":    evilHooks,
		"GIT_CONFIG_PARAMETERS": "'core.hooksPath'='" + evilHooks + "'",
		"HOME":                  tmp,
	} {
		t.Setenv(k, v)
	}
	allowed := map[string]bool{
		"GIT_TERMINAL_PROMPT": true, "GIT_ASKPASS": true, "GIT_CONFIG_NOSYSTEM": true,
		"GIT_CONFIG_GLOBAL": true, "GIT_ATTR_NOSYSTEM": true,
	}
	for _, kv := range gitEnv() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") && !allowed[k] {
			t.Errorf("leaked %s", kv)
		}
		if k == "GIT_CONFIG_GLOBAL" && v != os.DevNull || k == "HOME" && v == tmp {
			t.Errorf("unexpected %s", kv)
		}
	}
	writeFile(t, filepath.Join(repo, "a.txt"), "changed\n")
	head := must(CommitAll(ctx, repo, "in the right repo"))
	if head == "" || head == decoyHead {
		t.Fatal(head)
	}
	if log := must(LogOneline(ctx, repo, 1)); !strings.HasSuffix(log[0], "in the right repo") {
		t.Fatal(log)
	}
	if got := must(HeadSHA(ctx, decoy)); got != decoyHead {
		t.Fatal("the decoy repository was written")
	}
	if exists(marker) {
		t.Fatal("the operator-configured hook ran")
	}
}

func TestSafeHomeIsRecreatedWhenRemoved(t *testing.T) {
	if err := os.RemoveAll(safeHome()); err != nil {
		t.Fatal(err)
	}
	if !isDir(safeHome()) {
		t.Fatal("safe HOME not recreated")
	}
}
