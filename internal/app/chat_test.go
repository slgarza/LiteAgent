package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func postChat(t *testing.T, a *App, chatID, message string) Run {
	t.Helper()
	w := callAPI(t, a, "POST", "/api/chat", ChatRequest{ChatID: chatID, Message: message}, nil)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var run Run
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestChatCommandsAndSecretRedaction(t *testing.T) {
	a := newTestApp(t)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer secret-for-test" {
			t.Error("incorrect model list request")
		}
		fmt.Fprint(w, `{"data":[{"id":"test-model"}]}`)
	}))
	defer model.Close()
	run := postChat(t, a, "", "/onboard")
	for _, message := range []string{"openai", model.URL, "test-model", "secret-for-test"} {
		run = postChat(t, a, run.ChatID, message)
	}
	if !strings.Contains(run.Result, "Settings saved.") || a.store.Snapshot().Config.APIKey != "secret-for-test" {
		t.Fatal("onboarding did not save settings")
	}
	if r := postChat(t, a, run.ChatID, "/models"); !strings.Contains(r.Result, "test-model") {
		t.Fatal(r.Result)
	}
	for _, message := range []string{"/config key replacement-secret", "/config\tkey\tmalformed-secret"} {
		postChat(t, a, run.ChatID, message)
	}
	public := callAPI(t, a, "GET", "/api/state", nil, nil).Body.String()
	for _, secret := range []string{"secret-for-test", "replacement-secret", "malformed-secret"} {
		if strings.Contains(public, secret) {
			t.Fatal("credential exposed in public state")
		}
	}
	if r := postChat(t, a, run.ChatID, "/status"); !strings.Contains(r.Result, "idle") {
		t.Fatal(r.Result)
	}
	if r := postChat(t, a, run.ChatID, "/skills"); !strings.Contains(r.Result, "No skills") {
		t.Fatal(r.Result)
	}
	if r := postChat(t, a, run.ChatID, "/schedules"); !strings.Contains(r.Result, "No reminders") {
		t.Fatal(r.Result)
	}
	postChat(t, a, run.ChatID, "/onboard")
	postChat(t, a, run.ChatID, "anthropic")
	postChat(t, a, run.ChatID, "/cancel")
	if a.store.Snapshot().Config.Provider != "openai" {
		t.Fatal("cancelled wizard changed saved provider")
	}
	other := postChat(t, a, run.ChatID, "/new")
	if other.ChatID == run.ChatID {
		t.Fatal("new conversation reused its ID")
	}
	if resumed := postChat(t, a, other.ChatID, "/resume "+run.ChatID); resumed.ChatID != run.ChatID {
		t.Fatal("resume did not switch conversations")
	}
}

func TestConversationPersistsAndIsolatesContext(t *testing.T) {
	a := newTestApp(t)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		raw, _ := json.Marshal(payload.Messages)
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, `{"message":{"role":"assistant","content":"Your project is Indigo."}}`)
		case 2:
			if !strings.Contains(string(raw), "project is Indigo") || !strings.Contains(string(raw), "What is its name?") {
				t.Error("follow-up lost persisted history")
			}
			fmt.Fprint(w, `{"message":{"role":"assistant","content":"Indigo."}}`)
		case 3:
			if strings.Contains(string(raw), "Indigo") {
				t.Error("conversation leaked into a new chat")
			}
			fmt.Fprint(w, `{"message":{"role":"assistant","content":"A fresh conversation."}}`)
		}
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	first := postChat(t, a, "", "My project is Indigo.")
	waitRun(t, a, first.ID)
	a.wg.Wait()
	reopened, err := openStore(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	second := postChat(t, a, first.ChatID, "What is its name?")
	if r := waitRun(t, a, second.ID); r.Result != "Indigo." {
		t.Fatal(r.Result)
	}
	a.wg.Wait()
	third := postChat(t, a, "", "Start fresh.")
	waitRun(t, a, third.ID)
}

func TestNextOccurrenceAndRestartDelivery(t *testing.T) {
	a := newTestApp(t)
	current := time.Date(2030, 6, 1, 19, 0, 0, 0, time.UTC)
	s := Schedule{ID: "daily", Kind: "reminder", Title: "Check the report", ChatID: "reminder-chat", Time: "14:00", Timezone: "America/Mexico_City", Repeat: "daily", Enabled: true}
	next, err := nextOccurrence(s, current)
	if err != nil || !next.Equal(time.Date(2030, 6, 1, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("wrong local occurrence: %v %v", next, err)
	}
	s.NextRun = next.Format(time.RFC3339)
	single := s
	single.ID = "once"
	single.Repeat = "once"
	single.Date = "2030-06-01"
	_ = a.store.Update(func(st *State) error { st.Schedules = []Schedule{s, single}; return nil })
	a.processDue(current.Add(30 * time.Minute))
	if len(a.store.Snapshot().Runs) != 0 {
		t.Fatal("reminder delivered early")
	}
	a.processDue(current.Add(2 * time.Hour))
	st := a.store.Snapshot()
	if len(st.Runs) != 2 || st.Schedules[1].Enabled {
		t.Fatal("due reminders were not committed correctly")
	}
	reopened, err := openStore(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	a.processDue(current.Add(3 * time.Hour))
	if len(a.store.Snapshot().Runs) != 2 {
		t.Fatal("restart duplicated a delivered reminder")
	}
	// After several missed days, deliver one occurrence and advance beyond now.
	a.processDue(current.Add(5 * 24 * time.Hour))
	st = a.store.Snapshot()
	if len(st.Runs) != 3 {
		t.Fatal("missed daily schedule was replayed incorrectly")
	}
	next, _ = time.Parse(time.RFC3339, st.Schedules[0].NextRun)
	if !next.After(current.Add(5 * 24 * time.Hour)) {
		t.Fatal("next reminder is still overdue")
	}
	if _, err = nextOccurrence(single, current.Add(2*time.Hour)); err == nil {
		t.Fatal("one-time schedule accepted a past date")
	}
}

func TestScheduledSkillWaitsForBusyAgent(t *testing.T) {
	a := newTestApp(t)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if !strings.Contains(payload.Messages[len(payload.Messages)-1].Content, "Verify the ticket total") {
			t.Error("scheduled run lost its skill")
		}
		calls.Add(1)
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"Scheduled skill finished."}}`)
	}))
	defer model.Close()
	chat := postChat(t, a, "", "/status")
	current := time.Now()
	_ = a.store.Update(func(st *State) error {
		st.Config.BaseURL = model.URL
		st.Skills = []Skill{{ID: "ticket", Name: "superticket", Instructions: "Verify the ticket total"}}
		st.Schedules = []Schedule{{ID: "schedule", ChatID: chat.ChatID, Kind: "skill", Title: "superticket", SkillID: "ticket", Time: "14:00", Timezone: "America/Mexico_City", Repeat: "daily", Enabled: true, NextRun: current.Add(-time.Minute).UTC().Format(time.RFC3339)}}
		return nil
	})
	a.runMu.Lock()
	a.active = "busy"
	a.runMu.Unlock()
	a.processDue(current)
	if a.store.Snapshot().Schedules[0].LastRun != "" || calls.Load() != 0 {
		t.Fatal("busy schedule was consumed")
	}
	a.runMu.Lock()
	a.active = ""
	a.runMu.Unlock()
	a.processDue(current)
	run := a.store.Snapshot().Runs[0]
	if run.Origin != "scheduled" || run.ChatID != chat.ChatID {
		t.Fatal("scheduled execution lost chat association")
	}
	waitRun(t, a, run.ID)
	a.wg.Wait()
	a.processDue(current.Add(time.Second))
	if calls.Load() != 1 {
		t.Fatal("scheduled skill executed twice")
	}
}

func TestSkillAndScheduleTools(t *testing.T) {
	a := newTestApp(t)
	chat := postChat(t, a, "", "/status")
	apply := func(name string, args map[string]any) (string, error) {
		result, handled, err := a.applicationTool(context.Background(), chat, name, args)
		if !handled {
			t.Fatal("tool not handled")
		}
		return result, err
	}
	skillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# Superticket\nRead the ticket count and verify the total.")
	}))
	defer skillServer.Close()
	result, err := apply("skills_install", map[string]any{"name": "superticket", "description": "Review ticket totals", "url": skillServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	var skill Skill
	_ = json.Unmarshal([]byte(result), &skill)
	_, err = apply("schedules_create", map[string]any{"title": "Review tickets", "kind": "skill", "skill_id": skill.ID, "time": "14:00", "repeat": "daily"})
	if err != nil {
		t.Fatal(err)
	}
	id := a.store.Snapshot().Schedules[0].ID
	if _, err = apply("skills_remove", map[string]any{"id": skill.ID}); err == nil {
		t.Fatal("removed a skill needed by a schedule")
	}
	for _, action := range []string{"pause", "resume"} {
		if _, err = apply("schedules_update", map[string]any{"id": id, "action": action}); err != nil {
			t.Fatal(err)
		}
		if a.store.Snapshot().Schedules[0].Enabled != (action == "resume") {
			t.Fatal("schedule state unchanged")
		}
	}
	if w := callAPI(t, a, "DELETE", "/api/chats/"+chat.ChatID, nil, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(a.store.Snapshot().Schedules) != 0 {
		t.Fatal("deleted conversation kept orphan schedules")
	}
	if _, err = apply("skills_remove", map[string]any{"id": skill.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestCLIUsesSharedChatAndCommands(t *testing.T) {
	a := newTestApp(t)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	var output bytes.Buffer
	input := strings.NewReader("/onboard\nollama\ndefault\nqwen3:8b\n/status\n/skills\n/exit\n")
	if err := chatCLI(context.Background(), server.URL, "", input, &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Settings saved.", "Activity: idle", "No skills installed"} {
		if !strings.Contains(output.String(), want) {
			t.Fatal("CLI did not show " + want)
		}
	}
	st := a.store.Snapshot()
	if len(st.Chats) != 1 || len(st.Runs) != 6 {
		t.Fatal("CLI did not retain one conversation")
	}
	if strings.Contains(terminalText("text\x1b[31m\x9dtitle"), "\x1b") {
		t.Fatal("terminal escape was not removed")
	}
}

func TestOnlyOneServerCanOwnStorage(t *testing.T) {
	dir := t.TempDir()
	first, err := startLocalService(context.Background(), "127.0.0.1:0", dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := startLocalService(context.Background(), "127.0.0.1:0", dir)
	if err == nil {
		second.Close()
		first.Close()
		t.Fatal("two servers acquired the same storage")
	}
	first.Close()
	third, err := startLocalService(context.Background(), "127.0.0.1:0", dir)
	if err != nil {
		t.Fatal("could not reopen released storage:", err)
	}
	third.Close()
}
