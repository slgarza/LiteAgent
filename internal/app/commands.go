package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type onboarding struct {
	Step   int
	Config Config
}

// Record deterministic replies and reminders without asking a model. The visible
// user message can be redacted before this function ever sees it.
func (a *App) recordReply(chatID, message, reply, origin string, claim func(*State) error) (Run, error) {
	if chatID == "" {
		chatID = newID()
	}
	run := Run{ID: newID(), ChatID: chatID, Origin: origin, Task: message, Status: "completed", CreatedAt: now(), FinishedAt: now(), SkillIDs: []string{}, Events: []Event{}, Result: reply}
	return a.recordRun(run, claim)
}

func (a *App) recordRun(run Run, claim func(*State) error) (Run, error) {
	chatID, message := run.ChatID, run.Task
	if a.deletingChats[chatID] {
		return Run{}, errors.New("this conversation is being deleted")
	}
	if a.requestID != "" {
		run.RequestID = a.requestID
		run.RequestHash = a.requestHash
	}
	err := a.store.Update(func(st *State) error {
		if claim != nil {
			if err := claim(st); err != nil {
				return err
			}
		}
		found := false
		for i, chat := range st.Chats {
			if chat.ID == chatID {
				chat.UpdatedAt = now()
				st.Chats = append(st.Chats[:i], st.Chats[i+1:]...)
				st.Chats = append([]Chat{chat}, st.Chats...)
				found = true
				break
			}
		}
		if !found {
			st.Chats = append([]Chat{{ID: chatID, Title: cut(message, 70), CreatedAt: now(), UpdatedAt: now(), SkillIDs: []string{}}}, st.Chats...)
		}
		st.Runs = append([]Run{run}, st.Runs...)
		linkSchedule(st, run)
		return nil
	})
	return run, err
}

const commandHelp = `You can chat normally and ask me to use the browser, create folders, read or save local text files, read and edit Excel (.xlsx), Word (.docx) or PowerPoint (.pptx) files, manage skills or schedule work.

**Commands**
- /onboard — connect a model, step by step
- /config — view settings and how to change them
- /models — list models from your provider
- /model MODEL — switch models
- /status — model, current activity and local time
- /skills — list installed skills
- /schedules — list reminders and scheduled skills
- /stop — stop the current reply
- /new — start a new conversation
- /reset (or /clear) — reset context, keep saved messages and schedules
- /reasoning MODE — auto, off/on or effort levels supported by your provider
- /context — inspect the current context size
- /rename TITLE — rename this conversation
- /retry confirm — repeat the last user request (may repeat browser actions)
- /cron — manage cron jobs without a model
- /crons — list scheduled jobs
- /chats — list conversation IDs
- /resume ID — return to a saved conversation
- /cancel — leave onboarding
- /help — show this message
- /notifications — enable browser desktop reminders

Try: “What skills do you have?”, “Save this procedure as superticket”, or “Run superticket every day at 2 PM”.`

// Caller holds runMu so configuration and the onboarding state cannot race a run.
func (a *App) command(ctx context.Context, input ChatRequest) (Run, error) {
	text := strings.TrimSpace(input.Task)
	if input.Message != "" {
		text = strings.TrimSpace(input.Message)
	}
	chatID := input.ChatID
	if chatID == "" {
		chatID = newID()
	}
	if a.wizards == nil {
		a.wizards = map[string]*onboarding{}
	}
	cmd, argument := splitCommand(text)
	reply := ""
	visible := text
	fields := strings.Fields(text)
	if len(fields) >= 2 && strings.EqualFold(fields[0], "/config") && strings.EqualFold(fields[1], "key") {
		visible = "/config key [hidden]"
	}
	c := a.store.Snapshot().Config
	if cmd == "/cancel" {
		delete(a.wizards, chatID)
		return a.recordReply(chatID, visible, "Onboarding cancelled. Your saved settings are unchanged.", "command", nil)
	}
	if wizard := a.wizards[chatID]; wizard != nil && !strings.HasPrefix(text, "/") {
		if wizard.Step == 3 {
			visible = "[API key supplied]"
		}
		if a.active != "" {
			return a.recordReply(chatID, visible, "Stop the current reply before continuing setup.", "command", nil)
		}
		switch wizard.Step {
		case 0:
			provider := strings.ToLower(text)
			if provider != "ollama" && provider != "openai" && provider != "anthropic" && provider != "openai-responses" && provider != "vscode" {
				reply = "Reply with **ollama**, **openai**, **openai-responses**, **anthropic**, or **vscode**."
				break
			}
			wizard.Config.Provider = provider
			if provider == "vscode" {
				wizard.Config.MaxTokens = 0
			}
			wizard.Config.Reasoning = ""
			wizard.Config.Thinking = nil
			wizard.Config.Temperature = nil
			wizard.Config.APIKey = ""
			wizard.Config.BaseURL = providerURL(provider)
			wizard.Step = 1
			reply = "Server URL? Send `default` to use `" + wizard.Config.BaseURL + "`."
		case 1:
			if strings.ToLower(text) != "default" {
				wizard.Config.BaseURL = strings.TrimRight(text, "/")
			}
			check := wizard.Config
			check.Model = "temporary"
			if err := check.Validate(); err != nil {
				reply = err.Error() + ". Enter the server URL again."
				break
			}
			wizard.Step = 2
			reply = "Which model? Enter its model ID, for example `qwen3:8b` for Ollama."
		case 2:
			if text == "" || len(text) > 200 {
				reply = "Enter a model ID up to 200 characters."
				break
			}
			wizard.Config.Model = text
			if wizard.Config.Provider == "ollama" {
				reply = a.finishOnboarding(chatID, wizard)
			} else {
				wizard.Step = 3
				reply = "Enter your API key. It will be hidden from conversation history. Send `/cancel` to leave setup."
			}
		case 3:
			visible = "[API key supplied]"
			wizard.Config.APIKey = text
			reply = a.finishOnboarding(chatID, wizard)
		}
		return a.recordReply(chatID, visible, reply, "command", nil)
	}
	switch cmd {
	case "/onboard":
		if a.active != "" {
			reply = "Stop the current reply before connecting a model."
			break
		}
		if input.Timezone != "" {
			if _, err := time.LoadLocation(input.Timezone); err == nil {
				c.Timezone = input.Timezone
			}
		}
		a.wizards[chatID] = &onboarding{Config: c}
		reply = "Let's connect your model. Which provider? Reply **ollama**, **openai**, **openai-responses**, **anthropic**, or **vscode**. You can send `/cancel` at any point."
	case "/status":
		activity := "idle"
		if a.active != "" {
			activity = "working"
		}
		loc, err := time.LoadLocation(c.Timezone)
		if err != nil {
			loc = time.UTC
		}
		st := a.store.Snapshot()
		activeSchedules := 0
		for _, s := range st.Schedules {
			if s.Enabled {
				activeSchedules++
			}
		}
		reply = fmt.Sprintf("**LiteAgent is %s.**\n\n- Provider: %s\n- Model: `%s`\n- Skills: %d\n- Active schedules: %d\n- Local time: %s (%s)\n\nUse /models to check provider connectivity.", activity, c.Provider, c.Model, len(st.Skills), activeSchedules, time.Now().In(loc).Format("2006-01-02 15:04"), c.Timezone)
		reply += fmt.Sprintf("\n\nReasoning: %s · history: %d turns · action limit: %d · run timeout: %ds\nActive run: `%s`", c.reasoningMode(), c.historyTurns(), c.MaxSteps, c.runTimeout(), a.active)
	case "/reasoning":
		if argument == "" {
			reply = a.reasoningReply(c)
			break
		}
		if a.active != "" {
			reply = "Stop the current reply before changing reasoning."
			break
		}
		c.Reasoning = argument
		c.Thinking = nil
		reply = a.saveCommandConfig(c)
	case "/reset", "/clear":
		if a.active != "" {
			reply = "Stop the current reply before resetting context."
			break
		}
		delete(a.wizards, chatID)
		return a.resetChat(chatID, visible)
	case "/rename":
		if argument == "" || len(argument) > 200 {
			reply = "Use /rename TITLE (up to 200 characters)."
			break
		}
		err := a.store.Update(func(st *State) error {
			for i := range st.Chats {
				if st.Chats[i].ID == chatID {
					st.Chats[i].Title = argument
					return nil
				}
			}
			return errors.New("conversation not found")
		})
		if err != nil {
			reply = err.Error()
		} else {
			reply = "Conversation renamed."
		}
	case "/context":
		history := chatContext(a.store.Snapshot(), chatID)
		chars := 0
		for _, m := range history {
			chars += len(m.Content)
		}
		reply = fmt.Sprintf("Context: %d recent turns, %d bytes. Configured history: %d turns. /reset starts fresh without deleting history or schedules.", len(history)/2, chars, c.historyTurns())
	case "/retry":
		for _, previous := range a.store.Snapshot().Runs {
			if previous.ChatID != chatID || previous.Origin != "user" {
				continue
			}
			if argument != "confirm" {
				reply = "Repeat the latest user request with `/retry confirm`. Browser actions may be performed again."
				break
			}
			return a.beginRun(ChatRequest{ChatID: chatID, Task: previous.Task, SkillIDs: previous.SkillIDs}, "user", nil)
		}
		if reply == "" {
			reply = "No user request to retry in this conversation."
		}
	case "/cron":
		return a.cronCommand(chatID, visible, argument)
	case "/help":
		reply = commandHelp
	case "/new":
		delete(a.wizards, chatID)
		chatID = newID()
		reply = "New conversation. How can I help?"
	case "/chats":
		reply = "**Saved conversations**\n\n"
		for _, chat := range a.store.Snapshot().Chats {
			reply += "- " + chat.Title + " — `" + chat.ID + "`\n"
		}
		reply += "\nUse `/resume ID` to continue one."
	case "/resume":
		reply = "Conversation not found. Use /chats to list IDs."
		for _, chat := range a.store.Snapshot().Chats {
			if chat.ID == argument {
				chatID = chat.ID
				reply = "Resumed **" + chat.Title + "**. Your recent conversation is available as context."
				break
			}
		}
	case "/stop":
		a.acceptingSteer = false
		if err := a.store.Update(func(st *State) error { pauseMessages(st, "Stopped. Resume this message when ready."); return nil }); err != nil {
			a.queueBlocked = true
			a.logError(err)
		}
		if a.cancel != nil {
			a.cancel()
			reply = "Stopping the current reply. Actions already performed remain in effect."
		} else {
			reply = "Nothing is running."
		}
	case "/skills":
		st := a.store.Snapshot()
		if len(st.Skills) == 0 {
			reply = "No skills installed yet. Tell me a procedure to remember, or give me a direct Markdown skill URL to install."
		} else {
			lines := []string{}
			for _, s := range st.Skills {
				lines = append(lines, "- **"+s.Name+"**: "+s.Description+" (ID: `"+s.ID+"`)")
			}
			reply = strings.Join(lines, "\n")
		}
	case "/schedules", "/crons":
		reply = scheduleSummary(a.store.Snapshot().Schedules)
	case "/notifications":
		switch argument {
		case "granted":
			reply = "Desktop notifications are enabled in this browser. Keep the chat open to receive them; reminders are always saved in their conversation."
		case "denied":
			reply = "Desktop notifications are blocked in this browser. Reminders will still appear in their conversation."
		default:
			reply = "Use /notifications in the web chat to enable desktop notifications. Reminders also appear in their conversation and in the CLI."
		}
	case "/models":
		reply = input.ModelsReply
		if reply == "" {
			reply = modelsReply(ctx, c)
		}

	case "/model":
		if argument == "" {
			reply = "Current model: `" + c.Model + "`. Use `/model MODEL_ID` to switch."
			break
		}
		if a.active != "" {
			reply = "Stop the current reply before changing models."
			break
		}
		c.Model = argument
		reply = a.saveCommandConfig(c)
	case "/config":
		if argument == "" {
			reply = fmt.Sprintf("**Current settings**\n\nProvider: %s\nURL: `%s`\nModel: `%s`\nTime zone: `%s`\nRequest timeout: %d seconds\n\nChange one setting with:\n- `/config provider ollama`\n- `/config url http://127.0.0.1:11434`\n- `/config model qwen3:8b`\n- `/config key YOUR_KEY` (hidden from history)\n- `/config timezone America/Mexico_City`\n- `/config headless true`\n- `/config max_steps 20`\n- `/config timeout 900`\n- `/config thinking false` (Ollama; true or auto also supported)\n- `/config browser_path PATH`\n- `/config driver_path PATH`\n\nOr use /onboard for guided setup.", c.Provider, c.BaseURL, c.Model, c.Timezone, c.TimeoutSec)
			reply += fmt.Sprintf("\n\nReasoning: %s; max_tokens: %d; context_tokens: %d; history_turns: %d; run_timeout: %ds.\n\nAlso: `/reasoning MODE`, `/config temperature auto`, `/config max_tokens 4096`, `/config context_tokens 16384`, `/config history_turns 12`, `/config run_timeout 1800`, `/config system_prompt TEXT` (or clear). Zero restores default for numeric generation settings.", c.reasoningMode(), c.MaxTokens, c.contextTokens(), c.historyTurns(), c.runTimeout())
			break
		}
		key, value := splitCommand(argument)
		if value == "" {
			reply = "Use `/config SETTING VALUE` or `/onboard`."
			break
		}
		if key == "key" {
			visible = "/config key [hidden]"
		}
		if a.active != "" {
			reply = "Stop the current reply before changing settings."
			break
		}
		var err error
		switch key {
		case "provider":
			c.Reasoning = ""
			c.Thinking = nil
			c.Temperature = nil
			c.Provider = value
			if value == "vscode" {
				c.MaxTokens = 0
			}
			c.BaseURL = providerURL(value)
			c.APIKey = ""
		case "url":
			c.BaseURL = strings.TrimRight(value, "/")
			c.APIKey = ""
		case "model":
			c.Model = value
		case "key":
			c.APIKey = value
			if value == "clear" {
				c.APIKey = ""
			}
		case "timezone":
			c.Timezone = value
		case "headless":
			c.Headless, err = strconv.ParseBool(value)
		case "max_steps":
			c.MaxSteps, err = strconv.Atoi(value)
		case "timeout":
			c.TimeoutSec, err = strconv.Atoi(value)
		case "thinking":
			c.Reasoning = ""
			if value == "auto" {
				c.Thinking = nil
			} else {
				var enabled bool
				enabled, err = strconv.ParseBool(value)
				if err == nil {
					c.Thinking = &enabled
				}
			}
		case "reasoning":
			c.Reasoning = value
			c.Thinking = nil
		case "temperature":
			if value == "auto" {
				c.Temperature = nil
			} else {
				var v float64
				v, err = strconv.ParseFloat(value, 64)
				c.Temperature = &v
			}
		case "max_tokens":
			c.MaxTokens, err = strconv.Atoi(value)
		case "context_tokens":
			c.ContextTokens, err = strconv.Atoi(value)
		case "history_turns":
			c.HistoryTurns, err = strconv.Atoi(value)
		case "run_timeout":
			c.RunTimeoutSec, err = strconv.Atoi(value)
		case "system_prompt":
			c.SystemPrompt = value
			if value == "clear" {
				c.SystemPrompt = ""
			}
		case "browser_path":
			c.BrowserPath = value
			if value == "auto" {
				c.BrowserPath = ""
			}
		case "driver_path":
			c.DriverPath = value
			if value == "auto" {
				c.DriverPath = ""
			}
		default:
			err = errors.New("unknown setting; use /config to see available settings")
		}
		if err != nil {
			reply = err.Error()
		} else {
			reply = a.saveCommandConfig(c)
		}
	default:
		reply = "Unknown command. Use /help to see available commands."
	}
	return a.recordReply(chatID, visible, reply, "command", nil)
}

func providerURL(provider string) string {
	switch provider {
	case "ollama":
		return "http://127.0.0.1:11434"
	case "openai", "openai-responses":
		return "https://api.openai.com/v1"
	case "anthropic":
		return "https://api.anthropic.com"
	case "vscode":
		return "http://127.0.0.1:8788/v1"
	}
	return ""
}
func (a *App) saveCommandConfig(c Config) string {
	previous := a.store.Snapshot().Config
	if previous.Model != c.Model || previous.Provider != c.Provider || previous.BaseURL != c.BaseURL {
		c.Reasoning, c.Thinking = "", nil
	}
	if err := c.Validate(); err != nil {
		return "Settings not saved: " + err.Error()
	}
	if c.reasoningMode() != previous.reasoningMode() {
		if err := a.validateModelReasoning(c); err != nil {
			return "Settings not saved: " + err.Error()
		}
	}
	if err := a.store.Update(func(st *State) error { saveConfig(st, c); return nil }); err != nil {
		return "Could not save settings: " + err.Error()
	}
	if previous.Headless != c.Headless || previous.DriverPath != c.DriverPath || previous.BrowserPath != c.BrowserPath {
		a.browser.Close()
	}
	return "Settings saved. Model: `" + c.Model + "`. Use /models to check the connection, or send a message."
}
func (a *App) finishOnboarding(chatID string, wizard *onboarding) string {
	reply := a.saveCommandConfig(wizard.Config)
	if strings.HasPrefix(reply, "Settings saved.") {
		delete(a.wizards, chatID)
		reply += "\n\nYour chat is ready. Ask me what skills I have, open a website, or create a reminder. Schedules use " + wizard.Config.Timezone + "."
	}
	return reply
}

func listModels(ctx context.Context, c Config) ([]string, error) {
	models := []string{}
	if c.Provider == "ollama" {
		var data struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := jsonRequest(ctx, c, http.MethodGet, "/api/tags", nil, &data); err != nil {
			return nil, err
		}
		for _, m := range data.Models {
			models = append(models, m.Name)
		}
	} else {
		path := "/models"
		if c.Provider == "anthropic" && !strings.HasSuffix(strings.TrimRight(c.BaseURL, "/"), "/v1") {
			path = "/v1/models"
		}
		var data struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := jsonRequest(ctx, c, http.MethodGet, path, nil, &data); err != nil {
			return nil, err
		}
		for _, m := range data.Data {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

func modelsReply(ctx context.Context, c Config) string {
	reply := ""

	timeout, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	models, err := listModels(timeout, c)
	if err != nil {
		reply = "Could not list models: " + err.Error() + "\n\nUse /onboard or /config to check your connection."
	} else {
		reply = "**Available models**\n\n"
		for _, m := range models[:min(100, len(models))] {
			reply += "- `" + m + "`\n"
		}
		if len(models) == 0 {
			reply += "No models returned. For local Ollama, download a model first."
		}
		reply += "\nSwitch with `/model MODEL_ID`."
	}
	return reply
}
