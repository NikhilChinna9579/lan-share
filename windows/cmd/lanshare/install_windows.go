package main

// Installing = copying lanshare.exe to %LocalAppData%\Programs\LanShare (no admin
// rights needed), adding a Start-menu shortcut, and starting with Windows. Settings and
// pairings live in %AppData%\LanShare and are kept across installs and updates.

import (
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func installDir() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "LanShare")
}

func installedExe() string { return filepath.Join(installDir(), "lanshare.exe") }

func startMenuShortcut() string {
	return filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "LAN Share.lnk")
}

func isInstalled() bool {
	exe, err := os.Executable()
	return err == nil && strings.EqualFold(filepath.Clean(exe), filepath.Clean(installedExe()))
}

func installSupported() bool { return os.Getenv("LOCALAPPDATA") != "" }

// install copies this .exe into place, adds the shortcut and autostart, and starts the
// installed copy. The caller then quits, handing over to it.
func install() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(installDir(), 0o755); err != nil {
		return err
	}
	if err := copyFile(self, installedExe()); err != nil {
		return err
	}
	if err := makeShortcut(startMenuShortcut(), installedExe(), "", "Send files between this PC and your phone"); err != nil {
		log.Printf("install: shortcut: %v", err)
	}
	cmd := exec.Command(installedExe(), "--after-install")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: false}
	return cmd.Start()
}

// afterInstall runs in the freshly installed copy.
func afterInstall() {
	if err := setAutostart(true); err != nil {
		log.Printf("install: autostart: %v", err)
	}
}

// uninstall removes the shortcuts and autostart, and deletes the installed .exe once
// this process has exited. Settings and pairings are kept unless removeData is set.
func uninstall(dataDir string, removeData bool) error {
	setAutostart(false)
	os.Remove(startMenuShortcut())
	os.Remove(filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "SendTo", "LAN Share.lnk"))
	if removeData {
		defer os.RemoveAll(dataDir) // after the log file is closed is better, but this is best-effort
	}
	if !isInstalled() {
		return nil
	}
	// A running .exe can't delete itself: ask cmd to do it a moment after we exit.
	script := `ping -n 3 127.0.0.1 >nul & rmdir /s /q "` + installDir() + `"`
	cmd := exec.Command("cmd", "/c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

func makeShortcut(lnk, target, args, desc string) error {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	_, err := powershell("$s=(New-Object -ComObject WScript.Shell).CreateShortcut(" + q(lnk) + ");" +
		"$s.TargetPath=" + q(target) + ";$s.Arguments=" + q(args) + ";$s.IconLocation=" + q(target+",0") + ";" +
		"$s.Description=" + q(desc) + ";$s.Save()")
	return err
}

func copyFile(src, dst string) error {
	if strings.EqualFold(filepath.Clean(src), filepath.Clean(dst)) {
		return errors.New("already installed")
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	os.Remove(dst)
	return os.Rename(tmp, dst)
}
