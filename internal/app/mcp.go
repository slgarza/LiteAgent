package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type MCPServer struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	Token      string `json:"token,omitempty"`
	TokenSet   bool   `json:"token_set,omitempty"`
	Enabled    bool   `json:"enabled"`
	TimeoutSec int    `json:"timeout_sec"`
}

func (s MCPServer) validate() error {
	u, err := url.Parse(s.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("MCP needs an HTTP(S) endpoint without embedded credentials or query parameters")
	}
	if strings.TrimSpace(s.Name) == "" || len(s.Name) > 120 || len(s.Token) > 8192 {
		return errors.New("provide a server name up to 120 characters and a valid token")
	}
	if s.TimeoutSec < 5 || s.TimeoutSec > 120 {
		return errors.New("MCP timeout must be 5–120 seconds")
	}
	return nil
}

type mcpClient struct {
	server           MCPServer
	session, version string
}
type mcpTool struct {
	client *mcpClient
	name   string
	def    ToolDef
}
type mcpReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *mcpClient) scrub(text string) string {
	if c.server.Token != "" {
		text = strings.ReplaceAll(text, c.server.Token, "[token hidden]")
	}
	return text
}
func (c *mcpClient) request(ctx context.Context, method string, params any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.server.TimeoutSec)*time.Second)
	defer cancel()
	id := newID()
	payload := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		payload["params"] = params
	}
	notification := strings.HasPrefix(method, "notifications/")
	if !notification {
		payload["id"] = id
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.server.URL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.server.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.server.Token)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.version != "" {
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("MCP redirects are not followed; use the final endpoint URL")
	}}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("MCP %s: %s", c.server.Name, c.scrub(err.Error()))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("MCP %s HTTP %d: %s", c.server.Name, response.StatusCode, c.scrub(cut(string(body), 700)))
	}
	if method == "initialize" {
		c.session = response.Header.Get("Mcp-Session-Id")
	}
	if notification {
		return nil
	}
	consume := func(raw []byte) (bool, error) {
		var reply mcpReply
		if err := json.Unmarshal(raw, &reply); err != nil {
			return false, errors.New("invalid MCP JSON response")
		}
		var replyID string
		if json.Unmarshal(reply.ID, &replyID) != nil || replyID != id {
			return false, nil
		}
		if reply.JSONRPC != "2.0" {
			return true, errors.New("invalid MCP JSON-RPC version")
		}
		if reply.Error != nil {
			return true, fmt.Errorf("MCP %s (%d): %s", c.server.Name, reply.Error.Code, c.scrub(reply.Error.Message))
		}
		if len(reply.Result) == 0 {
			return true, errors.New("MCP response is missing its result")
		}
		if out != nil {
			return true, json.Unmarshal(reply.Result, out)
		}
		return true, nil
	}
	reader := io.LimitReader(response.Body, 4<<20)
	if strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		var event strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if event.Len() > 0 {
					matched, err := consume([]byte(event.String()))
					if err != nil || matched {
						return err
					}
					event.Reset()
				}
			} else if strings.HasPrefix(line, "data:") {
				event.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				event.WriteByte('\n')
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("MCP stream: %w", err)
		}
		if event.Len() > 0 {
			matched, err := consume([]byte(event.String()))
			if err != nil || matched {
				return err
			}
		}
		return errors.New("MCP stream ended without a matching result; action was not retried")
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	matched, err := consume(raw)
	if err != nil {
		return err
	}
	if !matched {
		return errors.New("MCP response ID did not match; action was not retried")
	}
	return nil
}
func (c *mcpClient) close() {
	if c.session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.server.URL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", c.session)
	req.Header.Set("MCP-Protocol-Version", c.version)
	if c.server.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.server.Token)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
}
func connectMCP(ctx context.Context, s MCPServer) (*mcpClient, []mcpTool, error) {
	if err := s.validate(); err != nil {
		return nil, nil, err
	}
	c := &mcpClient{server: s}
	var init struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
	}
	err := c.request(ctx, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "LiteAgent", "version": Version}}, &init)
	if err != nil {
		c.close()
		return nil, nil, err
	}
	switch init.ProtocolVersion {
	case "2025-11-25", "2025-06-18", "2025-03-26":
		c.version = init.ProtocolVersion
	default:
		c.close()
		return nil, nil, errors.New("unsupported MCP protocol version")
	}
	if err = c.request(ctx, "notifications/initialized", nil, nil); err != nil {
		c.close()
		return nil, nil, err
	}
	result := []mcpTool{}
	if _, ok := init.Capabilities["tools"]; !ok {
		return c, result, nil
	}
	cursor := ""
	seen := map[string]bool{}
	names := map[string]bool{}
	for page := 0; page < 8; page++ {
		var list struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err = c.request(ctx, "tools/list", params, &list); err != nil {
			c.close()
			return nil, nil, err
		}
		for _, t := range list.Tools {
			if t.Name == "" || names[t.Name] || t.InputSchema["type"] != "object" {
				c.close()
				return nil, nil, errors.New("MCP returned duplicate names or invalid tool schemas")
			}
			if len(result) >= 64 {
				c.close()
				return nil, nil, errors.New("MCP server exceeds the limit of 64 tools")
			}
			names[t.Name] = true
			digest := sha256.Sum256([]byte(s.ID + "\x00" + t.Name))
			alias := fmt.Sprintf("mcp_%x", digest[:12])
			result = append(result, mcpTool{client: c, name: t.Name, def: ToolDef{"function", FunctionDef{alias, cut(s.Name+" / "+t.Name+": "+t.Description, 2000), t.InputSchema}}})
		}
		if list.NextCursor == "" {
			return c, result, nil
		}
		if seen[list.NextCursor] {
			c.close()
			return nil, nil, errors.New("MCP returned a repeated pagination cursor")
		}
		cursor = list.NextCursor
		seen[cursor] = true
	}
	c.close()
	return nil, nil, errors.New("MCP tools/list pagination limit reached")
}
func (t mcpTool) call(ctx context.Context, args map[string]any) (string, error) {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
	}
	// Tool calls are never retried: a lost response may already have caused an external action.
	if err := t.client.request(ctx, "tools/call", map[string]any{"name": t.name, "arguments": args}, &result); err != nil {
		return "", err
	}
	parts := []string{}
	for _, content := range result.Content {
		if content.Type == "text" {
			parts = append(parts, content.Text)
		} else {
			parts = append(parts, "[MCP returned "+content.Type+" content; only text and structured results are supported]")
		}
	}
	if len(result.Structured) > 0 {
		parts = append(parts, string(result.Structured))
	}
	output := t.client.scrub(cut(strings.Join(parts, "\n"), 18000))
	if result.IsError {
		return "", fmt.Errorf("MCP tool error: %s", output)
	}
	return output, nil
}

func (a *App) mcpAPI(w http.ResponseWriter, r *http.Request) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.active != "" {
		fail(w, 409, errors.New("stop the current run before changing MCP servers"))
		return
	}
	if r.Method == "DELETE" {
		err := a.store.Update(func(st *State) error {
			for i, s := range st.MCPServers {
				if s.ID == r.PathValue("id") {
					st.MCPServers = append(st.MCPServers[:i], st.MCPServers[i+1:]...)
					return nil
				}
			}
			return errors.New("MCP server not found")
		})
		if err != nil {
			fail(w, 404, err)
			return
		}
		sendJSON(w, 200, map[string]bool{"deleted": true})
		return
	}
	var input struct {
		MCPServer
		ClearToken bool `json:"clear_token"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	s := input.MCPServer
	if input.ClearToken {
		s.Token = ""
	}
	s.Name = strings.TrimSpace(s.Name)
	s.URL = strings.TrimSpace(s.URL)
	s.TokenSet = false
	if s.TimeoutSec == 0 {
		s.TimeoutSec = 30
	}
	if err := s.validate(); err != nil {
		fail(w, 400, err)
		return
	}
	fresh := s.ID == ""
	if fresh {
		s.ID = newID()
	}
	err := a.store.Update(func(st *State) error {
		for i, old := range st.MCPServers {
			if old.ID == s.ID {
				if s.Token == "" && !input.ClearToken && s.URL == old.URL {
					s.Token = old.Token
				}
				st.MCPServers[i] = s
				return nil
			}
		}
		if !fresh {
			return errors.New("MCP server not found")
		}
		if len(st.MCPServers) >= 12 {
			return errors.New("MCP server limit reached")
		}
		st.MCPServers = append(st.MCPServers, s)
		return nil
	})
	if err != nil {
		fail(w, 400, err)
		return
	}
	s.TokenSet = s.Token != ""
	s.Token = ""
	sendJSON(w, 200, s)
}
func (a *App) testMCP(w http.ResponseWriter, r *http.Request) {
	for _, s := range a.store.Snapshot().MCPServers {
		if s.ID == r.PathValue("id") {
			ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
			defer cancel()
			client, ts, err := connectMCP(ctx, s)
			if err != nil {
				fail(w, 502, err)
				return
			}
			defer client.close()
			names := []string{}
			for _, t := range ts {
				names = append(names, t.name)
			}
			sendJSON(w, 200, map[string]any{"tools": names, "protocol": client.version})
			return
		}
	}
	fail(w, 404, errors.New("MCP server not found"))
}
