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

func TestFetchFromDevice(t *testing.T) {
	dir := t.TempDir()
	laptop, _ := identity.LoadOrCreate(filepath.Join(dir, "laptop"), "Laptop")
	phone, _ := identity.LoadOrCreate(filepath.Join(dir, "phone"), "Phone")
	ts, _ := trust.Open(filepath.Join(dir, "phone"))
	ts.Add(trust.Device{ID: laptop.Fingerprint, Name: "Laptop"})
	shared := filepath.Join(dir, "shared")
	os.MkdirAll(filepath.Join(shared, "Sub"), 0o755)
	data := make([]byte, 700_001)
	rand.Read(data)
	os.WriteFile(filepath.Join(shared, "Sub", "photo.jpg"), data, 0o600)
	os.WriteFile(filepath.Join(shared, "note.txt"), []byte("hi"), 0o600)
	os.WriteFile(filepath.Join(shared, ".secret"), []byte("no"), 0o600)
	os.WriteFile(filepath.Join(dir, "outside.txt"), []byte("no"), 0o600)
	folders := files.NewFolders("docs")
	folders.Add("docs", "Docs", shared)
	var sent atomic.Int32
	srv := &server.Server{Name: "Phone", Identity: phone, Trust: ts, Folders: folders, OnEvent: func(e server.Event) {
		if e.Kind == server.EventSent {
			sent.Add(1)
		}
	}}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go srv.Serve(ln)

	dial := func() *Conn {
		c, err := Dial(ln.Addr().String(), phone.Fingerprint, laptop, "Laptop", 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	save := filepath.Join(dir, "got")
	c := dial()
	var last int64
	saved, err := c.FetchFiles("f1", "docs", "Sub", []string{"photo.jpg"}, save, func(n, _ int64, _ int) { last = n })
	c.Close()
	if err != nil || len(saved) != 1 || last != int64(len(data)) {
		t.Fatalf("fetch: %v %v %d", saved, err, last)
	}
	got, _ := os.ReadFile(filepath.Join(save, "photo.jpg"))
	if !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	// Two files at once, from the top of the folder.
	c = dial()
	saved, err = c.FetchFiles("f2", "docs", "", []string{"note.txt", "note.txt"}, save, nil)
	c.Close()
	if err != nil || len(saved) != 2 || saved[1] == saved[0] {
		t.Fatalf("fetch two: %v %v", saved, err)
	}
	time.Sleep(50 * time.Millisecond)
	if sent.Load() != 2 {
		t.Fatalf("server reported %d sent events", sent.Load())
	}
	// Refused: hidden files, paths, files outside the folder, missing files.
	for _, bad := range []struct{ path, name string }{
		{"", ".secret"}, {"", "../outside.txt"}, {"..", "outside.txt"}, {"", "missing.txt"}, {"", "Sub"},
	} {
		c = dial()
		if _, err := c.FetchFiles("f3", "docs", bad.path, []string{bad.name}, save, nil); err == nil {
			t.Fatalf("fetched %s/%s", bad.path, bad.name)
		}
		c.Close()
	}
	if _, err := os.Stat(filepath.Join(save, "outside.txt")); err == nil {
		t.Fatal("outside file leaked")
	}
}
