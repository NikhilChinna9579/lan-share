package files

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":          "report.pdf",
		"../../etc/passwd":    "passwd",
		`..\..\Windows\x.dll`: "x.dll",
		"a<b>c:d.txt":         "abcd.txt",
		"CON.txt":             "_CON.txt",
		"trailing dots...":    "trailing dots",
		"photo\x00\x1f.jpg":   "photo.jpg",
		"नमस्ते.txt":          "नमस्ते.txt",
	}
	for in, want := range cases {
		got, err := SanitizeName(in)
		if err != nil || got != want {
			t.Errorf("SanitizeName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "..", "/", "...", "  "} {
		if _, err := SanitizeName(bad); err == nil {
			t.Errorf("SanitizeName(%q) should fail", bad)
		}
	}
}

func TestUniquePath(t *testing.T) {
	dir := t.TempDir()
	if got := UniquePath(dir, "a.txt"); got != filepath.Join(dir, "a.txt") {
		t.Fatal(got)
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "a (2).txt"), nil, 0o600)
	if got := UniquePath(dir, "a.txt"); got != filepath.Join(dir, "a (3).txt") {
		t.Fatal(got)
	}
}
