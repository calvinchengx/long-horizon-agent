package coordination

import (
	"context"
	"strings"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Lease granting: an implementer asks for a file outside its write-set in the middle of a wave
// (python: lha.coordination.leases). The request_lease tool hands (path, reason) to a
// LeaseBroker, which decides it against the COMMITTED ownership map and checklist (DecideLease):
//
//   - granted when the file is unowned (unassigned space, which belongs to the lead, who never
//     writes during a wave), already the requester's, or owned by a finished writer (an
//     implementer whose item is done or split);
//   - refused when another writer whose item is still open owns it, when it is a shared file,
//     when it is harness-owned (.lha/, .git/), or when the path is invalid.
//
// Every decision is committed to the mission anchor by itself (CommitAnchorUpdate: only .lha/
// files are in that commit) as a "lease" event; a grant also rewrites .lha/ownership.json in the
// same commit. Decisions are serialized by an in-process lock plus the checkout's flock
// (.git/lha-cycle.lock, shared with the Python implementation), so two implementers can never
// both be granted one file.

// LeaseEvent is the event kind of a lease decision.
const LeaseEvent = "lease"

const maxLeaseReason = 500

var harnessDirs = []string{".lha", ".git"}

// LeaseHandler is what the request_lease tool calls: (path, reason) -> (granted, message for the
// agent).
type LeaseHandler func(ctx context.Context, path, reason string) (bool, string, error)

// LeaseDecision is the outcome of one LeaseRequest (persisted as a "lease" event; its payload is
// the pydantic model_dump: writer, path, reason, granted, previous_owner, why).
type LeaseDecision struct {
	Writer        string
	Path          string
	Reason        string
	Granted       bool
	PreviousOwner string // "" = python None
	HasPrevious   bool
	Why           string
}

// Message is what the requesting agent is told.
func (d LeaseDecision) Message() string {
	if d.Granted {
		return "lease granted: you may now write " + contracts.PyRepr(d.Path) + " (" + d.Why +
			"). It is yours until your item is done."
	}
	return "lease refused for " + contracts.PyRepr(d.Path) + ": " + d.Why + ". Do not write it; " +
		"finish what you can within your write-set and say in your summary what still needs this file."
}

// Payload is the decision's event payload (python: LeaseDecision.model_dump()).
func (d LeaseDecision) Payload() map[string]any {
	var previous any
	if d.HasPrevious {
		previous = d.PreviousOwner
	}
	return map[string]any{
		"writer": d.Writer, "path": d.Path, "reason": d.Reason, "granted": d.Granted,
		"previous_owner": previous, "why": d.Why,
	}
}

// FinishedWriters are the implementer ids whose item is finished (done or split): their leases
// have ended.
func FinishedWriters(checklist contracts.Checklist) []string {
	out := []string{}
	for _, item := range checklist.Items {
		if item.Status == contracts.StatusDone || item.Status == contracts.StatusSplit {
			out = append(out, WriterForItem(item.ID))
		}
	}
	return out
}

// DecideLease grants or refuses request against ownership (pure; nothing is changed).
func DecideLease(ownership *FileOwnershipMap, request LeaseRequest, finished []string) LeaseDecision {
	reason := pyfmt.Head(pyfmt.PyStrip(request.Reason), maxLeaseReason)
	refuse := func(path, why, owner string) LeaseDecision {
		return LeaseDecision{Writer: request.Writer, Path: path, Reason: reason, Why: why,
			PreviousOwner: owner, HasPrevious: owner != ""}
	}
	norm, err := NormalizePath(request.Path)
	if err != nil {
		return refuse(request.Path, err.Error(), "")
	}
	if request.Writer == Lead {
		return refuse(norm, "the lead needs no lease (it owns all unassigned space)", "")
	}
	first, _, _ := strings.Cut(norm, "/")
	for _, dir := range harnessDirs {
		if Casefold(first) == dir {
			return refuse(norm, "harness-owned files (.lha/, .git/) are never leased", "")
		}
	}
	if isSharedNorm(norm) {
		return refuse(norm, "it is a shared file (build manifest, lockfile, package entry point, ...) "+
			"that only the lead writes", "")
	}
	owner := ownership.OwnerOf(norm)
	granted := func(why string) LeaseDecision {
		return LeaseDecision{Writer: request.Writer, Path: norm, Reason: reason, Granted: true,
			PreviousOwner: owner, HasPrevious: owner != "", Why: why}
	}
	if owner == request.Writer {
		return granted("you already own it")
	}
	isFinished := false
	for _, w := range finished {
		isFinished = isFinished || w == owner
	}
	switch {
	case owner == "" || owner == Lead:
		return granted("it was unassigned")
	case isFinished:
		return granted("its owner " + contracts.PyRepr(owner) + " has finished")
	}
	return refuse(norm, "it is owned by "+contracts.PyRepr(owner)+", whose item is still open", owner)
}

// NewLeaseEvent is the decision as a "lease" event.
func NewLeaseEvent(d LeaseDecision, cycleID string) contracts.EventRecord {
	return contracts.EventRecord{Kind: LeaseEvent, CycleID: cycleID, Payload: d.Payload()}
}

// LeaseBroker decides lease requests against the committed anchor and commits each decision.
// In orchestrate pass the run's own anchor instance, so a grant and the ownership release the
// orchestrator staged are written together; in a durable activity a fresh instance is fine.
type LeaseBroker struct {
	anchor *state.GitMissionAnchor
	mu     sync.Mutex
}

// NewLeaseBroker returns a broker committing to anchor.
func NewLeaseBroker(anchor *state.GitMissionAnchor) *LeaseBroker { return &LeaseBroker{anchor: anchor} }

// Request decides request and commits the decision (and a grant's ownership change).
func (b *LeaseBroker) Request(ctx context.Context, request LeaseRequest, cycleID string) (LeaseDecision, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	anchor := b.anchor
	unlock, err := WorkdirFlock(ctx, anchor.Workdir(), CycleLock)
	if err != nil {
		return LeaseDecision{}, err
	}
	defer unlock()
	checklist, err := anchor.ReadChecklist(ctx)
	if err != nil {
		return LeaseDecision{}, err
	}
	finished := FinishedWriters(checklist)
	persisted, err := ReadOwnership(ctx, anchor)
	if err != nil {
		return LeaseDecision{}, err
	}
	current := EffectiveOwnership(persisted, finished)
	decision := DecideLease(current, request, finished)
	changed := decision.Granted && (!decision.HasPrevious || decision.PreviousOwner != decision.Writer)
	if changed {
		if _, err := current.Reassign(decision.Path, decision.Writer); err != nil {
			return LeaseDecision{}, err
		}
		if err := StageOwnership(anchor, current); err != nil {
			return LeaseDecision{}, err
		}
	}
	verb := "refused"
	if decision.Granted {
		verb = "granted"
	}
	summary := ""
	if changed {
		summary = "- " + cycleID + " lease " + verb + ": " + decision.Path + " to " + decision.Writer
	}
	if _, err := anchor.CommitAnchorUpdate(ctx, contracts.Checkpoint{
		CycleID:         cycleID,
		ProgressSummary: summary,
		Checklist:       checklist,
		Decisions:       []contracts.DecisionRecord{},
		Events:          []contracts.EventRecord{NewLeaseEvent(decision, cycleID)},
		CommitMessage:   "lha: lease " + verb + ": " + decision.Path + " (" + decision.Writer + ")",
	}); err != nil {
		return LeaseDecision{}, err
	}
	return decision, nil
}

// LeaseLog collects one implementer's lease decisions (safe for concurrent use).
type LeaseLog struct {
	mu        sync.Mutex
	decisions []LeaseDecision
}

// Append records a decision.
func (l *LeaseLog) Append(d LeaseDecision) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.decisions = append(l.decisions, d)
}

// Decisions is a copy of the recorded decisions, in order.
func (l *LeaseLog) Decisions() []LeaseDecision {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]LeaseDecision{}, l.decisions...)
}

// NewLeaseHandler is the request_lease callback for one implementer. A grant is also applied to
// ownership, the in-memory map the implementer's OwnershipGuard and its git-layer check use, so
// the leased file becomes writable at once. Every decision is appended to log (when non-nil).
func NewLeaseHandler(broker *LeaseBroker, writer, cycleID string, ownership *FileOwnershipMap, log *LeaseLog) LeaseHandler {
	return func(ctx context.Context, path, reason string) (bool, string, error) {
		decision, err := broker.Request(ctx, LeaseRequest{Writer: writer, Path: path, Reason: reason}, cycleID)
		if err != nil {
			return false, "", err
		}
		if decision.Granted && ownership.OwnerOf(decision.Path) != writer {
			if _, err := ownership.Reassign(decision.Path, writer); err != nil {
				return false, "", err
			}
		}
		if log != nil {
			log.Append(decision)
		}
		return decision.Granted, decision.Message(), nil
	}
}
