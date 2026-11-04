// Command lanshare is the Windows side of LAN Share.
//
//	lanshare.exe                 open the LAN Share window: receive files, see phones, send
//	lanshare.exe send FILE...    send files to a phone (used by Explorer's "Send to" menu)
//	lanshare.exe FILE...         same; this is what dragging files onto the .exe does
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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
	"github.com/NikhilChinna9579/lan-share/windows/internal/ui"
)

// version is overwritten at build time: -ldflags "-X main.version=..."
var version = "dev"

const defaultPort = 47821

var (
	flagData        = flag.String("data", "", "data folder (default: %AppData%\\LanShare)")
	flagPort        = flag.Int("port", defaultPort, "TCP port to listen on")
	flagName        = flag.String("name", "", "device name (default: computer name)")
	flagFolder      = flag.String("folder", "", "send: folder key on the phone, e.g. downloads (skips the question)")
	flagTo          = flag.String("to", "", "send: phone name (skips the question)")
	flagPairingFile = flag.String("pairing-file", "", "testing: also write the pairing QR text to this file")
)

type app struct {
	name    string
	dataDir string
	id      *identity.Identity
	trust   *trust.Store
	folders *files.Folders
	tracker *presence.Tracker
	srv     *server.Server
	host    string
	port    int
}

func main() {
	flag.Parse()
	args := flag.Args()
	a, err := setup()
	if err == nil {
		switch {
		case len(args) > 0 && args[0] == "send":
			err = a.sendMode(args[1:])
		case len(args) > 0:
			err = a.sendMode(args) // files dropped onto the .exe
		default:
			err = a.run()
		}
	}
	if err != nil {
		fmt.Println()
		fmt.Println("  " + ui.Red("Error: ") + err.Error())
		waitForEnter()
		os.Exit(1)
	}
}

func setup() (*app, error) {
	a := &app{dataDir: *flagData, name: *flagName}
	if a.dataDir == "" {
		cfg, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		a.dataDir = filepath.Join(cfg, "LanShare")
	}
	if a.name == "" {
		a.name, _ = os.Hostname()
		if a.name == "" {
			a.name = "Laptop"
		}
	}
	var err error
	if a.id, err = identity.LoadOrCreate(a.dataDir, a.name); err != nil {
		return nil, err
	}
	if a.trust, err = trust.Open(a.dataDir); err != nil {
		return nil, err
	}
	if a.folders, err = files.UserFolders(); err != nil {
		return nil, err
	}
	a.tracker = &presence.Tracker{Trust: a.trust, Me: a.id, MyName: a.name, DefaultPort: defaultPort}
	return a, nil
}

// ---------------------------------------------------------------- main window

func (a *app) run() error {
	ui.Title("LAN Share")
	a.srv = &server.Server{Name: a.name, Identity: a.id, Trust: a.trust, Folders: a.folders, OnEvent: a.onServerEvent}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *flagPort))
	if err != nil {
		// Port taken (another copy running?): fall back to any free port; mDNS tells phones.
		if ln, err = net.Listen("tcp", ":0"); err != nil {
			return err
		}
	}
	a.port = ln.Addr().(*net.TCPAddr).Port
	go a.srv.Serve(ln)

	var others []string
	a.host, others = lanAddress()
	if a.host == "" {
		return errors.New("no Wi-Fi or Ethernet connection found; connect to your home network and try again")
	}

	// Announce ourselves and listen for phones (PROTOCOL.md §2).
	node, err := mdns.Start(&mdns.Service{
		Instance: a.name, Host: a.name, Port: a.port,
		TXT: map[string]string{"v": strconv.Itoa(protocol.Version), "id": a.id.Fingerprint, "p": "windows"},
	}, a.tracker.Seen)
	mdnsNote := ""
	if err != nil {
		mdnsNote = "Auto-discovery is off (" + err.Error() + "); phones will search the network instead."
	} else {
		defer node.Close()
		node.Query()
		go func() {
			for range time.Tick(15 * time.Second) {
				node.Query()
			}
		}()
	}

	a.tracker.OnChange = a.onPresence
	stop := make(chan struct{})
	defer close(stop)
	go a.tracker.Run(stop)
	go installSendTo(a.dataDir)

	// Ctrl+C: say goodbye on mDNS so phones mark us offline at once.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		if node != nil {
			node.Close()
		}
		os.Exit(0)
	}()

	a.printHeader(others, mdnsNote)
	if len(a.trust.List()) == 0 {
		a.showPairing()
	} else {
		time.Sleep(1500 * time.Millisecond) // let the first presence check finish
		a.printPhones()
	}
	a.printMenu()

	for {
		line, ok := <-ui.Lines
		if !ok {
			select {} // no console (started hidden): keep serving
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "s":
			a.interactiveSend()
			a.printMenu()
		case "p":
			a.showPairing()
		case "qr":
			a.showPairingInConsole()
		case "d":
			a.managePhones()
			a.printMenu()
		case "q":
			return nil
		case "":
		default:
			a.printMenu()
		}
	}
}

func (a *app) printHeader(others []string, note string) {
	fmt.Println()
	fmt.Printf("  %s  %s\n", ui.Accent("LAN Share"), ui.Dim(version))
	fmt.Println("  " + ui.Rule())
	label := func(s string) string { return ui.Dim(fmt.Sprintf("%-13s", s)) } // pad before colouring
	fmt.Printf("  %s%s  %s\n", label("This laptop"), ui.Bold(a.name), ui.Dim(fmt.Sprintf("%s:%d", a.host, a.port)))
	fmt.Printf("  %s%s\n", label("Saving to"), mustPath(a.folders, a.folders.Default))
	if len(others) > 0 {
		fmt.Printf("  %s%s\n", label("Also on"), ui.Dim(strings.Join(others, ", ")))
	}
	if note != "" {
		fmt.Println("  " + ui.Yellow(note))
	}
	fmt.Println()
	fmt.Println("  " + ui.Dim("First time? If Windows asks about the firewall, tick \"Private networks\" and Allow."))
}

func (a *app) printPhones() {
	fmt.Println()
	fmt.Println("  " + ui.Bold("Phones"))
	states := a.tracker.Snapshot()
	if len(states) == 0 {
		fmt.Println("  " + ui.Dim("None paired yet. Press P to pair one."))
		return
	}
	for _, s := range states {
		detail := ui.Dim(s.Status.String())
		switch s.Status {
		case presence.Online:
			detail = ui.Green("online") + ui.Dim(" · "+hostOnly(s.Addr))
		case presence.Offline:
			detail = ui.Dim("offline · open LAN Share on the phone")
		}
		fmt.Printf("  %s %-22s %s\n", ui.Dot(s.Status.String()), s.Name, detail)
	}
}

func (a *app) printMenu() {
	fmt.Println()
	fmt.Printf("  %s Send files   %s Pair a phone   %s Phones   %s Quit\n",
		ui.Key("S"), ui.Key("P"), ui.Key("D"), ui.Key("Q"))
	fmt.Println("  " + ui.Dim("Type a letter and press Enter. You can also right-click any file → Send to → LAN Share."))
	fmt.Println("  " + ui.Rule())
}

func stamp() string { return ui.Dim(time.Now().Format("15:04")) }

func (a *app) onServerEvent(e server.Event) {
	switch e.Kind {
	case server.EventPaired:
		fmt.Printf("  %s  %s Paired with %s. It will find this laptop by itself from now on.\n", stamp(), ui.Green("●"), ui.Bold(e.Device))
		writePairedPage(a.name, e.Device)
		if e.PeerIP != "" { // a good first guess at where the phone listens
			a.trust.SetAddr(e.DeviceID, net.JoinHostPort(e.PeerIP, strconv.Itoa(defaultPort)))
		}
		a.tracker.CheckNow()
	case server.EventReceived:
		secs := e.Duration.Seconds()
		if secs < 0.001 {
			secs = 0.001
		}
		fmt.Printf("  %s  %s Received %s from %s %s\n", stamp(), ui.Green("↓"), ui.Bold(filepath.Base(e.Path)), e.Device,
			ui.Dim(fmt.Sprintf("· %s · %s/s", ui.Size(e.Bytes), ui.Size(int64(float64(e.Bytes)/secs)))))
	case server.EventFailed:
		fmt.Printf("  %s  %s Transfer from %s failed: %v\n", stamp(), ui.Red("×"), e.Device, e.Err)
	}
}

func (a *app) onPresence(s presence.State) {
	switch s.Status {
	case presence.Online:
		fmt.Printf("  %s  %s %s is online %s\n", stamp(), ui.Green("●"), ui.Bold(s.Name), ui.Dim("· "+hostOnly(s.Addr)))
	case presence.Offline:
		fmt.Printf("  %s  %s %s is offline\n", stamp(), ui.Dim("○"), s.Name)
	}
}

// ---------------------------------------------------------------- pairing

func (a *app) pairingPayload() (string, string, time.Time, error) {
	code, expires, err := a.srv.StartPairing()
	if err != nil {
		return "", "", time.Time{}, err
	}
	payload, _ := json.Marshal(protocol.PairingQR{
		V: protocol.Version, Host: a.host, Port: a.port, ID: a.id.Fingerprint, Name: a.name, Code: code,
	})
	if *flagPairingFile != "" {
		os.WriteFile(*flagPairingFile, payload, 0o600)
	}
	return string(payload), code, expires, nil
}

func (a *app) showPairing() {
	payload, code, expires, err := a.pairingPayload()
	if err != nil {
		fmt.Println("  " + ui.Red("Could not start pairing: ") + err.Error())
		return
	}
	qr, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		fmt.Println("  " + ui.Red("Could not make QR code: ") + err.Error())
		return
	}
	png, _ := qr.PNG(512)
	fmt.Println()
	fmt.Println("  " + ui.Bold("Pair a phone"))
	opened := false
	if err := writePairPage(pairPage{Name: a.name, Code: code, Expires: expires.Format(time.RFC3339)}, png); err == nil && runtime.GOOS == "windows" {
		opened = exec.Command("rundll32", "url.dll,FileProtocolHandler", pairPagePath()).Start() == nil
	}
	if opened {
		fmt.Println("  The pairing page opened in your browser. On the phone: LAN Share → Pair a laptop → scan.")
		fmt.Printf("  %s\n", ui.Dim(fmt.Sprintf("Code %s, valid until %s. Can't see the page? Type QR and press Enter.", code, expires.Format("15:04"))))
	} else {
		fmt.Println("  On the phone: LAN Share → Pair a laptop → scan this code.")
		fmt.Println(qr.ToSmallString(false))
	}
}

func (a *app) showPairingInConsole() {
	payload, code, expires, err := a.pairingPayload()
	if err != nil {
		return
	}
	qr, _ := qrcode.New(payload, qrcode.Medium)
	fmt.Println(qr.ToSmallString(false))
	fmt.Printf("  %s\n", ui.Dim(fmt.Sprintf("Code %s, valid until %s.", code, expires.Format("15:04"))))
}

// ---------------------------------------------------------------- phones

func (a *app) managePhones() {
	states := a.tracker.Snapshot()
	a.printPhones()
	if len(states) == 0 {
		return
	}
	fmt.Println()
	for i, s := range states {
		fmt.Printf("  %s %s\n", ui.Dim(fmt.Sprintf("%d", i+1)), s.Name)
	}
	ans, _ := ui.Ask("  Type a number to " + ui.Bold("forget") + " that phone, or press Enter to go back: ")
	n, err := strconv.Atoi(ans)
	if err != nil || n < 1 || n > len(states) {
		return
	}
	s := states[n-1]
	if yes, _ := ui.Ask(fmt.Sprintf("  Forget %s? It will need to pair again. (y/N): ", s.Name)); strings.EqualFold(yes, "y") {
		a.trust.Remove(s.ID)
		fmt.Printf("  Forgot %s.\n", s.Name)
	}
}

// ---------------------------------------------------------------- sending

func (a *app) interactiveSend() {
	fmt.Println()
	fmt.Println("  " + ui.Bold("Send files to a phone"))
	line, ok := ui.Ask("  Drag files into this window (or paste paths), then press Enter:\n  ")
	if !ok || line == "" {
		return
	}
	if err := a.send(ui.SplitPaths(line)); err != nil {
		fmt.Println("\n  " + ui.Red("Not sent: ") + err.Error())
	}
}

// sendMode is a separate small window opened by "Send to" or by dropping files on the .exe.
func (a *app) sendMode(paths []string) error {
	ui.Title("LAN Share · Send")
	fmt.Println()
	fmt.Printf("  %s  %s\n", ui.Accent("LAN Share"), ui.Dim("send"))
	fmt.Println("  " + ui.Rule())
	if len(a.trust.List()) == 0 {
		return errors.New("no phone paired yet. Open LAN Share (double-click lanshare.exe) and press P to pair")
	}
	// Not running the full window here: just look around the network briefly.
	if node, err := mdns.Start(nil, a.tracker.Seen); err == nil {
		node.Query()
		time.Sleep(1200 * time.Millisecond)
		node.Close()
	}
	if err := a.send(paths); err != nil {
		return err
	}
	fmt.Println("\n  " + ui.Dim("This window closes in a few seconds."))
	time.Sleep(4 * time.Second)
	return nil
}

func (a *app) send(paths []string) error {
	var total int64
	for i, p := range paths {
		abs, err := filepath.Abs(strings.Trim(p, `"`))
		if err != nil {
			return err
		}
		st, err := os.Stat(abs)
		if err != nil {
			return fmt.Errorf("can't find %s", p)
		}
		if st.IsDir() {
			return fmt.Errorf("%s is a folder; folders can't be sent yet", filepath.Base(abs))
		}
		paths[i] = abs
		total += st.Size()
	}
	if len(paths) == 0 {
		return errors.New("no files given")
	}
	fmt.Printf("  %s, %s\n", fileCount(len(paths)), ui.Size(total))

	phone, addr, err := a.pickPhone()
	if err != nil {
		return err
	}
	c, err := client.Dial(addr, phone.ID, a.id, a.name, 5*time.Second)
	if err != nil {
		return fmt.Errorf("couldn't connect to %s: %w", phone.Name, err)
	}
	defer c.Close()

	folders, err := c.Folders()
	if err != nil {
		return err
	}
	folder, label, err := pickFolder(folders, phone.Name)
	if err != nil {
		return err
	}

	tid := make([]byte, 8)
	rand.Read(tid)
	started := time.Now()
	fmt.Println()
	saved, err := c.SendFiles(hex.EncodeToString(tid), folder, paths, func(sent, total int64, i int) {
		ui.Bar(filepath.Base(paths[i]), sent, total, started)
	})
	fmt.Println()
	if err != nil {
		return err
	}
	fmt.Printf("  %s Sent %s to %s → %s %s\n", ui.Green("↑"), fileCount(len(saved)), ui.Bold(phone.Name), label,
		ui.Dim(fmt.Sprintf("· %s in %.1f s", ui.Size(total), time.Since(started).Seconds())))
	return nil
}

// pickPhone finds the online phones and lets the user choose when there's more than one.
func (a *app) pickPhone() (presence.State, string, error) {
	fmt.Println("  " + ui.Dim("Looking for your phones…"))
	a.tracker.CheckAllNow()
	var online []presence.State
	for _, s := range a.tracker.Snapshot() {
		if s.Status == presence.Online {
			online = append(online, s)
		}
	}
	if len(online) == 0 {
		fmt.Println("  " + ui.Dim("Not found yet, searching the whole Wi-Fi network…"))
		for _, d := range a.trust.List() {
			if addr, ok := a.tracker.Locate(d.ID); ok {
				online = append(online, presence.State{ID: d.ID, Name: d.Name, Status: presence.Online, Addr: addr})
			}
		}
	}
	if *flagTo != "" {
		for _, s := range online {
			if strings.EqualFold(s.Name, *flagTo) {
				return s, s.Addr, nil
			}
		}
		return presence.State{}, "", fmt.Errorf("%s isn't online", *flagTo)
	}
	switch len(online) {
	case 0:
		return presence.State{}, "", errors.New("no phone is online. Open LAN Share on your phone and make sure it's on the same Wi-Fi")
	case 1:
		return online[0], online[0].Addr, nil
	}
	fmt.Println()
	for i, s := range online {
		fmt.Printf("  %s %s %s\n", ui.Key(strconv.Itoa(i+1)), s.Name, ui.Dim(hostOnly(s.Addr)))
	}
	ans, _ := ui.Ask("  Send to which phone? ")
	n, err := strconv.Atoi(ans)
	if err != nil || n < 1 || n > len(online) {
		return presence.State{}, "", errors.New("cancelled")
	}
	return online[n-1], online[n-1].Addr, nil
}

// pickFolder always asks where to save (requirement), unless -folder was given.
func pickFolder(f protocol.FoldersResponse, phone string) (key, label string, err error) {
	if len(f.Folders) == 0 {
		return f.Default, "default folder", nil
	}
	if *flagFolder != "" {
		for _, x := range f.Folders {
			if x.Key == *flagFolder {
				return x.Key, x.Label, nil
			}
		}
		return "", "", fmt.Errorf("%s has no folder %q", phone, *flagFolder)
	}
	fmt.Println()
	fmt.Printf("  Save on %s in:\n", ui.Bold(phone))
	def := 1
	for i, x := range f.Folders {
		extra := ""
		if x.Key == f.Default {
			def = i + 1
			extra = ui.Dim("  (default)")
		}
		fmt.Printf("  %s %s%s\n", ui.Key(strconv.Itoa(i+1)), x.Label, extra)
	}
	ans, ok := ui.Ask(fmt.Sprintf("  Folder number, or Enter for %s: ", f.Folders[def-1].Label))
	if !ok {
		return "", "", errors.New("cancelled")
	}
	n := def
	if ans != "" {
		if n, err = strconv.Atoi(ans); err != nil || n < 1 || n > len(f.Folders) {
			return "", "", errors.New("cancelled")
		}
	}
	return f.Folders[n-1].Key, f.Folders[n-1].Label, nil
}

// ---------------------------------------------------------------- helpers

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
				rank = 5 // unusual for a home network, but better than nothing
			}
			if virtual {
				rank += 10
			}
			s := ip.String()
			if rank < bestRank {
				if best != "" {
					others = append(others, best)
				}
				best, bestRank = s, rank
			} else {
				others = append(others, s)
			}
		}
	}
	return best, others
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

func mustPath(f *files.Folders, key string) string {
	p, _ := f.Path(key)
	return p
}

func waitForEnter() {
	fmt.Println("  Press Enter to close.")
	<-ui.Lines
}
