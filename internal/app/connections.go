package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

type Connection struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key,omitempty"`
	KeySet   bool   `json:"key_set,omitempty"`
}

func (p Connection) config(base Config) Config {
	if p.Provider == "vscode" && base.Provider != "vscode" {
		base.MaxTokens = 0
	}
	if base.Provider != p.Provider || base.BaseURL != p.BaseURL || base.Model != p.Model {
		base.Reasoning = ""
		base.Thinking = nil
		base.Temperature = nil
	}
	base.ConnectionID = p.ID
	base.Provider = p.Provider
	base.BaseURL = p.BaseURL
	base.Model = p.Model
	base.APIKey = p.APIKey
	return base
}

// Discover models before a connection has a name or model, without saving it.
func (a *App) connectionModels(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Connection
		ClearAPIKey bool `json:"clear_api_key"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	p := input.Connection
	if input.ClearAPIKey && p.APIKey != "" {
		fail(w, 400, errors.New("enter a new API key or remove the saved key, not both"))
		return
	}
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if len(p.APIKey) > 8192 {
		fail(w, 400, errors.New("provide a valid API key"))
		return
	}
	if input.ClearAPIKey {
		p.APIKey = ""
	}
	if p.ID != "" {
		found := false
		for _, saved := range a.store.Snapshot().Connections {
			if saved.ID == p.ID {
				found = true
				if p.APIKey == "" && !input.ClearAPIKey && p.Provider == saved.Provider && p.BaseURL == saved.BaseURL {
					p.APIKey = saved.APIKey
				}
				break
			}
		}
		if !found {
			fail(w, 404, errors.New("connection not found"))
			return
		}
	}
	// Discovery needs only a provider, endpoint and optional credential.
	p.Model = "discovery"
	c := p.config(defaultConfig())
	if err := c.Validate(); err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	models, err := listModels(ctx, c)
	if err != nil {
		fail(w, 502, err)
		return
	}
	sendJSON(w, 200, map[string]any{"models": models})
}

func (a *App) connectionsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && r.PathValue("id") != "" {
		id := r.PathValue("id")
		var input struct {
			Action string `json:"action"`
		}
		if err := decode(w, r, &input); err != nil {
			fail(w, 400, err)
			return
		}
		if input.Action == "test" {
			st := a.store.Snapshot()
			for _, p := range st.Connections {
				if p.ID == id {
					ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
					defer cancel()
					models, err := listModels(ctx, p.config(st.Config))
					if err != nil {
						fail(w, 502, err)
						return
					}
					sendJSON(w, 200, map[string]any{"models": models})
					return
				}
			}
			fail(w, 404, errors.New("connection not found"))
			return
		}
		if input.Action != "activate" {
			fail(w, 400, errors.New("action must be activate or test"))
			return
		}
		a.runMu.Lock()
		defer a.runMu.Unlock()
		if a.active != "" {
			fail(w, 409, errors.New("stop the current run before switching connections"))
			return
		}
		err := a.store.Update(func(st *State) error {
			for _, p := range st.Connections {
				if p.ID == id {
					c := p.config(st.Config)
					if err := c.Validate(); err != nil {
						return err
					}
					st.Config = c
					return nil
				}
			}
			return errors.New("connection not found")
		})
		if err != nil {
			fail(w, 400, err)
			return
		}
		sendJSON(w, 200, map[string]bool{"saved": true})
		return
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.active != "" {
		fail(w, 409, errors.New("stop the current run before changing connections"))
		return
	}
	if r.Method == "DELETE" {
		err := a.store.Update(func(st *State) error {
			id := r.PathValue("id")
			if st.Config.ConnectionID == id {
				return errors.New("activate another connection before removing this one")
			}
			for i, p := range st.Connections {
				if p.ID == id {
					st.Connections = append(st.Connections[:i], st.Connections[i+1:]...)
					return nil
				}
			}
			return errors.New("connection not found")
		})
		if err != nil {
			fail(w, 400, err)
			return
		}
		sendJSON(w, 200, map[string]bool{"deleted": true})
		return
	}
	var input struct {
		Connection
		ClearAPIKey bool `json:"clear_api_key"`
		Activate    bool `json:"activate"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	p := input.Connection
	if input.ClearAPIKey && p.APIKey != "" {
		fail(w, 400, errors.New("enter a new API key or remove the saved key, not both"))
		return
	}
	if input.ClearAPIKey {
		p.APIKey = ""
	}
	p.Name = strings.TrimSpace(p.Name)
	p.Model = strings.TrimSpace(p.Model)
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	p.KeySet = false
	if p.Name == "" || len(p.Name) > 120 || len(p.APIKey) > 8192 {
		fail(w, 400, errors.New("provide a connection name (up to 120 characters) and a valid key"))
		return
	}
	fresh := p.ID == ""
	if fresh {
		p.ID = newID()
	}
	err := a.store.Update(func(st *State) error {
		index := -1
		for i, old := range st.Connections {
			if old.ID == p.ID {
				index = i
				if p.APIKey == "" && !input.ClearAPIKey && p.Provider == old.Provider && p.BaseURL == old.BaseURL {
					p.APIKey = old.APIKey
				}
				break
			}
		}
		if !fresh && index < 0 {
			return errors.New("connection not found")
		}
		c := p.config(st.Config)
		if err := c.Validate(); err != nil {
			return err
		}
		if index < 0 {
			if len(st.Connections) >= 20 {
				return errors.New("connection limit reached")
			}
			st.Connections = append(st.Connections, p)
		} else {
			st.Connections[index] = p
		}
		if input.Activate || st.Config.ConnectionID == p.ID {
			st.Config = c
		}
		return nil
	})
	if err != nil {
		fail(w, 400, err)
		return
	}
	p.KeySet = p.APIKey != ""
	p.APIKey = ""
	sendJSON(w, 200, p)
}

// Keep the active preset in sync, without moving credentials to another endpoint.
func saveConfig(st *State, c Config) {
	id := st.Config.ConnectionID
	c.ConnectionID = ""
	if c.Provider == st.Config.Provider && c.BaseURL == st.Config.BaseURL {
		for i := range st.Connections {
			if st.Connections[i].ID == id {
				c.ConnectionID = id
				st.Connections[i].Model = c.Model
				st.Connections[i].APIKey = c.APIKey
			}
		}
	}
	st.Config = c
}
