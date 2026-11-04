//go:build !windows

package main

// installSendTo is Windows-only; on other systems (development, CI) it does nothing.
func installSendTo(string) {}
