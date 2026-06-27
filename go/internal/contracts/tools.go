package contracts

import "context"

// ToolSpec describes a tool to the model (JSON-Schema parameters) and to the dispatcher (policy).
type ToolSpec struct {
	Name           string         `json:"name"`
	Description    string         `json:"description"`
	Parameters     map[string]any `json:"parameters"`
	Mutating       bool           `json:"mutating"`
	Egress         bool           `json:"egress"`
	UntrustedInput bool           `json:"untrusted_input"`
	// Argument names holding workspace paths: a mutating tool may not target .lha/ or .git/.
	PathArgs []string `json:"path_args"`
	// Argument name holding an argv list, screened for irreversible commands.
	CommandArg *string `json:"command_arg"`
}

// ToolResult is a compact, model-facing tool result.
type ToolResult struct {
	OK          bool    `json:"ok"`
	Content     string  `json:"content"`
	ArtifactRef *string `json:"artifact_ref"`
	Error       *string `json:"error"`
}

// Success builds a successful ToolResult.
func Success(content string) ToolResult { return ToolResult{OK: true, Content: content} }

// Failure builds a failed ToolResult.
func Failure(err string) ToolResult { return ToolResult{OK: false, Error: &err} }

// ErrorText returns the error message ("" when none).
func (r ToolResult) ErrorText() string {
	if r.Error == nil {
		return ""
	}
	return *r.Error
}

// ToolContext is the live, non-serializable context handed to a tool at execution time.
type ToolContext struct {
	MissionID string
	Session   SandboxSession
}

// Tool is a single capability.
type Tool interface {
	Spec() ToolSpec
	Run(ctx context.Context, arguments map[string]any, tctx ToolContext) ToolResult
}

// ToolDispatcher gates and routes tool calls. Dispatch never returns an error: failures come back
// as a ToolResult.
type ToolDispatcher interface {
	Specs() []ToolSpec
	Dispatch(ctx context.Context, call ToolCall, tctx ToolContext) ToolResult
}
