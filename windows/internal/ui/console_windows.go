package ui

import "syscall"

// Switches the Windows console into virtual-terminal mode so ANSI colours work.
// Windows 10 (1511) and later support it; on older consoles we stay plain.
func init() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getMode := kernel32.NewProc("GetConsoleMode")
	setMode := kernel32.NewProc("SetConsoleMode")
	h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil {
		return
	}
	var mode uint32
	if r, _, _ := getMode.Call(uintptr(h), uintptr(unsafePointer(&mode))); r == 0 {
		return // not a console (output redirected)
	}
	const enableVirtualTerminalProcessing = 0x0004
	if r, _, _ := setMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing)); r != 0 {
		Color = true
	}
}
