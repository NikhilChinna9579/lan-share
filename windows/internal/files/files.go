// Package files handles where received files go and how they are named safely.
package files

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Folders maps the folder keys we offer (PROTOCOL.md "Folders") to real paths.
// Senders only ever send a key, so they can't write anywhere else.
type Folders struct {
	Default string
	paths   map[string]string
	labels  map[string]string
	order   []string
}

// NewFolders returns an empty set whose default is defaultKey.
func NewFolders(defaultKey string) *Folders {
	return &Folders{Default: defaultKey, paths: map[string]string{}, labels: map[string]string{}}
}

// UserFolders returns the standard folders under the user's home directory.
func UserFolders() (*Folders, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	f := NewFolders("downloads")
	for _, e := range []struct{ key, label, dir string }{
		{"downloads", "Downloads", "Downloads"},
		{"documents", "Documents", "Documents"},
		{"pictures", "Pictures", "Pictures"},
		{"desktop", "Desktop", "Desktop"},
	} {
		f.Add(e.key, e.label, filepath.Join(home, e.dir))
	}
	return f, nil
}

// Add registers a folder key.
func (f *Folders) Add(key, label, path string) {
	if _, ok := f.paths[key]; !ok {
		f.order = append(f.order, key)
	}
	f.paths[key] = path
	f.labels[key] = label
}

// Path resolves a key; ok is false for unknown keys.
func (f *Folders) Path(key string) (string, bool) {
	if key == "" {
		key = f.Default
	}
	p, ok := f.paths[key]
	return p, ok
}

// List returns keys and labels in display order.
func (f *Folders) List() (keys, labels []string) {
	for _, k := range f.order {
		keys = append(keys, k)
		labels = append(labels, f.labels[k])
	}
	return
}

var reserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// ErrBadName means nothing usable was left after cleaning a file name.
var ErrBadName = errors.New("files: bad file name")

// SanitizeName strips path parts and characters Windows forbids (PROTOCOL.md §5 rule 1).
func SanitizeName(name string) (string, error) {
	// Keep only the last path element, whichever separator the sender used.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"|?*`, r) {
			continue
		}
		b.WriteRune(r)
	}
	// Windows silently drops trailing dots and spaces; do it ourselves.
	clean := strings.TrimRight(strings.TrimSpace(b.String()), ". ")
	if clean == "" || clean == "." || clean == ".." {
		return "", ErrBadName
	}
	base := strings.ToUpper(strings.SplitN(clean, ".", 2)[0])
	if reserved[base] {
		clean = "_" + clean
	}
	if len(clean) > 200 {
		ext := filepath.Ext(clean)
		if len(ext) > 20 {
			ext = ""
		}
		clean = clean[:200-len(ext)] + ext
	}
	return clean, nil
}

// UniquePath returns dir/name, or dir/"name (2).ext", "(3)"… if taken.
func UniquePath(dir, name string) string {
	p := filepath.Join(dir, name)
	if !exists(p) {
		return p
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		p = filepath.Join(dir, stem+" ("+itoa(i)+")"+ext)
		if !exists(p) {
			return p
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}
