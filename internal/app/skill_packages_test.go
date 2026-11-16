package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func packageFixture() map[string][]byte {
	return map[string][]byte{
		"SKILL.md":                []byte("---\nname: report-check\ndescription: |\n  Review a report against its source.\nmetadata:\n  version: '2.1'\n  openclaw: {requires: {bins: [python3], env: [REPORT_TOKEN]}}\n  hermes: {platforms: [linux, windows]}\n  custom: {preserve: yes}\n---\n\nRead references/checklist.md, then copy assets/template.bin to a working file.\n"),
		"references/checklist.md": []byte("Confirm totals, date and source.\n"),
		"assets/template.bin":     {0, 1, 2, 255},
		"scripts/check.py":        []byte("print('fixture only')\n"),
		"SOUL.md":                 []byte("Package resource only."),
	}
}
func fixtureZIP(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for p, data := range files {
		f, e := w.Create(p)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(data); e != nil {
			t.Fatal(e)
		}
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func importFixture(t *testing.T, a *App, files map[string][]byte) Skill {
	t.Helper()
	s, e := a.store.installSkillPackage(context.Background(), "", "", files, SkillSource{Kind: "zip"}, "")
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestSkillPackageRoundTripAndEdits(t *testing.T) {
	a := newTestApp(t)
	files := packageFixture()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("archive", "package.zip")
	part.Write(fixtureZIP(t, files))
	form.WriteField("request", `{"kind":"zip"}`)
	form.Close()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/skills/import", &body)
	r.Header.Set("X-LiteAgent", "1")
	r.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("import %d: %s", w.Code, w.Body.String())
	}
	var s Skill
	json.Unmarshal(w.Body.Bytes(), &s)
	if s.Compatibility.Status != "Needs adaptation" || s.Version != "2.1" || len(s.Files) != 5 {
		t.Fatalf("incomplete metadata: %+v", s)
	}
	exported := callAPI(t, a, "GET", "/api/skills/"+s.ID+"/export", nil, nil)
	round, e := readSkillZip(exported.Body.Bytes(), "")
	if e != nil || !reflect.DeepEqual(files, round) {
		t.Fatalf("lossy export: %v", e)
	}
	get := callAPI(t, a, "GET", "/api/skills/"+s.ID+"/file?path=references/checklist.md", nil, nil)
	var page map[string]any
	json.Unmarshal(get.Body.Bytes(), &page)
	edit := map[string]any{"path": "references/checklist.md", "content": "Updated checklist.", "expected_revision": s.Revision, "expected_sha256": page["sha256"]}
	saved := callAPI(t, a, "PUT", "/api/skills/"+s.ID+"/file", edit, nil)
	if saved.Code != 200 {
		t.Fatal(saved.Body.String())
	}
	stale := callAPI(t, a, "PUT", "/api/skills/"+s.ID+"/file", edit, nil)
	if stale.Code != 409 {
		t.Fatalf("stale edit accepted: %d", stale.Code)
	}
	current, _ := a.findSkill(s.ID)
	got, e := readSkillResource(a.dir, current, "SKILL.md")
	if e != nil || !bytes.Equal(got, files["SKILL.md"]) {
		t.Fatal("resource edit changed main document", e)
	}
	current.Instructions = "Check totals and verify the source."
	updated, e := a.saveSimpleSkill(context.Background(), current, current.Revision)
	if e != nil {
		t.Fatal(e)
	}
	got, _ = readSkillResource(a.dir, updated, "SKILL.md")
	if !bytes.Contains(got, []byte("custom:")) || !bytes.Contains(got, []byte("preserve: yes")) {
		t.Fatal("basic edit lost unknown metadata")
	}
	bin, _ := readSkillResource(a.dir, updated, "assets/template.bin")
	if !bytes.Equal(bin, files["assets/template.bin"]) {
		t.Fatal("binary resource changed")
	}
	if result := callAPI(t, a, "GET", "/api/skills/"+s.ID+"/file?offset=1junk", nil, nil); result.Code != 400 {
		t.Fatal("invalid offset accepted")
	}
	if result := callAPI(t, a, "GET", "/api/skills/"+s.ID+"/file?revision="+s.Revision, nil, nil); result.Code != 409 {
		t.Fatal("mixed revision read accepted")
	}
}
func TestSkillPackagePathsAndYAML(t *testing.T) {
	for _, p := range []string{"../escape", "/absolute", "C:/file", "dir\\file", "a/../b", "CON.txt", "a/LPT1", "a./file", "a:stream", "a\x00b"} {
		t.Run(p, func(t *testing.T) {
			files := packageFixture()
			files[p] = []byte("x")
			if _, e := readSkillZip(fixtureZIP(t, files), ""); e == nil {
				t.Fatal("unsafe ZIP accepted")
			}
		})
	}
	for _, extra := range []map[string][]byte{{"Readme": {}, "README": {}}, {"a": {}, "a/b": {}}, {"huge": make([]byte, maxSkillFileBytes+1)}} {
		files := packageFixture()
		for k, v := range extra {
			files[k] = v
		}
		if _, e := describeSkillPackage(files); e == nil {
			t.Fatal("invalid resource set accepted")
		}
	}
	for _, header := range []string{"name: ok\nname: again\ndescription: test", "name: ok\ndescription: &a test\nextra: *a", "name: Bad Name\ndescription: test", "name: ok\ndescription: ''", "name: ok\ndescription: test\nextra: {x: 1, x: 2}"} {
		if _, e := describeSkillPackage(map[string][]byte{"SKILL.md": []byte("---\n" + header + "\n---\nDo the work.")}); e == nil {
			t.Fatal("invalid YAML accepted", header)
		}
	}
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	header := &zip.FileHeader{Name: "link"}
	header.SetMode(os.ModeSymlink | 0700)
	f, _ := z.CreateHeader(header)
	f.Write([]byte("outside"))
	z.Close()
	if _, e := readSkillZip(b.Bytes(), ""); e == nil {
		t.Fatal("symlink accepted")
	}
	all := map[string][]byte{"repo/a/SKILL.md": packageFixture()["SKILL.md"], "repo/b/SKILL.md": packageFixture()["SKILL.md"]}
	if _, e := selectSkillFiles(all, ""); e == nil {
		t.Fatal("ambiguous package accepted")
	}
	if files, e := selectSkillFiles(all, "repo/a"); e != nil || len(files) != 1 {
		t.Fatal("subdirectory failed", e)
	}
	if _, e := selectSkillFiles(all, "/repo/a"); e == nil {
		t.Fatal("absolute subdirectory accepted")
	}
}
func TestSkillMigrationPinnedRevisionAndRollback(t *testing.T) {
	a := newTestApp(t)
	old := Skill{ID: "old-id", Name: "My saved procedure", Instructions: "Check this report and verify its totals."}
	if e := a.store.Update(func(st *State) error {
		st.Skills = []Skill{old}
		st.Schedules = []Schedule{{ID: "schedule", SkillID: old.ID}}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	migrated, e := openStore(a.dir)
	if e != nil {
		t.Fatal(e)
	}
	a.store = migrated
	s := a.store.Snapshot().Skills[0]
	if s.ID != old.ID || s.Name != old.Name || s.Revision == "" || a.store.Snapshot().Schedules[0].SkillID != old.ID {
		t.Fatal("migration changed identity")
	}
	run := Run{skillCatalog: []Skill{s}}
	files, _ := readSkillFiles(a.dir, s)
	files["reference.txt"] = []byte("new resource")
	next, e := a.store.installSkillPackage(context.Background(), s.ID, s.Revision, files, s.Source, s.Name)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := a.skillTools(context.Background(), run, "skills_read", map[string]any{"id": s.ID, "path": "reference.txt"}); e == nil {
		t.Fatal("active run read a newer revision")
	}
	if _, e := a.skillTools(context.Background(), run, "skills_read", map[string]any{"id": s.ID}); e != nil {
		t.Fatal("old revision lost", e)
	}
	files["reference.txt"] = []byte("never committed")
	statePath := a.store.path
	a.store.path = filepath.Join(a.dir, "cannot-save")
	os.Mkdir(a.store.path, 0700)
	failed, e := a.store.installSkillPackage(context.Background(), s.ID, next.Revision, files, s.Source, s.Name)
	a.store.path = statePath
	if e == nil {
		t.Fatal("persistence failure ignored")
	}
	if a.store.Snapshot().Skills[0].Revision != next.Revision {
		t.Fatal("rollback lost current revision")
	}
	if _, e := os.Stat(skillPackageDir(a.dir, failed)); !os.IsNotExist(e) {
		t.Fatal("failed revision left published", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := a.store.installSkillPackage(ctx, s.ID, next.Revision, files, s.Source, s.Name); e == nil {
		t.Fatal("cancelled install succeeded")
	}
}
func TestSkillConcurrentUpdateAndDisabledSchedules(t *testing.T) {
	a := newTestApp(t)
	s := importFixture(t, a, packageFixture())
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, value := range []string{"first", "second"} {
		wg.Add(1)
		go func(v string) {
			defer wg.Done()
			files := packageFixture()
			files["notes.txt"] = []byte(v)
			_, e := a.store.installSkillPackage(context.Background(), s.ID, s.Revision, files, s.Source, "")
			results <- e
		}(value)
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("expected exactly one revision update", success)
	}
	schedule, e := a.createSchedule(Schedule{Title: "report", Kind: "skill", SkillID: s.ID, Repeat: "daily", Time: "14:00", Timezone: "UTC", Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	current, _ := a.findSkill(s.ID)
	w := callAPI(t, a, "POST", "/api/skills/"+s.ID+"/toggle", map[string]any{"disabled": true, "expected_revision": current.Revision}, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if a.store.Snapshot().Schedules[0].Enabled {
		t.Fatal("schedule not paused")
	}
	if e := a.updateSchedule(schedule.ID, "resume"); e == nil {
		t.Fatal("disabled skill schedule resumed")
	}
	if _, e := selectedSkills(a.store.Snapshot(), []string{s.ID}); e == nil {
		t.Fatal("disabled skill selected")
	}
	input := ChatRequest{Task: "/skill " + s.ID + " test"}
	if e := resolveSkillCommand(&input, a.store.Snapshot().Skills); e == nil {
		t.Fatal("disabled command accepted")
	}
	input.Task = "/skill report-check summarize November"
	if e := resolveSkillCommand(&input, []Skill{s}); e != nil || input.Task != "summarize November" || input.SkillIDs[0] != s.ID {
		t.Fatal("command failed", e)
	}
}
func TestSkillResourcePagingCopyAndIntegrity(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	files := packageFixture()
	files["large.txt"] = []byte(strings.Repeat("🦋<&", 8000))
	s := importFixture(t, a, files)
	var collected strings.Builder
	offset := 0
	for {
		page, e := skillResourcePage(a.dir, s, "large.txt", offset, 6000)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(page)
		if len(raw) > 14000 {
			t.Fatal("unbounded tool page")
		}
		collected.WriteString(page["content"].(string))
		next := page["next_offset"].(int)
		if next <= offset {
			t.Fatal("paging made no progress")
		}
		offset = next
		if !page["truncated"].(bool) {
			break
		}
	}
	if collected.String() != string(files["large.txt"]) {
		t.Fatal("paging lost Unicode")
	}
	output, e := a.skillTools(context.Background(), Run{}, "skills_search", map[string]any{"query": "report"})
	if e != nil || strings.Contains(output, "Read references/") || strings.Contains(output, "print(") {
		t.Fatal("search loaded body", e)
	}
	args := map[string]any{"id": s.ID, "path": "assets/template.bin", "destination": "working.bin"}
	if _, e := a.skillTools(context.Background(), Run{}, "skills_copy_resource", args); e != nil {
		t.Fatal(e)
	}
	if _, e := a.skillTools(context.Background(), Run{}, "skills_copy_resource", args); e == nil {
		t.Fatal("overwrite without hash")
	}
	args["expected_sha256"] = localFileHash(files["assets/template.bin"])
	if _, e := a.skillTools(context.Background(), Run{}, "skills_copy_resource", args); e != nil {
		t.Fatal(e)
	}
	args["destination"] = "../outside"
	if _, e := a.skillTools(context.Background(), Run{}, "skills_copy_resource", args); e == nil {
		t.Fatal("escaped home")
	}
	os.WriteFile(filepath.Join(skillPackageDir(a.dir, s), "SKILL.md"), []byte("tampered"), 0600)
	if _, e := readSkillResource(a.dir, s, "SKILL.md"); e == nil {
		t.Fatal("external tampering ignored")
	}
}

type skillTransport func(*http.Request) (*http.Response, error)

func (f skillTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSkillGitHubPinnedImport(t *testing.T) {
	sha := strings.Repeat("a", 40)
	files := packageFixture()
	var calls []string
	client := &http.Client{Transport: skillTransport(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.String())
		var raw []byte
		switch {
		case strings.Contains(r.URL.Path, "/commits/"):
			raw = []byte(`{"sha":"` + sha + `"}`)
		case strings.Contains(r.URL.Path, "/git/trees/"):
			entries := []map[string]any{{"path": "other/private.txt", "type": "blob", "mode": "100644", "size": 999999999}}
			for p, b := range files {
				entries = append(entries, map[string]any{"path": "skills/report/" + p, "type": "blob", "mode": "100644", "size": len(b)})
			}
			raw, _ = json.Marshal(map[string]any{"tree": entries})
		default:
			if r.URL.Host != "raw.githubusercontent.com" || !strings.Contains(r.URL.Path, sha+"/skills/report/") {
				t.Fatalf("unpinned or unrelated download %s", r.URL)
			}
			raw = files[strings.SplitN(r.URL.Path, "/skills/report/", 2)[1]]
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
	})}
	got, source, e := githubSkill(context.Background(), skillImportInput{URL: "https://github.com/example/skills/tree/main/skills/report"}, client)
	if e != nil || !reflect.DeepEqual(files, got) || source.Commit != sha || len(calls) != len(files)+2 {
		t.Fatal("GitHub import", source, e)
	}
	for _, address := range []string{"https://github.com.evil/repo/a", "http://github.com/a/b", "https://user:pass@github.com/a/b", "https://github.com/a/b?token=x"} {
		if _, _, e := githubSkill(context.Background(), skillImportInput{URL: address}, client); e == nil {
			t.Fatal("invalid GitHub URL accepted")
		}
	}
}

func TestSkillPackageCleanupOnRestart(t *testing.T) {
	a := newTestApp(t)
	old := importFixture(t, a, packageFixture())
	files := packageFixture()
	files["notes.txt"] = []byte("new")
	current, e := a.store.installSkillPackage(context.Background(), old.ID, old.Revision, files, old.Source, "")
	if e != nil {
		t.Fatal(e)
	}
	removedFiles := packageFixture()
	removedFiles["SKILL.md"] = bytes.ReplaceAll(removedFiles["SKILL.md"], []byte("name: report-check"), []byte("name: removable"))
	removed := importFixture(t, a, removedFiles)
	if w := callAPI(t, a, "DELETE", "/api/skills/"+removed.ID, nil, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, e := readSkillResource(a.dir, removed, "SKILL.md"); e != nil {
		t.Fatal("removed skill lost active revision", e)
	}
	if _, e := openStore(a.dir); e != nil {
		t.Fatal(e)
	}
	for _, s := range []Skill{old, removed} {
		if _, e := os.Stat(skillPackageDir(a.dir, s)); !os.IsNotExist(e) {
			t.Fatal("obsolete package retained", e)
		}
	}
	if _, e := readSkillResource(a.dir, current, "SKILL.md"); e != nil {
		t.Fatal("current revision removed", e)
	}
}

func TestSkillCommandStartsProviderAndDeduplicates(t *testing.T) {
	a := newTestApp(t)
	s := importFixture(t, a, packageFixture())
	requests := make(chan []Message, 2)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		requests <- body.Messages
		io.WriteString(w, `{"message":{"content":"Skill selected."}}`)
	}))
	defer model.Close()
	a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	input := map[string]any{"message": "/skill report-check /status is the example to review", "request_id": "skill-submit"}
	w := callAPI(t, a, "POST", "/api/chat", input, nil)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var run Run
	json.Unmarshal(w.Body.Bytes(), &run)
	done := waitRun(t, a, run.ID)
	if done.Status != "completed" || len(done.SkillIDs) != 1 || done.SkillIDs[0] != s.ID || done.Result != "Skill selected." {
		t.Fatalf("skill command misrouted: %+v", done)
	}
	messages := <-requests
	last := messages[len(messages)-1].Content
	if !strings.Contains(last, "Selected procedures") || !strings.Contains(last, "Read references/checklist.md") || strings.Contains(last, "print('fixture only')") {
		t.Fatal("incorrect progressive context")
	}
	duplicate := callAPI(t, a, "POST", "/api/chat", input, nil)
	var repeated Run
	json.Unmarshal(duplicate.Body.Bytes(), &repeated)
	if repeated.ID != run.ID {
		t.Fatal("submission was repeated")
	}
}

func TestSkillMigrationPreservesDuplicateLegacyNames(t *testing.T) {
	a := newTestApp(t)
	a.store.Update(func(st *State) error {
		st.Skills = []Skill{{ID: "first", Name: "Legacy", Instructions: "Check the first report."}, {ID: "second", Name: "Legacy", Instructions: "Check the second report."}}
		return nil
	})
	reopened, e := openStore(a.dir)
	if e != nil {
		t.Fatal(e)
	}
	skills := reopened.Snapshot().Skills
	if len(skills) != 2 || skills[0].ID != "first" || skills[1].ID != "second" || skills[0].Name != "Legacy" || skills[1].Name != "Legacy" {
		t.Fatal("legacy identities changed")
	}
	input := ChatRequest{Task: "/skill legacy"}
	if e := resolveSkillCommand(&input, skills); e == nil {
		t.Fatal("ambiguous display name accepted")
	}
	input.Task = "/skill second"
	if e := resolveSkillCommand(&input, skills); e != nil || input.SkillIDs[0] != "second" {
		t.Fatal("ID selection failed", e)
	}
}
