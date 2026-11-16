package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxOfficeFileBytes     = 32 << 20
	maxOfficeExpandedBytes = 64 << 20
	maxOfficePartBytes     = 16 << 20
	maxOfficeNodes         = 200000
)

func officeToolDefinitions() []ToolDef {
	p := stringProp("Path to an existing .xlsx, .docx or .pptx inside the user's home")
	h := stringProp("sha256 from filesystem_office_read; required before editing")
	s := stringProp("Exact section id returned by filesystem_office_read")
	return []ToolDef{
		tool("filesystem_office_read", "Read Excel, Word or PowerPoint. Omit section to list sheets/document parts/slides; supply section to list cells/paragraphs. Supply item for full paginated text, and field=formula for an Excel formula. Lists contain previews; follow next_offset while truncated. Values are stored data, not recalculated or visually rendered.", map[string]any{"path": p, "section": s, "item": stringProp("Cell address or paragraph id from a section read"), "field": stringProp("text (default), or formula for a spreadsheet cell"), "offset": map[string]any{"type": "number", "minimum": 0}, "limit": map[string]any{"type": "number", "minimum": 1, "maximum": 10000}, "expected_sha256": h}, "path"),
		tool("filesystem_office_edit", "Replace one unique text fragment in a Word/PowerPoint paragraph, including table cells, using section and item ids from a read. Preserves other package parts and surrounding runs; replacement inherits the first matched run's formatting. No paragraph insertion, layout editing or field/track-change editing.", map[string]any{"path": p, "section": s, "item": stringProp("Paragraph id from a section read"), "old_text": stringProp("Nonempty exact text occurring once within this paragraph"), "new_text": stringProp("Replacement text; empty removes the match"), "expected_sha256": h}, "path", "section", "item", "old_text", "new_text", "expected_sha256"),
		tool("filesystem_sheet_set", "Set up to 100 cells in one existing Excel worksheet, preserving cell styles. Read the sheet first. Each change has cell, type (string, number, boolean, formula, blank) and value as a string. Formula values omit the leading =. Does not calculate formulas; Excel/LibreOffice must recalculate. Merged subordinate cells and shared/array formulas cannot be overwritten.", map[string]any{"path": p, "section": s, "expected_sha256": h, "cells": map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "items": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"cell": stringProp("A1 cell address"), "type": stringProp("string, number, boolean, formula, or blank"), "value": stringProp("Text, finite decimal number, true/false, formula without =, or empty for blank")}, "required": []string{"cell", "type", "value"}}}}, "path", "section", "cells", "expected_sha256"),
	}
}

type officeNode struct {
	name                            xml.Name
	attrs                           []xml.Attr
	start, openEnd, closeStart, end int
	children                        []*officeNode
	parent                          *officeNode
	text                            string
}

func parseOfficeXML(ctx context.Context, data []byte) (*officeNode, error) {
	if len(data) > maxOfficePartBytes {
		return nil, errors.New("Office XML part exceeds 16 MiB")
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	var root *officeNode
	var stack []*officeNode
	count := 0
	for {
		before := int(d.InputOffset())
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid Office XML: %w", err)
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.Directive:
			return nil, errors.New("XML directives are not supported in Office documents")
		case xml.StartElement:
			count++
			if count > maxOfficeNodes || len(stack) >= 128 {
				return nil, errors.New("Office XML structure exceeds supported limits")
			}
			n := &officeNode{name: t.Name, attrs: t.Attr, start: before, openEnd: int(d.InputOffset())}
			if len(stack) > 0 {
				n.parent = stack[len(stack)-1]
				n.parent.children = append(n.parent.children, n)
			} else {
				if root != nil {
					return nil, errors.New("multiple XML roots")
				}
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			n := stack[len(stack)-1]
			n.closeStart = before
			n.end = int(d.InputOffset())
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(t)
			} else if strings.TrimSpace(string(t)) != "" {
				return nil, errors.New("text outside XML root")
			}
		}
	}
	if root == nil || len(stack) != 0 {
		return nil, errors.New("incomplete Office XML")
	}
	return root, nil
}
func (n *officeNode) attr(name string) string {
	for _, a := range n.attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
func officeNamespace(n *officeNode, kind string) bool {
	return n.name.Space == "http://schemas.openxmlformats.org/"+kind+"/2006/main" || n.name.Space == "http://purl.oclc.org/ooxml/"+kind+"/main"
}
func officeWalk(n *officeNode, fn func(*officeNode)) {
	fn(n)
	for _, c := range n.children {
		officeWalk(c, fn)
	}
}
func officeFind(n *officeNode, local string) []*officeNode {
	out := []*officeNode{}
	officeWalk(n, func(c *officeNode) {
		if c.name.Local == local && c.name.Space == n.name.Space {
			out = append(out, c)
		}
	})
	return out
}
func officeChild(n *officeNode, local string) *officeNode {
	for _, c := range n.children {
		if c.name.Local == local && c.name.Space == n.name.Space {
			return c
		}
	}
	return nil
}
func officeText(n *officeNode) string {
	var b strings.Builder
	officeWalk(n, func(c *officeNode) { b.WriteString(c.text) })
	return b.String()
}
func officeEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func officeValidText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r != 9 && r != 10 && r != 13 && (r < 32 || r == 0xfffe || r == 0xffff) {
			return false
		}
	}
	return true
}

// Patch original byte ranges instead of reserializing XML namespaces, styles,
// relationships or unknown extension markup.
type officePatch struct {
	start, end int
	text       string
}

func officePatchXML(data []byte, patches []officePatch) ([]byte, error) {
	sort.Slice(patches, func(i, j int) bool {
		if patches[i].start == patches[j].start {
			return patches[i].end < patches[j].end
		}
		return patches[i].start < patches[j].start
	})
	var b bytes.Buffer
	pos := 0
	for _, p := range patches {
		if p.start < pos || p.end < p.start || p.end > len(data) {
			return nil, errors.New("overlapping Office edits")
		}
		b.Write(data[pos:p.start])
		b.WriteString(p.text)
		pos = p.end
	}
	b.Write(data[pos:])
	if b.Len() > maxOfficePartBytes {
		return nil, errors.New("edited XML part exceeds 16 MiB")
	}
	return b.Bytes(), nil
}

var officeAttrPattern = regexp.MustCompile(`\s+([^\s=]+)\s*=\s*(?:"[^"]*"|'[^']*')`)

func officeQName(data []byte, n *officeNode) string {
	raw := string(data[n.start+1 : n.openEnd])
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '/' || r == '>' })[0]
}
func officeElement(data []byte, n *officeNode, attrs map[string]string, body string) string {
	opening := string(data[n.start:n.openEnd])
	opening = strings.TrimSuffix(opening, ">")
	opening = strings.TrimSuffix(opening, "/")
	seen := map[string]bool{}
	opening = officeAttrPattern.ReplaceAllStringFunc(opening, func(a string) string {
		m := officeAttrPattern.FindStringSubmatch(a)
		key := m[1]
		value, ok := attrs[key]
		if !ok {
			return a
		}
		seen[key] = true
		if value == "" {
			return ""
		}
		return " " + key + `="` + officeEscape(value) + `"`
	})
	keys := []string{}
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !seen[k] && attrs[k] != "" {
			opening += " " + k + `="` + officeEscape(attrs[k]) + `"`
		}
	}
	return opening + ">" + body + "</" + officeQName(data, n) + ">"
}
func officeBody(data []byte, n *officeNode) string {
	if n.closeStart == n.openEnd && n.end == n.openEnd {
		return ""
	}
	return string(data[n.openEnd:n.closeStart])
}

type officePackage struct {
	archive      *zip.Reader
	parts        map[string][]byte
	format, main string
	sections     []officeSection
}
type officeSection struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Part string `json:"-"`
}
type officeRelationship struct {
	id, kind, target string
	external         bool
}

func readOfficeBytes(root *os.Root, p string) ([]byte, error) {
	return readLocalFile(root, p, maxOfficeFileBytes)
}
func openOffice(ctx context.Context, data []byte, ext string) (*officePackage, error) {
	if ext != ".xlsx" && ext != ".docx" && ext != ".pptx" {
		return nil, errors.New("supported Office formats: .xlsx, .docx and .pptx; convert legacy .xls/.doc/.ppt first")
	}
	if len(data) > maxOfficeFileBytes {
		return nil, errors.New("Office file exceeds 32 MiB")
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("not an unencrypted Office Open XML file")
	}
	if len(z.File) > 4096 {
		return nil, errors.New("Office package exceeds 4096 parts")
	}
	p := &officePackage{archive: z, parts: map[string][]byte{}, format: ext[1:]}
	var total uint64
	for _, f := range z.File {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Name != path.Clean(f.Name) || strings.HasPrefix(f.Name, "/") || strings.HasPrefix(f.Name, "../") || strings.ContainsAny(f.Name, "\\\x00") {
			return nil, errors.New("invalid Office package part path")
		}
		if _, ok := p.parts[f.Name]; ok {
			return nil, errors.New("duplicate Office package part")
		}
		if f.UncompressedSize64 > maxOfficePartBytes || total+f.UncompressedSize64 > maxOfficeExpandedBytes {
			return nil, errors.New("Office package exceeds expanded size limits (16 MiB per part, 64 MiB total)")
		}
		total += f.UncompressedSize64
		r, e := f.Open()
		if e != nil {
			return nil, e
		}
		b, e := io.ReadAll(io.LimitReader(r, maxOfficePartBytes+1))
		closeErr := r.Close()
		if e != nil {
			return nil, e
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(b) > maxOfficePartBytes {
			return nil, errors.New("Office part exceeds expanded size limit")
		}
		p.parts[f.Name] = b
		if strings.EqualFold(path.Base(f.Name), "vbaProject.bin") {
			return nil, errors.New("macro-enabled Office documents are not supported")
		}
	}
	ct, err := p.xml(ctx, "[Content_Types].xml")
	if err != nil {
		return nil, err
	}
	for _, n := range ct.children {
		s := strings.ToLower(n.attr("ContentType"))
		if strings.Contains(s, "macroenabled") || strings.Contains(s, "vbaproject") {
			return nil, errors.New("macro-enabled Office documents are not supported")
		}
	}
	rels, err := p.relationships(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, r := range rels {
		if r.kind == "officeDocument" && !r.external {
			if p.main != "" {
				return nil, errors.New("multiple Office main parts")
			}
			p.main = r.target
		}
	}
	main, err := p.xml(ctx, p.main)
	if err != nil {
		return nil, err
	}
	want := map[string]string{"xlsx": "spreadsheetml", "docx": "wordprocessingml", "pptx": "presentationml"}[p.format]
	if !officeNamespace(main, want) {
		return nil, errors.New("Office contents do not match the filename extension")
	}
	wantRoot := map[string]string{"xlsx": "workbook", "docx": "document", "pptx": "presentation"}[p.format]
	wantType := map[string]string{"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml", "docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml", "pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"}[p.format]
	validType := false
	for _, n := range ct.children {
		if n.name.Local == "Override" && n.attr("PartName") == "/"+p.main && n.attr("ContentType") == wantType {
			validType = true
		}
	}
	if main.name.Local != wantRoot || !validType {
		return nil, errors.New("Office main part has an unsupported content type")
	}
	if err = p.discover(ctx, main); err != nil {
		return nil, err
	}
	return p, nil
}
func (p *officePackage) xml(ctx context.Context, name string) (*officeNode, error) {
	data, ok := p.parts[name]
	if !ok {
		return nil, fmt.Errorf("missing Office part %q", name)
	}
	return parseOfficeXML(ctx, data)
}
func (p *officePackage) relationships(ctx context.Context, part string) ([]officeRelationship, error) {
	name := "_rels/.rels"
	if part != "" {
		name = path.Join(path.Dir(part), "_rels", path.Base(part)+".rels")
	}
	if _, ok := p.parts[name]; !ok {
		if part != "" {
			return nil, nil
		}
		return nil, errors.New("missing Office package relationships")
	}
	root, err := p.xml(ctx, name)
	if err != nil {
		return nil, err
	}
	out := []officeRelationship{}
	seen := map[string]bool{}
	for _, n := range root.children {
		if n.name.Local != "Relationship" {
			continue
		}
		id := n.attr("Id")
		if id == "" || seen[id] {
			return nil, errors.New("invalid Office relationship id")
		}
		seen[id] = true
		r := officeRelationship{id: id, kind: path.Base(n.attr("Type")), external: n.attr("TargetMode") == "External"}
		if !r.external {
			u, e := url.Parse(n.attr("Target"))
			if e != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(u.Path, "\\\x00") {
				return nil, errors.New("unsupported Office relationship target")
			}
			r.target = path.Clean(path.Join(path.Dir(part), u.Path))
			if strings.HasPrefix(u.Path, "/") {
				r.target = strings.TrimPrefix(path.Clean(u.Path), "/")
			}
			if r.target == ".." || strings.HasPrefix(r.target, "../") {
				return nil, errors.New("Office relationship escapes package")
			}
		}
		out = append(out, r)
	}
	return out, nil
}
func (p *officePackage) discover(ctx context.Context, main *officeNode) error {
	rels, err := p.relationships(ctx, p.main)
	if err != nil {
		return err
	}
	byID := map[string]officeRelationship{}
	for _, r := range rels {
		byID[r.id] = r
	}
	if p.format == "docx" {
		p.sections = append(p.sections, officeSection{"document", "Document", "document", p.main})
		counts := map[string]int{}
		for _, r := range rels {
			switch r.kind {
			case "header", "footer", "footnotes", "endnotes":
				if !r.external {
					counts[r.kind]++
					p.sections = append(p.sections, officeSection{fmt.Sprintf("%s%d", r.kind, counts[r.kind]), r.kind, "text", r.target})
				}
			}
		}
		return nil
	}
	nodeName, relKind, prefix := "sheet", "worksheet", "sheet"
	if p.format == "pptx" {
		nodeName = "sldId"
		relKind = "slide"
		prefix = "slide"
	}
	for i, n := range officeFind(main, nodeName) {
		id := ""
		for _, a := range n.attrs {
			if a.Name.Local == "id" && (strings.HasSuffix(a.Name.Space, "/relationships")) {
				id = a.Value
			}
		}
		r, ok := byID[id]
		if !ok || r.external {
			return errors.New("missing internal sheet/slide relationship")
		}
		if r.kind != relKind {
			continue
		} // Chartsheets are preserved but have no editable cell grid.
		sid := prefix + strconv.Itoa(i+1)
		name := n.attr("name")
		if name == "" {
			name = sid
		}
		p.sections = append(p.sections, officeSection{sid, name, relKind, r.target})
		if p.format == "pptx" {
			rs, e := p.relationships(ctx, r.target)
			if e != nil {
				return e
			}
			for _, nr := range rs {
				if nr.kind == "notesSlide" && !nr.external {
					p.sections = append(p.sections, officeSection{sid + ".notes", name + " notes", "notes", nr.target})
				}
			}
		}
	}
	return nil
}
func (p *officePackage) encode(ctx context.Context, changed map[string][]byte) ([]byte, error) {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, f := range p.archive.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if data, ok := changed[f.Name]; ok {
			if data == nil {
				continue
			}
			if _, err := parseOfficeXML(ctx, data); err != nil {
				return nil, err
			}
			h := f.FileHeader
			h.Method = zip.Deflate
			out, err := w.CreateHeader(&h)
			if err != nil {
				return nil, err
			}
			if _, err = out.Write(data); err != nil {
				return nil, err
			}
		} else if err := w.Copy(f); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if b.Len() > maxOfficeFileBytes {
		return nil, errors.New("edited Office file exceeds 32 MiB")
	}
	return b.Bytes(), nil
}

func officeTool(ctx context.Context, root *os.Root, filename, fullPath, name string, args map[string]any) (any, error) {
	data, err := readOfficeBytes(root, filename)
	if err != nil {
		return nil, err
	}
	hash := localFileHash(data)
	expected, _ := args["expected_sha256"].(string)
	if expected != "" && strings.ToLower(expected) != hash {
		return nil, errors.New("file changed since it was read; read it again before continuing")
	}
	if name != "filesystem_office_read" && expected == "" {
		return nil, errors.New("read the Office file first and supply expected_sha256")
	}
	p, err := openOffice(ctx, data, strings.ToLower(filepath.Ext(filename)))
	if err != nil {
		return nil, err
	}
	sectionID, _ := args["section"].(string)
	var section *officeSection
	for i := range p.sections {
		if p.sections[i].ID == sectionID {
			section = &p.sections[i]
			break
		}
	}
	if sectionID != "" && section == nil {
		return nil, errors.New("unknown section; list sections with filesystem_office_read")
	}
	if name == "filesystem_office_read" {
		return p.read(ctx, fullPath, hash, section, args)
	}
	if section == nil {
		return nil, errors.New("supply the section id from a prior read")
	}
	for part := range p.parts {
		if strings.HasPrefix(strings.ToLower(part), "_xmlsignatures/") {
			return nil, errors.New("digitally signed Office documents cannot be edited")
		}
	}
	manifest, err := p.xml(ctx, "[Content_Types].xml")
	if err != nil {
		return nil, err
	}
	for _, part := range manifest.children {
		if strings.Contains(strings.ToLower(part.attr("ContentType")), "digital-signature") {
			return nil, errors.New("digitally signed Office documents cannot be edited")
		}
	}
	var changed map[string][]byte
	switch name {
	case "filesystem_office_edit":
		changed, err = p.editText(ctx, *section, args)
	case "filesystem_sheet_set":
		changed, err = p.setCells(ctx, *section, args)
	default:
		return nil, errors.New("unknown Office tool")
	}
	if err != nil {
		return nil, err
	}
	updated, err := p.encode(ctx, changed)
	if err != nil {
		return nil, err
	}
	if _, err = openOffice(ctx, updated, "."+p.format); err != nil {
		return nil, fmt.Errorf("edited package validation failed: %w", err)
	}
	if err = saveLocalFile(ctx, root, filename, updated, expected, readOfficeBytes); err != nil {
		return nil, err
	}
	return map[string]any{"path": fullPath, "saved": true, "sha256": localFileHash(updated), "size_bytes": len(updated), "verify_with": "filesystem_office_read", "formula_calculation": "not performed"}, nil
}

type officeItem struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Kind     string `json:"kind"`
	Formula  string `json:"formula,omitempty"`
	Style    string `json:"style,omitempty"`
	Location string `json:"location,omitempty"`
	Editable bool   `json:"editable"`
	nodes    []*officeNode
}

func (p *officePackage) items(ctx context.Context, s officeSection) ([]officeItem, error) {
	if p.format == "xlsx" {
		return p.sheetItems(ctx, s)
	}
	root, err := p.xml(ctx, s.Part)
	if err != nil {
		return nil, err
	}
	out := []officeItem{}
	// Paragraph ids cover visible runs, including text boxes and table cells.
	kind := "wordprocessingml"
	if p.format == "pptx" {
		kind = "drawingml"
	}
	tables := map[*officeNode]int{}
	tableCount := 0
	officeWalk(root, func(n *officeNode) {
		if n.name.Local == "tbl" {
			tableCount++
			tables[n] = tableCount
		}
	})
	officeWalk(root, func(n *officeNode) {
		if n.name.Local != "p" || !officeNamespace(n, kind) {
			return
		}
		item := officeItem{ID: fmt.Sprintf("p%d", len(out)+1), Kind: "paragraph", Editable: true}
		for a := n.parent; a != nil; a = a.parent {
			if a.name.Local == "tc" {
				row := a.parent
				tbl := row.parent
				if tbl != nil {
					ri, ci := 0, 0
					for _, c := range tbl.children {
						if c.name.Local == "tr" {
							ri++
						}
						if c == row {
							break
						}
					}
					for _, c := range row.children {
						if c.name.Local == "tc" {
							ci++
						}
						if c == a {
							break
						}
					}
					item.Location = fmt.Sprintf("table%d/row%d/cell%d", tables[tbl], ri, ci)
					break
				}
			}
		}
		var visit func(*officeNode)
		visit = func(c *officeNode) {
			if c != n && c.name.Local == "p" && officeNamespace(c, kind) {
				return
			}
			switch c.name.Local {
			case "fld", "fldChar", "fldSimple", "instrText", "ins", "del", "moveFrom", "moveTo", "sdt":
				item.Editable = false
			}
			if officeNamespace(c, kind) {
				switch c.name.Local {
				case "t":
					item.Text += officeText(c)
					item.nodes = append(item.nodes, c)
				case "tab":
					item.Text += "\t"
					item.nodes = append(item.nodes, c)
				case "br", "cr":
					item.Text += "\n"
					item.nodes = append(item.nodes, c)
				}
			}
			for _, child := range c.children {
				visit(child)
			}
		}
		visit(n)
		for a := n.parent; a != nil; a = a.parent {
			switch a.name.Local {
			case "ins", "del", "sdt", "fldSimple":
				item.Editable = false
			}
		}
		out = append(out, item)
	})
	return out, nil
}
func officePreview(s string) string {
	r := []rune(s)
	if len(r) > 400 {
		return string(r[:400])
	}
	return s
}
func (p *officePackage) read(ctx context.Context, filename, hash string, s *officeSection, args map[string]any) (any, error) {
	offset, err := fileToolNumber(args, "offset", 0, 0, maxOfficeExpandedBytes)
	if err != nil {
		return nil, err
	}
	itemID, _ := args["item"].(string)
	field, _ := args["field"].(string)
	if field != "" && field != "text" && field != "formula" {
		return nil, errors.New("field must be text or formula")
	}
	if field == "" {
		field = "text"
	}
	result := map[string]any{"path": filename, "sha256": hash, "format": p.format, "offset": offset}
	if itemID != "" && s == nil {
		return nil, errors.New("item requires a section")
	}
	limit, err := fileToolNumber(args, "limit", 25, 1, 10000)
	if err != nil {
		return nil, err
	}
	var values []any
	key := "sections"
	if s == nil {
		for _, section := range p.sections {
			values = append(values, section)
		}
	} else {
		result["section"] = s
		key = "items"
		items, e := p.items(ctx, *s)
		if e != nil {
			return nil, e
		}
		if itemID != "" {
			for _, item := range items {
				if item.ID != itemID {
					continue
				}
				text := item.Text
				if field == "formula" {
					if p.format != "xlsx" {
						return nil, errors.New("formula is only available for spreadsheets")
					}
					text = item.Formula
				}
				r := []rune(text)
				if offset > len(r) {
					return nil, errors.New("offset beyond item text")
				}
				if _, ok := args["limit"]; !ok {
					limit = 6000
				}
				end := min(len(r), offset+limit)
				for end > offset {
					b, _ := json.Marshal(string(r[offset:end]))
					if len(b) <= 11000 {
						break
					}
					end = offset + (end-offset)/2
				}
				result["item"] = item.ID
				result["kind"] = item.Kind
				result["editable"] = item.Editable
				result["field"] = field
				result["content"] = string(r[offset:end])
				result["total_characters"] = len(r)
				result["next_offset"] = end
				result["truncated"] = end < len(r)
				return result, nil
			}
			return nil, errors.New("unknown item; read the section first")
		}
		for _, item := range items {
			values = append(values, map[string]any{"id": item.ID, "text": officePreview(item.Text), "text_truncated": len([]rune(item.Text)) > 400, "kind": item.Kind, "formula": officePreview(item.Formula), "formula_truncated": len([]rune(item.Formula)) > 400, "style": item.Style, "location": item.Location, "editable": item.Editable})
		}
	}
	if offset > len(values) {
		return nil, errors.New("offset beyond section/item list")
	}
	entries := []any{}
	budget := 11500
	for _, v := range values[offset:min(len(values), offset+min(limit, 100))] {
		raw, _ := json.Marshal(v)
		if len(raw) > budget {
			break
		}
		budget -= len(raw)
		entries = append(entries, v)
	}
	if len(entries) == 0 && offset < len(values) {
		return nil, errors.New("Office item metadata exceeds observation limit")
	}
	result[key] = entries
	result["total"] = len(values)
	result["next_offset"] = offset + len(entries)
	result["truncated"] = offset+len(entries) < len(values)
	if p.format == "xlsx" {
		result["values"] = "Raw stored values; numeric dates use the workbook date system. Formula results are cached and may be stale; formulas are not calculated."
		result["date_system"] = p.dateSystem(ctx)
	}
	return result, nil
}
func (p *officePackage) editText(ctx context.Context, s officeSection, args map[string]any) (map[string][]byte, error) {
	if p.format == "xlsx" {
		return nil, errors.New("use filesystem_sheet_set for Excel cells")
	}
	id, _ := args["item"].(string)
	old, _ := args["old_text"].(string)
	replacement, ok := args["new_text"].(string)
	if old == "" || !ok || len(replacement) > 32768 || !officeValidText(replacement) {
		return nil, errors.New("provide nonempty old_text and XML-compatible new_text up to 32768 bytes")
	}
	if strings.ContainsAny(replacement, "\t\r\n") {
		return nil, errors.New("paragraph edits cannot insert tabs or line/paragraph breaks")
	}
	// Respect document protection; this tool does not remove it.
	for name := range p.parts {
		if strings.HasSuffix(name, "settings.xml") {
			n, err := p.xml(ctx, name)
			if err != nil {
				return nil, err
			}
			if len(officeFind(n, "documentProtection")) > 0 {
				return nil, errors.New("protected Word documents cannot be edited")
			}
		}
	}
	items, err := p.items(ctx, s)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		if !item.Editable {
			return nil, errors.New("this paragraph contains fields, tracked changes or controlled content and is read-only")
		}
		if strings.Count(item.Text, old) != 1 {
			return nil, errors.New("old_text must match exactly once in the selected paragraph")
		}
		start := strings.Index(item.Text, old)
		end := start + len(old)
		pos := 0
		inserted := false
		patches := []officePatch{}
		data := p.parts[s.Part]
		for _, n := range item.nodes {
			text := officeText(n)
			if n.name.Local == "tab" {
				text = "\t"
			}
			if n.name.Local == "br" || n.name.Local == "cr" {
				text = "\n"
			}
			next := pos + len(text)
			if pos < end && next > start {
				if n.name.Local != "t" {
					return nil, errors.New("matches spanning tabs or line breaks cannot be edited")
				}
				left := max(0, start-pos)
				right := min(len(text), end-pos)
				value := text[:left]
				if !inserted {
					value += replacement
					inserted = true
				}
				value += text[right:]
				patches = append(patches, officePatch{n.start, n.end, officeElement(data, n, map[string]string{"xml:space": "preserve"}, officeEscape(value))})
			}
			pos = next
		}
		edited, e := officePatchXML(data, patches)
		if e != nil {
			return nil, e
		}
		return map[string][]byte{s.Part: edited}, nil
	}
	return nil, errors.New("unknown paragraph; read the section first")
}
