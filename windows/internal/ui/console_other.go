//go:build !windows

package ui

import "os"

// On Linux and macOS (used for development and CI), colour when writing to a terminal.
func init() {
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 && os.Getenv("TERM") != "dumb" {
		Color = true
	}
}
