//go:build !windows

package main

// The "Copied on …" box is a Win32 window; elsewhere a notification stands in.
const clipPopupSupported = false

func showClipPopup(a *app, title, preview string) {
	notifyClick(title, preview+" · click to copy", func() { a.copyOffered() })
}
