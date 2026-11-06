package client

import (
	"bytes"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
	"github.com/NikhilChinna9579/lan-share/windows/internal/identity"
	"github.com/NikhilChinna9579/lan-share/windows/internal/server"
	"github.com/NikhilChinna9579/lan-share/windows/internal/trust"
)

func TestResumeAfterDrop(t *testing.T) {
	dir := t.TempDir()
	laptop, _ := identity.LoadOrCreate(filepath.Join(dir, "laptop"), "Laptop")
	phone, _ := identity.LoadOrCreate(filepath.Join(dir, "phone"), "Phone")
	ts, _ := trust.Open(filepath.Join(dir, "phone"))
	ts.Add(trust.Device{ID: laptop.Fingerprint, Name: "Laptop"})
	inbox := filepath.Join(dir, "inbox")
	partial := filepath.Join(dir, "partial")
	folders := files.NewFolders("d")
	folders.Add("d", "Downloads", inbox)
	srv := &server.Server{Name: "Phone", Identity: phone, Trust: ts, Folders: folders, PartialDir: partial, Throttle: 8 << 20}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go srv.Serve(ln)

	src := filepath.Join(dir, "movie.mp4")
	data := make([]byte, 12<<20)
	rand.Read(data)
	os.WriteFile(src, data, 0o600)
	dial := func() *Conn {
		c, err := Dial(ln.Addr().String(), phone.Fingerprint, laptop, "Laptop", 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// 1. The connection drops halfway.
	c := dial()
	var aborted atomic.Bool
	_, err := c.SendFiles("t1", "d", "", []string{src}, func(sent, _ int64, _ int) {
		if sent > 5<<20 && aborted.CompareAndSwap(false, true) {
			go c.Abort()
		}
	})
	if err == nil {
		t.Fatal("expected the transfer to break")
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(inbox, "movie.mp4")); err == nil {
		t.Fatal("a half file is in the shared folder")
	}
	left, _ := os.ReadDir(partial)
	if len(left) != 1 {
		t.Fatalf("expected 1 unfinished file kept, got %d", len(left))
	}
	info, _ := left[0].Info()
	if info.Size() == 0 {
		t.Fatal("nothing was kept")
	}

	// 2. Retry: continues from there, and the file is intact.
	srv.Throttle = 0
	c = dial()
	var first int64 = -1
	_, err = c.SendFiles("t2", "d", "", []string{src}, func(sent, _ int64, _ int) {
		if first < 0 {
			first = sent
		}
	})
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	if first < info.Size() {
		t.Fatalf("started from %d, but %d bytes were already there", first, info.Size())
	}
	got, _ := os.ReadFile(filepath.Join(inbox, "movie.mp4"))
	if !bytes.Equal(got, data) {
		t.Fatal("resumed file differs")
	}
	if left, _ := os.ReadDir(partial); len(left) != 0 {
		t.Fatal("unfinished file not cleaned up")
	}

	// 3. Cancelled on the receiving side: nothing kept for later.
	srv.Throttle = 8 << 20
	os.Remove(filepath.Join(inbox, "movie.mp4"))
	c = dial()
	var cancelled atomic.Bool
	c.SendFiles("t3", "d", "", []string{src}, func(sent, _ int64, _ int) {
		if sent > 3<<20 && cancelled.CompareAndSwap(false, true) {
			go srv.Cancel("t3")
		}
	})
	c.Close()
	time.Sleep(300 * time.Millisecond)
	if left, _ := os.ReadDir(partial); len(left) != 0 {
		t.Fatal("a cancelled transfer left data behind")
	}
}
