package client

import (
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
	"github.com/NikhilChinna9579/lan-share/windows/internal/identity"
	"github.com/NikhilChinna9579/lan-share/windows/internal/server"
	"github.com/NikhilChinna9579/lan-share/windows/internal/trust"
)

// Cancelling midway must leave nothing behind on the receiver.
func TestAbortLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	laptop, _ := identity.LoadOrCreate(filepath.Join(dir, "laptop"), "Laptop")
	phone, _ := identity.LoadOrCreate(filepath.Join(dir, "phone"), "Phone")
	ts, _ := trust.Open(filepath.Join(dir, "phone"))
	ts.Add(trust.Device{ID: laptop.Fingerprint, Name: "Laptop"})
	inbox := filepath.Join(dir, "inbox")
	folders := files.NewFolders("downloads")
	folders.Add("downloads", "Downloads", inbox)
	srv := &server.Server{Name: "Phone", Identity: phone, Trust: ts, Folders: folders, Throttle: 2 << 20}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go srv.Serve(ln)

	src := filepath.Join(dir, "big.bin")
	data := make([]byte, 8<<20)
	rand.Read(data)
	os.WriteFile(src, data, 0o600)

	c, err := Dial(ln.Addr().String(), phone.Fingerprint, laptop, "Laptop", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(700 * time.Millisecond); c.Abort() }()
	if _, err := c.SendFiles("t1", "downloads", "", []string{src}, nil); err == nil {
		t.Fatal("send should fail after abort")
	}
	for i := 0; i < 50; i++ {
		entries, _ := os.ReadDir(inbox)
		if len(entries) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	entries, _ := os.ReadDir(inbox)
	t.Fatalf("left behind: %v", entries)
}
