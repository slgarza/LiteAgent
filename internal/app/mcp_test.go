package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mockMCP(t *testing.T, stream bool, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mcp-secret" {
			t.Error("missing MCP authentication")
		}
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		var request struct {
			ID     string         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Method != "initialize" && (r.Header.Get("Mcp-Session-Id") != "test-session" || r.Header.Get("MCP-Protocol-Version") != "2025-11-25") {
			t.Error("lost MCP session or negotiated protocol")
		}
		if request.Method == "notifications/initialized" {
			w.WriteHeader(202)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			if r.Header.Get("Accept") != "application/json, text/event-stream" {
				t.Error("missing accepted transports")
			}
			w.Header().Set("Mcp-Session-Id", "test-session")
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "read_report", "description": "Read a report", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}}}}}
		case "tools/call":
			calls.Add(1)
			if request.Params["name"] != "read_report" {
				t.Error("wrong original tool name")
			}
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "Report verified: 42"}}, "structuredContent": map[string]int{"total": 42}}
		default:
			t.Errorf("unexpected MCP method: %s", request.Method)
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			raw, _ := json.Marshal(response)
			fmt.Fprint(w, ": heartbeat\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
		} else {
			sendJSON(w, 200, response)
		}
	}))
}

func TestMCPHTTPAndSSEThroughAgent(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			a := newTestApp(t)
			var calls atomic.Int32
			remote := mockMCP(t, stream, &calls)
			defer remote.Close()
			var modelCalls atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Messages []Message `json:"messages"`
					Tools    []ToolDef `json:"tools"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				if modelCalls.Add(1) == 1 {
					name := ""
					for _, tool := range payload.Tools {
						if strings.HasPrefix(tool.Function.Name, "mcp_") {
							name = tool.Function.Name
						}
					}
					if name == "" {
						t.Error("MCP tool not exposed to model")
					}
					sendJSON(w, 200, map[string]any{"message": Message{Role: "assistant", ToolCalls: []ToolCall{{Type: "function", Function: FunctionCall{name, json.RawMessage(`{}`)}}}}})
					return
				}
				if !strings.Contains(payload.Messages[len(payload.Messages)-1].Content, "Report verified: 42") {
					t.Error("tool observation not forwarded")
				}
				fmt.Fprint(w, `{"message":{"content":"Report verified: 42"}}`)
			}))
			defer model.Close()
			_ = a.store.Update(func(st *State) error {
				st.Config.BaseURL = model.URL
				st.MCPServers = []MCPServer{{ID: "remote", Name: "Reports", URL: remote.URL, Token: "mcp-secret", Enabled: true, TimeoutSec: 5}}
				return nil
			})
			run := postChat(t, a, "", "Read the report using MCP")
			finished := waitRun(t, a, run.ID)
			a.wg.Wait()
			if finished.Status != "completed" || calls.Load() != 1 {
				t.Fatalf("MCP execution failed: %+v", finished)
			}
			found := false
			for _, e := range finished.Events {
				if e.Name == "Reports / read_report" {
					found = true
				}
			}
			if !found {
				t.Fatal("missing readable tool trace")
			}
			public := callAPI(t, a, "GET", "/api/state", nil, nil).Body.String()
			if strings.Contains(public, "mcp-secret") {
				t.Fatal("MCP token leaked")
			}
			w := callAPI(t, a, "POST", "/api/mcp/remote/test", map[string]any{}, nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "read_report") || calls.Load() != 1 {
				t.Fatal("test must discover tools without executing them", w.Body.String())
			}
		})
	}
}

func TestMCPDoesNotRetryAmbiguousActionsOrFollowRedirects(t *testing.T) {
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "upstream lost mcp-secret", 503)
	}))
	defer remote.Close()
	client := &mcpClient{server: MCPServer{Name: "Test", URL: remote.URL, Token: "mcp-secret", TimeoutSec: 5}}
	_, err := (mcpTool{client: client, name: "write"}).call(context.Background(), map[string]any{})
	if err == nil || strings.Contains(err.Error(), "mcp-secret") || calls.Load() != 1 {
		t.Fatal("ambiguous action retried or secret exposed", err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect with credentials") }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	client.server.URL = redirect.URL
	if err = client.request(context.Background(), "tools/list", nil, nil); err == nil {
		t.Fatal("accepted redirect")
	}
}

func TestMCPSettingsPreserveTokenOnlyForSameEndpoint(t *testing.T) {
	a := newTestApp(t)
	input := map[string]any{"name": "Local", "url": "http://localhost:3000/mcp", "token": "keep-me", "enabled": false, "timeout_sec": 10}
	w := callAPI(t, a, "PUT", "/api/mcp", input, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "keep-me") {
		t.Fatal(w.Body.String())
	}
	s := a.store.Snapshot().MCPServers[0]
	input["id"] = s.ID
	input["token"] = ""
	input["enabled"] = true
	w = callAPI(t, a, "PUT", "/api/mcp", input, nil)
	if w.Code != 200 || a.store.Snapshot().MCPServers[0].Token != "keep-me" {
		t.Fatal("token not preserved")
	}
	input["url"] = "http://localhost:3001/mcp"
	w = callAPI(t, a, "PUT", "/api/mcp", input, nil)
	if w.Code != 200 || a.store.Snapshot().MCPServers[0].Token != "" {
		t.Fatal("token carried to a different endpoint")
	}
	for _, url := range []string{"file:///etc/passwd", "http://user:pass@localhost/mcp", "https://example.com/mcp?key=secret"} {
		input["url"] = url
		if w = callAPI(t, a, "PUT", "/api/mcp", input, nil); w.Code != 400 {
			t.Fatal("unsafe endpoint accepted")
		}
	}
}

func TestConnectionsCredentialsAndActivation(t *testing.T) {
	a := newTestApp(t)
	initial := a.store.Snapshot().Config
	w := callAPI(t, a, "PUT", "/api/connections", map[string]any{"name": "Claude", "provider": "anthropic", "base_url": "https://api.anthropic.com", "model": "test", "api_key": "claude-test-secret", "activate": true}, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "claude-test-secret") {
		t.Fatal(w.Body.String())
	}
	var p Connection
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	c := a.store.Snapshot().Config
	if c.APIKey != "claude-test-secret" || c.ConnectionID != p.ID || c.Provider != "anthropic" {
		t.Fatal("profile not activated")
	}
	if w = callAPI(t, a, "DELETE", "/api/connections/"+p.ID, nil, nil); w.Code != 400 {
		t.Fatal("deleted active credentials")
	}
	p.Name = "Renamed"
	p.APIKey = ""
	if w = callAPI(t, a, "PUT", "/api/connections", p, nil); w.Code != 200 || a.store.Snapshot().Config.APIKey != "claude-test-secret" {
		t.Fatal("edit lost existing key")
	}
	w = callAPI(t, a, "PUT", "/api/connections", map[string]any{"id": p.ID, "name": p.Name, "provider": p.Provider, "base_url": p.BaseURL, "model": p.Model, "api_key": "replacement-secret", "clear_api_key": true}, nil)
	if w.Code != 400 || a.store.Snapshot().Config.APIKey != "claude-test-secret" {
		t.Fatal("contradictory key settings silently removed the credential")
	}
	public := callAPI(t, a, "GET", "/api/state", nil, nil).Body.String()
	if strings.Contains(public, "claude-test-secret") {
		t.Fatal("profile credential exposed")
	}
	first := a.store.Snapshot().Connections[0]
	w = callAPI(t, a, "POST", "/api/connections/"+first.ID, map[string]string{"action": "activate"}, nil)
	if w.Code != 200 || a.store.Snapshot().Config.Provider != initial.Provider || a.store.Snapshot().Config.APIKey != "" {
		t.Fatal("activation retained other provider key")
	}
	if w = callAPI(t, a, "DELETE", "/api/connections/"+p.ID, nil, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestMCPRequestCancellation(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Flush an SSE stream so cancellation also covers reading the response body.
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": waiting\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer remote.Close()
	c := &mcpClient{server: MCPServer{Name: "slow", URL: remote.URL, TimeoutSec: 30}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := c.request(ctx, "tools/call", nil, nil)
	if err == nil || time.Since(started) > time.Second {
		t.Fatal("MCP cancellation did not stop response reading", err)
	}
}
