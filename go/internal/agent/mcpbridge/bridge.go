// Package mcpbridge is a minimal in-process MCP server that hands LHA's tools to a claude -p
// session (python: lha.agent.mcp_bridge).
//
// The claude_code lead engine switches Claude Code's built-in tools off and gives it these
// instead, so every file write and command still goes through LHA's dispatcher: the sandbox, the
// path rules, the irreversible-command gate and the egress policy apply exactly as they do to the
// built-in loop.
//
// It speaks MCP's Streamable HTTP transport in its simplest form: JSON-RPC over POST, answered
// with a plain application/json body (no SSE stream; GET is 405). It listens on 127.0.0.1 on a
// random port for the length of one cycle and requires a random bearer token, so only the claude
// process LHA started (which gets the token in its --mcp-config) can call it.
//
// Tool calls are served one at a time: the sandbox session is not built for concurrent commands.
// Methods: initialize, ping, tools/list and tools/call; notifications get 202.
//
// It is the standard library only (net/http + encoding/json): the protocol subset is four
// methods over one POST endpoint, and an MCP SDK would add a dependency and a session layer the
// Python bridge (and so the argv/config parity) does not have.
package mcpbridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// ProtocolVersion is the MCP protocol version answered when the client names none.
const ProtocolVersion = "2025-06-18"

const maxBody = 8 * 1024 * 1024

// Handler takes a call's arguments and returns (text for the model, isError). A returned error
// (or a panic) reaches the model as a failed tool result; it never kills the server.
type Handler func(ctx context.Context, arguments map[string]any) (text string, isError bool, err error)

// Tool is one bridged tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     Handler
}

// NewTool builds a Tool whose schema is one MCP accepts: an object schema, even for a tool with
// no parameters (python: bridge_tool).
func NewTool(name, description string, parameters map[string]any, handler Handler) Tool {
	schema := map[string]any{}
	for k, v := range parameters {
		schema[k] = v
	}
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if _, ok := schema["properties"]; !ok {
		schema["properties"] = map[string]any{}
	}
	return Tool{Name: name, Description: description, InputSchema: schema, Handler: handler}
}

// Bridge serves tools over MCP on 127.0.0.1 between Start and Close.
type Bridge struct {
	Name    string
	version string
	order   []string
	tools   map[string]Tool
	token   string

	mu     sync.Mutex // one tool call at a time
	calls  int
	port   int
	server *http.Server
	// ctx is what handlers run under: the session's context, not the HTTP request's, so a
	// client that disconnects mid-call does not abort a command half way (python: asyncio).
	ctx context.Context
}

// New returns a bridge named "lha" (version "0") for tools. A later tool with a name already
// used replaces the earlier one in its position (python: a dict).
func New(tools []Tool) *Bridge {
	b := &Bridge{Name: "lha", version: "0", tools: map[string]Tool{}, token: newToken()}
	for _, t := range tools {
		if _, seen := b.tools[t.Name]; !seen {
			b.order = append(b.order, t.Name)
		}
		b.tools[t.Name] = t
	}
	return b
}

// newToken is python's secrets.token_urlsafe(32).
func newToken() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// URL is the endpoint Claude Code posts to (port 0 before Start).
func (b *Bridge) URL() string { return fmt.Sprintf("http://127.0.0.1:%d/mcp", b.port) }

// Token is the bearer token a client must present.
func (b *Bridge) Token() string { return b.token }

// Calls is the number of tools/call requests served for a known tool.
func (b *Bridge) Calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// MCPConfig is the --mcp-config JSON that points Claude Code at this server, byte-identical to
// Python's json.dumps(bridge.mcp_config()).
func (b *Bridge) MCPConfig() string {
	q := func(s string) string {
		out, _ := json.Marshal(s)
		return string(out)
	}
	return `{"mcpServers": {` + q(b.Name) + `: {"type": "http", "url": ` + q(b.URL()) +
		`, "headers": {"Authorization": ` + q("Bearer "+b.token) + `}}}}`
}

// OpenCodeServer is the mcp.servers.<name> entry that points OpenCode at this server
// (python: McpBridge.opencode_server). codemode is off so the tools keep their native
// <server>_<tool> names and the agent calls them directly.
func (b *Bridge) OpenCodeServer() map[string]any {
	return map[string]any{
		"type":     "remote",
		"url":      b.URL(),
		"headers":  map[string]any{"Authorization": "Bearer " + b.token},
		"codemode": false,
	}
}

// AllowedTools are the --allowedTools entries that pre-approve every bridged tool.
func (b *Bridge) AllowedTools() []string {
	out := make([]string, len(b.order))
	for i, name := range b.order {
		out[i] = "mcp__" + b.Name + "__" + name
	}
	return out
}

// Start listens on 127.0.0.1 on a random port. Handlers run under ctx.
func (b *Bridge) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	b.ctx = ctx
	b.port = ln.Addr().(*net.TCPAddr).Port
	b.server = &http.Server{Handler: http.HandlerFunc(b.serveHTTP), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = b.server.Serve(ln) }()
	return nil
}

// Close stops the server (waiting briefly for requests in flight).
func (b *Bridge) Close() error {
	if b.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := b.server.Shutdown(ctx)
	if err != nil {
		err = b.server.Close()
	}
	b.server = nil
	return err
}

// --- HTTP ---------------------------------------------------------------------------------

func (b *Bridge) serveHTTP(w http.ResponseWriter, r *http.Request) {
	status, payload := b.handleHTTP(r)
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (b *Bridge) handleHTTP(r *http.Request) (int, any) {
	expected := []byte("Bearer " + b.token)
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), expected) != 1 {
		return http.StatusUnauthorized, map[string]any{"error": "unauthorized"}
	}
	switch r.Method {
	case http.MethodDelete:
		return http.StatusOK, nil // session end; there is no session state to drop
	case http.MethodPost:
	default:
		return http.StatusMethodNotAllowed, nil
	}
	if r.ContentLength > maxBody {
		return http.StatusBadRequest, map[string]any{"error": "body too large"}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return http.StatusBadRequest, map[string]any{"error": err.Error()}
	}
	if len(raw) > maxBody {
		return http.StatusBadRequest, map[string]any{"error": "body too large"}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("null")
	}
	var message json.RawMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return http.StatusBadRequest, map[string]any{"error": err.Error()}
	}
	if trimmed := bytes.TrimSpace(message); len(trimmed) > 0 && trimmed[0] == '[' { // a batch
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return http.StatusBadRequest, map[string]any{"error": err.Error()}
		}
		replies := []any{}
		for _, m := range batch {
			if reply := b.rpc(m); reply != nil {
				replies = append(replies, reply)
			}
		}
		if len(replies) == 0 {
			return http.StatusAccepted, nil
		}
		return http.StatusOK, replies
	}
	if reply := b.rpc(message); reply != nil {
		return http.StatusOK, reply
	}
	return http.StatusAccepted, nil
}

// --- JSON-RPC ------------------------------------------------------------------------------

// envelope keeps the id verbatim (echoed as sent) and decodes params the way tools expect
// arguments (encoding/json: float64 numbers).
type envelope struct {
	ID     json.RawMessage `json:"id"`
	Method any             `json:"method"`
	Params any             `json:"params"`
}

func (b *Bridge) rpc(raw json.RawMessage) map[string]any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return rpcError(nil, -32600, "invalid request")
	}
	var msg envelope
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		return rpcError(nil, -32600, "invalid request")
	}
	method, ok := msg.Method.(string)
	if !ok {
		return rpcError(nil, -32600, "invalid request")
	}
	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		return nil // a notification (initialized, cancelled, ...): nothing to answer
	}
	id := msg.ID
	params, _ := msg.Params.(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	switch method {
	case "initialize":
		version, ok := params["protocolVersion"].(string)
		if !ok {
			version = ProtocolVersion
		}
		return rpcOK(id, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": b.Name, "version": b.version},
		})
	case "ping":
		return rpcOK(id, map[string]any{})
	case "tools/list":
		tools := make([]any, 0, len(b.order))
		for _, name := range b.order {
			t := b.tools[name]
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
		}
		return rpcOK(id, map[string]any{"tools": tools})
	case "tools/call":
		return rpcOK(id, b.call(params))
	}
	return rpcError(id, -32601, "method not found: "+method)
}

func (b *Bridge) call(params map[string]any) map[string]any {
	name, isStr := params["name"].(string)
	tool, known := b.tools[name]
	if !isStr || !known {
		return textResult("unknown tool: "+pyfmt.PyReprValue(params["name"]), true)
	}
	arguments, _ := params["arguments"].(map[string]any)
	if arguments == nil {
		arguments = map[string]any{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	text, isError := b.runHandler(tool, arguments)
	return textResult(text, isError)
}

// runHandler runs a tool; a handler error or panic must reach the model, not kill the server.
func (b *Bridge) runHandler(tool Tool, arguments map[string]any) (text string, isError bool) {
	defer func() {
		if p := recover(); p != nil {
			slog.Warn("bridge_tool_failed", "tool", tool.Name, "error", fmt.Sprint(p))
			text, isError = fmt.Sprintf("tool %s failed: panic: %v", tool.Name, p), true
		}
	}()
	ctx := b.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	text, isError, err := tool.Handler(ctx, arguments)
	if err != nil {
		slog.Warn("bridge_tool_failed", "tool", tool.Name, "error", err.Error())
		return fmt.Sprintf("tool %s failed: %s: %v", tool.Name, pyfmt.ExcTypeName(err), err), true
	}
	return text, isError
}

func rpcOK(id json.RawMessage, result map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id json.RawMessage, code int, message string) map[string]any {
	var rid any
	if id != nil {
		rid = id
	}
	return map[string]any{"jsonrpc": "2.0", "id": rid, "error": map[string]any{"code": code, "message": message}}
}

func textResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}
}
