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
	return StreamFrom(w, paths, sizes, nil, progress)
}

// StreamFrom is Stream resuming each file at offsets[i] (bytes the receiver already
// has from an interrupted try). Those bytes are read for the checksum but not sent.
func StreamFrom(w io.Writer, paths []string, sizes, offsets []int64, progress func(sent, total int64, idx int)) error {
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
		if i < len(offsets) && offsets[i] > 0 && offsets[i] <= sizes[i] {
			if _, err := io.CopyN(h, f, offsets[i]); err != nil {
				f.Close()
				return fmt.Errorf("%s changed while sending: %w", filepath.Base(p), err)
			}
			sent += offsets[i]
		}
		for remaining := sizes[i] - offsetOf(offsets, i); remaining > 0; {
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

func offsetOf(offsets []int64, i int) int64 {
	if i < len(offsets) && offsets[i] > 0 {
		return offsets[i]
	}
	return 0
}

// Resume is where an unfinished file is kept so a later try can continue it.
// Path "" means no resuming for that file (it is written next to its final place
// as name.part and deleted on failure).
type Resume struct {
	Path   string // in the app's private folder, not the shared one
	Offset int64  // bytes already there
}

// MinResume: smaller files simply start over.
const MinResume = 4 << 20

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
	return ReceiveResume(rw, src, dirs, names, sizes, nil, progress, saved)
}

// ReceiveResume is Receive where file i continues from resume[i] (if set). After a
// dropped connection those files are kept, to be continued; after a checksum
// mismatch they're deleted.
func ReceiveResume(rw io.ReadWriter, src io.Reader, dirs []string, names []string, sizes []int64, resume []Resume,
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
		var r Resume
		if i < len(resume) {
			r = resume[i]
		}
		h := sha256.New()
		var part string
		var f *os.File
		var err error
		if r.Path != "" {
			part = r.Path
			os.MkdirAll(filepath.Dir(part), 0o700)
			f, err = os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o600)
			if err == nil && r.Offset > 0 {
				// The checksum covers the whole file: read back what we already have.
				_, err = io.CopyN(h, f, r.Offset)
			}
			if err == nil {
				err = f.Truncate(r.Offset)
			}
			if err == nil {
				_, err = f.Seek(r.Offset, io.SeekStart)
			}
		} else {
			part = files.UniquePath(dir, name+".part")
			f, err = os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			parts = append(parts, part)
		}
		if err != nil {
			if f != nil {
				f.Close()
			}
			protocol.WriteFrame(rw, protocol.Error{Type: "error", Code: "io", Message: err.Error()})
			return nil, err
		}
		done += r.Offset
		pw := &progressWriter{fn: progress, done: done, total: total, idx: i}
		_, err = io.CopyN(io.MultiWriter(f, h, pw), src, sizes[i]-r.Offset)
		done -= r.Offset
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
			if r.Path != "" {
				os.Remove(r.Path) // damaged: never continue from it
			}
			return nil, fmt.Errorf("%s: checksum mismatch", name)
		}
		final := files.UniquePath(dir, name)
		if err := moveFile(part, final); err != nil {
			return nil, err
		}
		if r.Path == "" {
			parts = parts[:len(parts)-1]
		}
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

// moveFile renames, or copies when the two places are on different drives.
func moveFile(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(to)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(to)
		return err
	}
	in.Close()
	return os.Remove(from)
}

// ResumeKey names an unfinished file: the same file sent again by the same device
// to the same place gets the same key.
func ResumeKey(peerID, folder, path, rel, name string, size int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d", peerID, folder, path, rel, name, size)))
	return hex.EncodeToString(sum[:12])
}

// CleanPartials deletes unfinished files older than a few days.
func CleanPartials(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 72*time.Hour {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
