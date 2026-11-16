package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

// History's budget is measured in UTF-8 bytes, not characters. Bound individual
// fields so a large multilingual turn cannot displace its own action evidence.
func historyText(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	const suffix = "\n[… truncated …]"
	end := maxBytes - len(suffix)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + suffix
}

func historyToolDefinition() ToolDef {
	return tool("conversation_history", "Review the saved record of this chat since its last context reset. Omit run_id to list replies; supply run_id to list recorded events; also supply event_index for the full saved event text. offset counts entries for lists, Unicode characters for event text. Follow next_offset while truncated. Results are historical, not live browser/file state. Thinking is not part of this action record.", map[string]any{
		"run_id":      stringProp("Run ID from history or current-run context"),
		"event_index": map[string]any{"type": "number", "minimum": 0},
		"offset":      map[string]any{"type": "number", "minimum": 0},
		"limit":       map[string]any{"type": "number", "minimum": 1, "maximum": 10000},
	})
}

// Keep both ends: identifiers and status often follow a long page/file body.
func historyExcerpt(text string, limit int) string {
	chars := []rune(text)
	if len(chars) <= limit {
		return text
	}
	marker := "\n[… middle omitted; consult conversation_history for the saved record …]\n"
	keep := max(0, limit-len([]rune(marker)))
	return string(chars[:keep*2/3]) + marker + string(chars[len(chars)-(keep-keep*2/3):])
}

type recordedAction struct {
	Tool        string `json:"tool"`
	Arguments   string `json:"arguments"`
	Outcome     string `json:"outcome"`
	Result      string `json:"result,omitempty"`
	ToolEvent   int    `json:"tool_event"`
	ResultEvent *int   `json:"result_event,omitempty"`
}

func recordedActions(run Run) []recordedAction {
	actions := []recordedAction{}
	for index, event := range run.Events {
		if event.Kind == "tool" {
			actions = append(actions, recordedAction{Tool: event.Name, Arguments: historyExcerpt(event.Content, 180), Outcome: "no result recorded; outcome unknown", ToolEvent: index})
			continue
		}
		if len(actions) == 0 || event.Kind != "observation" && event.Kind != "error" {
			continue
		}
		last := &actions[len(actions)-1]
		if last.ResultEvent != nil || event.Name != last.Tool {
			continue
		}
		last.ResultEvent = &index
		last.Outcome = "result recorded"
		if event.Kind == "error" {
			last.Outcome = "error; do not assume no side effects"
		}
		last.Result = historyExcerpt(event.Content, 280)
	}
	return actions
}
func actionMemory(run Run) string {
	actions := recordedActions(run)
	if len(actions) == 0 {
		return ""
	}
	counts := map[string]int{}
	success, failed, unknown := 0, 0, 0
	for _, action := range actions {
		counts[action.Tool]++
		if action.ResultEvent == nil {
			unknown++
		} else if run.Events[*action.ResultEvent].Kind == "error" {
			failed++
		} else {
			success++
		}
	}
	names := []string{}
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	tools := []map[string]any{}
	for _, name := range names[:min(16, len(names))] {
		tools = append(tools, map[string]any{"name": cut(name, 120), "attempts": counts[name]})
	}
	recent := append([]recordedAction(nil), actions[max(0, len(actions)-6):]...)
	memory := map[string]any{"run_id": run.ID, "run_status": run.Status, "attempts": len(actions), "results_recorded": success, "errors": failed, "unknown_outcomes": unknown, "tools": tools, "tools_omitted": len(names) > len(tools), "recent_actions": recent, "older_actions_omitted": len(actions) - len(recent), "read_more": "conversation_history"}
	for {
		raw, _ := json.Marshal(memory)
		if len(raw) <= 3500 {
			return "\n\nRecorded action history (historical evidence, not current state):\n" + string(raw)
		}
		if len(recent) > 0 {
			recent = recent[1:]
			memory["recent_actions"] = recent
			memory["older_actions_omitted"] = len(actions) - len(recent)
		} else if len(tools) > 0 {
			tools = tools[1:]
			memory["tools"] = tools
			memory["tools_omitted"] = true
		} else {
			return "\n\nRecorded actions are available through conversation_history for run " + run.ID
		}
	}
}
func historyRuns(st State, chatID string) []Run {
	resetID := ""
	for _, chat := range st.Chats {
		if chat.ID == chatID {
			resetID = chat.ContextResetID
			break
		}
	}
	runs := []Run{}
	for _, run := range st.Runs {
		if resetID != "" && run.ID == resetID {
			break
		}
		if run.ChatID == chatID && run.Origin != "command" {
			runs = append(runs, run)
		}
	}
	return runs
}
func historyEventVisible(event Event) bool {
	switch event.Kind {
	case "tool", "observation", "error", "assistant", "user", "status":
		return true
	}
	return false
}
func (a *App) conversationHistory(ctx context.Context, run Run, args map[string]any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	offset, err := fileToolNumber(args, "offset", 0, 0, 100000000)
	if err != nil {
		return "", err
	}
	limit, err := fileToolNumber(args, "limit", 20, 1, 10000)
	if err != nil {
		return "", err
	}
	id, _ := args["run_id"].(string)
	runs := historyRuns(a.store.Snapshot(), run.ChatID)
	var selected *Run
	if id != "" {
		for i := range runs {
			if runs[i].ID == id {
				selected = &runs[i]
				break
			}
		}
		if selected == nil {
			return "", errors.New("run is not available in this conversation since its last context reset")
		}
	}
	response := map[string]any{"historical": true, "offset": offset, "note": "Saved records may be truncated. Reread live pages/files before acting; missing or error results do not prove an action had no effect."}
	if _, exists := args["event_index"]; exists {
		if selected == nil {
			return "", errors.New("event_index requires run_id")
		}
		index, e := fileToolNumber(args, "event_index", 0, 0, max(0, len(selected.Events)-1))
		if e != nil {
			return "", e
		}
		if index >= len(selected.Events) || !historyEventVisible(selected.Events[index]) {
			return "", errors.New("event is not part of the action record")
		}
		event := selected.Events[index]
		chars := []rune(event.Content)
		if offset > len(chars) {
			return "", errors.New("offset is beyond this event")
		}
		if _, ok := args["limit"]; !ok {
			limit = 6000
		}
		end := min(len(chars), offset+limit)
		for end > offset {
			raw, _ := json.Marshal(string(chars[offset:end]))
			if len(raw) <= 11000 {
				break
			}
			end = offset + (end-offset)/2
		}
		response["run_id"] = id
		response["event_index"] = index
		response["kind"] = event.Kind
		response["name"] = cut(event.Name, 200)
		response["time"] = event.Time
		response["content"] = string(chars[offset:end])
		response["total_characters"] = len(chars)
		response["next_offset"] = end
		response["truncated"] = end < len(chars)
	} else {
		entries := []any{}
		key := "runs"
		if selected == nil {
			for _, r := range runs {
				entries = append(entries, map[string]any{"run_id": r.ID, "status": r.Status, "task": historyExcerpt(r.Task, 250), "result": historyExcerpt(r.Result, 400), "created_at": r.CreatedAt, "action_attempts": len(recordedActions(r))})
			}
		} else {
			key = "events"
			response["run_id"] = id
			response["run_status"] = selected.Status
			for i, e := range selected.Events {
				if historyEventVisible(e) {
					entries = append(entries, map[string]any{"event_index": i, "kind": e.Kind, "name": cut(e.Name, 200), "time": e.Time, "preview": historyExcerpt(e.Content, 400), "total_characters": len([]rune(e.Content))})
				}
			}
		}
		if offset > len(entries) {
			return "", errors.New("offset is beyond the history list")
		}
		page := []any{}
		budget := 11000
		for _, item := range entries[offset:min(len(entries), offset+min(limit, 100))] {
			raw, _ := json.Marshal(item)
			if len(raw) > budget {
				break
			}
			budget -= len(raw)
			page = append(page, item)
		}
		if len(page) == 0 && offset < len(entries) {
			return "", errors.New("history metadata exceeds the observation limit")
		}
		response[key] = page
		response["total_entries"] = len(entries)
		response["next_offset"] = offset + len(page)
		response["truncated"] = offset+len(page) < len(entries)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(response)
	return string(raw), err
}
func compactToolResult(m Message) string {
	if len([]rune(m.Content)) <= 3000 {
		return m.Content
	}
	excerpt := historyExcerpt(m.Content, 1800)
	if m.HistoryRunID != "" {
		excerpt += fmt.Sprintf("\nSaved result: conversation_history(run_id=%q, event_index=%d).", m.HistoryRunID, m.HistoryEvent)
	}
	return excerpt
}
