package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkspaceUI(t *testing.T) {
	if os.Getenv("LITEAGENT_BROWSER_TEST") != "1" {
		t.Skip("set LITEAGENT_BROWSER_TEST=1 for real Chrome UI")
	}
	a := newTestApp(t)
	var modelListMode atomic.Int32
	quit := make(chan struct{}, 1)
	a.shutdown = func() { quit <- struct{}{} }
	slowStarted, slowCancelled := make(chan struct{}), make(chan struct{})
	replyStarted, replyCancelled, releaseReply := make(chan struct{}), make(chan struct{}), make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			_, _ = io.Copy(io.Discard, r.Body)
			close(replyStarted)
			select {
			case <-r.Context().Done():
				close(replyCancelled)
			case <-releaseReply:
			}
		case "/api/tags":
			switch modelListMode.Load() {
			case 1:
				http.Error(w, "Ollama unavailable", 503)
			case 2:
				fmt.Fprint(w, `{"models":[]}`)
			case 3:
				fmt.Fprint(w, `{"models":[{"name":"hf.co/Qwen/Qwen3-8B-GGUF:Q4_K_M"}]}`)
			default:
				fmt.Fprint(w, `{"models":[{"name":"qwen3:8b"},{"name":"qwen3:0.6b"}]}`)
			}
		case "/models":
			if r.Header.Get("Authorization") != "Bearer ui-secret" {
				http.Error(w, "missing test credential", 401)
				return
			}
			fmt.Fprint(w, `{"data":[{"id":"test-model"},{"id":"test-agent"}]}`)
		case "/empty/models":
			fmt.Fprint(w, `{"data":[]}`)
		case "/vscode/models":
			if r.Header.Get("Authorization") != "Bearer bridge-fixture-secret" {
				http.Error(w, "missing bridge token", 401)
				return
			}
			fmt.Fprint(w, `{"data":[{"id":"fixture/text"},{"id":"fixture/tool"}]}`)
		case "/slow/models":
			close(slowStarted)
			select {
			case <-r.Context().Done():
				close(slowCancelled)
			case <-time.After(5 * time.Second):
				fmt.Fprint(w, `{"data":[{"id":"stale-model"}]}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() { close(releaseReply); model.Close() }()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	browser := &Browser{dir: t.TempDir()}
	defer browser.Close()
	config := defaultConfig()
	config.Headless = true
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if cache := os.Getenv("LITEAGENT_TEST_DATA"); cache != "" {
		var err error
		config.DriverPath, _, err = ensureDriver(ctx, config, cache)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := browser.Start(ctx, config); err != nil {
		t.Fatal(err)
	}
	script := func(js string) json.RawMessage {
		t.Helper()
		raw, err := browser.execute(ctx, js, nil)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	click := func(selector string) {
		t.Helper()
		raw, err := browser.command(ctx, "POST", "/session/"+browser.session+"/element", map[string]string{"using": "css selector", "value": selector})
		if err != nil {
			t.Fatal(err)
		}
		var element map[string]string
		if err = json.Unmarshal(raw, &element); err != nil {
			t.Fatal(err)
		}
		if _, err = browser.command(ctx, "POST", "/session/"+browser.session+"/element/"+element["element-6066-11e4-a52e-4f735466cecf"]+"/click", map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	await := func(js string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if string(script("return Boolean("+js+")")) == "true" {
				return
			}
			time.Sleep(80 * time.Millisecond)
		}
		t.Fatalf("UI condition timed out: %s\n%s", js, script("return document.body.innerText"))
	}
	if _, err := browser.Tool(ctx, "browser_open", map[string]any{"url": server.URL}); err != nil {
		t.Fatal(err)
	}
	script("window.uiErrors=[]; window.addEventListener('error',e=>uiErrors.push(e.message)); window.addEventListener('unhandledrejection',e=>uiErrors.push(String(e.reason)))")
	await("document.getElementById('connection').textContent==='Connected locally'")
	if string(script("return document.getElementById('model-description').getBoundingClientRect().height")) != "0" {
		t.Fatal("model description occupies chat")
	}
	script("document.getElementById('help').click()")
	await("document.getElementById('commands-dialog').open && document.querySelectorAll('.palette-item').length>10")
	if len(a.store.Snapshot().Runs) != 0 {
		t.Fatal("command palette polluted chat")
	}
	script("document.getElementById('command-search').value='skills';document.getElementById('command-search').dispatchEvent(new Event('input'));document.querySelector('.palette-item').click()")
	await("document.getElementById('settings-dialog').open && !document.getElementById('panel-skills').hidden")
	script("document.getElementById('new-skill').click();document.getElementById('skill-name').value='UI procedure';document.getElementById('skill-instructions').value='Read the report and verify the date.';document.getElementById('skill-form').requestSubmit()")
	await("document.getElementById('skills-feedback').textContent==='Skill saved.'")
	if len(a.store.Snapshot().Skills) != 1 {
		t.Fatal("skill UI did not persist")
	}
	script("document.querySelector('[data-settings-tab=generation]').click()")
	if string(script("return document.getElementById('setting-timeout_sec').value")) != `"900"` {
		t.Fatal("settings did not show the default 900-second request timeout")
	}
	script("document.getElementById('setting-temperature').value='0.3';document.getElementById('setting-timeout_sec').value='1200';document.getElementById('settings-form').requestSubmit()")
	await("!document.getElementById('settings-dialog').open")
	if value := a.store.Snapshot().Config.Temperature; value == nil || *value != 0.3 {
		t.Fatal("settings were not saved")
	}
	if a.store.Snapshot().Config.TimeoutSec != 1200 {
		t.Fatal("settings did not save a request timeout above 600 seconds")
	}
	script("document.getElementById('show-settings').click()")
	await("document.getElementById('settings-dialog').open && document.getElementById('setting-timeout_sec').value==='1200'")
	script("document.getElementById('settings-dialog').close()")
	script("document.getElementById('model-menu-toggle').click()")
	await("!document.getElementById('model-menu').hidden && !document.getElementById('model-select').disabled")
	script("document.getElementById('model-select').value='qwen3:0.6b';document.getElementById('model-select').dispatchEvent(new Event('change'))")
	await("document.getElementById('composer-model').textContent==='Qwen3 · 0.6B' && !document.getElementById('reasoning-select').disabled")
	if a.store.Snapshot().Config.Model != "qwen3:0.6b" {
		t.Fatal("composer model selection was not saved")
	}
	script("document.getElementById('reasoning-select').value='off';document.getElementById('reasoning-select').dispatchEvent(new Event('change'))")
	await("document.getElementById('model-feedback').textContent.includes('Reasoning saved') && document.getElementById('composer-reasoning').textContent==='Off'")
	if a.store.Snapshot().Config.reasoningMode() != "off" {
		t.Fatal("reasoning picker failed")
	}
	script("document.getElementById('about-model').click()")
	await("document.getElementById('model-dialog').open")
	script("document.getElementById('model-dialog').close();document.getElementById('message').value='/sta';document.getElementById('message').dispatchEvent(new Event('input'))")
	await("!document.getElementById('command-menu').hidden")
	script("document.getElementById('message').dispatchEvent(new KeyboardEvent('keydown',{key:'Tab',bubbles:true}))")
	await("document.getElementById('message').value==='/status '")
	script("document.getElementById('chat-form').requestSubmit()")
	await("document.getElementById('status-dialog').open")
	if len(a.store.Snapshot().Runs) != 0 {
		t.Fatal("UI-only commands were sent as messages")
	}
	script("document.getElementById('status-dialog').close();document.getElementById('show-schedules').click();document.getElementById('schedule-editor').open=true;document.getElementById('schedule-title').value='UI reminder';document.getElementById('schedule-kind').value='reminder';document.getElementById('schedule-kind').dispatchEvent(new Event('change'));document.getElementById('schedule-form').requestSubmit()")
	await("document.getElementById('schedules-feedback').textContent==='Schedule created.'")
	if len(a.store.Snapshot().Schedules) != 1 {
		t.Fatal("schedule form did not persist")
	}
	script("document.getElementById('settings-dialog').close();document.getElementById('show-settings').click();document.querySelector('[data-settings-tab=connections]').click();document.getElementById('new-connection').click()")
	setURL := func(suffix string) {
		t.Helper()
		script(fmt.Sprintf("document.getElementById('connection-url').value=%q;document.getElementById('connection-url').dispatchEvent(new Event('input'));document.getElementById('connection-url').dispatchEvent(new Event('change'))", model.URL+suffix))
	}
	setURL("")
	await("document.getElementById('connection-model-feedback').textContent.includes('2 models loaded') && document.getElementById('connection-model-select').value==='qwen3:8b'")
	if len(a.store.Snapshot().Connections) != 1 {
		t.Fatal("model discovery saved an unfinished connection")
	}
	script("document.getElementById('connection-name').value='UI OpenAI';document.getElementById('connection-provider').value='openai-responses';document.getElementById('connection-provider').dispatchEvent(new Event('change'))")
	await("document.getElementById('connection-model-feedback').textContent.includes('Add your API key') && document.getElementById('connection-model').value===''")
	setURL("")
	script("document.getElementById('connection-key').value='ui-secret';document.getElementById('connection-key').dispatchEvent(new Event('input'))")
	await("document.getElementById('connection-model-feedback').textContent.includes('2 models loaded')")
	script("document.getElementById('connection-model-select').value='test-agent';document.getElementById('connection-model-select').dispatchEvent(new Event('change'));document.getElementById('connection-activate').checked=false;document.getElementById('connection-form').requestSubmit()")
	await("document.getElementById('connections-feedback').textContent==='Connection saved.'")
	saved := a.store.Snapshot().Connections
	if len(saved) != 2 || saved[1].Model != "test-agent" || saved[1].APIKey != "ui-secret" || saved[1].Provider != "openai-responses" {
		t.Fatal("model selection or pasted API key did not survive changing providers and saving")
	}
	if string(script("return document.body.innerText.includes('ui-secret') || document.getElementById('connection-key').value!==''")) != "false" {
		t.Fatal("UI retained plaintext credential")
	}
	script("[...document.querySelectorAll('#connections-list .management-card')].find(c=>c.querySelector('strong').textContent==='UI OpenAI').querySelector('button').click()")
	await("document.getElementById('connection-model-feedback').textContent.includes('2 models loaded') && document.getElementById('connection-model-select').value==='test-agent'")
	setURL("/slow")
	select {
	case <-slowStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("draft model discovery did not start")
	}
	setURL("")
	await("document.getElementById('connection-model-feedback').textContent.includes('2 models loaded')")
	select {
	case <-slowCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("changing the provider URL did not cancel stale discovery")
	}
	if string(script("return [...document.getElementById('connection-model-select').options].some(o=>o.value==='stale-model')")) != "false" {
		t.Fatal("a stale provider response replaced the current models")
	}
	setURL("/empty")
	await("document.getElementById('connection-model-feedback').textContent.includes('returned no models')")
	script("document.getElementById('connection-model-select').value='__manual__';document.getElementById('connection-model-select').dispatchEvent(new Event('change'));document.getElementById('connection-model').value='private-model';document.getElementById('connection-fetch-models').click()")
	await("document.getElementById('connection-model-feedback').textContent.includes('returned no models') && !document.getElementById('connection-fetch-models').disabled")
	if string(script("return !document.getElementById('connection-custom-model').hidden && document.getElementById('connection-model').value==='private-model'")) != "true" {
		t.Fatal("refresh replaced the manually entered model")
	}
	script("document.getElementById('cancel-connection').click()")
	script("document.querySelector('[data-settings-tab=mcp]').click();document.getElementById('new-mcp').click();document.getElementById('mcp-name').value='Local MCP';document.getElementById('mcp-url').value='http://127.0.0.1:9999/mcp';document.getElementById('mcp-enabled').checked=false;document.getElementById('mcp-form').requestSubmit()")
	await("document.getElementById('mcp-feedback').textContent.includes('MCP server saved')")
	if len(a.store.Snapshot().MCPServers) != 1 {
		t.Fatal("MCP form did not persist")
	}
	script("document.querySelector('[data-settings-tab=connections]').click();document.getElementById('new-connection').click();document.getElementById('connection-provider').value='vscode';document.getElementById('connection-provider').dispatchEvent(new Event('change'))")
	await("!document.getElementById('connection-vscode-help').hidden && document.getElementById('connection-url').value==='http://127.0.0.1:8788/v1' && document.getElementById('connection-model-feedback').textContent.includes('bridge token')")
	setURL("/vscode")
	script("document.getElementById('connection-name').value='UI VS Code';document.getElementById('connection-key').value='bridge-fixture-secret';document.getElementById('connection-key').dispatchEvent(new Event('input'))")
	await("document.getElementById('connection-model-feedback').textContent.includes('2 models loaded')")
	script("document.getElementById('connection-model-select').value='fixture/text';document.getElementById('connection-model-select').dispatchEvent(new Event('change'));document.getElementById('connection-activate').checked=false;document.getElementById('connection-form').requestSubmit()")
	await("document.getElementById('connection-form').hidden")
	bridgeConnection := a.store.Snapshot().Connections[2]
	if bridgeConnection.Provider != "vscode" || bridgeConnection.Model != "fixture/text" || bridgeConnection.APIKey != "bridge-fixture-secret" {
		t.Fatal("VS Code connection did not persist")
	}
	// Browser permission prompts and audio devices are stubbed; the actual reminder,
	// API polling, preferences, sound scheduling and navigation use the real UI.
	script(`window.testAlerts=[];window.testSounds=0;
window.Notification=class {static permission='default';static requestPermission(){this.permission='granted';return Promise.resolve('granted')} constructor(title,options){this.title=title;this.options=options;testAlerts.push(this)} close(){this.closed=true}};
window.AudioContext=class {state='running';currentTime=0;destination={};resume(){return Promise.resolve()} createOscillator(){return {frequency:{},connect(){},disconnect(){},start(){testSounds++},stop(){}}} createGain(){return {gain:{setValueAtTime(){},exponentialRampToValueAtTime(){}},connect(){},disconnect(){}}}};
notificationAudio=null;document.querySelector('[data-settings-tab=notifications]').click();document.getElementById('enable-notifications').click()`)
	await("document.getElementById('notification-permission').textContent==='Enabled'")
	script("document.getElementById('test-notification').click()")
	await("testAlerts.length===1 && testSounds===2 && document.querySelectorAll('.notification-toast').length===1")
	script("testAlerts=[];testSounds=0;lastChimeAt=0;[...document.querySelectorAll('.notification-toast')].forEach(n=>n.dismissNotification());document.querySelector('[data-settings-tab=schedules]').click()")
	await("document.getElementById('schedule-summary').textContent.includes('1 total') && document.querySelectorAll('#schedule-list .schedule-card').length===1")
	script("[...document.querySelectorAll('#schedule-list button')].find(b=>b.textContent==='Run now').click()")
	await("testAlerts.length===1 && testSounds===2 && testAlerts[0].options.body==='UI reminder'")
	script("window.alertRefreshDone=false;refresh().then(()=>refresh()).then(()=>window.alertRefreshDone=true)")
	await("window.alertRefreshDone")
	if string(script("return testAlerts.length===1 && testSounds===2")) != "true" {
		t.Fatal("polling repeated the same notification")
	}
	script("testAlerts[0].onclick()")
	await("!document.getElementById('settings-dialog').open && document.getElementById('messages').textContent.includes('UI reminder')")
	script("document.getElementById('show-settings').click();document.querySelector('[data-settings-tab=notifications]').click();document.getElementById('notification-sound').click();testSounds=0;lastChimeAt=0;document.querySelector('[data-settings-tab=schedules]').click();[...document.querySelectorAll('#schedule-list button')].find(b=>b.textContent==='Run now').click()")
	await("testAlerts.length===2")
	if string(script("return testSounds===0 && JSON.parse(localStorage.getItem('liteagent-notification-settings')).sound===false")) != "true" {
		t.Fatal("muting sound did not persist or prevent playback")
	}
	script("[...document.querySelectorAll('#schedule-list button')].find(b=>b.textContent==='Pause').click()")
	await("document.getElementById('schedule-summary').textContent.includes('0 active')")
	script("document.getElementById('schedule-filter').value='active';document.getElementById('schedule-filter').dispatchEvent(new Event('change'))")
	await("document.getElementById('schedule-list').textContent.includes('No schedules match')")
	script("document.getElementById('schedule-filter').value='paused';document.getElementById('schedule-filter').dispatchEvent(new Event('change'));[...document.querySelectorAll('#schedule-list button')].find(b=>b.textContent==='Resume').click()")
	await("document.getElementById('schedule-summary').textContent.includes('1 active')")
	script("document.getElementById('schedule-filter').value='all';document.getElementById('schedule-filter').dispatchEvent(new Event('change'));document.querySelector('[data-settings-tab=notifications]').click();Notification.permission='denied';renderNotificationSettings();document.getElementById('test-notification').click()")
	await("document.getElementById('notification-permission').textContent==='Blocked' && document.getElementById('enable-notifications').disabled")
	if string(script("return testAlerts.length===2 && testSounds===0 && document.querySelectorAll('.notification-toast').length>0")) != "true" {
		t.Fatal("blocked desktop permission broke the in-app fallback")
	}
	script("[...document.querySelectorAll('.notification-toast')].forEach(n=>n.dismissNotification());Notification.permission='granted';renderNotificationSettings()")
	script("window.batchDone=false;const batch=[{id:'concurrent-test',kind:'scheduled',title:'Finished task',text:'Task completed'}];Promise.all([notifyUpdates(batch),notifyUpdates(batch)]).then(()=>window.batchDone=true)")
	await("window.batchDone")
	if string(script("return testAlerts.length===3")) != "true" {
		t.Fatal("concurrent notification delivery was not deduplicated")
	}
	script("document.getElementById('notification-replies').click();Object.defineProperty(document,'hidden',{configurable:true,value:true});window.replyCheckDone=false;notifyUpdates([{id:'muted-reply-test',kind:'reply',title:'Reply ready',text:'Hello'}]).then(()=>window.replyCheckDone=true)")
	await("window.replyCheckDone")
	if string(script("return testAlerts.length===3")) != "true" {
		t.Fatal("disabled background reply alerts were delivered")
	}
	script("document.getElementById('notification-replies').click();window.replyCheckDone=false;notifyUpdates([{id:'enabled-reply-test',kind:'reply',title:'Reply ready',text:'Hello'}]).then(()=>window.replyCheckDone=true)")
	await("window.replyCheckDone && testAlerts.length===4")
	script("delete document.hidden;[...document.querySelectorAll('.notification-toast')].forEach(n=>n.dismissNotification())")
	screenshot := func(name string) {
		if dir := os.Getenv("LITEAGENT_SCREENSHOT_DIR"); dir != "" {
			raw, err := browser.command(ctx, "GET", "/session/"+browser.session+"/screenshot", nil)
			if err != nil {
				t.Fatal(err)
			}
			var data string
			_ = json.Unmarshal(raw, &data)
			image, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, name), image, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, _ = browser.command(ctx, "POST", "/session/"+browser.session+"/window/rect", map[string]int{"width": 1440, "height": 1000})
	screenshot("notifications-desktop.png")
	script("document.getElementById('settings-dialog').close()")
	localConfig := a.store.Snapshot().Config
	if w := callAPI(t, a, "POST", "/api/connections/"+saved[1].ID, map[string]string{"action": "activate"}, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	script("refresh()")
	await("document.getElementById('composer-model').textContent==='test-agent' && !document.getElementById('refresh-models').disabled")
	screenshot("composer-desktop.png")
	script("document.getElementById('model-menu-toggle').click()")
	await("!document.getElementById('model-menu').hidden")
	if string(script("return document.getElementById('about-model').hidden && document.getElementById('model-description').hidden && [...document.getElementById('model-select').options].every(o=>o.textContent===o.value) && !document.body.textContent.includes('Profile not available')")) != "true" {
		t.Fatal("API models showed an unavailable profile")
	}
	script("document.getElementById('reasoning-select').value='xhigh';document.getElementById('reasoning-select').dispatchEvent(new Event('change'))")
	await("document.getElementById('composer-reasoning').textContent==='Extra high' && !document.getElementById('model-menu-toggle').disabled")
	screenshot("composer-model-menu-desktop.png")
	script("document.getElementById('reasoning-select').dispatchEvent(new KeyboardEvent('keydown',{key:'Escape',bubbles:true}))")
	await("document.getElementById('model-menu').hidden && document.activeElement.id==='model-menu-toggle'")
	for _, width := range []int{390, 320} {
		_, _ = browser.command(ctx, "POST", "/session/"+browser.session+"/window/rect", map[string]int{"width": width, "height": 844})
		script("document.getElementById('model-menu-toggle').click()")
		if string(script(`const form=document.getElementById('chat-form').getBoundingClientRect(), menu=document.getElementById('model-menu').getBoundingClientRect(), trigger=document.getElementById('model-menu-toggle').getBoundingClientRect(), send=document.getElementById('send').getBoundingClientRect(), message=document.getElementById('message').getBoundingClientRect();return document.documentElement.scrollWidth<=innerWidth && menu.left>=0 && menu.right<=innerWidth && menu.top>=0 && trigger.top>=message.bottom && trigger.right<=send.left && send.right<=form.right && form.bottom<=innerHeight`)) != "true" {
			t.Fatal("composer controls overlap or overflow on mobile", width)
		}
		screenshot(fmt.Sprintf("composer-menu-%d.png", width))
		click("#message")
		await("document.getElementById('model-menu').hidden")
		screenshot(fmt.Sprintf("composer-%d.png", width))
	}
	if w := callAPI(t, a, "POST", "/api/connections/"+bridgeConnection.ID, map[string]string{"action": "activate"}, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	script("refresh()")
	await("document.getElementById('composer-model').textContent==='fixture/text' && document.getElementById('composer-reasoning').textContent==='Auto' && !modelsLoading")
	_, _ = browser.command(ctx, "POST", "/session/"+browser.session+"/window/rect", map[string]int{"width": 1440, "height": 1000})
	script("document.getElementById('show-settings').click()")
	if string(script("return document.getElementById('setting-temperature').disabled && document.getElementById('setting-max_tokens').disabled && document.getElementById('reasoning-select').options.length===1")) != "true" {
		t.Fatal("VS Code exposed unsupported generation overrides")
	}
	script("document.querySelector('[data-settings-tab=connections]').click();[...document.querySelectorAll('#connections-list .management-card')].find(c=>c.querySelector('strong').textContent==='UI VS Code').querySelector('button').click()")
	await("document.getElementById('connection-model-feedback').textContent.includes('2 models loaded') && !document.getElementById('connection-vscode-help').hidden && document.getElementById('connection-key').value===''")
	screenshot("vscode-bridge-settings.png")
	script("document.getElementById('settings-dialog').close()")
	if w := callAPI(t, a, "PUT", "/api/config", localConfig, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	script("refresh()")
	await("document.getElementById('composer-model').textContent==='Qwen3 · 0.6B' && !document.getElementById('model-select').disabled")
	_, _ = browser.command(ctx, "POST", "/session/"+browser.session+"/window/rect", map[string]int{"width": 1440, "height": 1000})
	script("document.getElementById('show-settings').click()")
	script("document.querySelector('[data-settings-tab=schedules]').click()")
	screenshot("schedules-desktop.png")
	screenshot("settings-desktop.png")
	script("document.getElementById('settings-dialog').close();document.getElementById('help').click()")
	screenshot("commands-desktop.png")
	_, _ = browser.command(ctx, "POST", "/session/"+browser.session+"/window/rect", map[string]int{"width": 390, "height": 844})
	screenshot("commands-mobile.png")
	script("document.getElementById('commands-dialog').close();document.getElementById('show-settings').click();document.querySelector('[data-settings-tab=mcp]').click()")
	if string(script("return document.documentElement.scrollWidth <= window.innerWidth && document.getElementById('settings-dialog').scrollWidth <= document.getElementById('settings-dialog').clientWidth")) != "true" {
		t.Fatal("mobile horizontal overflow")
	}
	screenshot("settings-mobile.png")
	for _, tab := range []string{"notifications", "schedules"} {
		script(fmt.Sprintf("document.querySelector('[data-settings-tab=%s]').click()", tab))
		if string(script("return document.getElementById('settings-dialog').scrollWidth <= document.getElementById('settings-dialog').clientWidth")) != "true" {
			t.Fatal("mobile settings overflow", tab)
		}
		screenshot(tab + "-mobile.png")
	}
	if raw := script("return window.uiErrors"); string(raw) != "[]" {
		t.Fatalf("UI errors: %s", raw)
	}
	if _, err := browser.Tool(ctx, "browser_open", map[string]any{"url": server.URL}); err != nil {
		t.Fatal(err)
	}
	await("document.getElementById('connection').textContent==='Connected locally'")
	script("document.getElementById('show-settings').click();document.querySelector('[data-settings-tab=notifications]').click()")
	if string(script("return !document.getElementById('notification-sound').checked && document.querySelectorAll('.notification-toast').length===0")) != "true" {
		t.Fatal("notification preferences or seen alerts did not survive reload")
	}
	script("document.getElementById('settings-dialog').close();document.getElementById('new-chat').click()")
	// Installed models can appear and disappear without changing the connection.
	modelListMode.Store(1)
	script("loadModels()")
	await("!document.getElementById('ollama-setup').hidden && document.getElementById('composer-model').textContent==='Connect Ollama' && !modelsLoading")
	if string(script("return document.getElementById('send').disabled && document.getElementById('model-select').value==='' && [...document.getElementById('model-select').options].every(o=>o.value==='')")) != "true" {
		t.Fatal("an unavailable Ollama model was presented as usable")
	}
	script("document.getElementById('message').value='/status';document.getElementById('chat-form').requestSubmit()")
	await("document.getElementById('status-dialog').open")
	script("document.getElementById('status-dialog').close()")
	_, _ = browser.command(ctx, "POST", "/session/"+browser.session+"/window/rect", map[string]int{"width": 1440, "height": 1000})
	screenshot("ollama-setup-desktop.png")
	modelListMode.Store(2)
	script("document.getElementById('check-ollama').click()")
	await("document.getElementById('ollama-setup-title').textContent==='Download your first model' && !modelsLoading")
	_, _ = browser.command(ctx, "POST", "/session/"+browser.session+"/window/rect", map[string]int{"width": 390, "height": 844})
	if string(script("return document.documentElement.scrollWidth<=innerWidth && document.getElementById('send').disabled")) != "true" {
		t.Fatal("empty-model setup overflowed or enabled sending")
	}
	screenshot("ollama-empty-mobile.png")
	modelListMode.Store(3)
	// Advance the refresh clock; polling must discover the installation itself.
	script("lastModelsCheck=0;refresh()")
	await("document.getElementById('ollama-setup-title').textContent==='Choose an installed model' && !modelsLoading")
	script("document.getElementById('choose-ollama').click()")
	await("!document.getElementById('model-menu').hidden")
	script("document.getElementById('model-select').value='hf.co/Qwen/Qwen3-8B-GGUF:Q4_K_M';document.getElementById('model-select').dispatchEvent(new Event('change'))")
	await("document.getElementById('ollama-setup').hidden && !document.getElementById('send').disabled && document.getElementById('composer-model').textContent==='hf.co/Qwen/Qwen3-8B-GGUF:Q4_K_M'")
	if a.store.Snapshot().Config.Model != "hf.co/Qwen/Qwen3-8B-GGUF:Q4_K_M" {
		t.Fatal("discovery did not preserve the exact installed model ID")
	}
	modelListMode.Store(0)
	if w := callAPI(t, a, "PUT", "/api/config", localConfig, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	script("document.getElementById('close-model-menu').click();refresh().then(()=>loadModels())")
	await("document.getElementById('composer-model').textContent==='Qwen3 · 0.6B' && !modelsLoading && !document.getElementById('send').disabled")
	script("document.getElementById('message').value='UI active reply';document.getElementById('chat-form').requestSubmit()")
	waitChatSignal(t, replyStarted)
	await("!document.getElementById('stop').hidden && [...document.querySelectorAll('.history-item')].some(b=>b.title==='UI active reply')")
	script("document.getElementById('model-menu-toggle').click()")
	if string(script("return !document.getElementById('model-menu').hidden && document.getElementById('model-select').disabled && document.getElementById('reasoning-select').disabled && document.getElementById('send').querySelector('svg')!==null")) != "true" {
		t.Fatal("composer controls did not preserve the active reply state")
	}
	script("document.getElementById('close-model-menu').click()")
	script("window.confirm=question=>{window.deleteQuestion=question;return true};[...document.querySelectorAll('.history-item')].find(b=>b.title==='UI reminder').parentElement.querySelector('.delete-chat').click()")
	await("![...document.querySelectorAll('.history-item')].some(b=>b.title==='UI reminder') && document.getElementById('chat-feedback').textContent==='Conversation deleted.'")
	if len(a.store.Snapshot().Schedules) != 0 {
		t.Fatal("deleting another chat did not remove its schedules")
	}
	select {
	case <-replyCancelled:
		t.Fatal("deleting another chat from the UI stopped the current reply")
	default:
	}
	a.browser.mu.Lock()
	cleanupLocked := true
	defer func() {
		if cleanupLocked {
			a.browser.mu.Unlock()
		}
	}()
	script("[...document.querySelectorAll('.history-item')].find(b=>b.title==='UI active reply').parentElement.querySelector('.delete-chat').click()")
	waitChatSignal(t, replyCancelled)
	await("window.deleteQuestion.startsWith('Stop this reply') && document.querySelector('.delete-chat').disabled && document.getElementById('chat-feedback').textContent.includes('Stopping the reply')")
	screenshot("deleting-active-chat.png")
	a.browser.mu.Unlock()
	cleanupLocked = false
	await("document.querySelectorAll('.chat-history-row').length===0 && document.getElementById('stop').hidden && !new URL(location.href).searchParams.has('chat')")
	a.wg.Wait()
	if st := a.store.Snapshot(); len(st.Chats) != 0 || len(st.Runs) != 0 || len(st.Schedules) != 0 {
		t.Fatal("deleting the active chat left records behind")
	}
	script("document.getElementById('quit-app').click()")
	await("document.getElementById('connection').textContent==='LiteAgent stopped' && document.getElementById('send').disabled")
	select {
	case <-quit:
	default:
		t.Fatal("Quit did not reach the server")
	}
	t.Log("Real Chrome: composer, settings, notifications, schedules, chat deletion, missing/empty/installed Ollama models, automatic discovery and Quit")
}
