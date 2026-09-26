// Package claudecodetest is a fake claude executable for tests of the claude_code model backend
// and lead engine (python: the FAKE_CLAUDE script in tests/unit/test_claude_code.py).
//
// The fake is the test binary itself: a test package's TestMain calls Main first, and when the
// process was started as the fake (EnvFake=1 in its environment) Main behaves like claude -p and
// exits. Install points a test at it. In "mcp" mode it is a real MCP client: it reads the
// --mcp-config LHA passed, talks JSON-RPC over HTTP to LHA's bridge, calls the tools listed in
// FAKE_CLAUDE_CALLS and prints a result shaped like claude -p --output-format json. So the whole
// path (argv, bridge, dispatcher, verifier, checkpoint, ledger) is real; only the model is
// scripted.
//
// Environment (the same as the Python fake): FAKE_CLAUDE_MODE (text | auth | budget | mcp, plus
// the Go-only sleep and crash), FAKE_CLAUDE_LOG (append {"argv", "stdin", "cwd"} per run),
// FAKE_CLAUDE_REPLY (text mode's result) and FAKE_CLAUDE_CALLS ([[name, arguments], ...]).
package claudecodetest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// EnvFake marks a process started as the fake claude.
const EnvFake = "LHA_TEST_FAKE_CLAUDE"

// Main runs the fake and exits when this process is the fake claude; otherwise it returns.
func Main() {
	if os.Getenv(EnvFake) != "1" {
		return
	}
	os.Exit(run(os.Args[1:]))
}

// Install makes the test binary usable as claude for the rest of the test (the environment is
// inherited by every claude the code under test starts). It returns the binary to run and the
// path of the call log.
func Install(t *testing.T) (binary, log string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log = filepath.Join(t.TempDir(), "claude.log")
	t.Setenv(EnvFake, "1")
	t.Setenv("FAKE_CLAUDE_LOG", log)
	return exe, log
}

// Call is one logged run of the fake.
type Call struct {
	Argv  []string `json:"argv"`
	Stdin string   `json:"stdin"`
	Cwd   string   `json:"cwd"`
	// Texts are the tool results the bridge returned (mcp mode, which logs when it finishes).
	Texts []string `json:"texts,omitempty"`
}

// Calls reads the fake's call log (none if it never ran).
func Calls(t *testing.T, log string) []Call {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []Call
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var c Call
		if err := json.Unmarshal(line, &c); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	return calls
}

func run(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("2.0.0 (Claude Code, fake)")
		return 0
	}
	prompt, _ := io.ReadAll(os.Stdin)
	cwd, _ := os.Getwd()
	call := Call{Argv: append([]string{}, args...), Stdin: string(prompt), Cwd: cwd}
	mode := os.Getenv("FAKE_CLAUDE_MODE")
	if mode == "" {
		mode = "text"
	}
	if mode != "mcp" {
		logCall(call)
	}
	switch mode {
	case "text":
		reply, ok := os.LookupEnv("FAKE_CLAUDE_REPLY")
		if !ok {
			reply = `{"done": true, "summary": "ok"}`
		}
		result(reply, 0.01, nil)
	case "auth":
		result("Failed to authenticate: OAuth session expired", 0, map[string]any{"is_error": true})
	case "budget":
		result("", 0.9, map[string]any{"subtype": "error_max_budget_usd", "is_error": true})
	case "sleep":
		time.Sleep(time.Minute)
	case "crash":
		fmt.Fprintln(os.Stderr, "Error: unknown option '--frobnicate'")
		return 2
	case "mcp":
		if err := mcp(call); err != nil {
			fmt.Fprintln(os.Stderr, "fake claude:", err)
			return 1
		}
	}
	return 0
}

func logCall(call Call) {
	log := os.Getenv("FAKE_CLAUDE_LOG")
	if log == "" {
		return
	}
	line, _ := json.Marshal(call)
	f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
}

func result(text string, cost float64, extra map[string]any) {
	out := map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": text,
		"num_turns": 3, "session_id": "sess-1", "stop_reason": "end_turn",
		"total_cost_usd": cost,
		"usage": map[string]any{
			"input_tokens": 120, "output_tokens": 30, "cache_read_input_tokens": 1000,
			"cache_creation_input_tokens": 200,
			"cache_creation":              map[string]any{"ephemeral_1h_input_tokens": 50},
		},
		"modelUsage": map[string]any{
			"claude-haiku-4-5":  map[string]any{"outputTokens": 2},
			"claude-sonnet-4-6": map[string]any{"outputTokens": 28},
		},
	}
	for k, v := range extra {
		out[k] = v
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
}

func mcp(call Call) error {
	args := call.Argv
	var configJSON string
	for i, a := range args {
		if a == "--mcp-config" && i+1 < len(args) {
			configJSON = args[i+1]
		}
	}
	var config struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return err
	}
	server := config.MCPServers["lha"]
	seq := 0
	rpc := func(method string, params any, notify bool) (map[string]any, error) {
		msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
		if params == nil {
			msg["params"] = map[string]any{}
		}
		if !notify {
			seq++
			msg["id"] = seq
		}
		body, _ := json.Marshal(msg)
		req, _ := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(body))
		for k, v := range server.Headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("%s: HTTP %d %s", method, resp.StatusCode, raw)
		}
		if len(raw) == 0 {
			return nil, nil
		}
		var out map[string]any
		return out, json.Unmarshal(raw, &out)
	}
	if _, err := rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "fake", "version": "0"}}, false); err != nil {
		return err
	}
	if _, err := rpc("notifications/initialized", nil, true); err != nil {
		return err
	}
	listed, err := rpc("tools/list", nil, false)
	if err != nil {
		return err
	}
	names := []string{}
	for _, tool := range listed["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	var calls [][2]json.RawMessage
	if raw := os.Getenv("FAKE_CLAUDE_CALLS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &calls); err != nil {
			return err
		}
	}
	texts := []string{}
	for _, c := range calls {
		var name string
		var arguments map[string]any
		_ = json.Unmarshal(c[0], &name)
		_ = json.Unmarshal(c[1], &arguments)
		reply, err := rpc("tools/call", map[string]any{"name": name, "arguments": arguments}, false)
		if err != nil {
			return err
		}
		content := reply["result"].(map[string]any)["content"].([]any)
		texts = append(texts, content[0].(map[string]any)["text"].(string))
	}
	call.Texts = texts
	logCall(call)
	summary, _ := json.Marshal(map[string]any{"tools": names, "texts": texts})
	result(string(summary), 0.42, nil)
	return nil
}
