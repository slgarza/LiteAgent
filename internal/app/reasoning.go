package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Only metadata supplied by this connection can advertise model controls.
// Protocol enums in settings.go are for validating manual overrides, not discovery.
type reasoningCapabilities struct {
	Modes   []string `json:"modes"`
	Known   bool     `json:"known"`
	Default string   `json:"default,omitempty"`
	Note    string   `json:"note"`
}

func unknownReasoning() reasoningCapabilities {
	return reasoningCapabilities{Modes: []string{"auto"}, Note: "This provider does not report reasoning controls for this model. Auto uses its default."}
}

func reasoningKey(c Config) [32]byte {
	b, _ := json.Marshal([]string{c.Provider, c.BaseURL, c.Model, c.APIKey})
	return sha256.Sum256(b)
}

func (a *App) cachedReasoning(c Config) reasoningCapabilities {
	a.reasoningMu.Lock()
	defer a.reasoningMu.Unlock()
	if a.reasoningKey == reasoningKey(c) && a.reasoningInfo.Modes != nil {
		return a.reasoningInfo
	}
	return unknownReasoning()
}

func (a *App) discoverReasoning(ctx context.Context, c Config) reasoningCapabilities {
	a.reasoningMu.Lock()
	a.reasoningRequest++
	request := a.reasoningRequest
	a.reasoningMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := modelReasoning(ctx, c)
	if err != nil {
		info = unknownReasoning()
		// Provider errors can contain credentials or arbitrary model metadata.
		info.Note = "Could not read this model's reasoning controls. Use Refresh models to retry. Auto uses its default."
	}
	current := reasoningKey(a.store.Snapshot().Config)
	a.reasoningMu.Lock()
	if request == a.reasoningRequest && current == reasoningKey(c) {
		a.reasoningKey, a.reasoningInfo = current, info
	}
	a.reasoningMu.Unlock()
	return info
}

func (a *App) reasoning(w http.ResponseWriter, r *http.Request) {
	c := a.store.Snapshot().Config
	info := a.discoverReasoning(r.Context(), c)
	sendJSON(w, 200, map[string]any{"provider": c.Provider, "base_url": c.BaseURL, "model": c.Model, "reasoning": info})
}

func modelReasoning(ctx context.Context, c Config) (reasoningCapabilities, error) {
	info := unknownReasoning()
	if c.Provider == "vscode" {
		info.Known = true
		info.Note = "The VS Code bridge uses the model's default reasoning; it exposes no overrides."
		return info, nil
	}
	add := func(mode string) {
		if validReasoningName(mode) && !slices.Contains(info.Modes, mode) {
			info.Modes = append(info.Modes, mode)
		}
	}
	if c.Provider == "ollama" {
		var data struct {
			Thinking *struct {
				Values  []any `json:"values"`
				Default any   `json:"default"`
			} `json:"thinking"`
		}
		if err := jsonRequest(ctx, c, http.MethodPost, "/api/show", map[string]string{"model": c.Model}, &data); err != nil {
			return info, err
		}
		if data.Thinking != nil && len(data.Thinking.Values) > 0 {
			for _, value := range data.Thinking.Values {
				add(thinkingValue(value))
			}
			info.Known = len(info.Modes) > 1
			if value := thinkingValue(data.Thinking.Default); slices.Contains(info.Modes, value) {
				info.Default = value
			}
		}
	} else {
		path := "/models/" + url.PathEscape(c.Model)
		if c.Provider == "anthropic" && !strings.HasSuffix(strings.TrimRight(c.BaseURL, "/"), "/v1") {
			path = "/v1" + path
		}
		type support struct {
			Supported bool `json:"supported"`
		}
		var data struct {
			// Optional extension used by compatible endpoints. Standard OpenAI
			// model metadata currently does not advertise reasoning levels.
			Efforts      []string `json:"supported_reasoning_efforts"`
			Capabilities *struct {
				Thinking *struct {
					Supported bool               `json:"supported"`
					Types     map[string]support `json:"types"`
				} `json:"thinking"`
				Effort map[string]json.RawMessage `json:"effort"`
			} `json:"capabilities"`
		}
		if err := jsonRequest(ctx, c, http.MethodGet, path, nil, &data); err != nil {
			return info, err
		}
		if c.Provider == "anthropic" {
			if caps := data.Capabilities; caps != nil && caps.Thinking != nil {
				info.Known = true
				if caps.Thinking.Types["disabled"].Supported {
					add("off")
				}
				if caps.Thinking.Supported && caps.Thinking.Types["adaptive"].Supported {
					for _, mode := range reasoningModes(c.Provider) {
						var available support
						if json.Unmarshal(caps.Effort[mode], &available) == nil && available.Supported {
							add(mode)
						}
					}
				}
			}
		} else if data.Efforts != nil {
			info.Known = true
			for _, mode := range data.Efforts {
				if slices.Contains(reasoningModes(c.Provider), mode) {
					add(mode)
				}
			}
		}
	}
	if info.Known {
		info.Note = "Controls reported by this model's API. Auto uses the model default."
		if len(info.Modes) == 1 || slices.Equal(info.Modes, []string{"auto", "off"}) {
			info.Note = "This model reports no adjustable thinking levels. Auto uses the model default."
		}
	}
	return info, nil
}

func thinkingValue(value any) string {
	switch value := value.(type) {
	case bool:
		if value {
			return "on"
		}
		return "off"
	case string:
		return value
	}
	return ""
}

func validReasoningName(mode string) bool {
	if len(mode) == 0 || len(mode) > 64 {
		return false
	}
	for _, ch := range mode {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

func (a *App) validateModelReasoning(c Config) error {
	info := a.cachedReasoning(c)
	if info.Known && !slices.Contains(info.Modes, c.reasoningMode()) {
		return fmt.Errorf("reasoning for %s must be one of the reported controls: %s", c.Model, strings.Join(info.Modes, ", "))
	}
	return nil
}

func (a *App) reasoningReply(c Config) string {
	info := a.cachedReasoning(c)
	return "Reasoning: **" + c.reasoningMode() + "**. Model: `" + c.Model + "`.\n\n" + info.Note + "\n\nAvailable controls: " + strings.Join(info.Modes, ", ") + "."
}
