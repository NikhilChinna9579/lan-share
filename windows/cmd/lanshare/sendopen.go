package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// "Send what's open" (Ctrl+Alt+S, or the tray menu): works out which file the
// person is looking at and opens the send sheet with it.
//
//   - File Explorer: the selected files.
//   - Any other app (Photos, Word, a PDF viewer…): its window title usually names
//     the file ("IMG_2041.jpg - Photos", "Report - Word"); that name is matched
//     against Windows' recently opened files, then against the shared folders.

// openWindow describes the window in front when asked.
type openWindowInfo struct {
	Title     string
	Selection []string // File Explorer's selected items (full paths)
	Recent    []string // recently opened files, newest first (full paths)
}

// titleCandidates pulls likely file names out of a window title.
func titleCandidates(title string) []string {
	t := strings.TrimSpace(title)
	for _, junk := range []string{"[Compatibility Mode]", "(Read-Only)", "[Read-Only]", "(Protected View)", "- Saved", "• Edited"} {
		t = strings.ReplaceAll(t, junk, "")
	}
	t = strings.TrimLeft(t, "*● ")
	var out []string
	for _, sep := range []string{" - ", " — ", " – ", " | "} {
		t = strings.ReplaceAll(t, sep, "\x00")
	}
	for _, p := range strings.Split(t, "\x00") {
		p = strings.Trim(strings.TrimSpace(p), "\"'*")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sameName(a, b string) bool { return strings.EqualFold(a, b) }

// resolveOpen picks the file(s) the window is showing; how says how it knew.
func resolveOpen(w openWindowInfo, search func(name string) []string) (paths []string, how string, err error) {
	if len(w.Selection) > 0 {
		return w.Selection, "selected in File Explorer", nil
	}
	cands := titleCandidates(w.Title)
	if len(cands) == 0 {
		return nil, "", errors.New("nothing is open in the window in front")
	}
	exists := func(p string) bool { st, err := os.Stat(p); return err == nil && !st.IsDir() }
	// A full path in the title (Notepad++, VS Code, some viewers).
	for _, c := range cands {
		if filepath.IsAbs(c) && exists(c) {
			return []string{c}, "named in the window title", nil
		}
	}
	// The exact name ("IMG_2041.jpg"), then the name without its extension ("Report").
	for _, exact := range []bool{true, false} {
		for _, c := range cands {
			for _, r := range w.Recent {
				base := filepath.Base(r)
				ok := sameName(base, c)
				if !exact {
					ok = sameName(strings.TrimSuffix(base, filepath.Ext(base)), c)
				}
				if ok && exists(r) {
					return []string{r}, "recently opened", nil
				}
			}
		}
	}
	if search != nil {
		for _, c := range cands {
			if filepath.Ext(c) == "" || len(c) < 3 {
				continue
			}
			for _, p := range search(c) {
				if sameName(filepath.Base(p), c) && exists(p) {
					return []string{p}, "found in your folders", nil
				}
			}
		}
	}
	return nil, "", errors.New("couldn't tell which file “" + cands[0] + "” is")
}

// sendWhatsOpen resolves the file in front and opens the send sheet with it.
func (a *app) sendWhatsOpen(w openWindowInfo) error {
	paths, how, err := resolveOpen(w, a.searchByName)
	if err != nil {
		logf("send what's open: %q: %v", w.Title, err)
		a.note("error", "Couldn't tell which file is open", err.Error()+". Use Send files, or right-click it → Send to → LAN Share.", "")
		a.showWindow()
		return err
	}
	logf("send what's open: %s (%s)", strings.Join(paths, ", "), how)
	a.handoff(paths)
	return nil
}

// searchByName finds a file by its exact name in the shared folders.
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
