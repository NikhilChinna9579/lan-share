package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTitleCandidates(t *testing.T) {
	for title, want := range map[string][]string{
		"IMG_2041.jpg - Photos":                      {"IMG_2041.jpg", "Photos"},
		"*Report.docx [Compatibility Mode] - Word":   {"Report.docx", "Word"},
		"Budget 2026 - Excel":                        {"Budget 2026", "Excel"},
		"plan.pdf — Adobe Acrobat Reader (64-bit)":   {"plan.pdf", "Adobe Acrobat Reader (64-bit)"},
		`C:\Users\n\notes.txt - Notepad++`:           {`C:\Users\n\notes.txt`, "Notepad++"},
		"● main.go - lan-share - Visual Studio Code": {"main.go", "lan-share", "Visual Studio Code"},
	} {
		if got := titleCandidates(title); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %q, want %q", title, got, want)
		}
	}
}

func TestResolveOpen(t *testing.T) {
	dir := t.TempDir()
	mk := func(n string) string { p := filepath.Join(dir, n); os.WriteFile(p, []byte("x"), 0o644); return p }
	photo, report, old := mk("IMG_2041.jpg"), mk("Report.docx"), mk("Report.pdf")
	recent := []string{photo, report, old, filepath.Join(dir, "gone.txt")}

	check := func(w openWindowInfo, search func(string) []string, want string) {
		t.Helper()
		got, how, err := resolveOpen(w, search)
		if want == "" {
			if err == nil {
				t.Errorf("%q: want an error, got %v", w.Title, got)
			}
			return
		}
		if err != nil || len(got) != 1 || got[0] != want {
			t.Errorf("%q: got %v (%s, %v), want %s", w.Title, got, how, err, want)
		}
	}
	check(openWindowInfo{Title: "IMG_2041.jpg - Photos", Recent: recent}, nil, photo)
	check(openWindowInfo{Title: "Report - Word", Recent: recent}, nil, report) // newest match wins
	check(openWindowInfo{Title: "Report.pdf - Edge", Recent: recent}, nil, old)
	check(openWindowInfo{Title: filepath.Join(dir, "Report.pdf") + " - Viewer"}, nil, old)
	check(openWindowInfo{Title: "gone.txt - Notepad", Recent: recent}, nil, "")
	check(openWindowInfo{Title: "Holiday.png - Photos"}, func(string) []string { return []string{photo, mk("Holiday.png")} }, filepath.Join(dir, "Holiday.png"))
	check(openWindowInfo{Title: ""}, nil, "")
	got, how, _ := resolveOpen(openWindowInfo{Title: "Downloads", Selection: []string{photo, report}}, nil)
	if len(got) != 2 || how != "selected in File Explorer" {
		t.Errorf("explorer selection: %v %s", got, how)
	}
}
