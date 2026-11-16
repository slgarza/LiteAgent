package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/robfig/cron/v3"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"
)

type Schedule struct {
	RequestID      string `json:"request_id,omitempty"`
	RequestHash    string `json:"request_hash,omitempty"`
	Cron           string `json:"cron,omitempty"`
	LastRunID      string `json:"last_run_id,omitempty"`
	LastStatus     string `json:"last_status,omitempty"`
	LastFinishedAt string `json:"last_finished_at,omitempty"`

	ID        string `json:"id"`
	ChatID    string `json:"chat_id"`
	Title     string `json:"title"`
	Kind      string `json:"kind"`
	Time      string `json:"time"`
	Date      string `json:"date,omitempty"`
	Repeat    string `json:"repeat"`
	Timezone  string `json:"timezone"`
	SkillID   string `json:"skill_id,omitempty"`
	Task      string `json:"task,omitempty"`
	Enabled   bool   `json:"enabled"`
	NextRun   string `json:"next_run"`
	LastRun   string `json:"last_run,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

func nextOccurrence(s Schedule, after time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil || s.Timezone == "" || s.Timezone == "Local" {
		return time.Time{}, errors.New("invalid IANA time zone")
	}
	if s.Repeat == "cron" {
		if len(strings.Fields(s.Cron)) != 5 {
			return time.Time{}, errors.New("cron needs five fields: minute hour day month weekday")
		}
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		spec, err := parser.Parse("CRON_TZ=" + s.Timezone + " " + s.Cron)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid cron: %w", err)
		}
		next := spec.Next(after)
		if next.IsZero() {
			return time.Time{}, errors.New("cron has no occurrence in the next five years")
		}
		return next.UTC(), nil
	}
	clock, err := time.Parse("15:04", s.Time)
	if err != nil {
		return time.Time{}, errors.New("time must use 24-hour HH:MM, for example 14:00")
	}
	local := after.In(loc)
	year, month, day := local.Date()
	if s.Repeat == "once" {
		date, err := time.Parse("2006-01-02", s.Date)
		if err != nil {
			return time.Time{}, errors.New("a one-time schedule needs a YYYY-MM-DD date")
		}
		year, month, day = date.Date()
	} else if s.Repeat != "daily" {
		return time.Time{}, errors.New("repeat must be daily, once or cron")
	}
	if s.Repeat == "daily" {
		spec, _ := cron.ParseStandard(fmt.Sprintf("CRON_TZ=%s %d %d * * *", s.Timezone, clock.Minute(), clock.Hour()))
		return spec.Next(after).UTC(), nil
	}
	next := time.Date(year, month, day, clock.Hour(), clock.Minute(), 0, 0, loc)
	if next.Format("2006-01-02 15:04") != s.Date+" "+s.Time {
		return time.Time{}, errors.New("this local time does not exist due to daylight saving time")
	}
	if !next.After(after) {
		return time.Time{}, errors.New("the scheduled time is in the past")
	}
	return next.UTC(), nil
}

func scheduleSummary(schedules []Schedule) string {
	if len(schedules) == 0 {
		return "No reminders or scheduled skills yet."
	}
	lines := []string{}
	for _, s := range schedules {
		status := "paused"
		if s.Enabled {
			status = "active"
		}
		line := fmt.Sprintf("- **%s** (%s): %s at %s, %s. %s. ID: `%s`", s.Title, s.Kind, s.Repeat, s.Time, s.Timezone, status, s.ID)
		if s.Repeat == "cron" {
			line += " Cron: `" + s.Cron + "`."
		}
		line += " Next: `" + s.NextRun + "`. Last status: " + s.LastStatus + "."
		if s.LastError != "" {
			line += " Last error: " + s.LastError
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n") + "\n\nSchedules run on this computer while LiteAgent is open. Missed occurrences are delivered once when it resumes."
}

func (a *App) schedulerLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	a.processDue(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case current := <-ticker.C:
			a.processDue(current)
		}
	}
}

func (a *App) processDue(current time.Time) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.root.Err() != nil {
		return
	}
	a.launchQueued()
	schedules := a.store.Snapshot().Schedules
	// Oldest due occurrence first prevents a frequent job starving another one.
	sort.SliceStable(schedules, func(i, j int) bool { return schedules[i].NextRun < schedules[j].NextRun })
	for _, s := range schedules {
		if !s.Enabled || a.deletingChats[s.ChatID] {
			continue
		}
		due, err := time.Parse(time.RFC3339, s.NextRun)
		if err != nil {
			a.pauseScheduleError(s.ID, "invalid saved next_run")
			continue
		}
		if due.After(current) || s.Kind != "reminder" && a.active != "" {
			continue
		}
		if _, err = a.launchSchedule(s, current, true); err != nil {
			a.pauseScheduleError(s.ID, err.Error())
		}
	}
}

func (a *App) pauseScheduleError(id, message string) {
	if err := a.store.Update(func(st *State) error {
		for i := range st.Schedules {
			if st.Schedules[i].ID == id {
				st.Schedules[i].Enabled = false
				st.Schedules[i].LastStatus = "failed"
				st.Schedules[i].LastError = message
			}
		}
		return nil
	}); err != nil {
		a.logError(err)
	}
}

// runMu is held. The occurrence and run are persisted in the same transaction.
func (a *App) launchSchedule(schedule Schedule, current time.Time, advance bool) (Run, error) {
	if err := a.available(); err != nil {
		return Run{}, err
	}
	if a.deletingChats[schedule.ChatID] {
		return Run{}, errors.New("this conversation is being deleted")
	}
	occurrence := schedule.NextRun
	if !advance {
		occurrence = current.UTC().Format(time.RFC3339)
	}
	claim := func(st *State) error {
		for i := range st.Schedules {
			s := &st.Schedules[i]
			if s.ID != schedule.ID {
				continue
			}
			if advance && (!s.Enabled || s.NextRun != schedule.NextRun) {
				return errors.New("schedule already claimed or changed")
			}
			s.LastRun = current.UTC().Format(time.RFC3339)
			s.LastError = ""
			s.LastFinishedAt = ""
			if advance {
				if s.Repeat == "once" {
					s.Enabled = false
					s.NextRun = ""
				} else {
					next, err := nextOccurrence(*s, current)
					if err != nil {
						return err
					}
					s.NextRun = next.Format(time.RFC3339)
				}
			}
			return nil
		}
		return errors.New("schedule no longer exists")
	}
	if schedule.Kind == "reminder" {
		manual := schedule
		manual.NextRun = occurrence
		return a.recordScheduleReminder(manual, claim)
	}
	task := schedule.Task
	if task == "" {
		task = "Run the saved skill for: " + schedule.Title
	}
	var ids []string
	if schedule.Kind == "skill" {
		ids = []string{schedule.SkillID}
	}
	return a.beginRun(ChatRequest{Task: task, ChatID: schedule.ChatID, SkillIDs: ids, ScheduleID: schedule.ID, ScheduledFor: occurrence}, "scheduled", claim)
}

func finishSchedule(st *State, run Run) {
	if run.ScheduleID == "" {
		return
	}
	for i := range st.Schedules {
		s := &st.Schedules[i]
		if s.ID != run.ScheduleID || s.LastRunID != run.ID {
			continue
		}
		s.LastStatus = run.Status
		s.LastFinishedAt = run.FinishedAt
		if run.Status != "completed" {
			s.Enabled = false
			s.LastError = run.Result
		} else {
			s.LastError = ""
		}
	}
}

func appToolDefinitions() []ToolDef {
	return []ToolDef{
		tool("browser_start", "Open the agent browser so the user can sign in manually.", nil),
		tool("browser_close", "Close the agent browser. The profile and sign-in sessions are kept.", nil),
		tool("skills_list", "List enabled skill summaries only when asked. Does not load instructions.", map[string]any{"offset": map[string]any{"type": "number", "minimum": 0}}),
		tool("skills_files", "List files in a requested skill package without loading their contents.", map[string]any{"id": stringProp("Skill ID"), "offset": map[string]any{"type": "number", "minimum": 0}}, "id"),
		tool("skills_copy_resource", "Copy a skill template or other resource to a working file inside home. Existing files require their prior hash. Does not execute scripts.", map[string]any{"id": stringProp("Skill ID"), "path": stringProp("Package-relative resource"), "destination": stringProp("Working file path inside home"), "expected_sha256": stringProp("Hash of existing destination, if replacing")}, "id", "path", "destination"),
		tool("skills_save", "Create or update a reusable plain-text skill when the user asks to remember, create or edit one. Include verification steps. No executable code or passwords.", map[string]any{"id": stringProp("Existing skill ID to update; omit to create"), "name": stringProp("Skill name"), "description": stringProp("When to use it"), "instructions": stringProp("Reusable steps and verification"), "expected_revision": stringProp("Required when updating: revision from the latest skill read")}, "name", "description", "instructions"),
		tool("skills_install", "Install a plain-text Markdown skill from a direct HTTP/HTTPS URL explicitly supplied by the user. It stores instructions only, never executes downloaded code.", map[string]any{"name": stringProp("Skill name"), "description": stringProp("When to use it"), "url": stringProp("Direct URL of a text skill file")}, "name", "description", "url"),
		tool("skills_remove", "Delete a saved skill only when the user requests its removal. Schedules using it must be removed first.", map[string]any{"id": stringProp("Skill ID")}, "id"),
		tool("schedules_list", "List saved reminders and scheduled skill executions, including their status.", nil),
		tool("schedules_create", "Create a reminder or scheduled skill run requested by the user. Clarify ambiguous times such as 'at 2' before calling. Supports once, daily, or five-field cron. The app must be running.", map[string]any{"title": stringProp("Reminder text or schedule title"), "kind": map[string]any{"type": "string", "enum": []string{"reminder", "skill", "task"}}, "time": stringProp("24-hour local HH:MM"), "repeat": map[string]any{"type": "string", "enum": []string{"daily", "once", "cron"}}, "cron": stringProp("Five-field expression for repeat cron, e.g. 0 14 * * 1-5"), "timezone": stringProp("IANA time zone; omit to use configured zone"), "date": stringProp("YYYY-MM-DD for one-time schedule"), "skill_id": stringProp("Required for kind skill; use an installed skill ID"), "task": stringProp("Complete instructions and parameter values for the scheduled skill")}, "title", "kind", "repeat"),
		tool("schedules_update", "Pause, resume or delete a schedule at the user's request.", map[string]any{"id": stringProp("Schedule ID"), "action": map[string]any{"type": "string", "enum": []string{"pause", "resume", "delete"}}}, "id", "action"),
	}
}

func (a *App) applicationTool(ctx context.Context, run Run, name string, args map[string]any) (string, bool, error) {
	str := func(key string) string { v, _ := args[key].(string); return v }
	encode := func(v any) string { raw, _ := json.Marshal(v); return string(raw) }
	switch name {
	case "conversation_history":
		output, err := a.conversationHistory(ctx, run, args)
		return output, true, err
	case "browser_start":
		err := a.browser.Start(ctx, a.store.Snapshot().Config)
		return "Browser opened. The user can sign in manually.", true, err
	case "browser_close":
		a.browser.Close()
		return "Browser closed. Sessions remain in its profile.", true, nil
	case "skills_list", "skills_search", "skills_read", "skills_files", "skills_copy_resource":
		text, err := a.skillTools(ctx, run, name, args)
		return text, true, err
	case "skills_save", "skills_install":
		instructions := str("instructions")
		if name == "skills_install" {
			address := str("url")
			if !strings.HasPrefix(address, "https://") && !strings.HasPrefix(address, "http://") {
				return "", true, errors.New("use a direct HTTP/HTTPS text URL")
			}
			data, err := download(ctx, address, maxSkillDocumentBytes)
			if err != nil {
				return "", true, err
			}
			if !skillText(data) || strings.Contains(strings.ToLower(string(data)), "<html") {
				return "", true, errors.New("use a direct UTF-8 Markdown URL")
			}
			if strings.HasPrefix(strings.TrimPrefix(string(data), "\ufeff"), "---\n") || strings.HasPrefix(string(data), "---\r\n") {
				saved, err := a.store.installSkillPackage(ctx, "", "", map[string][]byte{"SKILL.md": data}, SkillSource{Kind: "markdown"}, "")
				return encode(saved), true, err
			}
			instructions = string(data)
		}
		saved, err := a.saveSimpleSkill(ctx, Skill{ID: str("id"), Name: str("name"), Description: str("description"), Instructions: instructions}, str("expected_revision"))
		return encode(saved), true, err
	case "skills_remove":
		a.store.skillsMu.Lock()
		defer a.store.skillsMu.Unlock()
		err := a.store.Update(func(st *State) error {
			for _, s := range st.Schedules {
				if s.SkillID == str("id") {
					return errors.New("remove schedules using this skill first")
				}
			}
			for i, s := range st.Skills {
				if s.ID == str("id") {
					st.Skills = append(st.Skills[:i], st.Skills[i+1:]...)
					return nil
				}
			}
			return errors.New("skill not found")
		})
		return "Skill removed.", true, err
	case "schedules_list":
		return scheduleSummary(a.store.Snapshot().Schedules), true, nil
	case "schedules_create":
		s, err := a.createSchedule(Schedule{ChatID: run.ChatID, Title: str("title"), Kind: str("kind"), Time: str("time"), Date: str("date"), Repeat: str("repeat"), Cron: str("cron"), Timezone: str("timezone"), SkillID: str("skill_id"), Task: str("task"), Enabled: true})
		return scheduleSummary([]Schedule{s}), true, err
	case "schedules_update":
		err := a.updateSchedule(str("id"), str("action"))
		return "Schedule updated.", true, err
	}

	return "", false, nil
}
