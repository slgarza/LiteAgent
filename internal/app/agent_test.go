package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{store: st, browser: &Browser{dir: dir}, dir: dir, root: ctx}
	t.Cleanup(func() { cancel(); a.wg.Wait(); a.browser.Close() })
	return a
}
func callAPI(t *testing.T, a *App, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, bytes.NewReader(data))
	r.Header.Set("X-LiteAgent", "1")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func waitRun(t *testing.T, a *App, id string) Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range a.store.Snapshot().Runs {
			if r.ID == id && r.Status != "running" {
				return r
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the task did not finish")
	return Run{}
}

func TestPersistenceAndInterruptedRuns(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update(func(st *State) error {
		st.Skills = append(st.Skills, Skill{ID: "saved", Name: "Report", Instructions: "Open and verify the report"})
		st.Runs = append(st.Runs, Run{ID: "one", Status: "running"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := reopened.Snapshot()
	if len(st.Skills) != 1 || st.Skills[0].ID != "saved" || st.Runs[0].Status != "interrupted" {
		t.Fatalf("state not persisted: %+v", st)
	}
	err = reopened.Update(func(st *State) error { st.Skills = nil; return fmt.Errorf("abort") })
	if err == nil || len(reopened.Snapshot().Skills) != 1 {
		t.Fatal("failed update was not rolled back")
	}
	if err = os.WriteFile(filepath.Join(dir, "state.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = openStore(dir); err == nil {
		t.Fatal("must not overwrite corrupted state")
	}
}

func TestLocalAPIAndSecrets(t *testing.T) {
	a := newTestApp(t)
	c := defaultConfig()
	c.APIKey = "secret-example"
	if w := callAPI(t, a, "PUT", "/api/config", c, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w := callAPI(t, a, "GET", "/api/state", nil, nil)
	if strings.Contains(w.Body.String(), "secret-example") {
		t.Fatal("the key was exposed")
	}
	c.APIKey = ""
	if w = callAPI(t, a, "PUT", "/api/config", c, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if a.store.Snapshot().Config.APIKey != "secret-example" {
		t.Fatal("the key was lost on save")
	}
	c.BaseURL = "https://other.example/v1"
	callAPI(t, a, "PUT", "/api/config", c, nil)
	if a.store.Snapshot().Config.APIKey != "" {
		t.Fatal("the key was carried to a different server")
	}
	w = callAPI(t, a, "POST", "/api/runs", map[string]any{"task": "hello"}, map[string]string{"Origin": "https://attacker.example"})
	if w.Code != 403 {
		t.Fatal("an external origin was allowed")
	}
	w = callAPI(t, a, "POST", "/api/runs", map[string]any{"task": "hello"}, map[string]string{"X-LiteAgent": ""})
	if w.Code != 403 {
		t.Fatal("a write without the header was allowed")
	}
	r := httptest.NewRequest("GET", "http://attacker.example/api/state", nil)
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("an external host was allowed")
	}
}

func TestProviderToolFormats(t *testing.T) {
	for _, provider := range []string{"ollama", "openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if provider == "openai" {
					msgs := payload["messages"].([]any)
					tc := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
					if _, ok := tc["function"].(map[string]any)["arguments"].(string); !ok {
						t.Error("OpenAI requires string-encoded arguments")
					}
					if r.Header.Get("Authorization") != "Bearer key" {
						t.Error("missing auth")
					}
				}
				if provider == "anthropic" {
					if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "key" {
						t.Error("incorrect Anthropic path or auth")
					}
					fmt.Fprint(w, `{"content":[{"type":"tool_use","id":"call2","name":"browser_read","input":{}}],"stop_reason":"tool_use"}`)
					return
				}
				if provider == "ollama" {
					options := payload["options"].(map[string]any)
					if _, forced := options["temperature"]; forced {
						t.Error("Ollama requests must preserve the model's configured sampling parameters")
					}
					fmt.Fprint(w, `{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"browser_read","arguments":{}}}]}}`)
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call2","type":"function","function":{"name":"browser_read","arguments":"{}"}}]}}]}`)
				}
			}))
			defer server.Close()
			c := defaultConfig()
			c.Provider = provider
			c.BaseURL = server.URL
			c.APIKey = "key"
			msgs := []Message{{Role: "user", Content: "read"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "call1", Type: "function", Function: FunctionCall{"browser_open", json.RawMessage(`{"url":"https://example.com"}`)}}}}, {Role: "tool", ToolCallID: "call1", ToolName: "browser_open", Content: "ok"}}
			m, err := complete(context.Background(), c, msgs, toolDefinitions())
			if err != nil {
				t.Fatal(err)
			}
			if len(m.ToolCalls) != 1 || m.ToolCalls[0].ID == "" || string(m.ToolCalls[0].Function.Arguments) != "{}" {
				t.Fatalf("incorrect normalization: %+v", m)
			}
		})
	}
}

func TestAgentRecallsSkillsAndPersists(t *testing.T) {
	a := newTestApp(t)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if calls.Add(1) == 1 {
			if !strings.Contains(payload.Messages[len(payload.Messages)-1].Content, "Verify period") {
				t.Error("did not retrieve the skill")
			}
			fmt.Fprint(w, `{"message":{"role":"assistant","content":"I will check the procedure.","tool_calls":[{"function":{"name":"skills_search","arguments":{"query":"report"}}}]}}`)
		} else {
			if !strings.Contains(payload.Messages[len(payload.Messages)-1].Content, "Monthly report") {
				t.Error("did not receive the tool result")
			}
			fmt.Fprint(w, `{"message":{"role":"assistant","content":"Procedure found."}}`)
		}
	}))
	defer model.Close()
	if err := a.store.Update(func(st *State) error {
		st.Config.BaseURL = model.URL
		st.Skills = []Skill{{ID: "report", Name: "Monthly report", Description: "Report", Instructions: "Verify period and columns"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := callAPI(t, a, "POST", "/api/runs", map[string]any{"task": "Find the monthly report"}, nil)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var run Run
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	run = waitRun(t, a, run.ID)
	if run.Status != "completed" || len(run.SkillIDs) != 1 || len(run.Events) < 4 {
		t.Fatalf("incomplete run: %+v", run)
	}
	reopened, err := openStore(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Runs[0].Result != "Procedure found." {
		t.Fatal("the result did not persist")
	}
}

func TestCancellationAndConcurrentRun(t *testing.T) {
	a := newTestApp(t)
	entered := make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	w := callAPI(t, a, "POST", "/api/runs", map[string]any{"task": "A slow task"}, nil)
	var run Run
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the model did not receive the task")
	}
	if w = callAPI(t, a, "POST", "/api/runs", map[string]any{"task": "Another task"}, nil); w.Code != 409 {
		t.Fatal("allowed two concurrent tasks")
	}
	callAPI(t, a, "POST", "/api/stop", map[string]any{}, nil)
	run = waitRun(t, a, run.ID)
	if run.Status != "cancelled" {
		t.Fatal(run.Status)
	}
}

func TestToolsRejectMalformedArguments(t *testing.T) {
	for _, tc := range []ToolCall{{Function: FunctionCall{"shell", json.RawMessage(`{}`)}}, {Function: FunctionCall{"browser_click", json.RawMessage(`{"element_id":1}`)}}, {Function: FunctionCall{"browser_open", json.RawMessage(`{}`)}}, {Function: FunctionCall{"browser_read", json.RawMessage(`{"code":"bad"}`)}}} {
		if _, err := validateToolCall(tc); err == nil {
			t.Fatalf("accepted an invalid tool: %+v", tc)
		}
	}
}
