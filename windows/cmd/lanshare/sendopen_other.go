//go:build !windows

package main

// The hotkey and window detection are Windows-only; /api/sendopen still works with a
// described window (tests).
func (a *app) sendFront(uintptr) {}
