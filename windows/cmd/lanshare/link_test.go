package main

import "testing"

func TestLinkIn(t *testing.T) {
	cases := map[string]string{
		"https://youtu.be/abc?si=x1":                          "https://youtu.be/abc?si=x1",
		"Look at this: https://amzn.in/d/5Xy. Nice?":          "https://amzn.in/d/5Xy",
		"Great video\nhttps://www.youtube.com/watch?v=q&t=9s": "https://www.youtube.com/watch?v=q&t=9s",
		"(see https://example.com/a)":                         "https://example.com/a",
		"https://en.wikipedia.org/wiki/Go_(game)":             "https://en.wikipedia.org/wiki/Go_(game)",
		"HTTP://EXAMPLE.COM":                                  "HTTP://EXAMPLE.COM",
		"first http://a.example then https://b.example":       "http://a.example",
		"Meeting at 5pm":                                      "",
		"file:///C:/Windows/system32/cmd.exe":                 "",
		"javascript:alert(1)":                                 "",
		"https://":                                            "",
		"ms-settings:privacy https:/nohost":                   "",
		"example.com without a scheme":                        "",
	}
	for in, want := range cases {
		if got := linkIn(in); got != want {
			t.Errorf("linkIn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenLinkRefusesOtherSchemes(t *testing.T) {
	for _, s := range []string{"file:///etc/passwd", "C:\\Windows\\notepad.exe", "ms-settings:", "https://"} {
		if openLink(s) == nil {
			t.Errorf("openLink(%q) should refuse", s)
		}
	}
}
