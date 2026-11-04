//go:build !windows

package main

// Installing is Windows-only; on Linux (development, CI) LAN Share runs from anywhere.

func isInstalled() bool            { return false }
func installSupported() bool       { return false }
func installedExe() string         { return "" }
func install() error               { return errUnsupported }
func afterInstall()                {}
func uninstall(string, bool) error { return errUnsupported }
