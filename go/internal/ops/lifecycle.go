// Package ops is the operations side of a mission (python/src/lha/ops): dependency degradation
// and safe-park decisions, and the mission lifecycle's negative paths (abort / impossible).
package ops

// MissionOutcome is a named terminal (or parked) mission state (python: MissionOutcome).
type MissionOutcome string

// The mission outcomes (values match the Python enum).
const (
	OutcomeDone       MissionOutcome = "DONE"
	OutcomeAborted    MissionOutcome = "ABORTED"
	OutcomeImpossible MissionOutcome = "IMPOSSIBLE"
	OutcomeParked     MissionOutcome = "DEGRADED_PARK"
)

// DefaultImpossibleThreshold is should_declare_impossible's default threshold.
const DefaultImpossibleThreshold = 3

// ShouldDeclareImpossible is true when an item has failed verification too many times in a row
// (escalate to a human) (python: should_declare_impossible). Pure: safe in workflow code.
func ShouldDeclareImpossible(consecutiveFailures, threshold int) bool {
	return consecutiveFailures >= threshold
}
