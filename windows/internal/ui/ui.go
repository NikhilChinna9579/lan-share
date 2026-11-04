// Package ui has small helpers for a tidy, coloured console: styles, a progress bar,
// size formatting and line input. Colours are ANSI escape codes, which the Windows 10+
// console understands once virtual-terminal mode is switched on (see console_windows.go).
package ui

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

// Color is true when the console can show colours.
var Color bool

func style(code, s string) string {
	if !Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func Bold(s string) string   { return style("1", s) }
func Dim(s string) string    { return style("2", s) }
func Green(s string) string  { return style("32", s) }
func Red(s string) string    { return style("31", s) }
func Yellow(s string) string { return style("33", s) }
func Blue(s string) string   { return style("38;5;75", s) }
func Accent(s string) string { return style("1;38;5;75", s) }

// Key renders a keyboard shortcut like [S].
func Key(k string) string { return style("1;30;48;5;75", " "+k+" ") }

// Dot is a status dot: green for online, grey for offline, yellow for checking.
func Dot(status string) string {
	switch status {
	case "online":
		return Green("●")
	case "offline":
		return Dim("○")
	}
	return Yellow("◌")
}

// Rule is a thin horizontal line.
func Rule() string { return Dim(strings.Repeat("─", 58)) }

// Title sets the console window title.
func Title(t string) {
	if Color {
		fmt.Printf("\x1b]0;%s\x07", t)
	}
}

// Size formats a byte count for people.
func Size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Ago formats how long ago t was: "just now", "5 min ago", "3 h ago".
func Ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case t.IsZero():
		return "not seen yet"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// Bar draws a progress line in place: name, bar, percent, speed.
func Bar(label string, done, total int64, started time.Time) {
	const width = 24
	frac := 1.0
	if total > 0 {
		frac = float64(done) / float64(total)
	}
	filled := int(frac * width)
	bar := Blue(strings.Repeat("█", filled)) + Dim(strings.Repeat("░", width-filled))
	secs := time.Since(started).Seconds()
	if secs < 0.001 {
		secs = 0.001
	}
	if len(label) > 22 {
		label = label[:21] + "…"
	}
	fmt.Printf("\r  %-22s %s %3.0f%%  %s/s   ", label, bar, frac*100, Size(int64(float64(done)/secs)))
}

// Lines delivers each line typed on the console. One reader for the whole program,
// so prompts and the main menu never fight over input.
var Lines = func() <-chan string {
	ch := make(chan string)
	go func() {
		s := bufio.NewScanner(os.Stdin)
		for s.Scan() {
			ch <- s.Text()
		}
		close(ch)
	}()
	return ch
}()

// Ask prints a prompt and waits for one line. ok is false if input has closed.
func Ask(prompt string) (string, bool) {
	fmt.Print(prompt)
	line, ok := <-Lines
	return strings.TrimSpace(line), ok
}

// SplitPaths splits text pasted or dragged into the console into file paths,
// keeping "quoted paths with spaces" together.
func SplitPaths(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			if !inQuote {
				flush()
			}
		case r == ' ' && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}
