package execution

// A linked worktree's .git is a FILE; it (and any git dir it names inside the work tree) must be
// bound read-only, or sandboxed code could repoint the host's next commit at a planted repo.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// linkedWorktree is a repo plus one worktree at <repo>/.git/lha-worktrees/<name>, the layout
// lha.agents.integrator.add_worktree uses.
func linkedWorktree(t *testing.T) (repo, wt string) {
	t.Helper()
	ctx := context.Background()
	repo = t.TempDir()
	if err := state.InitRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := state.CommitAll(ctx, repo, "base"); err != nil {
		t.Fatal(err)
	}
	wt = filepath.Join(repo, ".git", "lha-worktrees", "implementer-a")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := state.RunGit(ctx, repo, "worktree", "add", "--quiet", "-B", "lha/implementer-a", wt, "HEAD"); err != nil {
		t.Fatal(err)
	}
	return repo, wt
}

func TestLinkedWorktreeGitFileIsMountedReadOnly(t *testing.T) {
	_, wt := linkedWorktree(t)
	if st, err := os.Stat(filepath.Join(wt, ".git")); err != nil || !st.Mode().IsRegular() {
		t.Fatal("expected a .git file", err)
	}
	sb, err := NewDockerSandbox(DockerOptions{CLI: &fakeDocker{}})
	if err != nil {
		t.Fatal(err)
	}
	host := Realpath(wt)
	got := flagValues(sb.RunArgs("img", wt, "", nil), "-v")
	want := []string{host + ":/workspace:rw", host + "/.git:/workspace/.git:ro"}
	if !slices.Equal(got, want) {
		t.Fatalf("-v = %v, want %v", got, want)
	}
}

func TestGitDirInsideTheMountedTreeIsMountedReadOnly(t *testing.T) {
	src := t.TempDir()
	if err := state.InitRepo(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	if out, err := exec.Command("cp", "-R", filepath.Join(src, ".git"), filepath.Join(wt, "store")).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, _ := NewDockerSandbox(DockerOptions{CLI: &fakeDocker{}})
	host := Realpath(wt)
	got := flagValues(sb.RunArgs("img", wt, "", nil), "-v")
	want := []string{host + ":/workspace:rw", host + "/.git:/workspace/.git:ro", host + "/store:/workspace/store:ro"}
	if !slices.Equal(got, want) {
		t.Fatalf("-v = %v, want %v", got, want)
	}
}

func TestDocker_IT_LinkedWorktreeGitFileIsReadOnlyInsideTheContainer(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	repo, wt := linkedWorktree(t)
	pointer, err := os.ReadFile(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	sb, err := NewDockerSandbox(DockerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := sb.Open(ctx, wt, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"sh", "-c", "echo 'gitdir: /workspace/planted' > .git"},
		{"python", "-c", "open('.git', 'w').write('gitdir: /workspace/planted')"},
		{"sh", "-c", "rm -f .git"},
		{"sh", "-c", "mv .git moved"},
	} {
		res, err := s.Exec(ctx, argv, contracts.ExecOptions{})
		if err != nil || res.OK() {
			t.Errorf("%v succeeded: %v %v", argv, res, err)
		}
	}
	if res, err := s.Exec(ctx, []string{"sh", "-c", "echo x > scratch.txt"}, contracts.ExecOptions{}); err != nil || !res.OK() {
		t.Error("the work tree itself must stay writable", res, err)
	}
	if err := s.Close(ctx); err != nil {
		t.Error(err)
	}
	after, err := os.ReadFile(filepath.Join(wt, ".git"))
	if err != nil || string(after) != string(pointer) {
		t.Fatalf(".git changed: %q -> %q (%v)", pointer, after, err)
	}
	common, err := state.CommonDir(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.CheckGitLink(wt, common); err != nil {
		t.Fatal(err)
	}
}
