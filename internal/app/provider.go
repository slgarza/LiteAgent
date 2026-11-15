package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type FunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}
type Message struct {
	Thinking   string           `json:"thinking,omitempty"`
	Blocks     []map[string]any `json:"-"`
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []ToolCall       `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolName   string           `json:"tool_name,omitempty"`
}
type FunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}
type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

func jsonRequest(ctx context.Context, c Config, method, path string, body any, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		if c.Provider == "anthropic" {
			req.Header.Set("x-api-key", c.APIKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
	}
	if c.Provider == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	client := &http.Client{Timeout: time.Duration(c.TimeoutSec) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("the provider returned a redirect; check its URL")
	}}
	resp, err := client.Do(req)
	if err != nil {
		return modelRequestError(ctx, c, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return modelRequestError(ctx, c, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := string(b)
		if c.APIKey != "" {
			detail = strings.ReplaceAll(detail, c.APIKey, "[key hidden]")
		}
		return fmt.Errorf("provider HTTP %d: %s", resp.StatusCode, cut(detail, 700))
	}
	if err = json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("invalid provider response: %w", err)
	}
	return nil
}

func modelRequestError(ctx context.Context, c Config, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("model request stopped: %w", ctx.Err())
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		hint := "The server may still be loading or processing the request."
		if c.Provider == "ollama" {
			hint = "Ollama may still be loading the model or processing the conversation on CPU. Try a smaller model or a new chat."
		}
		hint += " You can adjust the request timeout in Settings > Chat & runs."
		if c.TimeoutSec < defaultConfig().TimeoutSec {
			hint += fmt.Sprintf(" To use the default, enter /config timeout %d.", defaultConfig().TimeoutSec)
		}
		return fmt.Errorf("the model did not finish within %d seconds. %s: %w", c.TimeoutSec, hint, err)
	}
	return fmt.Errorf("could not communicate with the model: %w", err)
}

func complete(ctx context.Context, c Config, msgs []Message, defs []ToolDef) (Message, error) {
	if c.Provider == "openai-responses" {
		return completeResponses(ctx, c, msgs, defs)
	}
	if c.Provider == "anthropic" {
		return completeAnthropic(ctx, c, msgs, defs)
	}
	wire := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		v := map[string]any{"role": m.Role, "content": m.Content}
		if c.Provider == "ollama" && m.Thinking != "" {
			v["thinking"] = m.Thinking
		}
		if len(m.ToolCalls) > 0 {
			calls := []map[string]any{}
			for _, tc := range m.ToolCalls {
				var args any = tc.Function.Arguments
				if c.Provider == "openai" || c.Provider == "vscode" {
					args = string(tc.Function.Arguments)
				}
				call := map[string]any{"type": "function", "function": map[string]any{"name": tc.Function.Name, "arguments": args}}
				if c.Provider == "openai" || c.Provider == "vscode" {
					call["id"] = tc.ID
				}
				calls = append(calls, call)
			}
			v["tool_calls"] = calls
		}
		if m.Role == "tool" {
			if c.Provider == "openai" || c.Provider == "vscode" {
				v["tool_call_id"] = m.ToolCallID
			} else {
				v["tool_name"] = m.ToolName
			}
		}
		wire = append(wire, v)
	}
	body := map[string]any{"model": c.Model, "messages": wire, "stream": false}
	if len(defs) > 0 {
		body["tools"] = defs
	}
	path := "/chat/completions"
	if c.Provider == "ollama" {
		path = "/api/chat"
		// Preserve the model's sampling defaults; different model families need
		// different temperatures and penalties to avoid degraded output.
		options := map[string]any{"num_ctx": c.contextTokens()}
		if c.Temperature != nil {
			options["temperature"] = *c.Temperature
		}
		if c.MaxTokens > 0 {
			options["num_predict"] = c.MaxTokens
		}
		body["options"] = options
		switch c.reasoningMode() {
		case "auto":
		case "off":
			body["think"] = false
		case "on":
			body["think"] = true
		default:
			body["think"] = c.reasoningMode()
		}
	} else {
		if c.reasoningMode() != "auto" {
			body["reasoning_effort"] = c.reasoningMode()
		}
		if c.Temperature != nil {
			body["temperature"] = *c.Temperature
		}
		if c.MaxTokens > 0 {
			body["max_completion_tokens"] = c.MaxTokens
		}
	}
	var res struct {
		Message    Message `json:"message"`
		DoneReason string  `json:"done_reason"`
		Choices    []struct {
			FinishReason string  `json:"finish_reason"`
			Message      Message `json:"message"`
		} `json:"choices"`
		Error any `json:"error"`
	}
	if err := jsonRequest(ctx, c, http.MethodPost, path, body, &res); err != nil {
		return Message{}, err
	}
	if res.Error != nil {
		return Message{}, fmt.Errorf("provider error: %v", res.Error)
	}
	m := res.Message
	if c.Provider == "openai" || c.Provider == "vscode" {
		if len(res.Choices) == 0 {
			return m, errors.New("the provider did not return a response")
		}
		if res.Choices[0].FinishReason == "length" {
			return Message{}, errors.New("model output limit reached; increase max_tokens or shorten the request (no partial tool calls executed)")
		}
		m = res.Choices[0].Message
	}
	if res.DoneReason == "length" {
		return Message{}, errors.New("model output limit reached; increase max_tokens")
	}
	m.Role = "assistant"
	for i := range m.ToolCalls {
		tc := &m.ToolCalls[i]
		if tc.ID == "" {
			tc.ID = "call_" + newID()
		}
		tc.Type = "function"
		var str string
		if json.Unmarshal(tc.Function.Arguments, &str) == nil {
			tc.Function.Arguments = json.RawMessage(str)
		}
		if !json.Valid(tc.Function.Arguments) {
			return Message{}, errors.New("the model generated invalid tool arguments")
		}
	}
	if m.Content == "" && len(m.ToolCalls) == 0 {
		return Message{}, errors.New("the model returned an empty response")
	}
	return m, nil
}

func completeAnthropic(ctx context.Context, c Config, msgs []Message, defs []ToolDef) (Message, error) {
	var system string
	wire := []map[string]any{}
	for _, m := range msgs {
		if m.Role == "system" {
			system += m.Content + "\n"
			continue
		}
		role := m.Role
		blocks := []map[string]any{}
		if role == "tool" {
			role = "user"
			blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content})
		} else if len(m.Blocks) > 0 {
			blocks = m.Blocks
		} else {
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": tc.Function.Arguments})
			}
		}
		if len(wire) > 0 && wire[len(wire)-1]["role"] == role {
			prev := wire[len(wire)-1]
			prev["content"] = append(prev["content"].([]map[string]any), blocks...)
		} else {
			wire = append(wire, map[string]any{"role": role, "content": blocks})
		}
	}
	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	body := map[string]any{"model": c.Model, "max_tokens": maxTokens, "system": system, "messages": wire}
	if c.Temperature != nil {
		body["temperature"] = *c.Temperature
	}
	switch c.reasoningMode() {
	case "auto":
	case "off":
		body["thinking"] = map[string]any{"type": "disabled"}
	default:
		body["thinking"] = map[string]any{"type": "adaptive"}
		body["output_config"] = map[string]any{"effort": c.reasoningMode()}
	}
	if len(defs) > 0 {
		ts := []map[string]any{}
		for _, d := range defs {
			ts = append(ts, map[string]any{"name": d.Function.Name, "description": d.Function.Description, "input_schema": d.Function.Parameters})
		}
		body["tools"] = ts
	}
	var res struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
	}
	path := "/v1/messages"
	if strings.HasSuffix(strings.TrimRight(c.BaseURL, "/"), "/v1") {
		path = "/messages"
	}
	if err := jsonRequest(ctx, c, http.MethodPost, path, body, &res); err != nil {
		return Message{}, err
	}
	m := Message{Role: "assistant", Blocks: res.Content}
	for _, b := range res.Content {
		if b["type"] == "text" {
			text, _ := b["text"].(string)
			m.Content += text
		} else if b["type"] == "tool_use" {
			id, _ := b["id"].(string)
			name, _ := b["name"].(string)
			input, err := json.Marshal(b["input"])
			if err != nil || id == "" || name == "" {
				return Message{}, errors.New("invalid Claude tool call")
			}
			m.ToolCalls = append(m.ToolCalls, ToolCall{id, "function", FunctionCall{name, input}})
		}
	}
	if res.StopReason == "max_tokens" {
		return Message{}, errors.New("Claude reached its response limit; simplify the task")
	}
	if m.Content == "" && len(m.ToolCalls) == 0 {
		return m, errors.New("Claude returned an empty response")
	}
	return m, nil
}
