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

No delete, move, shell-execution or binary-document tool is provided. Metadata can be inspected for binary files, but text tools do not read their contents.

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
