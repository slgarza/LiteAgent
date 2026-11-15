//go:build !windows

package app

import (
	"path/filepath"
	"testing"
)

func TestXDGDesktopPaths(t *testing.T) {
	home := t.TempDir()
	for _, config := range []string{
		`XDG_DESKTOP_DIR="$HOME/Escritorio"`,
		"# comment\nXDG_DOWNLOAD_DIR=\"$HOME/Downloads\"\nXDG_DESKTOP_DIR=\"${HOME}/Escritorio\"\n",
	} {
		if got := xdgDesktop(home, config); got != filepath.Join(home, "Escritorio") {
			t.Fatalf("wrong localized Desktop: %q", got)
		}
	}
	for _, config := range []string{`XDG_DESKTOP_DIR=$(touch malicious)`, `XDG_DESKTOP_DIR="relative"`, `# XDG_DESKTOP_DIR="/ignored"`} {
		if got := xdgDesktop(home, config); got != "" {
			t.Fatalf("invalid Desktop assignment accepted: %q", got)
		}
	}
}
