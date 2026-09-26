package coordination

import (
	"fmt"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Typed task contracts + the ticket lifecycle (python: lha.coordination.ticket). A TaskContract
// is the structured delegation handed to a sub-agent — objective, output shape, explicit
// boundaries, the files it owns, budgets and acceptance criteria.

// TicketStatus is a ticket's lifecycle state.
type TicketStatus string

// Ticket states.
const (
	TicketCreated        TicketStatus = "created"
	TicketInProgress     TicketStatus = "in_progress"
	TicketAwaitingVerify TicketStatus = "awaiting_verify"
	TicketAwaitingMerge  TicketStatus = "awaiting_merge"
	TicketDone           TicketStatus = "done"
	TicketFailed         TicketStatus = "failed"
)

// AllowedTransitions is the ticket lifecycle; anything not listed is illegal. Done and failed
// are terminal (a failed piece of work is retried as a NEW ticket). Verification or merge can
// bounce a ticket back to in_progress for another attempt.
var AllowedTransitions = map[TicketStatus][]TicketStatus{
	TicketCreated:        {TicketInProgress, TicketFailed},
	TicketInProgress:     {TicketAwaitingVerify, TicketFailed},
	TicketAwaitingVerify: {TicketAwaitingMerge, TicketInProgress, TicketFailed},
	TicketAwaitingMerge:  {TicketDone, TicketInProgress, TicketFailed},
	TicketDone:           {},
	TicketFailed:         {},
}

// IllegalTransitionError is a ticket asked to move along an edge not in AllowedTransitions.
type IllegalTransitionError struct{ Message string }

func (e *IllegalTransitionError) Error() string { return e.Message }

// TaskContract is the typed work order for a sub-agent.
type TaskContract struct {
	Objective    string   `json:"objective"`
	Role         string   `json:"role"`          // researcher | implementer | reviewer | tester | integrator
	OutputSchema string   `json:"output_schema"` // description of the expected output shape
	Boundaries   []string `json:"boundaries"`    // explicit do-nots
	WriteSet     []string `json:"write_set"`     // files this writer owns (implementers)
	TokenBudget  int      `json:"token_budget"`  // 0 = inherit default
	ToolBudget   int      `json:"tool_budget"`
	Acceptance   []string `json:"acceptance"` // check names that must pass
}

// Ticket is a unit of delegated work with a durable lifecycle.
type Ticket struct {
	ID            string       `json:"id"`
	Contract      TaskContract `json:"contract"`
	ItemID        string       `json:"item_id"` // the checklist item this advances
	Status        TicketStatus `json:"status"`
	Branch        string       `json:"branch"`
	ResultSummary string       `json:"result_summary"`
	Attempts      int          `json:"attempts"`
}

// NewTicket is a created ticket.
func NewTicket(id string, contract TaskContract, itemID string, attempts int) Ticket {
	return Ticket{ID: id, Contract: contract, ItemID: itemID, Status: TicketCreated, Attempts: attempts}
}

// CanTransition reports whether to is a legal next state.
func (t Ticket) CanTransition(to TicketStatus) bool {
	for _, s := range AllowedTransitions[t.Status] {
		if s == to {
			return true
		}
	}
	return false
}

// Transition returns a copy advanced to "to" (an *IllegalTransitionError for an illegal edge).
// Entering in_progress counts as a new attempt.
func (t Ticket) Transition(to TicketStatus) (Ticket, error) {
	if !t.CanTransition(to) {
		return t, &IllegalTransitionError{fmt.Sprintf("ticket %s: illegal transition %s -> %s",
			contracts.PyRepr(t.ID), t.Status, to)}
	}
	t.Status = to
	if to == TicketInProgress {
		t.Attempts++
	}
	return t, nil
}
