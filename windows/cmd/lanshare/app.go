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
	cancelled map[string]bool
}

type activity struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"` // received, sent, paired, online, offline, error, info
	Text   string    `json:"text"`
	Detail string    `json:"detail,omitempty"`
	Path   string    `json:"path,omitempty"` // received file, for "Show in folder"
}

type transfer struct {
	ID       string    `json:"id"`
	Dir      string    `json:"dir"` // "in" or "out"
	Device   string    `json:"device"`
	Name     string    `json:"name"`  // current file
	Index    int       `json:"index"` // current file, 1-based
	Count    int       `json:"count"` // files in this transfer
	Files    int       `json:"files"` // files finished
	Sent     int64     `json:"sent"`
	Total    int64     `json:"total"`
	Speed    int64     `json:"speed"`   // bytes per second (average so far)
	Elapsed  int64     `json:"elapsed"` // milliseconds, set when it ends
	State    string    `json:"state"`   // running, done, error, cancelled
	Message  string    `json:"message,omitempty"`
	Folder   string    `json:"folder,omitempty"`
	CanRetry bool      `json:"canRetry"`
	Started  time.Time `json:"started"`
	Updated  time.Time `json:"updated"`

	cancel func()   // stops it (closes the connection)
	retry  *sendReq // how to send it again (outgoing only)
}

// sendReq is everything needed to (re)send a set of files.
type sendReq struct {
	DeviceID, Folder, FolderLabel, Sub string
	List                               []fileInfo
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
	if data, err := os.ReadFile(filepath.Join(dataDir, "history.json")); err == nil {
		json.Unmarshal(data, &a.activity) // history from earlier runs
	}
	os.RemoveAll(filepath.Join(dataDir, "outbox")) // leftovers of dropped files from earlier runs
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
	data, _ := json.Marshal(a.activity)
	a.mu.Unlock()
	os.WriteFile(filepath.Join(a.dataDir, "history.json"), data, 0o600)
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
			t.Name, t.Sent, t.Total, t.Index, t.Count = e.Name, e.Bytes, e.Total, e.Index+1, e.Count
			tid := e.Transfer
			t.cancel = func() { a.srv.Cancel(tid) }
		})
	case server.EventReceived:
		folder := a.folderLabel(e.Folder)
		a.note("received", "Received "+filepath.Base(e.Path),
			fmt.Sprintf("From %s · %s in %s · in %s", e.Device, sizeStr(e.Bytes), durStr(e.Duration), folder), e.Path)
		a.updateTransfer(e.Transfer, "in", e.Device, func(t *transfer) {
			t.Files++
			t.Folder = folder
			if e.Done {
				t.State, t.Sent, t.Elapsed = "done", t.Total, time.Since(t.Started).Milliseconds()
			}
		})
	case server.EventFailed:
		if a.wasCancelled(e.Transfer) {
			a.note("info", "Cancelled receiving from "+e.Device, "Nothing was kept.", "")
			a.updateTransfer(e.Transfer, "in", e.Device, func(t *transfer) { t.State, t.Message = "cancelled", "Cancelled. Nothing was kept." })
			return
		}
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
	if secs := time.Since(t.Started).Seconds(); secs > 0.2 && t.State == "running" {
		t.Speed = int64(float64(t.Sent) / secs)
	}
	if t.State != "running" && t.Elapsed == 0 {
		t.Elapsed = time.Since(t.Started).Milliseconds()
	}
	if t.Elapsed > 0 && t.State == "done" {
		t.Speed = t.Total * 1000 / max(t.Elapsed, 1)
	}
	t.CanRetry = t.retry != nil && (t.State == "error" || t.State == "cancelled")
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
	return a.startSend(hex.EncodeToString(tidb), &sendReq{deviceID, folder, folderLabel, sub, list})
}

func (a *app) startSend(tid string, req *sendReq) string {
	var total int64
	for _, f := range req.List {
		total += f.Size
	}
	d, _ := a.trust.Get(req.DeviceID)
	where := req.FolderLabel
	if req.Sub != "" {
		where += "/" + req.Sub
	}
	a.updateTransfer(tid, "out", d.Name, func(t *transfer) {
		t.Total, t.Folder, t.Count, t.retry = total, where, len(req.List), req
		t.Name, t.Index = req.List[0].Name, 1
	})
	go func() {
		started := time.Now()
		err := a.sendNowList(tid, req.DeviceID, req.Folder, req.Sub, req.List)
		took := time.Since(started)
		what := fileCount(len(req.List))
		if len(req.List) == 1 {
			what = req.List[0].Name
		}
		switch {
		case a.wasCancelled(tid):
			a.updateTransfer(tid, "out", d.Name, func(t *transfer) { t.State, t.Message = "cancelled", "Cancelled. Nothing was kept on the phone." })
			a.note("info", "Cancelled sending "+what+" to "+d.Name, "", "")
		case err != nil:
			a.updateTransfer(tid, "out", d.Name, func(t *transfer) { t.State, t.Message = "error", err.Error() })
			a.note("error", "Couldn't send "+what+" to "+d.Name, err.Error(), "")
		default:
			for _, f := range req.List {
				if f.Temp {
					os.Remove(f.Path) // a dropped file's copy is no longer needed
				}
			}
			a.updateTransfer(tid, "out", d.Name, func(t *transfer) {
				t.State, t.Sent, t.Files, t.retry = "done", t.Total, len(req.List), nil
			})
			a.note("sent", "Sent "+what+" to "+d.Name,
				fmt.Sprintf("In %s · %s in %s (%s/s)", where, sizeStr(total), durStr(took), sizeStr(total*1000/max(took.Milliseconds(), 1))), "")
		}
	}()
	return tid
}

func (a *app) sendNowList(tid, deviceID, folder, sub string, list []fileInfo) error {
	c, d, err := a.dial(deviceID)
	if err != nil {
		return err
	}
	defer c.Close()
	a.updateTransfer(tid, "out", d.Name, func(t *transfer) { t.cancel = func() { c.Abort() } })
	if a.wasCancelled(tid) { // cancelled while we were still connecting
		return errors.New("cancelled")
	}
	paths := make([]string, len(list))
	for i, f := range list {
		paths[i] = f.Path
	}
	_, err = c.SendFiles(tid, folder, sub, paths, func(sent, total int64, i int) {
		a.updateTransfer(tid, "out", d.Name, func(t *transfer) { t.Sent, t.Total, t.Name, t.Index = sent, total, list[i].Name, i+1 })
	})
	return err
}

// cancelTransfer stops a running transfer in either direction.
func (a *app) cancelTransfer(id string) error {
	a.mu.Lock()
	var stop func()
	for _, t := range a.transfers {
		if t.ID == id && t.State == "running" {
			stop = t.cancel
		}
	}
	if a.cancelled == nil {
		a.cancelled = map[string]bool{}
	}
	a.cancelled[id] = true
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
	return nil
}

func (a *app) wasCancelled(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancelled[id]
}

// retryTransfer sends a failed or cancelled transfer again, as a new transfer.
func (a *app) retryTransfer(id string) (string, error) {
	a.mu.Lock()
	var req *sendReq
	for i, t := range a.transfers {
		if t.ID == id && t.retry != nil {
			req = t.retry
			a.transfers = append(a.transfers[:i], a.transfers[i+1:]...) // the new attempt replaces it
			break
		}
	}
	a.mu.Unlock()
	if req == nil {
		return "", errors.New("nothing to retry")
	}
	req.List = a.checkedFiles(req.List)
	if len(req.List) == 0 {
		return "", errors.New("the files are no longer there")
	}
	return a.send(req.DeviceID, req.Folder, req.FolderLabel, req.Sub, req.List), nil
}

func (a *app) dismissTransfer(id string) {
	a.mu.Lock()
	for i, t := range a.transfers {
		if t.ID == id && t.State != "running" {
			a.transfers = append(a.transfers[:i], a.transfers[i+1:]...)
			break
		}
	}
	a.mu.Unlock()
	a.hub.changed()
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
		if t.State == "running" || time.Since(t.Updated) < 20*time.Second || (t.CanRetry && time.Since(t.Updated) < 30*time.Minute) {
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

// durStr: "0.4 s", "12 s", "2 min 5 s", "1 h 3 min".
func durStr(d time.Duration) string {
	switch {
	case d < 100*time.Millisecond:
		return "under 0.1 s"
	case d < 10*time.Second:
		return fmt.Sprintf("%.1f s", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
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
