package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// installSendTo adds "LAN Share" to Explorer's right-click → Send to menu. It runs
// again only if the .exe has moved (e.g. a new version downloaded somewhere else).
func installSendTo(dataDir string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return
	}
	lnk := filepath.Join(appData, "Microsoft", "Windows", "SendTo", "LAN Share.lnk")
	marker := filepath.Join(dataDir, "sendto.txt")
	if old, err := os.ReadFile(marker); err == nil && string(old) == exe {
		if _, err := os.Stat(lnk); err == nil {
			return
		}
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := "$s=(New-Object -ComObject WScript.Shell).CreateShortcut(" + q(lnk) + ");" +
		"$s.TargetPath=" + q(exe) + ";$s.Arguments='send';$s.IconLocation=" + q(exe+",0") + ";" +
		"$s.Description='Send to your phone with LAN Share';$s.Save()"
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if cmd.Run() == nil {
		os.WriteFile(marker, []byte(exe), 0o600)
	}
}
