package main

// The floating bubble: a small round window that stays on top of every app. Click it for
// a menu (send files, paste from a phone, open LAN Share, hide); drag it anywhere; drop
// files on it to send them. Win32 only, like the tray: a layered window drawn from
// the app icon with smooth (anti-aliased) edges, never taking the focus from your app.

import (
	"bytes"
	_ "embed"
	"image"
	"image/png"
	"math"
	"syscall"
	"unsafe"
)

//go:embed assets/icon-256.png
var bubbleIconPNG []byte

const bubbleSupported = true

var (
	gdi32 = syscall.NewLazyDLL("gdi32.dll")

	pUpdateLayeredWindow   = user32.NewProc("UpdateLayeredWindow")
	pGetDC                 = user32.NewProc("GetDC")
	pReleaseDC             = user32.NewProc("ReleaseDC")
	pSetCapture            = user32.NewProc("SetCapture")
	pReleaseCapture        = user32.NewProc("ReleaseCapture")
	pSetWindowPos          = user32.NewProc("SetWindowPos")
	pShowWindow            = user32.NewProc("ShowWindow")
	pSystemParametersInfo  = user32.NewProc("SystemParametersInfoW")
	pGetSystemMetrics      = user32.NewProc("GetSystemMetrics")
	pGetDpiForSystem       = user32.NewProc("GetDpiForSystem")
	pSetThreadDpiAwareness = user32.NewProc("SetThreadDpiAwarenessContext")
	pCreateCompatibleDC    = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIBSection      = gdi32.NewProc("CreateDIBSection")
	pSelectObject          = gdi32.NewProc("SelectObject")
	pDragAcceptFiles       = shell32.NewProc("DragAcceptFiles")
	pDragQueryFile         = shell32.NewProc("DragQueryFileW")
	pDragFinish            = shell32.NewProc("DragFinish")
)

const (
	wsPopup          = 0x80000000
	wsExLayered      = 0x00080000
	wsExTopmost      = 0x00000008
	wsExToolWindow   = 0x00000080
	wsExNoActivate   = 0x08000000
	wsExAcceptFiles  = 0x00000010
	wmMouseActivate  = 0x0021
	wmLButtonDown    = 0x0201
	wmMouseMove      = 0x0200
	wmRButtonDown    = 0x0204
	wmDropFiles      = 0x0233
	maNoActivate     = 3
	swpNoSize        = 0x0001
	swpNoZOrder      = 0x0004
	swpNoActivate    = 0x0010
	swShowNoActivate = 4
	swHide           = 0
	ulwAlpha         = 2
	tpmReturnCmd     = 0x0100
	wmBubble         = wmApp + 10 // tray thread: show/hide the bubble per the setting
	wmBubbleLook     = wmApp + 11 // tray thread: redraw (listening or not)
)

type bubbleWin struct {
	a       *app
	hwnd    uintptr
	size    int32
	idle    uintptr // memory DC holding the bubble's picture
	showing uintptr

	down, dragging bool
	startCur       [2]int32
	startWin       [2]int32
}

var bubble *bubbleWin

// bubbleRefresh shows or hides the bubble to match the setting (from any goroutine).
func bubbleRefresh(a *app) {
	if t := theTray; t != nil && t.hwnd != 0 {
		pPostMessage.Call(t.hwnd, wmBubble, 0, 0)
	}
}

func (t *tray) bubbleUpdate() {
	t.a.mu.Lock()
	on := !t.a.settings.NoBubble
	t.a.mu.Unlock()
	if !on {
		if bubble != nil && bubble.hwnd != 0 {
			pShowWindow.Call(bubble.hwnd, swHide)
			logf("bubble: hidden")
		}
		return
	}
	if bubble == nil {
		bubble = &bubbleWin{a: t.a}
		if !bubble.create() {
			bubble = nil
			return
		}
	}
	pShowWindow.Call(bubble.hwnd, swShowNoActivate)
	bubble.draw(bubble.showing)
	vis, _, _ := user32.NewProc("IsWindowVisible").Call(bubble.hwnd)
	logf("bubble: on screen=%v (window %#x)", vis != 0, bubble.hwnd)
}

func (b *bubbleWin) create() bool {
	dpi := uint32(96)
	if pGetDpiForSystem.Find() == nil {
		if d, _, _ := pGetDpiForSystem.Call(); d > 0 {
			dpi = uint32(d)
		}
	}
	b.size = int32(60 * dpi / 96)
	inst, _, _ := pGetModuleHandle.Call(0)
	cls, _ := syscall.UTF16PtrFromString("LanShareBubble")
	wc := wndClassEx{LpfnWndProc: syscall.NewCallback(b.wndProc), HInstance: inst, LpszClassName: cls}
	wc.HCursor, _, _ = user32.NewProc("LoadCursorW").Call(0, 32649) // IDC_HAND
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	x, y := b.startPos()
	title, _ := syscall.UTF16PtrFromString("LAN Share bubble")
	b.hwnd, _, _ = pCreateWindowEx.Call(wsExLayered|wsExTopmost|wsExToolWindow|wsExNoActivate|wsExAcceptFiles,
		uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)), wsPopup,
		uintptr(x), uintptr(y), uintptr(b.size), uintptr(b.size), 0, 0, inst, 0)
	if b.hwnd == 0 {
		logf("bubble: couldn't create the window")
		return false
	}
	pDragAcceptFiles.Call(b.hwnd, 1)
	img, err := png.Decode(bytes.NewReader(bubbleIconPNG))
	if err != nil {
		return false
	}
	b.idle = b.bitmap(renderBubble(img, int(b.size), false))
	b.showing = b.idle
	logf("bubble: shown at %d,%d (%dpx)", x, y, b.size)
	return true
}

// startPos: where it was left, or bottom-right of the screen.
func (b *bubbleWin) startPos() (int32, int32) {
	var work [4]int32
	pSystemParametersInfo.Call(0x0030, 0, uintptr(unsafe.Pointer(&work)), 0) // SPI_GETWORKAREA
	x, y := work[2]-b.size-24, work[3]-b.size-96
	b.a.mu.Lock()
	if b.a.settings.BubbleX != nil && b.a.settings.BubbleY != nil {
		x, y = int32(*b.a.settings.BubbleX), int32(*b.a.settings.BubbleY)
	}
	b.a.mu.Unlock()
	return b.clamp(x, y)
}

// clamp keeps it on the screens (all monitors together).
func (b *bubbleWin) clamp(x, y int32) (int32, int32) {
	m := func(i uintptr) int32 { v, _, _ := pGetSystemMetrics.Call(i); return int32(v) }
	vx, vy, vw, vh := m(76), m(77), m(78), m(79)
	if vw == 0 {
		return x, y
	}
	x = max(vx, min(x, vx+vw-b.size))
	y = max(vy, min(y, vy+vh-b.size))
	return x, y
}

type bitmapInfoHeader struct {
	Size                          uint32
	Width, Height                 int32
	Planes, BitCount              uint16
	Compression, SizeImage        uint32
	XPels, YPels, ClrUsed, ClrImp uint32
}

// bitmap puts premultiplied BGRA pixels in a memory DC for UpdateLayeredWindow.
func (b *bubbleWin) bitmap(px []byte) uintptr {
	screen, _, _ := pGetDC.Call(0)
	defer pReleaseDC.Call(0, screen)
	dc, _, _ := pCreateCompatibleDC.Call(screen)
	bi := bitmapInfoHeader{Width: b.size, Height: -b.size, Planes: 1, BitCount: 32}
	bi.Size = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer // Windows writes the pixel buffer's address here
	hbm, _, _ := pCreateDIBSection.Call(dc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == nil {
		return 0
	}
	copy(unsafe.Slice((*byte)(bits), len(px)), px)
	pSelectObject.Call(dc, hbm)
	return dc
}

func (b *bubbleWin) draw(dc uintptr) {
	if dc == 0 {
		return
	}
	b.showing = dc
	screen, _, _ := pGetDC.Call(0)
	defer pReleaseDC.Call(0, screen)
	size := [2]int32{b.size, b.size}
	src := [2]int32{0, 0}
	blend := [4]byte{0, 0, 255, 1} // AC_SRC_OVER, 0, opaque, AC_SRC_ALPHA
	pUpdateLayeredWindow.Call(b.hwnd, screen, 0, uintptr(unsafe.Pointer(&size)), dc,
		uintptr(unsafe.Pointer(&src)), 0, uintptr(*(*uint32)(unsafe.Pointer(&blend))), ulwAlpha)
}

func (b *bubbleWin) wndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmMouseActivate:
		return maNoActivate // clicking it never takes the focus from your app
	case wmLButtonDown:
		b.down, b.dragging = true, false
		pGetCursorPos.Call(uintptr(unsafe.Pointer(&b.startCur)))
		var r [4]int32
		user32.NewProc("GetWindowRect").Call(hwnd, uintptr(unsafe.Pointer(&r)))
		b.startWin = [2]int32{r[0], r[1]}
		pSetCapture.Call(hwnd)
		return 0
	case wmMouseMove:
		if !b.down {
			return 0
		}
		var cur [2]int32
		pGetCursorPos.Call(uintptr(unsafe.Pointer(&cur)))
		dx, dy := cur[0]-b.startCur[0], cur[1]-b.startCur[1]
		if !b.dragging && dx*dx+dy*dy > 25 {
			b.dragging = true
		}
		if b.dragging {
			pSetWindowPos.Call(hwnd, 0, uintptr(b.startWin[0]+dx), uintptr(b.startWin[1]+dy), 0, 0, swpNoSize|swpNoZOrder|swpNoActivate)
		}
		return 0
	case wmLButtonUp:
		if !b.down {
			return 0
		}
		b.down = false
		pReleaseCapture.Call()
		if b.dragging {
			var r [4]int32
			user32.NewProc("GetWindowRect").Call(hwnd, uintptr(unsafe.Pointer(&r)))
			x, y := b.clamp(r[0], r[1])
			pSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y), 0, 0, swpNoSize|swpNoZOrder|swpNoActivate)
			xi, yi := int(x), int(y)
			b.a.mu.Lock()
			b.a.settings.BubbleX, b.a.settings.BubbleY = &xi, &yi
			b.a.mu.Unlock()
			go b.a.saveSettings()
			logf("bubble: moved to %d,%d", x, y)
			return 0
		}
		logf("bubble: clicked")
		b.menu(hwnd)
		return 0
	case wmRButtonUp:
		b.menu(hwnd)
		return 0
	case wmDropFiles:
		n, _, _ := pDragQueryFile.Call(wParam, 0xFFFFFFFF, 0, 0)
		var paths []string
		for i := uintptr(0); i < n; i++ {
			var buf [1024]uint16
			pDragQueryFile.Call(wParam, i, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
			paths = append(paths, syscall.UTF16ToString(buf[:]))
		}
		pDragFinish.Call(wParam)
		logf("bubble: %d file(s) dropped", len(paths))
		go b.a.handoff(paths)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}

func (b *bubbleWin) menu(hwnd uintptr) {
	m, _, _ := pCreatePopupMenu.Call()
	add := func(id uintptr, text string) {
		p, _ := syscall.UTF16PtrFromString(text)
		pAppendMenu.Call(m, mfString, id, uintptr(unsafe.Pointer(p)))
	}
	add(2, "Send files…")
	var phones []string
	var ids []string
	for _, s := range b.a.tracker.Snapshot() {
		if s.Status.String() == "online" {
			phones, ids = append(phones, s.Name), append(ids, s.ID)
		}
	}
	for i, p := range phones {
		add(uintptr(10+i), "Paste from "+p)
	}
	pAppendMenu.Call(m, mfSeparator, 0, 0)
	add(4, "Open LAN Share")
	add(5, "Hide bubble")
	var pt [2]int32
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWin.Call(hwnd) // so the menu closes when clicking elsewhere
	cmd, _, _ := pTrackPopupMenu.Call(m, tpmBottom|tpmRightBtn|tpmReturnCmd, uintptr(pt[0]), uintptr(pt[1]), 0, hwnd, 0)
	pDestroyMenu.Call(m)
	a := b.a
	switch {
	case cmd == 2:
		go func() {
			if paths, err := pickFiles(); err == nil && len(paths) > 0 {
				a.handoff(paths)
			}
		}()
	case cmd == 4:
		go a.showWindow()
	case cmd == 5:
		a.mu.Lock()
		a.settings.NoBubble = true
		a.mu.Unlock()
		go a.saveSettings()
		pShowWindow.Call(hwnd, swHide)
		a.hub.changed()
		notify("Bubble hidden", "Show it again from the tray icon or Settings.")
	case cmd >= 10 && int(cmd-10) < len(ids):
		id, name := ids[cmd-10], phones[cmd-10]
		go func() {
			if n, err := a.pasteFromPhone(id); err != nil {
				notify("Couldn't paste from "+name, err.Error())
			} else {
				notify("Copied from "+name, itoa(n)+" characters · paste with Ctrl+V")
			}
		}()
	}
}

func itoa(n int) string { return string(appendInt(nil, n)) }

func appendInt(b []byte, n int) []byte {
	if n >= 10 {
		b = appendInt(b, n/10)
	}
	return append(b, byte('0'+n%10))
}

// renderBubble draws the bubble: a soft shadow and the app icon in a circle with smooth
// edges (listening: a red ring, unused for now). Premultiplied BGRA, top-down.
func renderBubble(icon image.Image, size int, listening bool) []byte {
	px := make([]byte, size*size*4)
	s := float64(size)
	cx, cy := s/2, s/2-s*0.02
	r := s/2 - s*0.09 // room for the shadow and ring
	ib := icon.Bounds()
	// The icon's blue square, mapped onto the circle (its corners fall outside).
	srcSpan := float64(ib.Dx()) * 0.80
	srcOff := float64(ib.Dx()) * 0.10
	scale := srcSpan / (2 * r)
	sample := func(x, y float64) (float64, float64, float64, float64) {
		// Box filter over the source footprint (the icon is much bigger than the bubble).
		var rs, gs, bs, as, n float64
		n0 := int(math.Max(1, math.Round(scale)))
		for j := 0; j < n0; j++ {
			for i := 0; i < n0; i++ {
				sx := int(srcOff + (x-(cx-r))*scale + float64(i)*scale/float64(n0))
				sy := int(srcOff + (y-(cy-r))*scale + float64(j)*scale/float64(n0))
				if sx < 0 || sy < 0 || sx >= ib.Dx() || sy >= ib.Dy() {
					continue
				}
				cr, cg, cb, ca := icon.At(ib.Min.X+sx, ib.Min.Y+sy).RGBA()
				rs, gs, bs, as, n = rs+float64(cr), gs+float64(cg), bs+float64(cb), as+float64(ca), n+1
			}
		}
		if n == 0 {
			return 0, 0, 0, 0
		}
		return rs / n / 65535, gs / n / 65535, bs / n / 65535, as / n / 65535
	}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			d := math.Hypot(fx-cx, fy-cy)
			// Shadow: a soft dark disc slightly lower.
			sd := math.Hypot(fx-cx, fy-cy-s*0.04)
			shadow := 0.35 * math.Max(0, math.Min(1, (r+s*0.07-sd)/(s*0.09)))
			var R, G, B, A float64
			A = shadow // black, premultiplied: colour stays 0
			cover := math.Max(0, math.Min(1, r-d+0.5))
			if cover > 0 {
				ir, ig, ibl, ia := sample(fx, fy)
				if ia < 0.99 { // outside the icon's square: brand blue
					ir, ig, ibl = 0.145*(1-ia)+ir, 0.388*(1-ia)+ig, 0.922*(1-ia)+ibl
				}
				// "over" the shadow, premultiplied.
				R, G, B = ir*cover+R*(1-cover), ig*cover+G*(1-cover), ibl*cover+B*(1-cover)
				A = cover + A*(1-cover)
			}
			if listening { // red ring just outside the circle
				ring := math.Max(0, math.Min(1, 1-math.Abs(d-(r+s*0.035))/(s*0.03)))
				if ring > 0 {
					R, G, B = 0.937*ring+R*(1-ring), 0.267*ring+G*(1-ring), 0.267*ring+B*(1-ring)
					A = ring + A*(1-ring)
				}
			}
			o := (y*size + x) * 4
			px[o], px[o+1], px[o+2], px[o+3] = byte(B*255+0.5), byte(G*255+0.5), byte(R*255+0.5), byte(A*255+0.5)
		}
	}
	return px
}
