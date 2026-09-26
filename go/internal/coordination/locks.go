package coordination

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Exclusive per-checkout locks (python: lha.state.locks): an flock on a file under .git/.
// Everything that changes a mission checkout's index or HEAD from outside the run's own sequence
// takes the same lock (CycleLock): a lease decision, a durable cycle attempt, a wave's
// integration. The lock file is shared with the Python implementation (flock interoperates).

// CycleLock is the lock file name under the repository's .git directory.
const CycleLock = "lha-cycle.lock"

// LockWait is how long WorkdirFlock waits for the lock before giving up.
var LockWait = 300 * time.Second

const lockPoll = 500 * time.Millisecond

// WorkdirBusyError is another holder keeping the checkout's lock longer than LockWait.
type WorkdirBusyError struct{ Message string }

func (e *WorkdirBusyError) Error() string { return e.Message }

// WorkdirFlock takes the exclusive lock .git/<name> of workdir's repository, polling until it is
// free (or LockWait passes: *WorkdirBusyError). The returned func releases it.
func WorkdirFlock(ctx context.Context, workdir, name string) (func(), error) {
	gitDir, err := state.GitDir(ctx, workdir)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(gitDir, name), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(LockWait)
	for {
		ok, err := tryLock(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if ok {
			return func() {
				unlock(f)
				_ = f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, &WorkdirBusyError{fmt.Sprintf("workdir %s is locked by another writer (%s)", workdir, name)}
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}
