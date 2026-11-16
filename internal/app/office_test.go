package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testWordNS = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
const testSheetNS = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
const testSlideNS = "http://schemas.openxmlformats.org/presentationml/2006/main"
const testDrawingNS = "http://schemas.openxmlformats.org/drawingml/2006/main"
const testRelNS = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"

func officeTestRelationships(entries string) string {
	return `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + entries + `</Relationships>`
}
func officeTestRel(id, kind, target string) string {
	return `<Relationship Id="` + id + `" Type="` + testRelNS + `/` + kind + `" Target="` + target + `"/>`
}
func officeTestParts(format string) map[string]string {
	main := map[string]string{"docx": "word/document.xml", "xlsx": "xl/workbook.xml", "pptx": "ppt/presentation.xml"}[format]
	typ := map[string]string{"docx": "wordprocessingml.document", "xlsx": "spreadsheetml.sheet", "pptx": "presentationml.presentation"}[format]
	p := map[string]string{"[Content_Types].xml": `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/` + main + `" ContentType="application/vnd.openxmlformats-officedocument.` + typ + `.main+xml"/></Types>`, "_rels/.rels": officeTestRelationships(officeTestRel("main", "officeDocument", main)), "custom/untouched.bin": "opaque data \x00 preserved"}
	switch format {
	case "docx":
		p[main] = `<w:document xmlns:w="` + testWordNS + `"><w:body><w:p><w:r><w:rPr><w:b/></w:rPr><w:t>Hola </w:t></w:r><w:r><w:rPr><w:i/></w:rPr><w:t>mundo 🌎</w:t></w:r></w:p><w:tbl><w:tr><w:tc><w:p><w:r><w:t>Total: 10</w:t></w:r></w:p></w:tc></w:tr></w:tbl><w:p><w:fldSimple w:instr="DATE"><w:r><w:t>2026</w:t></w:r></w:fldSimple></w:p></w:body></w:document>`
		p["word/_rels/document.xml.rels"] = officeTestRelationships(officeTestRel("header", "header", "header1.xml"))
		p["word/header1.xml"] = `<w:hdr xmlns:w="` + testWordNS + `"><w:p><w:r><w:t>Encabezado</w:t></w:r></w:p></w:hdr>`
	case "xlsx":
		p[main] = `<workbook xmlns="` + testSheetNS + `" xmlns:r="` + testRelNS + `"><workbookPr date1904="1"/><sheets><sheet name="Ventas" sheetId="1" r:id="sheet"/></sheets><calcPr calcMode="manual"/></workbook>`
		p["xl/_rels/workbook.xml.rels"] = officeTestRelationships(officeTestRel("sheet", "worksheet", "worksheets/sheet9.xml") + officeTestRel("strings", "sharedStrings", "strings.xml") + officeTestRel("chain", "calcChain", "calcChain.xml"))
		p["xl/strings.xml"] = `<sst xmlns="` + testSheetNS + `" count="1" uniqueCount="1"><si><r><t>Nombre </t></r><r><t>🌎</t></r><rPh sb="0" eb="1"><t>phonetic</t></rPh></si></sst>`
		p["xl/worksheets/sheet9.xml"] = `<worksheet xmlns="` + testSheetNS + `"><dimension ref="A1:D3"/><sheetData><row r="1" spans="1:4"><c r="A1" s="2" t="s"><v>0</v></c><c r="C1"><v>10</v></c><c r="D1"><f>C1*2</f><v>20</v></c></row><row r="3"><c r="A3" t="inlineStr"><is><t>Keep me</t></is></c></row></sheetData><mergeCells count="1"><mergeCell ref="A3:B3"/></mergeCells></worksheet>`
		p["xl/calcChain.xml"] = `<calcChain xmlns="` + testSheetNS + `"><c r="D1" i="1"/></calcChain>`
		p["[Content_Types].xml"] = strings.Replace(p["[Content_Types].xml"], "</Types>", `<Override PartName="/xl/calcChain.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.calcChain+xml"/></Types>`, 1)
	case "pptx":
		p[main] = `<p:presentation xmlns:p="` + testSlideNS + `" xmlns:r="` + testRelNS + `"><p:sldIdLst><p:sldId id="256" r:id="second"/><p:sldId id="257" r:id="first"/></p:sldIdLst></p:presentation>`
		p["ppt/_rels/presentation.xml.rels"] = officeTestRelationships(officeTestRel("first", "slide", "slides/slide1.xml") + officeTestRel("second", "slide", "slides/slide2.xml"))
		for i := 1; i <= 2; i++ {
			p[fmt.Sprintf("ppt/slides/slide%d.xml", i)] = `<p:sld xmlns:p="` + testSlideNS + `" xmlns:a="` + testDrawingNS + `"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:rPr b="1"/><a:t>Slide </a:t></a:r><a:r><a:t>` + fmt.Sprint(i) + `</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`
		}
		p["ppt/slides/_rels/slide2.xml.rels"] = officeTestRelationships(officeTestRel("notes", "notesSlide", "../notesSlides/notesSlide1.xml"))
		p["ppt/notesSlides/notesSlide1.xml"] = `<p:notes xmlns:p="` + testSlideNS + `" xmlns:a="` + testDrawingNS + `"><p:cSld><a:p><a:r><a:t>Speaker notes</a:t></a:r></a:p></p:cSld></p:notes>`
	}
	return p
}
func officeTestZip(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	keys := []string{}
	for k := range parts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f, e := w.Create(k)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write([]byte(parts[k])); e != nil {
			t.Fatal(e)
		}
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func officeTestFile(t *testing.T, a *App, format string, parts map[string]string) string {
	t.Helper()
	name := "sample." + format
	if err := os.WriteFile(filepath.Join(a.filesHome, name), officeTestZip(t, parts), 0640); err != nil {
		t.Fatal(err)
	}
	return name
}
func officeTestError(t *testing.T, a *App, name string, args map[string]any, contains string) {
	t.Helper()
	_, err := a.filesystemTool(context.Background(), name, args)
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("wanted error containing %q, got %v", contains, err)
	}
}
func officeTestLoad(t *testing.T, a *App, name string) *officePackage {
	t.Helper()
	data, e := os.ReadFile(filepath.Join(a.filesHome, name))
	if e != nil {
		t.Fatal(e)
	}
	p, e := openOffice(context.Background(), data, filepath.Ext(name))
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func TestOfficeWordReadEditPreservesRunsAndPackage(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	parts := officeTestParts("docx")
	name := officeTestFile(t, a, "docx", parts)
	info := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name})
	if len(info["sections"].([]any)) != 2 {
		t.Fatal(info)
	}
	page := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name, "section": "document"})
	items := page["items"].([]any)
	if items[0].(map[string]any)["text"] != "Hola mundo 🌎" || items[1].(map[string]any)["location"] != "table1/row1/cell1" || items[2].(map[string]any)["editable"] != false {
		t.Fatal(page)
	}
	args := map[string]any{"path": name, "section": "document", "item": "p1", "old_text": "a mundo", "new_text": "a equipo & amigos", "expected_sha256": info["sha256"]}
	saved := fileTool(t, a, "filesystem_office_edit", args)
	read := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name, "section": "document", "item": "p1"})
	if read["content"] != "Hola equipo & amigos 🌎" || read["sha256"] != saved["sha256"] {
		t.Fatal(read)
	}
	after := officeTestLoad(t, a, name)
	for part, original := range parts {
		if part != "word/document.xml" && string(after.parts[part]) != original {
			t.Fatalf("unrelated part changed: %s", part)
		}
	}
	xml := string(after.parts["word/document.xml"])
	for _, keep := range []string{"<w:rPr><w:b/></w:rPr>", "<w:rPr><w:i/></w:rPr>", "Total: 10", "xml:space=\"preserve\""} {
		if !strings.Contains(xml, keep) {
			t.Fatal("lost formatting/content", keep)
		}
	}
	officeTestError(t, a, "filesystem_office_edit", args, "changed since")
	args["expected_sha256"] = saved["sha256"]
	args["item"] = "p3"
	args["old_text"] = "2026"
	officeTestError(t, a, "filesystem_office_edit", args, "read-only")
	args["item"] = "p2"
	args["old_text"] = "10"
	args["new_text"] = "20"
	table := fileTool(t, a, "filesystem_office_edit", args)
	if table["saved"] != true {
		t.Fatal(table)
	}
	if runtime.GOOS != "windows" {
		stat, _ := os.Stat(filepath.Join(a.filesHome, name))
		if stat.Mode().Perm() != 0640 {
			t.Fatal("lost permissions")
		}
	}
}
func TestOfficePresentationUsesSlideOrderAndNotes(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	parts := officeTestParts("pptx")
	name := officeTestFile(t, a, "pptx", parts)
	read := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name, "section": "slide1", "item": "p1"})
	if read["content"] != "Slide 2" {
		t.Fatal(read)
	}
	fileTool(t, a, "filesystem_office_edit", map[string]any{"path": name, "section": "slide1", "item": "p1", "old_text": "Slide 2", "new_text": "Presentación", "expected_sha256": read["sha256"]})
	notes := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name, "section": "slide1.notes", "item": "p1"})
	if notes["content"] != "Speaker notes" {
		t.Fatal(notes)
	}
	after := officeTestLoad(t, a, name)
	for part, original := range parts {
		if part != "ppt/slides/slide2.xml" && string(after.parts[part]) != original {
			t.Fatalf("unrelated slide part changed: %s", part)
		}
	}
}
func TestOfficeSpreadsheetTypedEditsAndFormulaInvalidation(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	parts := officeTestParts("xlsx")
	name := officeTestFile(t, a, "xlsx", parts)
	page := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name, "section": "sheet1"})
	items := page["items"].([]any)
	if items[0].(map[string]any)["text"] != "Nombre 🌎" || items[2].(map[string]any)["formula"] != "C1*2" || page["date_system"] != "1904" {
		t.Fatal(page)
	}
	change := func(cell, kind, value string) map[string]any {
		return map[string]any{"cell": cell, "type": kind, "value": value}
	}
	saved := fileTool(t, a, "filesystem_sheet_set", map[string]any{"path": name, "section": "sheet1", "expected_sha256": page["sha256"], "cells": []any{change("A1", "string", " Literal _x0041_ & 🌎 "), change("B1", "boolean", "true"), change("C1", "number", "15.5"), change("D1", "formula", "C1*3"), change("A2", "string", "New row"), change("C2", "blank", ""), change("XFD1048576", "number", "42")}})
	p := officeTestLoad(t, a, name)
	if p.parts["xl/calcChain.xml"] != nil || strings.Contains(string(p.parts["xl/_rels/workbook.xml.rels"]), "calcChain") || strings.Contains(string(p.parts["[Content_Types].xml"]), "calcChain") {
		t.Fatal("stale dependency chain retained")
	}
	for _, part := range []string{"xl/strings.xml", "custom/untouched.bin"} {
		if string(p.parts[part]) != parts[part] {
			t.Fatal("unrelated workbook part changed", part)
		}
	}
	cells, err := p.sheetItems(context.Background(), p.sections[0])
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]officeItem{}
	for _, c := range cells {
		byID[c.ID] = c
	}
	if byID["A1"].Text != " Literal _x0041_ & 🌎 " || byID["A1"].Style != "2" || byID["B1"].Text != "true" || byID["C1"].Text != "15.5" || byID["D1"].Formula != "C1*3" || byID["D1"].Text != "" || byID["A2"].Text != "New row" || byID["A3"].Text != "Keep me" || byID["XFD1048576"].Text != "42" {
		t.Fatal(byID)
	}
	if !strings.Contains(string(p.parts[p.main]), `fullCalcOnLoad="1"`) {
		t.Fatal("recalculation not requested")
	}
	formula := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name, "section": "sheet1", "item": "D1", "field": "formula"})
	if formula["content"] != "C1*3" {
		t.Fatal(formula)
	}
	args := map[string]any{"path": name, "section": "sheet1", "expected_sha256": saved["sha256"], "cells": []any{change("B3", "string", "bad")}}
	officeTestError(t, a, "filesystem_sheet_set", args, "merged range")
	args["cells"] = []any{change("A1", "string", "should roll back"), change("C2", "number", "NaN")}
	officeTestError(t, a, "filesystem_sheet_set", args, "finite decimal")
	current, _ := os.ReadFile(filepath.Join(a.filesHome, name))
	if localFileHash(current) != saved["sha256"] {
		t.Fatal("invalid batch modified file")
	}
}
func TestOfficeEmptyAndPrefixedWorksheets(t *testing.T) {
	for _, sheet := range []string{`<worksheet xmlns="` + testSheetNS + `"><sheetData/></worksheet>`, `<x:worksheet xmlns:x="` + testSheetNS + `"><x:sheetData><x:row r="1"/></x:sheetData></x:worksheet>`} {
		t.Run(fmt.Sprint(len(sheet)), func(t *testing.T) {
			a := newTestApp(t)
			a.filesHome = t.TempDir()
			parts := officeTestParts("xlsx")
			parts["xl/worksheets/sheet9.xml"] = sheet
			name := officeTestFile(t, a, "xlsx", parts)
			read := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name})
			fileTool(t, a, "filesystem_sheet_set", map[string]any{"path": name, "section": "sheet1", "expected_sha256": read["sha256"], "cells": []any{map[string]any{"cell": "A1", "type": "string", "value": "first"}, map[string]any{"cell": "B2", "type": "number", "value": "2"}}})
			read = fileTool(t, a, "filesystem_office_read", map[string]any{"path": name, "section": "sheet1", "item": "A1"})
			if read["content"] != "first" {
				t.Fatal(read)
			}
		})
	}
}
func TestOfficeLongTextPaginationAndHash(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	parts := officeTestParts("docx")
	value := strings.Repeat("<&🌎", 10000)
	parts["word/document.xml"] = `<w:document xmlns:w="` + testWordNS + `"><w:body><w:p><w:r><w:t>` + officeEscape(value) + `</w:t></w:r></w:p></w:body></w:document>`
	name := officeTestFile(t, a, "docx", parts)
	list := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name, "section": "document"})
	if list["items"].([]any)[0].(map[string]any)["text_truncated"] != true {
		t.Fatal("preview must disclose truncation")
	}
	offset := float64(0)
	var b strings.Builder
	for {
		r := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name, "section": "document", "item": "p1", "offset": offset, "limit": 10000, "expected_sha256": list["sha256"]})
		b.WriteString(r["content"].(string))
		offset = r["next_offset"].(float64)
		if !r["truncated"].(bool) {
			break
		}
	}
	if b.String() != value {
		t.Fatal("long Unicode text lost on pagination")
	}
	parts["word/header1.xml"] += " "
	officeTestFile(t, a, "docx", parts)
	officeTestError(t, a, "filesystem_office_read", map[string]any{"path": name, "expected_sha256": list["sha256"]}, "changed since")
}
func TestOfficeRejectsUnsafePackagesAndEdits(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"traversal", func(p map[string]string) { p["../escape"] = "bad" }, "part path"},
		{"macro", func(p map[string]string) { p["word/vbaProject.bin"] = "macro" }, "macro-enabled"},
		{"doctype", func(p map[string]string) {
			p["word/document.xml"] = `<!DOCTYPE x [<!ENTITY x SYSTEM "file:///etc/passwd">]>` + p["word/document.xml"]
		}, "directives"},
		{"invalid XML", func(p map[string]string) { p["word/document.xml"] = `<unclosed>` }, "invalid Office XML"},
		{"oversized", func(p map[string]string) { p["media/large.bin"] = strings.Repeat("x", maxOfficePartBytes+1) }, "expanded size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			a.filesHome = t.TempDir()
			parts := officeTestParts("docx")
			tc.mutate(parts)
			name := officeTestFile(t, a, "docx", parts)
			officeTestError(t, a, "filesystem_office_read", map[string]any{"path": name}, tc.want)
		})
	}
	for _, format := range []string{"docx", "xlsx"} {
		t.Run("protection "+format, func(t *testing.T) {
			a := newTestApp(t)
			a.filesHome = t.TempDir()
			parts := officeTestParts(format)
			if format == "docx" {
				parts["word/settings.xml"] = `<w:settings xmlns:w="` + testWordNS + `"><w:documentProtection w:enforcement="1"/></w:settings>`
			} else {
				parts["xl/worksheets/sheet9.xml"] = strings.Replace(parts["xl/worksheets/sheet9.xml"], "</worksheet>", `<sheetProtection sheet="1"/></worksheet>`, 1)
			}
			name := officeTestFile(t, a, format, parts)
			read := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name})
			args := map[string]any{"path": name, "expected_sha256": read["sha256"], "section": "document", "item": "p1", "old_text": "Hola", "new_text": "Hi"}
			tool := "filesystem_office_edit"
			if format == "xlsx" {
				tool = "filesystem_sheet_set"
				args = map[string]any{"path": name, "expected_sha256": read["sha256"], "section": "sheet1", "cells": []any{map[string]any{"cell": "A1", "type": "string", "value": "hi"}}}
			}
			officeTestError(t, a, tool, args, "protected")
		})
	}
}
func TestOfficeScopeCancellationAndConcurrentEdits(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	name := officeTestFile(t, a, "docx", officeTestParts("docx"))
	read := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name})
	args := map[string]any{"path": name, "section": "document", "item": "p1", "old_text": "Hola", "new_text": "Hi", "expected_sha256": read["sha256"]}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := a.filesystemTool(ctx, "filesystem_office_edit", args); e == nil {
		t.Fatal("cancelled edit succeeded")
	}
	officeTestError(t, a, "filesystem_office_read", map[string]any{"path": "../sample.docx"}, "home directory")
	if runtime.GOOS != "windows" {
		outside := t.TempDir()
		_ = os.WriteFile(filepath.Join(outside, name), officeTestZip(t, officeTestParts("docx")), 0600)
		_ = os.Symlink(outside, filepath.Join(a.filesHome, "escape"))
		if _, e := a.filesystemTool(context.Background(), "filesystem_office_read", map[string]any{"path": "escape/" + name}); e == nil {
			t.Fatal("symlink escaped root")
		}
		_ = os.Symlink(name, filepath.Join(a.filesHome, "link.docx"))
		linked := map[string]any{}
		for k, v := range args {
			linked[k] = v
		}
		linked["path"] = "link.docx"
		officeTestError(t, a, "filesystem_office_edit", linked, "symbolic link")
	}
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.filesystemTool(context.Background(), "filesystem_office_edit", args); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("stale concurrent edit was accepted")
	}
	entries, _ := os.ReadDir(a.filesHome)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".liteagent-") {
			t.Fatal("temporary file leaked")
		}
	}
}
func TestAgentEditsOfficeDocumentWithoutBrowser(t *testing.T) {
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	name := officeTestFile(t, a, "docx", officeTestParts("docx"))
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []Message `json:"messages"`
		}
		if e := json.NewDecoder(r.Body).Decode(&payload); e != nil {
			t.Error(e)
			return
		}
		n := calls.Add(1)
		tool := "filesystem_office_read"
		args := map[string]any{"path": name, "section": "document", "item": "p1"}
		if n == 2 {
			var observed map[string]any
			_ = json.Unmarshal([]byte(payload.Messages[len(payload.Messages)-1].Content), &observed)
			tool = "filesystem_office_edit"
			args = map[string]any{"path": name, "section": "document", "item": "p1", "old_text": "mundo", "new_text": "equipo", "expected_sha256": observed["sha256"]}
		}
		if n == 4 {
			var observed map[string]any
			_ = json.Unmarshal([]byte(payload.Messages[len(payload.Messages)-1].Content), &observed)
			if observed["content"] != "Hola equipo 🌎" {
				t.Error("edited document not verified", observed)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"message": Message{Role: "assistant", Content: "Documento editado y verificado."}})
			return
		}
		raw, _ := json.Marshal(args)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": Message{Role: "assistant", ToolCalls: []ToolCall{{Function: FunctionCall{tool, raw}}}}})
	}))
	defer model.Close()
	_ = a.store.Update(func(st *State) error { st.Config.BaseURL = model.URL; return nil })
	run := postChat(t, a, "", "Cambia mundo por equipo en sample.docx y verifica.")
	done := waitRun(t, a, run.ID)
	if done.Status != "completed" || calls.Load() != 4 {
		t.Fatal(done.Status, done.Result)
	}
	if a.browser.session != "" {
		t.Fatal("Office operation launched browser")
	}
}

// This optional check uses independent document writers/readers, then asks an
// isolated LibreOffice process to render only the generated synthetic fixtures.
func TestOfficeApplicationCompatibility(t *testing.T) {
	python := os.Getenv("LITEAGENT_OFFICE_TEST_PYTHON")
	if python == "" {
		t.Skip("set LITEAGENT_OFFICE_TEST_PYTHON to a Python with python-docx, python-pptx and openpyxl")
	}
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	script := filepath.Join("..", "..", "scripts", "test-office.py")
	run := func(command string, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, command, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("Office compatibility check: %v\n%s", err, out)
		}
		t.Log(string(out))
	}
	run(python, script, "create", a.filesHome)
	for _, f := range []struct{ path, section string }{{"document.docx", "document"}, {"deck.pptx", "slide1"}} {
		read := fileTool(t, a, "filesystem_office_read", map[string]any{"path": f.path, "section": f.section})
		id := ""
		for _, value := range read["items"].([]any) {
			item := value.(map[string]any)
			if item["text"] == "Hola mundo" {
				id = item["id"].(string)
			}
		}
		if id == "" {
			t.Fatal("fixture paragraph not found")
		}
		fileTool(t, a, "filesystem_office_edit", map[string]any{"path": f.path, "section": f.section, "item": id, "old_text": "mundo", "new_text": "equipo", "expected_sha256": read["sha256"]})
	}
	read := fileTool(t, a, "filesystem_office_read", map[string]any{"path": "report.xlsx", "section": "sheet1"})
	fileTool(t, a, "filesystem_sheet_set", map[string]any{"path": "report.xlsx", "section": "sheet1", "expected_sha256": read["sha256"], "cells": []any{map[string]any{"cell": "B2", "type": "number", "value": "25"}, map[string]any{"cell": "C2", "type": "formula", "value": "B2*3"}, map[string]any{"cell": "F5", "type": "string", "value": "Nuevo 🌎"}}})
	run(python, script, "verify", a.filesHome)
	if office := os.Getenv("LITEAGENT_OFFICE_TEST_LIBREOFFICE"); office != "" {
		outdir := t.TempDir()
		profile := t.TempDir()
		u := url.URL{Scheme: "file", Path: filepath.ToSlash(profile)}
		run(office, "-env:UserInstallation="+u.String(), "--headless", "--convert-to", "pdf", "--outdir", outdir, filepath.Join(a.filesHome, "document.docx"), filepath.Join(a.filesHome, "deck.pptx"), filepath.Join(a.filesHome, "report.xlsx"))
		for _, name := range []string{"document.pdf", "deck.pdf", "report.pdf"} {
			data, err := os.ReadFile(filepath.Join(outdir, name))
			if err != nil || !bytes.HasPrefix(data, []byte("%PDF-")) || len(data) < 1000 {
				t.Fatalf("LibreOffice did not render %s: %v", name, err)
			}
		}
	}
}

func TestOfficeRefusesFormulaRangesSignaturesAndDuplicateParts(t *testing.T) {
	for _, kind := range []string{"shared", "array", "dataTable"} {
		t.Run(kind, func(t *testing.T) {
			a := newTestApp(t)
			a.filesHome = t.TempDir()
			parts := officeTestParts("xlsx")
			parts["xl/worksheets/sheet9.xml"] = strings.Replace(parts["xl/worksheets/sheet9.xml"], "<f>C1*2</f>", `<f t="`+kind+`" ref="D1:D4">C1*2</f>`, 1)
			name := officeTestFile(t, a, "xlsx", parts)
			read := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name})
			args := map[string]any{"path": name, "section": "sheet1", "expected_sha256": read["sha256"], "cells": []any{map[string]any{"cell": "D3", "type": "number", "value": "3"}}}
			officeTestError(t, a, "filesystem_sheet_set", args, "formula range")
		})
	}
	a := newTestApp(t)
	a.filesHome = t.TempDir()
	parts := officeTestParts("docx")
	parts["[Content_Types].xml"] = strings.Replace(parts["[Content_Types].xml"], "</Types>", `<Override PartName="/signatures/signature.xml" ContentType="application/vnd.openxmlformats-package.digital-signature-xmlsignature+xml"/></Types>`, 1)
	parts["signatures/signature.xml"] = "<signature/>"
	name := officeTestFile(t, a, "docx", parts)
	read := fileTool(t, a, "filesystem_office_read", map[string]any{"path": name})
	officeTestError(t, a, "filesystem_office_edit", map[string]any{"path": name, "section": "document", "item": "p1", "old_text": "Hola", "new_text": "Hi", "expected_sha256": read["sha256"]}, "digitally signed")
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for i := 0; i < 2; i++ {
		f, err := w.Create("same.xml")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte("<x/>"))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openOffice(context.Background(), b.Bytes(), ".docx"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatal("accepted duplicate ZIP entries", err)
	}
}
