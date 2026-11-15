package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Browser struct {
	mu                     sync.Mutex
	endpoint, session, dir string
	cmd                    *exec.Cmd
	done                   chan error
	logFile                *os.File
}

func (b *Browser) command(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.endpoint+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var wrapper struct {
		Value json.RawMessage `json:"value"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&wrapper); err != nil {
		return nil, err
	}
	var failure struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(wrapper.Value, &failure)
	if resp.StatusCode >= 400 || failure.Error != "" {
		return nil, fmt.Errorf("browser: %s %s", failure.Error, cut(failure.Message, 800))
	}
	return wrapper.Value, nil
}

func (b *Browser) Start(ctx context.Context, c Config) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != "" {
		if _, err := b.command(ctx, http.MethodGet, "/session/"+b.session+"/url", nil); err == nil {
			return nil
		}
		b.closeLocked()
	}
	driver, binary, err := ensureDriver(ctx, c, b.dir)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	b.endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)
	if err = os.MkdirAll(filepath.Join(b.dir, "downloads"), 0700); err != nil {
		return err
	}
	b.logFile, err = os.OpenFile(filepath.Join(b.dir, "chromedriver.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	b.cmd = exec.Command(driver, fmt.Sprintf("--port=%d", port), "--allowed-ips=127.0.0.1")
	b.cmd.Stdout = b.logFile
	b.cmd.Stderr = b.logFile
	if err = b.cmd.Start(); err != nil {
		b.closeLocked()
		return err
	}
	b.done = make(chan error, 1)
	cmd := b.cmd
	done := b.done
	go func() { done <- cmd.Wait() }()
	ready := false
	for i := 0; i < 60; i++ {
		if ctx.Err() != nil {
			b.closeLocked()
			return ctx.Err()
		}
		check, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		_, err = b.command(check, http.MethodGet, "/status", nil)
		cancel()
		if err == nil {
			ready = true
			break
		}
		select {
		case err = <-b.done:
			b.closeLocked()
			return fmt.Errorf("ChromeDriver exited: %v", err)
		case <-ctx.Done():
			b.closeLocked()
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		b.closeLocked()
		return errors.New("ChromeDriver did not start; check its path and version")
	}
	args := []string{"--no-first-run", "--no-default-browser-check", "--disable-dev-shm-usage", "--window-size=1360,900", "--user-data-dir=" + filepath.Join(b.dir, "browser-profile")}
	if c.Headless {
		args = append(args, "--headless=new")
	}
	opts := map[string]any{"binary": binary, "args": args, "prefs": map[string]any{"download.default_directory": filepath.Join(b.dir, "downloads"), "download.prompt_for_download": false}}
	body := map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{"browserName": "chrome", "goog:chromeOptions": opts, "pageLoadStrategy": "eager"}}}
	raw, err := b.command(ctx, http.MethodPost, "/session", body)
	if err != nil {
		b.closeLocked()
		return err
	}
	var session struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &session)
	if session.SessionID == "" {
		b.closeLocked()
		return errors.New("ChromeDriver did not create a session")
	}
	b.session = session.SessionID
	_, err = b.command(ctx, http.MethodPost, "/session/"+b.session+"/timeouts", map[string]int{"pageLoad": 30000, "script": 15000, "implicit": 0})
	if err != nil {
		b.closeLocked()
		return err
	}
	return nil
}

func (b *Browser) closeLocked() {
	if b.session != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = b.command(ctx, http.MethodDelete, "/session/"+b.session, nil)
		cancel()
	}
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
	if b.logFile != nil {
		_ = b.logFile.Close()
	}
	b.session = ""
	b.cmd = nil
	b.done = nil
	b.logFile = nil
}
func (b *Browser) Close() { b.mu.Lock(); defer b.mu.Unlock(); b.closeLocked() }

const snapshotScript = `
const visible = e => !!(e.getClientRects().length) && getComputedStyle(e).visibility !== 'hidden';
const elements = [...document.querySelectorAll('a,button,input,textarea,select,[role="button"],[role="link"],[contenteditable="true"]')].filter(visible).slice(0,120);
window.__liteagentElements = elements;
return {url:location.href,title:document.title,text:document.body ? document.body.innerText.slice(0,11000) : '',elements:elements.map((e,i)=>({id:String(i+1),tag:e.tagName.toLowerCase(),type:e.type||'',label:(e.getAttribute('aria-label')||e.labels?.[0]?.innerText||e.innerText||e.placeholder||e.name||e.title||'').trim().slice(0,180),disabled:!!e.disabled,options:e.tagName==='SELECT'?[...e.options].slice(0,30).map(o=>({value:o.value,text:o.text})):undefined})),frames:document.querySelectorAll('iframe,frame').length};`

func (b *Browser) execute(ctx context.Context, script string, args []any) (json.RawMessage, error) {
	if args == nil {
		args = []any{}
	}
	return b.command(ctx, http.MethodPost, "/session/"+b.session+"/execute/sync", map[string]any{"script": script, "args": args})
}

func (b *Browser) Read(ctx context.Context) (string, error) {
	raw, err := b.execute(ctx, snapshotScript, nil)
	return string(raw), err
}

func (b *Browser) element(ctx context.Context, id string) (string, error) {
	raw, err := b.execute(ctx, `const n=Number(arguments[0]); const e=window.__liteagentElements?.[n-1]; if(!Number.isInteger(n)||n<1||!e?.isConnected) throw Error('Read the page again; the element is no longer available'); return e;`, []any{id})
	if err != nil {
		return "", err
	}
	var value map[string]string
	if err = json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	key := value["element-6066-11e4-a52e-4f735466cecf"]
	if key == "" {
		return "", errors.New("element unavailable; read the page again")
	}
	return key, nil
}

func (b *Browser) Tool(ctx context.Context, name string, args map[string]any) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session == "" {
		return "", errors.New("open the browser first")
	}
	base := "/session/" + b.session
	str := func(k string) string { v, _ := args[k].(string); return v }
	var err error
	switch name {
	case "browser_open":
		u, e := url.Parse(str("url"))
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return "", errors.New("use a valid HTTP or HTTPS URL")
		}
		_, err = b.command(ctx, http.MethodPost, base+"/url", map[string]string{"url": u.String()})
	case "browser_read":
		return b.Read(ctx)
	case "browser_click", "browser_type", "browser_select":
		id, e := b.element(ctx, str("element_id"))
		if e != nil {
			return "", e
		}
		path := base + "/element/" + url.PathEscape(id)
		if name == "browser_click" {
			_, err = b.command(ctx, http.MethodPost, path+"/click", map[string]any{})
		}
		if name == "browser_type" {
			_, err = b.command(ctx, http.MethodPost, path+"/clear", map[string]any{})
			if err == nil {
				_, err = b.command(ctx, http.MethodPost, path+"/value", map[string]any{"text": str("text")})
			}
		}
		if name == "browser_select" {
			_, err = b.execute(ctx, `const e=arguments[0],v=arguments[1];if(e.tagName!=='SELECT')throw Error('The element is not a select control');const o=[...e.options].find(o=>o.value===v||o.text===v);if(!o)throw Error('Option not found');e.value=o.value;e.dispatchEvent(new Event('input',{bubbles:true}));e.dispatchEvent(new Event('change',{bubbles:true}));return e.value;`, []any{map[string]string{"element-6066-11e4-a52e-4f735466cecf": id}, str("value")})
		}
	case "browser_press":
		keys := map[string]string{"Enter": "\ue007", "Tab": "\ue004", "Escape": "\ue00c", "ArrowDown": "\ue015", "ArrowUp": "\ue013"}
		key, ok := keys[str("key")]
		if !ok {
			return "", errors.New("unsupported key")
		}
		id, e := b.element(ctx, str("element_id"))
		if e != nil {
			return "", e
		}
		_, err = b.command(ctx, http.MethodPost, base+"/element/"+url.PathEscape(id)+"/value", map[string]string{"text": key})
	case "browser_scroll":
		direction := str("direction")
		dy := 650
		if direction == "up" {
			dy = -650
		} else if direction != "down" {
			return "", errors.New("direction must be up or down")
		}
		_, err = b.execute(ctx, "window.scrollBy(0,arguments[0]);return true;", []any{dy})
	case "browser_wait":
		seconds, _ := args["seconds"].(float64)
		if seconds < 0.1 || seconds > 10 {
			return "", errors.New("wait between 0.1 and 10 seconds")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(seconds * float64(time.Second))):
		}
	case "browser_tabs":
		raw, e := b.command(ctx, http.MethodGet, base+"/window/handles", nil)
		return string(raw), e
	case "browser_switch":
		_, err = b.command(ctx, http.MethodPost, base+"/window", map[string]string{"handle": str("tab_id")})
	case "browser_new_tab":
		raw, e := b.command(ctx, http.MethodPost, base+"/window/new", map[string]string{"type": "tab"})
		if e != nil {
			return "", e
		}
		var v struct {
			Handle string `json:"handle"`
		}
		_ = json.Unmarshal(raw, &v)
		_, err = b.command(ctx, http.MethodPost, base+"/window", map[string]string{"handle": v.Handle})
	case "browser_frame":
		id := str("frame_index")
		if id == "main" {
			_, err = b.command(ctx, http.MethodPost, base+"/frame", map[string]any{"id": nil})
		} else {
			var index int
			if _, e := fmt.Sscanf(id, "%d", &index); e != nil || index < 0 {
				return "", errors.New("enter a frame index or main")
			}
			_, err = b.command(ctx, http.MethodPost, base+"/frame", map[string]int{"id": index})
		}
	case "browser_dialog":
		action := str("action")
		if action != "accept" && action != "dismiss" {
			return "", errors.New("invalid dialog action")
		}
		_, err = b.command(ctx, http.MethodPost, base+"/alert/"+action, map[string]any{})
	case "browser_back":
		_, err = b.command(ctx, http.MethodPost, base+"/back", map[string]any{})
	case "browser_downloads":
		entries, e := os.ReadDir(filepath.Join(b.dir, "downloads"))
		if e != nil {
			return "", e
		}
		list := []map[string]any{}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, e := entry.Info()
			if e != nil {
				continue
			}
			list = append(list, map[string]any{"name": entry.Name(), "bytes": info.Size(), "complete": !strings.HasSuffix(entry.Name(), ".crdownload"), "modified": info.ModTime().UTC().Format(time.RFC3339)})
		}
		raw, _ := json.Marshal(list)
		return string(raw), nil
	default:
		return "", errors.New("unknown tool")
	}
	if err != nil {
		return "", err
	}
	return b.Read(ctx)
}
