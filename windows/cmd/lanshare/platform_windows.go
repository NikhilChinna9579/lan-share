package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

func platformName() string { return "windows" }

// hasConsole is false for the normal build (-H windowsgui): no console window at all.
func hasConsole() bool {
	h, err := syscall.GetStdHandle(syscall.STD_ERROR_HANDLE)
	return err == nil && h != 0 && h != syscall.InvalidHandle
}

// openWindow shows the UI in a chromeless Microsoft Edge "app" window with its own
// profile, so it looks and behaves like a normal app and doesn't touch the user's
// browser. Edge ships with Windows 10 and 11; if it's missing, the default browser opens.
func openWindow(url, dataDir string) error {
	for _, base := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LocalAppData")} {
		if base == "" {
			continue
		}
		edge := filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe")
		if _, err := os.Stat(edge); err != nil {
			continue
		}
		profile := filepath.Join(os.Getenv("LocalAppData"), "LanShare", "window")
		cmd := exec.Command(edge, "--app="+url, "--user-data-dir="+profile, "--window-size=480,820",
			"--no-first-run", "--no-default-browser-check", "--disable-features=Translate,msEdgeSidebarV2")
		cmd.Dir = os.TempDir() // not our folder: a window left open mustn't block uninstalling
		return cmd.Start()
	}
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// powershell runs a short script without flashing a console window.
func powershell(script string) (string, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command",
		"[Console]::OutputEncoding=[Text.Encoding]::UTF8;"+script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

const topOwner = "Add-Type -AssemblyName System.Windows.Forms;" +
	"$o=New-Object System.Windows.Forms.Form -Property @{TopMost=$true;ShowInTaskbar=$false;StartPosition='CenterScreen';Width=1;Height=1};$o.Show();$o.Hide();"

func pickFiles() ([]string, error) {
	out, err := powershell(topOwner + "$d=New-Object System.Windows.Forms.OpenFileDialog;$d.Multiselect=$true;$d.Title='Choose files to send';" +
		"if($d.ShowDialog($o) -eq 'OK'){$d.FileNames -join \"`n\"}")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(strings.ReplaceAll(out, "\r", ""), "\n"), nil
}

func pickFolder() (string, error) {
	out, err := powershell(topOwner + "$d=New-Object System.Windows.Forms.FolderBrowserDialog;" +
		"$d.Description='Choose a folder to share with your phones';$d.ShowNewFolderButton=$true;" +
		"if($d.ShowDialog($o) -eq 'OK'){$d.SelectedPath}")
	if err == nil && out == "" {
		err = errors.New("cancelled")
	}
	return out, err
}

// allowFirewall replaces any rule for this .exe (including the "block" rules Windows
// creates when its firewall prompt is dismissed) with one that allows it. Windows asks
// for permission once (UAC).
func allowFirewall() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := `netsh advfirewall firewall delete rule name=all program="` + exe + `" & ` +
		`netsh advfirewall firewall add rule name="LAN Share" dir=in action=allow program="` + exe + `" enable=yes profile=any protocol=tcp & ` +
		`netsh advfirewall firewall add rule name="LAN Share (discovery)" dir=in action=allow program="` + exe + `" enable=yes profile=any protocol=udp`
	_, err = powershell("$p=Start-Process cmd.exe -ArgumentList '/c " + strings.ReplaceAll(cmd, "'", "''") +
		"' -Verb RunAs -WindowStyle Hidden -Wait -PassThru; exit $p.ExitCode")
	if err != nil {
		return errors.New("Windows didn't allow the change (was the permission prompt declined?)")
	}
	return nil
}

const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func autostartEnabled() bool {
	cmd := exec.Command("reg", "query", runKey, "/v", "LanShare")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run() == nil
}

func setAutostart(on bool) error {
	var cmd *exec.Cmd
	if on {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd = exec.Command("reg", "add", runKey, "/v", "LanShare", "/t", "REG_SZ", "/d", `"`+exe+`" --background`, "/f")
	} else {
		cmd = exec.Command("reg", "delete", runKey, "/v", "LanShare", "/f")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}

// openPath opens a file or folder in Explorer; selectIt highlights a file in its folder.
func openPath(p string, selectIt bool) error {
	if selectIt {
		return exec.Command("explorer", "/select,", p).Start()
	}
	return exec.Command("explorer", p).Start()
}

// showError is used when something fails before the window can open.
func showError(msg string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	t, _ := syscall.UTF16PtrFromString("LAN Share")
	m, _ := syscall.UTF16PtrFromString(msg)
	user32.NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x10)
}
