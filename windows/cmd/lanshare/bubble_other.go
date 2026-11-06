//go:build !windows

package main

// The floating bubble is Windows-only (it's a Win32 window).
const bubbleSupported = false

func bubbleRefresh(*app)            {}
func bubbleListening(bool)          {}
func lastFrontInfo() openWindowInfo { return openWindowInfo{} }
