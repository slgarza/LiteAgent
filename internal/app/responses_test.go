package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResponsesProviderPreservesToolLoopAndReasoning(t *testing.T) {
	a := newTestApp(t)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer responses-secret" {
			t.Error("wrong Responses endpoint or authentication")
		}
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		if p["store"] != false || p["reasoning"].(map[string]any)["effort"] != "high" || p["max_output_tokens"] != float64(8000) {
			t.Errorf("wrong request controls: %v", p)
		}
		tools := p["tools"].([]any)
		if tools[0].(map[string]any)["name"] == nil || tools[0].(map[string]any)["strict"] != false {
			t.Error("wrong Responses tool schema")
		}
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"skills_list","arguments":"{}","status":"completed"}]}`)
			return
		}
		input := p["input"].([]any)
		last := input[len(input)-1].(map[string]any)
		if last["type"] != "function_call_output" || last["call_id"] != "call_1" {
			t.Error("lost tool call ID")
		}
		raw, _ := json.Marshal(input)
		if !strings.Contains(string(raw), "encrypted_content") || !strings.Contains(string(raw), "opaque") {
			t.Error("lost encrypted reasoning context")
		}
		fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"No skills installed."}]}]}`)
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error {
		st.Config.Provider = "openai-responses"
		st.Config.BaseURL = model.URL
		st.Config.APIKey = "responses-secret"
		st.Config.Reasoning = "high"
		st.Config.MaxTokens = 8000
		return nil
	})
	run := postChat(t, a, "", "List my skills")
	finished := waitRun(t, a, run.ID)
	a.wg.Wait()
	if finished.Status != "completed" || finished.Result != "No skills installed." || calls.Load() != 2 {
		t.Fatalf("Responses execution failed: %+v", finished)
	}
	public := callAPI(t, a, "GET", "/api/state", nil, nil).Body.String()
	if strings.Contains(public, "opaque") || strings.Contains(public, "responses-secret") {
		t.Fatal("private reasoning or API key leaked")
	}
}

func TestResponsesRejectsIncompleteToolOutput(t *testing.T) {
	a := newTestApp(t)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","call_id":"one","name":"skills_save","arguments":"{}"}]}`)
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error {
		st.Config.Provider = "openai-responses"
		st.Config.BaseURL = model.URL
		return nil
	})
	run := postChat(t, a, "", "Create skill")
	finished := waitRun(t, a, run.ID)
	if finished.Status != "failed" || !strings.Contains(finished.Result, "max_output_tokens") {
		t.Fatal("incomplete response accepted")
	}
	for _, event := range finished.Events {
		if event.Kind == "tool" {
			t.Fatal("incomplete tool call executed")
		}
	}
}
