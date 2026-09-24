package contracts

import "context"

// Human-in-the-loop contract (python/src/lha/contracts/hitl.py).
//
// Gates pause on high-blast-radius / irreversible actions. Every gate has a timeout and a default
// action, so an unattended run never stalls forever invisibly and never takes an irreversible
// action without a decision.

// GateDecision is a gate's answer (values match the Python enum).
type GateDecision string

// The gate decisions.
const (
	GateApprove GateDecision = "approve"
	GateReject  GateDecision = "reject"
	GateAbort   GateDecision = "abort"
)

// RiskTier classifies an action's blast radius.
type RiskTier string

// The risk tiers.
const (
	RiskReversible   RiskTier = "reversible"   // has a saga compensation; can auto-proceed
	RiskIrreversible RiskTier = "irreversible" // merge-to-protected-main, deploy, external comms
)

// PendingApproval is the ResolvedBy value of a resolution that did not decide yet: the call was
// queued for a human and may be allowed in a later cycle (python: lha.hitl.approvals.PENDING).
const PendingApproval = "pending-approval"

// GateRequest is a request for a human decision.
type GateRequest struct {
	GateID        string            `json:"gate_id"`
	Question      string            `json:"question"`
	Risk          RiskTier          `json:"risk"`
	DefaultAction GateDecision      `json:"default_action"` // applied on timeout
	Options       []GateDecision    `json:"options"`
	Context       map[string]string `json:"context"`
}

// NewGateRequest returns a request with the Python defaults: irreversible risk, abort on
// timeout, options [approve, reject], an empty context.
func NewGateRequest(gateID, question string) GateRequest {
	return GateRequest{
		GateID:        gateID,
		Question:      question,
		Risk:          RiskIrreversible,
		DefaultAction: GateAbort,
		Options:       []GateDecision{GateApprove, GateReject},
		Context:       map[string]string{},
	}
}

// GateResolution is the outcome of a gate.
type GateResolution struct {
	GateID     string       `json:"gate_id"`
	Decision   GateDecision `json:"decision"`
	ResolvedBy string       `json:"resolved_by"`
	Defaulted  bool         `json:"defaulted"` // true if the timeout default was applied
}

// HITLGate requests a decision and returns a resolution.
type HITLGate interface {
	Request(ctx context.Context, req GateRequest) (GateResolution, error)
}

// EventDrainer is implemented by gates and dispatchers that record events (tool approvals, gate
// reminders) for the agent loop to commit with the cycle's checkpoint. DrainEvents returns the
// events since the last drain, oldest first.
type EventDrainer interface {
	DrainEvents() []EventRecord
}
