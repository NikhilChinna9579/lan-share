//go:build !windows

package main

import (
	"errors"
	"os"
)

// Voice uses Windows' speech recogniser. Elsewhere (dev, CI) LANSHARE_FAKE_HEARD stands
// in for what was said, so the rest of the assistant can be tested.
func listen([]string) (string, error) {
	if s := os.Getenv("LANSHARE_FAKE_HEARD"); s != "" {
		return s, nil
	}
	return "", errors.New("Voice works on Windows.")
}
