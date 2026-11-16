package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const maxSkillBytes = 16 << 20
const maxSkillFileBytes = 8 << 20
const maxSkillDocumentBytes = 64 << 10
const maxSkillFiles = 128

type SkillFile struct {
	Path   string `json:"path"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
	Text   bool   `json:"text"`
}
type SkillSource struct {
	Kind   string `json:"kind"`
	URL    string `json:"url,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Path   string `json:"path,omitempty"`
	Commit string `json:"commit,omitempty"`
}
type SkillCompatibility struct {
	Status string   `json:"status"`
	Notes  []string `json:"notes"`
}

var skillSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var skillWindowsDevicePattern = regexp.MustCompile(`^(COM|LPT)[0-9¹²³]$`)

func skillPath(name string) error {
	if name == "" || len(name) > 240 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return errors.New("invalid package-relative file path")
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || strings.TrimRight(part, " .") != part {
			return errors.New("unsafe package path")
		}
		for _, r := range part {
			if r < 32 || r == 127 || strings.ContainsRune(`<>"|?*`, r) {
				return errors.New("file path is not portable to Windows")
			}
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CLOCK$" || skillWindowsDevicePattern.MatchString(base) {
			return errors.New("reserved Windows file name")
		}
	}
	return nil
}
func skillText(data []byte) bool { return utf8.Valid(data) && !bytes.ContainsRune(data, 0) }
func validateSkillFiles(files map[string][]byte) error {
	if len(files) == 0 || len(files) > maxSkillFiles {
		return fmt.Errorf("a package must contain 1 to %d files", maxSkillFiles)
	}
	total := 0
	seen := map[string]bool{}
	for name, data := range files {
		if err := skillPath(name); err != nil {
			return fmt.Errorf("%q: %w", name, err)
		}
		lower := strings.ToLower(name)
		if seen[lower] {
			return errors.New("duplicate or case-colliding package files")
		}
		seen[lower] = true
		if len(data) > maxSkillFileBytes {
			return errors.New("a package file exceeds 8 MiB")
		}
		total += len(data)
	}
	for name := range files {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if seen[strings.ToLower(dir)] {
				return errors.New("a file collides with a package directory")
			}
		}
	}
	if total > maxSkillBytes {
		return errors.New("expanded skill package exceeds 16 MiB")
	}
	return nil
}

func skillDocument(data []byte) (*yaml.Node, string, error) {
	if len(data) > maxSkillDocumentBytes || !skillText(data) {
		return nil, "", errors.New("SKILL.md must be UTF-8 text of at most 64 KiB")
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) < 3 || lines[0] != "---" {
		return nil, "", errors.New("SKILL.md needs YAML frontmatter with name and description")
	}
	end := 1
	for end < len(lines) && lines[end] != "---" {
		end++
	}
	if end == len(lines) {
		return nil, "", errors.New("SKILL.md frontmatter is not closed")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &doc); err != nil {
		return nil, "", errors.New("invalid SKILL.md YAML frontmatter")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, "", errors.New("frontmatter must be a YAML mapping")
	}
	count := 0
	var check func(*yaml.Node, int) error
	check = func(n *yaml.Node, depth int) error {
		count++
		if count > 4096 || depth > 24 || n.Kind == yaml.AliasNode || n.Anchor != "" {
			return errors.New("frontmatter aliases or excessive nesting are not supported")
		}
		if n.Kind == yaml.MappingNode {
			keys := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				key := n.Content[i]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || keys[key.Value] {
					return errors.New("frontmatter keys must be unique strings")
				}
				keys[key.Value] = true
			}
		}
		for _, child := range n.Content {
			if err := check(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := check(&doc, 0); err != nil {
		return nil, "", err
	}
	body := strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	if body == "" {
		return nil, "", errors.New("SKILL.md needs instructions after the frontmatter")
	}
	return doc.Content[0], body, nil
}
func yamlField(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
func yamlString(n *yaml.Node, key string) string {
	v := yamlField(n, key)
	if v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}
func setYAMLString(n *yaml.Node, key, value string) {
	v := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content[i+1] = v
			return
		}
	}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}
func legacySkillDocument(s Skill) []byte {
	slug := slugForSkill(s.Name)
	description := s.Description
	if strings.TrimSpace(description) == "" {
		description = "Saved procedure: " + s.Name
	}
	front := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setYAMLString(front, "name", slug)
	setYAMLString(front, "description", description)
	raw, _ := yaml.Marshal(front)
	return []byte("---\n" + string(raw) + "---\n\n" + s.Instructions + "\n")
}
func slugForSkill(name string) string {
	var out strings.Builder
	dash := false
	for _, c := range strings.ToLower(name) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			if dash && out.Len() > 0 {
				out.WriteByte('-')
			}
			out.WriteRune(c)
			dash = false
		} else {
			dash = true
		}
	}
	slug := strings.Trim(out.String(), "-")
	if len(slug) > 64 {
		slug = strings.TrimRight(slug[:64], "-")
	}
	if slug == "" {
		slug = "skill"
	}
	return slug
}
func describeSkillPackage(files map[string][]byte) (Skill, error) {
	if err := validateSkillFiles(files); err != nil {
		return Skill{}, err
	}
	front, body, err := skillDocument(files["SKILL.md"])
	if err != nil {
		return Skill{}, err
	}
	name, description := yamlString(front, "name"), yamlString(front, "description")
	if yamlField(front, "name") == nil || yamlField(front, "description") == nil || yamlField(front, "name").Tag != "!!str" || yamlField(front, "description").Tag != "!!str" || !skillSlugPattern.MatchString(name) || len(name) > 64 || strings.TrimSpace(description) == "" || len([]rune(description)) > 1024 {
		return Skill{}, errors.New("frontmatter needs a lowercase hyphenated name (1–64 characters) and description (1–1024 characters)")
	}
	s := Skill{Name: name, Slug: name, Description: description, Instructions: body, Version: yamlString(front, "version"), Compatibility: SkillCompatibility{Status: "Format valid", Notes: []string{"Instructions and resources can be used; runtime behavior has not been verified."}}}
	keys := []string{}
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	scripts := false
	for _, k := range keys {
		data := files[k]
		s.Files = append(s.Files, SkillFile{k, len(data), localFileHash(data), skillText(data)})
		ext := strings.ToLower(path.Ext(k))
		if strings.HasPrefix(k, "scripts/") || ext == ".py" || ext == ".sh" || ext == ".ps1" || ext == ".js" || ext == ".exe" || ext == ".bat" {
			scripts = true
		}
	}
	if scripts {
		s.Compatibility.Status = "Needs adaptation"
		s.Compatibility.Notes = append(s.Compatibility.Notes, "Scripts are stored and inspectable. LiteAgent cannot execute them or install their dependencies.")
	}
	if v := yamlString(front, "compatibility"); v != "" {
		s.Compatibility.Notes = append(s.Compatibility.Notes, "Declared compatibility: "+cut(v, 500))
	}
	if field := yamlField(front, "allowed-tools"); field != nil {
		raw, _ := yaml.Marshal(field)
		v := strings.TrimSpace(string(raw))
		s.Compatibility.Status = "Needs adaptation"
		s.Compatibility.Notes = append(s.Compatibility.Notes, "Declared tools must be mapped to LiteAgent: "+cut(v, 500))
	}
	if yamlField(front, "command-dispatch") != nil {
		s.Compatibility.Status = "Needs adaptation"
		s.Compatibility.Notes = append(s.Compatibility.Notes, "OpenClaw command dispatch is preserved but not executed by LiteAgent.")
	}
	var meta map[string]any
	if v := yamlField(front, "metadata"); v != nil {
		_ = v.Decode(&meta)
		if meta == nil && v.Kind == yaml.ScalarNode {
			_ = json.Unmarshal([]byte(v.Value), &meta)
		}
	}
	if s.Version == "" {
		if v, ok := meta["version"]; ok {
			s.Version = fmt.Sprint(v)
		}
	}
	for _, provider := range []string{"openclaw", "clawdbot", "clawdis", "hermes"} {
		value, ok := meta[provider]
		if !ok {
			continue
		}
		m, ok := value.(map[string]any)
		if !ok {
			if text, yes := value.(string); yes {
				_ = json.Unmarshal([]byte(text), &m)
			}
		}
		if m == nil {
			s.Compatibility.Status = "Needs adaptation"
			s.Compatibility.Notes = append(s.Compatibility.Notes, "Unrecognized "+provider+" metadata is preserved; review it before use.")
			continue
		}
		for _, key := range []string{"requires", "envVars", "primaryEnv", "install", "platforms", "os"} {
			if v, ok := m[key]; ok {
				raw, _ := json.Marshal(v)
				s.Compatibility.Status = "Needs adaptation"
				s.Compatibility.Notes = append(s.Compatibility.Notes, provider+" "+key+": "+cut(string(raw), 700))
			}
		}
	}
	if v := yamlField(front, "platforms"); v != nil {
		raw, _ := yaml.Marshal(v)
		s.Compatibility.Status = "Needs adaptation"
		s.Compatibility.Notes = append(s.Compatibility.Notes, "Declared platforms: "+cut(strings.TrimSpace(string(raw)), 300)+"; current platform: "+runtime.GOOS)
	}
	raw, _ := json.Marshal(s.Files)
	s.Revision = localFileHash(raw)
	return s, nil
}

// Select one skill, allowing a ZIP wrapper or an explicitly chosen repository
// subfolder. Files outside the selected skill never become runtime resources.
func skillRoot(all map[string][]byte, subdir string) (string, error) {
	roots := []string{}
	for name := range all {
		base := strings.ToLower(path.Base(name))
		if base == "skill.md" || base == "skills.md" {
			roots = append(roots, path.Dir(name))
		}
	}
	sort.Strings(roots)
	root := "."
	if subdir != "" && subdir != "." {
		if err := skillPath(subdir); err != nil {
			return "", err
		}
		root = subdir
	} else if subdir == "." {
		root = "."
	} else if len(roots) == 1 {
		root = roots[0]
	} else {
		return "", fmt.Errorf("choose one skill folder; found %d SKILL.md files: %s", len(roots), strings.Join(roots[:min(8, len(roots))], ", "))
	}
	return root, nil
}
func selectSkillFiles(all map[string][]byte, subdir string) (map[string][]byte, error) {
	root, err := skillRoot(all, subdir)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	found := false
	for name, data := range all {
		rel := name
		if root != "." {
			prefix := root + "/"
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			rel = strings.TrimPrefix(name, prefix)
		}
		if strings.EqualFold(rel, "SKILL.md") || strings.EqualFold(rel, "skills.md") {
			if found {
				return nil, errors.New("multiple root skill documents")
			}
			rel = "SKILL.md"
			found = true
		}
		files[rel] = data
	}
	if !found {
		return nil, errors.New("the selected folder has no SKILL.md")
	}
	return files, validateSkillFiles(files)
}
func readSkillZip(data []byte, subdir string) (map[string][]byte, error) {
	if len(data) > 32<<20 {
		return nil, errors.New("ZIP exceeds 32 MiB")
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("invalid ZIP package")
	}
	if len(reader.File) > 4096 {
		return nil, errors.New("archive has too many entries")
	}
	all := map[string][]byte{}
	seen := map[string]bool{}
	expanded := uint64(0)
	for _, f := range reader.File {
		name := strings.TrimSuffix(f.Name, "/")
		if err := skillPath(name); err != nil {
			return nil, err
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, errors.New("archive contains duplicate or case-colliding paths")
		}
		seen[key] = true
		if f.Mode()&os.ModeSymlink != 0 || !f.FileInfo().IsDir() && !f.Mode().IsRegular() {
			return nil, errors.New("archive links and special files are not supported")
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if f.UncompressedSize64 > maxSkillFileBytes {
			return nil, errors.New("archive contains a file larger than 8 MiB")
		}
		expanded += f.UncompressedSize64
		if expanded > 64<<20 {
			return nil, errors.New("expanded archive exceeds 64 MiB")
		}
		r, e := f.Open()
		if e != nil {
			return nil, e
		}
		b, e := io.ReadAll(io.LimitReader(r, maxSkillFileBytes+1))
		r.Close()
		if e != nil || len(b) > maxSkillFileBytes {
			return nil, errors.New("invalid or oversized ZIP entry")
		}
		all[f.Name] = b
	}
	return selectSkillFiles(all, subdir)
}
func skillPackageDir(dir string, s Skill) string {
	return filepath.Join(dir, "skills", localFileHash([]byte(s.ID)), s.Revision)
}
func readSkillResource(dir string, s Skill, name string) ([]byte, error) {
	if err := skillPath(name); err != nil {
		return nil, err
	}
	if s.Revision == "" {
		if name == "SKILL.md" {
			return legacySkillDocument(s), nil
		}
		return nil, errors.New("resource not found")
	}
	var record *SkillFile
	for i := range s.Files {
		if s.Files[i].Path == name {
			record = &s.Files[i]
			break
		}
	}
	if record == nil {
		return nil, errors.New("resource is not part of this skill revision")
	}
	root, err := os.OpenRoot(skillPackageDir(dir, s))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for p := name; p != "."; p = path.Dir(p) {
		info, e := root.Lstat(filepath.FromSlash(p))
		if e != nil {
			return nil, e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("skill resource links are not supported")
		}
	}
	b, err := readLocalFile(root, filepath.FromSlash(name), maxSkillFileBytes)
	if err != nil {
		return nil, err
	}
	if localFileHash(b) != record.SHA256 {
		return nil, errors.New("skill resource changed outside the package manager; reimport it before use")
	}
	return b, nil
}
func readSkillFiles(dir string, s Skill) (map[string][]byte, error) {
	if s.Revision == "" {
		return map[string][]byte{"SKILL.md": legacySkillDocument(s)}, nil
	}
	files := map[string][]byte{}
	for _, f := range s.Files {
		b, e := readSkillResource(dir, s, f.Path)
		if e != nil {
			return nil, e
		}
		files[f.Path] = b
	}
	return files, nil
}
func publishSkillFiles(ctx context.Context, dir string, s Skill, files map[string][]byte) (bool, error) {
	parent := filepath.Dir(skillPackageDir(dir, s))
	if err := os.MkdirAll(parent, 0700); err != nil {
		return false, err
	}
	if _, err := os.Stat(skillPackageDir(dir, s)); err == nil {
		_, err = readSkillFiles(dir, s)
		return false, err
	}
	temp, err := os.MkdirTemp(parent, ".stage-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(temp)
	root, err := os.OpenRoot(temp)
	if err != nil {
		return false, err
	}
	for name, data := range files {
		if err = ctx.Err(); err != nil {
			break
		}
		if err = root.MkdirAll(filepath.FromSlash(path.Dir(name)), 0700); err != nil {
			break
		}
		var f *os.File
		f, err = root.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			break
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			break
		}
	}
	root.Close()
	if err != nil {
		return false, err
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	err = os.Rename(temp, skillPackageDir(dir, s))
	return err == nil, err
}
func (s *Store) installSkillPackage(ctx context.Context, id, expected string, files map[string][]byte, source SkillSource, display string) (Skill, error) {
	s.skillsMu.Lock()
	defer s.skillsMu.Unlock()
	candidate, err := describeSkillPackage(files)
	if err != nil {
		return Skill{}, err
	}
	candidate.ID = id
	candidate.Source = source
	candidate.UpdatedAt = now()
	if display != "" {
		candidate.Name = display
	}
	fresh := id == ""
	if fresh {
		candidate.ID = newID()
	}
	st := s.Snapshot()
	// Older UI versions allowed duplicate display names. Preserve those names
	// during migration and updates; IDs still identify each package uniquely.
	unchangedName := false
	for _, old := range st.Skills {
		if old.ID == id && old.Name == candidate.Name {
			unchangedName = true
		}
	}
	found := false
	for _, old := range st.Skills {
		if old.ID == id {
			found = true
			if old.Revision != expected {
				return Skill{}, errors.New("skill changed; reopen it before saving")
			}
			candidate.Disabled = old.Disabled
		}
		if !unchangedName && old.ID != id && strings.EqualFold(old.Name, candidate.Name) {
			return Skill{}, errors.New("a skill with this name already exists; choose Update to replace it")
		}
	}
	if !fresh && !found {
		return Skill{}, errors.New("skill not found")
	}
	if fresh && len(st.Skills) >= 100 {
		return Skill{}, errors.New("the limit of 100 skills has been reached")
	}
	if err = ctx.Err(); err != nil {
		return Skill{}, err
	}
	dir := filepath.Dir(s.path)
	created, err := publishSkillFiles(ctx, dir, candidate, files)
	if err != nil {
		return Skill{}, err
	}
	err = s.Update(func(st *State) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for i := range st.Skills {
			if st.Skills[i].ID == candidate.ID {
				if st.Skills[i].Revision != expected {
					return errors.New("skill changed; reopen it before saving")
				}
				st.Skills[i] = candidate
				return nil
			}
		}
		if !fresh {
			return errors.New("skill was removed")
		}
		st.Skills = append(st.Skills, candidate)
		return nil
	})
	if err != nil && created {
		_ = os.RemoveAll(skillPackageDir(dir, candidate))
	}
	return candidate, err
}
func (s *Store) migrateSkills() error {
	for _, old := range s.Snapshot().Skills {
		if old.Revision != "" {
			continue
		}
		_, err := s.installSkillPackage(context.Background(), old.ID, "", map[string][]byte{"SKILL.md": legacySkillDocument(old)}, SkillSource{Kind: "local"}, old.Name)
		if err != nil {
			return fmt.Errorf("migrate saved skill: %w", err)
		}
	}
	return nil
}

// No worker exists while opening the store. Older revisions stay available to
// running tasks until this next startup, then only current packages are retained.
func (s *Store) pruneSkillPackages() error {
	root, err := os.OpenRoot(filepath.Join(filepath.Dir(s.path), "skills"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	current := map[string]string{}
	for _, skill := range s.Snapshot().Skills {
		current[localFileHash([]byte(skill.ID))] = skill.Revision
	}
	entries, err := os.ReadDir(root.Name())
	if err != nil {
		return err
	}
	hashName := func(name string) bool { return len(name) == 64 && strings.Trim(name, "0123456789abcdef") == "" }
	for _, entry := range entries {
		if !hashName(entry.Name()) || !entry.IsDir() {
			continue
		}
		revision, exists := current[entry.Name()]
		if !exists {
			if err = root.RemoveAll(entry.Name()); err != nil {
				return err
			}
			continue
		}
		child, e := root.OpenRoot(entry.Name())
		if e != nil {
			return e
		}
		versions, e := os.ReadDir(child.Name())
		child.Close()
		if e != nil {
			return e
		}
		for _, version := range versions {
			if version.Name() == revision || (!hashName(version.Name()) && !strings.HasPrefix(version.Name(), ".stage-")) {
				continue
			}
			if err = root.RemoveAll(filepath.Join(entry.Name(), version.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
