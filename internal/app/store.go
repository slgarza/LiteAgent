package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	ConnectionID  string   `json:"connection_id,omitempty"`
	Provider      string   `json:"provider"`
	BaseURL       string   `json:"base_url"`
	Model         string   `json:"model"`
	APIKey        string   `json:"api_key,omitempty"`
	MaxSteps      int      `json:"max_steps"`
	TimeoutSec    int      `json:"timeout_sec"`
	Thinking      *bool    `json:"thinking,omitempty"`
	Reasoning     string   `json:"reasoning,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	MaxTokens     int      `json:"max_tokens,omitempty"`
	ContextTokens int      `json:"context_tokens,omitempty"`
	HistoryTurns  int      `json:"history_turns,omitempty"`
	RunTimeoutSec int      `json:"run_timeout_sec,omitempty"`
	SystemPrompt  string   `json:"system_prompt,omitempty"`
	Headless      bool     `json:"headless"`
	DriverPath    string   `json:"driver_path"`
	BrowserPath   string   `json:"browser_path"`
	Timezone      string   `json:"timezone"`
}

func defaultConfig() Config {
	return Config{Provider: "ollama", BaseURL: "http://127.0.0.1:11434", Model: "qwen3:8b", MaxSteps: 20, TimeoutSec: 900, Timezone: "America/Mexico_City"}
}

func (c Config) Validate() error {
	if err := c.validateGeneration(); err != nil {
		return err
	}
	if c.Provider != "ollama" && c.Provider != "openai" && c.Provider != "anthropic" && c.Provider != "openai-responses" {
		return errors.New("invalid provider")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("the URL must use HTTP or HTTPS, without embedded credentials or query parameters")
	}
	if strings.TrimSpace(c.Model) == "" || len(c.Model) > 200 {
		return errors.New("enter a model name")
	}
	if c.MaxSteps < 1 || c.MaxSteps > 50 {
		return errors.New("the action limit must be between 1 and 50")
	}
	if c.TimeoutSec < 10 || c.TimeoutSec > 86400 {
		return errors.New("the request timeout must be between 10 and 86400 seconds")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil || c.Timezone == "" || c.Timezone == "Local" {
		return errors.New("enter a valid IANA time zone, such as America/Mexico_City")
	}
	return nil
}

type Skill struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	UpdatedAt    string `json:"updated_at"`
}

type Event struct {
	Time    string `json:"time"`
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Content string `json:"content"`
}

type Run struct {
	RequestID    string   `json:"request_id,omitempty"`
	RequestHash  string   `json:"request_hash,omitempty"`
	ScheduleID   string   `json:"schedule_id,omitempty"`
	ScheduledFor string   `json:"scheduled_for,omitempty"`
	Reasoning    string   `json:"reasoning,omitempty"`
	ID           string   `json:"id"`
	ChatID       string   `json:"chat_id"`
	Origin       string   `json:"origin"`
	Task         string   `json:"task"`
	Status       string   `json:"status"`
	CreatedAt    string   `json:"created_at"`
	FinishedAt   string   `json:"finished_at,omitempty"`
	Model        string   `json:"model"`
	SkillIDs     []string `json:"skill_ids"`
	Events       []Event  `json:"events"`
	Result       string   `json:"result,omitempty"`
}

type Chat struct {
	ContextResetID string   `json:"context_reset_id,omitempty"`
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	SkillIDs       []string `json:"skill_ids"`
}

type State struct {
	Connections []Connection `json:"connections"`
	MCPServers  []MCPServer  `json:"mcp_servers"`
	Config      Config       `json:"config"`
	Skills      []Skill      `json:"skills"`
	Runs        []Run        `json:"runs"`
	Chats       []Chat       `json:"chats"`
	Schedules   []Schedule   `json:"schedules"`
}

type Store struct {
	mu    sync.Mutex
	path  string
	state State
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "\n[… content truncated]"
	}
	return s
}

func openStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "state.json"), state: State{Config: defaultConfig(), Skills: []Skill{}, Runs: []Run{}}}
	b, err := os.ReadFile(s.path)
	if err == nil {
		if err = json.Unmarshal(b, &s.state); err != nil {
			return nil, fmt.Errorf("could not read state.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if s.state.Connections == nil {
		c := s.state.Config
		s.state.Connections = []Connection{{ID: newID(), Name: "Initial connection", Provider: c.Provider, BaseURL: c.BaseURL, Model: c.Model, APIKey: c.APIKey}}
		s.state.Config.ConnectionID = s.state.Connections[0].ID
	}
	for i := range s.state.Runs {
		if s.state.Runs[i].ChatID == "" {
			r := &s.state.Runs[i]
			r.ChatID = newID()
			s.state.Chats = append(s.state.Chats, Chat{ID: r.ChatID, Title: cut(r.Task, 70), CreatedAt: r.CreatedAt, UpdatedAt: r.CreatedAt, SkillIDs: r.SkillIDs})
		}
		if s.state.Runs[i].Status == "running" {
			s.state.Runs[i].Status = "interrupted"
			s.state.Runs[i].FinishedAt = now()
			s.state.Runs[i].Result = "The application closed before this task finished."
			finishSchedule(&s.state, s.state.Runs[i])
		}
	}
	if s.state.Config.Timezone == "" {
		s.state.Config.Timezone = defaultConfig().Timezone
	}
	if err = s.saveLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// Persist by atomic replacement; a partial write never replaces the last valid state.
func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, s.path)
	}
	return err
}

func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.state)
	var result State
	_ = json.Unmarshal(b, &result)
	return result
}

func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	backup, _ := json.Marshal(s.state)
	if err := fn(&s.state); err != nil {
		s.state = State{}
		_ = json.Unmarshal(backup, &s.state)
		return err
	}
	if err := s.saveLocked(); err != nil {
		s.state = State{}
		_ = json.Unmarshal(backup, &s.state)
		return err
	}
	return nil
}

func (s *Store) AddEvent(id, kind, name, content string) error {
	return s.Update(func(st *State) error {
		for i := range st.Runs {
			if st.Runs[i].ID == id {
				st.Runs[i].Events = append(st.Runs[i].Events, Event{now(), kind, name, cut(content, 18000)})
				return nil
			}
		}
		return errors.New("task not found")
	})
}
