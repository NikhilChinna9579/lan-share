package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
)

// Tap to open: a phone's file is fetched into a private cache and opened in the
// laptop's normal app. Opening it again while unchanged needs no download; the
// cache keeps files for a day (and at most 1 GB). "Save a copy" puts it in the
// default folder without fetching it again.

const (
	cacheMaxAge   = 24 * time.Hour
	cacheMaxBytes = 1 << 30
)

func (a *app) cacheRoot() string { return filepath.Join(a.dataDir, "cache") }

// cachePath is where a phone's file is kept here.
func (a *app) cachePath(deviceID, folder, sub, name string) (string, error) {
	clean, err := files.SanitizeName(name)
	if err != nil {
		return "", err
	}
	dev := deviceID
	if len(dev) > 12 {
		dev = dev[:12]
	}
	fk, err := files.SanitizeName(folder)
	if err != nil {
		fk = "f"
	}
	dir, err := files.SubDir(a.cacheRoot(), dev+"/"+fk+"/"+sub)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, clean), nil
}

func metaPath(p string) string { return filepath.Join(filepath.Dir(p), "."+filepath.Base(p)+".meta") }

// cacheFresh: already here, and the same size and date as on the phone?
func cacheFresh(p string, size, modified int64) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() || st.Size() != size {
		return false
	}
	b, err := os.ReadFile(metaPath(p))
	if err != nil {
		return false
	}
	m, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return err == nil && m == modified
}

// openRemote opens a phone's file, fetching it into the cache first if needed.
func (a *app) openRemote(deviceID, folder, sub, name string, size, modified int64) (map[string]any, error) {
	d, ok := a.trust.Get(deviceID)
	if !ok {
		return nil, errors.New("unknown phone")
	}
	dest, err := a.cachePath(deviceID, folder, sub, name)
	if err != nil {
		return nil, err
	}
	tidb := make([]byte, 8)
	rand.Read(tidb)
	tid := hex.EncodeToString(tidb)
	a.dropOpened(dest) // one card per opened file, the latest

	if cacheFresh(dest, size, modified) {
		now := time.Now()
		os.Chtimes(dest, now, now) // recently used: kept longer
		logf("open: %s from the cache", name)
		a.updateTransfer(tid, "in", d.Name, func(t *transfer) {
			t.Count, t.Files, t.Index, t.Name, t.Total, t.Sent = 1, 1, 1, filepath.Base(dest), size, size
			t.State, t.Folder, t.Opened = "done", "Opened (already here)", dest
		})
		return map[string]any{"id": tid, "cached": true}, openFile(dest)
	}

	dir := filepath.Dir(dest)
	os.Remove(dest) // FetchFiles never overwrites; an older copy would make "name (2)"
	os.Remove(metaPath(dest))
	a.updateTransfer(tid, "in", d.Name, func(t *transfer) {
		t.Count, t.Name, t.Index, t.Total, t.Folder = 1, name, 1, size, "Opening"
	})
	go func() {
		saved, err := func() ([]string, error) {
			c, _, err := a.dial(deviceID)
			if err != nil {
				return nil, err
			}
			defer c.Close()
			a.updateTransfer(tid, "in", d.Name, func(t *transfer) { t.cancel = func() { c.Abort() } })
			if a.wasCancelled(tid) {
				return nil, errors.New("cancelled")
			}
			return c.FetchFiles(tid, folder, sub, []string{name}, dir, func(got, total int64, _ int) {
				a.updateTransfer(tid, "in", d.Name, func(t *transfer) { t.Sent, t.Total = got, total })
			})
		}()
		switch {
		case a.wasCancelled(tid):
			a.updateTransfer(tid, "in", d.Name, func(t *transfer) { t.State, t.Message = "cancelled", "Cancelled." })
		case err != nil:
			logf("open: %s from %s failed: %v", name, d.Name, err)
			a.updateTransfer(tid, "in", d.Name, func(t *transfer) { t.State, t.Message = "error", "Couldn't open "+name+": "+err.Error() })
		default:
			got := dest
			if len(saved) > 0 {
				got = filepath.Join(dir, saved[0])
			}
			os.WriteFile(metaPath(got), []byte(strconv.FormatInt(modified, 10)), 0o600)
			logf("open: fetched %s from %s", name, d.Name)
			a.updateTransfer(tid, "in", d.Name, func(t *transfer) {
				t.State, t.Sent, t.Files, t.Folder, t.Opened = "done", t.Total, 1, "Opened", got
			})
			if err := openFile(got); err != nil {
				a.note("error", "Couldn't open "+name, err.Error(), "")
			}
		}
		a.cleanCache()
	}()
	return map[string]any{"id": tid}, nil
}

// reopen opens an opened file again, from the cache.
func (a *app) reopen(id string) error {
	p := a.openedPath(id)
	if p == "" {
		return errors.New("that file isn't kept any more; open it from the phone again")
	}
	return openFile(p)
}

// saveCopy puts an opened file into the default folder (no second download).
func (a *app) saveCopy(id string) (string, error) {
	src := a.openedPath(id)
	if src == "" {
		return "", errors.New("that file isn't kept any more; get it from the phone again")
	}
	key := a.folders.Default
	dir, ok := a.folders.Path(key)
	if !ok {
		return "", errors.New("no default folder")
	}
	dst := files.UniquePath(dir, filepath.Base(src))
	if err := copyNew(src, dst); err != nil {
		return "", err
	}
	device := ""
	a.mu.Lock()
	for _, t := range a.transfers {
		if t.ID == id {
			device = t.Device
		}
	}
	a.mu.Unlock()
	logf("save copy: %s", dst)
	a.note("received", "Saved "+filepath.Base(dst)+" from "+device, "In "+a.folderLabel(key)+" (already opened, no download)", dst)
	return dst, nil
}

func (a *app) openedPath(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.transfers {
		if t.ID == id && t.Opened != "" {
			if _, err := os.Stat(t.Opened); err == nil {
				return t.Opened
			}
		}
	}
	return ""
}

func copyNew(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// cleanCache deletes opened files older than a day, then the oldest until under 1 GB.
func (a *app) cleanCache() {
	type ent struct {
		p    string
		size int64
		at   time.Time
	}
	var list []ent
	var dirs []string
	filepath.Walk(a.cacheRoot(), func(p string, st os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if st.IsDir() {
			dirs = append(dirs, p)
		} else if !strings.HasPrefix(st.Name(), ".") {
			list = append(list, ent{p, st.Size(), st.ModTime()})
		}
		return nil
	})
	sort.Slice(list, func(i, j int) bool { return list[i].at.Before(list[j].at) })
	var total int64
	for _, e := range list {
		total += e.size
	}
	for _, e := range list {
		if time.Since(e.at) > cacheMaxAge || total > cacheMaxBytes {
			os.Remove(e.p)
			os.Remove(metaPath(e.p))
			total -= e.size
		}
	}
	// Meta files whose file is gone, then empty folders (deepest first).
	filepath.Walk(a.cacheRoot(), func(p string, st os.FileInfo, err error) error {
		if err == nil && !st.IsDir() && strings.HasPrefix(st.Name(), ".") && strings.HasSuffix(st.Name(), ".meta") {
			if _, e := os.Stat(filepath.Join(filepath.Dir(p), strings.TrimSuffix(strings.TrimPrefix(st.Name(), "."), ".meta"))); e != nil {
				os.Remove(p)
			}
		}
		return nil
	})
	for i := len(dirs) - 1; i > 0; i-- {
		os.Remove(dirs[i]) // only succeeds when empty
	}
}

// openFile shows a file in its normal app. For tests, LANSHARE_OPEN_LOG names a
// file that gets the path instead.
func openFile(p string) error {
	if log := os.Getenv("LANSHARE_OPEN_LOG"); log != "" {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintln(f, p)
		return err
	}
	return openPath(p, false)
}

// dropOpened removes earlier "Opened" cards for the same file.
func (a *app) dropOpened(p string) {
	a.mu.Lock()
	keep := a.transfers[:0]
	for _, t := range a.transfers {
		if t.Opened != p || t.State == "running" {
			keep = append(keep, t)
		}
	}
	a.transfers = keep
	a.mu.Unlock()
}
