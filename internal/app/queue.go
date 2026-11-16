package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Terminal entries retain submission IDs so a lost acknowledgement cannot
// enqueue, execute or apply the same message twice.
type QueuedMessage struct {
	ID          string   `json:"id"`
	RequestID   string   `json:"request_id,omitempty"`
	RequestHash string   `json:"request_hash,omitempty"`
	ChatID      string   `json:"chat_id"`
	Message     string   `json:"message"`
	SkillIDs    []string `json:"skill_ids,omitempty"`
	CreatedAt   string   `json:"created_at"`
	Status      string   `json:"status"`
	RunID       string   `json:"run_id,omitempty"`
	Note        string   `json:"note,omitempty"`
}

func pendingMessage(status string) bool {
	return status == "queued" || status == "paused" || status == "steering"
}

func (a *App) hasQueued() bool {
	for _, item := range a.store.Snapshot().Queue {
		if item.Status == "queued" && !a.deletingChats[item.ChatID] {
			return true
		}
	}
	return false
}

// Caller holds runMu, including for claims and edits below.
func (a *App) enqueueMessage(input ChatRequest) (QueuedMessage, error) {
	st := a.store.Snapshot()
	count := 0
	for _, item := range st.Queue {
		if pendingMessage(item.Status) {
			count++
		}
	}
	if count >= 20 {
		return QueuedMessage{}, errors.New("the queue is full (20 messages); remove or finish a message first")
	}
	if _, err := selectedSkills(st, input.SkillIDs); err != nil {
		return QueuedMessage{}, err
	}
	item := QueuedMessage{ID: newID(), RequestID: a.requestID, RequestHash: a.requestHash, ChatID: input.ChatID, Message: strings.TrimSpace(input.Task), SkillIDs: input.SkillIDs, CreatedAt: now(), Status: "queued"}
	if item.ChatID == "" {
		item.ChatID = newID()
	}
	err := a.store.Update(func(st *State) error {
		if input.ChatID == "" {
			st.Chats = append([]Chat{{ID: item.ChatID, Title: cut(item.Message, 70), CreatedAt: item.CreatedAt, UpdatedAt: item.CreatedAt, SkillIDs: item.SkillIDs}}, st.Chats...)
		}
		st.Queue = append(st.Queue, item)
		return nil
	})
	return item, err
}

func pauseMessages(st *State, note string) {
	for i := range st.Queue {
		if st.Queue[i].Status == "queued" || st.Queue[i].Status == "steering" {
			st.Queue[i].Status, st.Queue[i].Note = "paused", note
		}
	}
}

func (a *App) queueAction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Action          string `json:"action"`
		RunID           string `json:"run_id"`
		Message         string `json:"message"`
		ExpectedMessage string `json:"expected_message"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if err := a.available(); err != nil {
		fail(w, 503, err)
		return
	}
	st := a.store.Snapshot()
	index := -1
	for i, item := range st.Queue {
		if item.ID == r.PathValue("id") {
			index = i
			break
		}
	}
	if index < 0 {
		fail(w, 404, errors.New("queued message not found"))
		return
	}
	item := st.Queue[index]
	if a.deletingChats[item.ChatID] {
		fail(w, 409, errors.New("this conversation is being deleted"))
		return
	}
	// Repeated button submissions acknowledge the original operation.
	if input.Action == "remove" && item.Status == "dismissed" || input.Action == "steer" && item.RunID == input.RunID && (item.Status == "steering" || item.Status == "applied") || input.Action == "resume" && item.Status == "started" {
		sendJSON(w, 200, item)
		return
	}
	if !pendingMessage(item.Status) {
		fail(w, 409, errors.New("this message has already been handled; refresh the chat"))
		return
	}
	switch input.Action {
	case "steer":
		if item.Status == "steering" || input.RunID == "" || input.RunID != a.active || !a.acceptingSteer {
			fail(w, 409, errors.New("the reply is finishing or changed; the message stays in the queue"))
			return
		}
		activeChat := ""
		bytes := len(item.Message)
		for _, run := range st.Runs {
			if run.ID == a.active {
				activeChat = run.ChatID
				for _, event := range run.Events {
					if event.Kind == "user" {
						bytes += len(event.Content)
					}
				}
			}
		}
		for _, queued := range st.Queue {
			if queued.Status == "steering" && queued.RunID == a.active {
				bytes += len(queued.Message)
			}
		}
		if activeChat != item.ChatID {
			fail(w, 409, errors.New("Steer is only available in the conversation currently running"))
			return
		}
		if bytes > 24000 {
			fail(w, 400, errors.New("steering for one reply is limited to 24000 bytes; leave this message queued for the next reply"))
			return
		}
		item.Status, item.RunID, item.Note = "steering", a.active, "Waiting for the current step to finish."
	case "remove":
		item.Status, item.Note = "dismissed", ""
	case "edit":
		if item.Status == "steering" {
			fail(w, 409, errors.New("this message is being steered; wait for it to be applied"))
			return
		}
		if item.Message != input.ExpectedMessage {
			fail(w, 409, errors.New("this message changed; reopen the editor"))
			return
		}
		message := strings.TrimSpace(input.Message)
		if message == "" || len(message) > 12000 || strings.HasPrefix(message, "/") {
			fail(w, 400, errors.New("enter a message of 1–12000 bytes; send commands directly from the composer"))
			return
		}
		item.Message = message
	case "resume":
		if item.Status == "steering" {
			fail(w, 409, errors.New("this message is already being steered"))
			return
		}
		item.Status, item.RunID, item.Note = "queued", "", ""
	default:
		fail(w, 400, errors.New("action must be steer, remove, edit or resume"))
		return
	}
	if err := a.store.Update(func(st *State) error { st.Queue[index] = item; return nil }); err != nil {
		fail(w, 500, err)
		return
	}
	if input.Action == "resume" {
		a.queueBlocked = false
		a.launchQueued()
	}
	sendJSON(w, 200, item)
}

// Called only after tool cleanup, or by the idle scheduler. Claiming a queued
// message and creating its run share beginRun's persistence transaction.
func (a *App) launchQueued() {
	if a.active != "" || a.queueBlocked || a.available() != nil {
		return
	}
	for _, item := range a.store.Snapshot().Queue {
		if item.Status != "queued" || a.deletingChats[item.ChatID] {
			continue
		}
		_, err := a.beginRun(ChatRequest{Task: item.Message, ChatID: item.ChatID, SkillIDs: item.SkillIDs, QueueID: item.ID}, "user", nil)
		if err != nil {
			if saveErr := a.store.Update(func(st *State) error {
				for i := range st.Queue {
					if st.Queue[i].ID == item.ID && st.Queue[i].Status == "queued" {
						st.Queue[i].Status, st.Queue[i].Note = "paused", "Could not start this message. Check the model and selected skills, then resume."
					}
				}
				return nil
			}); saveErr != nil {
				a.logError(saveErr)
			}
		}
		return
	}
}

// Delivery and the visible user event are atomic. finish seals the run against
// late steering before a final model response can be accepted.
func (a *App) consumeSteering(ctx context.Context, runID string, finish bool) ([]Message, error) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st := a.store.Snapshot()
	activeChat := ""
	for _, run := range st.Runs {
		if run.ID == runID {
			activeChat = run.ChatID
			break
		}
	}
	if a.active != runID || activeChat == "" || a.deletingChats[activeChat] {
		return nil, errors.New("the reply is stopping")
	}
	items := []QueuedMessage{}
	for _, item := range st.Queue {
		if item.Status == "steering" && item.RunID == runID {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		if finish {
			a.acceptingSteer = false
		}
		return nil, nil
	}
	if err := a.store.Update(func(st *State) error {
		for i := range st.Runs {
			if st.Runs[i].ID != runID {
				continue
			}
			for j := range st.Queue {
				item := &st.Queue[j]
				if item.Status == "steering" && item.RunID == runID {
					st.Runs[i].Events = append(st.Runs[i].Events, Event{now(), "user", "Steered", item.Message})
					item.Status, item.Note = "applied", ""
				}
			}
			return nil
		}
		return errors.New("reply not found")
	}); err != nil {
		return nil, err
	}
	messages := make([]Message, 0, len(items))
	for _, item := range items {
		messages = append(messages, Message{Role: "user", Content: item.Message})
	}
	return messages, nil
}
