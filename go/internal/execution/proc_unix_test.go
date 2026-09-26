//go:build unix

package execution

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "child.pid")
	script := "sleep 60 & echo $! > " + pidFile + "; sleep 60"
	started := time.Now()
	res, err := RunProc(context.Background(), []string{"sh", "-c", script}, tmp, ProcOptions{TimeoutS: 2})
	if err != nil || !res.TimedOut || res.OK() || res.ExitCode != -1 || !strings.HasSuffix(res.Stderr, "\n[timed out after 2s]") {
		t.Fatal(res, err)
	}
	if time.Since(started) > 20*time.Second {
		t.Fatal("too slow")
	}
	raw, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	for i := 0; i < 50; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("grandchild survived the timeout")
}
