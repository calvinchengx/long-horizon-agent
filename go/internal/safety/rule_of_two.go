package safety

import "errors"

// Meta's "Rule of Two" for agent safety (python/src/lha/safety/rule_of_two.py).
//
// A single agent session must hold AT MOST TWO of the three dangerous capabilities at once:
// ingesting UNTRUSTED content, access to PRIVATE data, and EXTERNAL communications. Holding all
// three is the "lethal trifecta" that makes prompt injection catastrophic; the orchestrator
// checks this before granting a session its capability set.

// Capability is one of the three dangerous capabilities.
type Capability string

// The three capabilities (values match the Python enum).
const (
	UntrustedContent Capability = "untrusted_content"
	PrivateData      Capability = "private_data"
	ExternalComms    Capability = "external_comms"
)

// ErrRuleOfTwoViolation is returned by CheckRuleOfTwo for the full lethal trifecta.
var ErrRuleOfTwoViolation = errors.New(
	"session would hold untrusted content + private data + external comms " +
		"(the lethal trifecta); split capabilities across sessions")

// Permits reports whether the capability set is safe (holds at most two distinct capabilities;
// duplicates count once, as in the Python set).
func Permits(caps ...Capability) bool {
	distinct := map[Capability]struct{}{}
	for _, c := range caps {
		distinct[c] = struct{}{}
	}
	return len(distinct) < 3
}

// CheckRuleOfTwo returns ErrRuleOfTwoViolation if a session would hold the lethal trifecta.
func CheckRuleOfTwo(caps ...Capability) error {
	if !Permits(caps...) {
		return ErrRuleOfTwoViolation
	}
	return nil
}
