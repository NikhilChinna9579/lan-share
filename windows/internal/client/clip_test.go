package client

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
	"github.com/NikhilChinna9579/lan-share/windows/internal/identity"
	"github.com/NikhilChinna9579/lan-share/windows/internal/server"
	"github.com/NikhilChinna9579/lan-share/windows/internal/trust"
)

func TestClipboard(t *testing.T) {
	dir := t.TempDir()
	laptop, _ := identity.LoadOrCreate(filepath.Join(dir, "laptop"), "Laptop")
	phone, _ := identity.LoadOrCreate(filepath.Join(dir, "phone"), "Phone")
	ts, _ := trust.Open(filepath.Join(dir, "phone"))
	ts.Add(trust.Device{ID: laptop.Fingerprint, Name: "Laptop"})
	on := true
	var got, from string
	srv := &server.Server{Name: "Phone", Identity: phone, Trust: ts, Folders: files.NewFolders("d"),
		OnClip: func(_, f, text string) bool {
			if on {
				got, from = text, f
			}
			return on
		},
		ClipText: func() (string, bool, string) {
			if !on {
				return "", false, "clipboard sharing is off"
			}
			return "phone text ✓", true, ""
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
	c := dial()
	if err := c.PushClip("hello 👋 from the laptop"); err != nil || got != "hello 👋 from the laptop" || from != "Laptop" {
		t.Fatalf("push: %v %q %q", err, got, from)
	}
	text, err := c.RequestClip()
	c.Close()
	if err != nil || text != "phone text ✓" {
		t.Fatalf("request: %q %v", text, err)
	}
	on = false
	c = dial()
	if err := c.PushClip("x"); err == nil {
		t.Fatal("push accepted while sharing is off")
	}
	c.Close()
	c = dial()
	if _, err := c.RequestClip(); err == nil || !strings.Contains(err.Error(), "off") {
		t.Fatalf("request while off: %v", err)
	}
	c.Close()
	c = dial()
	if err := c.PushClip(strings.Repeat("x", 256<<10+1)); err == nil {
		t.Fatal("oversized clipboard accepted")
	}
	c.Close()
}
