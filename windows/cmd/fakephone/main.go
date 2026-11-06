// Command fakephone pretends to be the Android app, for testing the laptop app without
// a real phone (on one computer, or in CI). It pairs using the laptop's QR text, then
// receives files and announces itself over mDNS, like the real phone does.
//
//	fakephone -data DIR -pair pairing.json     pair, then keep running
//	fakephone -data DIR                        just keep running (already paired)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/client"
	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
	"github.com/NikhilChinna9579/lan-share/windows/internal/identity"
	"github.com/NikhilChinna9579/lan-share/windows/internal/mdns"
	"github.com/NikhilChinna9579/lan-share/windows/internal/protocol"
	"github.com/NikhilChinna9579/lan-share/windows/internal/server"
	"github.com/NikhilChinna9579/lan-share/windows/internal/trust"
)

func main() {
	data := flag.String("data", "fakephone-data", "where to keep this fake phone's key, pairings and inbox")
	pairFile := flag.String("pair", "", "pair using this file holding the laptop's QR text")
	name := flag.String("name", "Test phone", "phone name")
	port := flag.Int("port", 47821, "port to receive on")
	sendFile := flag.String("send", "", "after starting, send this file to the paired laptop (Downloads)")
	slowMB := flag.Int64("slow", 0, "receive at most this many MB per second (to test cancelling)")
	cantReach := flag.Bool("cant-reach", false, "pretend this phone can't connect to the laptop (firewall test)")
	clipPush := flag.String("clip-push", "", "after starting, put this text on the paired laptop's clipboard")
	clipAuto := flag.Bool("clip-auto", false, "send -clip-push as copied here by itself (the laptop may ask first)")
	clipText := flag.String("clip-text", "", "this fake phone's clipboard, for 'paste from phone'")
	allowPair := flag.String("allow-pair", "", "open pairing (like a phone showing its code) and write {code,id} to this file")
	fetch := flag.String("fetch", "", "after starting, get folder:name from the paired laptop into inbox/Fetched")
	flag.Parse()

	id, err := identity.LoadOrCreate(*data, *name)
	check(err)
	ts, err := trust.Open(*data)
	check(err)
	inbox := filepath.Join(*data, "inbox")
	folders := files.NewFolders("downloads")
	folders.Add("downloads", "Downloads", filepath.Join(inbox, "Download"))
	folders.Add("documents", "Documents", filepath.Join(inbox, "Documents"))
	folders.Add("pictures", "Pictures", filepath.Join(inbox, "Pictures"))
	os.MkdirAll(filepath.Join(inbox, "Pictures"), 0o755)

	if *pairFile != "" {
		raw, err := os.ReadFile(*pairFile)
		check(err)
		var qr protocol.PairingQR
		check(json.Unmarshal(raw, &qr))
		check(pair(id, ts, *name, qr))
		log.Printf("paired with %s", qr.Name)
	}

	if *sendFile != "" {
		go func() {
			time.Sleep(time.Second)
			for _, d := range ts.List() {
				c, err := client.Dial(d.Addr, d.ID, id, *name, 5*time.Second)
				if err != nil {
					log.Printf("send: %v", err)
					continue
				}
				saved, err := c.SendFiles(fmt.Sprint(time.Now().UnixNano()), "downloads", "", []string{*sendFile}, nil)
				c.Close()
				log.Printf("sent to %s: %v %v", d.Name, saved, err)
			}
		}()
	}
	if *fetch != "" {
		folder, want, _ := strings.Cut(*fetch, ":")
		go func() {
			time.Sleep(time.Second)
			for _, d := range ts.List() {
				c, err := client.Dial(d.Addr, d.ID, id, *name, 5*time.Second)
				if err != nil {
					log.Printf("fetch: %v", err)
					continue
				}
				saved, err := c.FetchFiles(fmt.Sprint(time.Now().UnixNano()), folder, "", []string{want}, filepath.Join(inbox, "Fetched"), nil)
				c.Close()
				log.Printf("fetched from %s: %v %v", d.Name, saved, err)
			}
		}()
	}
	srv := &server.Server{Name: *name, Identity: id, Trust: ts, Folders: folders, PartialDir: filepath.Join(*data, "partial"), OnEvent: func(e server.Event) {
		if e.Kind == server.EventReceived {
			log.Printf("received %s (%d bytes) from %s", e.Path, e.Bytes, e.Device)
		}
		if e.Kind == server.EventPaired {
			log.Printf("paired with %s", e.Device)
		}
		if e.Kind == server.EventSent {
			log.Printf("served %d files (%d bytes) to %s", e.Count, e.Bytes, e.Device)
		}
	}}
	srv.Throttle = *slowMB << 20
	srv.OnClip = func(_, from, text string, auto bool) bool {
		log.Printf("clipboard from %s: %q (auto %v)", from, text, auto)
		return true
	}
	srv.ClipText = func() (string, bool, string) {
		if *clipText == "" {
			return "", false, "LAN Share isn't open on the phone"
		}
		return *clipText, true, ""
	}
	if *clipPush != "" {
		go func() {
			time.Sleep(time.Second)
			for _, d := range ts.List() {
				c, err := client.Dial(d.Addr, d.ID, id, *name, 5*time.Second)
				if err != nil {
					log.Printf("clip: %v", err)
					continue
				}
				if *clipAuto {
					err = c.PushClipAuto(*clipPush)
				} else {
					err = c.PushClip(*clipPush)
				}
				c.Close()
				log.Printf("clip sent to %s: %v", d.Name, err)
			}
		}()
	}
	srv.Platform = "android"
	if *allowPair != "" {
		code, _, err := srv.StartPairing()
		check(err)
		b, _ := json.Marshal(map[string]string{"code": code, "id": id.Fingerprint})
		check(os.WriteFile(*allowPair, b, 0o600))
	}
	if *cantReach {
		no := false
		srv.ReportReach = &no
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	check(err)
	if node, err := mdns.Start(&mdns.Service{Instance: *name, Host: "fakephone", Port: *port,
		TXT: map[string]string{"v": "1", "id": id.Fingerprint, "p": "android"}}, nil); err == nil {
		defer node.Close()
	}
	log.Printf("fake phone %q listening on %s", *name, ln.Addr())
	check(srv.Serve(ln))
}

func pair(me *identity.Identity, ts *trust.Store, name string, qr protocol.PairingQR) error {
	c, err := client.Dial(net.JoinHostPort(qr.Host, fmt.Sprint(qr.Port)), qr.ID, me, name, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Pair(qr.Code, name); err != nil {
		return err
	}
	return ts.Add(trust.Device{ID: qr.ID, Name: qr.Name, PairedAt: time.Now(),
		Addr: net.JoinHostPort(qr.Host, fmt.Sprint(qr.Port))})
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
