package main

import (
	"encoding/json"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

var (
	pGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	pGetWindowText       = user32.NewProc("GetWindowTextW")
	pGetClassName        = user32.NewProc("GetClassNameW")
	pGetAncestor         = user32.NewProc("GetAncestor")
	pRegisterHotKey      = user32.NewProc("RegisterHotKey")
)

const (
	wmHotkey      = 0x0312
	hotkeySendID  = 1
	modAlt        = 0x1
	modControl    = 0x2
	modNoRepeat   = 0x4000
	gaRoot        = 2
	hotkeyComment = "Ctrl+Alt+S"
)

func windowText(h uintptr) string {
	var b [512]uint16
	pGetWindowText.Call(h, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	return syscall.UTF16ToString(b[:])
}

func windowClass(h uintptr) string {
	var b [256]uint16
	pGetClassName.Call(h, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	return syscall.UTF16ToString(b[:])
}

// lastFront is the last window in front that isn't the taskbar, the desktop or
// LAN Share itself: what the tray menu's "Send what's open" means.
var lastFront uintptr

func ignoredWindow(h uintptr) bool {
	switch windowClass(h) {
	case "Shell_TrayWnd", "Shell_SecondaryTrayWnd", "NotifyIconOverflowWindow", "LanShareTray", "Progman", "WorkerW", "#32768":
		return true
	}
	t := windowText(h)
	return t == "" || t == "LAN Share"
}

func trackFront() {
	for range time.Tick(700 * time.Millisecond) {
		h, _, _ := pGetForegroundWindow.Call()
		if h != 0 {
			if r, _, _ := pGetAncestor.Call(h, gaRoot); r != 0 {
				h = r
			}
			if !ignoredWindow(h) {
				lastFront = h
			}
		}
	}
}

// registerSendHotkey makes Ctrl+Alt+S post WM_HOTKEY to the tray window.
func registerSendHotkey(hwnd uintptr) {
	r, _, err := pRegisterHotKey.Call(hwnd, hotkeySendID, modControl|modAlt|modNoRepeat, 'S')
	if r == 0 {
		logf("hotkey %s: not available (%v)", hotkeyComment, err)
		return
	}
	logf("hotkey %s: send what's open", hotkeyComment)
	go trackFront()
}

// frontWindowInfo describes window h: its title, Explorer's selection, recent files.
func frontWindowInfo(h uintptr) openWindowInfo {
	w := openWindowInfo{Title: windowText(h)}
	explorer := windowClass(h) == "CabinetWClass"
	script := `$o=@{sel=@();recent=@()};`
	if explorer {
		script += `foreach($x in (New-Object -ComObject Shell.Application).Windows()){if($x.HWND -eq ` + strconv.FormatUint(uint64(h), 10) +
			`){$o.sel=@($x.Document.SelectedItems()|%{$_.Path})}};`
	} else {
		script += `$s=New-Object -ComObject WScript.Shell;$o.recent=@(Get-ChildItem "$env:APPDATA\Microsoft\Windows\Recent\*.lnk" -EA 0|` +
			`sort LastWriteTime -Desc|select -First 80|%{$s.CreateShortcut($_.FullName).TargetPath}|?{$_});`
	}
	script += `$o|ConvertTo-Json -Compress`
	out, err := powershell(script)
	if err != nil {
		logf("send what's open: %v", err)
		return w
	}
	var r struct {
		Sel    json.RawMessage `json:"sel"`
		Recent json.RawMessage `json:"recent"`
	}
	json.Unmarshal([]byte(out), &r)
	w.Selection, w.Recent = stringsOf(r.Sel), stringsOf(r.Recent)
	return w
}

// stringsOf reads PowerShell's JSON for a list, which is a bare string for one item.
func stringsOf(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	return nil
}

// sendFront: for the hotkey (the window in front now) or the tray (the last one).
func (a *app) sendFront(h uintptr) {
	if h == 0 || ignoredWindow(h) {
		h = lastFront
	}
	if h == 0 {
		a.note("error", "Nothing open to send", "Open or select a file, then press "+hotkeyComment+".", "")
		a.showWindow()
		return
	}
	a.sendWhatsOpen(frontWindowInfo(h))
}
