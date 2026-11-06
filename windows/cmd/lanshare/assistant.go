package main

// The laptop's assistant, typed in the window. It understands the same sentences as the phone's assistant ("send this to my
// phone", "get the latest photo from my phone", "paste from my phone"), says what it's
// about to do, waits 2 seconds for a Cancel, then does it. Missing details become a
// question with buttons ("Which phone?").

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/clipboard"
	"github.com/NikhilChinna9579/lan-share/windows/internal/commands"
	"github.com/NikhilChinna9579/lan-share/windows/internal/presence"
	"github.com/NikhilChinna9579/lan-share/windows/internal/protocol"
)

// assistState is what the window shows on the assistant card.
type assistState struct {
	Mode    string    `json:"mode"` // listening, thinking, asking, ready, working, done, error
	Heard   string    `json:"heard,omitempty"`
	Title   string    `json:"title"`
	Detail  string    `json:"detail,omitempty"`
	Options []string  `json:"options,omitempty"`
	Until   time.Time `json:"until,omitempty"` // "ready": does it at this time unless cancelled
	At      time.Time `json:"at"`

	gen    int      // bumped by every new step: older timers/answers are ignored
	choose []func() // what each option does
}

const assistWait = 2 * time.Second

func (a *app) assistSet(f func(s *assistState)) int {
	a.mu.Lock()
	if a.assist == nil {
		a.assist = &assistState{}
	}
	a.assist.gen++
	a.assist.Options, a.assist.choose, a.assist.Until = nil, nil, time.Time{}
	f(a.assist)
	a.assist.At = time.Now()
	gen := a.assist.gen
	logf("assistant: %s: %s %s", a.assist.Mode, a.assist.Title, a.assist.Detail)
	a.mu.Unlock()
	a.hub.changed()
	return gen
}

func (a *app) assistGen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.assist == nil {
		return 0
	}
	return a.assist.gen
}

// assistHeard handles a typed sentence.
func (a *app) assistHeard(text string) {
	names := a.phoneNames()
	c := commands.Parse(text, names)
	logf("assistant: heard %q → %+v", text, c)
	a.assistSet(func(s *assistState) {
		s.Mode, s.Heard, s.Title, s.Detail = "thinking", text, "One moment…", ""
	})
	if c.Kind == commands.Unknown {
		a.assistSet(func(s *assistState) {
			s.Mode, s.Title = "error", "I didn't understand that"
			s.Detail = "Try “send this to my phone”, “get the latest photo from my phone” or “paste from my phone”."
		})
		return
	}
	a.withPhone(c, func(id, name string) { a.assistPlan(c, id, name) })
}

// withPhone works out which phone is meant, asking if it's unclear.
func (a *app) withPhone(c commands.Command, then func(id, name string)) {
	phones := a.trust.List()
	if len(phones) == 0 {
		a.assistSet(func(s *assistState) {
			s.Mode, s.Title, s.Detail = "error", "No phone paired yet", "Pair your phone first."
		})
		return
	}
	for _, p := range phones {
		if c.Device != "" && p.Name == c.Device || c.Device == "" && len(phones) == 1 {
			then(p.ID, p.Name)
			return
		}
	}
	a.assistSet(func(s *assistState) {
		s.Mode, s.Title = "asking", "Which phone?"
		for _, p := range phones {
			p := p
			s.Options = append(s.Options, p.Name)
			s.choose = append(s.choose, func() { then(p.ID, p.Name) })
		}
	})
}

func (a *app) phoneNames() []string {
	var out []string
	for _, p := range a.trust.List() {
		out = append(out, p.Name)
	}
	return out
}

// assistPlan turns the command into an action, says it, and does it after 2 seconds.
func (a *app) assistPlan(c commands.Command, id, phone string) {
	if !a.isOnline(id) && c.Kind != commands.Status {
		a.assistSet(func(s *assistState) {
			s.Mode, s.Title, s.Detail = "error", phone+" is offline", "Open LAN Share on the phone and check it's on the same Wi-Fi."
		})
		return
	}
	switch c.Kind {
	case commands.Status:
		a.assistSet(func(s *assistState) {
			s.Mode = "done"
			if a.isOnline(id) {
				s.Title = phone + " is online"
			} else {
				s.Title, s.Detail = phone+" is offline", "Open LAN Share on the phone and check it's on the same Wi-Fi."
			}
		})
	case commands.Paste:
		a.ready("Pasting what's copied on "+phone, "", func() error {
			n, err := a.pasteFromPhone(id)
			if err == nil {
				a.assistSet(func(s *assistState) {
					s.Mode, s.Title, s.Detail = "done", fmt.Sprintf("Copied %d characters from %s", n, phone), "Paste with Ctrl+V."
				})
			}
			return err
		})
	case commands.Send:
		if c.What == commands.Clipboard {
			a.ready("Sending what's copied here to "+phone, "", func() error {
				text, private, err := clipboard.Read()
				switch {
				case err != nil:
					return err
				case private:
					return errors.New("what's copied is marked private (a password?), so it isn't sent")
				case text == "":
					return errors.New("nothing is copied on this laptop")
				}
				c, _, err := a.dial(id)
				if err != nil {
					return err
				}
				defer c.Close()
				if err := c.PushClip(text); err != nil {
					return err
				}
				a.assistSet(func(s *assistState) { s.Mode, s.Title = "done", "It's on "+phone+"'s clipboard" })
				return nil
			})
			return
		}
		files, err := a.filesFor(c)
		if err != nil {
			a.assistSet(func(s *assistState) { s.Mode, s.Title, s.Detail = "error", "Couldn't find the file", err.Error() })
			return
		}
		key, label := a.phoneFolder(id, c.Folder)
		what := files[0].Name
		if len(files) > 1 {
			what = fileCount(len(files))
		}
		a.ready(fmt.Sprintf("Sending %s to %s", what, phone), "Into "+label, func() error {
			a.send(id, key, label, "", files)
			a.assistSet(func(s *assistState) { s.Mode, s.Title, s.Detail = "done", "Sending "+what+" to "+phone, "Into "+label })
			return nil
		})
	case commands.Get:
		hit, err := a.phoneFile(id, c)
		if err != nil {
			a.assistSet(func(s *assistState) { s.Mode, s.Title, s.Detail = "error", "Couldn't find it on "+phone, err.Error() })
			return
		}
		to := a.folderLabel(a.folders.Default)
		a.ready(fmt.Sprintf("Getting %s from %s", hit.Name, phone), fmt.Sprintf("%s · into %s", sizeStr(hit.Size), to), func() error {
			a.fetch(id, hit.Folder, hit.Folder, hit.Path, []string{hit.Name})
			a.assistSet(func(s *assistState) {
				s.Mode, s.Title, s.Detail = "done", "Getting "+hit.Name+" from "+phone, "Into "+to
			})
			return nil
		})
	}
}

// ready shows what's about to happen and does it after 2 seconds unless cancelled.
func (a *app) ready(title, detail string, do func() error) {
	gen := a.assistSet(func(s *assistState) {
		s.Mode, s.Title, s.Detail, s.Until = "ready", title, detail, time.Now().Add(assistWait)
	})
	time.AfterFunc(assistWait, func() {
		if a.assistGen() != gen {
			return
		}
		a.assistSet(func(s *assistState) { s.Mode = "working"; s.Title = title; s.Detail = detail })
		if err := do(); err != nil {
			a.assistSet(func(s *assistState) { s.Mode, s.Title, s.Detail = "error", "Couldn't do that", err.Error() })
		}
	})
}

func (a *app) assistCancel() {
	a.assistSet(func(s *assistState) { s.Mode, s.Title, s.Detail = "", "", "" })
}

func (a *app) assistPick(i int) error {
	a.mu.Lock()
	var f func()
	if a.assist != nil && i >= 0 && i < len(a.assist.choose) {
		f = a.assist.choose[i]
	}
	a.mu.Unlock()
	if f == nil {
		return errors.New("that question has gone")
	}
	go f()
	return nil
}

func (a *app) isOnline(id string) bool {
	for _, s := range a.tracker.Snapshot() {
		if s.ID == id {
			return s.Status == presence.Online
		}
	}
	return false
}

// filesFor finds the laptop file(s) a Send means.
func (a *app) filesFor(c commands.Command) ([]fileInfo, error) {
	switch c.What {
	case commands.This:
		return nil, errors.New("type the file's name (e.g. “send report.pdf to my phone”), or use Send files")
	case commands.Named:
		var best string
		score := 0
		for _, p := range a.searchByName(c.Name) {
			if s := commands.NameScore(filepath.Base(p), c.Name); s > score {
				best, score = p, s
			}
		}
		if best == "" {
			// Search wants every word in the name; try the longest word alone.
			words := strings.Fields(strings.TrimSuffix(c.Name, filepath.Ext(c.Name)))
			sort.Slice(words, func(i, j int) bool { return len(words[i]) > len(words[j]) })
			if len(words) > 0 {
				for _, p := range a.searchByName(words[0]) {
					if s := commands.NameScore(filepath.Base(p), c.Name); s > score {
						best, score = p, s
					}
				}
			}
		}
		if best == "" {
			return nil, fmt.Errorf("nothing called “%s” in the shared folders", c.Name)
		}
		return statFiles([]string{best}), nil
	}
	p, err := a.newestLocal(c.What)
	if err != nil {
		return nil, err
	}
	return statFiles([]string{p}), nil
}

var (
	imageExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".heic": true, ".webp": true, ".gif": true, ".bmp": true}
	videoExt = map[string]bool{".mp4": true, ".mov": true, ".mkv": true, ".avi": true, ".webm": true, ".3gp": true, ".m4v": true}
)

// newestLocal: the laptop's latest download, screenshot, photo or video.
func (a *app) newestLocal(w commands.What) (string, error) {
	type place struct{ key, sub string }
	var places []place
	var ok func(string) bool
	what := w.String()
	switch w {
	case commands.Download:
		places, ok = []place{{"downloads", ""}}, func(string) bool { return true }
	case commands.Screenshot:
		places, ok = []place{{"pictures", "Screenshots"}, {"desktop", ""}}, func(n string) bool {
			return imageExt[strings.ToLower(filepath.Ext(n))] && (strings.Contains(strings.ToLower(n), "screenshot") || true)
		}
	case commands.Photo:
		places, ok = []place{{"pictures", ""}, {"downloads", ""}}, func(n string) bool { return imageExt[strings.ToLower(filepath.Ext(n))] }
	case commands.Video:
		places, ok = []place{{"videos", ""}, {"downloads", ""}}, func(n string) bool { return videoExt[strings.ToLower(filepath.Ext(n))] }
	}
	var best string
	var bestT time.Time
	for _, pl := range places {
		root, found := a.folders.Path(pl.key)
		if !found {
			continue
		}
		dir := filepath.Join(root, pl.sub)
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !ok(e.Name()) || strings.HasSuffix(e.Name(), ".crdownload") || strings.HasSuffix(e.Name(), ".part") {
				continue
			}
			if info, err := e.Info(); err == nil && info.ModTime().After(bestT) {
				best, bestT = filepath.Join(dir, e.Name()), info.ModTime()
			}
		}
		if best != "" {
			break // the first place that has one (Screenshots before Desktop)
		}
	}
	if best == "" {
		return "", fmt.Errorf("no %s found on this laptop", what)
	}
	return best, nil
}

// phoneFolder picks the phone's folder for a send: the one said, else its default.
func (a *app) phoneFolder(id, said string) (key, label string) {
	key, label = "downloads", "Downloads"
	c, _, err := a.dial(id)
	if err != nil {
		return
	}
	defer c.Close()
	r, err := c.Folders()
	if err != nil {
		return
	}
	var labels []string
	for _, f := range r.Folders {
		labels = append(labels, f.Label)
		if f.Key == r.Default {
			key, label = f.Key, f.Label
		}
	}
	if l := commands.FindFolder(said, labels); l != "" {
		for _, f := range r.Folders {
			if f.Label == l {
				return f.Key, f.Label
			}
		}
	}
	return
}

// phoneFile finds the phone file a Get means: by name, or its latest photo/screenshot/….
func (a *app) phoneFile(id string, c commands.Command) (protocol.SearchHit, error) {
	conn, _, err := a.dial(id)
	if err != nil {
		return protocol.SearchHit{}, err
	}
	defer conn.Close()
	if c.What == commands.Named {
		hits, err := conn.Search(c.Name)
		if err != nil {
			return protocol.SearchHit{}, err
		}
		var best protocol.SearchHit
		score := 0
		for _, h := range hits {
			if s := commands.NameScore(h.Name, c.Name); s > score {
				best, score = h, s
			}
		}
		if score == 0 {
			return best, fmt.Errorf("nothing called “%s” in its shared folders", c.Name)
		}
		return best, nil
	}
	type place struct{ key, path string }
	var places []place
	ok := func(n string) bool { return true }
	switch c.What {
	case commands.Photo:
		places, ok = []place{{"camera", ""}, {"pictures", ""}}, func(n string) bool { return imageExt[strings.ToLower(filepath.Ext(n))] }
	case commands.Screenshot:
		places, ok = []place{{"pictures", "Screenshots"}, {"camera", ""}}, func(n string) bool {
			return imageExt[strings.ToLower(filepath.Ext(n))] && strings.Contains(strings.ToLower(n), "screenshot")
		}
	case commands.Video:
		places, ok = []place{{"camera", ""}, {"movies", ""}}, func(n string) bool { return videoExt[strings.ToLower(filepath.Ext(n))] }
	default:
		places = []place{{"downloads", ""}}
	}
	for _, pl := range places {
		r, err := conn.List(pl.key, pl.path)
		if err != nil {
			continue
		}
		var best *protocol.ListFile
		for i, f := range r.Files {
			if ok(f.Name) && (best == nil || f.Modified > best.Modified) {
				best = &r.Files[i]
			}
		}
		if best != nil {
			return protocol.SearchHit{Folder: pl.key, Path: pl.path, Name: best.Name, Size: best.Size, Modified: best.Modified}, nil
		}
	}
	return protocol.SearchHit{}, fmt.Errorf("no %s found in its shared folders", c.What)
}

// pasteFromPhone puts the phone's clipboard on this laptop's.
func (a *app) pasteFromPhone(id string) (int, error) {
	c, d, err := a.dial(id)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	text, err := c.RequestClip()
	if err != nil {
		return 0, err
	}
	if err := clipboard.Write(text); err != nil {
		return 0, err
	}
	p, n := preview(text)
	a.mu.Lock()
	a.clipIn = &clipNote{Device: d.Name, Text: p, Chars: n, At: time.Now()}
	a.clipFull = text
	a.mu.Unlock()
	a.hub.changed()
	return n, nil
}

// searchByName finds a file by name in the shared folders.
func (a *app) searchByName(name string) []string {
	var out []string
	for _, h := range a.folders.Search(name, 20) {
		root, ok := a.folders.Path(h.Folder)
		if !ok {
			continue
		}
		out = append(out, filepath.Join(root, filepath.FromSlash(h.Path), h.Name))
	}
	return out
}
