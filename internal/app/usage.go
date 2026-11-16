package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Nil means not reported, including on older saved runs. Never substitute zero
// for an unavailable provider metric or infer billing/quota from these counts.
type TokenUsage struct {
	Input      *int64 `json:"input,omitempty"`
	Output     *int64 `json:"output,omitempty"`
	Cached     *int64 `json:"cached,omitempty"`
	CacheWrite *int64 `json:"cache_write,omitempty"`
	Reasoning  *int64 `json:"reasoning,omitempty"`
}

type RunMetrics struct {
	Requests      int        `json:"requests"`
	UsageReports  int        `json:"usage_reports"`
	Totals        TokenUsage `json:"totals"`
	LastUsage     TokenUsage `json:"last_usage"`
	ModelTimeMS   int64      `json:"model_time_ms"`
	LastRequestAt string     `json:"last_request_at,omitempty"`
}

func tokenSum(a, b *int64) *int64 {
	if a == nil && b == nil {
		return nil
	}
	var n int64
	if a != nil {
		n += *a
	}
	if b != nil {
		n += *b
	}
	return &n
}
func (u *TokenUsage) add(v TokenUsage) {
	u.Input = tokenSum(u.Input, v.Input)
	u.Output = tokenSum(u.Output, v.Output)
	u.Cached = tokenSum(u.Cached, v.Cached)
	u.CacheWrite = tokenSum(u.CacheWrite, v.CacheWrite)
	u.Reasoning = tokenSum(u.Reasoning, v.Reasoning)
}
func (u TokenUsage) available() bool {
	return u.Input != nil || u.Output != nil || u.Cached != nil || u.CacheWrite != nil || u.Reasoning != nil
}

func parseTokenUsage(provider string, data []byte) TokenUsage {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return TokenUsage{}
	}
	fields := raw
	if provider != "ollama" {
		fields = nil
		if json.Unmarshal(raw["usage"], &fields) != nil {
			return TokenUsage{}
		}
	}
	count := func(values map[string]json.RawMessage, name string) *int64 {
		var n *int64
		if json.Unmarshal(values[name], &n) != nil || n == nil || *n < 0 || *n > 1_000_000_000_000 {
			return nil
		}
		return n
	}
	detail := func(parent, name string) *int64 {
		var nested map[string]json.RawMessage
		if json.Unmarshal(fields[parent], &nested) != nil {
			return nil
		}
		return count(nested, name)
	}
	var u TokenUsage
	switch provider {
	case "ollama":
		u.Input, u.Output, u.Cached = count(fields, "prompt_eval_count"), count(fields, "eval_count"), count(fields, "prompt_eval_cached_count")
	case "anthropic":
		u.Input, u.Output = count(fields, "input_tokens"), count(fields, "output_tokens")
		u.Cached, u.CacheWrite = count(fields, "cache_read_input_tokens"), count(fields, "cache_creation_input_tokens")
		// Claude input_tokens excludes cache reads and writes. Other adapters'
		// input counts already include cached tokens.
		if u.Input != nil {
			u.Input = tokenSum(tokenSum(u.Input, u.Cached), u.CacheWrite)
		}
	case "openai-responses":
		u.Input, u.Output = count(fields, "input_tokens"), count(fields, "output_tokens")
		u.Cached, u.CacheWrite = detail("input_tokens_details", "cached_tokens"), detail("input_tokens_details", "cache_write_tokens")
		u.Reasoning = detail("output_tokens_details", "reasoning_tokens")
	case "openai", "vscode":
		u.Input, u.Output = count(fields, "prompt_tokens"), count(fields, "completion_tokens")
		u.Cached = detail("prompt_tokens_details", "cached_tokens")
		u.Reasoning = detail("completion_tokens_details", "reasoning_tokens")
	}
	if u.Input != nil && u.Cached != nil && *u.Cached > *u.Input {
		u.Cached = nil
	}
	if u.Output != nil && u.Reasoning != nil && *u.Reasoning > *u.Output {
		u.Reasoning = nil
	}
	return u
}

type usageContextKey struct{}
type usageCapture struct{ usage TokenUsage }

// Capture only documented numeric usage from this one request. HTTP errors and
// missing metrics remain unknown. The capture never changes provider payloads.
func captureUsage(ctx context.Context, provider string, data []byte) {
	if capture, ok := ctx.Value(usageContextKey{}).(*usageCapture); ok {
		capture.usage = parseTokenUsage(provider, data)
	}
}

func (s *Store) modelRequestStarted(id string) error {
	return s.Update(func(st *State) error {
		for i := range st.Runs {
			if st.Runs[i].ID == id {
				r := &st.Runs[i]
				if r.Metrics == nil {
					r.Metrics = &RunMetrics{}
				}
				r.Metrics.Requests++
				r.Metrics.LastRequestAt = now()
				r.Metrics.LastUsage = TokenUsage{}
				return nil
			}
		}
		return errors.New("task not found")
	})
}
func (s *Store) modelRequestFinished(id string, usage TokenUsage, elapsed time.Duration) error {
	return s.Update(func(st *State) error {
		for i := range st.Runs {
			if st.Runs[i].ID == id {
				m := st.Runs[i].Metrics
				if m == nil {
					return errors.New("model request was not recorded")
				}
				m.ModelTimeMS += max(0, elapsed.Milliseconds())
				m.LastUsage = usage
				if usage.available() {
					m.UsageReports++
					m.Totals.add(usage)
				}
				return nil
			}
		}
		return errors.New("task not found")
	})
}
