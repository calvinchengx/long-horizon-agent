package hitl

import (
	"context"
	"log/slog"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
)

// The terminal approver's hitl_gates rows (python: TerminalApprover.bind_store / _gate_event):
// every gate event (opened, reminder, resolved, defaulted) of an attended local run is written to
// the run's mission store, so `lha gates` lists it like a durable mission's gates.

// GateRecorder is where gate events go (persistence.Store satisfies it).
type GateRecorder interface {
	RecordGateEvent(ctx context.Context, event persistence.GateEvent) error
}

// BindStore records every gate event in store's hitl_gates table (nil stops it).
func (a *TerminalApprover) BindStore(store GateRecorder) {
	a.storeMu.Lock()
	defer a.storeMu.Unlock()
	a.store = store
}

// StoreFailures is the number of gate-row writes that failed (logged; never a reason to change
// the answer).
func (a *TerminalApprover) StoreFailures() int {
	a.storeMu.Lock()
	defer a.storeMu.Unlock()
	return a.storeFailures
}

// terminalUser is python's getpass.getuser() ("unknown" when it cannot be read).
func terminalUser() string {
	for _, key := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

// GateEventFor is the hitl_gates event for req (python: TerminalApprover._gate_event).
func GateEventFor(req contracts.GateRequest, event, decision, by string, step int) persistence.GateEvent {
	missionID := req.Context["mission_id"]
	if missionID == "" {
		missionID, _, _ = strings.Cut(req.GateID, ":tool:")
	}
	request := map[string]string{}
	for _, key := range []string{"fingerprint", "tool", "arguments", "reason"} {
		if v := req.Context[key]; v != "" {
			if key == "arguments" {
				v = obs.RedactText(v)
			}
			request[key] = v
		}
	}
	if argv := RequestArgv(req); len(argv) > 0 {
		redacted := make([]string, len(argv))
		for i, t := range argv {
			redacted[i] = obs.RedactText(t)
		}
		request["argv"] = persistence.PyDumps(redacted)
	}
	if len(request) == 0 {
		request = nil
	}
	return persistence.GateEvent{
		MissionID: missionID, GateID: req.GateID, Kind: "tool_call", Event: event,
		At: persistence.ISOMicro(time.Now()), Question: obs.RedactText(req.Question),
		Options: []string{"approve", "reject"}, DefaultAction: "reject", Decision: decision,
		ResolvedBy: by, Step: step, Risk: string(req.Risk), Request: request,
	}
}

func (a *TerminalApprover) persist(ctx context.Context, req contracts.GateRequest, event, decision, by string, step int) {
	a.storeMu.Lock()
	store := a.store
	a.storeMu.Unlock()
	if store == nil {
		return
	}
	ge := GateEventFor(req, event, decision, by, step)
	if err := store.RecordGateEvent(context.WithoutCancel(ctx), ge); err != nil {
		a.storeMu.Lock()
		a.storeFailures++
		a.storeMu.Unlock()
		slog.Default().With("logger", "lha.hitl").Warn("gate_row_write_failed",
			"gate_id", ge.GateID, "gate_event", ge.Event, "error", err.Error())
	}
}
