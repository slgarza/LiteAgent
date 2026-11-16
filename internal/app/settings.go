package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
)

// Wire values accepted for explicit overrides. These are not model capabilities
// and must never be used to populate the model picker.
func reasoningModes(provider string) []string {
	switch provider {
	case "ollama":
		return []string{"auto", "off", "on", "low", "medium", "high"}
	case "openai", "openai-responses":
		return []string{"auto", "none", "minimal", "low", "medium", "high", "xhigh", "max"}
	case "anthropic":
		return []string{"auto", "off", "low", "medium", "high", "xhigh", "max"}
	}
	return []string{"auto"}
}
func (c Config) reasoningMode() string {
	if c.Reasoning != "" {
		return c.Reasoning
	}
	if c.Provider == "ollama" && c.Thinking != nil {
		if *c.Thinking {
			return "on"
		}
		return "off"
	}
	return "auto"
}
func (c Config) historyTurns() int {
	if c.HistoryTurns == 0 {
		return 12
	}
	return c.HistoryTurns
}
func (c Config) toolFailureLimit() int {
	if c.MaxToolFailures == 0 {
		return 10
	}
	return c.MaxToolFailures
}
func (c Config) runTimeout() int {
	if c.RunTimeoutSec == 0 {
		return 1800
	}
	return c.RunTimeoutSec
}
func (c Config) contextTokens() int {
	if c.ContextTokens == 0 {
		return 16384
	}
	return c.ContextTokens
}
func (c Config) validateGeneration() error {
	if c.Provider == "vscode" && (c.Temperature != nil || c.MaxTokens != 0) {
		return errors.New("the VS Code bridge uses model defaults; clear temperature and output token overrides")
	}
	if !(c.Provider == "ollama" && validReasoningName(c.reasoningMode())) && !slices.Contains(reasoningModes(c.Provider), c.reasoningMode()) {
		return fmt.Errorf("reasoning for %s must be one of: %s", c.Provider, strings.Join(reasoningModes(c.Provider), ", "))
	}
	if c.Temperature != nil && (math.IsNaN(*c.Temperature) || math.IsInf(*c.Temperature, 0) || *c.Temperature < 0 || *c.Temperature > 2 || c.Provider == "anthropic" && *c.Temperature > 1) {
		return errors.New("temperature must be 0–2 (0–1 for Anthropic), or auto")
	}
	if c.Provider == "anthropic" && c.reasoningMode() != "auto" && c.reasoningMode() != "off" && c.Temperature != nil {
		return errors.New("use automatic temperature with Anthropic adaptive reasoning")
	}
	if c.MaxTokens < 0 || c.MaxTokens > 131072 {
		return errors.New("max_tokens must be 0 (provider default) to 131072")
	}
	if c.ContextTokens != 0 && (c.ContextTokens < 2048 || c.ContextTokens > 262144) {
		return errors.New("context_tokens must be 0 (16384 default) or 2048–262144")
	}
	if c.HistoryTurns < 0 || c.HistoryTurns > 100 {
		return errors.New("history_turns must be 0 (12 default) to 100")
	}
	if c.RunTimeoutSec != 0 && (c.RunTimeoutSec < 10 || c.RunTimeoutSec > 86400) {
		return errors.New("run_timeout must be 10–86400 seconds, or 0 (1800 default)")
	}
	if len(c.SystemPrompt) > 12000 {
		return errors.New("system_prompt must be at most 12000 characters")
	}
	return nil
}

// A generation-only patch cannot overwrite credentials or an endpoint changed in another tab.
func (a *App) generation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider string                     `json:"provider"`
		Model    string                     `json:"model"`
		BaseURL  string                     `json:"base_url"`
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.active != "" {
		fail(w, 409, errors.New("finish or stop the current reply before changing settings"))
		return
	}
	c := a.store.Snapshot().Config
	if input.Provider != c.Provider || input.Model != c.Model || input.BaseURL != c.BaseURL {
		fail(w, 409, errors.New("the model or provider changed; reopen settings"))
		return
	}
	if err := applyGeneration(&c, input.Settings); err != nil {
		fail(w, 400, err)
		return
	}
	if err := c.Validate(); err != nil {
		fail(w, 400, err)
		return
	}
	if _, changed := input.Settings["reasoning"]; changed {
		if err := a.validateModelReasoning(c); err != nil {
			fail(w, 400, err)
			return
		}
	}
	if err := a.store.Update(func(st *State) error { st.Config = c; return nil }); err != nil {
		fail(w, 500, err)
		return
	}
	c.APIKey = ""
	sendJSON(w, 200, c)
}

func applyGeneration(c *Config, settings map[string]json.RawMessage) error {
	for key, value := range settings {
		var target any
		switch key {
		case "reasoning":
			target = &c.Reasoning
			c.Thinking = nil
		case "temperature":
			target = &c.Temperature
		case "max_tokens":
			target = &c.MaxTokens
		case "context_tokens":
			target = &c.ContextTokens
		case "history_turns":
			target = &c.HistoryTurns
		case "run_timeout_sec":
			target = &c.RunTimeoutSec
		case "timeout_sec":
			target = &c.TimeoutSec
		case "max_steps":
			target = &c.MaxSteps
		case "max_tool_failures":
			target = &c.MaxToolFailures
		case "system_prompt":
			target = &c.SystemPrompt
		default:
			return fmt.Errorf("unknown generation setting: %s", key)
		}
		if string(value) == "null" && key != "temperature" {
			return fmt.Errorf("%s cannot be null", key)
		}
		if err := json.Unmarshal(value, target); err != nil {
			return fmt.Errorf("invalid %s: %w", key, err)
		}
	}
	return nil
}
