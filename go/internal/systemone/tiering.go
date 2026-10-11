package systemone

import (
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

// The System One difficulty question (python: lha.systemone.tiering): is the item a small,
// well-understood change or a large one? An implementer is about to work one checklist item; the
// answer is used only to make LHA CHEAPER -- a confident "small" routes the implementer to the
// next cheaper model tier (never a stronger one), and nothing else changes. Only meaningful for
// the claude backend, where roles have tiers, and off by default (LHA_SYSTEM_ONE_TIERING). Its
// threshold is unmeasured, so the system_one event records the judgment for a future gold set.

const (
	// TierQuestionID is the id of the difficulty question.
	TierQuestionID = "difficulty"
)

// TierCriteria are the difficulty options and their descriptions, in order.
func TierCriteria() *contracts.OrderedMap {
	return contracts.NewOrderedMap(
		"small", "The task is a small, well-understood change: one file or a few lines, no new "+
			"dependencies, packages or design decisions.",
		"large", "The task needs exploration, several files, new dependencies or packages, or design "+
			"decisions.",
	)
}

// TierQuestion is the one Choice the difficulty screen asks.
func TierQuestion() Question {
	return Choice("The task `task` is about to be given to an implementer. Is it a small, well-understood "+
		"change, or does it need exploration, several files, new dependencies or packages, or "+
		"design decisions?", TierCriteria())
}

// TieringState is what the model sees: the task (redacted) and the item's witnesses.
func TieringState(description string, witnesses []string) *contracts.OrderedMap {
	items := make([]any, 0, len(witnesses))
	for _, w := range witnesses {
		items = append(items, w)
	}
	return contracts.NewOrderedMap(
		"task", obs.RedactText(description),
		"witnesses", items,
	)
}
