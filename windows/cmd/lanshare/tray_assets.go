package main

import _ "embed"

// trayIconPNG is the tray icon when the .exe has no icon resource built in.
//
//go:embed assets/icon-32.png
var trayIconPNG []byte
