package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/NikhilChinna9579/lan-share/windows/internal/client"
	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
	"github.com/NikhilChinna9579/lan-share/windows/internal/identity"
	"github.com/NikhilChinna9579/lan-share/windows/internal/mdns"
	"github.com/NikhilChinna9579/lan-share/windows/internal/presence"
	"github.com/NikhilChinna9579/lan-share/windows/internal/protocol"
	"github.com/NikhilChinna9579/lan-share/windows/internal/server"
	"github.com/NikhilChinna9579/lan-share/windows/internal/trust"
)

type app struct {
	name, dataDir, logPath string
	id                     *identity.Identity
	trust                  *trust.Store
	folders                *files.Folders
	tracker                *presence.Tracker
	srv                    *server.Server
	node                   *mdns.Node
	host                   string
	port                   int
	hub                    *hub
	quit                   chan struct{}
	stopTracker            chan struct{}

	apiPort int
	token   string

	mu        sync.Mutex
	activity  []activity
	transfers []*transfer
	pairing   *pairing
	pending   []fileInfo // files handed to us (Send to / drop on .exe) waiting to be sent
	pendingN  int        // bumps each hand-off so the window knows to open the send view
	lastSeen  map[string]presence.State
}

type activity struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"` // received, sent, paired, online, offline, error, info
	Text   string    `json:"text"`
	Detail string    `json:"detail,omitempty"`
	Path   string    `json:"path,omitempty"` // received file, for "Show in folder"
}

type transfer struct {
	ID      string    `json:"id"`
	Dir     string    `json:"dir"` // "in" or "out"
	Device  string    `json:"device"`
	Name    string    `json:"name"` // current file
	Files   int       `json:"files"`
	Sent    int64     `json:"sent"`
	Total   int64     `json:"total"`
	Speed   int64     `json:"speed"` // bytes per second
	State   string    `json:"state"` // running, done, error
	Message string    `json:"message,omitempty"`
	Folder  string    `json:"folder,omitempty"`
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
}

type pairing struct {
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
	QR      string    `json:"qr"` // data: URL
	Paired  string    `json:"paired,omitempty"`
}

type fileInfo struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Temp bool   `json:"temp,omitempty"` // a dropped file copied into our outbox; deleted after sending
}

func newApp(dataDir, logPath string) (*app, error) {
	a := &app{dataDir: dataDir, logPath: logPath, name: *flagName, quit: make(chan struct{}), stopTracker: make(chan struct{})}
	if a.name == "" {
		a.name, _ = os.Hostname()
		if a.name == "" {
			a.name = "Laptop"
		}
	}
	var err error
	if a.id, err = identity.LoadOrCreate(dataDir, a.name); err != nil {
		return nil, err
	}
	if a.trust, err = trust.Open(dataDir); err != nil {
		return nil, err
	}
	if a.folders, err = files.LoadShared(dataDir); err != nil {
		return nil, err
	}
	a.tracker = &presence.Tracker{Trust: a.trust, Me: a.id, MyName: a.name, DefaultPort: defaultPort}
	a.hub = newHub(a.stateJSON)
	tok := make([]byte, 24)
	rand.Read(tok)
	a.token = hex.EncodeToString(tok)
	return a, nil
}

func (a *app) start() error {
	a.srv = &server.Server{Name: a.name, Identity: a.id, Trust: a.trust, Folders: a.folders, OnEvent: a.onServerEvent}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *flagPort))
	if err != nil {
		if ln, err = net.Listen("tcp", ":0"); err != nil { // port taken: any port; mDNS tells phones
			return err
		}
	}
	a.port = ln.Addr().(*net.TCPAddr).Port
	if *flagBlockIncoming {
		go func() { // accept and drop everything, like a blocking firewall
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
	} else {
		go a.srv.Serve(ln)
	}

	a.host, _ = lanAddress()
	if a.host == "" {
		a.host = "127.0.0.1"
		a.note("error", "No Wi-Fi or network connection", "Connect to your home Wi-Fi; LAN Share will pick it up.", "")
	}

	node, err := mdns.Start(&mdns.Service{
		Instance: a.name, Host: a.name, Port: a.port,
		TXT: map[string]string{"v": strconv.Itoa(protocol.Version), "id": a.id.Fingerprint, "p": "windows"},
	}, a.tracker.Seen)
	if err != nil {
		log.Printf("mDNS unavailable: %v", err)
	} else {
		a.node = node
		node.Query()
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					node.Query()
				case <-a.quit:
					return
				}
			}
		}()
	}

	a.tracker.OnChange = a.onPresence
	go a.tracker.Run(a.stopTracker)
	go installSendTo(a.dataDir)
	go func() { // heartbeat: keeps "last seen" times and expiring pairing codes fresh
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				a.hub.changed()
			case <-a.quit:
				return
			}
		}
	}()

	// The window's local API: 127.0.0.1 only, and every call needs the secret token.
	api, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	a.apiPort = api.Addr().(*net.TCPAddr).Port
	go serveAPI(a, api)
	return writeInstance(a.dataDir, instance{Port: a.apiPort, Token: a.token, PID: os.Getpid()})
}

func (a *app) stop() {
	close(a.stopTracker)
	if a.node != nil {
		a.node.Close() // goodbye on mDNS: phones mark us offline at once
	}
	os.Remove(instanceFile(a.dataDir))
}

func (a *app) uiURL() string { return fmt.Sprintf("http://127.0.0.1:%d/#t=%s", a.apiPort, a.token) }

func (a *app) showWindow() {
	if a.hub.clients() > 0 {
		a.hub.send("focus", "{}") // a window is already open: just bring it forward
		return
	}
	if *flagNoWindow {
		return
	}
	if err := openWindow(a.uiURL(), a.dataDir); err != nil {
		log.Printf("could not open the window: %v", err)
	}
}

// ---------------------------------------------------------------- events

func (a *app) note(kind, text, detail, path string) {
	log.Printf("%s: %s %s", kind, text, detail)
	a.mu.Lock()
	a.activity = append(a.activity, activity{Time: time.Now(), Kind: kind, Text: text, Detail: detail, Path: path})
	if len(a.activity) > 200 {
		a.activity = a.activity[len(a.activity)-200:]
	}
	a.mu.Unlock()
	a.hub.changed()
}

func (a *app) onServerEvent(e server.Event) {
	switch e.Kind {
	case server.EventPaired:
		if e.PeerIP != "" { // a good first guess at where the phone listens
			a.trust.SetAddr(e.DeviceID, net.JoinHostPort(e.PeerIP, strconv.Itoa(defaultPort)))
		}
		a.mu.Lock()
		if a.pairing != nil {
			a.pairing.Paired = e.Device
		}
		a.mu.Unlock()
		a.note("paired", "Paired with "+e.Device, "It finds this laptop by itself from now on.", "")
		a.tracker.CheckNow()
	case server.EventProgress:
		a.updateTransfer(e.Transfer, "in", e.Device, func(t *transfer) {
			t.Name, t.Sent, t.Total = e.Name, e.Bytes, e.Total
		})
	case server.EventReceived:
		folder := a.folderLabel(e.Folder)
		a.note("received", "Received "+filepath.Base(e.Path), fmt.Sprintf("From %s · %s · in %s", e.Device, sizeStr(e.Bytes), folder), e.Path)
		a.updateTransfer(e.Transfer, "in", e.Device, func(t *transfer) {
			t.Files++
			t.Folder = folder
			if e.Done {
				t.State, t.Sent = "done", t.Total
			}
		})
	case server.EventFailed:
		a.note("error", "Receiving from "+e.Device+" failed", e.Err.Error(), "")
		a.updateTransfer(e.Transfer, "in", e.Device, func(t *transfer) { t.State, t.Message = "error", e.Err.Error() })
	}
}

// onPresence records only real changes in the activity list, so it reads like a story.
func (a *app) onPresence(s presence.State) {
	a.mu.Lock()
	if a.lastSeen == nil {
		a.lastSeen = map[string]presence.State{}
	}
	prev, known := a.lastSeen[s.ID]
	a.lastSeen[s.ID] = s
	a.mu.Unlock()
	wasOnline := known && prev.Status == presence.Online
	switch {
	case s.Status == presence.Online && !wasOnline:
		a.note("online", s.Name+" is online", hostOnly(s.Addr), "")
	case s.Status == presence.Offline && wasOnline:
		a.note("offline", s.Name+" went offline", "", "")
	}
	if s.Status == presence.Online && s.CantReachUs && !(known && prev.CantReachUs) {
		a.note("error", s.Name+" can't connect to this laptop",
			"Windows Firewall is probably blocking LAN Share. Open Settings → Allow through firewall.", "")
	}
	if s.Status == presence.Online && !s.CantReachUs && known && prev.CantReachUs {
		a.note("info", s.Name+" can connect to this laptop again", "", "")
	}
	a.hub.changed()
}

func (a *app) updateTransfer(id, dir, device string, f func(*transfer)) {
	if id == "" {
		return
	}
	a.mu.Lock()
	var t *transfer
	for _, x := range a.transfers {
		if x.ID == id {
			t = x
		}
	}
	if t == nil {
		t = &transfer{ID: id, Dir: dir, Device: device, State: "running", Started: time.Now()}
		a.transfers = append(a.transfers, t)
		if len(a.transfers) > 8 {
			a.transfers = a.transfers[len(a.transfers)-8:]
		}
	}
	f(t)
	t.Updated = time.Now()
	if secs := time.Since(t.Started).Seconds(); secs > 0.2 {
		t.Speed = int64(float64(t.Sent) / secs)
	}
	a.mu.Unlock()
	a.hub.changed()
}

func (a *app) folderLabel(key string) string {
	for _, r := range a.folders.Roots() {
		if r.Key == key {
			return r.Label
		}
	}
	return key
}

// ---------------------------------------------------------------- pairing

func (a *app) startPairing() error {
	code, expires, err := a.srv.StartPairing()
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(protocol.PairingQR{
		V: protocol.Version, Host: a.host, Port: a.port, ID: a.id.Fingerprint, Name: a.name, Code: code,
	})
	if *flagPairingFile != "" {
		os.WriteFile(*flagPairingFile, payload, 0o600)
	}
	png, err := qrcode.Encode(string(payload), qrcode.Medium, 512)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.pairing = &pairing{Code: code, Expires: expires, QR: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)}
	a.mu.Unlock()
	log.Printf("pairing open until %s", expires.Format("15:04:05"))
	a.hub.changed()
	return nil
}

// ---------------------------------------------------------------- sending

func (a *app) handoff(paths []string) {
	var list []fileInfo
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		list = append(list, fileInfo{Path: p, Name: filepath.Base(p), Size: st.Size()})
	}
	a.mu.Lock()
	a.pending = list
	a.pendingN++
	a.mu.Unlock()
	a.hub.changed()
	a.showWindow()
}

// addrOf finds where a phone is right now.
func (a *app) addrOf(id string) (string, error) {
	for _, s := range a.tracker.Snapshot() {
		if s.ID == id && s.Status == presence.Online {
			return s.Addr, nil
		}
	}
	if addr, ok := a.tracker.Locate(id); ok {
		return addr, nil
	}
	d, _ := a.trust.Get(id)
	return "", fmt.Errorf("%s is offline. Open LAN Share on the phone and check it's on the same Wi-Fi", d.Name)
}

func (a *app) dial(id string) (*client.Conn, trust.Device, error) {
	d, ok := a.trust.Get(id)
	if !ok {
		return nil, d, errors.New("that phone isn't paired")
	}
	addr, err := a.addrOf(id)
	if err != nil {
		return nil, d, err
	}
	c, err := client.Dial(addr, id, a.id, a.name, 5*time.Second)
	if err != nil {
		return nil, d, fmt.Errorf("couldn't connect to %s: %w", d.Name, err)
	}
	return c, d, nil
}

// send starts a transfer in the background and returns its id at once.
func (a *app) send(deviceID, folder, folderLabel, sub string, list []fileInfo) string {
	tidb := make([]byte, 8)
	rand.Read(tidb)
	tid := hex.EncodeToString(tidb)
	var total int64
	for _, f := range list {
		total += f.Size
	}
	d, _ := a.trust.Get(deviceID)
	where := folderLabel
	if sub != "" {
		where += "/" + sub
	}
	a.updateTransfer(tid, "out", d.Name, func(t *transfer) { t.Total, t.Folder = total, where })
	go func() {
		err := a.sendNowList(tid, deviceID, folder, sub, list)
		for _, f := range list {
			if f.Temp {
				os.Remove(f.Path)
			}
		}
		if err != nil {
			a.updateTransfer(tid, "out", d.Name, func(t *transfer) { t.State, t.Message = "error", err.Error() })
			a.note("error", "Couldn't send to "+d.Name, err.Error(), "")
			return
		}
		what := fileCount(len(list))
		if len(list) == 1 {
			what = list[0].Name
		}
		a.updateTransfer(tid, "out", d.Name, func(t *transfer) { t.State, t.Sent, t.Files = "done", t.Total, len(list) })
		a.note("sent", "Sent "+what+" to "+d.Name, fmt.Sprintf("In %s · %s", where, sizeStr(total)), "")
	}()
	return tid
}

func (a *app) sendNowList(tid, deviceID, folder, sub string, list []fileInfo) error {
	c, d, err := a.dial(deviceID)
	if err != nil {
		return err
	}
	defer c.Close()
	paths := make([]string, len(list))
	for i, f := range list {
		paths[i] = f.Path
	}
	_, err = c.SendFiles(tid, folder, sub, paths, func(sent, total int64, i int) {
		a.updateTransfer(tid, "out", d.Name, func(t *transfer) { t.Sent, t.Total, t.Name = sent, total, list[i].Name })
	})
	return err
}

// ---------------------------------------------------------------- state for the window

func (a *app) stateJSON() []byte {
	type dev struct {
		ID          string    `json:"id"`
		Name        string    `json:"name"`
		Status      string    `json:"status"`
		Addr        string    `json:"addr"`
		LastSeen    time.Time `json:"lastSeen"`
		CantReachUs bool      `json:"cantReachUs"`
	}
	type folder struct {
		Key     string `json:"key"`
		Label   string `json:"label"`
		Path    string `json:"path"`
		Builtin bool   `json:"builtin"`
		Default bool   `json:"default"`
	}
	var devs []dev
	for _, s := range a.tracker.Snapshot() {
		devs = append(devs, dev{s.ID, s.Name, s.Status.String(), hostOnly(s.Addr), s.LastSeen, s.CantReachUs})
	}
	var fl []folder
	for _, r := range a.folders.Roots() {
		fl = append(fl, folder{r.Key, r.Label, r.Path, r.Builtin, r.Key == a.folders.Default})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var pair *pairing
	if a.pairing != nil && (a.pairing.Paired != "" || time.Now().Before(a.pairing.Expires.Add(30*time.Second))) {
		pair = a.pairing
	}
	act := make([]activity, len(a.activity))
	for i := range a.activity { // newest first
		act[i] = a.activity[len(a.activity)-1-i]
	}
	if len(act) > 60 {
		act = act[:60]
	}
	trs := make([]*transfer, 0, len(a.transfers))
	for i := len(a.transfers) - 1; i >= 0; i-- {
		t := a.transfers[i]
		if t.State == "running" || time.Since(t.Updated) < 20*time.Second {
			trs = append(trs, t)
		}
	}
	sort.SliceStable(devs, func(i, j int) bool { return strings.ToLower(devs[i].Name) < strings.ToLower(devs[j].Name) })
	b, _ := json.Marshal(map[string]any{
		"me": map[string]any{
			"name": a.name, "host": a.host, "port": a.port, "version": version,
			"logPath": a.logPath, "autostart": autostartEnabled(), "platform": platformName(),
		},
		"devices":   orEmpty(devs),
		"folders":   orEmpty(fl),
		"pairing":   pair,
		"activity":  act,
		"transfers": trs,
		"pending":   map[string]any{"n": a.pendingN, "files": orEmpty(a.pending)},
		"now":       time.Now(),
	})
	return b
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ---------------------------------------------------------------- helpers

func sizeStr(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func fileCount(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// lanAddress picks the IPv4 address a phone is most likely to reach: a private
// address on a real adapter, skipping virtual ones (WSL, Hyper-V, VirtualBox, VPNs).
func lanAddress() (best string, others []string) {
	ifaces, _ := net.Interfaces()
	bestRank := 99
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		lname := strings.ToLower(ifc.Name)
		virtual := false
		for _, v := range []string{"vethernet", "virtualbox", "vmware", "hyper-v", "wsl", "docker", "tailscale", "zerotier", "vpn", "bluetooth"} {
			if strings.Contains(lname, v) {
				virtual = true
			}
		}
		addrs, _ := ifc.Addrs()
		for _, addr := range addrs {
			ipn, ok := addr.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			ip := ipn.IP.To4()
			rank := 3
			switch {
			case ip[0] == 192 && ip[1] == 168:
				rank = 1
			case ip[0] == 10:
				rank = 2
			case !ipn.IP.IsPrivate():
				rank = 5
			}
			if virtual {
				rank += 10
			}
			if rank < bestRank {
				if best != "" {
					others = append(others, best)
				}
				best, bestRank = ip.String(), rank
			} else {
				others = append(others, ip.String())
			}
		}
	}
	return best, others
}
