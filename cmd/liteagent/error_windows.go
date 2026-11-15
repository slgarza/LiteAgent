//go:build windows && desktop

package main

import (
	"strings"
	"syscall"
	"unsafe"
)

func reportError(err error) {
	message, _ := syscall.UTF16PtrFromString(strings.ReplaceAll(err.Error(), "\x00", ""))
	title, _ := syscall.UTF16PtrFromString("LiteAgent could not start")
	syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(message)), uintptr(unsafe.Pointer(title)), 0x10)
}
