package serve

// MCP over the UI API: the tools of mcp.json (a copy of spec/serve/mcp.json), each one operation
// of spec/serve/openapi.json (python: lha.serve.mcp).
//
// A tool call is sent through this server's own routes, in process, so a tool answers exactly what
// its operation answers and is checked by the same contract: a 2xx response is the tool's
// structuredContent; an error response is a tool result with isError and the API's error body.
// There is no tool for answering a gate, aborting or editing the checklist (not_tools): an agent
// must not approve its own irreversible actions, which is what the human gate is for.
//
// Transports: lha serve's /mcp (Streamable HTTP answered with plain JSON: no SSE, GET is 405; it
// needs the token as X-LHA-Token or Authorization: Bearer) and lha mcp (newline-delimited JSON-RPC
// on stdin and stdout).

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

//go:embed mcp.json
var mcpJSON []byte

// MCPTool is one tool of mcp.json.
type MCPTool struct {
	Name        string            `json:"name"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Operation   string            `json:"operation"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	Arguments   map[string]string `json:"arguments"`
	InputSchema json.RawMessage   `json:"inputSchema"`
	Annotations json.RawMessage   `json:"annotations"`
}

// MCPSpec is mcp.json (generated from spec/serve/ by python/scripts/export_spec.py; a test checks
// the copy).
type MCPSpec struct {
	ProtocolVersion string            `json:"protocol_version"`
	Server          map[string]string `json:"server"`
	Tools           []MCPTool         `json:"tools"`
	NotTools        map[string]string `json:"not_tools"`
}

// MCP is the parsed mcp.json.
var MCP = func() MCPSpec {
	var spec MCPSpec
	if err := json.Unmarshal(mcpJSON, &spec); err != nil {
		panic(fmt.Sprintf("serve: mcp.json: %v", err))
	}
	return spec
}()

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

// protocolVersions are the MCP versions this server speaks, its own first.
func protocolVersions() []string { return []string{MCP.ProtocolVersion, "2025-03-26"} }

type rpcError struct {
	code    int
	message string
}

func rpcErr(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

// handleMCP is the response to one message (raw JSON), or nil for a notification.
func (s *Server) handleMCP(ctx context.Context, raw []byte) map[string]any {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return rpcErr(nil, rpcParseError, "not JSON")
	}
	message, ok := decoded.(map[string]any)
	if !ok {
		return rpcErr(nil, rpcInvalidRequest, "a message must be one JSON-RPC object")
	}
	id := message["id"]
	method, isString := message["method"].(string)
	if message["jsonrpc"] != "2.0" || !isString {
		return rpcErr(id, rpcInvalidRequest, "not a JSON-RPC 2.0 request")
	}
	if _, has := message["id"]; !has {
		return nil // a notification (notifications/initialized, cancelled, ...)
	}
	params := map[string]any{}
	if p, present := message["params"]; present && p != nil {
		if params, ok = p.(map[string]any); !ok {
			return rpcErr(id, rpcInvalidParams, "params must be an object")
		}
	}
	result, e := s.dispatchMCP(ctx, method, params)
	if e != nil {
		return rpcErr(id, e.code, e.message)
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func (s *Server) dispatchMCP(ctx context.Context, method string, params map[string]any) (map[string]any, *rpcError) {
	switch method {
	case "initialize":
		version := protocolVersions()[0]
		if asked, _ := params["protocolVersion"].(string); slices.Contains(protocolVersions(), asked) {
			version = asked
		}
		info := map[string]any{"version": s.Version}
		for k, v := range MCP.Server {
			info[k] = v
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      info,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools := make([]map[string]any, len(MCP.Tools))
		for i, t := range MCP.Tools {
			tools[i] = map[string]any{
				"name": t.Name, "title": t.Title, "description": t.Description,
				"inputSchema": t.InputSchema, "annotations": t.Annotations,
			}
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		return s.callTool(ctx, params)
	}
	return nil, &rpcError{rpcMethodNotFound, fmt.Sprintf("no method %s", contracts.PyRepr(method))}
}

func (s *Server) callTool(ctx context.Context, params map[string]any) (map[string]any, *rpcError) {
	name, _ := params["name"].(string)
	i := slices.IndexFunc(MCP.Tools, func(t MCPTool) bool { return t.Name == name })
	if i < 0 {
		return nil, &rpcError{rpcInvalidParams, fmt.Sprintf("unknown tool: %s", contracts.PyRepr(name))}
	}
	tool := MCP.Tools[i]
	arguments := map[string]any{}
	if a, present := params["arguments"]; present && a != nil {
		var ok bool
		if arguments, ok = a.(map[string]any); !ok {
			return nil, &rpcError{rpcInvalidParams, "arguments must be an object"}
		}
	}
	var unexpected, missing []string
	for name := range arguments {
		if _, known := tool.Arguments[name]; !known {
			unexpected = append(unexpected, name)
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return nil, &rpcError{rpcInvalidParams, "unexpected argument(s): " + strings.Join(unexpected, ", ")}
	}
	var schema struct {
		Required []string `json:"required"`
	}
	_ = json.Unmarshal(tool.InputSchema, &schema)
	for _, name := range schema.Required {
		if _, present := arguments[name]; !present {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, &rpcError{rpcInvalidParams, "missing argument(s): " + strings.Join(missing, ", ")}
	}
	path, query, body := tool.Path, url.Values{}, map[string]any{}
	for name, value := range arguments {
		switch tool.Arguments[name] {
		case "path":
			v, ok := value.(string)
			if !ok || v == "" {
				return nil, &rpcError{rpcInvalidParams, name + " must be a non-empty string"}
			}
			path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(v))
		case "query":
			v, err := queryValue(name, value)
			if err != nil {
				return nil, err
			}
			query.Set(name, v)
		default:
			body[name] = value
		}
	}
	var payload io.Reader
	if tool.Method == http.MethodPost {
		raw, _ := json.Marshal(body)
		payload = bytes.NewReader(raw)
	}
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req := httptest.NewRequestWithContext(ctx, tool.Method, target, payload)
	req.Host = fmt.Sprintf("127.0.0.1:%d", s.Port)
	req.Header.Set("X-LHA-Token", s.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var response any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			response = map[string]any{"error": map[string]any{"code": "internal", "message": err.Error()}}
		}
	}
	failed := rec.Code < 200 || rec.Code >= 300
	var text string
	if failed {
		body, _ := response.(map[string]any)
		e, _ := body["error"].(map[string]any)
		text = fmt.Sprintf("%d %v: %v", rec.Code, e["code"], e["message"])
	} else {
		raw, _ := json.Marshal(response)
		text = string(raw)
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": text}},
		"structuredContent": response,
		"isError":           failed,
	}, nil
}

func queryValue(name string, value any) (string, *rpcError) {
	switch v := value.(type) {
	case bool:
		return strconv.FormatBool(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case string:
		return v, nil
	}
	return "", &rpcError{rpcInvalidParams, name + " must be a string or a number"}
}

// mcpEndpoint is POST /mcp: JSON-RPC answered with plain JSON. A client sends the token as
// X-LHA-Token or Authorization: Bearer; the UI's cookie is not enough.
func (s *Server) mcpEndpoint(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-LHA-Token")
	if auth := r.Header.Get("Authorization"); token == "" && len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		token = strings.TrimSpace(auth[7:])
	}
	if err := s.guardHost(r); err != nil {
		writeError(w, err)
		return
	}
	if token == "" || !s.validToken(token) {
		writeError(w, fail(401, "unauthorized", "a valid X-LHA-Token or bearer token is required"))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, fail(400, "invalid_request", "%v", err))
		return
	}
	response := s.handleMCP(r.Context(), raw)
	if response == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	status := 200
	if e, ok := response["error"].(map[string]any); ok && (e["code"] == rpcParseError || e["code"] == rpcInvalidRequest) {
		status = 400
	}
	writeJSON(w, status, response)
}

// ServeMCPStdio answers newline-delimited JSON-RPC from in on out, one response per line, until
// in ends (lha mcp).
func (s *Server) ServeMCPStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if response := s.handleMCP(ctx, line); response != nil {
			if err := enc.Encode(response); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
