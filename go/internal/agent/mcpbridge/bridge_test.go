package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Ported from python/tests/unit/test_claude_code.py (the MCP bridge).

func TestBridgeSpeaksJSONRPCOverHTTPAndChecksTheToken(t *testing.T) {
	var seen []map[string]any
	echo := func(_ context.Context, args map[string]any) (string, bool, error) {
		seen = append(seen, args)
		return "echo " + strings.TrimSuffix(strings.TrimSuffix(jsonText(args["x"]), ".0"), "\n"), false, nil
	}
	boom := func(context.Context, map[string]any) (string, bool, error) { return "", false, errors.New("kaput") }
	panics := func(context.Context, map[string]any) (string, bool, error) { panic("oops") }
	bridge := New([]Tool{
		NewTool("echo", "Echo x", map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{}}}, echo),
		NewTool("boom", "Fails", map[string]any{}, boom),
		NewTool("panics", "Panics", nil, panics),
	})
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()

	var config struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(bridge.MCPConfig()), &config); err != nil {
		t.Fatal(err)
	}
	server := config.MCPServers["lha"]
	if server.Type != "http" || server.URL != bridge.URL() || !regexp.MustCompile(`^http://127\.0\.0\.1:\d+/mcp$`).MatchString(server.URL) {
		t.Fatalf("config %+v", server)
	}
	headers := server.Headers
	if want := []string{"mcp__lha__echo", "mcp__lha__boom", "mcp__lha__panics"}; !reflect.DeepEqual(bridge.AllowedTools(), want) {
		t.Fatalf("allowed %v", bridge.AllowedTools())
	}

	post := func(body any, auth map[string]string) (*http.Response, map[string]any) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, bridge.URL(), bytes.NewReader(raw))
		for k, v := range auth {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}
	rpc := func(body any) map[string]any {
		_, out := post(body, headers)
		return out
	}

	init := rpc(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	result := init["result"].(map[string]any)
	if result["serverInfo"].(map[string]any)["name"] != "lha" || result["protocolVersion"] != ProtocolVersion || init["id"] != float64(1) {
		t.Fatalf("initialize %v", init)
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("capabilities %v", result)
	}
	echoed := rpc(map[string]any{"jsonrpc": "2.0", "id": "s", "method": "initialize", "params": map[string]any{"protocolVersion": "2024-11-05"}})
	if echoed["result"].(map[string]any)["protocolVersion"] != "2024-11-05" || echoed["id"] != "s" {
		t.Fatalf("initialize echo %v", echoed)
	}
	if resp, _ := post(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, headers); resp.StatusCode != 202 {
		t.Fatalf("notification: %d", resp.StatusCode)
	}

	listed := rpc(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	schemas := map[string]any{}
	names := []string{}
	for _, tool := range listed["result"].(map[string]any)["tools"].([]any) {
		m := tool.(map[string]any)
		names = append(names, m["name"].(string))
		schemas[m["name"].(string)] = m["inputSchema"]
	}
	if !reflect.DeepEqual(names, []string{"echo", "boom", "panics"}) {
		t.Fatalf("names %v", names)
	}
	empty := map[string]any{"type": "object", "properties": map[string]any{}} // always an object
	if !reflect.DeepEqual(schemas["boom"], empty) || !reflect.DeepEqual(schemas["panics"], empty) {
		t.Fatalf("schemas %v", schemas)
	}

	reply := rpc(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "echo", "arguments": map[string]any{"x": 7}}})
	want := map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo 7"}}, "isError": false}
	if !reflect.DeepEqual(reply["result"], want) {
		t.Fatalf("echo %v", reply)
	}
	failed := rpc(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "boom"}})["result"].(map[string]any)
	if failed["isError"] != true || !strings.Contains(failed["content"].([]any)[0].(map[string]any)["text"].(string), "kaput") {
		t.Fatalf("boom %v", failed)
	}
	panicked := rpc(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": map[string]any{"name": "panics"}})["result"].(map[string]any)
	if panicked["isError"] != true || !strings.Contains(panicked["content"].([]any)[0].(map[string]any)["text"].(string), "oops") {
		t.Fatalf("panics %v", panicked)
	}
	unknownTool := rpc(map[string]any{"jsonrpc": "2.0", "id": 6, "method": "tools/call", "params": map[string]any{"name": "nope"}})["result"].(map[string]any)
	if unknownTool["isError"] != true || unknownTool["content"].([]any)[0].(map[string]any)["text"] != "unknown tool: 'nope'" {
		t.Fatalf("unknown tool %v", unknownTool)
	}
	unknown := rpc(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "resources/list"})
	if unknown["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("unknown method %v", unknown)
	}
	if pong := rpc(map[string]any{"jsonrpc": "2.0", "id": 8, "method": "ping"}); !reflect.DeepEqual(pong["result"], map[string]any{}) {
		t.Fatalf("ping %v", pong)
	}
	invalid := rpc([]any{1, map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled"}})
	_ = invalid // a batch answers with a list; decoded into a map it is nil

	if resp, _ := post(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "ping"}, nil); resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp, _ := post(map[string]any{"jsonrpc": "2.0", "id": 10, "method": "ping"}, map[string]string{"Authorization": "Bearer nope"}); resp.StatusCode != 401 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	get, _ := http.NewRequest(http.MethodGet, bridge.URL(), nil)
	get.Header.Set("Authorization", headers["Authorization"])
	resp, err := http.DefaultClient.Do(get)
	if err != nil || resp.StatusCode != 405 {
		t.Fatalf("GET: %v %v", resp, err)
	}
	resp.Body.Close()

	if !reflect.DeepEqual(seen, []map[string]any{{"x": float64(7)}}) || bridge.Calls() != 3 {
		t.Fatalf("seen %v calls %d", seen, bridge.Calls())
	}
}

func TestBridgeBatchesAndBadRequests(t *testing.T) {
	bridge := New(nil)
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	send := func(body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, bridge.URL(), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bridge.Token())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.String()
	}
	code, body := send(`[{"jsonrpc": "2.0", "id": 1, "method": "ping"}, {"jsonrpc": "2.0", "method": "x"}, 5]`)
	var replies []map[string]any
	if err := json.Unmarshal([]byte(body), &replies); err != nil || code != 200 || len(replies) != 2 {
		t.Fatalf("%d %s", code, body)
	}
	if replies[1]["error"].(map[string]any)["code"] != float64(-32600) || replies[1]["id"] != nil {
		t.Fatalf("%v", replies[1])
	}
	if code, _ := send(`[{"jsonrpc": "2.0", "method": "x"}]`); code != 202 {
		t.Fatalf("notification batch: %d", code)
	}
	if code, _ := send(`{not json`); code != 400 {
		t.Fatalf("bad json: %d", code)
	}
	if code, body := send(``); code != 200 || !strings.Contains(body, "invalid request") {
		t.Fatalf("empty body: %d %s", code, body)
	}
	if code, body := send(`{"jsonrpc": "2.0", "id": 12345678901234567890, "method": "ping"}`); code != 200 || !strings.Contains(body, `"id":12345678901234567890`) {
		t.Fatalf("big id echoed verbatim: %d %s", code, body)
	}
}

func jsonText(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
