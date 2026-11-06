package files

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// standardFolders asks Windows where the user's folders really are. This matters when
// OneDrive (or an admin) has moved Documents, Desktop or Pictures somewhere else.
func standardFolders() []Root {
	type kf struct {
		key, label, fallback string
		id                   syscall.GUID
	}
	list := []kf{
		{"downloads", "Downloads", "Downloads", guid(0x374DE290, 0x123F, 0x4565, 0x91, 0x64, 0x39, 0xC4, 0x92, 0x5E, 0x46, 0x7B)},
		{"documents", "Documents", "Documents", guid(0xFDD39AD0, 0x238F, 0x46AF, 0xAD, 0xB4, 0x6C, 0x85, 0x48, 0x03, 0x69, 0xC7)},
		{"desktop", "Desktop", "Desktop", guid(0xB4BFCC3A, 0xDB2C, 0x424C, 0xB0, 0x29, 0x7F, 0xE9, 0x9A, 0x87, 0xC6, 0x41)},
		{"pictures", "Pictures", "Pictures", guid(0x33E28130, 0x4E1E, 0x4676, 0x83, 0x5A, 0x98, 0x39, 0x5C, 0x3B, 0xC3, 0xBB)},
		{"videos", "Videos", "Videos", guid(0x18989B1D, 0x99B5, 0x455B, 0x84, 0x1C, 0xAB, 0x7C, 0x74, 0xE4, 0xDD, 0xFC)},
		{"music", "Music", "Music", guid(0x4BD8D571, 0x6D19, 0x48D3, 0xBE, 0x97, 0x42, 0x22, 0x20, 0x08, 0x0E, 0x43)},
	}
	home, _ := os.UserHomeDir()
	var out []Root
	for _, k := range list {
		p := knownFolder(&k.id)
		if p == "" {
			p = filepath.Join(home, k.fallback)
		}
		out = append(out, Root{Key: k.key, Label: k.label, Path: p})
	}
	return out
}

func guid(a uint32, b, c uint16, d0, d1, d2, d3, d4, d5, d6, d7 byte) syscall.GUID {
	return syscall.GUID{Data1: a, Data2: b, Data3: c, Data4: [8]byte{d0, d1, d2, d3, d4, d5, d6, d7}}
}

var (
	shell32              = syscall.NewLazyDLL("shell32.dll")
	ole32                = syscall.NewLazyDLL("ole32.dll")
	procKnownFolderPath  = shell32.NewProc("SHGetKnownFolderPath")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
	procGetFileAttribute = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFileAttributesW")
)

func knownFolder(id *syscall.GUID) string {
	var p *uint16
	if r, _, _ := procKnownFolderPath.Call(uintptr(unsafe.Pointer(id)), 0, 0, uintptr(unsafe.Pointer(&p))); r != 0 || p == nil {
		return ""
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	return syscall.UTF16ToString((*[1 << 15]uint16)(unsafe.Pointer(p))[:])
}

// isHidden skips dot-files and anything Windows marks hidden or system
// ($RECYCLE.BIN, desktop.ini, AppData, …).
func isHidden(dir, name string) bool {
	if len(name) > 0 && name[0] == '.' {
		return true
	}
	p, err := syscall.UTF16PtrFromString(filepath.Join(dir, name))
	if err != nil {
		return true
	}
	const hidden, system, invalid = 0x2, 0x4, 0xFFFFFFFF
	attr, _, _ := procGetFileAttribute.Call(uintptr(unsafe.Pointer(p)))
	return attr == invalid || attr&(hidden|system) != 0
}
