// Package services opens everything a run path persists, together: store + mission row + cost
// ledger + memory (python: lha.persistence.services).
//
// Open is what `lha run-local` / `mission` (the local runner) and the durable activities call:
//
//	svc, err := services.Open(ctx, settings, services.Request{MissionID: id, Workdir: dir, Meter: meter, ...})
//	defer svc.Close()
//	svc.Tracker.Running(ctx, "")
//	loop := ... with svc.CycleMemory() ...
//	svc.Finish(ctx, stoppedReason, head) // local runners only
//
// It opens the configured persistence.Store (SQLite unless LHA_POSTGRES_DSN), installs a
// LedgerSink on the meter so EVERY metered call lands in cost_ledger (Backfill also writes calls
// the meter recorded before the mission id existed, e.g. the Planner's), and opens the memory
// plane when memory_enabled.
package services

import (
	"context"
	"errors"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/memory"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
)

// Request is Open's input.
type Request struct {
	MissionID   string
	Workdir     string
	Meter       *governor.CostMeter
	Title       string
	Description string
	// Model is the metered model "model" consolidation uses (the librarian role).
	Model    contracts.ModelProvider
	Recorder *obs.TraceRecorder
	// KeyPrefix prefixes the ledger call keys (a durable activity's attempt-stable prefix).
	KeyPrefix string
	// SkipBackfill leaves out entries the meter recorded before the sink was attached.
	SkipBackfill bool
	WorkflowID   string
	// Memory are test seams for the memory plane.
	Memory memory.OpenOptions
	// SystemOne (systemone.Build, metered by Meter) serves memory reranking; the returned
	// services own it and close it.
	SystemOne systemone.Model
}

// RunServices is a run's store, mission row, persistent ledger and memory.
type RunServices struct {
	Store     persistence.Store
	Tracker   *persistence.MissionTracker
	Sink      *persistence.LedgerSink
	Memory    *memory.MissionMemory // nil when memory is disabled
	Meter     *governor.CostMeter
	SystemOne systemone.Model // nil when system_one_backend=off
}

// CycleMemory is the memory plane as the agent loop sees it (nil when disabled).
func (s *RunServices) CycleMemory() memory.CycleMemory {
	if s.Memory == nil {
		return nil
	}
	return s.Memory
}

// Finish records the terminal status for stoppedReason (local runners).
func (s *RunServices) Finish(ctx context.Context, stoppedReason, headSHA string) string {
	return s.Tracker.Finish(ctx, stoppedReason, headSHA)
}

// Close detaches the ledger sink, closes memory and the store.
func (s *RunServices) Close() error {
	if s.Meter != nil && s.Meter.Hook() == governor.CostHook(s.Sink) {
		s.Meter.SetHook(nil)
	}
	var memErr error
	if s.Memory != nil {
		memErr = s.Memory.Close()
	}
	return errors.Join(memErr, systemone.Close(s.SystemOne), s.Store.Close())
}

// Open opens the store, hooks the meter to the persistent ledger, and opens memory. A
// *persistence.StoreUnavailableError means the configured store is unusable (and fallback is
// off).
func Open(ctx context.Context, settings *config.Settings, r Request) (*RunServices, error) {
	store, err := persistence.OpenStore(ctx, settings, r.Workdir)
	if err != nil {
		_ = systemone.Close(r.SystemOne)
		return nil, err
	}
	sink := persistence.NewLedgerSink(store, r.MissionID, r.KeyPrefix)
	if r.Meter != nil {
		if !r.SkipBackfill {
			sink.Backfill(ctx, r.Meter.Ledger.Entries())
		}
		sink.Attach(r.Meter)
	}
	tracker := persistence.NewMissionTracker(store, r.MissionID, r.Title, r.Description, r.WorkflowID)
	opts := r.Memory
	opts.Model, opts.Recorder, opts.SystemOne = r.Model, r.Recorder, r.SystemOne
	mem := memory.OpenMissionMemory(ctx, settings, store, r.Workdir, r.MissionID, opts)
	return &RunServices{Store: store, Tracker: tracker, Sink: sink, Memory: mem, Meter: r.Meter, SystemOne: r.SystemOne}, nil
}
