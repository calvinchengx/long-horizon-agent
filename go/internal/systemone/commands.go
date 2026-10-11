package systemone

import (
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

// The System One command second-opinion question (python: lha.systemone.commands): is a command
// the deterministic classifier ALLOWED dangerous? The classifier (safety.ClassifyCommand) is the
// boundary; this question is asked only about a run_command it allowed, so a model can add a gate
// the rules did not know to add. The answer can only ever make LHA more cautious: a confident
// "dangerous" sends the call to the human gate exactly as an irreversible command is (denied when
// there is no gate); anything else, or a failed call, changes nothing. The state is the command
// argv alone -- never tool output.

const (
	// CommandQuestionID is the id of the command second-opinion question.
	CommandQuestionID = "danger"
	// ActionGate is the action a confident "dangerous" takes (the call goes to the human gate).
	ActionGate = "gate"
)

// CommandCriteria are the command second-opinion options and their descriptions, in order.
func CommandCriteria() *contracts.OrderedMap {
	return contracts.NewOrderedMap(
		"routine", "The command is an ordinary, reversible action with no data leaving the machine and no "+
			"risk to the host.",
		"dangerous", "The command could damage the host, delete or overwrite data, exfiltrate data over the "+
			"network, or is irreversible or outward-facing.",
	)
}

// CommandQuestion is the one Choice the command second opinion asks.
func CommandQuestion() Question {
	return Choice("The command `command` is about to run. Is it an ordinary, reversible action, or one a "+
		"human should decide on first?", CommandCriteria())
}

// CommandState is what the model sees: only the command (redacted), never tool output.
func CommandState(command []string) *contracts.OrderedMap {
	return contracts.NewOrderedMap("command", obs.RedactText(strings.Join(command, " ")))
}
