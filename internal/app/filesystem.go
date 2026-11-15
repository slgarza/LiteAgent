package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const maxLocalFileBytes = 1 << 20
const maxLocalDirectoryEntries = 10000

func filesystemToolDefinitions() []ToolDef {
	path := stringProp("Absolute path within the user's home, ~/path, or a path relative to home. Use filesystem_info for the Desktop path.")
	hash := stringProp("sha256 returned by filesystem_read; required to change an existing file, omitted to create a new file")
	return []ToolDef{
		tool("filesystem_info", "Get the local operating system, home directory, Desktop path and file access scope.", nil),
		tool("filesystem_stat", "Inspect a file, directory or symbolic link without reading its contents. Returns type, size, modification time and permissions. Does not follow the final symbolic link.", map[string]any{"path": path}, "path"),
		tool("filesystem_list", "List a local directory in name order, directories first (up to 10000 entries). Use next_offset and expected_snapshot from snapshot_sha256 for subsequent pages; restart if entries change.", map[string]any{"path": path, "offset": map[string]any{"type": "number", "minimum": 0}, "limit": map[string]any{"type": "number", "minimum": 1, "maximum": 100}, "expected_snapshot": stringProp("snapshot_sha256 from the previous directory page")}, "path"),
		tool("filesystem_read", "Read a UTF-8 text file (up to 1 MiB), with its sha256. Offsets count Unicode characters, not bytes. Use next_offset and expected_sha256 on subsequent pages to avoid mixing file versions.", map[string]any{"path": path, "offset": map[string]any{"type": "number", "minimum": 0}, "limit": map[string]any{"type": "number", "minimum": 1, "maximum": 10000}, "expected_sha256": stringProp("sha256 from a previous read; rejects a file that changed between pages")}, "path"),
		tool("filesystem_mkdir", "Create a local folder and any missing parent folders. Existing directories are preserved.", map[string]any{"path": path}, "path"),
		tool("filesystem_write", "Save complete UTF-8 text, HTML, CSS or code to a local file (up to 1 MiB), creating parent folders. Existing files require expected_sha256 from a prior read. This does not execute code.", map[string]any{"path": path, "content": stringProp("Complete file contents"), "expected_sha256": hash}, "path", "content"),
		tool("filesystem_edit", "Replace exactly one matching text fragment in an existing file. Read first and supply its sha256. Use a longer old_text if it occurs more than once.", map[string]any{"path": path, "old_text": stringProp("Exact text to replace, occurring once"), "new_text": stringProp("Replacement text, or empty to remove that fragment"), "expected_sha256": hash}, "path", "old_text", "new_text", "expected_sha256"),
	}
}

func fileToolNumber(args map[string]any, key string, fallback, minValue, maxValue int) (int, error) {
	value, exists := args[key]
	if !exists {
		return fallback, nil
	}
	n, ok := value.(float64)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || n < float64(minValue) || n > float64(maxValue) {
		return 0, fmt.Errorf("%s must be an integer from %d to %d", key, minValue, maxValue)
	}
	return int(n), nil
}

func localFilePath(home, input string) (string, error) {
	if strings.TrimSpace(input) == "" || strings.ContainsRune(input, 0) {
		return "", errors.New("provide a local file or directory path")
	}
	if input == "~" {
		input = "."
	} else if strings.HasPrefix(input, "~/") || runtime.GOOS == "windows" && strings.HasPrefix(input, `~\`) {
		input = input[2:]
	}
	if filepath.IsAbs(input) {
		var err error
		input, err = filepath.Rel(home, input)
		if err != nil {
			return "", errors.New("the path must be inside the user's home directory")
		}
	}
	input = filepath.Clean(input)
	if !filepath.IsLocal(input) {
		return "", errors.New("the path must be inside the user's home directory")
	}
	return input, nil
}

func localFileHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readLocalText(root *os.Root, path string) ([]byte, error) {
	info, err := root.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("only regular text files can be read or edited")
	}
	if info.Size() > maxLocalFileBytes {
		return nil, errors.New("file exceeds the 1 MiB text-file limit")
	}
	f, err := root.OpenFile(path, localReadFlags(), 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("the path is no longer a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxLocalFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxLocalFileBytes {
		return nil, errors.New("file exceeds the 1 MiB text-file limit")
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return nil, errors.New("this file is not UTF-8 text; binary documents are not supported")
	}
	return data, nil
}

// Stage the whole file before publishing it. A new file is linked exclusively;
// replacing one requires the hash of the content that the model actually read.
func saveLocalText(ctx context.Context, root *os.Root, path string, data []byte, expected string) error {
	if len(data) > maxLocalFileBytes || !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return errors.New("content must be UTF-8 text of at most 1 MiB, without NUL bytes")
	}
	if expected != "" {
		decoded, err := hex.DecodeString(expected)
		if err != nil || len(decoded) != sha256.Size {
			return errors.New("expected_sha256 must be the sha256 returned by filesystem_read")
		}
		expected = strings.ToLower(expected)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if expected == "" {
		if err := root.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
	}
	parent, err := root.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(path)
	mode := os.FileMode(0644)
	info, err := parent.Lstat(name)
	if err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("writes must target a regular file, not a directory or symbolic link")
		}
		if expected == "" {
			return errors.New("file already exists; read it first and supply expected_sha256 to replace it")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if expected != "" {
		return errors.New("the file no longer exists; reread the directory before changing it")
	}
	temp := ".liteagent-" + newID() + ".tmp"
	f, err := parent.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer parent.Remove(temp)
	// Opening a file applies the process umask. Restore an existing file's mode.
	if expected != "" {
		if err = f.Chmod(mode); err != nil {
			f.Close()
			return err
		}
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
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if expected == "" {
		if err := parent.Link(temp, name); err != nil {
			return fmt.Errorf("could not create file without replacing an existing one: %w", err)
		}
		return nil
	}
	current, err := readLocalText(parent, name)
	if err != nil {
		return err
	}
	if localFileHash(current) != expected {
		return errors.New("file changed since it was read; read it again before editing")
	}
	if info, err := parent.Lstat(name); err != nil || !info.Mode().IsRegular() {
		return errors.New("the destination changed; read it again before editing")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return parent.Rename(temp, name)
}

func (a *App) filesystemTool(ctx context.Context, name string, args map[string]any) (string, error) {
	a.filesMu.Lock()
	defer a.filesMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	home := a.filesHome
	var err error
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return "", err
		}
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return "", err
	}
	defer root.Close()
	var result any
	if name == "filesystem_info" {
		desktop := filepath.Join(home, "Desktop")
		if a.filesHome == "" {
			desktop = desktopDirectory(home)
		}
		_, desktopErr := localFilePath(home, desktop)
		result = map[string]any{"os": runtime.GOOS, "home": home, "desktop": desktop, "desktop_in_scope": desktopErr == nil,
			"access_root": home, "relative_paths": "relative to home", "max_file_bytes": maxLocalFileBytes, "max_directory_entries": maxLocalDirectoryEntries,
			"operations": []string{"stat", "list", "read_text", "create_directory", "write_text", "edit_text"}}
	} else {
		input, _ := args["path"].(string)
		path, err := localFilePath(home, input)
		if err != nil {
			return "", err
		}
		fullPath := filepath.Join(home, path)
		switch name {
		case "filesystem_stat":
			var info os.FileInfo
			info, err = root.Lstat(path)
			if err == nil {
				result = map[string]any{"path": fullPath, "type": localFileKind(info.Mode()), "size_bytes": info.Size(), "modified_at": info.ModTime().UTC().Format(time.RFC3339Nano), "permissions": info.Mode().Perm().String()}
			}
		case "filesystem_list":
			result, err = listLocalDirectory(ctx, root, path, fullPath, args)
		case "filesystem_read":
			var offset, limit int
			offset, err = fileToolNumber(args, "offset", 0, 0, maxLocalFileBytes)
			if err != nil {
				return "", err
			}
			limit, err = fileToolNumber(args, "limit", 6000, 1, 10000)
			if err != nil {
				return "", err
			}
			var data []byte
			data, err = readLocalText(root, path)
			if err == nil {
				expected, _ := args["expected_sha256"].(string)
				if expected != "" && localFileHash(data) != strings.ToLower(expected) {
					return "", errors.New("file changed between reads; start reading again at offset 0")
				}
				chars := []rune(string(data))
				if offset > len(chars) {
					return "", errors.New("offset is beyond the end of this file")
				}
				end := min(len(chars), offset+limit)
				// Keep escaped JSON comfortably below the agent's observation limit.
				for end > offset {
					encoded, _ := json.Marshal(string(chars[offset:end]))
					if len(encoded) <= 12000 {
						break
					}
					end = offset + (end-offset)/2
				}
				result = map[string]any{"path": fullPath, "content": string(chars[offset:end]), "sha256": localFileHash(data), "size_bytes": len(data), "total_characters": len(chars), "offset": offset, "next_offset": end, "truncated": end < len(chars)}
			}
		case "filesystem_mkdir":
			err = root.MkdirAll(path, 0755)
			result = map[string]any{"path": fullPath, "directory_exists": err == nil}
		case "filesystem_write", "filesystem_edit":
			content, contentProvided := args["content"].(string)
			if name == "filesystem_write" && !contentProvided {
				return "", errors.New("content must be a string; use an explicit empty string to create an empty file")
			}
			expected, _ := args["expected_sha256"].(string)
			expected = strings.ToLower(expected)
			if name == "filesystem_edit" {
				old, _ := args["old_text"].(string)
				replacement, replacementProvided := args["new_text"].(string)
				if !replacementProvided {
					return "", errors.New("new_text must be a string; use an explicit empty string to remove text")
				}
				if old == "" || expected == "" {
					return "", errors.New("provide nonempty old_text and expected_sha256 from filesystem_read")
				}
				data, readErr := readLocalText(root, path)
				if readErr != nil {
					return "", readErr
				}
				if localFileHash(data) != expected {
					return "", errors.New("file changed since it was read; read it again before editing")
				}
				if strings.Count(string(data), old) != 1 {
					return "", errors.New("old_text must match exactly once; read the file and use a unique fragment")
				}
				content = strings.Replace(string(data), old, replacement, 1)
			}
			err = saveLocalText(ctx, root, path, []byte(content), expected)
			result = map[string]any{"path": fullPath, "saved": err == nil, "size_bytes": len(content), "sha256": localFileHash([]byte(content))}
		default:
			return "", errors.New("unknown filesystem tool")
		}
		if err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := json.Marshal(result)
	return string(data), err
}

func localFileKind(mode os.FileMode) string {
	switch {
	case mode.IsDir():
		return "directory"
	case mode&os.ModeSymlink != 0:
		return "symlink"
	case mode.IsRegular():
		return "file"
	default:
		return "special"
	}
}

func listLocalDirectory(ctx context.Context, root *os.Root, path, fullPath string, args map[string]any) (any, error) {
	offset, err := fileToolNumber(args, "offset", 0, 0, maxLocalDirectoryEntries)
	if err != nil {
		return nil, err
	}
	limit, err := fileToolNumber(args, "limit", 50, 1, 100)
	if err != nil {
		return nil, err
	}
	dir, err := root.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries := []os.DirEntry{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := f.ReadDir(256)
		entries = append(entries, batch...)
		if len(entries) > maxLocalDirectoryEntries {
			return nil, fmt.Errorf("directory exceeds the %d-entry listing limit; choose a more specific subdirectory", maxLocalDirectoryEntries)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})
	// Hash names and entry types in stable order; content hashes belong to reads.
	names := make([][2]string, len(entries))
	for i, entry := range entries {
		names[i] = [2]string{entry.Name(), localFileKind(entry.Type())}
	}
	raw, _ := json.Marshal(names)
	snapshot := localFileHash(raw)
	if expected, _ := args["expected_snapshot"].(string); expected != "" && strings.ToLower(expected) != snapshot {
		return nil, errors.New("directory entries changed; start listing again at offset 0")
	}
	if offset > len(entries) {
		return nil, errors.New("offset is beyond the end of this directory")
	}
	items := []map[string]any{}
	budget := 12000
	for _, entry := range entries[offset:min(len(entries), offset+limit)] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("directory entry changed or is inaccessible; list again: %w", err)
		}
		item := map[string]any{"name": entry.Name(), "type": localFileKind(info.Mode()), "size_bytes": info.Size(), "modified_at": info.ModTime().UTC().Format(time.RFC3339Nano)}
		encoded, _ := json.Marshal(item)
		budget -= len(encoded)
		if budget < 0 {
			break
		}
		items = append(items, item)
	}
	next := offset + len(items)
	return map[string]any{"path": fullPath, "entries": items, "total_entries": len(entries), "snapshot_sha256": snapshot, "next_offset": next, "truncated": next < len(entries)}, nil
}
