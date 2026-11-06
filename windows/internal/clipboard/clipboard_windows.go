//go:build windows

package clipboard

import (
	"errors"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	user32                   = syscall.NewLazyDLL("user32.dll")
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	pOpenClipboard           = user32.NewProc("OpenClipboard")
	pCloseClipboard          = user32.NewProc("CloseClipboard")
	pEmptyClipboard          = user32.NewProc("EmptyClipboard")
	pGetClipboardData        = user32.NewProc("GetClipboardData")
	pSetClipboardData        = user32.NewProc("SetClipboardData")
	pIsFormatAvailable       = user32.NewProc("IsClipboardFormatAvailable")
	pRegisterClipboardFormat = user32.NewProc("RegisterClipboardFormatW")
	pGetClipboardSequenceNum = user32.NewProc("GetClipboardSequenceNumber")
	pGlobalAlloc             = kernel32.NewProc("GlobalAlloc")
	pGlobalFree              = kernel32.NewProc("GlobalFree")
	pGlobalLock              = kernel32.NewProc("GlobalLock")
	pGlobalUnlock            = kernel32.NewProc("GlobalUnlock")
	pGlobalSize              = kernel32.NewProc("GlobalSize")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

func sequence() uint32 {
	r, _, _ := pGetClipboardSequenceNum.Call()
	return uint32(r)
}

func format(name string) uintptr {
	p, _ := syscall.UTF16PtrFromString(name)
	r, _, _ := pRegisterClipboardFormat.Call(uintptr(unsafe.Pointer(p)))
	return r
}

// open retries for a moment: another program may be holding the clipboard.
func open() error {
	for i := 0; i < 20; i++ {
		if r, _, _ := pOpenClipboard.Call(0); r != 0 {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("the clipboard is busy")
}

// Read returns the clipboard's text. private is true when the program that copied it
// asked for it to stay out of clipboard monitors and history (password managers do).
func Read() (text string, private bool, err error) {
	if err := open(); err != nil {
		return "", false, err
	}
	defer pCloseClipboard.Call()
	if r, _, _ := pIsFormatAvailable.Call(format("ExcludeClipboardContentFromMonitorProcessing")); r != 0 {
		return "", true, nil
	}
	if f := format("CanIncludeInClipboardHistory"); f != 0 {
		if r, _, _ := pIsFormatAvailable.Call(f); r != 0 {
			if h, _, _ := pGetClipboardData.Call(f); h != 0 {
				if p, _, _ := pGlobalLock.Call(h); p != 0 {
					v := *(*uint32)(ptr(p))
					pGlobalUnlock.Call(h)
					if v == 0 {
						return "", true, nil
					}
				}
			}
		}
	}
	if r, _, _ := pIsFormatAvailable.Call(cfUnicodeText); r == 0 {
		return "", false, nil // not text (an image, files…)
	}
	h, _, _ := pGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", false, errors.New("couldn't read the clipboard")
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		return "", false, errors.New("couldn't read the clipboard")
	}
	defer pGlobalUnlock.Call(h)
	size, _, _ := pGlobalSize.Call(h)
	n := int(size / 2)
	units := unsafe.Slice((*uint16)(ptr(p)), n)
	end := 0
	for end < n && units[end] != 0 {
		end++
	}
	return string(utf16.Decode(units[:end])), false, nil
}

// Write puts text on the clipboard (it won't be sent back out by Watch).
func Write(text string) error {
	remember(text)
	return put(text)
}

// Copy (testing) imitates a person copying text.
func Copy(text string) {
	remember("")
	put(text)
}

func put(text string) error {
	units := utf16.Encode([]rune(text + "\x00"))
	if err := open(); err != nil {
		return err
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	h, _, _ := pGlobalAlloc.Call(gmemMoveable, uintptr(len(units)*2))
	if h == 0 {
		return errors.New("out of memory")
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return errors.New("couldn't write the clipboard")
	}
	copy(unsafe.Slice((*uint16)(ptr(p)), len(units)), units)
	pGlobalUnlock.Call(h)
	if r, _, _ := pSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		pGlobalFree.Call(h)
		return errors.New("couldn't write the clipboard")
	}
	return nil // Windows owns the memory now
}

// ptr turns a memory address returned by Win32 (GlobalLock) into a pointer.
func ptr(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }
