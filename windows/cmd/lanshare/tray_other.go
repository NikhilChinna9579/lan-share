//go:build !windows

package main

// Tray icon and notifications exist only on Windows; elsewhere (dev, CI) they log.

func startTray(*app)            { _ = trayIconPNG }
func stopTray()                 {}
func setTrayTip(string)         {}
func notify(title, text string) { logf("notify: %s — %s", title, text) }
