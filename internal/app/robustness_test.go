package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenerationWireOptions(t *testing.T) {
	for _, provider := range []string{"ollama", "openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			c := defaultConfig()
			c.Provider = provider
			c.Reasoning = "high"
			c.MaxTokens = 16384
			c.ContextTokens = 32768
			temperature := 0.25
			if provider != "anthropic" {
				c.Temperature = &temperature
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p map[string]any
				_ = json.NewDecoder(r.Body).Decode(&p)
				switch provider {
				case "ollama":
					o := p["options"].(map[string]any)
					if p["think"] != "high" || o["num_ctx"] != float64(32768) || o["num_predict"] != float64(16384) || o["temperature"] != 0.25 {
						t.Errorf("wrong Ollama options: %v", p)
					}
					fmt.Fprint(w, `{"message":{"content":"ok"}}`)
				case "openai":
					if p["reasoning_effort"] != "high" || p["max_completion_tokens"] != float64(16384) || p["temperature"] != 0.25 {
						t.Errorf("wrong OpenAI options: %v", p)
					}
					fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
				case "anthropic":
					if p["thinking"].(map[string]any)["type"] != "adaptive" || p["output_config"].(map[string]any)["effort"] != "high" || p["max_tokens"] != float64(16384) {
						t.Errorf("wrong Claude options: %v", p)
					}
					fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
				}
			}))
			defer server.Close()
			c.BaseURL = server.URL
			if _, err := complete(context.Background(), c, []Message{{Role: "user", Content: "hello"}}, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAnthropicPreservesOpaqueThinkingBlocksAcrossTools(t *testing.T) {
	c := defaultConfig()
	c.Provider = "anthropic"
	c.Reasoning = "high"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			fmt.Fprint(w, `{"content":[{"type":"thinking","thinking":"opaque test data","signature":"signed-block"},{"type":"tool_use","id":"one","name":"skills_list","input":{}}],"stop_reason":"tool_use"}`)
			return
		}
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		messages := p["messages"].([]any)
		blocks := messages[1].(map[string]any)["content"].([]any)
		if blocks[0].(map[string]any)["signature"] != "signed-block" || len(blocks) != 2 {
			t.Errorf("lost provider blocks: %v", blocks)
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	}))
	defer server.Close()
	c.BaseURL = server.URL
	messages := []Message{{Role: "user", Content: "list skills"}}
	m, err := complete(context.Background(), c, messages, toolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	messages = append(messages, m, Message{Role: "tool", ToolCallID: "one", Content: "[]"})
	if _, err = complete(context.Background(), c, messages, toolDefinitions()); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationPatchValidatesAndPreservesCredentials(t *testing.T) {
	a := newTestApp(t)
	_ = a.store.Update(func(st *State) error { st.Config.APIKey = "preserved-secret"; return nil })
	before := a.store.Snapshot().Config
	input := map[string]any{"provider": before.Provider, "base_url": before.BaseURL, "model": before.Model, "settings": map[string]any{"reasoning": "off", "temperature": 0.0, "history_turns": 8, "max_steps": 30, "max_tool_failures": 7, "timeout_sec": 1200, "run_timeout_sec": 1800}}
	w := callAPI(t, a, "PATCH", "/api/generation", input, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "preserved-secret") {
		t.Fatal(w.Body.String())
	}
	after := a.store.Snapshot().Config
	if after.MaxSteps != 30 || after.toolFailureLimit() != 7 || after.APIKey != before.APIKey || after.Temperature == nil || *after.Temperature != 0 || after.reasoningMode() != "off" || after.TimeoutSec != 1200 || after.runTimeout() != 1800 {
		t.Fatal("lost settings or credential")
	}
	for _, settings := range []map[string]any{{"reasoning": "invalid mode"}, {"temperature": -1}, {"max_tokens": -1}, {"history_turns": 101}, {"timeout_sec": 9}, {"timeout_sec": 86401}, {"timeout_sec": nil}, {"timeout_sec": 900.5}, {"run_timeout_sec": 1}, {"context_tokens": 100}, {"api_key": "overwritten"}, {"max_steps": 0}, {"max_tool_failures": -1}, {"max_tool_failures": 51}, {"max_tool_failures": nil}, {"reasoning": nil}} {
		input["settings"] = settings
		if w := callAPI(t, a, "PATCH", "/api/generation", input, nil); w.Code != 400 {
			t.Fatalf("accepted %v: %s", settings, w.Body.String())
		}
		if !reflect.DeepEqual(after, a.store.Snapshot().Config) {
			t.Fatal("invalid update partially applied")
		}
	}
	input["settings"] = map[string]any{"reasoning": "auto"}
	input["model"] = "stale"
	if w := callAPI(t, a, "PATCH", "/api/generation", input, nil); w.Code != 409 {
		t.Fatal(w.Body.String())
	}
	input["model"] = before.Model
	a.active = "busy"
	if w := callAPI(t, a, "PATCH", "/api/generation", input, nil); w.Code != 409 {
		t.Fatal(w.Body.String())
	}
	a.active = ""
	input["settings"] = map[string]any{"temperature": nil, "reasoning": "auto"}
	if w := callAPI(t, a, "PATCH", "/api/generation", input, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	reopened, err := openStore(a.dir)
	if err != nil || reopened.Snapshot().Config.Temperature != nil {
		t.Fatal("auto did not persist", err)
	}
	if reopened.Snapshot().Config.TimeoutSec != 1200 {
		t.Fatal("saved request timeout did not survive restart")
	}
}

func TestResetPreservesHistoryAndScheduleButRemovesContext(t *testing.T) {
	a := newTestApp(t)
	chat := postChat(t, a, "", "/new")
	_ = a.store.Update(func(st *State) error {
		st.Runs = append([]Run{{ID: "old", ChatID: chat.ChatID, Task: "remember indigo", Result: "indigo remembered", Status: "completed", Origin: "user"}}, st.Runs...)
		st.Schedules = []Schedule{{ID: "keep", ChatID: chat.ChatID}}
		return nil
	})
	postChat(t, a, chat.ChatID, "/status")
	if messages := chatContext(a.store.Snapshot(), chat.ChatID); len(messages) != 2 {
		t.Fatal("commands polluted context", messages)
	}
	postChat(t, a, chat.ChatID, "/reset")
	st := a.store.Snapshot()
	if len(st.Runs) != 4 || len(st.Schedules) != 1 || len(chatContext(st, chat.ChatID)) != 0 {
		t.Fatal("reset lost history or retained old context")
	}
	reopened, err := openStore(a.dir)
	if err != nil || len(chatContext(reopened.Snapshot(), chat.ChatID)) != 0 {
		t.Fatal("reset did not persist", err)
	}
	postChat(t, a, chat.ChatID, "/config\ttemperature\t0")
	if a.store.Snapshot().Config.Temperature == nil {
		t.Fatal("whitespace command parsing failed")
	}
	postChat(t, a, chat.ChatID, "/rename Automation desk")
	if a.store.Snapshot().Chats[0].Title != "Automation desk" {
		t.Fatal("rename failed")
	}
}

func TestConcurrentDuplicateSubmissionAndRestart(t *testing.T) {
	a := newTestApp(t)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"message":{"content":"ok"}}`)
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	input := ChatRequest{Message: "perform once", RequestID: "stable-client-id"}
	results := make(chan string, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := callAPI(t, a, "POST", "/api/chat", input, nil)
			if w.Code != 202 {
				results <- w.Body.String()
				return
			}
			var run Run
			_ = json.Unmarshal(w.Body.Bytes(), &run)
			results <- run.ID
		}()
	}
	wg.Wait()
	close(results)
	id := ""
	for value := range results {
		if id == "" {
			id = value
		}
		if value != id {
			t.Fatalf("duplicate submission: %s vs %s", id, value)
		}
	}
	waitRun(t, a, id)
	a.wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("inference was repeated")
	}
	reopened, err := openStore(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	w := callAPI(t, a, "POST", "/api/chat", input, nil)
	if w.Code != 202 || !strings.Contains(w.Body.String(), id) || calls.Load() != 1 {
		t.Fatal("restart lost deduplication")
	}
	input.Message = "different message"
	if w = callAPI(t, a, "POST", "/api/chat", input, nil); w.Code != 409 {
		t.Fatal("reused ID accepted different payload")
	}
}

func TestCronTimezoneDSTAndValidation(t *testing.T) {
	for _, test := range []struct{ expression, zone, after, want string }{
		{"0 14 * * 1-5", "America/Mexico_City", "2030-06-01T20:00:00Z", "2030-06-03T20:00:00Z"},
		{"*/15 * * * *", "UTC", "2030-06-01T20:00:00Z", "2030-06-01T20:15:00Z"},
		{"30 2 * * *", "America/New_York", "2030-03-10T06:00:00Z", "2030-03-11T06:30:00Z"},
		{"30 1 * * *", "America/New_York", "2030-11-03T05:30:00Z", "2030-11-03T06:30:00Z"},
	} {
		after, _ := time.Parse(time.RFC3339, test.after)
		next, err := nextOccurrence(Schedule{Repeat: "cron", Cron: test.expression, Timezone: test.zone}, after)
		if err != nil || next.Format(time.RFC3339) != test.want {
			t.Errorf("%s %s: %s %v", test.expression, test.zone, next, err)
		}
	}
	for _, expr := range []string{"* * * * * *", "@every 1s", "0 25 * * *", "0 0 31 2 *", "*/0 * * * *"} {
		if _, err := nextOccurrence(Schedule{Repeat: "cron", Cron: expr, Timezone: "UTC"}, time.Now()); err == nil {
			t.Errorf("accepted invalid cron %q", expr)
		}
	}
	if _, err := nextOccurrence(Schedule{Repeat: "once", Date: "2030-03-10", Time: "02:30", Timezone: "America/New_York"}, time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("accepted nonexistent local time")
	}
}

func TestScheduledFailurePausesAndRestartDoesNotReplay(t *testing.T) {
	a := newTestApp(t)
	chat := postChat(t, a, "", "/new")
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if len(payload["messages"].([]any)) != 2 {
			t.Error("scheduled task inherited chat history")
		}
		http.Error(w, "provider unavailable", http.StatusBadRequest)
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	s, err := a.createSchedule(Schedule{ChatID: chat.ChatID, Title: "Check", Kind: "task", Task: "Check the report", Repeat: "cron", Cron: "* * * * *", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	due, _ := time.Parse(time.RFC3339, s.NextRun)
	a.processDue(due)
	run := a.store.Snapshot().Runs[0]
	waitRun(t, a, run.ID)
	a.wg.Wait()
	s = a.store.Snapshot().Schedules[0]
	if s.Enabled || s.LastStatus != "failed" || s.LastRunID != run.ID || s.LastError == "" || s.LastFinishedAt == "" {
		t.Fatalf("failure not tracked: %+v", s)
	}
	a.processDue(due.Add(time.Hour))
	if calls.Load() != 1 {
		t.Fatal("failed action replayed")
	}
	// Simulate a separate in-flight occurrence interrupted by process death.
	_ = a.store.Update(func(st *State) error {
		st.Schedules[0].Enabled = true
		st.Schedules[0].LastStatus = "running"
		st.Runs[0].Status = "running"
		return nil
	})
	reopened, err := openStore(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	s = a.store.Snapshot().Schedules[0]
	if s.Enabled || s.LastStatus != "interrupted" {
		t.Fatal("interrupted job not paused")
	}
	a.processDue(due.Add(time.Hour))
	if calls.Load() != 1 {
		t.Fatal("interrupted occurrence replayed")
	}
}

func TestCronCommandsAndAPI(t *testing.T) {
	a := newTestApp(t)
	// Must work as the very first command in an empty conversation.
	run := postChat(t, a, "", `/cron add {"title":"Review","kind":"reminder","repeat":"cron","cron":"0 14 * * 1-5"}`)
	st := a.store.Snapshot()
	if len(st.Schedules) != 1 || st.Schedules[0].ChatID != run.ChatID {
		t.Fatal(run.Result)
	}
	id := st.Schedules[0].ID
	postChat(t, a, run.ChatID, "/cron pause "+id)
	if a.store.Snapshot().Schedules[0].Enabled {
		t.Fatal("pause failed")
	}
	w := callAPI(t, a, "POST", "/api/schedules/"+id, map[string]string{"action": "run"}, nil)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	s := a.store.Snapshot().Schedules[0]
	if s.Enabled || s.NextRun != st.Schedules[0].NextRun || s.LastStatus != "completed" {
		t.Fatal("manual run modified next occurrence or failed to record status")
	}
	postChat(t, a, run.ChatID, "/cron resume "+id)
	if !a.store.Snapshot().Schedules[0].Enabled {
		t.Fatal("resume failed")
	}
	if !strings.Contains(postChat(t, a, run.ChatID, "/cron history "+id).Result, "completed") {
		t.Fatal("missing execution history")
	}
	if !strings.Contains(postChat(t, a, run.ChatID, "/cron next */15 * * * * UTC").Result, "Next occurrences") {
		t.Fatal("preview failed")
	}
	if w := callAPI(t, a, "POST", "/api/schedules", map[string]any{"title": "bad", "kind": "task", "repeat": "cron", "cron": "* * * * *", "task": ""}, nil); w.Code != 400 {
		t.Fatal("empty task accepted")
	}
	postChat(t, a, run.ChatID, "/cron delete "+id)
	if len(a.store.Snapshot().Schedules) != 0 {
		t.Fatal("delete failed")
	}
}

func TestDueJobsAreFairAndBusyJobsRemainDue(t *testing.T) {
	a := newTestApp(t)
	chat := postChat(t, a, "", "/new")
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"message":{"content":"ok"}}`) }))
	defer model.Close()
	current := time.Now()
	_ = a.store.Update(func(st *State) error {
		st.Config.BaseURL = model.URL
		for i, age := range []time.Duration{time.Minute, 2 * time.Minute} {
			st.Schedules = append(st.Schedules, Schedule{ID: fmt.Sprint(i), ChatID: chat.ChatID, Title: "task", Kind: "task", Task: "hello", Repeat: "cron", Cron: "* * * * *", Timezone: "UTC", Enabled: true, NextRun: current.Add(-age).UTC().Format(time.RFC3339)})
		}
		return nil
	})
	a.processDue(current)
	st := a.store.Snapshot()
	if st.Runs[0].ScheduleID != "1" || st.Schedules[0].LastRun != "" {
		t.Fatal("oldest due task did not run first")
	}
	waitRun(t, a, st.Runs[0].ID)
	a.wg.Wait()
	a.processDue(current)
	st = a.store.Snapshot()
	if st.Runs[0].ScheduleID != "0" {
		t.Fatal("waiting occurrence was lost")
	}
	waitRun(t, a, st.Runs[0].ID)
}

func TestStoreRollbackClearsNewOptionalFields(t *testing.T) {
	a := newTestApp(t)
	err := a.store.Update(func(st *State) error {
		value := 0.5
		st.Config.Temperature = &value
		st.Config.SystemPrompt = "must disappear"
		return fmt.Errorf("abort")
	})
	if err == nil || a.store.Snapshot().Config.Temperature != nil || a.store.Snapshot().Config.SystemPrompt != "" {
		t.Fatal("rollback retained fields omitted from previous JSON")
	}
}

func TestScheduleCreationDeduplicatesAfterRestart(t *testing.T) {
	a := newTestApp(t)
	input := map[string]any{"title": "One schedule", "kind": "reminder", "repeat": "cron", "cron": "0 14 * * *", "request_id": "creation-retry"}
	first := callAPI(t, a, "POST", "/api/schedules", input, nil)
	if first.Code != 201 {
		t.Fatal(first.Body.String())
	}
	var created Schedule
	_ = json.Unmarshal(first.Body.Bytes(), &created)
	reopened, err := openStore(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	second := callAPI(t, a, "POST", "/api/schedules", input, nil)
	var duplicate Schedule
	_ = json.Unmarshal(second.Body.Bytes(), &duplicate)
	if second.Code != 201 || duplicate.ID != created.ID || len(a.store.Snapshot().Chats) != 1 || len(a.store.Snapshot().Schedules) != 1 {
		t.Fatal("schedule retry created another chat or job", second.Body.String())
	}
	input["title"] = "different"
	if w := callAPI(t, a, "POST", "/api/schedules", input, nil); w.Code != 400 {
		t.Fatal("creation ID reused for another definition")
	}
}

func TestModelDiscoveryDoesNotBlockStop(t *testing.T) {
	a := newTestApp(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, `{"models":[]}`)
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	done := make(chan struct{})
	go func() { defer close(done); callAPI(t, a, "POST", "/api/chat", ChatRequest{Message: "/models"}, nil) }()
	<-entered
	stopped := make(chan struct{})
	go func() { defer close(stopped); callAPI(t, a, "POST", "/api/stop", map[string]any{}, nil) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("model discovery held the execution lock")
	}
	close(release)
	<-done
}

func TestNewMessagesDoNotEvictActiveOrOlderHistory(t *testing.T) {
	a := newTestApp(t)
	_ = a.store.Update(func(st *State) error {
		for i := 0; i < 205; i++ {
			st.Runs = append(st.Runs, Run{ID: fmt.Sprint(i), ChatID: "saved", Status: "completed", Task: "retained"})
		}
		st.Runs = append(st.Runs, Run{ID: "active-old", ChatID: "saved", Status: "running"})
		return nil
	})
	postChat(t, a, "", "/status")
	st := a.store.Snapshot()
	if len(st.Runs) != 207 || st.Runs[len(st.Runs)-1].ID != "active-old" {
		t.Fatal("new message silently evicted history")
	}
}
