package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func token(n int64) *int64 { return &n }
func TestProviderUsageCapture(t *testing.T) {
	tests := []struct {
		provider, body        string
		input, output, cached int64
		wantError             bool
	}{
		{"ollama", `{"prompt_eval_count":120,"eval_count":15,"prompt_eval_cached_count":80,"message":{"content":"ok"}}`, 120, 15, 80, false},
		{"openai", `{"usage":{"prompt_tokens":120,"completion_tokens":15,"prompt_tokens_details":{"cached_tokens":80}},"choices":[{"message":{"content":"ok"}}]}`, 120, 15, 80, false},
		{"vscode", `{"usage":{"prompt_tokens":120,"completion_tokens":15,"prompt_tokens_details":{"cached_tokens":80}},"choices":[{"message":{"content":"ok"}}]}`, 120, 15, 80, false},
		{"anthropic", `{"usage":{"input_tokens":20,"cache_read_input_tokens":80,"cache_creation_input_tokens":20,"output_tokens":15},"content":[{"type":"text","text":"ok"}]}`, 120, 15, 80, false},
		{"openai-responses", `{"status":"completed","usage":{"input_tokens":120,"output_tokens":15,"input_tokens_details":{"cached_tokens":80}},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`, 120, 15, 80, false},
		{"openai", `{"usage":{"prompt_tokens":120,"completion_tokens":15,"prompt_tokens_details":{"cached_tokens":80}},"choices":[{"finish_reason":"length","message":{"tool_calls":[{"id":"one","function":{"name":"filesystem_write","arguments":"{}"}}]}}]}`, 120, 15, 80, true},
		{"openai-responses", `{"status":"incomplete","usage":{"input_tokens":120,"output_tokens":15,"input_tokens_details":{"cached_tokens":80}},"output":[]}`, 120, 15, 80, true},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, test.body) }))
			defer server.Close()
			c := defaultConfig()
			c.Provider = test.provider
			c.BaseURL = server.URL
			m, err := complete(context.Background(), c, []Message{{Role: "user", Content: "hello"}}, nil)
			if (err != nil) != test.wantError {
				t.Fatal(err)
			}
			if m.Usage.Input == nil || *m.Usage.Input != test.input || m.Usage.Output == nil || *m.Usage.Output != test.output || m.Usage.Cached == nil || *m.Usage.Cached != test.cached {
				t.Fatalf("incorrect usage %+v", m.Usage)
			}
			if test.wantError && len(m.ToolCalls) > 0 {
				t.Fatal("partial tools survived an output-limit error")
			}
			encoded, _ := json.Marshal(m)
			if strings.Contains(string(encoded), "usage") {
				t.Fatal("usage leaked into a model message")
			}
		})
	}
	for _, body := range []string{`{}`, `{"usage":null}`, `{"usage":{}}`, `{"usage":{"prompt_tokens":-1,"completion_tokens":1.5}}`, `{"usage":{"prompt_tokens":"120","completion_tokens":1000000000000000000}}`, `{"prompt_tokens":44,"usage":{}}`} {
		if u := parseTokenUsage("openai", []byte(body)); u.available() {
			t.Fatal("invented metrics for", body)
		}
	}
	zero := parseTokenUsage("openai", []byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}}`))
	if zero.Input == nil || zero.Output == nil || zero.Cached == nil {
		t.Fatal("reported zero was treated as missing")
	}
}

func TestUsagePersistsAcrossToolsFailureAndRestart(t *testing.T) {
	a := newTestApp(t)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"prompt_eval_count":100,"eval_count":20,"prompt_eval_cached_count":0,"message":{"tool_calls":[{"function":{"name":"conversation_history","arguments":{}}}]}}`)
			return
		}
		http.Error(w, "synthetic failure", 502)
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	run := postChat(t, a, "", "Review this conversation")
	run = waitRun(t, a, run.ID)
	a.wg.Wait()
	if run.Status != "failed" || run.Metrics == nil || run.Metrics.Requests != 2 || run.Metrics.UsageReports != 1 || run.Metrics.Totals.Input == nil || *run.Metrics.Totals.Input != 100 || run.Metrics.LastUsage.available() {
		t.Fatalf("bad run metrics %+v", run.Metrics)
	}
	reloaded, err := openStore(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	run = reloaded.Snapshot().Runs[0]
	if run.Metrics.Requests != 2 || *run.Metrics.Totals.Output != 20 {
		t.Fatal("usage did not survive restart")
	}
	original := a.store.path
	a.store.path = filepath.Join(t.TempDir(), "missing", "state.json")
	if err := a.store.modelRequestStarted(run.ID); err == nil {
		t.Fatal("expected persistence failure")
	}
	if a.store.Snapshot().Runs[0].Metrics.Requests != 2 {
		t.Fatal("failed save mutated metrics")
	}
	a.store.path = original
}

func TestDetailedStatusScopeRedactionAndNoInference(t *testing.T) {
	a := newTestApp(t)
	current := time.Date(2026, 11, 16, 18, 4, 0, 0, time.UTC)
	st := a.store.Snapshot()
	st.Config.APIKey = "provider-private-key"
	st.Config.BaseURL = "https://user:password@example.test/v1/provider-private-key?token=query-secret#fragment-secret"
	st.Config.ConnectionID = "test"
	st.Connections = []Connection{{ID: "test", Name: "<img src=x onerror=alert(1)> provider-private-key", APIKey: "connection-private-key"}}
	st.MCPServers = []MCPServer{{ID: "mcp", Name: "Test", Token: "mcp-private-key", Enabled: true}}
	st.Chats = []Chat{{ID: "this", CreatedAt: current.Add(-48 * time.Hour).Format(time.RFC3339), UpdatedAt: current.Add(-time.Minute).Format(time.RFC3339)}, {ID: "other"}}
	st.Runs = []Run{
		{ID: "run", ChatID: "this", Origin: "user", Status: "completed", Provider: "ollama", Model: "test-model", ContextLimit: 4000, CreatedAt: current.Add(-time.Minute).Format(time.RFC3339), FinishedAt: current.Format(time.RFC3339), Metrics: &RunMetrics{Requests: 2, UsageReports: 2, Totals: TokenUsage{Input: token(3500), Output: token(400)}, LastUsage: TokenUsage{Input: token(2000), Cached: token(1000)}, ModelTimeMS: 2400}, Events: []Event{{Kind: "tool", Name: "filesystem_write", Content: "private-task"}, {Kind: "observation", Name: "filesystem_write", Content: "private-result"}}},
		{ID: "legacy", ChatID: "this", Origin: "user", Status: "completed"},
		{ID: "foreign", ChatID: "other", Origin: "user", Task: "foreign-task-secret", Metrics: &RunMetrics{Requests: 99, UsageReports: 99, Totals: TokenUsage{Input: token(999999)}}},
	}
	st.Queue = []QueuedMessage{{ChatID: "this", Status: "queued", Message: "queued-task-secret"}, {ChatID: "other", Status: "queued"}, {ChatID: "this", Status: "paused"}, {ChatID: "this", Status: "steering"}}
	report := workspaceStatus(st, "this", "foreign", false, false, current.Add(-time.Hour), current)
	text := report.text()
	for _, want := range []string{"App uptime: 1h 0m", "3500 in / 400 out", "2/2 model requests", "1 older replies", "50.0% hit", "4000 configured (50.0%)", "1 succeeded", "working in another chat", "1 queued · 1 paused · 1 steering", "[hidden]", "API key configured"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing", want)
		}
	}
	for _, secret := range []string{"provider-private-key", "password", "query-secret", "fragment-secret", "connection-private-key", "mcp-private-key", "foreign-task-secret", "999999", "private-task", "private-result", "queued-task-secret"} {
		if strings.Contains(text, secret) {
			t.Fatal("status exposed", secret)
		}
	}
	if strings.Contains(safeEndpoint(st.Config.BaseURL), "user:") {
		t.Fatal("endpoint exposed URL credentials")
	}
	_ = a.store.Update(func(s *State) error { *s = st; return nil })
	before := len(a.store.Snapshot().Runs)
	w := callAPI(t, a, "GET", "/api/status?chat_id=this", nil, nil)
	if w.Code != 200 || len(a.store.Snapshot().Runs) != before {
		t.Fatal("read-only status failed", w.Code)
	}
	if !strings.Contains(w.Body.String(), "3500 in / 400 out") {
		t.Fatal("API status differs")
	}
	if w := callAPI(t, a, "GET", "/api/status?chat_id=missing", nil, nil); w.Code != 404 {
		t.Fatal(w.Code)
	}
	empty := workspaceStatus(st, "new", "", false, false, current, current).text()
	if strings.Contains(empty, "3500") || !strings.Contains(empty, "not reported in / not reported out") {
		t.Fatal("new chat inherited usage")
	}
	command := postChat(t, a, "this", "/status")
	if !strings.Contains(command.Result, "3500 in / 400 out") || strings.Contains(command.Result, st.Config.APIKey) {
		t.Fatal("terminal status differs or leaks credentials")
	}
}

func TestStatusDuringModelRequest(t *testing.T) {
	a := newTestApp(t)
	started, release := make(chan struct{}), make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		fmt.Fprint(w, `{"message":{"content":"done"}}`)
	}))
	defer model.Close()
	defer close(release)
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	run := postChat(t, a, "", "Wait for the provider")
	<-started
	w := callAPI(t, a, "GET", "/api/status?chat_id="+run.ChatID, nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "working in this chat") || !strings.Contains(w.Body.String(), "0/1 model requests") {
		t.Fatal("status missed pending request", w.Code)
	}
}

func TestSkillsDoNotLoadFromKeywordsOrPreviousSelection(t *testing.T) {
	a := newTestApp(t)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		n := calls.Add(1)
		last := body.Messages[len(body.Messages)-1].Content
		if n == 2 {
			if !strings.Contains(last, "explicit-procedure-marker") {
				t.Error("explicit skill missing")
			}
		} else if strings.Contains(last, "explicit-procedure-marker") || strings.Contains(last, "Selected procedures") {
			t.Error("skill loaded without selection")
		}
		if !strings.Contains(body.Messages[0].Content, "Skills are opt-in") {
			t.Error("missing opt-in instructions")
		}
		fmt.Fprint(w, `{"message":{"content":"done"}}`)
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error {
		st.Config.BaseURL = model.URL
		st.Skills = []Skill{{ID: "report", Name: "Monthly report", Description: "The monthly report with columns", Instructions: "explicit-procedure-marker"}}
		return nil
	})
	first := postChat(t, a, "", "Tell me what a monthly report is")
	if done := waitRun(t, a, first.ID); len(done.SkillIDs) != 0 {
		t.Fatal("keyword auto-selected a skill")
	}
	w := callAPI(t, a, "POST", "/api/chat", map[string]any{"chat_id": first.ChatID, "message": "Use the monthly report skill", "skill_ids": []string{"report"}}, nil)
	var selected Run
	_ = json.Unmarshal(w.Body.Bytes(), &selected)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	if done := waitRun(t, a, selected.ID); len(done.SkillIDs) != 1 {
		t.Fatal("explicit selection lost")
	}
	next := postChat(t, a, first.ChatID, "Explain the report's columns without using a skill")
	if done := waitRun(t, a, next.ID); len(done.SkillIDs) != 0 {
		t.Fatal("previous selection carried over")
	}
	// Explicit search still finds procedures when the user asks for one.
	if found := rankedSkills(a.store.Snapshot().Skills, "Monthly report"); len(found) != 1 {
		t.Fatal("explicit skill search regressed")
	}
}
