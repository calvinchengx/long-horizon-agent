package tools

import (
	"context"
	"fmt"
	"regexp"
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

// SymbolMisses are what ripwire prints when a symbol question names nothing it indexed.
var SymbolMisses = []string{"matched no symbol", "symbol not found", "matched no indexed definition"}

var identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// AlternateTargets are other spellings to try when a symbol question misses: Class.method is how
// models write a method, ripwire's form is Class::method, and the bare name is the last resort.
// Only for symbol kinds and a dotted run of identifiers (python: alternate_targets).
func AlternateTargets(kind, target string) []string {
	parts := strings.Split(pyfmt.PyStrip(target), ".")
	if kind == "find" || len(parts) < 2 {
		return []string{}
	}
	for _, p := range parts {
		if !identifierRE.MatchString(p) {
			return []string{}
		}
	}
	n := len(parts)
	return []string{parts[n-2] + "::" + parts[n-1], parts[n-1]}
}

// IsSymbolMiss reports whether ripwire's error says the symbol was not found (python:
// is_symbol_miss).
func IsSymbolMiss(detail string) bool {
	for _, miss := range SymbolMisses {
		if strings.Contains(detail, miss) {
			return true
		}
	}
	return false
}

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
	opts := contracts.ExecOptions{TimeoutS: max(1, int(t.TimeoutS))}
	res, err := tctx.Session.Exec(ctx, argv, opts)
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
		if !res.TimedOut && IsSymbolMiss(detail) {
			for _, other := range AlternateTargets(kind, target) {
				otherArgv, _ := CodeQueryArgv(kind, other, t.TokenBudget)
				retry, err := tctx.Session.Exec(ctx, otherArgv, opts)
				if err == nil && retry.OK() {
					note := "(no symbol " + contracts.PyRepr(pyfmt.PyStrip(target)) + "; answered for " +
						contracts.PyRepr(other) + ")\n"
					return contracts.Success(ClipAnswer(note + retry.Stdout))
				}
			}
		}
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
