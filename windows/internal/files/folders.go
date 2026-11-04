package files

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Root is one shared folder: the only places other devices may save into or browse.
// Nothing outside these roots is ever reachable, so system folders can't be touched.
type Root struct {
	Key     string `json:"key"`   // what goes on the wire, e.g. "downloads" or "f-3a9c12be"
	Label   string `json:"label"` // shown to people, e.g. "Downloads" or "Projects"
	Path    string `json:"path"`
	Builtin bool   `json:"builtin,omitempty"`
}

// Entry is a file inside a shared folder (for browsing and, later, fetching).
type Entry struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Folders is the set of shared folders. Safe for concurrent use.
type Folders struct {
	mu      sync.Mutex
	file    string // where it's saved; "" = memory only (tests)
	Default string
	roots   []Root
}

// ErrBadPath means the requested folder or subfolder isn't inside a shared folder.
var ErrBadPath = errors.New("files: not inside a shared folder")

// NewFolders returns an empty, unsaved set whose default is defaultKey.
func NewFolders(defaultKey string) *Folders { return &Folders{Default: defaultKey} }

// Add registers a folder under a fixed key (used for the standard folders and tests).
func (f *Folders) Add(key, label, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.roots {
		if r.Key == key {
			f.roots[i] = Root{Key: key, Label: label, Path: path, Builtin: true}
			return
		}
	}
	f.roots = append(f.roots, Root{Key: key, Label: label, Path: path, Builtin: true})
}

// LoadShared reads dataDir/shared.json. On first run it shares the standard user
// folders that exist (Downloads, Documents, Desktop, Pictures, Videos, Music).
func LoadShared(dataDir string) (*Folders, error) {
	f := &Folders{file: filepath.Join(dataDir, "shared.json"), Default: "downloads"}
	data, err := os.ReadFile(f.file)
	if err == nil {
		var saved struct {
			Default string `json:"default"`
			Folders []Root `json:"folders"`
		}
		if err := json.Unmarshal(data, &saved); err != nil {
			return nil, fmt.Errorf("shared.json is damaged: %w", err)
		}
		f.roots = saved.Folders
		if saved.Default != "" {
			f.Default = saved.Default
		}
		return f, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, k := range standardFolders() {
		if st, err := os.Stat(k.Path); err == nil && st.IsDir() {
			k.Builtin = true
			f.roots = append(f.roots, k)
		}
	}
	return f, f.save()
}

// Roots returns the shared folders in display order.
func (f *Folders) Roots() []Root {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Root(nil), f.roots...)
}

// List returns keys and labels in display order.
func (f *Folders) List() (keys, labels []string) {
	for _, r := range f.Roots() {
		keys = append(keys, r.Key)
		labels = append(labels, r.Label)
	}
	return
}

// Path resolves a key ("" = the default folder); ok is false for unknown keys.
func (f *Folders) Path(key string) (string, bool) {
	if key == "" {
		key = f.Default
	}
	for _, r := range f.Roots() {
		if r.Key == key {
			return r.Path, true
		}
	}
	return "", false
}

// AddFolder shares another folder chosen by the user.
func (f *Folders) AddFolder(path string) (Root, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Root{}, err
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return Root{}, fmt.Errorf("%s isn't a folder", path)
	}
	sum := sha256.Sum256([]byte(strings.ToLower(abs)))
	r := Root{Key: "f-" + hex.EncodeToString(sum[:4]), Label: filepath.Base(abs), Path: abs}
	if r.Label == "" || r.Label == "." || r.Label == string(filepath.Separator) {
		r.Label = abs // a whole drive, e.g. D:\
	}
	f.mu.Lock()
	for _, x := range f.roots {
		if x.Key == r.Key {
			f.mu.Unlock()
			return x, nil // already shared
		}
	}
	f.roots = append(f.roots, r)
	f.mu.Unlock()
	return r, f.save()
}

// Remove stops sharing a folder. The folder itself is not touched.
func (f *Folders) Remove(key string) error {
	f.mu.Lock()
	out := f.roots[:0]
	for _, r := range f.roots {
		if r.Key != key {
			out = append(out, r)
		}
	}
	f.roots = out
	if f.Default == key && len(out) > 0 {
		f.Default = out[0].Key
	}
	f.mu.Unlock()
	return f.save()
}

// Resolve turns (folder key, relative subfolder like "Work/2026") into a real path,
// refusing anything that would land outside the shared folder (.., links, junctions).
func (f *Folders) Resolve(key, rel string) (string, error) {
	root, ok := f.Path(key)
	if !ok {
		return "", ErrBadPath
	}
	target := root
	for _, part := range strings.Split(strings.ReplaceAll(rel, `\`, "/"), "/") {
		switch {
		case part == "" || part == ".":
			continue
		case part == ".." || strings.ContainsAny(part, `:*?"<>|`):
			return "", ErrBadPath
		}
		target = filepath.Join(target, part)
	}
	if target == root {
		os.MkdirAll(root, 0o755) // a standard folder that was deleted: bring it back
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", ErrBadPath
	}
	if r, err := filepath.Rel(realRoot, realTarget); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", ErrBadPath
	}
	if st, err := os.Stat(realTarget); err != nil || !st.IsDir() {
		return "", ErrBadPath
	}
	return target, nil
}

// ListDir lists the subfolders and files of a folder inside a shared folder,
// skipping hidden and system items.
func (f *Folders) ListDir(key, rel string) (dirs []string, list []Entry, err error) {
	dir, err := f.Resolve(key, rel)
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if isHidden(dir, e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		} else if info.Mode().IsRegular() {
			list = append(list, Entry{Name: e.Name(), Size: info.Size(), Modified: info.ModTime()})
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i]) < strings.ToLower(dirs[j]) })
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	return dirs, list, nil
}

func (f *Folders) save() error {
	if f.file == "" {
		return nil
	}
	f.mu.Lock()
	data, err := json.MarshalIndent(struct {
		Default string `json:"default"`
		Folders []Root `json:"folders"`
	}{f.Default, f.roots}, "", "  ")
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.file), 0o700); err != nil {
		return err
	}
	tmp := f.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.file)
}
