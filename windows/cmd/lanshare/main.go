// Command lanshare is the Windows side of LAN Share.
//
// Spike version: a console window that pairs with the phone by QR code and saves
// files the phone sends. The tray app replaces this console later.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
	"github.com/NikhilChinna9579/lan-share/windows/internal/identity"
	"github.com/NikhilChinna9579/lan-share/windows/internal/protocol"
	"github.com/NikhilChinna9579/lan-share/windows/internal/server"
	"github.com/NikhilChinna9579/lan-share/windows/internal/trust"
)

// version is overwritten at build time: -ldflags "-X main.version=..."
var version = "dev"

const defaultPort = 47821

func main() {
	if err := run(); err != nil {
		fmt.Println()
		fmt.Println("Error:", err)
		waitForEnter() // keep the window open so the message can be read
		os.Exit(1)
	}
}

func run() error {
	fmt.Printf("LAN Share for Windows %s (protocol v%d)\n\n", version, protocol.Version)

	cfg, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dataDir := filepath.Join(cfg, "LanShare")
	name, _ := os.Hostname()
	if name == "" {
		name = "Laptop"
	}

	id, err := identity.LoadOrCreate(dataDir, name)
	if err != nil {
		return err
	}
	ts, err := trust.Open(dataDir)
	if err != nil {
		return err
	}
	folders, err := files.UserFolders()
	if err != nil {
		return err
	}

	srv := &server.Server{Name: name, Identity: id, Trust: ts, Folders: folders, OnEvent: printEvent}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", defaultPort))
	if err != nil {
		// Port taken (another copy running?): fall back to any free port.
		if ln, err = net.Listen("tcp", ":0"); err != nil {
			return err
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go srv.Serve(ln)

	host, others := lanAddress()
	if host == "" {
		return fmt.Errorf("no Wi-Fi or Ethernet connection found; connect to your home network and try again")
	}
	fmt.Printf("Listening on %s:%d as %q\n", host, port, name)
	if len(others) > 0 {
		fmt.Printf("(Other network adapters: %s)\n", strings.Join(others, ", "))
	}
	fmt.Println("If Windows asks about the firewall, tick \"Private networks\" and click Allow.")
	fmt.Printf("Received files go to: %s\n", mustPath(folders, folders.Default))

	if paired := ts.List(); len(paired) == 0 {
		showPairing(srv, id, name, host, port)
	} else {
		names := make([]string, 0, len(paired))
		for _, d := range paired {
			names = append(names, d.Name)
		}
		sort.Strings(names)
		fmt.Printf("\nPaired with: %s\n", strings.Join(names, ", "))
	}

	fmt.Println("\nCommands: P + Enter = pair a phone, Q + Enter = quit")
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		switch strings.ToLower(strings.TrimSpace(in.Text())) {
		case "p":
			showPairing(srv, id, name, host, port)
		case "q":
			return nil
		}
	}
	// stdin closed (e.g. started without a console): keep serving.
	select {}
}

func showPairing(srv *server.Server, id *identity.Identity, name, host string, port int) {
	code, expires, err := srv.StartPairing()
	if err != nil {
		fmt.Println("Could not start pairing:", err)
		return
	}
	payload, _ := json.Marshal(protocol.PairingQR{
		V: protocol.Version, Host: host, Port: port, ID: id.Fingerprint, Name: name, Code: code,
	})
	qr, err := qrcode.New(string(payload), qrcode.Medium)
	if err != nil {
		fmt.Println("Could not make QR code:", err)
		return
	}
	fmt.Println("\n=== Pair your phone ===")
	fmt.Println("Open LAN Share on the phone, tap \"Pair with laptop\" and scan this code.")
	fmt.Printf("Code %s, valid until %s.\n\n", code, expires.Format("15:04:05"))

	// A picture window scans most reliably; the console version is a backup.
	png := filepath.Join(os.TempDir(), "lanshare-pair.png")
	if err := qr.WriteFile(360, png); err == nil && runtime.GOOS == "windows" {
		exec.Command("rundll32", "url.dll,FileProtocolHandler", png).Start()
		fmt.Println("(The QR code also opened in a picture window.)")
	}
	fmt.Println(qr.ToSmallString(false))
}

func printEvent(e server.Event) {
	ts := time.Now().Format("15:04:05")
	switch e.Kind {
	case server.EventPaired:
		fmt.Printf("[%s] Paired with %s. You can now send files from the phone.\n", ts, e.Device)
		os.Remove(filepath.Join(os.TempDir(), "lanshare-pair.png"))
	case server.EventReceived:
		secs := e.Duration.Seconds()
		if secs < 0.001 {
			secs = 0.001
		}
		fmt.Printf("[%s] Received %s from %s (%s at %s/s)\n", ts, filepath.Base(e.Path), e.Device,
			size(e.Bytes), size(int64(float64(e.Bytes)/secs)))
	case server.EventFailed:
		fmt.Printf("[%s] Transfer from %s failed: %v\n", ts, e.Device, e.Err)
	}
}

func size(n int64) string {
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

// lanAddress picks the IPv4 address the phone is most likely to reach: a private
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
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
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

func mustPath(f *files.Folders, key string) string {
	p, _ := f.Path(key)
	return p
}

func waitForEnter() {
	fmt.Println("Press Enter to close.")
	bufio.NewReader(os.Stdin).ReadString('\n')
}
