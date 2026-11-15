package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Stateless Responses requests retain opaque reasoning items within the current
// tool loop. They are never rendered or persisted as visible conversation text.
func completeResponses(ctx context.Context, c Config, messages []Message, defs []ToolDef) (Message, error) {
	input := []map[string]any{}
	for _, m := range messages {
		if len(m.Blocks) > 0 {
			input = append(input, m.Blocks...)
			continue
		}
		if m.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
			continue
		}
		input = append(input, map[string]any{"role": m.Role, "content": m.Content})
		for _, tc := range m.ToolCalls {
			input = append(input, map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Function.Name, "arguments": string(tc.Function.Arguments)})
		}
	}
	body := map[string]any{"model": c.Model, "input": input, "store": false, "stream": false, "include": []string{"reasoning.encrypted_content"}}
	if c.reasoningMode() != "auto" {
		body["reasoning"] = map[string]string{"effort": c.reasoningMode()}
	}
	if c.Temperature != nil {
		body["temperature"] = *c.Temperature
	}
	if c.MaxTokens > 0 {
		body["max_output_tokens"] = c.MaxTokens
	}
	if len(defs) > 0 {
		tools := []map[string]any{}
		for _, d := range defs {
			tools = append(tools, map[string]any{"type": "function", "name": d.Function.Name, "description": d.Function.Description, "parameters": d.Function.Parameters, "strict": false})
		}
		body["tools"] = tools
		body["parallel_tool_calls"] = false
	}
	var response struct {
		Status string           `json:"status"`
		Output []map[string]any `json:"output"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Incomplete *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := jsonRequest(ctx, c, http.MethodPost, "/responses", body, &response); err != nil {
		return Message{}, err
	}
	if response.Error != nil {
		detail := response.Error.Message
		if c.APIKey != "" {
			detail = strings.ReplaceAll(detail, c.APIKey, "[key hidden]")
		}
		return Message{}, fmt.Errorf("Responses API: %s", cut(detail, 700))
	}
	if response.Status != "completed" {
		reason := response.Status
		if response.Incomplete != nil {
			reason += " / " + response.Incomplete.Reason
		}
		return Message{}, fmt.Errorf("model response did not complete (%s); increase max_tokens if needed. No partial tool calls were executed", reason)
	}
	m := Message{Role: "assistant", Blocks: response.Output}
	for _, item := range response.Output {
		switch item["type"] {
		case "message":
			content, _ := item["content"].([]any)
			for _, raw := range content {
				block, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if block["type"] == "output_text" {
					value, _ := block["text"].(string)
					m.Content += value
				}
				if block["type"] == "refusal" {
					value, _ := block["refusal"].(string)
					m.Content += value
				}
			}
		case "function_call":
			id, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			args, _ := item["arguments"].(string)
			if id == "" || name == "" || !json.Valid([]byte(args)) {
				return Message{}, errors.New("Responses API returned invalid tool arguments")
			}
			m.ToolCalls = append(m.ToolCalls, ToolCall{id, "function", FunctionCall{name, json.RawMessage(args)}})
		}
	}
	if m.Content == "" && len(m.ToolCalls) == 0 {
		return Message{}, errors.New("Responses API returned no text or tool calls")
	}
	return m, nil
}
