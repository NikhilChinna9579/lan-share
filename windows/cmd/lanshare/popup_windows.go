package main

// "Copied on Pixel": a small box at the bottom-right of the screen (not inside LAN
// Share's window) with a preview and Copy / Dismiss, plus Open when it's a web link.
// It never takes the focus from the app you're in, and closes itself after 30 seconds.
// Plain Win32, like the tray.

import (
	"sync"
	"syscall"
	"unsafe"
)

const clipPopupSupported = true

const (
	wsPopup          = 0x80000000
	wsExTopmost      = 0x00000008
	wsExToolWindow   = 0x00000080
	wsExNoActivate   = 0x08000000
	wmMouseActivate  = 0x0021
	maNoActivate     = 3
	swpNoSize        = 0x0001
	swpNoActivate    = 0x0010
	swpNoZOrder      = 0x0004
	swShowNoActivate = 4
	swHide           = 0
)

const (
	wmClipPopup   = wmApp + 12 // tray thread: show the box with the latest offered text
	wmCtlColorSt  = 0x0138     // WM_CTLCOLORSTATIC
	wmTimer       = 0x0113
	wmSetFont     = 0x0030
	wmClose       = 0x0010
	wsChild       = 0x40000000
	wsVisible     = 0x10000000
	wsBorder      = 0x00800000
	bsPushButton  = 0x0000
	bsDefPush     = 0x0001
	ssEndEllipsis = 0x4000
	ssNoPrefix    = 0x0080
	popupCopy     = 1
	popupDismiss  = 2
	popupOpen     = 3
	popupTimer    = 1
)

var (
	gdi32 = syscall.NewLazyDLL("gdi32.dll")

	pSetWindowPos          = user32.NewProc("SetWindowPos")
	pShowWindow            = user32.NewProc("ShowWindow")
	pSystemParametersInfo  = user32.NewProc("SystemParametersInfoW")
	pGetDpiForSystem       = user32.NewProc("GetDpiForSystem")
	pSetThreadDpiAwareness = user32.NewProc("SetThreadDpiAwarenessContext")
	pSetWindowText         = user32.NewProc("SetWindowTextW")
	pSetTimer              = user32.NewProc("SetTimer")
	pKillTimer             = user32.NewProc("KillTimer")
	pSendMessage           = user32.NewProc("SendMessageW")
	pCreateFont            = gdi32.NewProc("CreateFontW")
	pGetSysColorBr         = user32.NewProc("GetSysColorBrush")
	pSetBkMode             = gdi32.NewProc("SetBkMode")
)

type clipPopupWin struct {
	a                   *app
	hwnd, title, body   uintptr
	open, copy, dismiss uintptr
	w, h                int32
	dpi                 int32
	fontBold, fontPlain uintptr
}

var (
	clipPopup     *clipPopupWin
	clipPopupMu   sync.Mutex
	clipPopupNext popupMsg // read on the tray thread
)

// showClipPopup asks the tray thread to show the box (from any goroutine).
func showClipPopup(a *app, m popupMsg) {
	t := theTray
	if t == nil || t.hwnd == 0 {
		if m.Copy {
			notifyClick(m.Title, m.Body+" · click to copy", func() { a.copyOffered() })
		} else {
			notifyClick(m.Title, m.Body+" · click to open", func() { a.openClipLink() })
		}
		return
	}
	clipPopupMu.Lock()
	clipPopupNext = m
	clipPopupMu.Unlock()
	pPostMessage.Call(t.hwnd, wmClipPopup, 0, 0)
}

// On the tray thread.
func (t *tray) clipPopupShow() {
	if clipPopup == nil {
		clipPopup = &clipPopupWin{a: t.a}
		if !clipPopup.create() {
			clipPopup = nil
			return
		}
	}
	clipPopupMu.Lock()
	next := clipPopupNext
	clipPopupMu.Unlock()
	p := clipPopup
	setText(p.title, next.Title)
	setText(p.body, next.Body)
	// Buttons from the right: Dismiss, then Copy, then Open (only those that apply).
	x := int32(244)
	place := func(h uintptr, show bool) {
		if !show {
			pShowWindow.Call(h, swHide)
			return
		}
		pSetWindowPos.Call(h, 0, uintptr(p.px(x)), uintptr(p.px(74)), 0, 0, swpNoSize|swpNoActivate|swpNoZOrder)
		pShowWindow.Call(h, swShowNoActivate)
		x -= 88
	}
	place(p.dismiss, true)
	place(p.copy, next.Copy)
	place(p.open, next.Link)
	var work [4]int32
	pSystemParametersInfo.Call(0x0030, 0, uintptr(unsafe.Pointer(&work)), 0) // SPI_GETWORKAREA
	x, y := work[2]-p.w-16, work[3]-p.h-16
	pSetWindowPos.Call(p.hwnd, ^uintptr(0) /* HWND_TOPMOST */, uintptr(x), uintptr(y), 0, 0, swpNoSize|swpNoActivate)
	pShowWindow.Call(p.hwnd, swShowNoActivate)
	pSetTimer.Call(p.hwnd, popupTimer, 30_000, 0)
	logf("clip popup: shown (copy %v, open %v)", next.Copy, next.Link)
}

func setText(h uintptr, s string) {
	p, _ := syscall.UTF16PtrFromString(s)
	pSetWindowText.Call(h, uintptr(unsafe.Pointer(p)))
}

func (p *clipPopupWin) px(v int32) int32 { return v * p.dpi / 96 }

func (p *clipPopupWin) create() bool {
	p.dpi = 96
	if pGetDpiForSystem.Find() == nil {
		if d, _, _ := pGetDpiForSystem.Call(); d > 0 {
			p.dpi = int32(d)
		}
	}
	px := p.px
	p.w, p.h = px(340), px(112)
	inst, _, _ := pGetModuleHandle.Call(0)
	cls, _ := syscall.UTF16PtrFromString("LanShareClipPopup")
	wc := wndClassEx{LpfnWndProc: syscall.NewCallback(p.wndProc), HInstance: inst, LpszClassName: cls}
	wc.HbrBackground, _, _ = pGetSysColorBr.Call(5) // COLOR_WINDOW
	wc.HCursor, _, _ = user32.NewProc("LoadCursorW").Call(0, 32512)
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	name, _ := syscall.UTF16PtrFromString("LAN Share: copied")
	p.hwnd, _, _ = pCreateWindowEx.Call(wsExTopmost|wsExToolWindow|wsExNoActivate,
		uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(name)), wsPopup|wsBorder,
		0, 0, uintptr(p.w), uintptr(p.h), 0, 0, inst, 0)
	if p.hwnd == 0 {
		logf("clip popup: couldn't create the window")
		return false
	}
	face, _ := syscall.UTF16PtrFromString("Segoe UI")
	height := func(v int32) uintptr { return uintptr(int64(-px(v))) } // negative: character height in pixels
	p.fontBold, _, _ = pCreateFont.Call(height(15), 0, 0, 0, 600, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(face)))
	p.fontPlain, _, _ = pCreateFont.Call(height(13), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(face)))
	child := func(class, text string, style uintptr, x, y, w, h int32, id uintptr, font uintptr) uintptr {
		c, _ := syscall.UTF16PtrFromString(class)
		t, _ := syscall.UTF16PtrFromString(text)
		hw, _, _ := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(c)), uintptr(unsafe.Pointer(t)), wsChild|wsVisible|style,
			uintptr(px(x)), uintptr(px(y)), uintptr(px(w)), uintptr(px(h)), p.hwnd, id, inst, 0)
		pSendMessage.Call(hw, wmSetFont, font, 1)
		return hw
	}
	p.title = child("STATIC", "", ssEndEllipsis|ssNoPrefix, 14, 10, 312, 22, 0, p.fontBold)
	p.body = child("STATIC", "", ssEndEllipsis|ssNoPrefix, 14, 34, 312, 36, 0, p.fontPlain)
	p.open = child("BUTTON", "Open", bsPushButton, 68, 74, 82, 28, popupOpen, p.fontPlain)
	p.copy = child("BUTTON", "Copy", bsDefPush, 156, 74, 82, 28, popupCopy, p.fontPlain)
	p.dismiss = child("BUTTON", "Dismiss", bsPushButton, 244, 74, 82, 28, popupDismiss, p.fontPlain)
	return true
}

func (p *clipPopupWin) hide() {
	pKillTimer.Call(p.hwnd, popupTimer)
	pShowWindow.Call(p.hwnd, swHide)
}

func (p *clipPopupWin) wndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmMouseActivate:
		return maNoActivate // clicking its buttons never takes the focus from your app
	case wmCtlColorSt: // white behind the text
		pSetBkMode.Call(wParam, 1) // TRANSPARENT
		br, _, _ := pGetSysColorBr.Call(5)
		return br
	case wmCommand:
		switch wParam & 0xFFFF {
		case popupCopy:
			p.hide()
			logf("clip popup: Copy")
			go func() {
				if err := p.a.copyOffered(); err != nil {
					notify("Couldn't copy", err.Error())
				}
			}()
		case popupOpen:
			p.hide()
			logf("clip popup: Open")
			go func() {
				if err := p.a.openClipLink(); err != nil {
					notify("Couldn't open the link", err.Error())
				}
			}()
		case popupDismiss:
			p.hide()
			logf("clip popup: Dismiss")
		}
		return 0
	case wmTimer:
		p.hide()
		return 0
	case wmClose:
		p.hide()
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}
