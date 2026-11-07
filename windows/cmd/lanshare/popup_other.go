//go:build !windows

package main

// The "Copied on …" box is a Win32 window; elsewhere a notification stands in.
const clipPopupSupported = false

func showClipPopup(a *app, m popupMsg) {
	if m.Copy {
		notifyClick(m.Title, m.Body+" · click to copy", func() { a.copyOffered() })
	} else {
		notifyClick(m.Title, m.Body+" · click to open", func() { a.openClipLink() })
	}
}
