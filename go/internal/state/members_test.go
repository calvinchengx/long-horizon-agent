package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Multi-repo workspaces (python: test_multi_repo.py): members are committed inside first, resets
// reach into them, the review diff expands their changes, and a pointer that does not link back is
// refused.

func upstream(t *testing.T, path, name string) string {
	t.Helper()
	ctx := context.Background()
	if err := InitRepo(ctx, path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(path, "README.md"), "# "+name+"\n")
	writeFile(t, filepath.Join(path, "tests", "test_a.py"), "def test_a():\n    assert True\n")
	must(CommitAll(ctx, path, "init"))
	return path
}

func workspace(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	svc := upstream(t, filepath.Join(root, "upstream", "svc.git"), "svc")
	lib := upstream(t, filepath.Join(root, "upstream", "lib"), "lib")
	ws := filepath.Join(root, "ws")
	if err := InitRepo(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := AddMember(ctx, ws, svc, "svc", ""); err != nil {
		t.Fatal(err)
	}
	if err := AddMember(ctx, ws, lib, "lib", "main"); err != nil {
		t.Fatal(err)
	}
	must(CommitAll(ctx, ws, "lha: workspace members svc, lib"))
	return ws
}

func TestMemberPathsAndWorkspace(t *testing.T) {
	ctx := context.Background()
	ws := workspace(t)
	members, err := MemberPaths(ctx, ws)
	if err != nil || strings.Join(members, ",") != "lib,svc" {
		t.Fatalf("%v %v", members, err)
	}
	if !IsWorkspace(ctx, ws) || IsWorkspace(ctx, filepath.Join(ws, "lib")) {
		t.Fatal("workspace detection")
	}
	if !isFile(filepath.Join(ws, "svc", ".git")) {
		t.Fatal("member .git is not a pointer file")
	}
}

func TestCommitAllCommitsInsideMembersFirst(t *testing.T) {
	ctx := context.Background()
	ws := workspace(t)
	before := must(HeadSHA(ctx, ws))
	svcBefore := must(HeadSHA(ctx, filepath.Join(ws, "svc")))
	writeFile(t, filepath.Join(ws, "svc", "app.py"), "x = 1\n")
	writeFile(t, filepath.Join(ws, "lib", "README.md"), "# lib\nmore\n")
	writeFile(t, filepath.Join(ws, "NOTES.md"), "workspace note\n")
	sha := must(CommitAll(ctx, ws, "lha: checkpoint c1"))
	if sha == before || must(HeadSHA(ctx, ws)) != sha {
		t.Fatal("no workspace commit")
	}
	for _, m := range []string{"svc", "lib"} {
		lines, _ := LogOneline(ctx, filepath.Join(ws, m), 1)
		if len(lines) != 1 || !strings.HasSuffix(lines[0], "lha: checkpoint c1") {
			t.Fatalf("%s log %v", m, lines)
		}
	}
	if must(HeadSHA(ctx, filepath.Join(ws, "svc"))) == svcBefore {
		t.Fatal("svc not committed")
	}
	if link := must(RunGit(ctx, ws, "rev-parse", "HEAD:svc")); link != must(HeadSHA(ctx, filepath.Join(ws, "svc"))) {
		t.Fatal("gitlink not updated")
	}
	if status := must(RunGit(ctx, ws, "status", "--porcelain")); status != "" {
		t.Fatalf("dirty: %q", status)
	}
	if again := must(CommitAll(ctx, ws, "lha: checkpoint c2")); again != sha {
		t.Fatal("a no-op commit moved HEAD")
	}
	diff := DiffRange(ctx, ws, before, sha, ".lha")
	for _, want := range []string{"+workspace note", "+++ b/svc/app.py", "+x = 1", "+++ b/lib/README.md", "+more", "Subproject commit"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff lacks %q:\n%s", want, diff)
		}
	}
	if strings.Contains(diff, "--- a/svc/app.py") {
		t.Fatal("a new file has no old side")
	}
}

func TestResetAndDiscardReachIntoMembers(t *testing.T) {
	ctx := context.Background()
	ws := workspace(t)
	writeFile(t, filepath.Join(ws, "svc", "README.md"), "edited\n")
	writeFile(t, filepath.Join(ws, "svc", "untracked.txt"), "u\n")
	writeFile(t, filepath.Join(ws, "lib", "build.out"), "ignored?\n")
	writeFile(t, filepath.Join(ws, "lib", ".gitignore"), "build.out\n")
	if err := DiscardChanges(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "svc", "README.md")); string(data) != "# svc\n" {
		t.Fatalf("README %q", data)
	}
	if _, err := os.Stat(filepath.Join(ws, "svc", "untracked.txt")); err == nil {
		t.Fatal("untracked file survived discard")
	}
	if _, err := os.Stat(filepath.Join(ws, "lib", "build.out")); err != nil {
		t.Fatal("discard removed an ignored file")
	}
	writeFile(t, filepath.Join(ws, "svc", "README.md"), "edited again\n")
	writeFile(t, filepath.Join(ws, "lib", ".venv", "bin"), "keep\n")
	if err := ResetToHead(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "svc", "README.md")); string(data) != "# svc\n" {
		t.Fatalf("README %q", data)
	}
	if _, err := os.Stat(filepath.Join(ws, "lib", "build.out")); err == nil {
		t.Fatal("a full reset kept an ignored file")
	}
	if _, err := os.Stat(filepath.Join(ws, "lib", ".venv", "bin")); err != nil {
		t.Fatal("a full reset removed the dependency environment")
	}
	writeFile(t, filepath.Join(ws, "svc", "extra.txt"), "e\n")
	must(RunGit(ctx, filepath.Join(ws, "svc"), "add", "-A"))
	must(RunGit(ctx, filepath.Join(ws, "svc"), "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "-m", "stray"))
	recorded := must(RunGit(ctx, ws, "rev-parse", "HEAD:svc"))
	if err := ResetToHead(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if must(HeadSHA(ctx, filepath.Join(ws, "svc"))) != recorded {
		t.Fatal("member not brought back to the recorded commit")
	}
	if _, err := os.Stat(filepath.Join(ws, "svc", "extra.txt")); err == nil {
		t.Fatal("stray commit's file survived")
	}
}

func TestAMemberPointerThatDoesNotLinkBackIsRefused(t *testing.T) {
	ctx := context.Background()
	ws := workspace(t)
	pointer := filepath.Join(ws, "svc", ".git")
	original, _ := os.ReadFile(pointer)
	must(RunGit(ctx, filepath.Join(ws, "svc"), "status"))
	for bad, want := range map[string]string{
		"gitdir: ../.git\n":             "is the enclosing repository",
		"gitdir: ../.git/modules/lib\n": "does not link back",
	} {
		_ = os.WriteFile(pointer, []byte(bad), 0o644)
		if _, err := RunGit(ctx, filepath.Join(ws, "svc"), "status"); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
	_ = os.WriteFile(pointer, original, 0o644)
	must(RunGit(ctx, filepath.Join(ws, "svc"), "status"))
}

func TestSnapshotListsMembers(t *testing.T) {
	ctx := context.Background()
	ws := workspace(t)
	anchor := NewGitMissionAnchor(ws)
	if _, err := anchor.Initialize(ctx, "t", "d", oneItem()); err != nil {
		t.Fatal(err)
	}
	snap, err := anchor.ReadSituationalAwareness(ctx)
	if err != nil || strings.Join(snap.Members, ",") != "lib,svc" {
		t.Fatalf("%v %v", snap.Members, err)
	}
}
