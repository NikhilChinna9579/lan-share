//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Linux/macOS versions, used for development and CI tests. The window is opened in a
// browser (or not at all with -no-window); native dialogs aren't available.

func platformName() string { return "other" }
func hasConsole() bool     { return true }

func openWindow(url, _ string) error {
	if err := exec.Command("xdg-open", url).Start(); err != nil {
		fmt.Println("Open this in a browser:", url)
	}
	return nil
}

var errUnsupported = errors.New("only available on Windows")

func pickFiles() ([]string, error) { return nil, errUnsupported }
func pickFolder() (string, error)  { return "", errUnsupported }
func allowFirewall() error         { return errUnsupported }
func autostartEnabled() bool       { return false }
func setAutostart(bool) error      { return errUnsupported }

func openPath(p string, _ bool) error { return exec.Command("xdg-open", p).Start() }

func showError(msg string) { fmt.Fprintln(os.Stderr, "LAN Share:", msg) }
