// Package transfer moves the bytes of an accepted file_offer (PROTOCOL.md §5). Both
// directions use it: sending to a device and serving a fetch stream with Stream;
// receiving files and fetching files read with Receive.
package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
	"github.com/NikhilChinna9579/lan-share/windows/internal/protocol"
)

// Stream writes each file's bytes followed by its file_end frame.
// progress (may be nil) gets bytes sent so far, the total, and the file index.
func Stream(w io.Writer, paths []string, sizes []int64, progress func(sent, total int64, idx int)) error {
	var total, sent int64
	for _, s := range sizes {
		total += s
	}
	buf := make([]byte, 256*1024)
	for i, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		h := sha256.New()
		for remaining := sizes[i]; remaining > 0; {
			n := int64(len(buf))
			if remaining < n {
				n = remaining
			}
			got, err := io.ReadFull(f, buf[:n])
			if err != nil {
				f.Close()
				return fmt.Errorf("%s changed while sending: %w", filepath.Base(p), err)
			}
			h.Write(buf[:got])
			if _, err := w.Write(buf[:got]); err != nil {
				f.Close()
				return err
			}
			remaining -= int64(got)
			sent += int64(got)
			if progress != nil {
				progress(sent, total, i)
			}
		}
		f.Close()
		if err := protocol.WriteFrame(w, protocol.FileEnd{Type: "file_end", Index: i, SHA256: hex.EncodeToString(h.Sum(nil))}); err != nil {
			return err
		}
	}
	return nil
}

// Saved describes one file Receive finished.
type Saved struct {
	Index int
	Path  string
	Size  int64
	Took  time.Duration
}

// Receive reads the files of an offer, file i into dirs[i] (a whole folder being
// received has its files in several subfolders). Bytes are read from src (which may
// be rw slowed down, for tests); frames from rw. Files are written as name.part and
// renamed only when their checksum matches; on any failure the .part files are
// deleted. names must already be sanitized.
func Receive(rw io.ReadWriter, src io.Reader, dirs []string, names []string, sizes []int64,
	progress func(done, total int64, idx int), saved func(Saved)) ([]string, error) {
	if src == nil {
		src = rw
	}
	var parts []string
	defer func() {
		for _, p := range parts {
			os.Remove(p)
		}
	}()
	var total, done int64
	for _, s := range sizes {
		total += s
	}
	out := make([]string, 0, len(names))
	for i, name := range names {
		start := time.Now()
		dir := dirs[i]
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		part := files.UniquePath(dir, name+".part")
		f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			protocol.WriteFrame(rw, protocol.Error{Type: "error", Code: "io", Message: err.Error()})
			return nil, err
		}
		parts = append(parts, part)
		h := sha256.New()
		pw := &progressWriter{fn: progress, done: done, total: total, idx: i}
		_, err = io.CopyN(io.MultiWriter(f, h, pw), src, sizes[i])
		done += sizes[i]
		if err == nil && progress != nil {
			progress(done, total, i) // always report the end of each file
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("receiving %s: %w", name, err)
		}
		typ, raw, err := protocol.ReadFrame(rw)
		if err != nil {
			return nil, err
		}
		var end protocol.FileEnd
		if typ != "file_end" || json.Unmarshal(raw, &end) != nil || end.Index != i {
			protocol.WriteFrame(rw, protocol.Error{Type: "error", Code: "bad_frame"})
			return nil, errors.New("expected file_end")
		}
		if end.SHA256 != hex.EncodeToString(h.Sum(nil)) {
			protocol.WriteFrame(rw, protocol.Error{Type: "error", Code: "checksum"})
			return nil, fmt.Errorf("%s: checksum mismatch", name)
		}
		final := files.UniquePath(dir, name)
		if err := os.Rename(part, final); err != nil {
			return nil, err
		}
		parts = parts[:len(parts)-1]
		out = append(out, filepath.Base(final))
		if saved != nil {
			saved(Saved{Index: i, Path: final, Size: sizes[i], Took: time.Since(start)})
		}
	}
	return out, nil
}

// progressWriter reports progress at most ~6 times a second.
type progressWriter struct {
	fn    func(done, total int64, idx int)
	done  int64
	total int64
	idx   int
	last  time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.fn != nil && time.Since(p.last) > 150*time.Millisecond {
		p.last = time.Now()
		p.fn(p.done, p.total, p.idx)
	}
	return len(b), nil
}

// Check validates a fetch request against a folder on disk: every name must be a
// plain file name (no path parts), exist in dir as a regular, visible file. It returns
// the full paths and sizes in the order asked.
func Check(dir string, names []string, hidden func(dir, name string) bool) ([]string, []int64, error) {
	if len(names) == 0 {
		return nil, nil, errors.New("no files asked for")
	}
	paths := make([]string, len(names))
	sizes := make([]int64, len(names))
	for i, n := range names {
		clean, err := files.SanitizeName(n)
		if err != nil || clean != n || (hidden != nil && hidden(dir, n)) {
			return nil, nil, fmt.Errorf("bad name %q", n)
		}
		p := filepath.Join(dir, n)
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("%s isn't there", n)
		}
		paths[i], sizes[i] = p, st.Size()
	}
	return paths, sizes, nil
}
