package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

func sheetCellAddress(s string) (col, row int, err error) {
	i := 0
	for i < len(s) && s[i] >= 'A' && s[i] <= 'Z' {
		col = col*26 + int(s[i]-'A'+1)
		if col > 16384 {
			return 0, 0, errors.New("cell column exceeds XFD")
		}
		i++
	}
	if i == 0 || i == len(s) || s[i] == '0' {
		return 0, 0, errors.New("use an uppercase A1 cell address")
	}
	row, err = strconv.Atoi(s[i:])
	if err != nil || row < 1 || row > 1048576 {
		return 0, 0, errors.New("cell row must be from 1 to 1048576")
	}
	for _, c := range s[i:] {
		if c < '0' || c > '9' {
			return 0, 0, errors.New("invalid cell address")
		}
	}
	return col, row, nil
}
func sheetRangeContains(ref, cell string) (bool, error) {
	pair := strings.Split(ref, ":")
	if len(pair) == 1 {
		pair = append(pair, pair[0])
	}
	if len(pair) != 2 {
		return false, errors.New("unsupported worksheet range")
	}
	c1, r1, e := sheetCellAddress(pair[0])
	if e != nil {
		return false, e
	}
	c2, r2, e := sheetCellAddress(pair[1])
	if e != nil {
		return false, e
	}
	c, r, e := sheetCellAddress(cell)
	if e != nil {
		return false, e
	}
	if c1 > c2 || r1 > r2 {
		return false, errors.New("inverted worksheet range")
	}
	return c >= c1 && c <= c2 && r >= r1 && r <= r2, nil
}
func sheetString(n *officeNode) string {
	var b strings.Builder
	var walk func(*officeNode)
	walk = func(c *officeNode) {
		if c.name.Local == "rPh" {
			return
		}
		if c.name.Local == "t" && officeNamespace(c, "spreadsheetml") {
			b.WriteString(officeText(c))
			return
		}
		for _, child := range c.children {
			walk(child)
		}
	}
	walk(n)
	return sheetDecodeString(b.String())
}

var sheetEscapedChar = regexp.MustCompile(`_x[0-9A-Fa-f]{4}_`)

func sheetDecodeString(s string) string {
	// Decode in one pass: _x005F_x0041_ means the literal text _x0041_.
	units := []uint16{}
	pos := 0
	for _, match := range sheetEscapedChar.FindAllStringIndex(s, -1) {
		units = append(units, utf16.Encode([]rune(s[pos:match[0]]))...)
		n, _ := strconv.ParseUint(s[match[0]+2:match[1]-1], 16, 16)
		units = append(units, uint16(n))
		pos = match[1]
	}
	units = append(units, utf16.Encode([]rune(s[pos:]))...)
	return string(utf16.Decode(units))
}

func sheetEncodeString(s string) string {
	return sheetEscapedChar.ReplaceAllStringFunc(s, func(match string) string { return "_x005F_" + match[1:] })
}
func (p *officePackage) sharedStrings(ctx context.Context) ([]string, error) {
	rels, err := p.relationships(ctx, p.main)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, r := range rels {
		if r.kind != "sharedStrings" || r.external {
			continue
		}
		root, e := p.xml(ctx, r.target)
		if e != nil {
			return nil, e
		}
		for _, n := range root.children {
			if n.name.Local == "si" {
				out = append(out, sheetString(n))
			}
		}
	}
	return out, nil
}
func (p *officePackage) dateSystem(ctx context.Context) string {
	root, err := p.xml(ctx, p.main)
	if err == nil {
		if n := officeChild(root, "workbookPr"); n != nil && (n.attr("date1904") == "1" || n.attr("date1904") == "true") {
			return "1904"
		}
	}
	return "1900"
}
func (p *officePackage) sheetItems(ctx context.Context, s officeSection) ([]officeItem, error) {
	root, err := p.xml(ctx, s.Part)
	if err != nil {
		return nil, err
	}
	if !officeNamespace(root, "spreadsheetml") || root.name.Local != "worksheet" {
		return nil, errors.New("section is not a worksheet")
	}
	shared, err := p.sharedStrings(ctx)
	if err != nil {
		return nil, err
	}
	out := []officeItem{}
	seen := map[string]bool{}
	sd := officeChild(root, "sheetData")
	if sd == nil {
		return nil, errors.New("worksheet has no sheetData")
	}
	for _, row := range sd.children {
		for _, n := range row.children {
			if n.name.Local != "c" || n.name.Space != root.name.Space {
				continue
			}
			id := n.attr("r")
			if _, _, err = sheetCellAddress(id); err != nil {
				return nil, fmt.Errorf("worksheet cell requires explicit address: %w", err)
			}
			if seen[id] {
				return nil, errors.New("duplicate worksheet cell address")
			}
			seen[id] = true
			item := officeItem{ID: id, Kind: "number", Style: n.attr("s"), Editable: true}
			if v := officeChild(n, "v"); v != nil {
				item.Text = officeText(v)
			}
			switch n.attr("t") {
			case "s":
				i, e := strconv.Atoi(item.Text)
				if e != nil || i < 0 || i >= len(shared) {
					return nil, errors.New("invalid shared string reference")
				}
				item.Text = shared[i]
				item.Kind = "string"
			case "inlineStr":
				item.Kind = "string"
				item.Text = ""
				if v := officeChild(n, "is"); v != nil {
					item.Text = sheetString(v)
				}
			case "str":
				item.Kind = "string"
				item.Text = sheetDecodeString(item.Text)
			case "b":
				item.Kind = "boolean"
				if item.Text == "1" {
					item.Text = "true"
				} else if item.Text == "0" {
					item.Text = "false"
				}
			case "d":
				item.Kind = "date"
			case "e":
				item.Kind = "error"
			}
			if f := officeChild(n, "f"); f != nil {
				item.Formula = officeText(f)
				item.Kind = "cached_formula_result"
				if f.attr("t") != "" && f.attr("t") != "normal" {
					item.Editable = false
					item.Kind += " (" + f.attr("t") + ")"
				}
			}
			out = append(out, item)
		}
	}
	return out, nil
}

type officeCellChange struct {
	cell, kind, value string
	col, row          int
}

var officeNumberPattern = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func officeCellChanges(args map[string]any) ([]officeCellChange, error) {
	values, ok := args["cells"].([]any)
	if !ok || len(values) == 0 || len(values) > 100 {
		return nil, errors.New("cells must be an array of 1 to 100 changes")
	}
	out := []officeCellChange{}
	seen := map[string]bool{}
	for _, v := range values {
		m, ok := v.(map[string]any)
		if !ok || len(m) != 3 {
			return nil, errors.New("each cell change requires only cell, type and value")
		}
		cell, cok := m["cell"].(string)
		kind, kok := m["type"].(string)
		value, vok := m["value"].(string)
		if !cok || !kok || !vok {
			return nil, errors.New("cell, type and value must be strings")
		}
		col, row, err := sheetCellAddress(cell)
		if err != nil {
			return nil, err
		}
		if seen[cell] {
			return nil, errors.New("duplicate cell in edit batch")
		}
		seen[cell] = true
		if !officeValidText(value) {
			return nil, errors.New("cell value must be XML-compatible text")
		}
		switch kind {
		case "string":
			if len(utf16.Encode([]rune(value))) > 32767 {
				return nil, errors.New("Excel cell text exceeds 32767 UTF-16 characters")
			}
		case "number":
			n, e := strconv.ParseFloat(value, 64)
			if !officeNumberPattern.MatchString(value) || e != nil || math.IsInf(n, 0) || math.IsNaN(n) {
				return nil, errors.New("number must be a finite decimal value")
			}
		case "boolean":
			if value != "true" && value != "false" {
				return nil, errors.New("boolean must be true or false")
			}
		case "formula":
			if value == "" || strings.HasPrefix(value, "=") || len(utf16.Encode([]rune(value))) > 8192 {
				return nil, errors.New("formula must be nonempty, at most 8192 characters and omit the leading =")
			}
		case "blank":
			if value != "" {
				return nil, errors.New("blank cells require an empty value")
			}
		default:
			return nil, errors.New("cell type must be string, number, boolean, formula or blank")
		}
		out = append(out, officeCellChange{cell, kind, value, col, row})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].row != out[j].row {
			return out[i].row < out[j].row
		}
		return out[i].col < out[j].col
	})
	return out, nil
}
func officePrefix(data []byte, n *officeNode) string {
	q := officeQName(data, n)
	i := strings.IndexByte(q, ':')
	if i >= 0 {
		return q[:i+1]
	}
	return ""
}
func sheetCellXML(data []byte, n *officeNode, prefix string, c officeCellChange) (string, error) {
	typ := ""
	body := ""
	if n != nil {
		prefix = officePrefix(data, n)
	}
	tag := func(name, value string) string {
		return "<" + prefix + name + ">" + officeEscape(value) + "</" + prefix + name + ">"
	}
	switch c.kind {
	case "string":
		typ = "inlineStr"
		body = "<" + prefix + "is><" + prefix + `t xml:space="preserve">` + officeEscape(sheetEncodeString(c.value)) + "</" + prefix + "t></" + prefix + "is>"
	case "number":
		body = tag("v", c.value)
	case "boolean":
		typ = "b"
		v := "0"
		if c.value == "true" {
			v = "1"
		}
		body = tag("v", v)
	case "formula":
		body = tag("f", c.value)
	}
	if n == nil {
		t := ""
		if typ != "" {
			t = ` t="` + typ + `"`
		}
		return "<" + prefix + `c r="` + c.cell + `"` + t + ">" + body + "</" + prefix + "c>", nil
	}
	// Retain extension metadata after the cell's new value; style/other attributes
	// stay on the original cell element.
	for _, child := range n.children {
		if child.name.Space != n.name.Space || child.name.Local != "v" && child.name.Local != "is" && child.name.Local != "f" {
			body += string(data[child.start:child.end])
		}
	}
	return officeElement(data, n, map[string]string{"t": typ}, body), nil
}
func (p *officePackage) setCells(ctx context.Context, s officeSection, args map[string]any) (map[string][]byte, error) {
	if p.format != "xlsx" {
		return nil, errors.New("filesystem_sheet_set requires .xlsx")
	}
	changes, err := officeCellChanges(args)
	if err != nil {
		return nil, err
	}
	root, err := p.xml(ctx, s.Part)
	if err != nil {
		return nil, err
	}
	if !officeNamespace(root, "spreadsheetml") || root.name.Local != "worksheet" {
		return nil, errors.New("section is not a worksheet")
	}
	if len(officeFind(root, "sheetProtection")) > 0 {
		return nil, errors.New("protected worksheets cannot be edited")
	}
	sd := officeChild(root, "sheetData")
	if sd == nil {
		return nil, errors.New("worksheet has no sheetData")
	}
	rows := map[int]*officeNode{}
	cells := map[string]*officeNode{}
	lastRow := 0
	for _, r := range sd.children {
		if r.name.Local != "row" || r.name.Space != root.name.Space {
			return nil, errors.New("unsupported worksheet row structure")
		}
		rn, e := strconv.Atoi(r.attr("r"))
		if e != nil || rn <= lastRow || rn > 1048576 {
			return nil, errors.New("worksheet rows require ordered explicit row numbers")
		}
		lastRow = rn
		rows[rn] = r
		lastCol := 0
		for _, c := range r.children {
			if c.name.Local != "c" || c.name.Space != root.name.Space {
				continue
			}
			col, row, e := sheetCellAddress(c.attr("r"))
			if e != nil || row != rn || col <= lastCol {
				return nil, errors.New("worksheet cells require ordered explicit addresses")
			}
			lastCol = col
			cells[c.attr("r")] = c
		}
	}
	for _, c := range changes {
		for _, merge := range officeFind(root, "mergeCell") {
			inside, e := sheetRangeContains(merge.attr("ref"), c.cell)
			if e != nil {
				return nil, e
			}
			if inside && strings.Split(merge.attr("ref"), ":")[0] != c.cell {
				return nil, errors.New("edit the top-left cell of a merged range")
			}
		}
		for _, f := range officeFind(root, "f") {
			if f.attr("t") == "" || f.attr("t") == "normal" {
				continue
			}
			if f.parent.attr("r") == c.cell {
				return nil, errors.New("shared, array and data-table formulas cannot be overwritten")
			}
			if ref := f.attr("ref"); ref != "" {
				inside, e := sheetRangeContains(ref, c.cell)
				if e != nil {
					return nil, e
				}
				if inside {
					return nil, errors.New("cell belongs to a shared/array formula range")
				}
			}
		}
	}
	data := p.parts[s.Part]
	prefix := officePrefix(data, root)
	rowChanges := map[int][]officeCellChange{}
	for _, c := range changes {
		rowChanges[c.row] = append(rowChanges[c.row], c)
	}
	rowNumbers := []int{}
	for rn := range rowChanges {
		rowNumbers = append(rowNumbers, rn)
	}
	sort.Ints(rowNumbers)
	// Edit each row independently, then replace/insert complete rows in sheetData.
	patches := []officePatch{}
	insertions := map[int]string{}
	for _, rn := range rowNumbers {
		r := rows[rn]
		edits := rowChanges[rn]
		if r == nil {
			body := ""
			for _, c := range edits {
				x, e := sheetCellXML(data, nil, prefix, c)
				if e != nil {
					return nil, e
				}
				body += x
			}
			text := "<" + prefix + `row r="` + strconv.Itoa(rn) + `">` + body + "</" + prefix + "row>"
			pos := sd.closeStart
			for _, existing := range sd.children {
				n, _ := strconv.Atoi(existing.attr("r"))
				if n > rn {
					pos = existing.start
					break
				}
			}
			insertions[pos] += text
			continue
		}
		cellPatches := []officePatch{}
		newCells := map[int]string{}
		for _, c := range edits {
			existing := cells[c.cell]
			x, e := sheetCellXML(data, existing, officePrefix(data, r), c)
			if e != nil {
				return nil, e
			}
			if existing != nil {
				cellPatches = append(cellPatches, officePatch{existing.start - r.openEnd, existing.end - r.openEnd, x})
			} else {
				pos := r.closeStart
				for _, child := range r.children {
					if child.name.Local == "c" {
						col, _, _ := sheetCellAddress(child.attr("r"))
						if col > c.col {
							pos = child.start
							break
						}
					} else {
						pos = child.start
						break
					}
				}
				newCells[pos-r.openEnd] += x
			}
		}
		for pos, text := range newCells {
			cellPatches = append(cellPatches, officePatch{pos, pos, text})
		}
		body, e := officePatchXML([]byte(officeBody(data, r)), cellPatches)
		if e != nil {
			return nil, e
		}
		patches = append(patches, officePatch{r.start, r.end, officeElement(data, r, map[string]string{"spans": ""}, string(body))})
	}
	for pos, text := range insertions {
		patches = append(patches, officePatch{pos, pos, text})
	}
	// Self-closing sheetData needs expansion before inserting its first rows.
	if sd.end == sd.openEnd {
		text := insertions[sd.closeStart]
		patches = []officePatch{{sd.start, sd.end, officeElement(data, sd, nil, text)}}
	}
	// Dimension is optional. Remove stale bounds so consumers determine them from
	// the actual cell grid, including new rows and columns.
	if dim := officeChild(root, "dimension"); dim != nil {
		patches = append(patches, officePatch{dim.start, dim.end, ""})
	}
	edited, err := officePatchXML(data, patches)
	if err != nil {
		return nil, err
	}
	changed := map[string][]byte{s.Part: edited}
	workbook, err := p.xml(ctx, p.main)
	if err != nil {
		return nil, err
	}
	wb := p.parts[p.main]
	calc := officeChild(workbook, "calcPr")
	attrs := map[string]string{"calcMode": "auto", "fullCalcOnLoad": "1", "forceFullCalc": "1", "calcCompleted": "0"}
	var patch officePatch
	if calc != nil {
		patch = officePatch{calc.start, calc.end, officeElement(wb, calc, attrs, officeBody(wb, calc))}
	} else {
		pos := workbook.closeStart
		for _, child := range workbook.children {
			switch child.name.Local {
			case "oleSize", "customWorkbookViews", "pivotCaches", "smartTagPr", "smartTagTypes", "webPublishing", "fileRecoveryPr", "webPublishObjects", "extLst":
				pos = min(pos, child.start)
			}
		}
		patch = officePatch{pos, pos, "<" + officePrefix(wb, workbook) + `calcPr calcMode="auto" fullCalcOnLoad="1" forceFullCalc="1" calcCompleted="0"/>`}
	}
	changed[p.main], err = officePatchXML(wb, []officePatch{patch})
	if err != nil {
		return nil, err
	}
	// Existing dependency chains become stale after changing formulas. Do not
	// leave an invalid chain for consumers that honor it over fullCalcOnLoad.
	rels, err := p.relationships(ctx, p.main)
	if err != nil {
		return nil, err
	}
	for _, rel := range rels {
		if rel.kind == "calcChain" && !rel.external {
			changed[rel.target] = nil
		}
	}
	relPart := path.Join(path.Dir(p.main), "_rels", path.Base(p.main)+".rels")
	for _, part := range []string{relPart, "[Content_Types].xml"} {
		if _, ok := p.parts[part]; !ok {
			continue
		}
		n, e := p.xml(ctx, part)
		if e != nil {
			return nil, e
		}
		removals := []officePatch{}
		for _, child := range n.children {
			remove := part == relPart && path.Base(child.attr("Type")) == "calcChain"
			if part != relPart {
				value, exists := changed[strings.TrimPrefix(child.attr("PartName"), "/")]
				remove = exists && value == nil
			}
			if remove {
				removals = append(removals, officePatch{child.start, child.end, ""})
			}
		}
		if len(removals) > 0 {
			changed[part], e = officePatchXML(p.parts[part], removals)
			if e != nil {
				return nil, e
			}
		}
	}
	return changed, nil
}
