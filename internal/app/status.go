package app

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

type StatusRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
}
type StatusSection struct {
	Title string      `json:"title"`
	Rows  []StatusRow `json:"rows"`
}
type StatusReport struct {
	Title    string          `json:"title"`
	Sections []StatusSection `json:"sections"`
}

func (s StatusReport) text() string {
	var out strings.Builder
	fmt.Fprintf(&out, "**%s**\n", s.Title)
	for _, section := range s.Sections {
		fmt.Fprintf(&out, "\n**%s**\n", section.Title)
		for _, row := range section.Rows {
			fmt.Fprintf(&out, "- %s: %s\n", row.Label, row.Value)
		}
	}
	return out.String()
}
func age(from string, current time.Time) string {
	t, err := time.Parse(time.RFC3339, from)
	if err != nil {
		return "Not recorded"
	}
	return durationText(current.Sub(t))
}
func durationText(d time.Duration) string {
	seconds := int64(max(0, d.Seconds()))
	if seconds >= 86400 {
		return fmt.Sprintf("%dd %dh %dm", seconds/86400, seconds/3600%24, seconds/60%60)
	}
	if seconds >= 3600 {
		return fmt.Sprintf("%dh %dm", seconds/3600, seconds/60%60)
	}
	if seconds >= 60 {
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%ds", seconds)
}
func tokenText(n *int64) string {
	if n == nil {
		return "not reported"
	}
	return fmt.Sprintf("%d", *n)
}
func safeEndpoint(address string) string {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		return "Not configured"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	u.RawPath = ""
	return u.String()
}
func buildLabel() string {
	label := "LiteAgent " + Version
	if info, ok := debug.ReadBuildInfo(); ok {
		revision, dirty := "", false
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				revision = s.Value
			}
			if s.Key == "vcs.modified" {
				dirty = s.Value == "true"
			}
		}
		if len(revision) > 7 {
			revision = revision[:7]
		}
		if revision != "" {
			label += " · " + revision
		}
		if dirty {
			label += " · modified"
		}
	}
	return label
}

// Caller supplies a consistent snapshot of state and active execution. Status
// does no network discovery and includes neither prompts nor credentials.
func workspaceStatus(st State, chatID, active string, canSteer, queueBlocked bool, started, current time.Time) StatusReport {
	c := st.Config
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		loc = time.UTC
	}
	uptime := "Not recorded"
	if !started.IsZero() {
		uptime = durationText(current.Sub(started))
	}
	report := StatusReport{Title: buildLabel()}
	add := func(title string, rows ...StatusRow) {
		report.Sections = append(report.Sections, StatusSection{title, rows})
	}
	add("Runtime", StatusRow{"Local time", current.In(loc).Format("Monday, 2006-01-02 15:04:05") + " · " + c.Timezone}, StatusRow{"UTC", current.UTC().Format("2006-01-02 15:04:05 UTC")}, StatusRow{"App uptime", uptime}, StatusRow{"Runtime", fmt.Sprintf("%s/%s · %s", runtime.GOOS, runtime.GOARCH, runtime.Version())})
	auth := "No credential configured"
	if c.APIKey != "" {
		auth = "API key configured"
		if c.Provider == "vscode" {
			auth = "Bridge token configured · model account managed by VS Code"
		}
	}
	if c.APIKey == "" && c.Provider == "ollama" {
		auth = "No API key configured"
	}
	connection := "Default connection"
	for _, saved := range st.Connections {
		if saved.ID == c.ConnectionID {
			connection = saved.Name
			break
		}
	}
	add("Connection", StatusRow{"Model", c.Provider + " / " + c.Model}, StatusRow{"Connection", connection}, StatusRow{"Endpoint", safeEndpoint(c.BaseURL)}, StatusRow{"Authentication", auth}, StatusRow{"Reasoning", c.reasoningMode()}, StatusRow{"Account quota", "Not available through this integration"})
	var chat *Chat
	for i := range st.Chats {
		if st.Chats[i].ID == chatID {
			chat = &st.Chats[i]
			break
		}
	}
	var latest *Run
	var totals TokenUsage
	requests, reports, replies, legacy := 0, 0, 0, 0
	for i := range st.Runs {
		r := &st.Runs[i]
		if r.ChatID != chatID || r.Origin == "command" || r.Origin == "reminder" {
			continue
		}
		replies++
		if latest == nil {
			latest = r
		}
		if r.Metrics == nil {
			legacy++
			continue
		}
		requests += r.Metrics.Requests
		reports += r.Metrics.UsageReports
		totals.add(r.Metrics.Totals)
	}
	id, sessionAge, updated := "New chat", "—", "—"
	if chat != nil {
		id = chat.ID
		sessionAge = age(chat.CreatedAt, current)
		updated = age(chat.UpdatedAt, current) + " ago"
	}
	if latest != nil {
		stamp := latest.CreatedAt
		if len(latest.Events) > 0 {
			stamp = latest.Events[len(latest.Events)-1].Time
		}
		if latest.FinishedAt != "" {
			stamp = latest.FinishedAt
		}
		updated = age(stamp, current) + " ago"
	}
	coverage := fmt.Sprintf("%d/%d model requests have usage data", reports, requests)
	if legacy > 0 {
		coverage += fmt.Sprintf(" · %d older replies without metrics", legacy)
	}
	add("Session", StatusRow{"Chat ID", id}, StatusRow{"Age / updated", sessionAge + " / " + updated}, StatusRow{"Saved replies", fmt.Sprint(replies)}, StatusRow{"Tokens · reported totals", tokenText(totals.Input) + " in / " + tokenText(totals.Output) + " out"}, StatusRow{"Usage coverage", coverage})
	cache, context := "Not reported for the last request", "Not reported for the last request"
	if latest != nil && latest.Metrics != nil {
		u := latest.Metrics.LastUsage
		if u.Cached != nil {
			cache = fmt.Sprintf("%d cached input tokens", *u.Cached)
			if u.Input != nil && *u.Input > 0 && *u.Cached <= *u.Input {
				cache += fmt.Sprintf(" · %.1f%% hit", 100*float64(*u.Cached)/float64(*u.Input))
			}
		}
		if u.CacheWrite != nil {
			cache += " · " + tokenText(u.CacheWrite) + " cache-write tokens"
		}
		if u.Input != nil {
			context = tokenText(u.Input) + " input tokens · model " + latest.Model
			if latest.ContextLimit > 0 {
				context += fmt.Sprintf(" / %d configured (%.1f%%)", latest.ContextLimit, 100*float64(*u.Input)/float64(latest.ContextLimit))
			} else {
				context += " · capacity not reported"
			}
		}
	}
	history := chatContext(st, chatID)
	size := 0
	for _, m := range history {
		size += len(m.Content)
	}
	reset := "No reset boundary"
	if chat != nil && chat.ContextResetID != "" {
		reset = "After last /reset; older history remains saved"
	}
	add("Context & cache", StatusRow{"Last request input", context}, StatusRow{"Cache · last request", cache}, StatusRow{"Automatic history", fmt.Sprintf("%d/%d turns · %d/16000 bytes", len(history)/2, c.historyTurns(), size)}, StatusRow{"History boundary", reset})
	activity := "idle"
	if active != "" {
		activity = "working in another chat"
		if latest != nil && latest.ID == active {
			activity = "working in this chat"
		}
	}
	execution := []StatusRow{{"Activity", activity}, {"Execution", "Local tools · one active reply at a time"}}
	if latest != nil {
		end := current
		if latest.FinishedAt != "" {
			if t, e := time.Parse(time.RFC3339, latest.FinishedAt); e == nil {
				end = t
			}
		}
		success, failed, unknown := 0, 0, 0
		for _, action := range recordedActions(*latest) {
			if action.ResultEvent == nil {
				unknown++
			} else if latest.Events[*action.ResultEvent].Kind == "error" {
				failed++
			} else {
				success++
			}
		}
		last := "No event recorded"
		if len(latest.Events) > 0 {
			e := latest.Events[len(latest.Events)-1]
			last = e.Kind
			if e.Name != "" {
				last += " · " + e.Name
			}
		}
		provider, reasoning := latest.Provider, latest.Reasoning
		if provider == "" {
			provider = "provider not recorded"
		}
		if reasoning == "" {
			reasoning = "not recorded"
		}
		execution = append(execution, StatusRow{"Reply ID", latest.ID}, StatusRow{"Reply state", latest.Status + " · " + latest.Origin + " · " + age(latest.CreatedAt, end)}, StatusRow{"Reply model", provider + " / " + latest.Model + " · reasoning " + reasoning}, StatusRow{"Actions", fmt.Sprintf("%d succeeded · %d failed · %d unknown", success, failed, unknown)}, StatusRow{"Latest event", last}, StatusRow{"Selected skills", fmt.Sprintf("%d for this reply", len(latest.SkillIDs))})
		if latest.Metrics != nil {
			execution = append(execution, StatusRow{"Model requests", fmt.Sprintf("%d · %s total waiting time", latest.Metrics.Requests, durationText(time.Duration(latest.Metrics.ModelTimeMS)*time.Millisecond))})
		}
	}
	add("Execution", execution...)
	queued, paused, steering := 0, 0, 0
	for _, q := range st.Queue {
		if q.ChatID != chatID || !pendingMessage(q.Status) {
			continue
		}
		if q.Status == "paused" || queueBlocked {
			paused++
		} else if q.Status == "steering" {
			steering++
		} else {
			queued++
		}
	}
	queue := fmt.Sprintf("%d queued · %d paused · %d steering", queued, paused, steering)
	steer := "Queue follow-ups; Steer available during a reply"
	if latest != nil && latest.ID == active && canSteer {
		steer = "Steer available for the active reply"
	}
	schedules, mcp := 0, 0
	for _, s := range st.Schedules {
		if s.Enabled {
			schedules++
		}
	}
	for _, s := range st.MCPServers {
		if s.Enabled {
			mcp++
		}
	}
	add("Queue & limits", StatusRow{"This chat's queue", queue}, StatusRow{"Delivery", steer}, StatusRow{"Action budgets", fmt.Sprintf("%d successful / %d failed calls per reply", c.MaxSteps, c.toolFailureLimit())}, StatusRow{"Timeouts", fmt.Sprintf("%s per model request / %s per reply", durationText(time.Duration(c.TimeoutSec)*time.Second), durationText(time.Duration(c.runTimeout())*time.Second))}, StatusRow{"Skills", fmt.Sprintf("%d installed · only on request or explicitly scheduled", len(st.Skills))}, StatusRow{"Automation", fmt.Sprintf("%d active schedules · %d enabled MCP servers", schedules, mcp)})
	// Names, paths and IDs are displayed as text, but scrub saved secrets even if
	// a user accidentally pasted one into a name or endpoint path.
	secrets := []string{c.APIKey}
	for _, v := range st.Connections {
		secrets = append(secrets, v.APIKey)
	}
	for _, v := range st.MCPServers {
		secrets = append(secrets, v.Token)
	}
	for i := range report.Sections {
		for j := range report.Sections[i].Rows {
			for _, secret := range secrets {
				if secret != "" {
					row := &report.Sections[i].Rows[j]
					row.Value = strings.ReplaceAll(row.Value, secret, "[hidden]")
					row.Value = strings.ReplaceAll(row.Value, url.PathEscape(secret), "[hidden]")
				}
			}
		}
	}
	return report
}
func (a *App) statusAPI(w http.ResponseWriter, r *http.Request) {
	a.runMu.Lock()
	st := a.store.Snapshot()
	active, steer, blocked := a.active, a.acceptingSteer, a.queueBlocked
	started := a.startedAt
	a.runMu.Unlock()
	chatID := r.URL.Query().Get("chat_id")
	if chatID != "" && chatID != "new" {
		found := false
		for _, chat := range st.Chats {
			if chat.ID == chatID {
				found = true
				break
			}
		}
		if !found {
			fail(w, 404, errors.New("conversation not found"))
			return
		}
	}
	sendJSON(w, 200, workspaceStatus(st, chatID, active, steer, blocked, started, time.Now()))
}
