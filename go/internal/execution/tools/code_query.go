package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// code_query (python: lha.execution.tools.code_query): ask ripwire structural questions about the
// code instead of reading files. Enabled by LHA_CODE_QUERY=true; read-only, no egress. The target
// is one argument (no shell); answers are ranked and can miss things.

// CodeQueryKinds are the question kinds, in order, and the ripwire flag each maps to.
var CodeQueryKinds = []struct{ Kind, Flag string }{
	{"find", "--for"},
	{"definition", "--expand"},
	{"callers", "--callers"},
	{"uses", "--uses"},
	{"impact", "--impact"},
}

// Limits of a question's target and of the answer returned to the model (code points).
const (
	MaxSymbolChars = 300
	MaxFindChars   = 1000
	MaxAnswerChars = 16_000
)

// CodeQueryArgv is the ripwire command for one question; an unusable question is an error with
// Python's message (python: code_query_argv).
func CodeQueryArgv(kind, target string, tokenBudget int) ([]string, error) {
	flag := ""
	names := make([]string, len(CodeQueryKinds))
	for i, k := range CodeQueryKinds {
		names[i] = k.Kind
		if k.Kind == kind {
			flag = k.Flag
		}
	}
	if flag == "" {
		return nil, fmt.Errorf("'kind' must be one of %s", strings.Join(names, ", "))
	}
	target = pyfmt.PyStrip(target)
	if target == "" {
		return nil, fmt.Errorf("'target' must not be empty")
	}
	limit := MaxSymbolChars
	if kind == "find" {
		limit = MaxFindChars
	}
	if pyfmt.RuneLen(target) > limit {
		return nil, fmt.Errorf("'target' is longer than %d characters", limit)
	}
	if kind != "find" && strings.ContainsAny(target, "\n\r\x00") {
		return nil, fmt.Errorf("a symbol 'target' must be a single line")
	}
	argv := []string{"ripwire", ".", flag + "=" + target}
	switch kind {
	case "find":
		argv = append(argv, fmt.Sprintf("--token-budget=%d", tokenBudget))
	case "definition":
		argv = append(argv, "--top-k=0")
	}
	return argv, nil
}

// ClipAnswer cuts an answer to MaxAnswerChars with a marker (python: clip_answer).
func ClipAnswer(text string) string {
	if pyfmt.RuneLen(text) <= MaxAnswerChars {
		return text
	}
	return pyfmt.Head(text, MaxAnswerChars) + "\n…[truncated]"
}

// CodeQueryTool is the code_query tool.
type CodeQueryTool struct {
	TokenBudget int
	TimeoutS    float64
}

// Spec implements contracts.Tool.
func (CodeQueryTool) Spec() contracts.ToolSpec {
	kinds := make([]any, len(CodeQueryKinds))
	for i, k := range CodeQueryKinds {
		kinds[i] = k.Kind
	}
	return contracts.ToolSpec{
		Name: "code_query",
		Description: "Ask the code map (ripwire) about this repository instead of reading whole files. " +
			"kind 'find': the code relevant to a task, target = the task in words; " +
			"'definition': a symbol's full body; 'callers': what calls a symbol; " +
			"'uses': where a symbol is used; 'impact': what reaches a symbol (the blast radius " +
			"of changing it). Answers are ranked and can miss things: read a file before you " +
			"change it.",
		Parameters: map[string]any{
			"type": "object",
			"properties": pyfmt.NewOrderedMap(
				"kind", pyfmt.NewOrderedMap("type", "string", "enum", kinds),
				"target", pyfmt.NewOrderedMap("type", "string"),
			),
			"required": []any{"kind", "target"},
		},
		PathArgs: []string{},
	}
}

// Run implements contracts.Tool.
func (t CodeQueryTool) Run(ctx context.Context, arguments map[string]any, tctx contracts.ToolContext) contracts.ToolResult {
	kind, _ := arguments["kind"].(string)
	target, _ := arguments["target"].(string)
	argv, err := CodeQueryArgv(kind, target, t.TokenBudget)
	if err != nil {
		return contracts.Failure(err.Error())
	}
	res, err := tctx.Session.Exec(ctx, argv, contracts.ExecOptions{TimeoutS: max(1, int(t.TimeoutS))})
	if err != nil {
		return contracts.Failure("code_query: " + err.Error())
	}
	if res.ExitCode == 127 {
		return contracts.Failure("code_query needs ripwire in the sandbox; it is not installed")
	}
	if !res.OK() {
		detail := res.Stderr
		if detail == "" {
			detail = res.Stdout
		}
		detail = pyfmt.Tail(pyfmt.PyStrip(detail), 500)
		reason := detail
		switch {
		case res.TimedOut:
			reason = "timed out"
		case reason == "":
			reason = fmt.Sprintf("exit %d", res.ExitCode)
		}
		return contracts.Failure("code_query: " + reason)
	}
	return contracts.Success(ClipAnswer(res.Stdout))
}
