// Package commands understands what was said (or typed) to the laptop's assistant.
// It's the same rule-based parser as the phone's (Commands.kt), so both understand the
// same sentences; here the "other device" words are phone words.
//
//	"send this to my phone"                     → Send{What: This}
//	"send the latest download to pixel in documents" → Send{What: Download, Folder: "documents"}
//	"get the latest photo from my phone"        → Get{What: Photo}
//	"get report dot pdf from my phone"          → Get{Name: "report.pdf"}
//	"paste from my phone"                       → Paste
//	"is my phone online"                        → Status
package commands

import (
	"regexp"
	"strings"
	"unicode"
)

// What is the thing to send or get.
type What int

const (
	Named What = iota
	This
	Photo
	Screenshot
	Video
	Download
	Clipboard
)

func (w What) String() string {
	return [...]string{"named", "this", "photo", "screenshot", "video", "download", "clipboard"}[w]
}

// Kind of command.
type Kind int

const (
	Unknown Kind = iota
	Send
	Get
	Paste
	Status
)

// Command is what was understood. Device is one of the names given to Parse, or ""
// if none was named (or it's unclear).
type Command struct {
	Kind   Kind
	What   What
	Name   string // the file's name, for What == Named
	Device string
	Folder string
}

var (
	sendVerbs = set("send", "share", "transfer", "move", "put", "upload", "throw", "push", "copy")
	getVerbs  = set("get", "fetch", "download", "bring", "grab", "pull", "open", "paste")
	generic   = set("phone", "mobile", "pixel", "android", "cell", "cellphone", "smartphone", "device", "tablet")
	filler    = set("the", "my", "a", "an", "please", "file", "called", "named", "last", "latest", "recent",
		"most", "newest", "folder", "directory", "to", "in", "into", "on", "s", "of", "me", "for", "it", "just", "now")
	statusRe = regexp.MustCompile(`\b(online|offline|status|connected|reachable|there)\b`)
)

func set(w ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range w {
		m[x] = true
	}
	return m
}

// Parse understands text, given the paired devices' names.
func Parse(text string, devices []string) Command {
	t := Normalize(text)
	if t == "" {
		return Command{}
	}
	words := strings.Split(t, " ")
	start := -1
	for i, w := range words {
		if sendVerbs[w] || getVerbs[w] {
			start = i
			break
		}
	}
	if start < 0 {
		if statusRe.MatchString(t) {
			return Command{Kind: Status, Device: FindDevice(t, devices)}
		}
		return Command{}
	}
	verb := words[start]
	rest := strings.Join(words[start+1:], " ")

	if getVerbs[verb] && (strings.Contains(" "+rest+" ", " from ") || verb != "copy") {
		obj, from, _ := splitAt(rest, "from", "off", "on")
		if verb == "paste" || isClipboard(obj) {
			return Command{Kind: Paste, Device: FindDevice(from, devices)}
		}
		what := whatOf(obj)
		if what == This || what == Clipboard {
			what = Named
		}
		c := Command{Kind: Get, What: what, Device: FindDevice(from, devices)}
		if what == Named {
			if c.Name = CleanName(obj); c.Name == "" {
				return Command{}
			}
		}
		return c
	}

	obj, target, hasTarget := splitAt(rest, "to", "with", "onto", "over to")
	c := Command{Kind: Send, What: whatOf(obj)}
	if c.What == Named {
		if c.Name = CleanName(obj); c.Name == "" {
			return Command{}
		}
	}
	if hasTarget {
		a, b, hasB := splitAt(target, "in", "into", "inside", "under", "on")
		c.Device = FindDevice(a, devices)
		if c.Device == "" && hasB {
			c.Device = FindDevice(b, devices)
		}
		folderPart := stripDevice(a, devices)
		if hasB && FindDevice(a, devices) != "" {
			folderPart = b
		}
		var f []string
		for _, w := range strings.Split(folderPart, " ") {
			if w != "" && !filler[w] {
				f = append(f, w)
			}
		}
		c.Folder = strings.Join(f, " ")
	}
	return c
}

var (
	dotRe   = regexp.MustCompile(`\s+dot\s+`)
	punctRe = regexp.MustCompile(`[^\p{L}\p{N}._\- ]`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// Normalize: lower case, "dot" → ".", apostrophes and most punctuation removed.
func Normalize(text string) string {
	t := strings.ToLower(text)
	t = dotRe.ReplaceAllString(t, ".")
	t = strings.ReplaceAll(t, "'s ", " s ")
	t = strings.ReplaceAll(t, "’s ", " s ")
	t = punctRe.ReplaceAllString(t, " ")
	t = spaceRe.ReplaceAllString(t, " ")
	return strings.TrimRight(strings.TrimSpace(t), ".")
}

func splitAt(s string, seps ...string) (string, string, bool) {
	padded := " " + s + " "
	best, l := -1, 0
	for _, sep := range seps {
		if i := strings.Index(padded, " "+sep+" "); i >= 0 && (best < 0 || i < best) {
			best, l = i, len(sep)+2
		}
	}
	if best < 0 {
		return s, "", false
	}
	return strings.TrimSpace(padded[:best]), strings.TrimSpace(padded[best+l:]), true
}

func isClipboard(obj string) bool {
	for _, w := range strings.Split(obj, " ") {
		if w == "clipboard" || w == "clip" || w == "copied" {
			return true
		}
	}
	return false
}

func whatOf(obj string) What {
	w := strings.Split(obj, " ")
	has := func(f func(string) bool) bool {
		for _, x := range w {
			if f(x) {
				return true
			}
		}
		return false
	}
	switch {
	case isClipboard(obj):
		return Clipboard
	case has(func(x string) bool { return x == "this" || x == "it" || x == "that" || x == "these" || x == "those" }) &&
		!has(func(x string) bool { return strings.Contains(x, ".") }):
		return This
	case has(func(x string) bool { return strings.HasPrefix(x, "screenshot") || x == "screen" }):
		return Screenshot
	case has(func(x string) bool { return set("photo", "photos", "picture", "pic", "pics", "image", "selfie", "snap")[x] }):
		return Photo
	case has(func(x string) bool { return set("video", "videos", "recording", "movie")[x] }):
		return Video
	case has(func(x string) bool { return x == "download" || x == "downloaded" }):
		return Download
	}
	return Named
}

// CleanName: "the file called report.pdf" → "report.pdf"; "" if nothing is left.
func CleanName(obj string) string {
	w := strings.Split(obj, " ")
	i := 0
	for i < len(w) && filler[w[i]] {
		i++
	}
	var out []string
	for _, x := range w[i:] {
		if x != "" {
			out = append(out, x)
		}
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.Join(out, " "), " file"))
}

func squash(s string) string {
	var b strings.Builder
	for _, r := range Normalize(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func nameParts(name string) map[string]bool {
	return set(strings.FieldsFunc(Normalize(name), func(r rune) bool { return r == ' ' || r == '-' || r == '_' })...)
}

// FindDevice: which of devices the phrase means: by name, or "phone"/"pixel" when there's exactly one.
func FindDevice(phrase string, devices []string) string {
	p := squash(phrase)
	if p == "" {
		return ""
	}
	for _, d := range devices {
		if squash(d) == p {
			return d
		}
	}
	one := func(f func(string) bool) string {
		var hits []string
		for _, d := range devices {
			if f(d) {
				hits = append(hits, d)
			}
		}
		if len(hits) == 1 {
			return hits[0]
		}
		return ""
	}
	if d := one(func(d string) bool {
		s := squash(d)
		return s != "" && (strings.Contains(p, s) || strings.Contains(s, p) && len(p) >= 3)
	}); d != "" {
		return d
	}
	var words []string
	for _, w := range strings.Split(phrase, " ") {
		if len(w) >= 3 && !filler[w] {
			words = append(words, w)
		}
	}
	if d := one(func(d string) bool {
		parts := nameParts(d)
		for _, w := range words {
			if parts[w] && !generic[w] { // "phone" alone doesn't pick "Mom's phone" out of two
				return true
			}
		}
		return false
	}); d != "" {
		return d
	}
	for _, w := range words {
		if generic[w] && len(devices) == 1 {
			return devices[0]
		}
	}
	return ""
}

func stripDevice(target string, devices []string) string {
	parts := map[string]bool{}
	if d := FindDevice(target, devices); d != "" {
		parts = nameParts(d)
	}
	var out []string
	for _, w := range strings.Split(target, " ") {
		if !parts[w] && !generic[w] {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

// FindFolder: which of labels a spoken folder name means ("docs" → Documents).
func FindFolder(phrase string, labels []string) string {
	p := squash(phrase)
	if p == "" {
		return ""
	}
	for _, l := range labels {
		if squash(l) == p {
			return l
		}
	}
	for _, l := range labels {
		if strings.TrimSuffix(squash(l), "s") == strings.TrimSuffix(p, "s") {
			return l
		}
	}
	var hits []string
	for _, l := range labels {
		s := squash(l)
		if strings.HasPrefix(s, p) || strings.HasPrefix(p, s) {
			hits = append(hits, l)
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return ""
}

// NameScore: how well a file name matches what was said (0 = not at all).
func NameScore(file, said string) int {
	f, s := Normalize(file), Normalize(said)
	if f == s {
		return 100
	}
	base := func(x string) string {
		if i := strings.LastIndex(x, "."); i > 0 {
			return x[:i]
		}
		return x
	}
	if base(f) == base(s) {
		return 90
	}
	var words []string
	for _, w := range strings.FieldsFunc(base(s), func(r rune) bool { return r == ' ' || r == '-' || r == '_' }) {
		if len(w) >= 2 {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return 0
	}
	hit := 0
	for _, w := range words {
		if strings.Contains(squash(f), squash(w)) {
			hit++
		}
	}
	if hit == len(words) {
		return 50 + hit
	}
	return 0
}
