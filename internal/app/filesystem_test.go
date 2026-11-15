package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestFilesystemStableDirectoryPagesAndMetadata(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	for _, name := range []string{"z.txt", "a.txt", "é.txt"} {
		fileTool(t, a, "filesystem_write", map[string]any{"path": "files/" + name, "content": name})
	}
	fileTool(t, a, "filesystem_mkdir", map[string]any{"path": "files/folder"})
	first := fileTool(t, a, "filesystem_list", map[string]any{"path": "files", "limit": 2})
	entries := first["entries"].([]any)
	if first["total_entries"] != float64(4) || entries[0].(map[string]any)["name"] != "folder" || entries[1].(map[string]any)["name"] != "a.txt" {
		t.Fatal("directory pages are not sorted directories-first", first)
	}
	second := fileTool(t, a, "filesystem_list", map[string]any{"path": "files", "limit": 2, "offset": first["next_offset"], "expected_snapshot": first["snapshot_sha256"]})
	entries = second["entries"].([]any)
	if second["truncated"] != false || entries[0].(map[string]any)["name"] != "z.txt" || entries[1].(map[string]any)["name"] != "é.txt" {
		t.Fatal("directory continuation lost the expected order")
	}
	info := fileTool(t, a, "filesystem_stat", map[string]any{"path": "files/a.txt"})
	if info["type"] != "file" || info["size_bytes"] != float64(5) || info["modified_at"] == "" || info["permissions"] == "" {
		t.Fatal("file metadata is incomplete", info)
	}
	fileTool(t, a, "filesystem_write", map[string]any{"path": "files/b.txt", "content": "new"})
	if _, err := a.filesystemTool(context.Background(), "filesystem_list", map[string]any{"path": "files", "offset": first["next_offset"], "expected_snapshot": first["snapshot_sha256"]}); err == nil {
		t.Fatal("directory pagination silently mixed snapshots")
	}
	empty := fileTool(t, a, "filesystem_list", map[string]any{"path": "files/folder"})
	if len(empty["entries"].([]any)) != 0 || empty["truncated"] != false {
		t.Fatal("empty directory listing should be complete")
	}
}

func TestFilesystemReadRejectsChangedPage(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	fileTool(t, a, "filesystem_write", map[string]any{"path": "page.txt", "content": "Hola 🌎"})
	first := fileTool(t, a, "filesystem_read", map[string]any{"path": "page.txt", "limit": 5})
	page := fileTool(t, a, "filesystem_read", map[string]any{"path": "page.txt", "offset": first["next_offset"], "expected_sha256": strings.ToUpper(first["sha256"].(string))})
	if page["content"] != "🌎" {
		t.Fatal("read offsets must preserve Unicode characters")
	}
	fileTool(t, a, "filesystem_write", map[string]any{"path": "page.txt", "content": "Changed", "expected_sha256": first["sha256"]})
	if _, err := a.filesystemTool(context.Background(), "filesystem_read", map[string]any{"path": "page.txt", "offset": first["next_offset"], "expected_sha256": first["sha256"]}); err == nil {
		t.Fatal("file pagination silently mixed versions")
	}
}

func TestFilesystemConcurrentEditsRejectStaleHash(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	fileTool(t, a, "filesystem_write", map[string]any{"path": "shared.txt", "content": "original"})
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, content := range []string{"first edit", "second edit"} {
		wg.Add(1)
		go func(content string) {
			defer wg.Done()
			_, err := a.filesystemTool(context.Background(), "filesystem_write", map[string]any{"path": "shared.txt", "content": content, "expected_sha256": localFileHash([]byte("original"))})
			errors <- err
		}(content)
	}
	wg.Wait()
	close(errors)
	succeeded := 0
	for err := range errors {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatal("concurrent writes using the same old hash must not both succeed")
	}
	entries, err := os.ReadDir(a.filesHome)
	if err != nil || len(entries) != 1 || entries[0].Name() != "shared.txt" {
		t.Fatal("write cleanup left temporary files", err)
	}
}

func TestFilesystemFailedUpdatesDoNotCreateFolders(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	for _, args := range []map[string]any{
		{"path": "missing/nested/file.txt", "content": "update", "expected_sha256": localFileHash([]byte("old"))},
		{"path": "missing/empty.txt"},
		{"path": "missing/invalid.txt", "content": 42.0},
	} {
		if _, err := a.filesystemTool(context.Background(), "filesystem_write", args); err == nil {
			t.Fatal("invalid update succeeded")
		}
	}
	if entries, err := os.ReadDir(a.filesHome); err != nil || len(entries) != 0 {
		t.Fatal("a rejected update created directories", err)
	}
}

func fileTool(t *testing.T, a *App, name string, args map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(args)
	if args == nil {
		raw = []byte(`{}`)
	}
	validated, err := validateToolCall(ToolCall{Function: FunctionCall{name, raw}})
	if err != nil {
		t.Fatal(err)
	}
	output, err := a.filesystemTool(context.Background(), name, validated)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if len(output) > 18000 {
		t.Fatal("file tool result exceeds the observation limit")
	}
	return result
}

func TestFilesystemCreatesReadsAndEditsPage(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	info := fileTool(t, a, "filesystem_info", nil)
	if info["home"] != a.filesHome || info["desktop"] != filepath.Join(a.filesHome, "Desktop") || info["os"] != runtime.GOOS {
		t.Fatal("wrong local environment", info)
	}
	folder := filepath.Join(info["desktop"].(string), "Mi página")
	fileTool(t, a, "filesystem_mkdir", map[string]any{"path": folder})
	path := filepath.Join(folder, "index.html")
	content := "<!doctype html><title>Mi página</title><h1>Hola 🌎</h1>"
	fileTool(t, a, "filesystem_write", map[string]any{"path": path, "content": content})
	read := fileTool(t, a, "filesystem_read", map[string]any{"path": "~/Desktop/Mi página/index.html"})
	if read["content"] != content || read["sha256"] != localFileHash([]byte(content)) {
		t.Fatal("file contents changed while saving or reading")
	}
	if _, err := a.filesystemTool(context.Background(), "filesystem_write", map[string]any{"path": path, "content": "overwrite"}); err == nil {
		t.Fatal("overwrote an existing file without a prior read")
	}
	fileTool(t, a, "filesystem_edit", map[string]any{"path": path, "old_text": "Hola 🌎", "new_text": "Listo ✅", "expected_sha256": read["sha256"]})
	if _, err := a.filesystemTool(context.Background(), "filesystem_write", map[string]any{"path": path, "content": "stale", "expected_sha256": read["sha256"]}); err == nil {
		t.Fatal("a stale read replaced a newer edit")
	}
	read = fileTool(t, a, "filesystem_read", map[string]any{"path": path})
	if read["content"] != strings.Replace(content, "Hola 🌎", "Listo ✅", 1) {
		t.Fatal("targeted edit lost unrelated content")
	}
	fileTool(t, a, "filesystem_write", map[string]any{"path": path, "content": content, "expected_sha256": read["sha256"]})
	list := fileTool(t, a, "filesystem_list", map[string]any{"path": folder})
	entries := list["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["name"] != "index.html" {
		t.Fatal("directory listing is wrong or temporary files were left behind", list)
	}
}

func TestFilesystemScopeAndSymlinks(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "original.txt")
	if err := os.WriteFile(victim, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../escape.txt", victim, "", string([]byte{0})} {
		if _, err := a.filesystemTool(context.Background(), "filesystem_write", map[string]any{"path": path, "content": "bad"}); err == nil {
			t.Fatalf("allowed an invalid path: %q", path)
		}
	}
	if err := os.Symlink(outside, filepath.Join(a.filesHome, "outside")); err != nil {
		t.Skip("symlinks unavailable", err)
	}
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"filesystem_read", map[string]any{"path": "outside/original.txt"}},
		{"filesystem_list", map[string]any{"path": "outside"}},
		{"filesystem_mkdir", map[string]any{"path": "outside/created"}},
		{"filesystem_write", map[string]any{"path": "outside/new.txt", "content": "bad"}},
	} {
		if _, err := a.filesystemTool(context.Background(), test.name, test.args); err == nil {
			t.Fatal("tool followed a symlink outside home", test.name)
		}
	}
	// Replacing an in-scope hard link must replace that entry, not truncate the target.
	if err := os.Link(victim, filepath.Join(a.filesHome, "hard-link.txt")); err != nil {
		t.Fatal(err)
	}
	fileTool(t, a, "filesystem_write", map[string]any{"path": "hard-link.txt", "content": "local edit", "expected_sha256": localFileHash([]byte("untouched"))})
	if data, err := os.ReadFile(victim); err != nil || string(data) != "untouched" {
		t.Fatal("an out-of-scope target was modified", err)
	}
	fileTool(t, a, "filesystem_mkdir", map[string]any{"path": "real"})
	if err := os.Symlink("real", filepath.Join(a.filesHome, "inside")); err != nil {
		t.Fatal(err)
	}
	fileTool(t, a, "filesystem_write", map[string]any{"path": "inside/ok.txt", "content": "inside"})
	if data, err := os.ReadFile(filepath.Join(a.filesHome, "real", "ok.txt")); err != nil || string(data) != "inside" {
		t.Fatal("in-scope symlink directory did not work", err)
	}
}

func TestFilesystemPaginationLimitsAndCancellation(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	content := strings.Repeat("🌎<hello>\n", 2000)
	fileTool(t, a, "filesystem_write", map[string]any{"path": "large.txt", "content": content})
	offset, combined := 0, ""
	for {
		page := fileTool(t, a, "filesystem_read", map[string]any{"path": "large.txt", "offset": offset, "limit": 10000})
		combined += page["content"].(string)
		next := int(page["next_offset"].(float64))
		if page["truncated"] == false {
			break
		}
		if next <= offset {
			t.Fatal("file pagination made no progress")
		}
		offset = next
	}
	if combined != content {
		t.Fatal("pagination split or lost Unicode content")
	}
	for i := range 5 {
		fileTool(t, a, "filesystem_write", map[string]any{"path": fmt.Sprintf("folder/%d.txt", i), "content": "text"})
	}
	seen := map[string]bool{}
	offset = 0
	for {
		page := fileTool(t, a, "filesystem_list", map[string]any{"path": "folder", "offset": offset, "limit": 2})
		for _, entry := range page["entries"].([]any) {
			name := entry.(map[string]any)["name"].(string)
			if seen[name] {
				t.Fatal("directory pagination repeated an entry")
			}
			seen[name] = true
		}
		if page["truncated"] == false {
			break
		}
		offset = int(page["next_offset"].(float64))
	}
	if len(seen) != 5 {
		t.Fatal("directory pagination lost entries")
	}
	for _, args := range []map[string]any{{"path": "large.txt", "limit": 10001.0}, {"path": "large.txt", "offset": 0.5}, {"path": "large.txt", "offset": -1.0}} {
		if _, err := a.filesystemTool(context.Background(), "filesystem_read", args); err == nil {
			t.Fatal("invalid file pagination was accepted")
		}
	}
	for _, content := range []string{strings.Repeat("x", maxLocalFileBytes+1), "binary\x00data"} {
		if _, err := a.filesystemTool(context.Background(), "filesystem_write", map[string]any{"path": "invalid.txt", "content": content}); err == nil {
			t.Fatal("invalid content was written")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.filesystemTool(ctx, "filesystem_write", map[string]any{"path": "cancelled/new.txt", "content": "no"}); !errors.Is(err, context.Canceled) {
		t.Fatal("write did not honor cancellation", err)
	}
	if _, err := os.Stat(filepath.Join(a.filesHome, "cancelled")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled write created a directory")
	}
}

func TestAgentCreatesDesktopPageWithFileTools(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	pagePath := filepath.Join(a.filesHome, "Desktop", "Mi página", "index.html")
	content := "<!doctype html><html lang=\"es\"><title>Prueba</title><h1>Hola</h1></html>"
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []Message `json:"messages"`
			Tools    []ToolDef `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if !strings.Contains(payload.Messages[0].Content, "filesystem_info") {
			t.Error("system prompt did not describe actual file access")
		}
		name, args := "", map[string]any{}
		switch calls.Add(1) {
		case 1:
			found := false
			for _, def := range payload.Tools {
				found = found || def.Function.Name == "filesystem_write"
			}
			if !found {
				t.Error("file tools were not exposed to the model")
			}
			name = "filesystem_info"
		case 2:
			if !strings.Contains(payload.Messages[len(payload.Messages)-1].Content, a.filesHome) {
				t.Error("the model did not receive the local home directory")
			}
			name, args = "filesystem_mkdir", map[string]any{"path": filepath.Dir(pagePath)}
		case 3:
			name, args = "filesystem_write", map[string]any{"path": pagePath, "content": content}
		case 4:
			name, args = "filesystem_read", map[string]any{"path": pagePath}
		default:
			var observation map[string]any
			_ = json.Unmarshal([]byte(payload.Messages[len(payload.Messages)-1].Content), &observation)
			if observation["content"] != content {
				t.Error("the agent did not verify the saved file")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"message": Message{Role: "assistant", Content: "Página creada y verificada: " + pagePath}})
			return
		}
		raw, _ := json.Marshal(args)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": Message{Role: "assistant", ToolCalls: []ToolCall{{Function: FunctionCall{name, raw}}}}})
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	run := postChat(t, a, "", "Crea una carpeta Mi página en mi Escritorio y guarda index.html. Verifica el archivo.")
	done := waitRun(t, a, run.ID)
	if done.Status != "completed" || !strings.Contains(done.Result, pagePath) {
		t.Fatalf("file task did not complete: %s: %s", done.Status, done.Result)
	}
	if data, err := os.ReadFile(pagePath); err != nil || string(data) != content {
		t.Fatal("agent did not create the actual HTML file", err)
	}
	if a.browser.session != "" {
		t.Fatal("a filesystem operation unexpectedly started the browser")
	}
}
