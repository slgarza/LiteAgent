package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type skillImportInput struct {
	Kind             string   `json:"kind"`
	ID               string   `json:"id"`
	ExpectedRevision string   `json:"expected_revision"`
	URL              string   `json:"url"`
	Ref              string   `json:"ref"`
	Path             string   `json:"path"`
	Paths            []string `json:"paths"`
}

func fetchSkillURL(ctx context.Context, client *http.Client, address string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "LiteAgent/"+Version)
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("could not download the skill source")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("skill source returned HTTP %d; GitHub imports require a public repository", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("skill download exceeds its size limit")
	}
	return data, nil
}
func githubSkill(ctx context.Context, input skillImportInput, client *http.Client) (map[string][]byte, SkillSource, error) {
	u, err := url.Parse(input.URL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, SkillSource{}, errors.New("use a public https://github.com/owner/repository URL without credentials or query parameters")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return nil, SkillSource{}, errors.New("GitHub URL needs owner and repository")
	}
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
	if !regexpRepoPart(owner) || !regexpRepoPart(repo) {
		return nil, SkillSource{}, errors.New("invalid GitHub repository")
	}
	ref, subdir := input.Ref, input.Path
	if len(parts) > 2 {
		if len(parts) < 4 || (parts[2] != "tree" && parts[2] != "blob") {
			return nil, SkillSource{}, errors.New("use a repository or /tree/ref/skill-folder URL")
		}
		if ref == "" {
			ref = parts[3]
		}
		if subdir == "" && len(parts) > 4 {
			subdir = strings.Join(parts[4:], "/")
			if parts[2] == "blob" {
				subdir = path.Dir(subdir)
				if subdir == "." {
					subdir = ""
				}
			}
		}
	}
	if ref == "" {
		ref = "HEAD"
	}
	if len(ref) > 200 || strings.ContainsAny(ref, "\x00\r\n") {
		return nil, SkillSource{}, errors.New("invalid GitHub revision")
	}
	base := "https://api.github.com/repos/" + owner + "/" + repo
	raw, err := fetchSkillURL(ctx, client, base+"/commits/"+url.PathEscape(ref), 1<<20)
	if err != nil {
		return nil, SkillSource{}, err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if json.Unmarshal(raw, &commit) != nil || len(commit.SHA) != 40 || strings.Trim(commit.SHA, "0123456789abcdef") != "" {
		return nil, SkillSource{}, errors.New("GitHub did not return a commit SHA")
	}
	raw, err = fetchSkillURL(ctx, client, base+"/git/trees/"+commit.SHA+"?recursive=1", 8<<20)
	if err != nil {
		return nil, SkillSource{}, err
	}
	var tree struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path, Mode, Type string
			Size             int64
		} `json:"tree"`
	}
	if json.Unmarshal(raw, &tree) != nil || tree.Truncated {
		return nil, SkillSource{}, errors.New("repository tree is incomplete; import the selected skill as a ZIP instead")
	}
	candidates := map[string][]byte{}
	for _, f := range tree.Tree {
		if f.Type == "blob" {
			candidates[f.Path] = nil
		}
	}
	selectedRoot, err := skillRoot(candidates, subdir)
	if err != nil {
		return nil, SkillSource{}, err
	}
	if _, err = selectSkillFiles(candidates, selectedRoot); err != nil {
		return nil, SkillSource{}, err
	}
	files := map[string][]byte{}
	total := int64(0)
	for _, f := range tree.Tree {
		prefix := ""
		if selectedRoot != "." {
			prefix = selectedRoot + "/"
		}
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		rel := strings.TrimPrefix(f.Path, prefix)
		if f.Type == "tree" {
			continue
		}
		if f.Type != "blob" || f.Mode != "100644" && f.Mode != "100755" {
			return nil, SkillSource{}, errors.New("skill repository contains a link or submodule")
		}
		if f.Size < 0 || f.Size > maxSkillFileBytes {
			return nil, SkillSource{}, errors.New("repository resource exceeds 8 MiB")
		}
		total += f.Size
		if total > maxSkillBytes {
			return nil, SkillSource{}, errors.New("skill package exceeds 16 MiB")
		}
		if err := skillPath(rel); err != nil {
			return nil, SkillSource{}, err
		}
		rawURL := &url.URL{Scheme: "https", Host: "raw.githubusercontent.com", Path: "/" + owner + "/" + repo + "/" + commit.SHA + "/" + f.Path}
		data, e := fetchSkillURL(ctx, client, rawURL.String(), maxSkillFileBytes)
		if e != nil {
			return nil, SkillSource{}, e
		}
		if strings.EqualFold(rel, "skill.md") || strings.EqualFold(rel, "skills.md") {
			rel = "SKILL.md"
		}
		if int64(len(data)) != f.Size {
			return nil, SkillSource{}, errors.New("GitHub resource size changed")
		}
		files[rel] = data
	}
	source := SkillSource{Kind: "github", URL: "https://github.com/" + owner + "/" + repo, Ref: ref, Path: selectedRoot, Commit: commit.SHA}
	if source.Path == "." {
		source.Path = ""
	}
	return files, source, validateSkillFiles(files)
}
func regexpRepoPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.", c)) {
			return false
		}
	}
	return true
}

func (a *App) importSkill(w http.ResponseWriter, r *http.Request) {
	var input skillImportInput
	var files map[string][]byte
	var source SkillSource
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, 34<<20)
		reader, e := r.MultipartReader()
		if e != nil {
			fail(w, 400, e)
			return
		}
		data := map[string][]byte{}
		parts := 0
		for {
			part, e := reader.NextPart()
			if e == io.EOF {
				break
			}
			if e != nil {
				err = e
				break
			}
			parts++
			if parts > maxSkillFiles+2 {
				err = errors.New("too many upload parts")
				break
			}
			name := part.FormName()
			if _, exists := data[name]; exists {
				err = errors.New("duplicate upload part")
				break
			}
			limit := int64(maxSkillFileBytes)
			if name == "request" {
				limit = 64 << 10
			}
			if name == "archive" {
				limit = 32 << 20
			}
			b, e := io.ReadAll(io.LimitReader(part, limit+1))
			part.Close()
			if e != nil || int64(len(b)) > limit {
				err = errors.New("upload part exceeds its size limit")
				break
			}
			data[name] = b
		}
		if err == nil {
			err = json.Unmarshal(data["request"], &input)
		}
		if err == nil {
			switch input.Kind {
			case "zip":
				files, err = readSkillZip(data["archive"], input.Path)
			case "folder":
				all := map[string][]byte{}
				if len(input.Paths) == 0 || len(input.Paths) > maxSkillFiles {
					err = errors.New("choose a skill folder with at most 128 files")
					break
				}
				for i, p := range input.Paths {
					if _, ok := all[p]; ok {
						err = errors.New("duplicate uploaded path")
						break
					}
					if e := skillPath(p); e != nil {
						err = e
						break
					}
					b, ok := data[fmt.Sprint("file", i)]
					if !ok {
						err = errors.New("uploaded resource is missing")
						break
					}
					all[p] = b
				}
				if err == nil {
					files, err = selectSkillFiles(all, input.Path)
				}
			default:
				err = errors.New("unsupported upload kind")
			}
		}
		source = SkillSource{Kind: input.Kind}
	} else {
		if err = decode(w, r, &input); err == nil {
			if input.Kind != "github" {
				err = errors.New("use ZIP, folder or GitHub import")
			} else {
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
				defer cancel()
				client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
					return errors.New("skill source redirects are not accepted")
				}}
				files, source, err = githubSkill(ctx, input, client)
			}
		}
	}
	if err != nil {
		fail(w, 400, err)
		return
	}
	s, err := a.store.installSkillPackage(r.Context(), input.ID, input.ExpectedRevision, files, source, "")
	if err != nil {
		fail(w, 400, err)
		return
	}
	sendJSON(w, 200, s)
}
func (a *App) findSkill(id string) (Skill, error) {
	for _, s := range a.store.Snapshot().Skills {
		if s.ID == id {
			return s, nil
		}
	}
	return Skill{}, errors.New("skill not found")
}
func skillResourcePage(dir string, s Skill, name string, offset, limit int) (map[string]any, error) {
	b, err := readSkillResource(dir, s, name)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"id": s.ID, "revision": s.Revision, "path": name, "sha256": localFileHash(b), "size": len(b), "text": skillText(b)}
	if skillText(b) {
		if offset < 0 || limit < 1 || limit > 6000 {
			return nil, errors.New("invalid resource page")
		}
		chars := []rune(string(b))
		if offset > len(chars) {
			return nil, errors.New("offset is beyond the resource")
		}
		end := offset + min(len(chars)-offset, limit)
		for end > offset {
			raw, _ := json.Marshal(string(chars[offset:end]))
			if len(raw) <= 12000 {
				break
			}
			end = offset + (end-offset)/2
		}
		result["content"] = string(chars[offset:end])
		result["next_offset"] = end
		result["truncated"] = end < len(chars)
		result["total_characters"] = len(chars)
	} else {
		result["note"] = "Binary resource. Copy it to a working file with skills_copy_resource, then use the appropriate file tools."
	}
	return result, nil
}
func (a *App) skillFileAPI(w http.ResponseWriter, r *http.Request) {
	s, err := a.findSkill(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	if r.Method == "PUT" {
		var input struct {
			Path, Content    string
			ExpectedRevision string `json:"expected_revision"`
			ExpectedSHA256   string `json:"expected_sha256"`
		}
		if err = decode(w, r, &input); err != nil {
			fail(w, 400, err)
			return
		}
		if s.Revision != input.ExpectedRevision || input.ExpectedSHA256 == "" {
			fail(w, 409, errors.New("read the latest revision and file hash before editing"))
			return
		}
		files, e := readSkillFiles(a.dir, s)
		if e != nil {
			fail(w, 400, e)
			return
		}
		previous, ok := files[input.Path]
		if !ok || localFileHash(previous) != input.ExpectedSHA256 {
			fail(w, 409, errors.New("resource changed; read it again"))
			return
		}
		if !skillText([]byte(input.Content)) || len(input.Content) > maxSkillDocumentBytes {
			fail(w, 400, errors.New("edited resource must be UTF-8 text of at most 64 KiB"))
			return
		}
		files[input.Path] = []byte(input.Content)
		saved, e := a.store.installSkillPackage(r.Context(), s.ID, input.ExpectedRevision, files, s.Source, "")
		if e != nil {
			fail(w, 400, e)
			return
		}
		sendJSON(w, 200, saved)
		return
	}
	name := r.URL.Query().Get("path")
	if name == "" {
		name = "SKILL.md"
	}
	offset := 0
	if value := r.URL.Query().Get("offset"); value != "" {
		if offset, err = strconv.Atoi(value); err != nil || offset < 0 {
			fail(w, 400, errors.New("invalid offset"))
			return
		}
	}
	if revision := r.URL.Query().Get("revision"); revision != "" && revision != s.Revision {
		fail(w, 409, errors.New("skill changed; reopen the file"))
		return
	}
	page, err := skillResourcePage(a.dir, s, name, offset, 6000)
	if err != nil {
		fail(w, 400, err)
		return
	}
	sendJSON(w, 200, page)
}
func (a *App) exportSkill(w http.ResponseWriter, r *http.Request) {
	s, err := a.findSkill(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	if r.URL.Query().Get("path") != "" {
		name := r.URL.Query().Get("path")
		b, e := readSkillResource(a.dir, s, name)
		if e != nil {
			fail(w, 400, e)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(name)}))
		w.Write(b)
		return
	}
	files, err := readSkillFiles(a.dir, s)
	if err != nil {
		fail(w, 400, err)
		return
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	slug := s.Slug
	if slug == "" {
		slug = slugForSkill(s.Name)
	}
	for name, data := range files {
		f, e := writer.Create(slug + "/" + name)
		if e != nil {
			err = e
			break
		}
		_, err = f.Write(data)
		if err != nil {
			break
		}
	}
	if e := writer.Close(); err == nil {
		err = e
	}
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": slug + ".zip"}))
	w.Write(buffer.Bytes())
}
func (a *App) toggleSkill(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Disabled         bool   `json:"disabled"`
		ExpectedRevision string `json:"expected_revision"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.store.skillsMu.Lock()
	defer a.store.skillsMu.Unlock()
	err := a.store.Update(func(st *State) error {
		for i := range st.Skills {
			if st.Skills[i].ID == r.PathValue("id") {
				if st.Skills[i].Revision != input.ExpectedRevision {
					return errors.New("skill changed; refresh the library")
				}
				st.Skills[i].Disabled = input.Disabled
				if input.Disabled {
					for j := range st.Schedules {
						if st.Schedules[j].SkillID == st.Skills[i].ID {
							st.Schedules[j].Enabled = false
						}
					}
				}
				return nil
			}
		}
		return errors.New("skill not found")
	})
	if err != nil {
		fail(w, 400, err)
		return
	}
	sendJSON(w, 200, map[string]bool{"disabled": input.Disabled})
}
func (a *App) saveSimpleSkill(ctx context.Context, s Skill, expected string) (Skill, error) {
	s.Name = strings.TrimSpace(s.Name)
	s.Description = strings.TrimSpace(s.Description)
	s.Instructions = strings.TrimSpace(s.Instructions)
	if s.Name == "" || len(s.Name) > 120 || len([]rune(s.Description)) > 1024 || len(s.Instructions) < 10 || len(s.Instructions) > maxSkillDocumentBytes/2 {
		return Skill{}, errors.New("a skill needs a name and 10–32768 bytes of instructions")
	}
	files := map[string][]byte{"SKILL.md": legacySkillDocument(s)}
	source := SkillSource{Kind: "local"}
	if s.ID != "" {
		old, err := a.findSkill(s.ID)
		if err != nil {
			return Skill{}, err
		}
		if old.Revision != expected {
			return Skill{}, errors.New("skill changed; reopen it before saving")
		}
		files, err = readSkillFiles(a.dir, old)
		if err != nil {
			return Skill{}, err
		}
		front, _, err := skillDocument(files["SKILL.md"])
		if err != nil {
			return Skill{}, err
		}
		setYAMLString(front, "name", slugForSkill(s.Name))
		description := s.Description
		if description == "" {
			description = "Saved procedure: " + s.Name
		}
		setYAMLString(front, "description", description)
		raw, err := yaml.Marshal(front)
		if err != nil {
			return Skill{}, err
		}
		files["SKILL.md"] = []byte("---\n" + string(raw) + "---\n\n" + s.Instructions + "\n")
		source = old.Source
	}
	return a.store.installSkillPackage(ctx, s.ID, expected, files, source, s.Name)
}
