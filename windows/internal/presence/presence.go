// Package presence keeps track of which paired devices are online and where they are.
//
// Addresses come from three places, best first: mDNS announcements, the address that
// worked last time, and — if a device has gone quiet — a quick scan of the local subnet.
// A device counts as online only after it answers a ping over a pinned TLS connection,
// so a stale or spoofed address can never show a device as online.
package presence

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/client"
	"github.com/NikhilChinna9579/lan-share/windows/internal/identity"
	"github.com/NikhilChinna9579/lan-share/windows/internal/mdns"
	"github.com/NikhilChinna9579/lan-share/windows/internal/trust"
)

const (
	checkEvery    = 5 * time.Second
	pingTimeout   = 2 * time.Second
	missesOffline = 2                // pings in a row before we call it offline
	scanCooldown  = 30 * time.Second // don't scan the network more often than this
)

// Status of one paired device.
type Status int

const (
	Checking Status = iota
	Online
	Offline
)

func (s Status) String() string {
	return [...]string{"checking", "online", "offline"}[s]
}

// State is what we know about one paired device right now.
type State struct {
	ID       string
	Name     string
	Status   Status
	Addr     string    // where it answered last, "ip:port"
	LastSeen time.Time // zero if never seen this run
	// CantReachUs is true when the device answers us but reports it can't connect
	// back to this computer: almost always Windows Firewall blocking LAN Share.
	CantReachUs bool
}

// Tracker pings paired devices in the background.
type Tracker struct {
	Trust       *trust.Store
	Me          *identity.Identity
	MyName      string
	DefaultPort int
	OnChange    func(State) // called when a device's status changes

	mu         sync.Mutex
	states     map[string]*State
	misses     map[string]int
	mdnsAddr   map[string]string
	cantStreak map[string]int
	lastScan   time.Time
	kick       chan struct{}
}

func (t *Tracker) init() {
	if t.states == nil {
		t.states = map[string]*State{}
		t.misses = map[string]int{}
		t.mdnsAddr = map[string]string{}
		t.kick = make(chan struct{}, 1)
	}
}

// Seen records an mDNS announcement. Unpaired devices are ignored.
func (t *Tracker) Seen(p mdns.Peer) {
	if _, ok := t.Trust.Get(p.ID); !ok {
		return
	}
	t.mu.Lock()
	t.init()
	changed := t.mdnsAddr[p.ID] != p.Addr.String()
	t.mdnsAddr[p.ID] = p.Addr.String()
	t.mu.Unlock()
	if changed {
		t.CheckNow()
	}
}

// CheckNow asks the background loop to ping everyone right away.
func (t *Tracker) CheckNow() {
	t.mu.Lock()
	t.init()
	k := t.kick
	t.mu.Unlock()
	select {
	case k <- struct{}{}:
	default:
	}
}

// Run pings all paired devices every few seconds until stop is closed.
func (t *Tracker) Run(stop <-chan struct{}) {
	t.mu.Lock()
	t.init()
	kick := t.kick
	t.mu.Unlock()
	tick := time.NewTicker(checkEvery)
	defer tick.Stop()
	for {
		t.checkAll()
		select {
		case <-stop:
			return
		case <-tick.C:
		case <-kick:
		}
	}
}

// Snapshot returns the current state of every paired device, in name order.
func (t *Tracker) Snapshot() []State {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	var out []State
	for _, d := range t.Trust.List() {
		if s, ok := t.states[d.ID]; ok {
			c := *s
			c.Name = d.Name
			out = append(out, c)
		} else {
			out = append(out, State{ID: d.ID, Name: d.Name, Status: Checking})
		}
	}
	return out
}

// Locate finds a working address for one device right now, scanning if needed.
func (t *Tracker) Locate(id string) (string, bool) {
	if addr, ok := t.tryDevice(id); ok {
		return addr, true
	}
	t.scan(true)
	return t.tryDevice(id)
}

// CheckAllNow pings every paired device and returns when done.
func (t *Tracker) CheckAllNow() { t.checkAll() }

func (t *Tracker) checkAll() {
	devices := t.Trust.List()
	var wg sync.WaitGroup
	anyOffline := false
	var mu sync.Mutex
	for _, d := range devices {
		wg.Add(1)
		go func(d trust.Device) {
			defer wg.Done()
			_, ok := t.tryDevice(d.ID)
			if !ok {
				mu.Lock()
				anyOffline = true
				mu.Unlock()
			}
		}(d)
	}
	wg.Wait()
	if anyOffline && t.scan(false) {
		for _, d := range devices {
			t.tryDevice(d.ID)
		}
	}
}

// candidates lists addresses worth trying for a device, best first, no duplicates.
func (t *Tracker) candidates(id string) []string {
	d, _ := t.Trust.Get(id)
	t.mu.Lock()
	t.init()
	list := []string{t.mdnsAddr[id]}
	if s, ok := t.states[id]; ok {
		list = append(list, s.Addr)
	}
	t.mu.Unlock()
	list = append(list, d.Addr)
	var out []string
	seen := map[string]bool{"": true}
	for _, a := range list {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

func (t *Tracker) tryDevice(id string) (string, bool) {
	for _, addr := range t.candidates(id) {
		c, err := client.Dial(addr, id, t.Me, t.MyName, pingTimeout)
		if err != nil {
			continue
		}
		reach, err := c.PingInfo()
		c.Close()
		if err == nil {
			t.Trust.SetName(id, c.PeerName) // it may have been renamed
			t.update(id, Online, addr)
			t.setReach(id, reach != nil && !*reach)
			t.Trust.SetAddr(id, addr)
			return addr, true
		}
	}
	t.update(id, Offline, "")
	return "", false
}

func (t *Tracker) update(id string, st Status, addr string) {
	d, ok := t.Trust.Get(id)
	if !ok {
		return
	}
	t.mu.Lock()
	t.init()
	s, existed := t.states[id]
	if !existed {
		s = &State{ID: id, Status: Checking}
		t.states[id] = s
	}
	prev := s.Status
	if st == Online {
		t.misses[id] = 0
		s.Addr = addr
		s.LastSeen = time.Now()
		s.Status = Online
	} else {
		t.misses[id]++
		// One missed ping can be Wi-Fi noise (or a phone that is just starting up).
		if t.misses[id] >= missesOffline {
			s.Status = Offline
			s.CantReachUs = false // we can't reach it either; the warning no longer applies
		}
	}
	s.Name = d.Name
	snap := *s
	t.mu.Unlock()
	if snap.Status != prev && t.OnChange != nil {
		t.OnChange(snap)
	}
}

func (t *Tracker) setReach(id string, cant bool) {
	t.mu.Lock()
	if t.cantStreak == nil {
		t.cantStreak = map[string]int{}
	}
	if cant {
		t.cantStreak[id]++
		cant = t.cantStreak[id] >= 2 // one report can be a hiccup; two in a row is a pattern
	} else {
		t.cantStreak[id] = 0
	}
	s, ok := t.states[id]
	changed := ok && s.CantReachUs != cant
	if ok {
		s.CantReachUs = cant
	}
	var snap State
	if ok {
		snap = *s
	}
	t.mu.Unlock()
	if changed && t.OnChange != nil {
		t.OnChange(snap)
	}
}

// scan looks for LAN Share devices on the local /24 and records any paired ones.
// Returns true if it found at least one paired device. force skips the cooldown.
func (t *Tracker) scan(force bool) bool {
	t.mu.Lock()
	t.init()
	if !force && time.Since(t.lastScan) < scanCooldown {
		t.mu.Unlock()
		return false
	}
	t.lastScan = time.Now()
	t.mu.Unlock()

	found := false
	var mu sync.Mutex
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	for _, host := range LocalHosts() {
		addr := netip.AddrPortFrom(host, uint16(t.DefaultPort)).String()
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			id, ok := t.probe(addr)
			if !ok {
				return
			}
			if _, paired := t.Trust.Get(id); paired {
				t.mu.Lock()
				t.mdnsAddr[id] = addr
				t.mu.Unlock()
				mu.Lock()
				found = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return found
}

// probe connects to addr and reads the key fingerprint it presents, without trusting it.
func (t *Tracker) probe(addr string) (string, bool) {
	raw, err := net.DialTimeout("tcp", addr, 400*time.Millisecond)
	if err != nil {
		return "", false
	}
	var id string
	c := tls.Client(raw, &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{t.Me.Cert},
		InsecureSkipVerify: true, // we only read the fingerprint; nothing is sent
		VerifyPeerCertificate: func(rc [][]byte, _ [][]*x509.Certificate) error {
			if len(rc) > 0 {
				if cert, err := x509.ParseCertificate(rc[0]); err == nil {
					id = identity.Fingerprint(cert)
				}
			}
			return nil
		},
	})
	c.SetDeadline(time.Now().Add(time.Second))
	c.Handshake()
	c.Close()
	return id, id != ""
}

// LocalHosts lists every address in this machine's private /24 networks.
func LocalHosts() []netip.Addr {
	var out []netip.Addr
	seen := map[netip.Prefix]bool{}
	ifaces, _ := net.Interfaces()
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || !ipn.IP.IsPrivate() {
				continue
			}
			me, _ := netip.AddrFromSlice(ipn.IP.To4())
			// Never scan more than a /24, even on bigger networks: 254 hosts takes ~2 s.
			pfx := netip.PrefixFrom(me, 24).Masked()
			if seen[pfx] {
				continue
			}
			seen[pfx] = true
			for h := pfx.Addr().Next(); pfx.Contains(h); h = h.Next() {
				if h.As4()[3] != 255 { // includes ourselves: handy when testing on one machine
					out = append(out, h)
				}
			}
		}
	}
	return out
}
