package main

// System tray icon and Windows notifications, using only the Win32 API (no libraries):
// a hidden message window receives clicks on the icon, and Shell_NotifyIcon shows both
// the icon and the "toast" notifications.

import (
	"log"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	user32  = syscall.NewLazyDLL("user32.dll")
	shell32 = syscall.NewLazyDLL("shell32.dll")
	kernel  = syscall.NewLazyDLL("kernel32.dll")

	pRegisterClassEx   = user32.NewProc("RegisterClassExW")
	pCreateWindowEx    = user32.NewProc("CreateWindowExW")
	pDefWindowProc     = user32.NewProc("DefWindowProcW")
	pGetMessage        = user32.NewProc("GetMessageW")
	pTranslateMessage  = user32.NewProc("TranslateMessage")
	pDispatchMessage   = user32.NewProc("DispatchMessageW")
	pPostMessage       = user32.NewProc("PostMessageW")
	pCreatePopupMenu   = user32.NewProc("CreatePopupMenu")
	pAppendMenu        = user32.NewProc("AppendMenuW")
	pTrackPopupMenu    = user32.NewProc("TrackPopupMenu")
	pDestroyMenu       = user32.NewProc("DestroyMenu")
	pGetCursorPos      = user32.NewProc("GetCursorPos")
	pSetForegroundWin  = user32.NewProc("SetForegroundWindow")
	pLoadIcon          = user32.NewProc("LoadIconW")
	pCreateIconFromRes = user32.NewProc("CreateIconFromResourceEx")
	pShellNotifyIcon   = shell32.NewProc("Shell_NotifyIconW")
	pGetModuleHandle   = kernel.NewProc("GetModuleHandleW")
	pRegisterWindowMsg = user32.NewProc("RegisterWindowMessageW")
)

const (
	wmApp       = 0x8000
	wmTray      = wmApp + 1
	wmNotify    = wmApp + 2 // our own: show a pending notification (posted from any goroutine)
	wmTip       = wmApp + 3
	wmCommand   = 0x0111
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205
	wmContext   = 0x007B
	nimAdd      = 0
	nimModify   = 1
	nimDelete   = 2
	nimSetVer   = 4
	nifMessage  = 0x1
	nifIcon     = 0x2
	nifTip      = 0x4
	nifInfo     = 0x10
	niifUser    = 0x4
	niifNoSound = 0x10
	tpmBottom   = 0x20
	tpmRightBtn = 0x2
	mfString    = 0x0
	mfSeparator = 0x800
	menuOpen    = 1
	menuPair    = 2
	menuQuit    = 3
	hwndMessage = ^uintptr(2) // HWND_MESSAGE (-3): a window that is never shown
)

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type msg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      [2]int32
}

type tray struct {
	a       *app
	hwnd    uintptr
	icon    uintptr
	pending chan [2]string // notifications waiting for the tray thread
	tips    chan string
	created uint32 // "TaskbarCreated": Explorer restarted, add the icon again
}

var theTray *tray

// startTray adds the icon; everything Win32 runs on one locked OS thread.
func startTray(a *app) {
	t := &tray{a: a, pending: make(chan [2]string, 16), tips: make(chan string, 4)}
	theTray = t
	ready := make(chan bool)
	go func() {
		runtime.LockOSThread()
		ok := t.create()
		ready <- ok
		if ok {
			t.loop()
		}
	}()
	if <-ready {
		log.Printf("tray: icon added")
	} else {
		log.Printf("tray: not available")
	}
}

func (t *tray) create() bool {
	inst, _, _ := pGetModuleHandle.Call(0)
	cls, _ := syscall.UTF16PtrFromString("LanShareTray")
	wc := wndClassEx{LpfnWndProc: syscall.NewCallback(t.wndProc), HInstance: inst, LpszClassName: cls}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	if r, _, _ := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return false
	}
	t.hwnd, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, inst, 0)
	if t.hwnd == 0 {
		return false
	}
	name, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := pRegisterWindowMsg.Call(uintptr(unsafe.Pointer(name)))
	t.created = uint32(r)
	t.icon = loadAppIcon()
	return t.add()
}

func (t *tray) data() *notifyIconData {
	d := &notifyIconData{HWnd: t.hwnd, UID: 1}
	d.CbSize = uint32(unsafe.Sizeof(*d))
	return d
}

func (t *tray) add() bool {
	d := t.data()
	d.UFlags = nifMessage | nifIcon | nifTip
	d.UCallbackMessage = wmTray
	d.HIcon = t.icon
	copyUTF16(d.SzTip[:], "LAN Share")
	r, _, _ := pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(d)))
	d.UVersion = 4 // NOTIFYICON_VERSION_4
	pShellNotifyIcon.Call(nimSetVer, uintptr(unsafe.Pointer(d)))
	return r != 0
}

func (t *tray) loop() {
	var m msg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (t *tray) wndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch {
	case message == wmTray:
		switch uint32(lParam) & 0xFFFF { // version 4: the event is in the low word
		case wmLButtonUp:
			go t.a.showWindow()
		case wmRButtonUp, wmContext:
			t.menu()
		}
		return 0
	case message == wmNotify:
		for {
			select {
			case n := <-t.pending:
				d := t.data()
				d.UFlags = nifInfo
				copyUTF16(d.SzInfoTitle[:], n[0])
				copyUTF16(d.SzInfo[:], n[1])
				d.DwInfoFlags = niifUser | niifNoSound
				d.HBalloonIcon = t.icon
				pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(d)))
			default:
				return 0
			}
		}
	case message == wmTip:
		select {
		case tip := <-t.tips:
			d := t.data()
			d.UFlags = nifTip
			copyUTF16(d.SzTip[:], tip)
			pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(d)))
		default:
		}
		return 0
	case message == wmCommand:
		switch wParam & 0xFFFF {
		case menuOpen:
			go t.a.showWindow()
		case menuPair:
			go func() { t.a.startPairing(); t.a.showWindow() }()
		case menuQuit:
			go t.a.requestQuit()
		}
		return 0
	case t.created != 0 && message == t.created:
		t.add()
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}

func (t *tray) menu() {
	m, _, _ := pCreatePopupMenu.Call()
	add := func(id uintptr, text string) {
		p, _ := syscall.UTF16PtrFromString(text)
		pAppendMenu.Call(m, mfString, id, uintptr(unsafe.Pointer(p)))
	}
	add(menuOpen, "Open LAN Share")
	add(menuPair, "Pair a phone…")
	pAppendMenu.Call(m, mfSeparator, 0, 0)
	add(menuQuit, "Quit")
	var pt [2]int32
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWin.Call(t.hwnd) // so the menu closes when clicking elsewhere
	pTrackPopupMenu.Call(m, tpmBottom|tpmRightBtn, uintptr(pt[0]), uintptr(pt[1]), 0, t.hwnd, 0)
	pDestroyMenu.Call(m)
}

// notify shows a Windows notification (from any goroutine).
func notify(title, text string) {
	t := theTray
	if t == nil || t.hwnd == 0 {
		return
	}
	select {
	case t.pending <- [2]string{title, text}:
		pPostMessage.Call(t.hwnd, wmNotify, 0, 0)
	default:
	}
}

// setTrayTip updates the text shown when hovering the icon.
func setTrayTip(tip string) {
	t := theTray
	if t == nil || t.hwnd == 0 {
		return
	}
	select {
	case t.tips <- tip:
		pPostMessage.Call(t.hwnd, wmTip, 0, 0)
	default:
	}
}

func stopTray() {
	if t := theTray; t != nil && t.hwnd != 0 {
		pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(t.data())))
	}
}

// loadAppIcon uses the icon built into the .exe if there is one, else a built-in one.
func loadAppIcon() uintptr {
	inst, _, _ := pGetModuleHandle.Call(0)
	for _, id := range []uintptr{1, 2, 101} { // resource ids go-winres and rsrc use
		if h, _, _ := pLoadIcon.Call(inst, id); h != 0 {
			return h
		}
	}
	if len(trayIconPNG) > 0 { // PNG-in-ICO data works with CreateIconFromResourceEx
		h, _, _ := pCreateIconFromRes.Call(uintptr(unsafe.Pointer(&trayIconPNG[0])), uintptr(len(trayIconPNG)), 1, 0x00030000, 32, 32, 0)
		if h != 0 {
			return h
		}
	}
	h, _, _ := pLoadIcon.Call(0, 32512) // IDI_APPLICATION
	return h
}

func copyUTF16(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)-1]
		u = append(u, 0)
	}
	copy(dst, u)
}
