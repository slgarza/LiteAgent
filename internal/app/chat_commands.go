package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
)

func splitCommand(text string) (string, string) {
	text = strings.TrimSpace(text)
	i := strings.IndexFunc(text, unicode.IsSpace)
	if i < 0 {
		return strings.ToLower(text), ""
	}
	return strings.ToLower(text[:i]), strings.TrimSpace(text[i:])
}

func (a *App) resetChat(chatID, message string) (Run, error) {
	run := Run{ID: newID(), ChatID: chatID, Task: message, Result: "Context reset. Saved messages, skills and schedules are preserved.", Origin: "command", Status: "completed", CreatedAt: now(), FinishedAt: now(), Events: []Event{}, SkillIDs: []string{}}
	return a.recordRun(run, func(st *State) error {
		for i := range st.Chats {
			if st.Chats[i].ID == chatID {
				st.Chats[i].ContextResetID = run.ID
				return nil
			}
		}
		// A fresh chat has no earlier context to reset.
		return nil
	})
}

func (a *App) cronCommand(chatID, visible, argument string) (Run, error) {
	action, rest := splitCommand(argument)
	reply := ""
	switch action {
	case "list":
		reply = scheduleSummary(a.store.Snapshot().Schedules)
	case "add":
		var s Schedule
		d := json.NewDecoder(strings.NewReader(rest))
		d.DisallowUnknownFields()
		if err := d.Decode(&s); err != nil {
			reply = "Use /cron add with a JSON definition: " + err.Error()
			break
		}
		var extra any
		if err := d.Decode(&extra); err != io.EOF {
			reply = "Use one JSON definition only."
			break
		}
		exists := false
		for _, chat := range a.store.Snapshot().Chats {
			if chat.ID == chatID {
				exists = true
				break
			}
		}
		targetChat := chatID
		if !exists {
			targetChat = ""
		}
		// Do not trust caller supplied execution metadata.
		created, err := a.createSchedule(Schedule{RequestID: a.requestID, ChatID: targetChat, Title: s.Title, Kind: s.Kind, Repeat: s.Repeat, Time: s.Time, Date: s.Date, Cron: s.Cron, Timezone: s.Timezone, SkillID: s.SkillID, Task: s.Task, Enabled: true})
		if err != nil {
			reply = err.Error()
		} else {
			reply = scheduleSummary([]Schedule{created})
			chatID = created.ChatID
		}
	case "pause", "resume", "delete":
		if err := a.updateSchedule(rest, action); err != nil {
			reply = err.Error()
		} else {
			reply = "Schedule updated."
		}
	case "run":
		if a.active != "" {
			reply = "Wait for the current execution or use /stop."
			break
		}
		for _, s := range a.store.Snapshot().Schedules {
			if s.ID == rest {
				return a.launchSchedule(s, time.Now(), false)
			}
		}
		reply = "Schedule not found."
	case "history":
		reply = "**Recent scheduled executions**\n\n"
		count := 0
		for _, r := range a.store.Snapshot().Runs {
			if r.ScheduleID == "" || rest != "" && r.ScheduleID != rest {
				continue
			}
			reply += fmt.Sprintf("- `%s` · %s · %s · run `%s`\n", r.ScheduleID, r.CreatedAt, r.Status, r.ID)
			count++
			if count == 20 {
				break
			}
		}
		if count == 0 {
			reply += "No executions yet."
		}
	case "next":
		fields := strings.Fields(rest)
		zone := a.store.Snapshot().Config.Timezone
		if len(fields) == 6 {
			zone = fields[5]
			fields = fields[:5]
		}
		schedule := Schedule{Repeat: "cron", Cron: strings.Join(fields, " "), Timezone: zone}
		current := time.Now()
		reply = "**Next occurrences** (" + zone + ")\n\n"
		for i := 0; i < 5; i++ {
			next, err := nextOccurrence(schedule, current)
			if err != nil {
				reply = err.Error()
				break
			}
			loc, _ := time.LoadLocation(zone)
			reply += "- " + next.In(loc).Format("2006-01-02 Mon 15:04 MST -07:00") + "\n"
			current = next
		}
	default:
		reply = "**Cron commands**\n\n- `/cron list` (or `/crons`)\n- `/cron next 0 14 * * 1-5 America/Mexico_City`\n- `/cron add {\"title\":\"Daily check\",\"kind\":\"task\",\"task\":\"Check example.com and report the heading\",\"repeat\":\"cron\",\"cron\":\"0 14 * * 1-5\"}`\n- `/cron pause ID`, `/cron resume ID`, `/cron delete ID`\n- `/cron run ID` — run now; the next occurrence stays unchanged\n- `/cron history ID`\n\nKinds: reminder, task, skill. For skill, include skill_id. Repeat: cron, daily (time), once (time + date). Runs use saved model settings and require LiteAgent to be running. Failed executions pause the job."
	}
	return a.recordReply(chatID, visible, reply, "command", nil)
}

func chatContext(st State, chatID string) []Message {
	resetID := ""
	for _, c := range st.Chats {
		if c.ID == chatID {
			resetID = c.ContextResetID
			break
		}
	}
	return boundedConversationContext(st.Runs, chatID, resetID, st.Config.historyTurns())
}

// Fail before doing work if the service is shutting down.
func (a *App) available() error {
	if a.root.Err() != nil {
		return errors.New("LiteAgent is shutting down")
	}
	return nil
}
