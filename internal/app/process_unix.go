//go:build !windows

package app

import "os/exec"

func hideChildConsole(cmd *exec.Cmd) {}
