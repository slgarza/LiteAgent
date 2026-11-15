package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	webassets "github.com/slgarza/LiteAgent/web"
)

type App struct {
	store         *Store
	browser       *Browser
	dir           string
	filesHome     string     // Empty uses the current user's home; tests use an isolated directory.
	filesMu       sync.Mutex // Serialize local reads and hash-guarded mutations within this app.
	runMu         sync.Mutex
	active        string
	cancel        context.CancelFunc
	runDone       <-chan struct{}
	deletingChats map[string]bool
	root          context.Context
	wg            sync.WaitGroup
	requestID     string
	requestHash   string
	wizards       map[string]*onboarding
}

func (a *App) logError(err error) { log.Printf("LiteAgent: %v", err) }
func sendJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func fail(w http.ResponseWriter, status int, err error) {
	sendJSON(w, status, map[string]string{"error": err.Error()})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("invalid request: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("only one JSON object is allowed")
	}
	return nil
}

func localHost(host string) bool {
	h := host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		h = parsed
	}
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("PUT /api/config", a.config)
	mux.HandleFunc("PATCH /api/generation", a.generation)
	mux.HandleFunc("PUT /api/connections", a.connectionsAPI)
	mux.HandleFunc("POST /api/connections/models", a.connectionModels)
	mux.HandleFunc("POST /api/connections/{id}", a.connectionsAPI)
	mux.HandleFunc("DELETE /api/connections/{id}", a.connectionsAPI)
	mux.HandleFunc("PUT /api/mcp", a.mcpAPI)
	mux.HandleFunc("POST /api/mcp/{id}/test", a.testMCP)
	mux.HandleFunc("DELETE /api/mcp/{id}", a.mcpAPI)
	mux.HandleFunc("GET /api/schedules", a.schedulesAPI)
	mux.HandleFunc("POST /api/schedules", a.schedulesAPI)
	mux.HandleFunc("POST /api/schedules/{id}", a.schedulesAPI)
	mux.HandleFunc("GET /api/models", a.models)
	mux.HandleFunc("PUT /api/model", a.selectModel)
	mux.HandleFunc("POST /api/test", a.testModel)
	mux.HandleFunc("POST /api/runs", a.startRun)
	mux.HandleFunc("POST /api/chat", a.startRun)
	mux.HandleFunc("DELETE /api/chats/{id}", a.deleteChat)
	mux.HandleFunc("POST /api/stop", a.stopRun)
	mux.HandleFunc("POST /api/browser/open", a.openBrowser)
	mux.HandleFunc("POST /api/browser/close", a.closeBrowser)
	mux.HandleFunc("PUT /api/skills", a.saveSkill)
	mux.HandleFunc("DELETE /api/skills/{id}", a.deleteSkill)
	mux.HandleFunc("POST /api/skills/draft", a.draftSkill)
	files := http.FileServer(http.FS(webassets.Assets))
	mux.Handle("GET /", files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if !localHost(r.Host) {
			fail(w, 403, errors.New("host not allowed"))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || u.Scheme != "http" {
				fail(w, 403, errors.New("origin not allowed"))
				return
			}
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			fail(w, 403, errors.New("cross-site request not allowed"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			if r.Method != "GET" && r.Header.Get("X-LiteAgent") != "1" {
				fail(w, 403, errors.New("application header is missing"))
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *App) state(w http.ResponseWriter, r *http.Request) {
	a.runMu.Lock()
	st := a.store.Snapshot()
	active := a.active
	a.runMu.Unlock()
	hasKey := st.Config.APIKey != ""
	st.Config.APIKey = ""
	for i := range st.Connections {
		st.Connections[i].KeySet = st.Connections[i].APIKey != ""
		st.Connections[i].APIKey = ""
	}
	for i := range st.MCPServers {
		st.MCPServers[i].TokenSet = st.MCPServers[i].Token != ""
		st.MCPServers[i].Token = ""
	}
	notices := []map[string]string{}
	alerts := stateAlerts(st)
	for _, run := range st.Runs {
		if run.Origin == "reminder" {
			notices = append(notices, map[string]string{"id": run.ID, "chat_id": run.ChatID, "text": run.Result, "created_at": run.CreatedAt})
			if len(notices) >= 20 {
				break
			}
		}
	}
	activeChatID := ""
	for _, run := range st.Runs {
		if run.ID == active {
			activeChatID = run.ChatID
			break
		}
	}
	if id := r.URL.Query().Get("chat_id"); id != "" {
		runs := []Run{}
		for _, run := range st.Runs {
			if run.ChatID == id {
				runs = append(runs, run)
			}
		}
		st.Runs = runs
	}
	if st.Chats == nil {
		st.Chats = []Chat{}
	}
	sendJSON(w, 200, map[string]any{"config": st.Config, "api_key_set": hasKey, "skills": st.Skills, "runs": st.Runs, "chats": st.Chats, "notices": notices, "alerts": alerts, "active": active, "active_chat_id": activeChatID, "downloads_dir": filepath.Join(a.dir, "downloads"), "connections": st.Connections, "mcp_servers": st.MCPServers, "schedules": st.Schedules, "reasoning_modes": reasoningModes(st.Config.Provider), "version": Version})
}

func (a *App) config(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Config
		ClearAPIKey bool `json:"clear_api_key"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	input.BaseURL = strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	input.Model = strings.TrimSpace(input.Model)
	if err := input.Config.Validate(); err != nil {
		fail(w, 400, err)
		return
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.active != "" {
		fail(w, 409, errors.New("stop the task before changing settings"))
		return
	}
	err := a.store.Update(func(st *State) error {
		// Never carry a saved credential over to a different endpoint or provider.
		if input.APIKey == "" && !input.ClearAPIKey && input.Provider == st.Config.Provider && input.BaseURL == st.Config.BaseURL {
			input.APIKey = st.Config.APIKey
		}
		saveConfig(st, input.Config)
		return nil
	})
	if err != nil {
		fail(w, 500, err)
		return
	}
	a.browser.Close()
	sendJSON(w, 200, map[string]bool{"saved": true})
}

func (a *App) models(w http.ResponseWriter, r *http.Request) {
	c := a.store.Snapshot().Config
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	models, err := listModels(ctx, c)
	if err != nil {
		fail(w, http.StatusBadGateway, fmt.Errorf("could not list models: %w", err))
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"models": models, "provider": c.Provider, "base_url": c.BaseURL})
}

func (a *App) selectModel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model    string `json:"model"`
		Provider string `json:"provider"`
		BaseURL  string `json:"base_url"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.active != "" {
		fail(w, http.StatusConflict, errors.New("finish or stop the current reply before switching models"))
		return
	}
	c := a.store.Snapshot().Config
	if input.Provider != c.Provider || input.BaseURL != c.BaseURL {
		fail(w, http.StatusConflict, errors.New("the provider changed; refresh the model list and try again"))
		return
	}
	c.Model = strings.TrimSpace(input.Model)
	if err := c.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := a.store.Update(func(st *State) error {
		saveConfig(st, c)
		return nil
	}); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	sendJSON(w, http.StatusOK, map[string]string{"model": c.Model})
}

func (a *App) testModel(w http.ResponseWriter, r *http.Request) {
	c := a.store.Snapshot().Config
	msg, err := complete(r.Context(), c, []Message{{Role: "user", Content: "Reply only: Connection ready."}}, nil)
	if err != nil {
		fail(w, 502, err)
		return
	}
	sendJSON(w, 200, map[string]string{"message": cut(msg.Content, 200), "model": c.Model})
}

type ChatRequest struct {
	ModelsReply  string   `json:"-"`
	RequestID    string   `json:"request_id,omitempty"`
	ScheduleID   string   `json:"-"`
	ScheduledFor string   `json:"-"`
	Task         string   `json:"task"`
	Message      string   `json:"message"`
	ChatID       string   `json:"chat_id"`
	SkillIDs     []string `json:"skill_ids"`
	Timezone     string   `json:"timezone"`
}

func (a *App) startRun(w http.ResponseWriter, r *http.Request) {
	var input ChatRequest
	if err := decode(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	// Network discovery must not block /stop, state polling, or due reminders.
	message := input.Task
	if input.Message != "" {
		message = input.Message
	}
	if cmd, _ := splitCommand(message); cmd == "/models" {
		input.ModelsReply = modelsReply(r.Context(), a.store.Snapshot().Config)
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if err := a.available(); err != nil {
		fail(w, 503, err)
		return
	}
	if a.deletingChats[input.ChatID] {
		fail(w, 409, errors.New("this conversation is being deleted"))
		return
	}
	if input.ChatID != "" {
		found := false
		for _, chat := range a.store.Snapshot().Chats {
			found = found || chat.ID == input.ChatID
		}
		if !found {
			fail(w, 404, errors.New("conversation not found"))
			return
		}
	}
	if len(input.RequestID) > 128 {
		fail(w, 400, errors.New("request_id is too long"))
		return
	}
	// Reusing a submission ID returns its durable result, including after restart.
	id := input.RequestID
	input.RequestID = ""
	raw, _ := json.Marshal(input)
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	if id != "" {
		for _, previous := range a.store.Snapshot().Runs {
			if previous.RequestID == id {
				if previous.RequestHash != hash {
					fail(w, 409, errors.New("request_id was already used for another message"))
					return
				}
				sendJSON(w, 202, previous)
				return
			}
		}
	}
	a.requestID = id
	a.requestHash = hash
	defer func() { a.requestID = ""; a.requestHash = "" }()
	if input.Message != "" {
		input.Task = input.Message
	}
	if len(strings.TrimSpace(input.Task)) == 0 || len(input.Task) > 12000 {
		fail(w, 400, errors.New("enter a message between 1 and 12000 characters"))
		return
	}
	if strings.HasPrefix(strings.TrimSpace(input.Task), "/") || a.wizards[input.ChatID] != nil {
		run, err := a.command(r.Context(), input)
		if err != nil {
			fail(w, 400, err)
			return
		}
		sendJSON(w, 202, run)
		return
	}
	run, err := a.beginRun(input, "user", nil)
	if err != nil {
		code := 400
		if a.active != "" {
			code = 409
		}
		fail(w, code, err)
		return
	}
	sendJSON(w, 202, run)
}

// Caller holds runMu. The optional claim is committed with the new run, so a
// scheduled occurrence cannot be claimed without also recording its execution.
func (a *App) beginRun(input ChatRequest, origin string, claim func(*State) error) (Run, error) {
	if err := a.available(); err != nil {
		return Run{}, err
	}
	if a.active != "" {
		return Run{}, errors.New("another reply is still running")
	}
	if a.deletingChats[input.ChatID] {
		return Run{}, errors.New("this conversation is being deleted")
	}
	if input.Message != "" {
		input.Task = input.Message
	}
	input.Task = strings.TrimSpace(input.Task)
	if len(input.Task) < 1 || len(input.Task) > 12000 {
		return Run{}, errors.New("enter a message between 1 and 12000 characters")
	}
	st := a.store.Snapshot()
	if input.ChatID != "" {
		found := false
		for _, chat := range st.Chats {
			if chat.ID == input.ChatID {
				found = true
				break
			}
		}
		if !found {
			return Run{}, errors.New("conversation not found")
		}
	}
	if err := st.Config.Validate(); err != nil {
		return Run{}, err
	}
	skills, err := selectedSkills(st, input.Task, input.SkillIDs)
	if err != nil {
		return Run{}, err
	}
	ids := []string{}
	for _, s := range skills {
		ids = append(ids, s.ID)
	}
	chatID := input.ChatID
	if chatID == "" {
		chatID = newID()
	}
	run := Run{ID: newID(), RequestID: a.requestID, RequestHash: a.requestHash, ScheduleID: input.ScheduleID, ScheduledFor: input.ScheduledFor, Reasoning: st.Config.reasoningMode(), ChatID: chatID, Origin: origin, Task: input.Task, Status: "running", CreatedAt: now(), Model: st.Config.Model, SkillIDs: ids, Events: []Event{}}
	history := chatContext(st, chatID)
	if origin == "scheduled" {
		history = nil
	}
	err = a.store.Update(func(s *State) error {
		if claim != nil {
			if err := claim(s); err != nil {
				return err
			}
		}
		if input.ChatID == "" {
			s.Chats = append([]Chat{{ID: chatID, Title: cut(input.Task, 70), CreatedAt: now(), UpdatedAt: now(), SkillIDs: input.SkillIDs}}, s.Chats...)
		} else {
			for i, chat := range s.Chats {
				if chat.ID == chatID {
					chat.UpdatedAt = now()
					if origin == "user" && strings.HasPrefix(chat.Title, "/") {
						chat.Title = cut(input.Task, 70)
					}
					chat.SkillIDs = input.SkillIDs
					s.Chats = append(s.Chats[:i], s.Chats[i+1:]...)
					s.Chats = append([]Chat{chat}, s.Chats...)
					break
				}
			}
		}
		s.Runs = append([]Run{run}, s.Runs...)
		linkSchedule(s, run)
		return nil
	})
	if err != nil {
		return Run{}, err
	}
	ctx, cancel := context.WithTimeout(a.root, time.Duration(st.Config.runTimeout())*time.Second)
	a.cancel = cancel
	a.active = run.ID
	done := make(chan struct{})
	a.runDone = done
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer close(done)
		defer cancel()
		a.executeRun(ctx, run, st.Config, skills, history)
	}()
	return run, nil
}

func (a *App) deleteChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.runMu.Lock()
	if a.deletingChats[id] {
		a.runMu.Unlock()
		fail(w, 409, errors.New("this conversation is already being deleted"))
		return
	}
	if a.deletingChats == nil {
		a.deletingChats = map[string]bool{}
	}
	a.deletingChats[id] = true
	a.runMu.Unlock()
	defer func() { a.runMu.Lock(); delete(a.deletingChats, id); a.runMu.Unlock() }()
	for {
		a.runMu.Lock()
		if r.Context().Err() != nil {
			a.runMu.Unlock()
			fail(w, 408, errors.New("deletion was interrupted, so the conversation was kept"))
			return
		}
		st := a.store.Snapshot()
		found, running := false, false
		for _, chat := range st.Chats {
			found = found || chat.ID == id
		}
		if !found {
			a.runMu.Unlock()
			fail(w, 404, errors.New("conversation not found"))
			return
		}
		for _, run := range st.Runs {
			if run.ID == a.active && run.ChatID == id {
				running = true
				break
			}
		}
		if running {
			done := a.runDone
			if a.cancel == nil || done == nil {
				a.runMu.Unlock()
				fail(w, 409, errors.New("the active reply cannot be stopped yet; try again shortly"))
				return
			}
			a.cancel()
			a.runMu.Unlock()
			// The run needs runMu during cleanup. Wait without holding it, and
			// keep its history until all tools and scheduled-work cleanup finish.
			select {
			case <-done:
				continue
			case <-r.Context().Done():
				fail(w, 408, errors.New("reply stopping; deletion was interrupted, so the conversation was kept"))
				return
			}
		}
		err := a.store.Update(func(st *State) error { return removeChatRecords(st, id) })
		if err == nil {
			delete(a.wizards, id)
		}
		a.runMu.Unlock()
		if err != nil {
			fail(w, 500, err)
			return
		}
		sendJSON(w, 200, map[string]bool{"deleted": true})
		return
	}
}

func removeChatRecords(st *State, id string) error {
	found := false
	chats := []Chat{}
	for _, c := range st.Chats {
		if c.ID == id {
			found = true
		} else {
			chats = append(chats, c)
		}
	}
	if !found {
		return errors.New("conversation not found")
	}
	st.Chats = chats
	schedules := []Schedule{}
	for _, s := range st.Schedules {
		if s.ChatID != id {
			schedules = append(schedules, s)
		}
	}
	st.Schedules = schedules
	runs := []Run{}
	for _, run := range st.Runs {
		if run.ChatID != id {
			runs = append(runs, run)
		}
	}
	st.Runs = runs
	return nil
}

func (a *App) stopRun(w http.ResponseWriter, r *http.Request) {
	a.runMu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.runMu.Unlock()
	sendJSON(w, 200, map[string]bool{"stopping": true})
}

func (a *App) openBrowser(w http.ResponseWriter, r *http.Request) {
	a.runMu.Lock()
	if a.active != "" {
		a.runMu.Unlock()
		fail(w, 409, errors.New("wait for the current operation to finish"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	a.active = "browser"
	a.cancel = cancel
	a.runMu.Unlock()
	defer func() { cancel(); a.runMu.Lock(); a.active = ""; a.cancel = nil; a.runMu.Unlock() }()
	if err := a.browser.Start(ctx, a.store.Snapshot().Config); err != nil {
		fail(w, 500, err)
		return
	}
	sendJSON(w, 200, map[string]string{"message": "Browser ready. You can sign in there before running a task."})
}
func (a *App) closeBrowser(w http.ResponseWriter, r *http.Request) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.active != "" {
		fail(w, 409, errors.New("stop the task before closing the browser"))
		return
	}
	a.browser.Close()
	sendJSON(w, 200, map[string]bool{"closed": true})
}

func (a *App) saveSkill(w http.ResponseWriter, r *http.Request) {
	var s Skill
	if err := decode(w, r, &s); err != nil {
		fail(w, 400, err)
		return
	}
	s.Name = strings.TrimSpace(s.Name)
	s.Description = strings.TrimSpace(s.Description)
	s.Instructions = strings.TrimSpace(s.Instructions)
	if s.Name == "" || len(s.Name) > 120 || len(s.Description) > 600 || len(s.Instructions) < 10 || len(s.Instructions) > 12000 {
		fail(w, 400, errors.New("a skill needs a name and instructions (10 to 12000 characters)"))
		return
	}
	s.UpdatedAt = now()
	if s.ID == "" {
		s.ID = newID()
	}
	err := a.store.Update(func(st *State) error {
		for i := range st.Skills {
			if st.Skills[i].ID == s.ID {
				st.Skills[i] = s
				return nil
			}
		}
		if len(st.Skills) >= 100 {
			return errors.New("the limit of 100 skills has been reached")
		}
		st.Skills = append(st.Skills, s)
		return nil
	})
	if err != nil {
		fail(w, 400, err)
		return
	}
	sendJSON(w, 200, s)
}
func (a *App) deleteSkill(w http.ResponseWriter, r *http.Request) {
	err := a.store.Update(func(st *State) error {
		for _, schedule := range st.Schedules {
			if schedule.SkillID == r.PathValue("id") {
				return errors.New("remove schedules using this skill first")
			}
		}
		for i, s := range st.Skills {
			if s.ID == r.PathValue("id") {
				st.Skills = append(st.Skills[:i], st.Skills[i+1:]...)
				return nil
			}
		}
		return errors.New("skill not found")
	})
	if err != nil {
		fail(w, 404, err)
		return
	}
	sendJSON(w, 200, map[string]bool{"deleted": true})
}
func (a *App) draftSkill(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RunID string `json:"run_id"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	st := a.store.Snapshot()
	var run *Run
	for i := range st.Runs {
		if st.Runs[i].ID == input.RunID {
			run = &st.Runs[i]
			break
		}
	}
	if run == nil {
		fail(w, 404, errors.New("task not found"))
		return
	}
	if run.Status != "completed" {
		fail(w, 400, errors.New("create a skill from a finished task"))
		return
	}
	transcript := "Task: " + run.Task + "\nResult: " + run.Result + "\n"
	for _, e := range run.Events {
		if e.Kind == "tool" || e.Kind == "observation" || e.Kind == "error" {
			transcript += e.Kind + " " + e.Name + ": " + cut(e.Content, 1500) + "\n"
		}
	}
	msg, err := complete(r.Context(), st.Config, []Message{{Role: "system", Content: "Write a reusable procedure in English from this execution. Return plain text with Objective, When to use, Steps and Verification. Keep useful URLs; replace passwords, personal data and task-specific values with parameters. Do not save element IDs (they change with each observation). Describe how to locate controls by their labels. Do not turn failed attempts into valid steps or invent verification results."}, {Role: "user", Content: cut(transcript, 24000)}}, nil)
	if err != nil {
		fail(w, 502, err)
		return
	}
	sendJSON(w, 200, Skill{Name: cut(run.Task, 90), Description: "Procedure based on a previous task. Review before saving.", Instructions: cut(msg.Content, 12000)})
}
