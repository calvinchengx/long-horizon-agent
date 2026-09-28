// Package tools holds the concrete tools and the run toolset assembly
// (python/src/lha/execution/tools).
//
// Every tool's result strings match the Python implementation byte for byte for the same inputs
// and filesystem state; lengths and slices count code points like Python's str.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
)

// Filesystem tools: read / write / list / grep, scoped to the sandbox workspace.
//
// All paths are confined to the workspace (execution.ResolveWithin): absolute paths, .. and
// symlinks pointing outside are rejected; walks skip symlinked entries that resolve outside and
// never descend into symlinked directories.
//
// grep treats pattern as a LITERAL substring by default. regex: true opts into regular
// expressions, with ReDoS guards: patterns are length-capped and rejected if they contain
// backreferences or a quantified group that itself contains a quantifier; lines are matched only
// up to maxLine characters and files larger than maxFileBytes are skipped. Parity note: regex
// patterns are compiled with Go's RE2, not Python's re — the guards reject the same patterns,
// but RE2 syntax errors read differently, RE2 rejects some Python-only syntax (lookaround, \Z),
// and \d \w \s \b are ASCII-only in RE2 (Unicode-aware in Python).

// MaxToolOutput is the hard cap on a single tool result's text (code points).
const MaxToolOutput = 25_000

var ignoreParts = map[string]bool{
	".git": true, "__pycache__": true, ".venv": true, "node_modules": true, ".mypy_cache": true,
	".ruff_cache": true,
}

const (
	maxPattern   = 256
	maxLine      = 2_000
	maxFileBytes = 2_000_000
	maxFiles     = 2_000
	maxHits      = 500
)

var (
	nestedQuantifier      = regexp.MustCompile(`\((?:[^()\\]|\\.)*[+*}](?:[^()\\]|\\.)*\)\s*(?:[+*]|\{\d)`)
	alternationQuantified = regexp.MustCompile(`\((?:[^()\\]|\\.)*\|(?:[^()\\]|\\.)*\)\s*(?:[+*]|\{\d)`)
	backreferencePattern  = regexp.MustCompile(`\\[1-9]|\(\?P=`)
	errPatternEmpty       = pyval.NewError("ValueError", "pattern must not be empty")
	errBackreference      = pyval.NewError("ValueError", "backreferences are not allowed")
	errNestedQuantifier   = pyval.NewError("ValueError", "nested/alternated quantified groups are not allowed (ReDoS risk)")
)

const truncatedMarker = "\n…[truncated]"

func clip(text string) string {
	if pyval.Len(text) <= MaxToolOutput {
		return text
	}
	return pyval.Head(text, MaxToolOutput) + truncatedMarker
}

// Matcher reports whether a line matches a grep pattern.
type Matcher func(line string) bool

// CompileSearchPattern compiles a grep pattern: literal by default, guarded regex when regex is
// true. It returns a ValueError-typed error for over-long or backtracking-prone patterns (and bad
// regex syntax).
func CompileSearchPattern(pattern string, regex bool) (Matcher, error) {
	if pattern == "" {
		return nil, errPatternEmpty
	}
	if pyval.Len(pattern) > maxPattern {
		return nil, pyval.NewError("ValueError", "pattern longer than "+strconv.Itoa(maxPattern)+" characters")
	}
	if !regex {
		return func(line string) bool { return strings.Contains(line, pattern) }, nil
	}
	if backreferencePattern.MatchString(pattern) {
		return nil, errBackreference
	}
	if nestedQuantifier.MatchString(pattern) || alternationQuantified.MatchString(pattern) {
		return nil, errNestedQuantifier
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, pyval.NewError("ValueError", "bad regex: "+err.Error())
	}
	return re.MatchString, nil
}

func strArg(arguments map[string]any, key string) string {
	v, ok := arguments[key]
	if !ok {
		return ""
	}
	return pyval.Str(v)
}

// orEmptyArg is str(arguments.get(key, "") or "").
func orEmptyArg(arguments map[string]any, key string) string {
	v, ok := arguments[key]
	if !ok || !pyval.Truthy(v) {
		return ""
	}
	return pyval.Str(v)
}

func failureFor(prefix string, err error) contracts.ToolResult {
	if pyval.IsOSError(err) || pyval.ExcTypeName(err) == "UnicodeDecodeError" {
		return contracts.Failure(prefix + pyval.OSErrorText(err))
	}
	return contracts.Failure(pyval.ExcText(err))
}

// ReadFileTool reads a UTF-8 text file relative to the workspace root.
type ReadFileTool struct{}

// Spec implements contracts.Tool.
func (ReadFileTool) Spec() contracts.ToolSpec {
	return contracts.ToolSpec{
		Name:        "read_file",
		Description: "Read a UTF-8 text file at a path relative to the workspace root.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []any{"path"},
		},
		PathArgs: []string{"path"},
	}
}

// Run implements contracts.Tool.
func (ReadFileTool) Run(ctx context.Context, arguments map[string]any, tctx contracts.ToolContext) contracts.ToolResult {
	path := strArg(arguments, "path")
	content, err := tctx.Session.ReadFile(ctx, path)
	if err != nil {
		return failureFor("cannot read "+contracts.PyRepr(path)+": ", err)
	}
	return contracts.Success(clip(content))
}

// WriteFileTool creates or overwrites a UTF-8 text file (mutating).
type WriteFileTool struct{}

// Spec implements contracts.Tool.
func (WriteFileTool) Spec() contracts.ToolSpec {
	return contracts.ToolSpec{
		Name:        "write_file",
		Description: "Create or overwrite a UTF-8 text file (path relative to the workspace root).",
		Parameters: map[string]any{
			"type": "object",
			"properties": pyfmt.NewOrderedMap(
				"path", map[string]any{"type": "string"},
				"content", map[string]any{"type": "string"},
			),
			"required": []any{"path", "content"},
		},
		Mutating: true,
		PathArgs: []string{"path"},
	}
}

// Run implements contracts.Tool.
func (WriteFileTool) Run(ctx context.Context, arguments map[string]any, tctx contracts.ToolContext) contracts.ToolResult {
	path := strArg(arguments, "path")
	raw, present := arguments["content"]
	content, isStr := raw.(string)
	if present && !isStr {
		return contracts.Failure("'content' must be a string")
	}
	if err := tctx.Session.WriteFile(ctx, path, content); err != nil {
		if pyval.IsOSError(err) {
			return contracts.Failure("cannot write " + contracts.PyRepr(path) + ": " + pyval.OSErrorText(err))
		}
		return contracts.Failure(pyval.ExcText(err))
	}
	return contracts.Success("wrote " + strconv.Itoa(pyval.Len(content)) + " bytes to " + path)
}

// ApplyEdit is content with its one occurrence of oldText replaced by newText; it is an error
// when oldText is empty, equals newText, or does not match exactly once.
func ApplyEdit(content, oldText, newText string) (string, error) {
	if oldText == "" {
		return "", errors.New("'old_text' must not be empty")
	}
	if oldText == newText {
		return "", errors.New("'new_text' is the same as 'old_text'")
	}
	switch count := strings.Count(content, oldText); {
	case count == 0:
		return "", errors.New("'old_text' was not found; read the file again and copy the text exactly")
	case count > 1:
		return "", fmt.Errorf("'old_text' matches %d places; include more surrounding lines so it matches one", count)
	}
	return strings.Replace(content, oldText, newText, 1), nil
}

// EditFileTool changes part of a file without rewriting it (mutating): a whole-file rewrite
// can drop code by mistake.
type EditFileTool struct{}

// Spec implements contracts.Tool.
func (EditFileTool) Spec() contracts.ToolSpec {
	return contracts.ToolSpec{
		Name: "edit_file",
		Description: "Replace one exact snippet in a UTF-8 text file (path relative to the workspace " +
			"root). 'old_text' must appear exactly once: copy it from read_file with enough " +
			"lines to be unique. Prefer this to write_file for changing an existing file.",
		Parameters: map[string]any{
			"type": "object",
			"properties": pyfmt.NewOrderedMap(
				"path", map[string]any{"type": "string"},
				"old_text", map[string]any{"type": "string"},
				"new_text", map[string]any{"type": "string"},
			),
			"required": []any{"path", "old_text", "new_text"},
		},
		Mutating: true,
		PathArgs: []string{"path"},
	}
}

// Run implements contracts.Tool.
func (EditFileTool) Run(ctx context.Context, arguments map[string]any, tctx contracts.ToolContext) contracts.ToolResult {
	path := strArg(arguments, "path")
	texts := [2]string{}
	for i, name := range []string{"old_text", "new_text"} {
		raw, present := arguments[name]
		text, isStr := raw.(string)
		if present && !isStr {
			return contracts.Failure("'old_text' and 'new_text' must be strings")
		}
		texts[i] = text
	}
	content, err := tctx.Session.ReadFile(ctx, path)
	if err != nil {
		return failureFor("cannot read "+contracts.PyRepr(path)+": ", err)
	}
	edited, err := ApplyEdit(content, texts[0], texts[1])
	if err != nil {
		return contracts.Failure(path + ": " + err.Error())
	}
	if err := tctx.Session.WriteFile(ctx, path, edited); err != nil {
		if pyval.IsOSError(err) {
			return contracts.Failure("cannot write " + contracts.PyRepr(path) + ": " + pyval.OSErrorText(err))
		}
		return contracts.Failure(pyval.ExcText(err))
	}
	return contracts.Success("edited " + path)
}

// containedFiles are the regular files under start whose real path stays inside root (sorted by
// path components, like sorted(Path.rglob("*")), capped at maxFiles). Symlinked directories are
// never descended into.
func containedFiles(root, start string) []string {
	var found []string
	var walk func(dir string, relParts []string) bool
	walk = func(dir string, relParts []string) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		for _, entry := range entries {
			full := dir + "/" + entry.Name()
			parts := append(append([]string(nil), relParts...), entry.Name())
			ignored := false
			for _, p := range parts {
				if ignoreParts[p] {
					ignored = true
					break
				}
			}
			isLink := entry.Type()&os.ModeSymlink != 0
			skip := ignored || (isLink && !execution.IsRelativeTo(execution.Realpath(full), root))
			if !skip {
				if st, err := os.Stat(full); err == nil && st.Mode().IsRegular() {
					found = append(found, full)
					if len(found) >= maxFiles {
						return false
					}
				}
			}
			if entry.IsDir() && !isLink {
				if !walk(full, parts) {
					return false
				}
			}
		}
		return true
	}
	var startParts []string
	if start != root {
		startParts = strings.Split(strings.TrimPrefix(start, root+"/"), "/")
	}
	if st, err := os.Lstat(start); err == nil && st.IsDir() {
		walk(start, startParts)
	}
	return found
}

func relTo(p, root string) string {
	if root == "/" {
		return strings.TrimPrefix(p, "/")
	}
	return strings.TrimPrefix(p, root+"/")
}

// ListFilesTool lists files under the workspace (optionally a subdirectory), ignoring noise.
type ListFilesTool struct{}

// Spec implements contracts.Tool.
func (ListFilesTool) Spec() contracts.ToolSpec {
	return contracts.ToolSpec{
		Name:        "list_files",
		Description: "List files under the workspace (optionally a subdirectory), ignoring noise.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"subdir": map[string]any{"type": "string"}},
		},
		PathArgs: []string{"subdir"},
	}
}

// Run implements contracts.Tool.
func (ListFilesTool) Run(_ context.Context, arguments map[string]any, tctx contracts.ToolContext) contracts.ToolResult {
	subdir := orEmptyArg(arguments, "subdir")
	root := execution.Realpath(contracts.HostRoot(tctx.Session))
	start, err := execution.ResolveWithin(root, subdir)
	if err != nil {
		if pyval.IsOSError(err) {
			return contracts.Failure(pyval.OSErrorText(err))
		}
		return contracts.Failure(pyval.ExcText(err))
	}
	var files []string
	for _, p := range containedFiles(root, start) {
		files = append(files, relTo(p, root))
	}
	if len(files) == 0 {
		return contracts.Success("(no files)")
	}
	return contracts.Success(clip(strings.Join(files, "\n")))
}

// GrepTool searches file contents; it returns "path:line: text" matches.
type GrepTool struct{}

// Spec implements contracts.Tool.
func (GrepTool) Spec() contracts.ToolSpec {
	return contracts.ToolSpec{
		Name: "grep",
		Description: "Search file contents; returns path:line: text matches. 'pattern' is a literal " +
			"string unless 'regex' is true (simple regular expressions only).",
		Parameters: map[string]any{
			"type": "object",
			"properties": pyfmt.NewOrderedMap(
				"pattern", map[string]any{"type": "string"},
				"subdir", map[string]any{"type": "string"},
				"regex", map[string]any{"type": "boolean"},
			),
			"required": []any{"pattern"},
		},
		PathArgs: []string{"subdir"},
	}
}

// universalNewlines is Python's text-mode newline translation ("\r\n" and "\r" become "\n").
func universalNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// Run implements contracts.Tool.
func (GrepTool) Run(_ context.Context, arguments map[string]any, tctx contracts.ToolContext) contracts.ToolResult {
	pattern := strArg(arguments, "pattern")
	subdir := orEmptyArg(arguments, "subdir")
	regex, _ := arguments["regex"].(bool)
	matcher, err := CompileSearchPattern(pattern, regex)
	if err != nil {
		return contracts.Failure(err.Error())
	}
	root := execution.Realpath(contracts.HostRoot(tctx.Session))
	start, err := execution.ResolveWithin(root, subdir)
	if err != nil {
		if pyval.IsOSError(err) {
			return contracts.Failure(pyval.OSErrorText(err))
		}
		return contracts.Failure(pyval.ExcText(err))
	}
	var hits []string
search:
	for _, p := range containedFiles(root, start) {
		st, err := os.Stat(p)
		if err != nil || st.Size() > maxFileBytes {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		text, err := pyval.DecodeUTF8(data)
		if err != nil {
			continue
		}
		for i, line := range pyval.SplitLines(universalNewlines(text)) {
			if matcher(pyval.Head(line, maxLine)) {
				hits = append(hits, relTo(p, root)+":"+strconv.Itoa(i+1)+": "+pyval.Head(pystr.Strip(line), 200))
				if len(hits) >= maxHits {
					break search
				}
			}
		}
	}
	if len(hits) == 0 {
		return contracts.Success("(no matches)")
	}
	return contracts.Success(clip(strings.Join(hits, "\n")))
}
