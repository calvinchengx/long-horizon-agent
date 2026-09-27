package org

import (
	"context"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/hitl"
	"github.com/calvinchengx/long-horizon-agent/go/internal/memory"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence/services"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
)

// What a run persists besides the anchor (python: lha.persistence.services.open_run_services):
// the mission row, the persistent cost ledger (a sink on the meter) and tiered memory. The
// orchestrator talks to it through this small interface; DefaultServices opens the persistence
// plane (PersistentServices), and OrchestratorOptions.Services can replace it (NoopServices
// persists nothing).

// ServicesRequest is what a ServicesOpener gets (python: open_run_services' arguments).
type ServicesRequest struct {
	Settings    *config.Settings
	MissionID   string
	Workdir     string
	Meter       *governor.CostMeter
	Title       string
	Description string
	// Model is the librarian's provider (the lead's model, metered as "librarian").
	Model    contracts.ModelProvider
	Recorder *obs.TraceRecorder
	// KeyPrefix keeps a resumed run's ledger keys apart ("" on the first run, "run<N>" after).
	KeyPrefix string
	// Gate is the run's human gate, for services that record its events (python:
	// bind_gate_store); nil when there is none.
	Gate contracts.HITLGate
	// SystemOne serves memory_rerank=system_one; the orchestrator owns it.
	SystemOne systemone.Model
}

// RunServices is one run's persistence (python: RunServices).
type RunServices interface {
	// Running marks the mission row running (python: tracker.running()).
	Running(ctx context.Context) error
	// Finish records how the run ended (python: services.finish(stopped_reason, head_sha=)).
	Finish(ctx context.Context, stoppedReason, headSHA string) error
	// Close releases the store and memory and detaches the ledger sink.
	Close(ctx context.Context) error
	// Memory is the lead's tiered memory (python: services.memory); nil when there is none.
	Memory() memory.CycleMemory
}

// ServicesOpener opens a run's services.
type ServicesOpener func(ctx context.Context, req ServicesRequest) (RunServices, error)

// NoopServices persists nothing.
type NoopServices struct{}

// Running implements RunServices.
func (NoopServices) Running(context.Context) error { return nil }

// Finish implements RunServices.
func (NoopServices) Finish(context.Context, string, string) error { return nil }

// Close implements RunServices.
func (NoopServices) Close(context.Context) error { return nil }

// Memory implements RunServices.
func (NoopServices) Memory() memory.CycleMemory { return nil }

// DefaultServices is the opener an Orchestrator uses when its options name none: the persistence
// plane (mission row, persistent cost ledger for every org role, tiered memory for the lead).
var DefaultServices ServicesOpener = PersistentServices

// PersistentServices opens the run's store, mission row, ledger sink and memory (python:
// open_run_services) and points a gate that records its events at the store (python:
// bind_gate_store), so `lha missions`, `lha costs` and `lha gates` see the orchestrated run.
func PersistentServices(ctx context.Context, req ServicesRequest) (RunServices, error) {
	svc, err := services.Open(ctx, req.Settings, services.Request{
		MissionID: req.MissionID, Workdir: req.Workdir, Meter: req.Meter,
		Title: req.Title, Description: req.Description, Model: req.Model,
		Recorder: req.Recorder, KeyPrefix: req.KeyPrefix, SystemOne: req.SystemOne,
	})
	if err != nil {
		return nil, err
	}
	if binder, ok := req.Gate.(interface{ BindStore(hitl.GateRecorder) }); ok {
		binder.BindStore(svc.Store)
	}
	return persistentServices{svc}, nil
}

type persistentServices struct{ svc *services.RunServices }

func (p persistentServices) Running(ctx context.Context) error {
	p.svc.Tracker.Running(ctx, "")
	return nil
}

func (p persistentServices) Finish(ctx context.Context, stoppedReason, headSHA string) error {
	p.svc.Finish(ctx, stoppedReason, headSHA)
	return nil
}

func (p persistentServices) Close(context.Context) error { return p.svc.Close() }

func (p persistentServices) Memory() memory.CycleMemory { return p.svc.CycleMemory() }
