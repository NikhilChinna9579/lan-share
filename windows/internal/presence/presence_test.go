package presence

import (
	"net"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
	"github.com/NikhilChinna9579/lan-share/windows/internal/identity"
	"github.com/NikhilChinna9579/lan-share/windows/internal/mdns"
	"github.com/NikhilChinna9579/lan-share/windows/internal/server"
	"github.com/NikhilChinna9579/lan-share/windows/internal/trust"
)

// A fake "phone": a real server that trusts the laptop.
func startPhone(t *testing.T, laptop *identity.Identity) (id string, addr string, stop func()) {
	dir := t.TempDir()
	phoneID, _ := identity.LoadOrCreate(dir, "Phone")
	ts, _ := trust.Open(dir)
	ts.Add(trust.Device{ID: laptop.Fingerprint, Name: "Laptop"})
	srv := &server.Server{Name: "Phone", Identity: phoneID, Trust: ts, Folders: files.NewFolders("downloads")}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	return phoneID.Fingerprint, ln.Addr().String(), func() { ln.Close() }
}

func newTracker(t *testing.T) *Tracker {
	dir := t.TempDir()
	me, _ := identity.LoadOrCreate(dir, "Laptop")
	ts, _ := trust.Open(filepath.Join(dir, "t"))
	return &Tracker{Trust: ts, Me: me, MyName: "Laptop", DefaultPort: 47821}
}

func TestOnlineThenOffline(t *testing.T) {
	tr := newTracker(t)
	phoneID, addr, stop := startPhone(t, tr.Me)
	tr.Trust.Add(trust.Device{ID: phoneID, Name: "Phone", Addr: addr})

	changes := make(chan State, 8)
	tr.OnChange = func(s State) { changes <- s }

	tr.checkAll()
	if s := <-changes; s.Status != Online || s.Addr != addr {
		t.Fatalf("got %+v, want online at %s", s, addr)
	}
	stop()
	tr.checkAll()
	tr.checkAll()
	select {
	case s := <-changes:
		if s.Status != Offline {
			t.Fatalf("got %+v, want offline", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never went offline")
	}
}

func TestNewAddressFromMDNS(t *testing.T) {
	tr := newTracker(t)
	phoneID, addr, stop := startPhone(t, tr.Me)
	defer stop()
	// Paired, but the saved address is stale (the phone got a new IP).
	tr.Trust.Add(trust.Device{ID: phoneID, Name: "Phone", Addr: "127.0.0.1:1"})
	tr.Seen(mdns.Peer{ID: phoneID, Addr: netip.MustParseAddrPort(addr)})
	got, ok := tr.tryDevice(phoneID)
	if !ok || got != addr {
		t.Fatalf("tryDevice = %q, %v", got, ok)
	}
	if d, _ := tr.Trust.Get(phoneID); d.Addr != addr {
		t.Fatalf("new address not saved: %q", d.Addr)
	}
}

func TestImpostorIsNotOnline(t *testing.T) {
	tr := newTracker(t)
	_, addr, stop := startPhone(t, tr.Me) // a real server, but not the one we paired
	defer stop()
	tr.Trust.Add(trust.Device{ID: "0000", Name: "Phone", Addr: addr})
	if _, ok := tr.tryDevice("0000"); ok {
		t.Fatal("impostor counted as online")
	}
}

func TestBatteryFromPing(t *testing.T) {
	tr := newTracker(t)
	dir := t.TempDir()
	phoneID, _ := identity.LoadOrCreate(dir, "Phone")
	ts, _ := trust.Open(dir)
	ts.Add(trust.Device{ID: tr.Me.Fingerprint, Name: "Laptop"})
	srv := &server.Server{Name: "Phone", Identity: phoneID, Trust: ts, Folders: files.NewFolders("downloads"), ReportBattery: 64}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	tr.Trust.Add(trust.Device{ID: phoneID.Fingerprint, Name: "Phone", Addr: ln.Addr().String()})
	changes := make(chan State, 8)
	tr.OnChange = func(s State) { changes <- s }

	tr.checkAll() // comes online already knowing its battery
	if s := <-changes; s.Status != Online || s.Battery != 64 || s.Charging {
		t.Fatalf("got %+v, want online at 64%%", s)
	}
	srv.ReportBattery, srv.ReportCharging = 65, true // plugged in: told, though still online
	tr.checkAll()
	select {
	case s := <-changes:
		if s.Battery != 65 || !s.Charging {
			t.Fatalf("got %+v, want 65%% charging", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("battery change not told")
	}
	tr.checkAll() // unchanged: nothing told
	select {
	case s := <-changes:
		t.Fatalf("told again with no change: %+v", s)
	case <-time.After(300 * time.Millisecond):
	}
}
