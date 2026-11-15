package app

// Alerts are independent of the selected chat. CLI clients retain their
// existing reminder-only notices.
func stateAlerts(st State) []map[string]string {
	titles := map[string]string{}
	for _, schedule := range st.Schedules {
		titles[schedule.ID] = schedule.Title
	}
	alerts := []map[string]string{}
	for _, run := range st.Runs {
		if run.Status == "running" || run.Status == "cancelled" || run.Origin == "command" {
			continue
		}
		kind, title := "", ""
		switch run.Origin {
		case "reminder":
			kind, title = "reminder", "Reminder"
		case "scheduled":
			kind, title = "scheduled", titles[run.ScheduleID]
			if title == "" {
				title = "Scheduled task"
			}
		case "user":
			kind, title = "reply", "Reply ready"
		default:
			continue
		}
		if run.Status != "completed" {
			title = "Needs attention · " + title
		}
		created := run.FinishedAt
		if created == "" {
			created = run.CreatedAt
		}
		alerts = append(alerts, map[string]string{"id": run.ID, "chat_id": run.ChatID, "kind": kind, "title": title, "text": cut(run.Result, 300), "created_at": created, "status": run.Status})
		if len(alerts) == 100 {
			break
		}
	}
	return alerts
}
