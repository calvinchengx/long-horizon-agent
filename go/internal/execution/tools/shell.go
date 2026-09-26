package tools

import (
	"context"
	"strconv"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// ShellTool runs a command (argv list, no shell) inside the sandbox. argv is the spec's
// CommandArg, so the dispatcher screens it with safety.ClassifyCommand and routes irreversible
// commands to a human gate.
type ShellTool struct{}

// Spec implements contracts.Tool.
func (ShellTool) Spec() contracts.ToolSpec {
	return contracts.ToolSpec{
		Name:        "run_command",
		Description: "Run a command (argv list, NO shell) in the workspace; returns output + exit code.",
		Parameters: map[string]any{
			"type": "object",
			"properties": pyfmt.NewOrderedMap(
				"argv", pyfmt.NewOrderedMap("type", "array", "items", map[string]any{"type": "string"}),
				"timeout_s", pyfmt.NewOrderedMap("type", "integer", "minimum", 1),
			),
			"required": []any{"argv"},
		},
		Mutating:   true,
		CommandArg: contracts.Str("argv"),
	}
}

// Run implements contracts.Tool.
func (ShellTool) Run(ctx context.Context, arguments map[string]any, tctx contracts.ToolContext) contracts.ToolResult {
	var raw []any
	switch x := arguments["argv"].(type) {
	case []any:
		raw = x
	case []string:
		for _, s := range x {
			raw = append(raw, s)
		}
	default:
		return contracts.Failure("'argv' must be a non-empty list of strings")
	}
	if len(raw) == 0 {
		return contracts.Failure("'argv' must be a non-empty list of strings")
	}
	argv := make([]string, len(raw))
	for i, token := range raw {
		s, ok := token.(string)
		if !ok {
			return contracts.Failure("'argv' entries must all be strings")
		}
		argv[i] = s
	}
	timeoutS := 600
	if rawTimeout, ok := arguments["timeout_s"]; ok {
		if !pyval.IsInt(rawTimeout) || pyval.Compare(rawTimeout, 1) < 0 {
			return contracts.Failure("'timeout_s' must be a positive integer")
		}
		timeoutS = pyval.Int(rawTimeout)
	}
	timeoutS = min(timeoutS, execution.MaxTimeoutS)

	result, err := tctx.Session.Exec(ctx, argv, contracts.ExecOptions{TimeoutS: timeoutS})
	if err != nil {
		return contracts.Failure(pyval.ExcText(err))
	}
	body := "exit_code=" + strconv.Itoa(result.ExitCode) + "\n" +
		"--- stdout ---\n" + result.Stdout + "\n" +
		"--- stderr ---\n" + result.Stderr
	body = clip(body)
	if result.OK() {
		return contracts.ToolResult{OK: true, Content: body}
	}
	errText := "exit " + strconv.Itoa(result.ExitCode)
	return contracts.ToolResult{OK: false, Content: body, Error: &errText}
}
