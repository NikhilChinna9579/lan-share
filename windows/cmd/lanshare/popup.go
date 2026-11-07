package main

// popupMsg is what the "Copied on …" box shows. Its buttons: Open (when the text has
// a web link), Copy (when it isn't on the clipboard yet) and Dismiss.
type popupMsg struct {
	Title, Body string
	Copy, Link  bool
}
