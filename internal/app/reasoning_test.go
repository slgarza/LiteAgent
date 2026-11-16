package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReasoningControlsComeFromModelMetadata(t *testing.T) {
	for _, test := range []struct {
		name, provider, response string
		modes                    []string
		known                    bool
		defaultMode              string
	}{
		{"ollama-toggle", "ollama", `{"thinking":{"values":[false,true],"default":true}}`, []string{"auto", "off", "on"}, true, "on"},
		{"ollama-levels", "ollama", `{"thinking":{"values":["low","medium","high"],"default":"medium"}}`, []string{"auto", "low", "medium", "high"}, true, "medium"},
		{"ollama-custom", "ollama", `{"thinking":{"values":["brief","Deep","Deep",123,"<script>"],"default":"Deep"}}`, []string{"auto", "brief", "Deep"}, true, "Deep"},
		{"ollama-no-thinking", "ollama", `{"thinking":{"values":[false],"default":false}}`, []string{"auto", "off"}, true, "off"},
		{"ollama-legacy", "ollama", `{"capabilities":["thinking","tools"]}`, []string{"auto"}, false, ""},
		{"openai-no-metadata", "openai", `{"id":"fixture"}`, []string{"auto"}, false, ""},
		{"compatible-levels", "openai-responses", `{"supported_reasoning_efforts":["low","xhigh","unsupported"]}`, []string{"auto", "low", "xhigh"}, true, ""},
		{"claude-adaptive", "anthropic", `{"capabilities":{"thinking":{"supported":true,"types":{"adaptive":{"supported":true},"disabled":{"supported":true}}},"effort":{"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"max":{"supported":false}}}}`, []string{"auto", "off", "low", "medium", "high"}, true, ""},
		{"claude-budget-only", "anthropic", `{"capabilities":{"thinking":{"supported":true,"types":{"enabled":{"supported":true},"disabled":{"supported":true}}},"effort":{"high":{"supported":true}}}}`, []string{"auto", "off"}, true, ""},
		{"claude-unknown", "anthropic", `{"id":"fixture"}`, []string{"auto"}, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.provider == "ollama" {
					var request map[string]string
					_ = json.NewDecoder(r.Body).Decode(&request)
					if r.Method != "POST" || r.URL.Path != "/api/show" || request["model"] != "fixture" {
						t.Error("wrong Ollama metadata request")
					}
				} else {
					want := "/models/fixture"
					if test.provider == "anthropic" {
						want = "/v1" + want
					}
					if r.Method != "GET" || r.URL.Path != want {
						t.Error("wrong model metadata request")
					}
				}
				fmt.Fprint(w, test.response)
			}))
			defer server.Close()
			c := defaultConfig()
			c.Provider, c.Model, c.BaseURL = test.provider, "fixture", server.URL
			info, err := modelReasoning(context.Background(), c)
			if err != nil || !reflect.DeepEqual(info.Modes, test.modes) || info.Known != test.known || info.Default != test.defaultMode {
				t.Fatalf("unexpected capabilities: %+v, %v", info, err)
			}
		})
	}
}

func TestReasoningDiscoveryScopesValidationAndDoesNotBlockStop(t *testing.T) {
	a := newTestApp(t)
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `{"thinking":{"values":["low","high"]}}`)
	}))
	defer server.Close()
	defer close(release)
	_ = a.store.Update(func(st *State) error {
		st.Config.BaseURL, st.Config.APIKey = server.URL, "capability-secret"
		return nil
	})
	done := make(chan struct{})
	go func() { defer close(done); a.discoverReasoning(context.Background(), a.store.Snapshot().Config) }()
	<-started
	stopped := make(chan struct{})
	go func() { defer close(stopped); callAPI(t, a, "POST", "/api/stop", nil, nil) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("discovery held runMu")
	}
	release <- struct{}{}
	<-done
	c := a.store.Snapshot().Config
	c.Reasoning = "on"
	if a.validateModelReasoning(c) == nil {
		t.Fatal("accepted an unadvertised toggle")
	}
	w := callAPI(t, a, "PATCH", "/api/generation", map[string]any{"provider": c.Provider, "model": c.Model, "base_url": c.BaseURL, "settings": map[string]string{"reasoning": "on"}}, nil)
	if w.Code != 400 {
		t.Fatal("generation accepted an unadvertised control")
	}
	for _, change := range []func(*Config){func(c *Config) { c.Model = "other" }, func(c *Config) { c.BaseURL += "/other" }, func(c *Config) { c.APIKey = "rotated" }, func(c *Config) { c.Provider = "openai" }} {
		other := c
		change(&other)
		if a.cachedReasoning(other).Known {
			t.Fatal("capabilities crossed model/connection boundaries")
		}
	}
	public := callAPI(t, a, "GET", "/api/state", nil, nil).Body.String()
	if strings.Contains(public, "capability-secret") || strings.Contains(public, `"reasoning_modes":["auto","off"`) {
		t.Fatal("state leaked credentials or generic controls")
	}
}

func TestReasoningDiscoveryFailureIsExplicit(t *testing.T) {
	a := newTestApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "private-provider-data", 500) }))
	defer server.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = server.URL; return nil })
	w := callAPI(t, a, "GET", "/api/reasoning", nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Could not read") || strings.Contains(w.Body.String(), "private-provider-data") {
		t.Fatal(w.Body.String())
	}
}

func TestReasoningCommandPreservesModelDefinedEffort(t *testing.T) {
	a := newTestApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			fmt.Fprint(w, `{"thinking":{"values":["brief","Deep"],"default":"brief"}}`)
			return
		}
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request["think"] != "Deep" {
			t.Error("model-defined effort changed on the wire")
		}
		fmt.Fprint(w, `{"message":{"content":"Done."}}`)
	}))
	defer server.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = server.URL; return nil })
	reply := postChat(t, a, "", "/reasoning")
	if !strings.Contains(reply.Result, "auto, brief, Deep") || strings.Contains(reply.Result, "medium") {
		t.Fatal("command used generic levels")
	}
	postChat(t, a, "", "/reasoning Deep")
	if a.store.Snapshot().Config.Reasoning != "Deep" {
		t.Fatal("effort lost case")
	}
	postChat(t, a, "", "/reasoning high")
	if a.store.Snapshot().Config.Reasoning != "Deep" {
		t.Fatal("command accepted unreported effort")
	}
	run := waitRun(t, a, postChat(t, a, "", "Check fixture").ID)
	a.wg.Wait()
	if run.Status != "completed" {
		t.Fatal("custom effort failed")
	}
	postChat(t, a, "", "/model different-model")
	if a.store.Snapshot().Config.reasoningMode() != "auto" {
		t.Fatal("model switch retained custom effort")
	}
}

func TestSlowReasoningDiscoveryCannotReplaceNewModelCapabilities(t *testing.T) {
	a := newTestApp(t)
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]string
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request["model"] == "old" {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, `{"thinking":{"values":[false,true]}}`)
		} else {
			fmt.Fprint(w, `{"thinking":{"values":["high"]}}`)
		}
	}))
	defer server.Close()
	defer close(release)
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL, st.Config.Model = server.URL, "old"; return nil })
	old := a.store.Snapshot().Config
	done := make(chan struct{})
	go func() { defer close(done); a.discoverReasoning(context.Background(), old) }()
	<-started
	_ = a.store.Update(func(st *State) error { st.Config.Model = "new"; return nil })
	current := a.store.Snapshot().Config
	a.discoverReasoning(context.Background(), current)
	release <- struct{}{}
	<-done
	if !reflect.DeepEqual(a.cachedReasoning(current).Modes, []string{"auto", "high"}) {
		t.Fatal("stale discovery replaced current capabilities")
	}
}

func TestAgentRecordsOnlyVisibleModelReasoning(t *testing.T) {
	for _, test := range []struct{ provider, response, visible string }{
		{"ollama", `{"message":{"thinking":"Model plan: verify the fixture.","content":"Done."}}`, "Model plan: verify the fixture."},
		{"openai", `{"choices":[{"message":{"reasoning_content":"Check the supplied totals.","content":"Done."}}]}`, "Check the supplied totals."},
		{"openai", `{"choices":[{"message":{"reasoning":"Read the fixture.","content":"Done."}}]}`, "Read the fixture."},
		{"anthropic", `{"content":[{"type":"thinking","thinking":"Compare the fixture dates.","signature":"private-signature"},{"type":"redacted_thinking","data":"private-redaction"},{"type":"text","text":"Done."}]}`, "Compare the fixture dates."},
		{"openai-responses", `{"status":"completed","output":[{"type":"reasoning","encrypted_content":"private-encrypted","summary":[{"type":"summary_text","text":"Verified the supplied data."}]},{"type":"message","content":[{"type":"output_text","text":"Done."}]}]}`, "Verified the supplied data."},
		{"ollama", `{"message":{"content":"Done."}}`, ""},
		{"ollama", `{"message":{"thinking":"Echo fixture-secret.","content":"Done."}}`, "Echo [key hidden]."},
	} {
		t.Run(test.provider+test.visible, func(t *testing.T) {
			a := newTestApp(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, test.response) }))
			defer server.Close()
			_ = a.store.Update(func(st *State) error {
				st.Config.Provider, st.Config.BaseURL, st.Config.APIKey = test.provider, server.URL, "fixture-secret"
				return nil
			})
			run := waitRun(t, a, postChat(t, a, "", "Check fixture").ID)
			a.wg.Wait()
			if run.Status != "completed" || run.Result != "Done." {
				t.Fatalf("run failed: %+v", run)
			}
			var thinking []string
			for _, event := range run.Events {
				if event.Kind == "thinking" {
					thinking = append(thinking, event.Content)
				}
			}
			if test.visible == "" && len(thinking) != 0 || test.visible != "" && (len(thinking) != 1 || thinking[0] != test.visible) {
				t.Fatalf("wrong visible reasoning: %v", thinking)
			}
			public := callAPI(t, a, "GET", "/api/state", nil, nil).Body.String()
			for _, secret := range []string{"private-signature", "private-redaction", "private-encrypted", "fixture-secret"} {
				if strings.Contains(public, secret) {
					t.Fatal("private metadata leaked")
				}
			}
			reopened, err := openStore(a.dir)
			if err != nil || !reflect.DeepEqual(reopened.Snapshot().Runs[0].Events, run.Events) {
				t.Fatal("reasoning history did not persist")
			}
		})
	}
}
