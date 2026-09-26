package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
)

func isEscape(err error) bool {
	var e *PathEscapeError
	return errors.As(err, &e)
}

func TestNormalizeRelpath(t *testing.T) {
	for _, bad := range []string{"../x", "a/../../x", "/etc/passwd", `C:\x`, `\\srv\x`, "a\x00b"} {
		if _, err := NormalizeRelpath(bad); !isEscape(err) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for in, want := range map[string]string{"a/./b/../c.txt": "a/c.txt", "": ".", "./": ".", "a//b/": "a/b"} {
		if got, err := NormalizeRelpath(in); err != nil || got != want {
			t.Errorf("%q = %q, %v", in, got, err)
		}
	}
	if got, _ := ContainedPosix("/workspace", "src/x.py"); got != "/workspace/src/x.py" {
		t.Fatal(got)
	}
	if got, _ := ContainedPosix("/workspace", ""); got != "/workspace" {
		t.Fatal(got)
	}
	if _, err := ContainedPosix("/workspace", "../etc/passwd"); !isEscape(err) {
		t.Fatal(err)
	}
	_, err := NormalizeRelpath("../x")
	if err.Error() != "path escapes the workspace: '../x'" {
		t.Fatal(err)
	}
	_, err = NormalizeRelpath("/etc/passwd")
	if err.Error() != "absolute paths are not allowed: '/etc/passwd'" {
		t.Fatal(err)
	}
}

func TestIsProtected(t *testing.T) {
	for p, want := range map[string]bool{".lha/checklist.json": true, "./.git/hooks/pre-commit": true,
		".GIT/config": true, "src/.git_notes": false, ".": false} {
		if got, err := IsProtected(p); err != nil || got != want {
			t.Errorf("%q = %v, %v", p, got, err)
		}
	}
}

func openLocal(t *testing.T, dir string) contracts.SandboxSession {
	t.Helper()
	s, err := NewLocalSandbox().Open(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLocalSessionRejectsTraversal(t *testing.T) {
	tmp := t.TempDir()
	work := filepath.Join(tmp, "work")
	s := openLocal(t, work)
	ctx := context.Background()
	os.WriteFile(filepath.Join(tmp, "secret.txt"), []byte("s3cret"), 0o644)
	if _, err := s.ReadFile(ctx, "../secret.txt"); !isEscape(err) {
		t.Fatal(err)
	}
	if _, err := s.ReadFile(ctx, filepath.Join(tmp, "secret.txt")); !isEscape(err) {
		t.Fatal(err)
	}
	if err := s.WriteFile(ctx, "../pwned.txt", "x"); !isEscape(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "pwned.txt")); err == nil {
		t.Fatal("wrote outside")
	}
	if _, err := s.Exec(ctx, []string{"true"}, contracts.ExecOptions{Cwd: ".."}); !isEscape(err) {
		t.Fatal(err)
	}
	if _, err := s.Exec(ctx, []string{"true"}, contracts.ExecOptions{Cwd: "/etc"}); err == nil ||
		err.Error() != "cwd escapes the workspace: '/etc'" {
		t.Fatal(err)
	}
}

func TestLocalSessionRejectsSymlinkEscape(t *testing.T) {
	tmp := t.TempDir()
	work := filepath.Join(tmp, "work")
	outside := filepath.Join(tmp, "outside")
	os.Mkdir(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s3cret"), 0o644)
	s := openLocal(t, work)
	os.Symlink(outside, filepath.Join(work, "link"))
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(work, "file_link"))
	ctx := context.Background()
	for _, p := range []string{"link/secret.txt", "file_link"} {
		if _, err := s.ReadFile(ctx, p); !isEscape(err) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := s.WriteFile(ctx, "link/new.txt", "x"); !isEscape(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("wrote outside")
	}
}

func TestResolveWithinAllowsInnerSymlink(t *testing.T) {
	tmp := t.TempDir()
	os.Mkdir(filepath.Join(tmp, "real"), 0o755)
	os.Symlink(filepath.Join(tmp, "real"), filepath.Join(tmp, "al"))
	got, err := ResolveWithin(tmp, "al/x")
	if err != nil || got != filepath.Join(Realpath(tmp), "real", "x") {
		t.Fatal(got, err)
	}
	protected, err := IsProtectedResolved(tmp, "al/x")
	if err != nil || protected {
		t.Fatal(protected, err)
	}
	os.Mkdir(filepath.Join(tmp, ".git"), 0o755)
	os.Symlink(filepath.Join(tmp, ".git"), filepath.Join(tmp, "innocent"))
	if protected, err := IsProtectedResolved(tmp, "innocent/config"); err != nil || !protected {
		t.Fatal(protected, err)
	}
}

func TestRealpathMatchesPython(t *testing.T) {
	tmp := Realpath(t.TempDir())
	os.Symlink("loop2", filepath.Join(tmp, "loop1"))
	os.Symlink("loop1", filepath.Join(tmp, "loop2"))
	// A loop leaves the rest unresolved (python: realpath(strict=False)).
	if got := Realpath(tmp + "/loop1/x"); got != tmp+"/loop1/x" {
		t.Fatal(got)
	}
	if _, err := ResolveWithin(tmp, "loop1"); err == nil || !strings.HasPrefix(err.Error(), "Symlink loop from ") {
		t.Fatal(err)
	}
	if got := Realpath(tmp + "/missing/../y"); got != tmp+"/y" {
		t.Fatal(got)
	}
}

func inThread(t *testing.T, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("file tool blocked on a FIFO")
	}
	return nil
}

func TestFIFOsNeverBlock(t *testing.T) {
	tmp := t.TempDir()
	if err := mkfifo(filepath.Join(tmp, "pipe"), 0o644); err != nil {
		t.Skip("no FIFOs")
	}
	err := inThread(t, func() error { _, err := ReadTextWithin(tmp, "pipe"); return err })
	if err == nil || err.Error() != "not a regular file: 'pipe'" {
		t.Fatal(err)
	}
	err = inThread(t, func() error { _, err := WriteTextWithin(tmp, "pipe", "x"); return err })
	if err == nil || err.Error() != "not a regular file: 'pipe'" {
		t.Fatal(err)
	}
	if _, err := WriteTextWithin(tmp, "a/b.txt", "hello"); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadTextWithin(tmp, "a/b.txt"); err != nil || got != "hello" {
		t.Fatal(got, err)
	}
}

func TestReadWriteErrorsMatchPython(t *testing.T) {
	tmp := Realpath(t.TempDir())
	_, err := ReadTextWithin(tmp, "nope.txt")
	want := "[Errno 2] No such file or directory: '" + tmp + "/nope.txt'"
	if got := osText(err); got != want {
		t.Fatalf("%q != %q", got, want)
	}
	os.Mkdir(filepath.Join(tmp, "d"), 0o755)
	if _, err := ReadTextWithin(tmp, "d"); err == nil || err.Error() != "not a regular file: 'd'" {
		t.Fatal(err)
	}
	if _, err := WriteTextWithin(tmp, ".", "x"); err == nil || err.Error() != "cannot write to the workspace root: '.'" {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(tmp, "bad.txt"), []byte("ab\xff"), 0o644)
	if _, err := ReadTextWithin(tmp, "bad.txt"); err == nil ||
		err.Error() != "'utf-8' codec can't decode byte 0xff in position 2: invalid start byte" {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(tmp, "f"), []byte("x"), 0o644)
	_, err = WriteTextWithin(tmp, "f/g.txt", "x")
	if got := osText(err); got != "[Errno 17] File exists: '"+tmp+"/f'" {
		t.Fatal(got)
	}
}

func osText(err error) string { return pyval.OSErrorText(err) }
