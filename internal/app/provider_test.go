package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestModelTimeoutExplainsSlowInference(t *testing.T) {
	for _, headersSent := range []bool{false, true} {
		t.Run(fmt.Sprintf("headers_sent_%t", headersSent), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if headersSent {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			c := defaultConfig()
			c.BaseURL, c.TimeoutSec = server.URL, 1
			var response any
			err := jsonRequest(context.Background(), c, http.MethodGet, "/", nil, &response)
			if err == nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected a wrapped timeout, got %v", err)
			}
			for _, want := range []string{"within 1 seconds", "Ollama", "Settings > Chat & runs", "/config timeout 900"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("missing %q in %v", want, err)
				}
			}
		})
	}
}

func TestModelCancellationIsNotReportedAsSlowInference(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := modelRequestError(ctx, defaultConfig(), context.DeadlineExceeded)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "/config timeout") {
		t.Fatalf("incorrect cancellation error: %v", err)
	}
}

func TestOllamaThinkingPreference(t *testing.T) {
	for _, mode := range []string{"auto", "false", "true"} {
		t.Run(mode, func(t *testing.T) {
			a := newTestApp(t)
			postChat(t, a, "", "/config thinking "+mode)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				value, present := payload["think"]
				if mode == "auto" && present || mode != "auto" && (!present || value != (mode == "true")) {
					t.Errorf("incorrect thinking preference: %v (present: %t)", value, present)
				}
				fmt.Fprint(w, `{"message":{"role":"assistant","content":"Hello"}}`)
			}))
			defer server.Close()
			c := a.store.Snapshot().Config
			c.BaseURL = server.URL
			if _, err := complete(context.Background(), c, []Message{{Role: "user", Content: "Hello"}}, nil); err != nil {
				t.Fatal(err)
			}
			postChat(t, a, "", "/config thinking auto")
			if a.store.Snapshot().Config.Thinking != nil {
				t.Fatal("auto did not restore the model default")
			}
		})
	}
}

func TestAgentPromptRetainsCacheablePrefixAndTurnContext(t *testing.T) {
	c := defaultConfig()
	current := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	history := []Message{{Role: "user", Content: "Earlier request"}, {Role: "assistant", Content: "Earlier reply"}}
	run := Run{Task: "Run the report", Origin: "scheduled"}
	skills := []Skill{{ID: "report", Instructions: "Verify period"}}
	first := agentMessages(run, c, skills, history, current)
	later := agentMessages(Run{Task: "hello"}, c, nil, history, current.Add(time.Minute))
	if first[0].Role != later[0].Role || first[0].Content != later[0].Content {
		t.Fatal("turn metadata invalidated the system prompt prefix")
	}
	if len(first) != 4 || first[1].Content != history[0].Content || first[2].Content != history[1].Content {
		t.Fatal("conversation history was not preserved")
	}
	for _, want := range []string{"2026-11-13 14:00", c.Timezone, "already scheduled", "Verify period", run.Task} {
		if !strings.Contains(first[3].Content, want) {
			t.Errorf("missing turn context %q", want)
		}
	}
	if !strings.Contains(later[3].Content, "14:01") || strings.Contains(later[3].Content, "Verify period") {
		t.Fatal("turn context is stale")
	}
}
