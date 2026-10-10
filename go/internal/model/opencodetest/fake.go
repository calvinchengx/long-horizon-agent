// Package opencodetest is a fake opencode executable for tests of the opencode model backend and
// lead engine (python: the FAKE_OPENCODE script in tests/unit/test_opencode.py).
//
// The fake is the test binary itself: a test package's TestMain calls Main first, and when the
// process was started as the fake (EnvFake=1 in its environment) Main behaves like opencode run
// and exits. Install points a test at it. In "mcp" mode it is a real MCP client: it reads the
// OPENCODE_CONFIG LHA injected, talks JSON-RPC over HTTP to LHA's bridge, calls the tools listed in
// FAKE_OPENCODE_CALLS and prints the newline-delimited JSON events shaped like
// opencode run --format json. So the whole path (argv, injected config, bridge, dispatcher,
// verifier, checkpoint, ledger) is real; only the model is scripted.
//
// Environment (the same as the Python fake): FAKE_OPENCODE_MODE (text | error | hang |
// hang-unpriced | budget | mcp), FAKE_OPENCODE_LOG (append the run's {argv, stdin, cwd, config,
// project_disabled, texts} per run), FAKE_OPENCODE_REPLY (text mode's final text) and
// FAKE_OPENCODE_CALLS ([[name, arguments], ...]).
package opencodetest

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

// EnvFake marks a process started as the fake opencode.
const EnvFake = "LHA_TEST_FAKE_OPENCODE"

// Main runs the fake and exits when this process is the fake opencode; otherwise it returns.
func Main() {
	if os.Getenv(EnvFake) != "1" {
		return
	}
	os.Exit(run(os.Args[1:]))
}

// Install makes the test binary usable as opencode for the rest of the test (the environment is
// inherited by every opencode the code under test starts). It returns the binary to run and the
// path of the call log.
func Install(t *testing.T) (binary, log string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log = filepath.Join(t.TempDir(), "opencode.log")
	t.Setenv(EnvFake, "1")
	t.Setenv("FAKE_OPENCODE_LOG", log)
	return exe, log
}

// Call is one logged run of the fake (python: the fake's log line).
type Call struct {
	Argv            []string       `json:"argv"`
	Stdin           string         `json:"stdin"`
	Cwd             string         `json:"cwd"`
	Config          map[string]any `json:"config"`
	ProjectDisabled string         `json:"project_disabled"`
	// Texts are the tool results the bridge returned (mcp mode).
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

const sessionID = "sess-oc-1"

func tokens() map[string]any {
	return map[string]any{"input": 100, "output": 1000, "cache": map[string]any{"read": 0, "write": 0}}
}

func run(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("0.0.0-fake (OpenCode)")
		return 0
	}
	prompt, _ := io.ReadAll(os.Stdin)
	cwd, _ := os.Getwd()
	config := map[string]any{}
	if path := os.Getenv("OPENCODE_CONFIG"); path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(raw, &config)
		}
	}
	call := Call{Argv: append([]string{}, args...), Stdin: string(prompt), Cwd: cwd, Config: config,
		ProjectDisabled: os.Getenv("OPENCODE_DISABLE_PROJECT_CONFIG")}
	mode := os.Getenv("FAKE_OPENCODE_MODE")
	if mode == "" {
		mode = "text"
	}
	switch mode {
	case "hang":
		logCall(call)
		start("msg-1")
		finish("msg-1", 0.0153, tokens(), "tool-calls")
		time.Sleep(10 * time.Minute)
	case "hang-unpriced":
		logCall(call)
		start("msg-1")
		finishNoCost("msg-1", tokens(), "tool-calls")
		time.Sleep(10 * time.Minute)
	case "budget":
		logCall(call)
		start("msg-1")
		finish("msg-1", 0.9, tokens(), "tool-calls")
		time.Sleep(10 * time.Minute)
	case "text":
		start("msg-1")
		finish("msg-1", 0.01, map[string]any{"input": 100, "output": 50, "cache": map[string]any{"read": 0, "write": 0}}, "stop")
		reply, ok := os.LookupEnv("FAKE_OPENCODE_REPLY")
		if !ok {
			reply = `{"done": true, "summary": "ok"}`
		}
		text(reply)
		logCall(call)
	case "error":
		emit(map[string]any{"type": "error", "sessionID": sessionID,
			"error": map[string]any{"message": "model overloaded 529"}})
		logCall(call)
	case "mcp":
		texts, err := mcp(config)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake opencode:", err)
			return 1
		}
		call.Texts = texts
		logCall(call)
	}
	return 0
}

func logCall(call Call) {
	log := os.Getenv("FAKE_OPENCODE_LOG")
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

func emit(event map[string]any) {
	raw, _ := json.Marshal(event)
	fmt.Println(string(raw))
}

func start(msgID string) {
	emit(map[string]any{"type": "step_start", "sessionID": sessionID,
		"part": map[string]any{"messageID": msgID, "type": "step-start"}})
}

func finish(msgID string, cost float64, stepTokens map[string]any, reason string) {
	emit(map[string]any{"type": "step_finish", "sessionID": sessionID,
		"part": map[string]any{"messageID": msgID, "type": "step-finish", "reason": reason,
			"cost": cost, "tokens": stepTokens}})
}

func finishNoCost(msgID string, stepTokens map[string]any, reason string) {
	emit(map[string]any{"type": "step_finish", "sessionID": sessionID,
		"part": map[string]any{"messageID": msgID, "type": "step-finish", "reason": reason,
			"tokens": stepTokens}})
}

func text(message string) {
	emit(map[string]any{"type": "text", "sessionID": sessionID, "part": map[string]any{"text": message}})
}

func mcp(config map[string]any) ([]string, error) {
	server := serverEntry(config, "lha")
	url, _ := server["url"].(string)
	headers, _ := server["headers"].(map[string]any)
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
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		for k, v := range headers {
			if s, ok := v.(string); ok {
				req.Header.Set(k, s)
			}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
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
		return nil, err
	}
	if _, err := rpc("notifications/initialized", nil, true); err != nil {
		return nil, err
	}
	listed, err := rpc("tools/list", nil, false)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, tool := range listed["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	var calls [][2]json.RawMessage
	if raw := os.Getenv("FAKE_OPENCODE_CALLS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &calls); err != nil {
			return nil, err
		}
	}
	texts := []string{}
	for n, c := range calls {
		var name string
		var arguments map[string]any
		_ = json.Unmarshal(c[0], &name)
		_ = json.Unmarshal(c[1], &arguments)
		msgID := fmt.Sprintf("msg-%d", n+1)
		start(msgID)
		emit(map[string]any{"type": "tool_use", "sessionID": sessionID,
			"part": map[string]any{"id": fmt.Sprintf("tool-%d", n+1), "messageID": msgID,
				"tool": "lha_" + name, "state": map[string]any{"status": "completed", "input": arguments}}})
		reply, err := rpc("tools/call", map[string]any{"name": name, "arguments": arguments}, false)
		if err != nil {
			return nil, err
		}
		content := reply["result"].(map[string]any)["content"].([]any)
		texts = append(texts, content[0].(map[string]any)["text"].(string))
		finish(msgID, 0.21, tokens(), "tool-calls")
	}
	final := fmt.Sprintf("msg-%d", len(calls)+1)
	start(final)
	summary, _ := json.Marshal(map[string]any{"tools": names, "texts": texts})
	text(string(summary))
	finish(final, 0.21, map[string]any{"input": 10, "output": 10, "cache": map[string]any{"read": 0, "write": 0}}, "stop")
	return texts, nil
}

func serverEntry(config map[string]any, name string) map[string]any {
	mcp, _ := config["mcp"].(map[string]any)
	servers, _ := mcp["servers"].(map[string]any)
	server, _ := servers[name].(map[string]any)
	return server
}
