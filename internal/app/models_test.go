package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestModelPickerListsConfiguredProvider(t *testing.T) {
	for _, provider := range []string{"ollama", "openai", "openai-responses", "anthropic", "vscode"} {
		t.Run(provider, func(t *testing.T) {
			a := newTestApp(t)
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path, auth, value := "/models", "Authorization", "Bearer picker-secret"
				if provider == "ollama" {
					path = "/api/tags"
				} else if provider == "anthropic" {
					path, auth, value = "/v1/models", "x-api-key", "picker-secret"
				}
				if r.Method != "GET" || r.URL.Path != path || r.Header.Get(auth) != value {
					t.Error("model discovery did not use the configured provider and credentials")
				}
				if provider == "ollama" {
					fmt.Fprint(w, `{"models":[{"name":"quick"},{"name":"agent"}]}`)
				} else {
					fmt.Fprint(w, `{"data":[{"id":"quick"},{"id":"agent"}]}`)
				}
			}))
			defer model.Close()
			_ = a.store.Update(func(st *State) error {
				st.Config.Provider, st.Config.BaseURL, st.Config.APIKey = provider, model.URL, "picker-secret"
				return nil
			})
			w := callAPI(t, a, "GET", "/api/models", nil, nil)
			var result struct {
				Models []string `json:"models"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			if !reflect.DeepEqual(result.Models, []string{"quick", "agent"}) || strings.Contains(w.Body.String(), "picker-secret") {
				t.Fatal("incorrect model list or exposed credentials")
			}
			if len(a.store.Snapshot().Runs) != 0 {
				t.Fatal("opening the model list added a chat message")
			}
		})
	}
}

func TestModelPickerChangesOnlyModelAndPersists(t *testing.T) {
	a := newTestApp(t)
	thinking := false
	_ = a.store.Update(func(st *State) error {
		st.Config.APIKey, st.Config.Thinking, st.Config.TimeoutSec = "keep-secret", &thinking, 450
		return nil
	})
	previous := a.store.Snapshot().Config
	input := map[string]string{"model": " quick ", "provider": previous.Provider, "base_url": previous.BaseURL}
	w := callAPI(t, a, "PUT", "/api/model", input, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "keep-secret") {
		t.Fatal(w.Body.String())
	}
	expected := previous
	expected.Model = "quick"
	expected.Reasoning, expected.Thinking = "", nil
	reopened, err := openStore(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened.Snapshot().Config, expected) || len(reopened.Snapshot().Chats) != 0 {
		t.Fatal("selection did not persist or changed unrelated settings/history")
	}
	for _, scenario := range []struct {
		name, model, provider, url, active string
		code                               int
	}{
		{"busy", "other", previous.Provider, previous.BaseURL, "running", 409},
		{"provider changed", "other", "openai", previous.BaseURL, "", 409},
		{"URL changed", "other", previous.Provider, "http://other.invalid", "", 409},
		{"empty model", "  ", previous.Provider, previous.BaseURL, "", 400},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			a.runMu.Lock()
			a.active = scenario.active
			a.runMu.Unlock()
			w := callAPI(t, a, "PUT", "/api/model", map[string]string{"model": scenario.model, "provider": scenario.provider, "base_url": scenario.url}, nil)
			if w.Code != scenario.code || !reflect.DeepEqual(a.store.Snapshot().Config, expected) {
				t.Fatal("invalid selection modified settings: " + w.Body.String())
			}
		})
	}
}

func TestConnectionModelsBeforeSaving(t *testing.T) {
	for _, provider := range []string{"ollama", "openai", "openai-responses", "anthropic", "vscode"} {
		t.Run(provider, func(t *testing.T) {
			a := newTestApp(t)
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path, header, key := "/models", "Authorization", "Bearer draft-secret"
				if provider == "ollama" {
					path = "/api/tags"
				} else if provider == "anthropic" {
					path, header, key = "/v1/models", "x-api-key", "draft-secret"
				}
				if r.Method != "GET" || r.URL.Path != path || r.Header.Get(header) != key {
					t.Error("draft discovery used the wrong endpoint or credential")
				}
				if provider == "ollama" {
					fmt.Fprint(w, `{"models":[{"name":"available-model"}]}`)
				} else {
					fmt.Fprint(w, `{"data":[{"id":"available-model"}]}`)
				}
			}))
			defer remote.Close()
			before := a.store.Snapshot()
			a.active = "another-run"
			w := callAPI(t, a, "POST", "/api/connections/models", map[string]any{"provider": provider, "base_url": remote.URL + "/", "api_key": "draft-secret"}, nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"available-model"`) || strings.Contains(w.Body.String(), "draft-secret") {
				t.Fatal("draft model discovery failed: " + w.Body.String())
			}
			if !reflect.DeepEqual(before, a.store.Snapshot()) {
				t.Fatal("discovering draft models changed saved settings or history")
			}
		})
	}
}

func TestConnectionModelDiscoveryCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, provider, suffix, key, expected string
		clear, providerError                  bool
	}{
		{name: "saved key", provider: "openai", expected: "saved-secret"},
		{name: "new key", provider: "openai", key: "new-secret", expected: "new-secret"},
		{name: "removed key", provider: "openai", clear: true},
		{name: "different URL", provider: "openai", suffix: "/changed"},
		{name: "different provider", provider: "anthropic"},
		{name: "provider error redacts key", provider: "openai", expected: "saved-secret", providerError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				if tc.provider == "anthropic" {
					auth = r.Header.Get("x-api-key")
				}
				if auth != tc.expected {
					t.Error("wrong credential used for discovery")
				}
				if tc.providerError {
					http.Error(w, "rejected saved-secret", 401)
					return
				}
				fmt.Fprint(w, `{"data":[]}`)
			}))
			defer remote.Close()
			_ = a.store.Update(func(st *State) error {
				st.Connections = append(st.Connections, Connection{ID: "saved", Name: "Saved", Provider: "openai", BaseURL: remote.URL, Model: "existing-model", APIKey: "saved-secret"})
				return nil
			})
			before := a.store.Snapshot()
			w := callAPI(t, a, "POST", "/api/connections/models", map[string]any{"id": "saved", "provider": tc.provider, "base_url": remote.URL + tc.suffix, "api_key": tc.key, "clear_api_key": tc.clear}, nil)
			code := 200
			if tc.providerError {
				code = 502
			}
			if w.Code != code || strings.Contains(w.Body.String(), "saved-secret") || strings.Contains(w.Body.String(), "new-secret") {
				t.Fatal("incorrect response or exposed credential")
			}
			if !reflect.DeepEqual(before, a.store.Snapshot()) {
				t.Fatal("model discovery changed the saved connection")
			}
		})
	}
}

func TestConnectionModelsRejectInvalidDrafts(t *testing.T) {
	a := newTestApp(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid draft reached the provider")
	}))
	defer remote.Close()
	for _, change := range []map[string]any{
		{"provider": "unknown"}, {"base_url": "file:///tmp/model"}, {"base_url": "https://user:password@example.com"},
		{"api_key": strings.Repeat("x", 8193)}, {"id": "missing"}, {"api_key": "new-secret", "clear_api_key": true},
	} {
		input := map[string]any{"provider": "openai", "base_url": remote.URL}
		for key, value := range change {
			input[key] = value
		}
		w := callAPI(t, a, "POST", "/api/connections/models", input, nil)
		if w.Code != 400 && w.Code != 404 {
			t.Fatalf("invalid draft accepted: %d", w.Code)
		}
	}
}
