package files

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Hit is a file found by Search.
type Hit struct {
	Folder string // shared folder key
	Path   string // subfolder inside it, "/"-separated
	Entry
}

// Search finds files whose names contain every word of query (any case), in all
// shared folders and their subfolders, newest first. It stops after limit hits or
// two seconds, so a huge folder can't stall the other device.
func (f *Folders) Search(query string, limit int) []Hit {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil
	}
	deadline := time.Now().Add(2 * time.Second)
	var hits []Hit
	for _, r := range f.Roots() {
		root := r.Path
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || len(hits) >= limit || time.Now().After(deadline) {
				return filepath.SkipDir
			}
			if p != root && isHidden(filepath.Dir(p), d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if rel, _ := filepath.Rel(root, p); strings.Count(rel, string(filepath.Separator)) >= 8 {
					return filepath.SkipDir // deep enough
				}
				return nil
			}
			name := strings.ToLower(d.Name())
			for _, w := range words {
				if !strings.Contains(name, w) {
					return nil
				}
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(root, filepath.Dir(p))
			if rel == "." {
				rel = ""
			}
			hits = append(hits, Hit{Folder: r.Key, Path: filepath.ToSlash(rel), Entry: Entry{Name: d.Name(), Size: info.Size(), Modified: info.ModTime()}})
			return nil
		})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Modified.After(hits[j].Modified) })
	return hits
}
