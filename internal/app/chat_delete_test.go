package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func blockedChat(t *testing.T) (*App, Run, <-chan struct{}) {
	t.Helper()
	a := newTestApp(t)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-release:
		}
	}))
	t.Cleanup(func() {
		a.runMu.Lock()
		if a.cancel != nil {
			a.cancel()
		}
		a.runMu.Unlock()
		close(release)
		model.Close()
	})
	if err := a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil }); err != nil {
		t.Fatal(err)
	}
	run := postChat(t, a, "", "Keep thinking")
	waitChatSignal(t, started)
	return a, run, cancelled
}

func waitChatSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("chat operation did not finish")
	}
}

func assertChatRemoved(t *testing.T, st State, id string) {
	t.Helper()
	for _, chat := range st.Chats {
		if chat.ID == id {
			t.Fatal("deleted conversation survived")
		}
	}
	for _, run := range st.Runs {
		if run.ChatID == id {
			t.Fatal("deleted conversation retained history")
		}
	}
	for _, schedule := range st.Schedules {
		if schedule.ChatID == id {
			t.Fatal("deleted conversation retained a schedule")
		}
	}
}

func TestDeleteOtherChatWhileReplyRuns(t *testing.T) {
	a, active, cancelled := blockedChat(t)
	other := postChat(t, a, "", "/help")
	if err := a.store.Update(func(st *State) error {
		st.Schedules = append(st.Schedules, Schedule{ID: "keep", ChatID: active.ChatID}, Schedule{ID: "remove", ChatID: other.ChatID})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var state struct {
		ActiveChatID string `json:"active_chat_id"`
	}
	if err := json.Unmarshal(callAPI(t, a, "GET", "/api/state?chat_id="+other.ChatID, nil, nil).Body.Bytes(), &state); err != nil || state.ActiveChatID != active.ChatID {
		t.Fatal("filtered chat state lost the active conversation")
	}
	if w := callAPI(t, a, "DELETE", "/api/chats/"+other.ChatID, nil, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	assertChatRemoved(t, a.store.Snapshot(), other.ChatID)
	if w := callAPI(t, a, "DELETE", "/api/chats/missing", nil, nil); w.Code != 404 {
		t.Fatal("missing conversation should return 404")
	}
	select {
	case <-cancelled:
		t.Fatal("deleting another conversation cancelled the active reply")
	default:
	}
	st := a.store.Snapshot()
	if len(st.Chats) != 1 || st.Chats[0].ID != active.ChatID || len(st.Runs) != 1 || st.Runs[0].Status != "running" || len(st.Schedules) != 1 || st.Schedules[0].ID != "keep" {
		t.Fatal("deleting another chat changed the active conversation")
	}
	// A stale tab must not recreate a deleted conversation with a slash command.
	if w := callAPI(t, a, "POST", "/api/chat", ChatRequest{ChatID: other.ChatID, Message: "/help"}, nil); w.Code != 404 {
		t.Fatal("stale command recreated a deleted conversation")
	}
}

func TestDeleteActiveChatWaitsForCleanup(t *testing.T) {
	a, run, cancelled := blockedChat(t)
	a.runMu.Lock()
	a.wizards = map[string]*onboarding{run.ChatID: {}}
	a.runMu.Unlock()
	// Hold browser cleanup so cancellation and deletion are observably separate.
	a.browser.mu.Lock()
	locked := true
	defer func() {
		if locked {
			a.browser.mu.Unlock()
		}
	}()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- callAPI(t, a, "DELETE", "/api/chats/"+run.ChatID, nil, nil) }()
	waitChatSignal(t, cancelled)
	select {
	case <-response:
		t.Fatal("deletion returned before tool cleanup")
	default:
	}
	if len(a.store.Snapshot().Chats) != 1 || len(a.store.Snapshot().Runs) != 1 {
		t.Fatal("history was removed while tools were still cleaning up")
	}
	if w := callAPI(t, a, "POST", "/api/chat", ChatRequest{ChatID: run.ChatID, Message: "/help"}, nil); w.Code != 409 {
		t.Fatal("pending deletion accepted another command")
	}
	if w := callAPI(t, a, "DELETE", "/api/chats/"+run.ChatID, nil, nil); w.Code != 409 {
		t.Fatal("duplicate deletion was not rejected")
	}
	// A tool that finishes saving a reminder during cancellation must be covered.
	if err := a.store.Update(func(st *State) error {
		st.Schedules = append(st.Schedules, Schedule{ID: "late", ChatID: run.ChatID})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.browser.mu.Unlock()
	locked = false
	select {
	case w := <-response:
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deletion did not finish after cleanup")
	}
	a.wg.Wait()
	assertChatRemoved(t, a.store.Snapshot(), run.ChatID)
	a.runMu.Lock()
	clean := a.active == "" && a.runDone == nil && a.wizards[run.ChatID] == nil && !a.deletingChats[run.ChatID]
	a.runMu.Unlock()
	if !clean {
		t.Fatal("deletion left runtime state behind")
	}
	reopened, err := openStore(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	assertChatRemoved(t, reopened.Snapshot(), run.ChatID)
}

func TestInterruptedChatDeletionKeepsHistory(t *testing.T) {
	a, run, cancelled := blockedChat(t)
	a.browser.mu.Lock()
	locked := true
	defer func() {
		if locked {
			a.browser.mu.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("DELETE", "http://127.0.0.1:8787/api/chats/"+run.ChatID, nil).WithContext(ctx)
	r.Header.Set("X-LiteAgent", "1")
	w := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() { a.Handler().ServeHTTP(w, r); close(finished) }()
	waitChatSignal(t, cancelled)
	cancel()
	waitChatSignal(t, finished)
	if w.Code != 408 || len(a.store.Snapshot().Chats) != 1 {
		t.Fatal("interrupted deletion did not keep the conversation")
	}
	a.browser.mu.Unlock()
	locked = false
	a.wg.Wait()
	if w := callAPI(t, a, "DELETE", "/api/chats/"+run.ChatID, nil, nil); w.Code != 200 {
		t.Fatal("could not retry interrupted deletion", w.Body.String())
	}
	assertChatRemoved(t, a.store.Snapshot(), run.ChatID)
}

func TestPendingChatDeletionSkipsSchedules(t *testing.T) {
	a := newTestApp(t)
	run := postChat(t, a, "", "/help")
	schedule := Schedule{ID: "pending", ChatID: run.ChatID, Title: "Reminder", Kind: "reminder", Repeat: "once", Enabled: true, NextRun: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
	if err := a.store.Update(func(st *State) error { st.Schedules = []Schedule{schedule}; return nil }); err != nil {
		t.Fatal(err)
	}
	a.runMu.Lock()
	a.deletingChats = map[string]bool{run.ChatID: true}
	a.runMu.Unlock()
	a.processDue(time.Now())
	if st := a.store.Snapshot(); len(st.Runs) != 1 || !st.Schedules[0].Enabled || st.Schedules[0].LastRun != "" {
		t.Fatal("scheduler consumed a reminder during deletion")
	}
	if w := callAPI(t, a, "POST", "/api/schedules/pending", map[string]string{"action": "run"}, nil); w.Code < 400 {
		t.Fatal("manual reminder ran during deletion")
	}
}
