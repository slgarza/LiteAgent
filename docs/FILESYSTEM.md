# Filesystem tools

[Project README](../README.md)

Tools operate on real files inside the user's home directory. `filesystem_info` reports the OS, home and actual Desktop. Relative paths resolve against home; `~/` and absolute in-scope paths are supported. Linux XDG and Windows Known Folders locate redirected/localized Desktops. A Desktop outside home is reported as out of scope.

| Tool | Behavior |
| --- | --- |
| `filesystem_info` | Scope, OS, Desktop and limits |
| `filesystem_stat` | Type, size, modification time and permission bits; no content read or final-symlink traversal |
| `filesystem_list` | Paginated metadata, folders first and names sorted |
| `filesystem_read` | UTF-8 content, SHA-256, size and Unicode continuation offset |
| `filesystem_mkdir` | Missing parent directories are created |
| `filesystem_write` | New file, or complete replacement guarded by the previous hash |
| `filesystem_edit` | One unique text-fragment replacement, guarded by the previous hash |
| `filesystem_office_read` | Sections, cell/paragraph previews and paginated full text/formulas from `.xlsx/.docx/.pptx` |
| `filesystem_office_edit` | Unique text replacement within a Word/PowerPoint paragraph, with prior hash |
| `filesystem_sheet_set` | Atomic batch of up to 100 typed Excel cell changes, with prior hash |

No delete, move or shell-execution tool is provided. Plain text tools do not read binary documents; use the dedicated Office tools for supported formats.

## Pagination

Text files must be valid UTF-8 without NUL bytes, at most **1 MiB**. A read returns up to 10,000 characters within the observation budget. Offsets count Unicode code points, not bytes. Pass `sha256` as `expected_sha256` on subsequent pages; restart at offset 0 if the file changed.

Directory scans are bounded to **10,000 entries**; choose a subdirectory for larger folders. Pages return up to 100 entries within the observation budget. Pass `snapshot_sha256` as `expected_snapshot` with `next_offset`. The snapshot covers sorted names and types, detecting additions/removals/renames between pages. It does not hash file contents; use read hashes for that.

## Writes

Read first, retain `sha256`, then supply it as `expected_sha256` for edits/replacements. Read back the saved file to verify the result. New-file creation never silently overwrites. Missing `content`/`new_text` is an error; an explicit empty string creates an empty file or removes the matched fragment. Existing-file updates do not create missing parents. Targeted edits require exactly one match.

Writes stage complete contents, sync and publish. New files use exclusive hard linking; replacements rename after a hash check. Existing permission bits are preserved, but ownership, ACLs, extended attributes and hard-link identity are not promised. New-file publication requires filesystem hard-link support. Temporary files are cleaned up after success/failure.

Operations serialize within LiteAgent: two edits using the same old hash cannot both succeed. The hash check is not an OS transaction against external editors; another process can still write between the final check and rename. Coordinate edits when other programs modify the same file.

## Scope and cancellation

Go's `os.Root` rejects parent traversal and symlink escapes. Writes reject a symbolic-link destination; in-scope symlink directories work. Replacing a hard link changes that entry instead of truncating the shared target. Home containment does not classify sensitive files; only access files needed for the task.

Cancellation is checked before work, during bounded scans and before publishing writes. Completed operations remain in effect. There is no multi-file rollback; parent folders already created may remain after cancellation.

## Office documents

Office support uses the Go standard library directly: no Office, Python, Node or converter is needed at runtime. It edits existing `.xlsx`, `.docx` and `.pptx` files. File access, cancellation, serialization, hash checks, atomic replacement and permission preservation are shared with text tools.

1. Call `filesystem_office_read` with `path` to list sections. IDs identify worksheets in workbook order, the Word document/headers/footers/footnotes/endnotes, or slides and their speaker notes in presentation order.
2. Read a `section` to list cell/paragraph previews. Follow `next_offset` while `truncated`; retain `sha256` as `expected_sha256` on every continuation. A section list reports up to 100 items within the observation budget.
3. Pass `section` and `item` for full text. `offset`/`next_offset` then count Unicode characters instead of entries; reads return up to 10,000 characters within the observation budget. For an Excel cell, use `field: "formula"` to read its formula. `text_truncated` and `formula_truncated` distinguish previews from complete content.
4. Edit with the prior hash, then read the affected item again to verify the actual saved result. IDs must come from the current file version.

### Word and PowerPoint

`filesystem_office_edit` takes `section`, `item`, `old_text`, `new_text` and `expected_sha256`. The old text must occur exactly once in that paragraph. Matches may span multiple styled runs; replacement text inherits the first matched run's formatting. Untouched XML, runs, images, relationships and other package parts are preserved. Table paragraphs include a table/row/cell location. Paragraphs with fields, tracked changes or controlled content are read-only.

Edits do not insert paragraphs, tabs or line breaks, change layouts, manipulate images, or promise that longer text fits on the same page or slide. Existing line breaks/tabs are readable; replacements cannot span them. Text extraction is structural, not a visual rendering. Charts, embedded documents, master/layout text and image text are not interpreted.

### Excel

`filesystem_sheet_set` takes `section`, `expected_sha256` and `cells`, an array of 1–100 objects containing `cell`, `type` and `value`. Addresses use uppercase A1 notation. Values are strings; types are `string`, `number`, `boolean`, `formula` or `blank`. Examples:

```json
{
  "path": "Documents/Budget.xlsx",
  "section": "sheet1",
  "expected_sha256": "<sha256 from the read>",
  "cells": [
    {"cell": "B2", "type": "number", "value": "25"},
    {"cell": "C2", "type": "formula", "value": "B2*3"},
    {"cell": "D2", "type": "string", "value": "Reviewed"}
  ]
}
```

A batch is validated and saved as one update; invalid changes leave the file unchanged. Cells and rows may be added to an existing worksheet. Existing cell styles are retained; new cells use the application's default style. Merged ranges can be changed only through their top-left cell. Shared/array/data-table formulas and their ranges cannot be overwritten. Worksheet creation, formatting changes and structural row/column operations are not provided.

Reads return stored values, shared/inline strings, cell style indexes and formulas. Numeric dates are raw serials, accompanied by the workbook's 1900/1904 date system; number formats are not applied. Formula values are cached results and may be stale. Edits clear the replaced formula's cache, discard the old calculation chain and request full recalculation when Excel/LibreOffice opens the file. LiteAgent does not evaluate formulas or refresh external data. Other formula caches and chart caches may remain stale until the application recalculates.

### Format limits

Maximum compressed file size is **32 MiB**, expanded total **64 MiB**, each part **16 MiB**, and **4,096** archive entries. XML is bounded to 200,000 elements and 128 levels per part. Package CRCs and changed XML are checked before publication. External package relationships are never fetched, and embedded code is never executed.

Legacy `.xls/.doc/.ppt`, PDFs, encrypted files and macro-enabled Office documents are unsupported; convert them in the appropriate application first. Digitally signed documents and protected Word documents/worksheets cannot be edited. Unusual or malformed worksheet structures are rejected instead of rewritten. This support does not create new Office files or control desktop application windows.
