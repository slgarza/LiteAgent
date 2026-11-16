package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func queueMessage(t *testing.T, a *App, chatID, message, requestID string) QueuedMessage {
	t.Helper()
	w := callAPI(t, a, "POST", "/api/chat", ChatRequest{ChatID: chatID, Message: message, RequestID: requestID, QueueIfBusy: true}, nil)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var item QueuedMessage
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil || item.Status != "queued" {
		t.Fatalf("expected queued message: %s", w.Body.String())
	}
	return item
}

func queueCommand(t *testing.T, a *App, item QueuedMessage, action, runID string) *httptest.ResponseRecorder {
	t.Helper()
	return callAPI(t, a, "POST", "/api/queue/"+item.ID, map[string]string{"action": action, "run_id": runID}, nil)
}

func TestQueueFIFOAndSubmissionDeduplication(t *testing.T) {
	a := newTestApp(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		switch calls.Add(1) {
		case 1:
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		case 2:
			if !strings.Contains(request.Messages[len(request.Messages)-1].Content, "Second") {
				t.Error("second queued message ran out of order")
			}
		case 3:
			if !strings.Contains(request.Messages[len(request.Messages)-1].Content, "Third") {
				t.Error("third queued message ran out of order")
			}
			if len(request.Messages) < 6 {
				t.Error("queue lost conversation context")
			}
		default:
			t.Error("duplicate execution")
		}
		fmt.Fprint(w, `{"message":{"content":"Done."}}`)
	}))
	defer model.Close()
	defer close(release)
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	first := postChat(t, a, "", "First")
	<-started
	second := queueMessage(t, a, first.ChatID, "Second", "second-request")
	third := queueMessage(t, a, first.ChatID, "Third", "third-request")
	duplicate := queueMessage(t, a, first.ChatID, "Second", "second-request")
	if duplicate.ID != second.ID || calls.Load() != 1 || len(a.store.Snapshot().Queue) != 2 {
		t.Fatal("queue repeated or started an active submission")
	}
	if w := callAPI(t, a, "POST", "/api/chat", ChatRequest{ChatID: first.ChatID, Message: "Different", RequestID: "second-request", QueueIfBusy: true}, nil); w.Code != 409 {
		t.Fatal("request ID accepted different content")
	}
	if len(stateAlerts(a.store.Snapshot())) != 0 {
		t.Fatal("queue generated reply notifications")
	}
	release <- struct{}{}
	a.wg.Wait()
	st := a.store.Snapshot()
	if calls.Load() != 3 || len(st.Runs) != 3 || st.Queue[0].Status != "started" || st.Queue[1].Status != "started" {
		t.Fatal("queue did not drain", calls.Load())
	}
	for _, item := range []QueuedMessage{second, third} {
		w := callAPI(t, a, "POST", "/api/chat", ChatRequest{ChatID: item.ChatID, Message: item.Message, RequestID: item.RequestID, QueueIfBusy: true}, nil)
		var run Run
		_ = json.Unmarshal(w.Body.Bytes(), &run)
		if w.Code != 202 || run.Status != "completed" {
			t.Fatal("retry did not return the existing run")
		}
	}
	if calls.Load() != 3 {
		t.Fatal("completed submission was replayed")
	}
	reopened, err := openStore(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	a.processDue(time.Now())
	if calls.Load() != 3 {
		t.Fatal("restart replayed a claimed queued message")
	}
}

func TestQueueLimitsIsolationAndPendingDeletion(t *testing.T) {
	a, run, cancelled := blockedChat(t)
	for i := 0; i < 20; i++ {
		queueMessage(t, a, run.ChatID, "Pending", fmt.Sprintf("limit-%d", i))
	}
	w := callAPI(t, a, "POST", "/api/chat", ChatRequest{Message: "Overflow", QueueIfBusy: true}, nil)
	if w.Code != 400 || len(a.store.Snapshot().Chats) != 1 {
		t.Fatal("overflow created a message or a conversation")
	}
	item := a.store.Snapshot().Queue[0]
	if w = queueCommand(t, a, item, "remove", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	other := queueMessage(t, a, "", "Other conversation", "isolated-queue")
	var public struct {
		Queue []QueuedMessage `json:"queue"`
	}
	w = callAPI(t, a, "GET", "/api/state?chat_id="+other.ChatID, nil, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &public)
	if len(public.Queue) != 1 || public.Queue[0].ID != other.ID {
		t.Fatal("queue leaked across conversations")
	}
	a.runMu.Lock()
	a.deletingChats = map[string]bool{other.ChatID: true}
	a.runMu.Unlock()
	if w = queueCommand(t, a, other, "remove", ""); w.Code != 409 {
		t.Fatal("queue mutation accepted pending deletion")
	}
	w = callAPI(t, a, "POST", "/api/chat", ChatRequest{ChatID: other.ChatID, Message: "More", QueueIfBusy: true}, nil)
	if w.Code != 409 {
		t.Fatal("enqueue accepted pending deletion")
	}
	callAPI(t, a, "POST", "/api/stop", nil, nil)
	<-cancelled
	a.wg.Wait()
}

func TestSteerReplansBeforeExecutingStaleModelTools(t *testing.T) {
	a := newTestApp(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, `{"message":{"tool_calls":[{"function":{"name":"skills_save","arguments":{"name":"Stale","description":"Old plan","instructions":"This must never be saved."}}}]}}`)
			return
		}
		var request struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "user" || last.Content != "Only explain; do not save anything." {
			t.Error("steering was not a user instruction")
		}
		fmt.Fprint(w, `{"message":{"content":"Explained without saving."}}`)
	}))
	defer model.Close()
	defer close(release)
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	run := postChat(t, a, "", "Prepare a procedure")
	<-started
	item := queueMessage(t, a, run.ChatID, "Only explain; do not save anything.", "steer-request")
	for i := 0; i < 2; i++ {
		if w := queueCommand(t, a, item, "steer", run.ID); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	release <- struct{}{}
	a.wg.Wait()
	st := a.store.Snapshot()
	if calls.Load() != 2 || len(st.Skills) != 0 || len(st.Runs) != 1 || st.Runs[0].Status != "completed" || st.Queue[0].Status != "applied" {
		t.Fatal("steering executed stale tools or created another turn")
	}
	count := 0
	for _, event := range st.Runs[0].Events {
		if event.Kind == "user" {
			count++
		}
		if event.Kind == "tool" {
			t.Fatal("stale tool ran")
		}
	}
	if count != 1 {
		t.Fatal("steering was applied more than once")
	}
	context := chatContext(st, run.ChatID)
	if !strings.Contains(context[0].Content, item.Message) {
		t.Fatal("steering was lost from future history")
	}
	if w := queueCommand(t, a, item, "steer", run.ID); w.Code != 200 {
		t.Fatal("lost acknowledgement reapplied steering")
	}
}

func TestSteerWaitsForToolAndSkipsRemainingBatch(t *testing.T) {
	a := newTestApp(t)
	started, release := make(chan struct{}), make(chan struct{})
	var downloads, calls atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "Read the fixture and verify the report date.")
	}))
	defer source.Close()
	defer close(release)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"tool_calls": []ToolCall{
				{ID: "download", Type: "function", Function: FunctionCall{Name: "skills_install", Arguments: json.RawMessage(fmt.Sprintf(`{"name":"Installed once","description":"Fixture","url":%q}`, source.URL))}},
				{ID: "stale", Type: "function", Function: FunctionCall{Name: "skills_save", Arguments: json.RawMessage(`{"name":"Stale","description":"Must skip","instructions":"This must not be created."}`)}},
			}}})
			return
		}
		var request struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		messages := request.Messages
		if len(messages) < 5 || messages[len(messages)-1].Content != "Stop after the current download." || messages[len(messages)-2].Role != "tool" || !strings.Contains(messages[len(messages)-2].Content, "Not executed") || messages[len(messages)-3].Role != "tool" {
			t.Error("steering broke tool/result pairing")
		}
		fmt.Fprint(w, `{"message":{"content":"The current download finished; no further changes."}}`)
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	run := postChat(t, a, "", "Install the procedure from "+source.URL)
	<-started
	item := queueMessage(t, a, run.ChatID, "Stop after the current download.", "batch-steer")
	if w := queueCommand(t, a, item, "steer", run.ID); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if a.store.Snapshot().Queue[0].Status != "steering" {
		t.Fatal("steering did not wait for tool completion")
	}
	release <- struct{}{}
	a.wg.Wait()
	st := a.store.Snapshot()
	if len(st.Skills) != 1 || st.Skills[0].Name != "Installed once" || downloads.Load() != 1 || calls.Load() != 2 || st.Runs[0].Status != "completed" {
		t.Fatal("tool was repeated, interrupted or stale batch was executed")
	}
}

func TestQueueEditingRemovalStopAndDeletion(t *testing.T) {
	a, run, cancelled := blockedChat(t)
	item := queueMessage(t, a, run.ChatID, "Original", "edit-request")
	w := callAPI(t, a, "POST", "/api/queue/"+item.ID, map[string]string{"action": "edit", "message": "Edited", "expected_message": "Original"}, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = callAPI(t, a, "POST", "/api/queue/"+item.ID, map[string]string{"action": "edit", "message": "Stale edit", "expected_message": "Original"}, nil)
	if w.Code != 409 {
		t.Fatal("concurrent edit overwrote newer content")
	}
	other := postChat(t, a, "", "/help")
	wrongChat := queueMessage(t, a, other.ChatID, "Other chat", "other-chat")
	if w = queueCommand(t, a, wrongChat, "steer", run.ID); w.Code != 409 {
		t.Fatal("cross-chat steering accepted")
	}
	if w = queueCommand(t, a, item, "steer", "stale-run"); w.Code != 409 {
		t.Fatal("stale run accepted steering")
	}
	for i := 0; i < 2; i++ {
		if w = queueCommand(t, a, wrongChat, "remove", ""); w.Code != 200 {
			t.Fatal("remove was not idempotent")
		}
	}
	if w = callAPI(t, a, "POST", "/api/stop", nil, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	<-cancelled
	a.wg.Wait()
	if a.store.Snapshot().Queue[0].Status != "paused" {
		t.Fatal("Stop did not pause the queue")
	}
	if w = callAPI(t, a, "DELETE", "/api/chats/"+run.ChatID, nil, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, queued := range a.store.Snapshot().Queue {
		if queued.ChatID == run.ChatID {
			t.Fatal("deletion left pending messages")
		}
	}
}

func TestQueueRestartAndPersistenceRollback(t *testing.T) {
	a, run, cancelled := blockedChat(t)
	item := queueMessage(t, a, run.ChatID, "Keep pending", "durable-request")
	if w := queueCommand(t, a, item, "steer", run.ID); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	reopened, err := openStore(a.dir)
	if err != nil || reopened.Snapshot().Queue[0].Status != "paused" {
		t.Fatal("restart automatically resumed pending steering")
	}
	before := a.store.Snapshot()
	a.store.mu.Lock()
	original := a.store.path
	a.store.path = filepath.Join(t.TempDir(), "missing", "state.json")
	a.store.mu.Unlock()
	w := queueCommand(t, a, item, "remove", "")
	if w.Code != 500 || !reflect.DeepEqual(before, a.store.Snapshot()) {
		t.Fatal("failed persistence changed the queue")
	}
	a.store.mu.Lock()
	a.store.path = original
	a.store.mu.Unlock()
	callAPI(t, a, "POST", "/api/stop", nil, nil)
	<-cancelled
	a.wg.Wait()
}

func TestConcurrentQueueSubmissionIsExactlyOnce(t *testing.T) {
	a, run, cancelled := blockedChat(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); queueMessage(t, a, run.ChatID, "One message", "same-request") }()
	}
	wg.Wait()
	if len(a.store.Snapshot().Queue) != 1 {
		t.Fatal("concurrent submissions duplicated the message")
	}
	callAPI(t, a, "POST", "/api/stop", nil, nil)
	<-cancelled
	a.wg.Wait()
}

func TestQueueFailurePausesAndResumeStartsOnlyPendingMessage(t *testing.T) {
	a := newTestApp(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			http.Error(w, "fixture failure", 500)
			return
		}
		fmt.Fprint(w, `{"message":{"content":"Pending message completed."}}`)
	}))
	defer model.Close()
	defer close(release)
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	run := postChat(t, a, "", "Will fail")
	<-started
	item := queueMessage(t, a, run.ChatID, "Next message", "resume-request")
	release <- struct{}{}
	a.wg.Wait()
	if calls.Load() != 1 || a.store.Snapshot().Queue[0].Status != "paused" {
		t.Fatal("failure automatically ran pending work")
	}
	if w := queueCommand(t, a, item, "resume", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a.wg.Wait()
	if calls.Load() != 2 || a.store.Snapshot().Queue[0].Status != "started" {
		t.Fatal("resume did not start pending message")
	}
	if w := queueCommand(t, a, item, "resume", ""); w.Code != 200 {
		t.Fatal("resume retry failed")
	}
	a.processDue(time.Now())
	if calls.Load() != 2 {
		t.Fatal("resume retried an already completed action")
	}
}
