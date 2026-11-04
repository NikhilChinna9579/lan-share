package client

import (
	"bytes"
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

func TestLaptopSendsToPhone(t *testing.T) {
	dir := t.TempDir()
	laptop, _ := identity.LoadOrCreate(filepath.Join(dir, "laptop"), "Laptop")
	phone, _ := identity.LoadOrCreate(filepath.Join(dir, "phone"), "Phone")
	ts, _ := trust.Open(filepath.Join(dir, "phone"))
	ts.Add(trust.Device{ID: laptop.Fingerprint, Name: "Laptop"})
	inbox := filepath.Join(dir, "inbox")
	folders := files.NewFolders("downloads")
	folders.Add("downloads", "Downloads", inbox)
	srv := &server.Server{Name: "Phone", Identity: phone, Trust: ts, Folders: folders}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go srv.Serve(ln)

	src := filepath.Join(dir, "a b.bin")
	data := make([]byte, 1<<20+3)
	rand.Read(data)
	os.WriteFile(src, data, 0o600)
	empty := filepath.Join(dir, "empty.txt")
	os.WriteFile(empty, nil, 0o600)

	c, err := Dial(ln.Addr().String(), phone.Fingerprint, laptop, "Laptop", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.PeerName != "Phone" {
		t.Fatalf("peer name %q", c.PeerName)
	}
	f, err := c.Folders()
	if err != nil || f.Default != "downloads" {
		t.Fatalf("folders %+v %v", f, err)
	}
	var last int64
	saved, err := c.SendFiles("t1", "downloads", []string{src, empty}, func(sent, total int64, _ int) { last = sent })
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[0] != "a b.bin" || last != int64(len(data)) {
		t.Fatalf("saved %v, progress %d", saved, last)
	}
	got, _ := os.ReadFile(filepath.Join(inbox, "a b.bin"))
	if !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
}

func TestDialRejectsWrongDevice(t *testing.T) {
	dir := t.TempDir()
	laptop, _ := identity.LoadOrCreate(filepath.Join(dir, "laptop"), "Laptop")
	phone, _ := identity.LoadOrCreate(filepath.Join(dir, "phone"), "Phone")
	ts, _ := trust.Open(filepath.Join(dir, "phone"))
	ts.Add(trust.Device{ID: laptop.Fingerprint, Name: "Laptop"})
	srv := &server.Server{Name: "Phone", Identity: phone, Trust: ts, Folders: files.NewFolders("downloads")}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go srv.Serve(ln)
	if _, err := Dial(ln.Addr().String(), "not-this-one", laptop, "Laptop", 2*time.Second); err == nil {
		t.Fatal("connected to the wrong device")
	}
}
