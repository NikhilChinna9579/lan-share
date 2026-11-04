// Package clipboard reads, writes and watches this computer's clipboard (text only).
// On Windows it uses the Win32 clipboard; elsewhere (development and tests) a clipboard
// kept in memory stands in for it.
package clipboard

import (
	"sync"
	"time"
)

// Watch calls onCopy with the text each time something new is copied on this computer
// (not when we put text there ourselves with Write). Copies that password managers mark
// as private are skipped. It checks twice a second until stop is closed.
func Watch(stop <-chan struct{}, onCopy func(text string)) {
	last := sequence()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		seq := sequence()
		if seq == last {
			continue
		}
		last = seq
		text, private, err := Read()
		if err != nil || private || text == "" || ours(text) {
			continue
		}
		onCopy(text)
	}
}

// What we wrote ourselves recently: not "copied" by the person, so not sent back out.
var (
	mu      sync.Mutex
	written string
)

func remember(text string) { mu.Lock(); written = text; mu.Unlock() }

func ours(text string) bool { mu.Lock(); defer mu.Unlock(); return text == written }
