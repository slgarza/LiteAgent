package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserIntegration(t *testing.T) {
	if os.Getenv("LITEAGENT_BROWSER_TEST") != "1" {
		t.Skip("set LITEAGENT_BROWSER_TEST=1 to test real Chrome")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/report.csv" {
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Disposition", `attachment; filename="report.csv"`)
			fmt.Fprint(w, "period,total\n2026-11,42\n")
			return
		}
		if r.URL.Path == "/second" {
			fmt.Fprint(w, "<html><title>Help</title><body>Second tab</body></html>")
			return
		}
		fmt.Fprint(w, `<!doctype html><html><title>LiteAgent test</title><body><form onsubmit="event.preventDefault();document.getElementById('result').textContent='Report for '+document.getElementById('reference').value+' in '+document.getElementById('period').value;document.getElementById('download').hidden=false;"><label>Reference<input id="reference"></label><label>Period<select id="period"><option value="october">October</option><option value="november">November</option></select></label><button>Query</button></form><p id="result"></p><a id="download" hidden href="/report.csv" download>Download CSV</a><a href="/second" target="_blank">Help</a></body></html>`)
	}))
	defer fixture.Close()
	dir := t.TempDir()
	b := &Browser{dir: dir}
	defer b.Close()
	c := defaultConfig()
	c.Headless = true
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if cache := os.Getenv("LITEAGENT_TEST_DATA"); cache != "" {
		var err error
		c.DriverPath, _, err = ensureDriver(ctx, c, cache)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Start(ctx, c); err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) string {
		t.Helper()
		out, err := b.Tool(ctx, name, args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	read := call("browser_open", map[string]any{"url": fixture.URL})
	find := func(snapshot, label string) string {
		t.Helper()
		var s struct {
			Elements []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"elements"`
		}
		if err := json.Unmarshal([]byte(snapshot), &s); err != nil {
			t.Fatal(err)
		}
		for _, e := range s.Elements {
			if strings.Contains(e.Label, label) {
				return e.ID
			}
		}
		t.Fatalf("could not find %s in %s", label, snapshot)
		return ""
	}
	read = call("browser_type", map[string]any{"element_id": find(read, "Reference"), "text": "TEST-42"})
	read = call("browser_select", map[string]any{"element_id": find(read, "Period"), "value": "november"})
	read = call("browser_click", map[string]any{"element_id": find(read, "Query")})
	if !strings.Contains(read, "Report for TEST-42 in november") {
		t.Fatal("did not verify the form")
	}
	read = call("browser_click", map[string]any{"element_id": find(read, "Download CSV")})
	for i := 0; i < 30; i++ {
		if _, err := os.Stat(filepath.Join(dir, "downloads", "report.csv")); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	content, err := os.ReadFile(filepath.Join(dir, "downloads", "report.csv"))
	if err != nil || !strings.Contains(string(content), "2026-11,42") {
		t.Fatalf("incorrect download: %s %v", content, err)
	}
	read = call("browser_read", map[string]any{})
	call("browser_click", map[string]any{"element_id": find(read, "Help")})
	var handles []string
	_ = json.Unmarshal([]byte(call("browser_tabs", map[string]any{})), &handles)
	if len(handles) != 2 {
		t.Fatalf("tabs: %v", handles)
	}
	read = call("browser_switch", map[string]any{"tab_id": handles[1]})
	if !strings.Contains(read, "Second tab") {
		t.Fatal("did not switch tabs")
	}
	if _, err := b.Tool(ctx, "browser_click", map[string]any{"element_id": "9999"}); err == nil {
		t.Fatal("accepted a missing element")
	}
	t.Log("Real Chrome: form, select, verification, CSV download and tab switching passed")
}

func TestAgentBrowserE2E(t *testing.T) {
	if os.Getenv("LITEAGENT_BROWSER_TEST") != "1" {
		t.Skip("set LITEAGENT_BROWSER_TEST=1")
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><html><body><button onclick="document.getElementById('result').textContent='Operation verified 742'">Verify</button><p id="result"></p></body></html>`)
	}))
	defer page.Close()
	step := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		name := "browser_open"
		args := map[string]any{"url": page.URL}
		if step == 1 {
			var snapshot struct {
				Elements []struct {
					ID    string `json:"id"`
					Label string `json:"label"`
				} `json:"elements"`
			}
			if err := json.Unmarshal([]byte(payload.Messages[len(payload.Messages)-1].Content), &snapshot); err != nil {
				t.Error(err)
			}
			id := ""
			for _, e := range snapshot.Elements {
				if e.Label == "Verify" {
					id = e.ID
				}
			}
			if id == "" {
				t.Error("the model did not receive browser controls")
			}
			name = "browser_click"
			args = map[string]any{"element_id": id}
		} else if step == 2 {
			if !strings.Contains(payload.Messages[len(payload.Messages)-1].Content, "Operation verified 742") {
				t.Error("the agent did not observe the click result")
			}
			sendJSON(w, 200, map[string]any{"message": Message{Role: "assistant", Content: "Operation verified 742"}})
			return
		}
		step++
		raw, _ := json.Marshal(args)
		sendJSON(w, 200, map[string]any{"message": Message{Role: "assistant", ToolCalls: []ToolCall{{Type: "function", Function: FunctionCall{name, raw}}}}})
	}))
	defer model.Close()
	a := newTestApp(t)
	c := defaultConfig()
	c.BaseURL = model.URL
	c.Headless = true
	if cache := os.Getenv("LITEAGENT_TEST_DATA"); cache != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		var err error
		c.DriverPath, _, err = ensureDriver(ctx, c, cache)
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = a.store.Update(func(st *State) error { st.Config = c; return nil })
	w := callAPI(t, a, "POST", "/api/runs", map[string]any{"task": "Open the website and click Verify. Check the result."}, nil)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var run Run
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	run = waitRun(t, a, run.ID)
	if run.Status != "completed" || run.Result != "Operation verified 742" {
		t.Fatalf("task failed: %+v", run)
	}
	t.Log("API -> mock model -> real Chrome -> observation -> persistent result: passed")
}
