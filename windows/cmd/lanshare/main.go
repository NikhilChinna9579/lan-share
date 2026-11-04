// Command lanshare is the Windows side of LAN Share: a small background program that
// receives files, tracks your phones, and shows a window (drawn by Microsoft Edge in
// "app" mode, so there is no browser UI and nothing extra to install).
//
//	lanshare.exe                 start (or bring up the window if already running)
//	lanshare.exe --background    start without opening the window (used at Windows sign-in)
//	lanshare.exe send FILE...    send files to a phone (Explorer's "Send to" menu)
//	lanshare.exe FILE...         same; this is what dropping files on the .exe does
//
// Closing the window leaves LAN Share running so phones can still send to this laptop.
// Quit it from the window's menu.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// version is overwritten at build time: -ldflags "-X main.version=..."
var version = "dev"

const defaultPort = 47821

var (
	flagData        = flag.String("data", "", "data folder (default: %AppData%\\LanShare)")
	flagPort        = flag.Int("port", defaultPort, "TCP port to receive on")
	flagName        = flag.String("name", "", "device name (default: computer name)")
	flagBackground  = flag.Bool("background", false, "start without opening the window")
	flagNoWindow    = flag.Bool("no-window", false, "testing: never open a window; print the window's address instead")
	flagPairingFile = flag.String("pairing-file", "", "testing: write the pairing QR text here whenever pairing starts")
	flagFolder      = flag.String("folder", "", "testing (send): folder key on the phone; skips the window")
)

func main() {
	flag.Parse()
	if err := run(flag.Args()); err != nil {
		log.Printf("error: %v", err)
		showError(err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	dataDir := *flagData
	if dataDir == "" {
		cfg, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		dataDir = filepath.Join(cfg, "LanShare")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	logPath := setupLog(dataDir)

	var files []string
	if len(args) > 0 {
		if args[0] == "send" {
			args = args[1:]
		}
		for _, a := range args {
			if abs, err := filepath.Abs(strings.Trim(a, `"`)); err == nil {
				files = append(files, abs)
			}
		}
	}

	// Already running? Hand over to it and stop: there is only ever one LAN Share.
	if inst, ok := findRunning(dataDir); ok {
		if len(files) > 0 {
			if *flagFolder != "" {
				return inst.sendNow(files, *flagFolder)
			}
			return inst.call("/api/handoff", map[string]any{"files": files})
		}
		if *flagBackground {
			return nil
		}
		return inst.call("/api/show", nil)
	}

	a, err := newApp(dataDir, logPath)
	if err != nil {
		return err
	}
	if err := a.start(); err != nil {
		return err
	}
	log.Printf("LAN Share %s started as %q on %s:%d; window at %s", version, a.name, a.host, a.port, a.uiURL())
	if len(files) > 0 {
		a.handoff(files)
	}
	switch {
	case *flagNoWindow:
		fmt.Println("UI:", a.uiURL())
	case !*flagBackground || len(files) > 0:
		a.showWindow()
	}
	<-a.quit
	a.stop()
	log.Printf("LAN Share stopped")
	return nil
}

// setupLog writes to %AppData%\LanShare\lanshare.log (kept under ~1 MB) and, when
// there is a console, to it as well.
func setupLog(dataDir string) string {
	path := filepath.Join(dataDir, "lanshare.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 1<<20 {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return ""
	}
	var w io.Writer = f
	if hasConsole() {
		w = io.MultiWriter(f, os.Stderr)
	}
	log.SetOutput(w)
	log.SetFlags(log.Ldate | log.Ltime)
	return path
}

var errNotRunning = errors.New("not running")
