package persistence

// The mission's shared event record: a run's trace events persisted to mission_events (python:
// lha.persistence.event_log). Every run path attaches one to its TraceRecorder, so any reader (lha
// serve, lha mission-report, SQL) can follow a run without its logs, whichever implementation
// runs it (docs/27-mission-ui.md). Events are stamped when recorded and written in order, in
// batches, about once a second while the run goes on and once more when it ends.
//
// Best effort: a store failure is logged and that batch is dropped (counted in Dropped); it never
// fails a run. At most MaxPendingEvents wait for a write; beyond that the oldest are dropped.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

const (
	// EventFlushEvery is how often pending events are written while a run goes on.
	EventFlushEvery = time.Second
	// MaxPendingEvents is the most events waiting for a write.
	MaxPendingEvents = 10_000
)

// MissionEventLog batches a run's trace events into mission_events; attach Add as a recorder
// listener.
type MissionEventLog struct {
	store      Store
	every      time.Duration
	maxPending int

	mu      sync.Mutex // guards pending, written, dropped
	pending []MissionEvent
	written int
	dropped int

	flushMu sync.Mutex // one flush at a time, so batches stay in order
	stop    chan struct{}
	done    chan struct{}
}

// NewMissionEventLog returns a log writing to store.
func NewMissionEventLog(store Store) *MissionEventLog {
	return &MissionEventLog{store: store, every: EventFlushEvery, maxPending: MaxPendingEvents}
}

// Add queues one recorded event (already redacted by the recorder), stamped now.
func (l *MissionEventLog) Add(e obs.TraceEvent) {
	payload := make(map[string]any, len(e.Data))
	for _, f := range e.Data {
		payload[f.Key] = f.Value
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = append(l.pending, MissionEvent{MissionID: e.MissionID, CycleID: e.CycleID, Kind: e.Kind, Payload: payload, TS: NowISO()})
	if excess := len(l.pending) - l.maxPending; excess > 0 {
		l.pending = append([]MissionEvent(nil), l.pending[excess:]...)
		l.dropped += excess
	}
}

// Flush writes everything pending, in order; a failure drops the batch and is logged.
func (l *MissionEventLog) Flush(ctx context.Context) {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	l.mu.Lock()
	batch := l.pending
	l.pending = nil
	l.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	err := l.store.AppendMissionEvents(ctx, batch)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.dropped += len(batch)
		obs.Logger("lha.persistence").Warn("mission_events_write_failed",
			"events", len(batch), "error", fmt.Sprintf("%s: %v", errorTypeName(err), err))
		return
	}
	l.written += len(batch)
}

// Start flushes every EventFlushEvery in the background until Close.
func (l *MissionEventLog) Start() {
	if l.stop != nil {
		return
	}
	l.stop, l.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(l.done)
		t := time.NewTicker(l.every)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
				l.Flush(context.Background())
			}
		}
	}()
}

// Close stops the background flush and writes what is left.
func (l *MissionEventLog) Close(ctx context.Context) {
	if l.stop != nil {
		close(l.stop)
		<-l.done
		l.stop = nil
	}
	l.Flush(ctx)
}

// Written is how many events reached the store.
func (l *MissionEventLog) Written() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written
}

// Dropped is how many events never will.
func (l *MissionEventLog) Dropped() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}
