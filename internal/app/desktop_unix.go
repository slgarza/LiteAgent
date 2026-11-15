//go:build !windows

package app

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func localReadFlags() int { return os.O_RDONLY | syscall.O_NONBLOCK }

func desktopDirectory(home string) string {
	if runtime.GOOS == "linux" {
		config, err := os.UserConfigDir()
		if err == nil {
			f, err := os.Open(filepath.Join(config, "user-dirs.dirs"))
			if err == nil {
				defer f.Close()
				data, err := io.ReadAll(io.LimitReader(f, 64<<10))
				if err == nil {
					if path := xdgDesktop(home, string(data)); path != "" {
						return path
					}
				}
			}
		}
	}
	return filepath.Join(home, "Desktop")
}

// Parse the XDG assignment as data, never as shell code.
func xdgDesktop(home, data string) string {
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(key) != "XDG_DESKTOP_DIR" {
			continue
		}
		path, err := strconv.Unquote(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		path = strings.ReplaceAll(strings.ReplaceAll(path, "${HOME}", home), "$HOME", home)
		if filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\r\n") {
			return filepath.Clean(path)
		}
	}
	return ""
}
