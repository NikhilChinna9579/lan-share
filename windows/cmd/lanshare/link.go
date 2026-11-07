package main

import (
	"errors"
	"net/url"
	"os"
	"strings"
)

// linkIn finds the web link in copied or shared text: the first http(s) address, as
// in "https://youtu.be/…" or "Look at this: https://amzn.in/d/…". "" when there's none.
// Only http and https: a phone can never make this laptop open a file or an app.
func linkIn(text string) string {
	if len(text) > 4096 {
		return ""
	}
	for _, f := range strings.Fields(text) {
		f = strings.TrimLeft(f, "(<[\"'")
		f = strings.TrimRight(f, ".,;:!?>]\"'")
		// A ")" ends the link only when it doesn't close a "(" inside it (Wikipedia links).
		for strings.HasSuffix(f, ")") && strings.Count(f, "(") < strings.Count(f, ")") {
			f = strings.TrimSuffix(f, ")")
		}
		if safeLink(f) {
			return f
		}
	}
	return ""
}

// safeLink: an http or https address with a host, and nothing odd in it.
func safeLink(s string) bool {
	low := strings.ToLower(s)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || r == '"' {
			return false
		}
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != ""
}

// openLink opens a web address in the default browser (checked again here).
func openLink(link string) error {
	if !safeLink(link) {
		return errors.New("that isn't a web link")
	}
	if os.Getenv("LANSHARE_OPEN_LOG") != "" {
		return openFile(link) // tests: the log gets it instead
	}
	return openURL(link)
}
