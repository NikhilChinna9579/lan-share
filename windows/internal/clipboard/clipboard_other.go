//go:build !windows

package clipboard

import "sync"

// An in-memory clipboard (Linux/macOS development and tests).
var (
	memMu   sync.Mutex
	memText string
	memSeq  uint32
)

func sequence() uint32 { memMu.Lock(); defer memMu.Unlock(); return memSeq }

// Read returns the clipboard's text; private is true for copies marked as passwords.
func Read() (text string, private bool, err error) {
	memMu.Lock()
	defer memMu.Unlock()
	return memText, false, nil
}

// Write puts text on the clipboard (it won't be sent back out by Watch).
func Write(text string) error {
	remember(text)
	Copy(text)
	return nil
}

// Copy (testing) imitates a person copying text.
func Copy(text string) {
	memMu.Lock()
	memText = text
	memSeq++
	memMu.Unlock()
}
