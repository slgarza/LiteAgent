package app

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (a *App) createSchedule(s Schedule) (Schedule, error) {
	if len(s.RequestID) > 128 {
		return s, errors.New("request_id is too long")
	}
	// Hash the submitted definition before generated IDs/defaults are applied.
	definition := s
	definition.RequestID = ""
	definition.RequestHash = ""
	raw, _ := json.Marshal(definition)
	s.RequestHash = fmt.Sprintf("%x", sha256.Sum256(raw))
	duplicate := func(st State) (Schedule, bool, error) {
		if s.RequestID == "" {
			return Schedule{}, false, nil
		}
		for _, old := range st.Schedules {
			if old.RequestID == s.RequestID {
				if old.RequestHash != s.RequestHash {
					return Schedule{}, false, errors.New("request_id already used for another schedule")
				}
				return old, true, nil
			}
		}
		return Schedule{}, false, nil
	}
	if old, found, err := duplicate(a.store.Snapshot()); found || err != nil {
		return old, err
	}
	s.ID = newID()
	s.Title = strings.TrimSpace(s.Title)
	s.Task = strings.TrimSpace(s.Task)
	if s.Title == "" || len(s.Title) > 1000 || len(s.Task) > 12000 {
		return s, errors.New("provide a title up to 1000 characters and a task up to 12000 characters")
	}
	if s.Kind != "reminder" && s.Kind != "skill" && s.Kind != "task" {
		return s, errors.New("kind must be reminder, skill or task")
	}
	if s.Kind == "task" && s.Task == "" {
		return s, errors.New("a scheduled task needs instructions")
	}
	if s.Kind != "skill" {
		s.SkillID = ""
	}
	if s.Timezone == "" {
		s.Timezone = a.store.Snapshot().Config.Timezone
	}
	if s.Repeat != "cron" {
		s.Cron = ""
	}
	if s.Repeat != "once" {
		s.Date = ""
	}
	next, err := nextOccurrence(s, time.Now())
	if err != nil {
		return s, err
	}
	s.NextRun = next.Format(time.RFC3339)
	err = a.store.Update(func(st *State) error {
		if old, found, err := duplicate(*st); found || err != nil {
			if found {
				s = old
			}
			return err
		}
		if len(st.Schedules) >= 100 {
			return errors.New("schedule limit reached")
		}
		if s.ChatID == "" {
			s.ChatID = newID()
			st.Chats = append([]Chat{{ID: s.ChatID, Title: s.Title, CreatedAt: now(), UpdatedAt: now(), SkillIDs: []string{}}}, st.Chats...)
		}
		found := false
		for _, c := range st.Chats {
			if c.ID == s.ChatID {
				found = true
				break
			}
		}
		if !found {
			return errors.New("conversation not found")
		}
		if s.Kind == "skill" {
			found = false
			for _, skill := range st.Skills {
				if skill.ID == s.SkillID && !skill.Disabled {
					found = true
					break
				}
			}
			if !found {
				return errors.New("install or identify the skill before scheduling it")
			}
		}
		for _, e := range st.Schedules {
			if e.Enabled && e.ChatID == s.ChatID && e.Kind == s.Kind && e.Title == s.Title && e.Task == s.Task && e.Time == s.Time && e.Repeat == s.Repeat && e.Cron == s.Cron && e.Date == s.Date && e.Timezone == s.Timezone && e.SkillID == s.SkillID {
				return errors.New("this schedule already exists")
			}
		}
		st.Schedules = append(st.Schedules, s)
		return nil
	})
	return s, err
}

func (a *App) updateSchedule(id, action string) error {
	return a.store.Update(func(st *State) error {
		for i := range st.Schedules {
			s := &st.Schedules[i]
			if s.ID != id {
				continue
			}
			switch action {
			case "delete":
				if s.LastStatus == "running" {
					return errors.New("stop the scheduled execution before deleting it")
				}
				st.Schedules = append(st.Schedules[:i], st.Schedules[i+1:]...)
			case "pause":
				s.Enabled = false
			case "resume":
				if s.Kind == "skill" {
					enabled := false
					for _, skill := range st.Skills {
						if skill.ID == s.SkillID && !skill.Disabled {
							enabled = true
						}
					}
					if !enabled {
						return errors.New("enable the skill before resuming its schedule")
					}
				}
				if s.LastStatus == "running" {
					return errors.New("wait for the scheduled execution to finish")
				}
				next, err := nextOccurrence(*s, time.Now())
				if err != nil {
					return err
				}
				s.Enabled = true
				s.NextRun = next.Format(time.RFC3339)
				s.LastError = ""
			default:
				return errors.New("action must be pause, resume or delete")
			}
			return nil
		}
		return errors.New("schedule not found")
	})
}

func (a *App) recordScheduleReminder(s Schedule, claim func(*State) error) (Run, error) {
	run := Run{ID: newID(), ChatID: s.ChatID, Origin: "reminder", Task: "Scheduled reminder", Status: "completed", CreatedAt: now(), FinishedAt: now(), Result: s.Title, Events: []Event{}, SkillIDs: []string{}, ScheduleID: s.ID, ScheduledFor: s.NextRun}
	return a.recordRun(run, claim)
}
func linkSchedule(st *State, run Run) {
	for i := range st.Schedules {
		if st.Schedules[i].ID == run.ScheduleID {
			st.Schedules[i].LastRunID = run.ID
			st.Schedules[i].LastStatus = run.Status
			if run.Status != "running" {
				st.Schedules[i].LastFinishedAt = run.FinishedAt
			}
		}
	}
}

func (a *App) schedulesAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		sendJSON(w, 200, a.store.Snapshot().Schedules)
		return
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if r.Method == "POST" && r.PathValue("id") == "" {
		// Accept definition fields only; execution metadata is owned by the scheduler.
		var input struct {
			RequestID string `json:"request_id,omitempty"`
			ChatID    string `json:"chat_id"`
			Title     string `json:"title"`
			Kind      string `json:"kind"`
			Time      string `json:"time"`
			Date      string `json:"date"`
			Repeat    string `json:"repeat"`
			Cron      string `json:"cron"`
			Timezone  string `json:"timezone"`
			SkillID   string `json:"skill_id"`
			Task      string `json:"task"`
		}
		if err := decode(w, r, &input); err != nil {
			fail(w, 400, err)
			return
		}
		s, err := a.createSchedule(Schedule{RequestID: input.RequestID, ChatID: input.ChatID, Title: input.Title, Kind: input.Kind, Time: input.Time, Date: input.Date, Repeat: input.Repeat, Cron: input.Cron, Timezone: input.Timezone, SkillID: input.SkillID, Task: input.Task, Enabled: true})
		if err != nil {
			fail(w, 400, err)
			return
		}
		sendJSON(w, 201, s)
		return
	}
	var input struct {
		Action string `json:"action"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	id := r.PathValue("id")
	if input.Action == "run" {
		for _, s := range a.store.Snapshot().Schedules {
			if s.ID == id {
				// A manual run leaves the next scheduled occurrence unchanged.
				if a.active != "" {
					fail(w, 409, errors.New("wait for the current execution"))
					return
				}
				run, err := a.launchSchedule(s, time.Now(), false)
				if err != nil {
					fail(w, 400, err)
					return
				}
				sendJSON(w, 202, run)
				return
			}
		}
		fail(w, 404, errors.New("schedule not found"))
		return
	}
	if err := a.updateSchedule(id, input.Action); err != nil {
		fail(w, 400, err)
		return
	}
	sendJSON(w, 200, map[string]bool{"saved": true})
}
