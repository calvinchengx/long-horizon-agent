//go:build unix

package execution

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

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
	if err := syscall.Mkfifo(filepath.Join(tmp, "pipe"), 0o644); err != nil {
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
