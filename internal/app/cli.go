package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"
)

type cliState struct {
	Version string `json:"version"`
	Runs    []Run  `json:"runs"`
	Chats   []Chat `json:"chats"`
	Notices []struct {
		ID     string `json:"id"`
		Text   string `json:"text"`
		ChatID string `json:"chat_id"`
	} `json:"notices"`
}

func cliRequest(ctx context.Context, base, path, method string, body, result any) error {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LiteAgent", "1")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	reader := io.LimitReader(resp.Body, 16<<20)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(reader).Decode(&failure) == nil && failure.Error != "" {
			return fmt.Errorf("%s", failure.Error)
		}
		return fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	if result == nil {
		return nil
	}
	return json.NewDecoder(reader).Decode(result)
}

func fetchCLIState(ctx context.Context, base, chatID string) (cliState, error) {
	var st cliState
	err := cliRequest(ctx, base, "/api/state?chat_id="+url.QueryEscape(chatID), "GET", nil, &st)
	if err == nil && st.Version == "" {
		err = fmt.Errorf("the server is not LiteAgent")
	}
	return st, err
}

// Model and webpage output must not be interpreted as terminal escape sequences.
func terminalText(text string) string {
	return strings.Map(func(r rune) rune {
		if (r < 32 && r != '\n' && r != '\t') || (r >= 127 && r <= 159) {
			return -1
		}
		return r
	}, text)
}

func chatCLI(ctx context.Context, base, chatID string, input io.Reader, output io.Writer) error {
	st, err := fetchCLIState(ctx, base, "new")
	if err != nil {
		return err
	}
	if chatID != "" {
		found := false
		for _, chat := range st.Chats {
			if chat.ID == chatID {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("conversation not found; use /chats to list conversations")
		}
	}
	fmt.Fprintln(output, "LiteAgent chat. /onboard connects a model. /help lists commands. /exit leaves the CLI.")
	lines := make(chan string)
	scanErrors := make(chan error, 1)
	readerCtx, cancelReader := context.WithCancel(ctx)
	defer cancelReader()
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), 128<<10)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-readerCtx.Done():
				return
			}
		}
		scanErrors <- scanner.Err()
	}()
	known := map[string]bool{}
	for _, n := range st.Notices {
		known[n.ID] = true
	}
	activeID, shownTools := "", 0
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	prompt := func() { fmt.Fprint(output, "\nYou> ") }
	printReply := func(r Run) {
		fmt.Fprintln(output, "\nLiteAgent> "+terminalText(r.Result))
		if r.Status != "completed" {
			fmt.Fprintln(output, "["+terminalText(r.Status)+"]")
		}
	}
	prompt()
	for {
		if lines == nil && activeID == "" {
			return <-scanErrors
		}
		select {
		case <-ctx.Done():
			return nil
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			line = strings.TrimSpace(line)
			if line == "" {
				prompt()
				continue
			}
			if line == "/exit" || line == "/quit" {
				return nil
			}
			if activeID != "" && !strings.HasPrefix(line, "/") {
				fmt.Fprintln(output, "Wait for the current reply or use /stop.")
				continue
			}
			var run Run
			if err := cliRequest(ctx, base, "/api/chat", "POST", ChatRequest{Message: line, ChatID: chatID}, &run); err != nil {
				fmt.Fprintln(output, "\nError: "+terminalText(err.Error()))
				prompt()
				continue
			}
			chatID = run.ChatID
			if run.Status == "running" {
				activeID, shownTools = run.ID, 0
				fmt.Fprintln(output, "\nThinking…")
			} else {
				printReply(run)
				prompt()
			}
		case <-ticker.C:
			current, err := fetchCLIState(ctx, base, "")
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			for i := len(current.Notices) - 1; i >= 0; i-- {
				n := current.Notices[i]
				if !known[n.ID] {
					fmt.Fprintln(output, "\nReminder> "+terminalText(n.Text))
					known[n.ID] = true
					if activeID == "" {
						prompt()
					}
				}
			}
			for _, run := range current.Runs {
				if run.ID != activeID {
					continue
				}
				for _, event := range run.Events[min(shownTools, len(run.Events)):] {
					if event.Kind == "tool" {
						fmt.Fprintln(output, "[Using "+terminalText(event.Name)+"]")
					}
				}
				shownTools = len(run.Events)
				if run.Status != "running" {
					printReply(run)
					activeID = ""
					prompt()
				}
			}
		}
	}
}

func runCLI(base, message string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	var run Run
	if err := cliRequest(ctx, base, "/api/chat", "POST", ChatRequest{Message: message}, &run); err != nil {
		return fmt.Errorf("start liteagent serve first: %w", err)
	}
	for run.Status == "running" {
		select {
		case <-ctx.Done():
			_ = cliRequest(context.Background(), base, "/api/stop", "POST", nil, nil)
			return fmt.Errorf("stop request sent")
		case <-time.After(time.Second):
		}
		st, err := fetchCLIState(ctx, base, run.ChatID)
		if err != nil {
			return err
		}
		for _, current := range st.Runs {
			if current.ID == run.ID {
				run = current
				break
			}
		}
	}
	fmt.Println(terminalText(run.Result))
	if run.Status != "completed" {
		return fmt.Errorf("status: %s", run.Status)
	}
	return nil
}
