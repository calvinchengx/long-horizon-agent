package persistence

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
)

// Run-path plumbing on top of Store (python: lha.persistence.tracking): the mission row and the
// persistent cost ledger.
//
//   - LedgerSink is installed as the CostMeter's hook: every metered model call's ledger entry is
//     written to cost_ledger under a deterministic idempotency key (keyPrefix + a per-sink
//     sequence number), so a replayed write is a no-op. Backfill writes entries recorded before
//     the sink existed (e.g. the Planner's call, made before the mission id was known).
//   - MissionTracker upserts the missions row as the run moves RUNNING -> DONE / ABORTED /
//     IMPOSSIBLE (StatusForStop maps a local runner's stop reason to the same terminal status the
//     durable workflow reports).
//
// Persistence is an observer of the run, never a reason to fail it: store errors are logged and
// counted (Failures), not returned into the agent loop.

// StatusForStop is the terminal missions.status for a local runner's stopped reason: complete ->
// DONE, deadlocked -> IMPOSSIBLE, budget / max cycles / loop / error -> ABORTED.
func StatusForStop(stoppedReason string) string {
	if stoppedReason == "complete" {
		return StatusDone
	}
	if strings.HasPrefix(stoppedReason, "deadlocked") {
		return StatusImpossible
	}
	return StatusAborted
}

// LedgerSink persists each CostEntry of a metered run to the store's cost_ledger. It implements
// governor.CostHook.
type LedgerSink struct {
	store     Store
	MissionID string
	prefix    string

	mu       sync.Mutex
	seq      int
	Written  int
	Failures int
}

var _ governor.CostHook = (*LedgerSink)(nil)

// NewLedgerSink is a sink for missionID whose call keys start with keyPrefix.
func NewLedgerSink(store Store, missionID, keyPrefix string) *LedgerSink {
	return &LedgerSink{store: store, MissionID: missionID, prefix: keyPrefix}
}

// RecordCost writes one entry (never returns an error: a failure is logged and counted).
func (s *LedgerSink) RecordCost(ctx context.Context, entry governor.CostEntry) error {
	s.mu.Lock()
	callKey := s.prefix + "#" + strconv.Itoa(s.seq)
	s.seq++
	s.mu.Unlock()
	written, err := s.store.RecordCost(ctx, s.MissionID, entry, callKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.Failures++
		logger().Warn("cost_ledger_write_failed", "mission_id", s.MissionID, "error", err.Error())
		return nil
	}
	if written {
		s.Written++
	}
	return nil
}

// Backfill writes entries recorded before the sink was attached.
func (s *LedgerSink) Backfill(ctx context.Context, entries []governor.CostEntry) {
	for _, e := range entries {
		_ = s.RecordCost(ctx, e)
	}
}

// Attach installs the sink as meter's hook.
func (s *LedgerSink) Attach(meter *governor.CostMeter) { meter.SetHook(s) }

// Counts is (written, failures).
func (s *LedgerSink) Counts() (written, failures int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Written, s.Failures
}

// MissionTracker keeps one missions row current for a run.
type MissionTracker struct {
	store       Store
	MissionID   string
	title       string
	description string
	workflowID  string
	Failures    int
}

// NewMissionTracker tracks missionID ("" workflowID = none).
func NewMissionTracker(store Store, missionID, title, description, workflowID string) *MissionTracker {
	return &MissionTracker{store: store, MissionID: missionID, title: title, description: description, workflowID: workflowID}
}

// SetStatus upserts the row (a terminal status stays unless reopen: see Store.UpsertMission).
// Errors are logged and counted, never returned.
func (t *MissionTracker) SetStatus(ctx context.Context, status, headSHA string, reopen bool) {
	err := t.store.UpsertMission(ctx, MissionUpsert{
		MissionID: t.MissionID, Title: t.title, Description: t.description, Status: status,
		HeadSHA: headSHA, WorkflowID: t.workflowID, Reopen: reopen,
	})
	if err != nil {
		t.Failures++
		logger().Warn("mission_upsert_failed", "mission_id", t.MissionID, "status", status, "error", err.Error())
	}
}

// Running marks the mission RUNNING.
func (t *MissionTracker) Running(ctx context.Context, headSHA string) {
	t.SetStatus(ctx, StatusRunning, headSHA, false)
}

// Finish records the terminal status for stoppedReason and returns it.
func (t *MissionTracker) Finish(ctx context.Context, stoppedReason, headSHA string) string {
	status := StatusForStop(stoppedReason)
	t.SetStatus(ctx, status, headSHA, false)
	return status
}
