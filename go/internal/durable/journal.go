package durable

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// The activity side's durable bookkeeping, shared with the Python worker byte-for-byte where it
// touches disk (python/src/lha/durable/activities.py, lha.state.locks):
//
//   - the per-checkout lock: flock on .git/lha-cycle.lock (the same file and lock Python takes),
//     so a Go and a Python attempt can never mutate one checkout at once;
//   - the spend journal .git/lha/spend.ndjson: one line per attempt, keyed so a replayed append
//     is idempotent (readers keep the last row per key), outside the worktree so reset keeps it;
//   - the exactly-once check: HEAD's .lha/events.ndjson already holding this cycle id's event.

const (
	// CycleLock is the lock file name under .git/ (python: CYCLE_LOCK).
	CycleLock = "lha-cycle.lock"
	// LockWait is how long an attempt waits for the checkout's lock (python: LOCK_WAIT_S).
	LockWait     = 300 * time.Second
	lockPoll     = 500 * time.Millisecond
	spendFile    = "lha/spend.ndjson"
	recentEvents = 256
	priorCycle   = "(prior)"
)

// WorkdirBusyError: another holder kept the checkout's lock for longer than the wait allowed.
type WorkdirBusyError struct{ Workdir, Name string }

func (e *WorkdirBusyError) Error() string {
	return fmt.Sprintf("workdir %s is locked by another writer (%s)", e.Workdir, e.Name)
}

// WorkdirLock holds the exclusive lock .git/<name> of workdir's repository until release is
// called. It polls without blocking the lock (onWait runs on every poll, e.g. a heartbeat) and
// fails with *WorkdirBusyError after wait.
func WorkdirLock(ctx context.Context, workdir, name string, wait time.Duration, onWait func()) (release func(), err error) {
	gitDir, err := state.GitDir(ctx, workdir)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(gitDir, name), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	var waited time.Duration
	for {
		ok, err := flockFile(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			break
		}
		if waited >= wait {
			f.Close()
			return nil, &WorkdirBusyError{Workdir: workdir, Name: name}
		}
		if onWait != nil {
			onWait()
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
		waited += lockPoll
	}
	return func() {
		_ = funlockFile(f)
		f.Close()
	}, nil
}

func spendPath(ctx context.Context, workdir string) (string, error) {
	gitDir, err := state.GitDir(ctx, workdir)
	if err != nil {
		return "", err
	}
	return filepath.Join(gitDir, filepath.FromSlash(spendFile)), nil
}

// ReadPriorSpend is (known USD, unknown-cost call count) over every recorded attempt of the
// mission checked out at workdir (python: read_prior_spend). Torn or malformed lines are skipped.
func ReadPriorSpend(ctx context.Context, workdir string) (float64, int, error) {
	path, err := spendPath(ctx, workdir)
	if err != nil {
		return 0, 0, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	type row struct {
		usd     float64
		unknown int
	}
	byKey := map[string]row{}
	var order []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var raw map[string]any
		dec := json.NewDecoder(strings.NewReader(scanner.Text()))
		dec.UseNumber()
		if dec.Decode(&raw) != nil {
			continue
		}
		key, ok1 := pyStrValue(raw["key"])
		usd, ok2 := pyFloatValue(raw["usd"])
		unknown, ok3 := pyIntValue(raw["unknown"])
		if !ok1 || !ok2 || !ok3 {
			continue
		}
		if _, seen := byKey[key]; !seen {
			order = append(order, key)
		}
		byKey[key] = row{usd, unknown}
	}
	var usd float64
	unknown := 0
	for _, k := range order { // Python sums in first-insertion order
		usd += byKey[k].usd
		unknown += byKey[k].unknown
	}
	return usd, unknown, nil
}

func pyStrValue(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		if x {
			return "True", true
		}
		return "False", true
	}
	return "", false
}

func pyFloatValue(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func pyIntValue(v any) (int, bool) {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i), true
		}
		f, err := x.Float64()
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, false
		}
		return int(f), true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(x))
		return i, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// pyFloat renders f like Python's float repr (json.dumps of a float).
func pyFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if strings.ContainsAny(s, ".eIN") {
		// Python writes 1e-05 / 1e+16 with a two-digit exponent, like Go.
		return s
	}
	return s + ".0"
}

// ownEntries are the ledger entries this attempt recorded (not the seeded prior spend).
func ownEntries(ledger *governor.CostLedger) []governor.CostEntry {
	var own []governor.CostEntry
	for _, e := range ledger.Entries() {
		if e.CycleID != priorCycle {
			own = append(own, e)
		}
	}
	return own
}

// RecordSpend appends one attempt's spend (idempotent per key: readers keep the last row per key)
// (python: record_spend). Nothing is written when the attempt made no metered call.
func RecordSpend(ctx context.Context, workdir, key, cycleID string, ledger *governor.CostLedger) error {
	own := ownEntries(ledger)
	if len(own) == 0 {
		return nil
	}
	path, err := spendPath(ctx, workdir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var usd float64
	unknown := 0
	for _, e := range own {
		usd += e.USD
		if !e.CostKnown {
			unknown++
		}
	}
	k, _ := json.Marshal(key)
	c, _ := json.Marshal(cycleID)
	line := fmt.Sprintf(`{"key": %s, "cycle_id": %s, "usd": %s, "unknown": %d, "calls": %d}`+"\n",
		k, c, pyFloat(usd), unknown, len(own))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// BuildCycleMeter is a meter whose ledger is seeded with the mission's prior spend and whose
// ceiling is the mission budget (budgetUSD, else LHA_BUDGET_USD_CEILING) (python:
// build_cycle_meter).
func BuildCycleMeter(ctx context.Context, settings *config.Settings, workdir, cycleID string, budgetUSD *float64, maxCycles int) (*governor.CostMeter, error) {
	ledger := governor.NewCostLedger()
	priorUSD, priorUnknown, err := ReadPriorSpend(ctx, workdir)
	if err != nil {
		return nil, err
	}
	prior := contracts.Usage{Model: priorCycle}
	if priorUSD != 0 {
		usd := priorUSD
		ledger.Record(priorCycle, prior, &usd, "")
	}
	for range priorUnknown {
		ledger.Record(priorCycle, prior, nil, "")
	}
	ceiling := settings.BudgetUSDCeiling
	if budgetUSD != nil {
		ceiling = *budgetUSD
	}
	meter := governor.NewCostMeter(ledger, governor.NewBudgetGovernor(ceiling, maxCycles, settings.AllowUnpricedModels))
	meter.SetCycleID(cycleID)
	return meter, nil
}

// CommittedCycleEvent is the payload of cycleID's checkpoint event (of kind) if HEAD's
// .lha/events.ndjson contains it among its last 256 lines (python: committed_cycle_event).
func CommittedCycleEvent(ctx context.Context, workdir, cycleID, kind string) (map[string]any, bool) {
	raw, err := state.RunGit(ctx, workdir, "show", "HEAD:"+state.AnchorDir+"/"+state.EventsFile)
	if err != nil {
		return nil, false
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > recentEvents {
		lines = lines[len(lines)-recentEvents:]
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var ev contracts.EventRecord
		if json.Unmarshal([]byte(lines[i]), &ev) != nil {
			continue
		}
		if ev.Kind == kind && ev.CycleID == cycleID {
			if ev.Payload == nil {
				ev.Payload = map[string]any{}
			}
			return ev.Payload, true
		}
	}
	return nil, false
}
