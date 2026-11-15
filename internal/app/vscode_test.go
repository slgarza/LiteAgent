package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestVSCodeBridgeProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-bridge-token" {
			t.Error("bridge endpoint or token missing")
		}
		var body struct {
			Model    string           `json:"model"`
			Messages []map[string]any `json:"messages"`
			Tools    []ToolDef        `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		call := body.Messages[1]["tool_calls"].([]any)[0].(map[string]any)
		if call["id"] != "call-1" || call["function"].(map[string]any)["arguments"] != `{"path":"~/report.txt"}` || body.Messages[2]["tool_call_id"] != "call-1" || len(body.Tools) != 1 {
			t.Error("VS Code tool history was not encoded as OpenAI chat")
		}
		fmt.Fprint(w, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"call-2","type":"function","function":{"name":"filesystem_stat","arguments":"{}"}}]}}]}`)
	}))
	defer server.Close()
	c := defaultConfig()
	c.Provider, c.Model, c.BaseURL, c.APIKey = "vscode", "fixture/model", server.URL, "fixture-bridge-token"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	messages := []Message{{Role: "user", Content: "Read"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Function: FunctionCall{Name: "filesystem_read", Arguments: json.RawMessage(`{"path":"~/report.txt"}`)}}}}, {Role: "tool", ToolCallID: "call-1", Content: "Report"}}
	result, err := complete(context.Background(), c, messages, []ToolDef{{Type: "function", Function: FunctionDef{Name: "filesystem_stat", Parameters: map[string]any{"type": "object"}}}})
	if err != nil || len(result.ToolCalls) != 1 || result.ToolCalls[0].ID != "call-2" {
		t.Fatalf("bridge result: %+v, %v", result, err)
	}
	if !reflect.DeepEqual(reasoningModes("vscode"), []string{"auto"}) {
		t.Fatal("bridge exposed unsupported reasoning settings")
	}
	c.MaxTokens = 100
	if c.Validate() == nil {
		t.Fatal("unsupported output limit accepted")
	}
}

func TestVSCodeConnectionClearsUnsupportedOverrides(t *testing.T) {
	c := defaultConfig()
	temperature := 0.4
	c.Temperature, c.MaxTokens, c.Reasoning = &temperature, 100, "high"
	p := Connection{Provider: "vscode", BaseURL: providerURL("vscode"), Model: "fixture/model"}
	c = p.config(c)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Temperature != nil || c.MaxTokens != 0 || c.reasoningMode() != "auto" {
		t.Fatal("incompatible generation options carried into VS Code")
	}
}
