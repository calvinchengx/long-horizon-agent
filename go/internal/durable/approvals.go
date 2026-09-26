package durable

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// DeferredApprovalGate is the human gate of a durable cycle (python:
// lha.hitl.approvals.DeferredApprovalGate). An activity must not block for hours on a person, so
// the gate records every flagged request and DENIES it for now (ResolvedBy
// contracts.PendingApproval); the cycle reports the pending requests, the MissionWorkflow opens a
// durable human gate, and an approval is passed to a later cycle, where the SAME action —
// identified by its fingerprint — is allowed exactly once.
type DeferredApprovalGate struct {
	mu       sync.Mutex
	approved map[string]bool
	// Pending are the queued requests (one per fingerprint, in order).
	Pending []PendingApproval
	// Used are the approved fingerprints this cycle consumed.
	Used []string
}

// NewDeferredApprovalGate allows each of the approved fingerprints once.
func NewDeferredApprovalGate(approved []string) *DeferredApprovalGate {
	g := &DeferredApprovalGate{approved: map[string]bool{}}
	for _, fp := range approved {
		g.approved[fp] = true
	}
	return g
}

var _ contracts.HITLGate = (*DeferredApprovalGate)(nil)

// Request approves a previously approved fingerprint (once); queues everything else.
func (g *DeferredApprovalGate) Request(_ context.Context, req contracts.GateRequest) (contracts.GateResolution, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fp := req.Context["fingerprint"]
	if fp != "" && g.approved[fp] && !contains(g.Used, fp) {
		g.Used = append(g.Used, fp)
		return contracts.GateResolution{GateID: req.GateID, Decision: contracts.GateApprove, ResolvedBy: "operator"}, nil
	}
	if fp != "" {
		known := false
		for _, p := range g.Pending {
			known = known || p.Fingerprint == fp
		}
		if !known {
			g.Pending = append(g.Pending, PendingApproval{
				Fingerprint: fp,
				Tool:        req.Context["tool"],
				Reason:      req.Context["reason"],
				Arguments:   req.Context["arguments"],
			})
		}
	}
	return contracts.GateResolution{GateID: req.GateID, Decision: contracts.GateReject, ResolvedBy: contracts.PendingApproval}, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// IdempotencyKey derives a deterministic key from parts (python: lha.ids.idempotency_key): the
// first 32 hex chars of sha256 of the parts joined by U+001F.
func IdempotencyKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])[:32]
}
