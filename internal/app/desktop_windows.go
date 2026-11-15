package app

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

func localReadFlags() int { return os.O_RDONLY }

// Resolve the actual Desktop, including a redirected OneDrive Desktop.
// https://learn.microsoft.com/en-us/windows/win32/api/shlobj_core/nf-shlobj_core-shgetknownfolderpath
func desktopDirectory(home string) string {
	fallback := filepath.Join(home, "Desktop")
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ole := syscall.NewLazyDLL("ole32.dll")
	initialized, _, _ := ole.NewProc("CoInitializeEx").Call(0, 2)
	if initialized == 0 || initialized == 1 {
		defer ole.NewProc("CoUninitialize").Call()
	} else if uint32(initialized) != 0x80010106 { // An existing COM apartment can also resolve the folder.
		return fallback
	}
	desktop := syscall.GUID{Data1: 0xB4BFCC3A, Data2: 0xDB2C, Data3: 0x424C, Data4: [8]byte{0xB0, 0x29, 0x7F, 0xE9, 0x9A, 0x87, 0xC6, 0x41}}
	var path *uint16
	result, _, _ := syscall.NewLazyDLL("shell32.dll").NewProc("SHGetKnownFolderPath").Call(uintptr(unsafe.Pointer(&desktop)), 0, 0, uintptr(unsafe.Pointer(&path)))
	if path != nil {
		defer ole.NewProc("CoTaskMemFree").Call(uintptr(unsafe.Pointer(path)))
	}
	if result != 0 || path == nil {
		return fallback
	}
	for n := 0; n < 32768; n++ {
		if *(*uint16)(unsafe.Add(unsafe.Pointer(path), n*2)) == 0 {
			return syscall.UTF16ToString(unsafe.Slice(path, n))
		}
	}
	return fallback
}
