// Package hitl is the human-in-the-loop side of a local run (python/src/lha/hitl): the escalation
// ladder shared by every human gate, the optional gate webhook and the terminal approver (the
// console y/N gate of `lha run-local --approve-interactive`).
//
// The durable gates (DeferredApprovalGate, the workflow's signal-based gate) belong to the
// Temporal spine, internal/durable, which uses the ladder here (pure and deterministic, so safe in
// workflow code).
package hitl

import "sort"

// EscalationSchedule is the sorted, unique reminder offsets strictly between 0 and
// timeoutSeconds (python: escalation_schedule).
func EscalationSchedule(timeoutSeconds float64, steps []float64) []float64 {
	seen := map[float64]bool{}
	out := []float64{}
	for _, s := range steps {
		if s > 0 && s < timeoutSeconds && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Float64s(out)
	return out
}

// Rung is the next rung of the ladder: wait WaitSeconds; then remind (Step > 0) or apply the
// default (Step == 0, the timeout).
type Rung struct {
	WaitSeconds float64
	Step        int // 1-based reminder number; 0 = the timeout
}

// NextRung is what to wait for next, given sent reminders already emitted at elapsed seconds
// (python: next_rung).
func NextRung(elapsed, timeoutSeconds float64, schedule []float64, sent int) Rung {
	if sent < len(schedule) {
		return Rung{WaitSeconds: max(0, schedule[sent]-elapsed), Step: sent + 1}
	}
	return Rung{WaitSeconds: max(0, timeoutSeconds-elapsed), Step: 0}
}
