package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func TestMultilingualHistoryKeepsLatestActionEvidence(t *testing.T) {
	text := strings.Repeat("你好🙂", 4000)
	run := Run{ID: "latest", ChatID: "chat", Origin: "user", Status: "failed", Task: text, Result: text,
		Events: []Event{{Kind: "tool", Name: "filesystem_write", Content: `{"path":"proof.txt"}`},
			{Kind: "observation", Name: "filesystem_write", Content: `{"saved":true}`}}}
	for range 5 {
		run.Events = append(run.Events, Event{Kind: "user", Content: text})
	}
	history := boundedConversationContext([]Run{run, run}, "chat", "", 12)
	if len(history) < 2 || !strings.Contains(history[len(history)-1].Content, "proof.txt") {
		t.Fatal("latest action evidence was displaced by long text")
	}
	bytes := 0
	for _, message := range history {
		bytes += len(message.Content)
		if !utf8.ValidString(message.Content) {
			t.Fatal("truncation split a Unicode character")
		}
	}
	if bytes > 16000 {
		t.Fatal("history exceeded its byte budget", bytes)
	}
}

func historyTool(t *testing.T, a *App, chat string, args map[string]any) map[string]any {
	t.Helper()
	data, _ := json.Marshal(args)
	if args == nil {
		data = []byte(`{}`)
	}
	validated, err := validateToolCall(ToolCall{Function: FunctionCall{"conversation_history", data}})
	if err != nil {
		t.Fatal(err)
	}
	text, handled, err := a.applicationTool(context.Background(), Run{ChatID: chat}, "conversation_history", validated)
	if err != nil || !handled {
		t.Fatal(err)
	}
	if len(text) > 18000 {
		t.Fatal("history exceeded observation budget")
	}
	var out map[string]any
	if err = json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestRecordedHistorySurvivesFailedReplyAndRestart(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		var name string
		var args map[string]any
		switch calls.Add(1) {
		case 1:
			name = "filesystem_write"
			args = map[string]any{"path": "proof.txt", "content": "File written once"}
		case 2:
			http.Error(w, "synthetic provider unavailable after file write", 502)
			return
		case 3:
			raw, _ := json.Marshal(body.Messages)
			for _, want := range []string{"filesystem_write", "proof.txt", "result recorded", "saved"} {
				if !strings.Contains(string(raw), want) {
					t.Error("lost action evidence", want)
				}
			}
			old := a.store.Snapshot().Runs[1]
			index := -1
			for i, event := range old.Events {
				if event.Kind == "observation" && event.Name == "filesystem_write" {
					index = i
				}
			}
			if index < 0 {
				t.Error("missing write observation")
			}
			name = "conversation_history"
			args = map[string]any{"run_id": old.ID, "event_index": index}
		case 4:
			var record map[string]any
			if err := json.Unmarshal([]byte(body.Messages[len(body.Messages)-1].Content), &record); err != nil {
				t.Error(err)
			}
			if !strings.Contains(fmt.Sprint(record["content"]), `"saved":true`) {
				t.Error("saved result could not be recovered", record)
			}
			fmt.Fprint(w, `{"message":{"content":"The file was saved before the earlier reply failed."}}`)
			return
		default:
			t.Error("unexpected model retry")
			http.Error(w, "unexpected request", 500)
			return
		}
		raw, _ := json.Marshal(args)
		sendJSON(w, 200, map[string]any{"message": Message{Role: "assistant", ToolCalls: []ToolCall{{Function: FunctionCall{name, raw}}}}})
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	first := postChat(t, a, "", "Save proof.txt")
	ended := waitRun(t, a, first.ID)
	if ended.Status != "failed" {
		t.Fatal(ended.Status)
	}
	a.wg.Wait()
	reopened, err := openStore(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	second := postChat(t, a, first.ChatID, "What did you already do?")
	ended = waitRun(t, a, second.ID)
	if ended.Status != "completed" {
		t.Fatal(ended.Result)
	}
	attempts := 0
	for _, r := range a.store.Snapshot().Runs {
		for _, e := range r.Events {
			if e.Kind == "tool" && e.Name == "filesystem_write" {
				attempts++
			}
		}
	}
	if attempts != 1 {
		t.Fatal("write was repeated")
	}
	data, err := os.ReadFile(filepath.Join(a.filesHome, "proof.txt"))
	if err != nil || string(data) != "File written once" {
		t.Fatal(err)
	}
}
func TestActionMemoryDistinguishesPlansErrorsAndUnknownOutcomes(t *testing.T) {
	run := Run{ID: "run", ChatID: "chat", Status: "failed", Origin: "user", Task: "Save a file", Result: "Provider failed", Events: []Event{
		{Kind: "thinking", Content: "private-thinking-not-history"}, {Kind: "assistant", Content: "unexecuted-plan-not-evidence"},
		{Kind: "tool", Name: "filesystem_write", Content: `{"path":"done.txt"}`}, {Kind: "observation", Name: "filesystem_write", Content: `{"saved":true,"path":"done.txt"}`},
		{Kind: "tool", Name: "filesystem_read", Content: `{"path":"missing.txt"}`}, {Kind: "error", Name: "filesystem_read", Content: "not found"},
		{Kind: "tool", Name: "browser_click", Content: `{"element_id":"old-id"}`},
	}}
	history := boundedConversationContext([]Run{run}, "chat", "", 12)
	if len(history) != 2 {
		t.Fatal(history)
	}
	text := history[1].Content
	for _, want := range []string{"done.txt", `"results_recorded":1`, `"errors":1`, `"unknown_outcomes":1`, "outcome unknown", "conversation_history"} {
		if !strings.Contains(text, want) {
			t.Fatal("lost action state", want, text)
		}
	}
	if strings.Contains(text, "private-thinking") || strings.Contains(text, "unexecuted-plan") {
		t.Fatal("thoughts or intentions used as execution evidence")
	}
	for i := 0; i < 50; i++ {
		run.Events = append(run.Events, Event{Kind: "tool", Name: "tool", Content: strings.Repeat("🌎", 1000)}, Event{Kind: "observation", Name: "tool", Content: strings.Repeat("🌎", 1000)})
	}
	if len(actionMemory(run)) > 3700 {
		t.Fatal("unbounded history summary")
	}
}
func TestCompactionRetainsResultEdgesAndRetrievalReference(t *testing.T) {
	original := "first evidence " + strings.Repeat("body", 2000) + " final path and hash"
	messages := []Message{{Role: "assistant", Thinking: "protocol-thinking", Blocks: []map[string]any{{"encrypted": "opaque"}}, ToolCalls: []ToolCall{{ID: "c1"}}}, {Role: "tool", Content: original, ToolCallID: "c1", HistoryRunID: "run", HistoryEvent: 3}}
	for i := 0; i < 3; i++ {
		messages = append(messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: fmt.Sprint(i)}}}, Message{Role: "tool", ToolCallID: fmt.Sprint(i), Content: strings.Repeat("recent", 1000)})
	}
	reduced := compactMessages(messages)
	for _, want := range []string{"first evidence", "final path and hash", "conversation_history", "event_index=3"} {
		if !strings.Contains(reduced[1].Content, want) {
			t.Fatal("compaction lost", want)
		}
	}
	if messages[1].Content != original || reduced[0].Thinking != "protocol-thinking" || reduced[0].Blocks[0]["encrypted"] != "opaque" || reduced[1].ToolCallID != "c1" {
		t.Fatal("compaction modified protocol context or stored history")
	}
	for _, index := range []int{3, 5, 7} {
		if reduced[index].Content != messages[index].Content {
			t.Fatal("recent result was compacted")
		}
	}
}
func TestHistoryScopeResetPaginationAndThinkingExclusion(t *testing.T) {
	a := newTestApp(t)
	value := strings.Repeat("<&🌎", 8000)
	_ = a.store.Update(func(st *State) error {
		st.Chats = []Chat{{ID: "a", ContextResetID: "reset"}, {ID: "b"}}
		st.Runs = []Run{
			{ID: "current", ChatID: "a", Origin: "user", Status: "running", Events: []Event{{Kind: "tool", Name: "filesystem_read"}, {Kind: "thinking", Content: "private-thought"}, {Kind: "observation", Name: "filesystem_read", Content: value}}},
			{ID: "foreign", ChatID: "b", Origin: "user", Result: "other chat"}, {ID: "reset", ChatID: "a", Origin: "command"}, {ID: "older", ChatID: "a", Origin: "user", Result: "before reset"},
		}
		return nil
	})
	list := historyTool(t, a, "a", nil)
	if len(list["runs"].([]any)) != 1 {
		t.Fatal("chat/reset boundary leaked", list)
	}
	events := historyTool(t, a, "a", map[string]any{"run_id": "current", "limit": 1})
	if events["total_entries"] != float64(2) || events["truncated"] != true {
		t.Fatal(events)
	}
	next := historyTool(t, a, "a", map[string]any{"run_id": "current", "offset": events["next_offset"]})
	if next["events"].([]any)[0].(map[string]any)["event_index"] != float64(2) {
		t.Fatal("event indices changed after filtering thinking")
	}
	var got strings.Builder
	offset := float64(0)
	for {
		page := historyTool(t, a, "a", map[string]any{"run_id": "current", "event_index": 2, "offset": offset, "limit": 10000})
		got.WriteString(page["content"].(string))
		if page["truncated"] == false {
			break
		}
		offset = page["next_offset"].(float64)
	}
	if got.String() != value {
		t.Fatal("history pagination lost text")
	}
	for _, args := range []map[string]any{{"run_id": "foreign"}, {"run_id": "older"}, {"run_id": "current", "event_index": float64(1)}, {"event_index": float64(0)}} {
		if _, err := a.conversationHistory(context.Background(), Run{ChatID: "a"}, args); err == nil {
			t.Fatal("invalid history read accepted", args)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.conversationHistory(ctx, Run{ChatID: "a"}, nil); err == nil {
		t.Fatal("cancelled history read proceeded")
	}
}

func TestActionLimitExcludesFailuresAndAllowsFinalReport(t *testing.T) {
	for _, extraTool := range []bool{false, true} {
		t.Run(fmt.Sprint(extraTool), func(t *testing.T) {
			a := newTestApp(t)
			a.filesHome = t.TempDir()
			var calls atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []Message `json:"messages"`
					Tools    []ToolDef `json:"tools"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				name := "filesystem_read"
				args := map[string]any{"path": "missing.txt"}
				switch calls.Add(1) {
				case 1:
				case 2:
					name = "filesystem_write"
					args = map[string]any{"path": "created.txt", "content": "done"}
				case 3:
					name = "filesystem_stat"
					args = map[string]any{"path": "created.txt"}
				case 4:
					if len(body.Tools) != 0 {
						t.Error("final report can still request tools")
					}
					raw, _ := json.Marshal(body.Messages)
					if !strings.Contains(string(raw), "2 successful tool calls of 2; 1 failed calls of 3") {
						t.Error("missing runtime budget")
					}
					if !extraTool {
						fmt.Fprint(w, `{"message":{"content":"created.txt was saved and checked."}}`)
						return
					}
					name = "filesystem_write"
					args = map[string]any{"path": "must-not-write.txt", "content": "bad"}
				default:
					t.Error("exceeded final report allowance")
					http.Error(w, "too many requests", 500)
					return
				}
				raw, _ := json.Marshal(args)
				sendJSON(w, 200, map[string]any{"message": Message{ToolCalls: []ToolCall{{Function: FunctionCall{name, raw}}}}})
			}))
			defer model.Close()
			_ = a.store.Update(func(st *State) error {
				st.Config.BaseURL = model.URL
				st.Config.MaxSteps = 2
				st.Config.MaxToolFailures = 3
				return nil
			})
			r := postChat(t, a, "", "Create and verify")
			done := waitRun(t, a, r.ID)
			if calls.Load() != 4 || done.Status != "limit" || !strings.Contains(done.Result, "2 successful, 1 failed") {
				t.Fatal(calls.Load(), done.Status, done.Result)
			}
			if !extraTool && !strings.Contains(done.Result, "saved and checked") {
				t.Fatal("lost final report")
			}
			if _, err := os.Stat(filepath.Join(a.filesHome, "created.txt")); err != nil {
				t.Fatal("failures consumed the success budget")
			}
			if _, err := os.Stat(filepath.Join(a.filesHome, "must-not-write.txt")); err == nil {
				t.Fatal("tool executed during final summary")
			}
		})
	}
}
func TestFailureBudgetStopsRepeatedErrors(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 3 {
			fmt.Fprint(w, `{"message":{"content":"The requested file was not found."}}`)
			return
		}
		fmt.Fprint(w, `{"message":{"tool_calls":[{"function":{"name":"filesystem_read","arguments":{"path":"missing.txt"}}}]}}`)
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; st.Config.MaxToolFailures = 2; return nil })
	r := postChat(t, a, "", "Read a missing file")
	done := waitRun(t, a, r.ID)
	if calls.Load() != 3 || done.Status != "limit" || !strings.Contains(done.Result, "0 successful, 2 failed") {
		t.Fatal(calls.Load(), done.Status, done.Result)
	}
	if defaultConfig().MaxSteps != 30 || defaultConfig().toolFailureLimit() != 10 {
		t.Fatal("wrong defaults")
	}
}
func TestActionBudgetBalancesSkippedBatchCalls(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"first","function":{"name":"filesystem_write","arguments":"{\"path\":\"first.txt\",\"content\":\"done\"}"}},{"id":"second","function":{"name":"filesystem_write","arguments":"{\"path\":\"second.txt\",\"content\":\"skipped\"}"}}]}}]}`)
			return
		}
		var body struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		results := map[string]string{}
		for _, m := range body.Messages {
			if m.Role == "tool" {
				results[m.ToolCallID] = m.Content
			}
		}
		if !strings.Contains(results["first"], `"saved":true`) || !strings.Contains(results["second"], "Not executed") {
			t.Error("unbalanced tool results", results)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"Only first.txt was saved."}}]}`)
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error {
		st.Config.Provider = "openai"
		st.Config.BaseURL = model.URL
		st.Config.MaxSteps = 1
		return nil
	})
	r := postChat(t, a, "", "Create files")
	done := waitRun(t, a, r.ID)
	if calls.Load() != 2 {
		t.Fatal(calls.Load(), done.Status, done.Result)
	}
	if _, err := os.Stat(filepath.Join(a.filesHome, "second.txt")); err == nil {
		t.Fatal("extra batch tool executed")
	}
}
